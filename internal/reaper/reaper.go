// Package reaper runs one dev-lease pass: managed projects without a lease
// get the default lease, a project whose lease expired is stopped (its
// containers, nothing else), and a stopped project is removed from the
// daemon once its effective removal delay has passed, or at once when its
// stack was already torn down (no containers and no volumes left on the
// daemon). A removal runs in exactly one
// pass: what to remove is decided from the daemon at removal time, by the
// label, and the lease is deleted whatever the outcome.
package reaper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// Config wires one pass.
type Config struct {
	// Patterns are globs over Compose project names; a project is managed
	// only if it matches one and is not Own.
	Patterns []string
	// Own is dev-lease's own Compose project; never reaped.
	Own string
	// DefaultTTL is the lease a new managed project gets (default 1h).
	DefaultTTL time.Duration
	// RemoveAfter is how long a stopped project is kept before it is
	// removed; 0 means never remove (default 24h at the CLI).
	RemoveAfter time.Duration
	// StopTimeout bounds each container stop (default 10s).
	StopTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives one JSON line per action taken. Nil means discard.
	Log io.Writer
}

// Report summarises one pass.
type Report struct {
	// Managed projects present on the daemon this pass.
	Managed []string
	// Projects that got the default lease this pass.
	Granted []string
	// Projects whose removal ended without a failed object this pass. The
	// lease of a removed project is deleted either way.
	Removed []string
	// Projects whose removal ended with errors (a failed listing) and
	// single objects that failed (5xx, timeouts, transport errors).
	Failed []string
	// Objects the daemon deliberately refused (4xx other than 404): shared
	// images, in-use volumes and networks. Reported instead of failing the
	// pass.
	Skipped []string
}

const defaultHolder = "default"

var (
	errRenewed  = errors.New("lease renewed while the removal was starting")
	errRemoving = errors.New("another removal is already running for the project")
)

// Pass runs one sweep with an empty via (the sweep's log lines keep their
// current shape, without a via field).
func Pass(ctx context.Context, cli *docker.Client, st lease.Store, cfg Config) (Report, error) {
	return PassWith(ctx, cli, st, cfg, "")
}

// PassWith runs one sweep like Pass, tagging every log line it writes with
// via (ignored when empty).
func PassWith(ctx context.Context, cli *docker.Client, st lease.Store, cfg Config, via string) (Report, error) {
	var rep Report
	now := cfg.Now
	if now == nil {
		// UTC: StoppedAt and the holder timestamps are compared and stored
		// as UTC, and the state file must not depend on the host timezone.
		now = func() time.Time { return time.Now().UTC() }
	}
	log := cfg.Log
	if log == nil {
		log = io.Discard
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = time.Hour
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}

	leases, err := st.Read()
	if err != nil {
		return rep, err
	}
	projects, err := cli.Projects(ctx)
	if err != nil {
		return rep, err
	}

	// A non-empty lease state at pass start can only come from this pass's
	// own serve (one serve per state file), so it was left by a failed
	// container listing or a serve that died: a stopping mark is reverted
	// (the lease goes back to what it was before the stop), a removing mark
	// is cleared and the replacement lease keeps the label record. A state outside the four known ones is
	// unknown (a future or foreign version): it is cleared only when the
	// lease is expired; a live lease with an unknown state is left alone
	// and logged, so an unknown mark never locks a live project and a
	// foreign reader's mark is not wiped from a lease it still manages.
	// inheritAfter carries a cleared lease's recorded label delay and
	// recorded services to the replacement lease the grant loop creates,
	// so a stale removing mark cannot erase the records its own stop
	// read.
	type inherit struct {
		labelRemoveAfter *time.Duration
		services         []string
	}
	inheritAfter := map[string]*inherit{}
	for name, l := range leases {
		if !Managed(name, cfg) {
			continue
		}
		clear := false
		keep := false
		switch l.State {
		case "", lease.StateStopped:
			continue
		case lease.StateStopping:
			// A stale stopping mark: the stop never finished, so the
			// lease goes back to what it was before the stop - stopped
			// again when a stop had already run (StoppedAt is set),
			// live otherwise. The recorded label delay stays either
			// way, and this same pass handles the reverted lease like
			// any other (an expired one is stopped again now).
			reverted := l
			if reverted.StoppedAt.IsZero() {
				reverted.State = ""
			} else {
				reverted.State = lease.StateStopped
			}
			if err := st.Update(func(m map[string]lease.Lease) error {
				if cur, ok := m[name]; ok && cur.State == lease.StateStopping {
					m[name] = reverted
				}
				return nil
			}); err != nil {
				return rep, err
			}
			leases[name] = reverted
			act(log, via, now(), name, "clear-state", "", l.State, nil)
			continue
		case lease.StateRemoving:
			clear = true
		default:
			if l.Expired(now()) {
				clear = true
			} else {
				keep = true
			}
		}
		if keep {
			act(log, via, now(), name, "unknown-state", "", l.State, nil)
			continue
		}
		if clear {
			if err := st.Update(func(m map[string]lease.Lease) error {
				if cur, ok := m[name]; ok && cur.State != "" {
					delete(m, name)
				}
				return nil
			}); err != nil {
				return rep, err
			}
			// Drop the cleared entry from the snapshot too, so the grant
			// loop below treats the project as lease-less.
			delete(leases, name)
			inheritAfter[name] = &inherit{labelRemoveAfter: l.LabelRemoveAfter, services: l.Services}
			act(log, via, now(), name, "clear-state", "", l.State, nil)
		}
	}

	// Default leases for new managed projects, and revival of stopped
	// projects: a running container means the developer ran compose up or
	// start, so the stopped lease is deleted and the default lease
	// granted (logged as revived). A container whose stop failed also
	// looks running here: it gets the default lease and the stop is
	// retried when that expires.
	grantDefault := func(name, action string, in *inherit) error {
		// The snapshot may be stale: a concurrent extend can land while
		// the pass lists projects, so the grant re-checks inside the
		// lock. in carries the records the replaced lease had; every
		// grant keeps them.
		var after *time.Duration
		var services []string
		if in != nil {
			after, services = in.labelRemoveAfter, in.services
		}
		granted := false
		if err := st.Update(func(m map[string]lease.Lease) error {
			if _, ok := m[name]; ok {
				return nil
			}
			l, err := lease.Extend(lease.Lease{Project: name, LabelRemoveAfter: after, Services: services}, defaultHolder, cfg.DefaultTTL, now())
			if err != nil {
				return err
			}
			m[name] = l
			granted = true
			return nil
		}); err != nil {
			return fmt.Errorf("grant default lease for %s: %w", name, err)
		}
		if granted {
			rep.Granted = append(rep.Granted, name)
			act(log, via, now(), name, action, "", name, nil)
		}
		return nil
	}
	for _, name := range docker.SortedProjects(projects) {
		if !Managed(name, cfg) {
			continue
		}
		rep.Managed = append(rep.Managed, name)
		l, exists := leases[name]
		if !exists {
			if err := grantDefault(name, "grant", inheritAfter[name]); err != nil {
				return rep, err
			}
			continue
		}
		if l.State != lease.StateStopped {
			continue
		}
		running, err := anyContainerRunning(ctx, cli, name)
		if err != nil {
			act(log, via, now(), name, "revive", "", "", err)
			continue
		}
		if !running {
			continue
		}
		// Delete the stopped lease under the lock; the grant re-checks
		// below, so an extend that lands in between wins.
		if err := st.Update(func(m map[string]lease.Lease) error {
			if cur, ok := m[name]; !ok || cur.State != lease.StateStopped {
				return nil
			}
			delete(m, name)
			return nil
		}); err != nil {
			return rep, fmt.Errorf("revive %s: %w", name, err)
		}
		// The replacement lease keeps the label delay and the services the
		// stopped lease had recorded: the revive must not erase what the
		// sweep read while the containers were there, and the refresh
		// below re-reads them from the labels when the containers still
		// carry them.
		after := l.LabelRemoveAfter
		delete(leases, name)
		if err := grantDefault(name, "revive", &inherit{labelRemoveAfter: after, services: l.Services}); err != nil {
			return rep, err
		}
	}

	// Refresh the recorded label delay: for every managed project with a
	// lease, the containers' dev-lease.remove-after labels are re-read and
	// recorded while the containers are there, so a later compose down
	// cannot erase the label before the stop. The refresh runs after the
	// grants and revives, so a lease created this pass is covered too. A
	// stopped lease's re-read label also merges into the recorded stop
	// delay exactly as a re-stop's does, so a compose up --no-start that
	// adds the label after the stop still counts, and the sweep's due
	// checks, the view and RemoveNow all read the same value. A project
	// stopped and already due for removal this pass is skipped: its lease
	// is deleted whatever the outcome, so a record for it could never be
	// read. A failed listing keeps the recorded values and is logged. A
	// change is written only when it differs from the snapshot this pass
	// read, so a pass with nothing to change never rewrites the state
	// file. One store update covers every change.
	type refreshChange struct {
		label       *time.Duration
		setLabel    bool
		after       *time.Duration
		afterFrom   string
		services    []string
		setServices bool
	}
	labelNames := map[string]bool{}
	for p := range projects {
		if Managed(p, cfg) {
			labelNames[p] = true
		}
	}
	for name := range leases {
		if Managed(name, cfg) {
			labelNames[name] = true
		}
	}
	changed := map[string]refreshChange{}
	for name := range labelNames {
		l, has := leases[name]
		if has && l.State != "" && l.State != lease.StateStopped {
			continue // an in-flight mark decides its own values
		}
		// A stopped lease that is already due is refreshed too: a label
		// its containers carry now (a 0 above all) must reach the record
		// before the removal step decides on it.
		conts, err := cli.Containers(ctx, name)
		if err != nil {
			act(log, via, now(), name, "label", "", "", err)
			continue
		}
		if len(conts) == 0 {
			// The containers are gone (a compose down): the recorded
			// value is exactly what the stop must fall back to, so it
			// stays.
			continue
		}
		d, fromLabel := removeAfterFor(conts, cfg.RemoveAfter)
		ch := refreshChange{}
		svcs := ServicesOf(conts, nil)
		if !has || !slices.Equal(l.Services, svcs) {
			ch.services, ch.setServices = svcs, true
		}
		if fromLabel {
			if !has || l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != d {
				ch.label, ch.setLabel = &d, true
			}
		} else if has && l.LabelRemoveAfter != nil {
			ch.label, ch.setLabel = nil, true
		}
		if has && l.State == lease.StateStopped && fromLabel {
			// The stopped lease's containers carry a label: merge it into
			// the recorded stop delay the way a re-stop's does.
			recorded, recordedFrom := cfg.RemoveAfter, "serve"
			if l.RemoveAfter != nil {
				recorded, recordedFrom = *l.RemoveAfter, l.RemoveAfterFrom
			}
			merged, from := mergeRecordedDelay(recorded, recordedFrom, d, "label", cfg.RemoveAfter)
			if !(l.RemoveAfter == nil && from == "serve" && merged == 0) &&
				(l.RemoveAfter == nil || *l.RemoveAfter != merged || l.RemoveAfterFrom != from) {
				ch.after, ch.afterFrom = &merged, from
			}
		}
		if ch.setLabel || ch.after != nil || ch.setServices {
			changed[name] = ch
		}
	}
	if len(changed) > 0 {
		if err := st.Update(func(m map[string]lease.Lease) error {
			for name, ch := range changed {
				l, ok := m[name]
				if !ok {
					continue
				}
				// Each field is written on its own: a label the store already
				// holds (a revive's replacement lease inherits it) must not
				// keep the services or the delay from being written.
				if ch.setLabel {
					l.LabelRemoveAfter = ch.label
				}
				if ch.after != nil && l.State == lease.StateStopped &&
					(l.RemoveAfter == nil || *l.RemoveAfter != *ch.after || l.RemoveAfterFrom != ch.afterFrom) {
					l.RemoveAfter, l.RemoveAfterFrom = ch.after, ch.afterFrom
				}
				if ch.setServices && !slices.Equal(l.Services, ch.services) {
					l.Services = ch.services
				}
				m[name] = l
			}
			return nil
		}); err != nil {
			return rep, err
		}
	}

	// The stopped and expired projects: a stopped project is removed once
	// its effective removal delay (lease.EffectiveRemovalDelay: a 0 from the
	// lease or from serve wins, a label-set delay as stored, a serve-set
	// delay the longer of the stored and the current setting) has passed
	// since StoppedAt, or at once when its stack was already torn down
	// underneath the lease (an agent or the operator ran compose down -v or
	// a test recipe's trap cleaned up: no container and no volume is left,
	// only built images and maybe a network - never for a delay of 0);
	// an expired project with no state is stopped. Sorted for
	// deterministic passes and logs.
	type removal struct {
		name  string
		force bool
	}
	due := make([]removal, 0, len(leases))
	for name, l := range leases {
		if !Managed(name, cfg) {
			continue
		}
		switch {
		case l.State == lease.StateStopped:
			d := l.EffectiveRemovalDelay(cfg.RemoveAfter)
			if d > 0 && !now().Before(l.StoppedAt.Add(d)) {
				due = append(due, removal{name: name})
				continue
			}
			// Nothing left on the daemon? The stopped lease has nothing to
			// remove, so it would only keep serve from going idle until the
			// removal deadline: drop it now. Only images or a network left?
			// The stack was torn down: remove what is left now, through the
			// same removal a due project gets.
			empty, tornDown, err := projectLeftovers(ctx, cli, name, l.Services)
			if err != nil {
				act(log, via, now(), name, "remove", "", "", err)
				continue
			}
			if empty {
				if err := st.Update(func(m map[string]lease.Lease) error {
					if cur, ok := m[name]; ok && cur.State == lease.StateStopped {
						delete(m, name)
					}
					return nil
				}); err != nil {
					return rep, fmt.Errorf("drop stopped lease for %s: %w", name, err)
				}
				delete(leases, name)
				rep.Removed = append(rep.Removed, name)
				act(log, via, now(), name, "remove", "", name, nil)
				continue
			}
			if d <= 0 || !tornDown {
				// A delay of 0 is never remove: the deadline never comes,
				// and the forced removal is refused with it.
				continue
			}
			act(log, via, now(), name, "remove", "", "torn down", nil)
			due = append(due, removal{name: name, force: true})
		case l.State == "" && l.Expired(now()):
			due = append(due, removal{name: name})
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].name < due[j].name })
	for _, r := range due {
		if ctx.Err() != nil {
			// The pass was cancelled during an earlier project's action:
			// stop before touching the later ones. Their leases stay as
			// they were; the next pass tries them again.
			break
		}
		name := r.name
		if leases[name].State == lease.StateStopped {
			skipped, failed, removed, err := removeProject(ctx, cli, st, name, cfg, now, log, via, r.force)
			if err != nil {
				// Store failure: report the project and stop its removal;
				// the lease stays as it is (a store that cannot be written
				// blocks everything).
				act(log, via, now(), name, "remove", "", "", err)
				rep.Failed = append(rep.Failed, name)
				continue
			}
			rep.Skipped = append(rep.Skipped, skipped...)
			rep.Failed = append(rep.Failed, failed...)
			if removed {
				rep.Removed = append(rep.Removed, name)
			}
			continue
		}
		skipped, failed, err := stopProject(ctx, cli, st, name, cfg, now, log, via)
		if err != nil {
			act(log, via, now(), name, "stop", "", "", err)
			rep.Failed = append(rep.Failed, name)
			continue
		}
		rep.Skipped = append(rep.Skipped, skipped...)
		rep.Failed = append(rep.Failed, failed...)
	}
	return rep, nil
}

// anyContainerRunning reports whether any non-kept container of the
// project is running. The listing includes stopped containers (all=1), so
// a stopped container is seen as not running. A kept container stays
// running by design and does not count: it alone never revives a stopped
// project.
func anyContainerRunning(ctx context.Context, cli *docker.Client, project string) (bool, error) {
	conts, err := cli.Containers(ctx, project)
	if err != nil {
		return false, err
	}
	for _, c := range conts {
		if c.Running && !docker.Kept(c) {
			return true, nil
		}
	}
	return false, nil
}

// projectEmpty reports whether the daemon holds no managed object of the
// project: no non-kept containers, and once none exist, no networks,
// volumes or images beyond what a kept container uses. The first non-empty
// listing short-circuits, so a stopped project with objects costs one
// container listing per pass.
func projectEmpty(ctx context.Context, cli *docker.Client, project string, recorded []string) (bool, error) {
	empty, _, err := projectLeftovers(ctx, cli, project, recorded)
	return empty, err
}

// projectLeftovers reports what the daemon still holds of the project.
// empty is projectEmpty's answer. tornDown reports the stronger finding
// behind the torn-down rule: no non-kept container and no non-kept volume
// is left (the same notion of kept projectEmpty uses), so only built
// images, a network and kept objects remain - an agent or the operator
// already ran the stack down (compose down -v, a test recipe's trap). A
// volume that is left is data kept on purpose, so it clears tornDown too.
// The first non-kept container or volume short-circuits both, so a
// stopped project with objects costs one container listing per pass.
func projectLeftovers(ctx context.Context, cli *docker.Client, project string, recorded []string) (empty, tornDown bool, err error) {
	conts, err := cli.Containers(ctx, project)
	if err != nil {
		return false, false, err
	}
	keptNets, keptVols, _ := keptUses(conts)
	for _, c := range conts {
		if !docker.Kept(c) {
			return false, false, nil
		}
	}
	nets, err := cli.Networks(ctx, project)
	if err != nil {
		return false, false, err
	}
	netLeft := false
	for _, n := range nets {
		if !keptNets[n.Name] {
			netLeft = true
		}
	}
	vols, err := cli.Volumes(ctx, project)
	if err != nil {
		return false, false, err
	}
	for _, v := range vols {
		if !docker.Kept(v) && !keptVols[v.Name] {
			return false, false, nil
		}
	}
	// No non-kept container and no non-kept volume: the stack is torn
	// down. Only a network or images can still hold it present.
	tornDown = true
	empty = !netLeft
	if empty {
		// Presence's image rule: only an image the removal would actually
		// delete something of keeps the lease (ImageScope.Step); an untagged
		// leftover, an image that only inherited the project label, an image
		// a kept container runs from or a kept service's tag never does. The
		// listing is unfiltered so the stack's unlabelled built images
		// (Compose v2) count too, through the services the lease recorded.
		imgs, err := cli.Images(ctx, "")
		if err != nil {
			return false, false, err
		}
		scope := NewImageScope(project, conts, recorded)
		for _, i := range imgs {
			if len(scope.Step(i).Remove) > 0 {
				empty = false
				break
			}
		}
	}
	return empty, tornDown, nil
}

// keptUses collects what the kept containers use: network names, volume
// names and image ids. Those objects are not removed and do not make the
// project present.
func keptUses(conts []docker.Object) (nets, vols, imgs map[string]bool) {
	nets = map[string]bool{}
	vols = map[string]bool{}
	imgs = map[string]bool{}
	for _, c := range conts {
		if !docker.Kept(c) {
			continue
		}
		for _, n := range c.Networks {
			nets[n] = true
		}
		for _, m := range c.MountNames {
			vols[m] = true
		}
		if c.ImageID != "" {
			imgs[c.ImageID] = true
		}
	}
	return nets, vols, imgs
}

// stopProject is the sweep's stop of an expired project: it marks the
// lease stopping under the store lock (after re-checking that the lease is
// still expired and unmarked, which closes the renewal race; from here on
// extend is refused for the project), then runs stopMarked. The action
// StopNow marks the lease itself and calls stopMarked directly.
func stopProject(ctx context.Context, cli *docker.Client, st lease.Store, project string, cfg Config, now func() time.Time, log io.Writer, via string) (skipped, failed []string, err error) {
	if err := st.Update(func(m map[string]lease.Lease) error {
		l, ok := m[project]
		if !ok {
			return errRenewed
		}
		if l.State != "" {
			return errRenewed
		}
		if !l.Expired(now()) {
			return errRenewed
		}
		l.State = lease.StateStopping
		m[project] = l
		return nil
	}); err != nil {
		if errors.Is(err, errRenewed) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return stopMarked(ctx, cli, st, project, cfg, now, log, via)
}

// stopMarked stops every running container of a project whose lease is
// marked stopping and records the stopped state. It removes nothing:
// containers, networks, volumes and images all stay; the removal runs
// later, once the effective removal delay has passed. Each failed or
// refused stop is reported as removal failures are. The stopped state is
// recorded whatever the stop outcome, so a container whose stop failed
// looks running on the next pass, revives the project with the default
// lease and is stopped again when that expires. A failed container listing
// leaves the stopping mark: the next pass reverts it and stops the project
// again.
func stopMarked(ctx context.Context, cli *docker.Client, st lease.Store, project string, cfg Config, now func() time.Time, log io.Writer, via string) (skipped, failed []string, err error) {
	conts, err := cli.Containers(ctx, project)
	if err != nil {
		act(log, via, now(), project, "stop", "", "", err)
		failed = append(failed, project)
		return skipped, failed, nil
	}
	for _, c := range conts {
		if ctx.Err() != nil {
			break
		}
		if !c.Running || docker.Kept(c) {
			continue // a stopped or kept container needs no stop
		}
		stopErr := cli.StopContainer(ctx, c.ID, cfg.StopTimeout)
		switch {
		case stopErr == nil || is404(stopErr):
			act(log, via, now(), project, "stop", c.ID, c.Name, nil)
		case isSkip(stopErr):
			actSkip(log, via, now(), project, "stop", c.ID, c.Name, "daemon refused the stop")
			skipped = append(skipped, c.Name)
		default:
			act(log, via, now(), project, "stop", c.ID, c.Name, stopErr)
			failed = append(failed, c.Name)
		}
	}

	// The effective removal delay comes from the containers'
	// dev-lease.remove-after labels: a 0 wins, else the longest; an
	// unparsable value (including one below 1h, like the flag's lower
	// bound) counts as 0. Without labels serve's setting applies. Bad keep
	// values and unparsable remove-after values are logged, once per pass.
	logBadPolicyLabels(log, via, now, project, conts)

	// Record the stopped state whatever the stop outcome. Nothing else can
	// change the lease here: extend is refused during stopping. The delay
	// in force and its origin are recorded in the lease; later serve
	// settings act on it only through lease.EffectiveRemovalDelay, so the
	// removal never comes sooner than the recorded delay. With the
	// containers gone (a compose down since the sweep's label record), the
	// delay falls back to the sweep-recorded label value, read under the
	// lock from the lease itself. A StopNow on an already stopped project
	// keeps the recorded StoppedAt: the removal deadline must not restart.
	// The re-read delay of a re-stop is merged into the recorded one by
	// effective value: a label delay is its own value, a serve delay is
	// the larger of the recorded value and serve's current setting, and a
	// recorded stop without RemoveAfter counts as serve's current setting.
	// The larger effective value wins with its origin, the recorded one on
	// a tie, so the deadline never moves earlier either.
	if err := st.Update(func(m map[string]lease.Lease) error {
		l, ok := m[project]
		if !ok || l.State != lease.StateStopping {
			return nil
		}
		d, fromLabel := removeAfterFor(conts, cfg.RemoveAfter)
		removeAfter := &d
		removeAfterFrom := "serve"
		if fromLabel {
			removeAfterFrom = "label"
		} else if len(conts) == 0 && l.LabelRemoveAfter != nil {
			// The containers are gone: keep the label delay the sweep
			// recorded while they existed.
			removeAfter, removeAfterFrom = l.LabelRemoveAfter, "label"
		}
		wasStopped := !l.StoppedAt.IsZero()
		l.State = lease.StateStopped
		if !wasStopped {
			l.StoppedAt = now()
		}
		if wasStopped {
			// A recorded stop without a recorded delay counts as serve's
			// current setting.
			recorded, recordedFrom := cfg.RemoveAfter, "serve"
			if l.RemoveAfter != nil {
				recorded, recordedFrom = *l.RemoveAfter, l.RemoveAfterFrom
			}
			d, from := mergeRecordedDelay(recorded, recordedFrom, *removeAfter, removeAfterFrom, cfg.RemoveAfter)
			if l.RemoveAfter == nil && from == "serve" && d == 0 {
				// A serve-origin 0 with nothing recorded records
				// nothing: the effective delay keeps following serve's
				// setting, so lifting a --remove-after 0 applies the
				// new setting to the old stop. A label 0 is recorded
				// by the merge above.
			} else {
				l.RemoveAfter, l.RemoveAfterFrom = &d, from
			}
		} else {
			l.RemoveAfter = removeAfter
			l.RemoveAfterFrom = removeAfterFrom
		}
		m[project] = l
		return nil
	}); err != nil {
		return skipped, failed, err
	}
	return skipped, failed, nil
}

// mergeRecordedDelay merges the re-read removal delay of a re-stop into
// the recorded one. The comparison runs on the effective delays (each
// serve-origin value capped from below by the serve's current setting),
// not the raw values: a shorter label than the recorded serve-set delay
// must not move the deadline earlier. A re-read 0 counts only when it
// comes from a label (a 0 label or an unreadable one): a serve-origin 0
// is the global off switch, not the stack's delay, and must not
// overwrite the recorded values. A recorded label 0 keeps the recorded
// values (never remove stays never remove); else a re-read label 0 wins;
// else the longer effective delay with its origin, the recorded one on a
// tie.
func mergeRecordedDelay(recorded time.Duration, recordedFrom string, reread time.Duration, rereadFrom string, serve time.Duration) (time.Duration, string) {
	if recorded == 0 && recordedFrom == "label" {
		return recorded, recordedFrom
	}
	if reread == 0 && rereadFrom == "label" {
		return reread, rereadFrom
	}
	if recorded == 0 {
		return recorded, recordedFrom
	}
	recordedEff := recorded
	if recordedFrom != "label" && serve > recordedEff {
		recordedEff = serve
	}
	rereadEff := reread
	if rereadFrom != "label" && serve > rereadEff {
		rereadEff = serve
	}
	if recordedEff >= rereadEff {
		return recorded, recordedFrom
	}
	return reread, rereadFrom
}

// removeAfterFor resolves the effective removal delay of a project from
// the dev-lease.remove-after labels of its containers: no label means
// serve's setting (reported as not from a label), a 0 label wins (never
// remove), otherwise the longest value. An unparsable value counts as 0.
func removeAfterFor(conts []docker.Object, serve time.Duration) (time.Duration, bool) {
	any := false
	zero := false
	longest := time.Duration(0)
	for _, c := range conts {
		raw, ok := c.Labels[docker.RemoveAfterLabel]
		if !ok {
			continue
		}
		any = true
		d, err := parseRemoveAfterLabel(raw)
		if err != nil || d == 0 {
			zero = true
			continue
		}
		if d > longest {
			longest = d
		}
	}
	if !any {
		return serve, false
	}
	if zero {
		return 0, true
	}
	return longest, true
}

// parseRemoveAfterLabel parses a dev-lease.remove-after value: a Go
// duration of at least 1h (like the flag's lower bound; negative refused),
// whole days ("7d", at most 3650d, a leading + refused) or "0" for never.
func parseRemoveAfterLabel(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= time.Hour {
		return d, nil
	}
	if !strings.HasSuffix(s, "d") || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("cannot parse %q: want a Go duration of at least 1h, whole days like 7d or 0", s)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n <= 0 || n > 3650 {
		return 0, fmt.Errorf("cannot parse %q: want a Go duration of at least 1h, whole days like 7d or 0", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// logBadPolicyLabels logs, once per pass and project, policy label values
// that are ignored: a keep value other than "true" and an unparsable
// remove-after value (which means never remove).
func logBadPolicyLabels(log io.Writer, via string, now func() time.Time, project string, conts []docker.Object) {
	var badKeep, badAfter []string
	for _, c := range conts {
		if v, ok := c.Labels[docker.KeepLabel]; ok && v != "true" {
			badKeep = append(badKeep, v)
		}
		if v, ok := c.Labels[docker.RemoveAfterLabel]; ok {
			if _, err := parseRemoveAfterLabel(v); err != nil {
				badAfter = append(badAfter, v)
			}
		}
	}
	if len(badKeep) == 0 && len(badAfter) == 0 {
		return
	}
	act(log, via, now(), project, "label", "", fmt.Sprintf("keep=%v remove-after=%v", badKeep, badAfter),
		errors.New("ignored: keep keeps only with the value true; an unparsable remove-after means never"))
}

// removeProject removes every object of one project: the lease is marked
// removing under the store lock (after re-checking that the lease is still
// stopped and due, which closes the renewal race), then the
// containers are removed without a stop (they are all stopped by then;
// Docker refuses one that was started meanwhile with 409, which counts as
// skipped) - collecting the image ids they ran from - then networks, then
// volumes, then images. It runs only on a stopped project whose effective
// removal delay is above 0 and has passed. What to remove is decided from the daemon at
// removal time, by the label, in this one pass: nothing is recorded for a
// later pass. A failed listing ends the removal of the project for this
// pass (reported in failed); the lease is deleted the same way. Any failure
// or refusal of one object is reported - failed for 5xx/timeouts/transport
// errors, skipped for 4xx other than 404; 404 counts as gone and is neither
// - and the pass goes on with the other objects. At the end the lease is
// deleted whatever the outcome (except when the fresh listing shows a
// running container: then the lease goes back to stopped and nothing is
// removed); a project still present after the pass is managed like a new
// one and gets the default lease.
func removeProject(ctx context.Context, cli *docker.Client, st lease.Store, project string, cfg Config, now func() time.Time, log io.Writer, via string, force bool) (skipped, failed []string, removed bool, err error) {
	// The mark closes the renewal race: an extend that landed since the
	// pass read the state shows up here as a live lease and aborts the
	// removal. From here on extend is refused for the project, so no
	// renewal can arrive mid-removal. The stopped state and the removal
	// deadline are re-checked under the lock: a lease that went live
	// since the snapshot (even one that already expired again) has not
	// had its stopped period, and a clock that moved back delays the
	// deadline. Anything else: leave the lease alone. leaseServices keeps
	// the services the stopped lease recorded, for the image rule after
	// the containers are gone.
	leaseServices := []string(nil)
	if err := st.Update(func(m map[string]lease.Lease) error {
		l, ok := m[project]
		if !ok {
			return errRenewed
		}
		if l.State == "removing" {
			return errRemoving
		}
		if l.State != lease.StateStopped {
			return errRenewed
		}
		if !force {
			if d := l.EffectiveRemovalDelay(cfg.RemoveAfter); d <= 0 || now().Before(l.StoppedAt.Add(d)) {
				return errRenewed
			}
		}
		l.State = "removing"
		leaseServices = l.Services
		m[project] = l
		return nil
	}); err != nil {
		if errors.Is(err, errRenewed) || errors.Is(err, errRemoving) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}

	// deleteLease clears the removing lease, whatever the outcome.
	deleteLease := func() error {
		return st.Update(func(m map[string]lease.Lease) error {
			if cur, ok := m[project]; ok && cur.State == "removing" {
				delete(m, project)
			}
			return nil
		})
	}

	// What to remove is decided now, from the daemon, by the label. The
	// image ids the containers ran from are collected before the
	// containers are removed, so the images stay removable in this pass.
	// Kept containers stay untouched: they are not stopped, not removed,
	// and what they use (networks, volumes, their image) is left alone.
	conts, err := cli.Containers(ctx, project)
	if err != nil {
		act(log, via, now(), project, "remove", "", "", err)
		failed = append(failed, project)
		return skipped, failed, false, deleteLease()
	}
	for _, c := range conts {
		if c.Running && !docker.Kept(c) {
			// Started again after the revive check ran and before the
			// removal listed the containers: set the state back to
			// stopped (keeping StoppedAt) and remove nothing. The next
			// pass sees the running container and revives the project.
			// A kept container stays running by design and does not
			// revert the removal.
			if err := st.Update(func(m map[string]lease.Lease) error {
				if cur, ok := m[project]; ok && cur.State == "removing" {
					cur.State = lease.StateStopped
					m[project] = cur
				}
				return nil
			}); err != nil {
				return skipped, failed, false, err
			}
			actSkip(log, via, now(), project, "remove", "", "", "the stack is running again; the next pass revives it")
			return nil, nil, false, nil
		}
	}
	keptNets, keptVols, _ := keptUses(conts)
	nets, err := cli.Networks(ctx, project)
	if err != nil {
		act(log, via, now(), project, "remove", "", "", err)
		failed = append(failed, project)
		return skipped, failed, false, deleteLease()
	}
	vols, err := cli.Volumes(ctx, project)
	if err != nil {
		act(log, via, now(), project, "remove", "", "", err)
		failed = append(failed, project)
		return skipped, failed, false, deleteLease()
	}

	// removeOne runs one network/volume/image removal and classifies the
	// answer: ok and 404 count as gone, any other 4xx is a deliberate
	// refusal (skipped), everything else is a failure. The removal always
	// goes on with the next object.
	removeOne := func(action, id, name, skipReason string, callErr error) {
		switch {
		case callErr == nil || is404(callErr):
			act(log, via, now(), project, action, id, name, nil)
		case isSkip(callErr):
			actSkip(log, via, now(), project, action, id, name, skipReason)
			skipped = append(skipped, name)
		default:
			act(log, via, now(), project, action, id, name, callErr)
			failed = append(failed, name)
		}
	}

	// Containers: every non-kept one of the project was stopped in an
	// earlier pass, and the fresh listing taken after the lock showed none
	// running. A container someone started
	// after the listing is still running: the daemon refuses its removal
	// with 409 (reported as skipped), its volumes stay because they are in
	// use, and the project, still present, is managed again next pass. A
	// kept container is never removed.
	for _, c := range conts {
		if ctx.Err() != nil {
			break
		}
		if docker.Kept(c) {
			actSkip(log, via, now(), project, "remove", c.ID, c.Name, "kept by label")
			skipped = append(skipped, c.Name)
			continue
		}
		removeOne("remove", c.ID, c.Name, "daemon refused the removal", cli.RemoveContainer(ctx, c.ID))
	}

	for _, n := range nets {
		if ctx.Err() != nil {
			break
		}
		if keptNets[n.Name] {
			actSkip(log, via, now(), project, "remove-network", n.ID, n.Name, "in use by a kept container of the project")
			skipped = append(skipped, n.Name)
			continue
		}
		removeOne("remove-network", n.ID, n.Name, "in use by a container of another project", cli.RemoveNetwork(ctx, n.ID))
	}

	for _, v := range vols {
		if ctx.Err() != nil {
			break
		}
		switch {
		case docker.Kept(v):
			actSkip(log, via, now(), project, "remove-volume", v.ID, v.Name, "kept by label")
			skipped = append(skipped, v.Name)
			continue
		case keptVols[v.Name]:
			actSkip(log, via, now(), project, "remove-volume", v.ID, v.Name, "mounted by a kept container of the project")
			skipped = append(skipped, v.Name)
			continue
		}
		removeOne("remove-volume", v.ID, v.Name, "in use by another container", cli.RemoveVolume(ctx, v.Name))
	}

	// Images: the removal lists them unfiltered, so the project's own
	// unlabelled built images (Compose v2 builds images without labels)
	// are in view too, and the image rule (ImageScope.Step) decides what
	// of each is this removal's: an image with one tag is deleted by id,
	// one with several tags loses the stack's own tags, each checked to
	// still name the listed image right before its delete, the services being the
	// service labels of the containers in this listing or, when no
	// container is left, the services the lease recorded. What stays for
	// a kept service or container, and the project's images that stay for
	// another reason, are logged as skipped; an image that is no part of
	// the project (another project's, a pulled image no container of the
	// project ran from, a dangling image) is not mentioned at all. A
	// cancelled context skips the listing altogether.
	scope := NewImageScope(project, conts, leaseServices)
	var imgs []docker.Object
	if ctx.Err() == nil {
		imgs, err = cli.Images(ctx, "")
		if err != nil {
			act(log, via, now(), project, "remove", "", "", err)
			failed = append(failed, project)
			return skipped, failed, false, deleteLease()
		}
	}
	for _, i := range imgs {
		if ctx.Err() != nil {
			break
		}
		step := scope.Step(i)
		for _, k := range step.Kept {
			actSkip(log, via, now(), project, "remove-image", i.ID, k, step.Reason)
			skipped = append(skipped, k)
		}
		if len(step.Kept) == 0 && len(step.Remove) == 0 && step.Reason != "" {
			actSkip(log, via, now(), project, "remove-image", i.ID, i.Name, step.Reason)
		}
		for _, r := range step.Remove {
			if r.Expect != "" {
				// A delete by tag hits whatever the tag names at that
				// moment: a build may have moved it since the listing. The
				// check narrows that window to the two calls; Docker has no
				// delete that names both a tag and an id.
				cur, err := cli.ImageID(ctx, r.Ref)
				if err != nil {
					removeOne("remove-image", i.ID, r.Name, "daemon refused the check", err)
					continue
				}
				if cur != r.Expect {
					actSkip(log, via, now(), project, "remove-image", i.ID, r.Name, "the tag names another image since the listing")
					skipped = append(skipped, r.Name)
					continue
				}
			}
			removeOne("remove-image", i.ID, r.Name, "daemon refused: in use", cli.RemoveImage(ctx, r.Ref))
		}
	}

	// A cancellation cuts the removal short: the project is reported as
	// failed, with a log line, and the lease is deleted like for any
	// other failure. The objects left behind keep the project present,
	// so the next pass grants the default lease and retries.
	if ctx.Err() != nil {
		act(log, via, now(), project, "remove", "", "", ctx.Err())
		failed = append(failed, project)
	}
	if err := deleteLease(); err != nil {
		return skipped, failed, false, err
	}
	return skipped, failed, len(failed) == 0, nil
}

func is404(err error) bool {
	var se *docker.StatusError
	return errors.As(err, &se) && se.Status == 404
}

// isSkip reports a deliberate refusal from the daemon: any 4xx other than
// 404 (handled as already gone) means another actor owns the object, so it
// is skipped and reported instead of failing the pass.
func isSkip(err error) bool {
	var se *docker.StatusError
	return errors.As(err, &se) && se.Status >= 400 && se.Status < 500
}

// act writes one JSON action line to the log.
func act(w io.Writer, via string, at time.Time, project, action, id, name string, err error) {
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	writeAction(w, via, at, project, action, id, name, result)
}

// actSkip writes one JSON action line for an object that was deliberately
// not removed.
func actSkip(w io.Writer, via string, at time.Time, project, action, id, name, reason string) {
	writeAction(w, via, at, project, action, id, name, "skipped: "+reason)
}

func writeAction(w io.Writer, via string, at time.Time, project, action, id, name, result string) {
	line := struct {
		Time    time.Time `json:"time"`
		Project string    `json:"project"`
		Action  string    `json:"action"`
		ID      string    `json:"id"`
		Name    string    `json:"name"`
		Result  string    `json:"result"`
		Via     string    `json:"via,omitempty"`
	}{
		Time:    at.UTC(),
		Project: project,
		Action:  action,
		ID:      id,
		Name:    name,
		Result:  result,
		Via:     via,
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(line)
}
