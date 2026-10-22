// Package view is the read model for the dashboard and the MCP server:
// one listing of the managed stacks (state, next event, holders, disk) and
// one detail of a single stack (containers, volumes, images, logs). Every
// read is live from the daemon except the volume sizes, which the client
// caches for one minute. Views never stop or remove anything: actions run
// through reaper.Runner.
package view

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// ErrNotManaged is reaper's refusal for a project no pattern matches (or
// dev-lease's own project), shared so callers match one error.
var ErrNotManaged = reaper.ErrNotManaged

// ErrNoStack is Details' refusal for a managed name with no daemon objects
// and no lease record (reaper's refusal for the same situation, shared so
// callers match one error).
var ErrNoStack = reaper.ErrNoStack

// Stack states.
const (
	StateLive     = "live"
	StateStopped  = "stopped"
	StateStopping = "stopping"
	StateRemoving = "removing"
	StateNew      = "new"
)

// The compose labels carrying the project path and its config files.
const (
	workDirLabel     = "com.docker.compose.project.working_dir"
	configFilesLabel = "com.docker.compose.project.config_files"
)

// soonWithin bounds Stack.Soon: the next event is within two hours.
const soonWithin = 2 * time.Hour

// Stack is one managed project as the listing shows it.
type Stack struct {
	Name        string
	Path        string
	ConfigFiles []string
	// State is "live", "stopped", "stopping", "removing" or "new".
	State string
	// Next is the human-readable next event ("stops in 1h38m",
	// "removed in 167h", "removal overdue", "never removed",
	// "stopping now", "removing now").
	Next   string
	NextAt time.Time
	// StoppedAt is when the stack stopped (zero for every other state),
	// so the dashboard can say "stopped 3h ago" without date logic of
	// its own.
	StoppedAt time.Time
	// Soon reports a next event within two hours, or a removal running.
	Soon    bool
	Holders []Holder
	// Running and Stopped count the project's containers (kept ones
	// included).
	Running, Stopped int
	// Up is the age of the longest-running container (zero when nothing
	// runs), so the listing can say "2 running · up 1h50m".
	Up time.Duration
	// RemoveAfter is the effective removal delay: the current
	// containers' dev-lease.remove-after labels for a live or new
	// stack, the recorded value for a stopped one, serve's setting when
	// nothing says otherwise. 0 means never remove.
	RemoveAfter time.Duration
	// DiskBytes is the sum of the project's volume sizes; -1 when one
	// volume's size is unknown.
	DiskBytes int64
	// NeverRemove reports a stack that will not be removed: its effective
	// removal delay is 0 (a label, a recorded delay, or serve's
	// --remove-after 0).
	NeverRemove bool
}

// Holder is one live claim on a stack.
type Holder struct {
	Name  string
	Until time.Time
}

// Container is one container of a stack's detail page.
type Container struct {
	Service     string
	Name        string
	State       string // "running" or "exited"
	Status      string
	Image       string
	MemoryBytes int64 // -1 when unknown
	Kept        bool
	// Up is how long a running container has run (zero for exited ones).
	Up time.Duration
}

// Volume is one volume of a stack's detail page.
type Volume struct {
	Name  string
	Bytes int64 // -1 when unknown
	Kept  bool
}

// Detail is one stack's detail page.
type Detail struct {
	Stack
	Containers []Container
	Volumes    []Volume
	// RemovedImages are the images a removal would remove (a container ran
	// from them, or their tag matches <project>-<service>); KeptImages are
	// everything else: pulled images, kept containers' images, untagged
	// leftovers.
	RemovedImages []string
	KeptImages    []string
	// Networks are the project's networks a removal would remove: every
	// project network except those a kept container uses (the kept
	// container survives, and a network only it uses cannot be removed).
	Networks []string
	// Plan is the engine's plan for the project, computed from the same
	// listings this detail was built from: the stop and remove dialogs
	// render exactly it.
	Plan         reaper.Plan
	StartCommand string
	Logs         []docker.LogLine
	LogService   string
}

// List returns the managed stacks sorted by their next event: the one
// being removed first, stacks that will not be removed last, everything
// else by NextAt ascending.
func List(ctx context.Context, cli *docker.Client, st lease.Store, cfg reaper.Config, now time.Time) ([]Stack, error) {
	projects, err := cli.Projects(ctx)
	if err != nil {
		return nil, err
	}
	leases, err := st.Read()
	if err != nil {
		return nil, err
	}
	conts, err := cli.Containers(ctx, "")
	if err != nil {
		return nil, err
	}
	vols, err := cli.Volumes(ctx, "")
	if err != nil {
		return nil, err
	}
	sizes, err := cli.VolumeSizes(ctx)
	if err != nil {
		// Unknown sizes, not a failed listing: every volume's size is -1
		// and the failure is not cached, so the next read retries.
		sizes = nil
	}
	sort.Slice(conts, func(i, j int) bool { return conts[i].Name < conts[j].Name })
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })

	// The stacks: managed projects on the daemon, plus in-flight marks
	// (stopping, removing) whose objects may already be gone.
	var names []string
	for p := range projects {
		if reaper.Managed(p, cfg) {
			names = append(names, p)
		}
	}
	for name, l := range leases {
		if !reaper.Managed(name, cfg) || projects[name] {
			continue
		}
		if l.State == lease.StateStopping || l.State == lease.StateRemoving {
			names = append(names, name)
		}
	}

	running, stopped := map[string]int{}, map[string]int{}
	up := map[string]time.Duration{}
	first := map[string]docker.Object{}
	byProject := map[string][]docker.Object{}
	for _, c := range conts {
		p := c.Labels[docker.ProjectLabel]
		if !reaper.Managed(p, cfg) {
			continue
		}
		byProject[p] = append(byProject[p], c)
		if c.Running {
			running[p]++
			if age := now.Sub(c.Created); age > up[p] {
				up[p] = age
			}
		} else {
			stopped[p]++
		}
		if _, ok := first[p]; !ok {
			first[p] = c
		}
	}
	disk, volCount, unknown := map[string]int64{}, map[string]int{}, map[string]bool{}
	for _, v := range vols {
		p := v.Labels[docker.ProjectLabel]
		if !reaper.Managed(p, cfg) {
			continue
		}
		volCount[p]++
		if s, ok := sizes[v.Name]; ok {
			disk[p] += s
		} else {
			unknown[p] = true
		}
	}

	stacks := make([]Stack, 0, len(names))
	for _, name := range names {
		l, has := leases[name]
		s := stackFromLease(name, l, has, cfg, now)
		if s.State == StateLive || s.State == StateNew {
			// The delay that will actually apply comes from the current
			// containers' labels, not from serve's setting. With the
			// containers gone (a compose down, the project present through
			// volumes or networks alone), the sweep-recorded label delay
			// is what the stop will apply.
			d, _ := reaper.RemoveAfterFor(byProject[name], cfg.RemoveAfter)
			if len(byProject[name]) == 0 && l.LabelRemoveAfter != nil {
				d = *l.LabelRemoveAfter
			}
			s.RemoveAfter = d
			s.NeverRemove = d == 0
		}
		if s.State == StateStopped {
			// A label 0 the containers carry now wins at once: RemoveNow
			// already refuses, and the next sweep records it.
			if d, fromLabel := reaper.RemoveAfterFor(byProject[name], cfg.RemoveAfter); fromLabel && d == 0 {
				s.RemoveAfter, s.NeverRemove = 0, true
				s.Next, s.NextAt, s.Soon = "never removed", time.Time{}, false
			}
		}
		if cfg.RemoveAfter == 0 {
			// Serve's --remove-after 0 is the engine's global off switch:
			// whatever the labels or the records say, no stack is removed
			// while it is in force, and the page says so.
			s.RemoveAfter = 0
			s.NeverRemove = true
		}
		s.Running, s.Stopped = running[name], stopped[name]
		s.Up = up[name]
		if !unknown[name] {
			s.DiskBytes = disk[name] // 0 when the project has no volumes
		}
		if c, ok := first[name]; ok {
			s.Path = c.Labels[workDirLabel]
			if cf := c.Labels[configFilesLabel]; cf != "" {
				s.ConfigFiles = strings.Split(cf, ",")
			}
		}
		stacks = append(stacks, s)
	}
	sort.SliceStable(stacks, func(i, j int) bool {
		a, b := stacks[i], stacks[j]
		if (a.State == StateRemoving) != (b.State == StateRemoving) {
			return a.State == StateRemoving
		}
		if a.NeverRemove != b.NeverRemove {
			return !a.NeverRemove
		}
		if !a.NextAt.Equal(b.NextAt) {
			return a.NextAt.Before(b.NextAt)
		}
		return a.Name < b.Name
	})
	return stacks, nil
}

// stackFromLease builds a Stack purely from the lease record, with no
// daemon objects behind it: the shape List's entries and Details' stack
// share. has reports whether a lease record exists for the name: without
// one the stack is new.
func stackFromLease(name string, l lease.Lease, has bool, cfg reaper.Config, now time.Time) Stack {
	s := Stack{Name: name, DiskBytes: -1}
	applyLease(&s, l, has, name, cfg, now)
	return s
}

// applyLease fills the state, next event and holders of one stack from
// its lease record (has reports whether the record exists): the one place
// the lease is translated, shared by the listing and the detail page.
func applyLease(s *Stack, l lease.Lease, has bool, name string, cfg reaper.Config, now time.Time) {
	switch {
	case has && l.State == lease.StateRemoving:
		s.RemoveAfter = l.EffectiveRemovalDelay(cfg.RemoveAfter)
		s.NeverRemove = s.RemoveAfter == 0
		s.State, s.Next, s.NextAt, s.Soon = StateRemoving, "removing now", now, true
	case has && l.State == lease.StateStopping:
		s.RemoveAfter = l.EffectiveRemovalDelay(cfg.RemoveAfter)
		s.NeverRemove = s.RemoveAfter == 0
		s.State, s.Next, s.NextAt, s.Soon = StateStopping, "stopping now", now, true
	case has && l.State == lease.StateStopped:
		s.State = StateStopped
		s.StoppedAt = l.StoppedAt
		d := l.EffectiveRemovalDelay(cfg.RemoveAfter)
		s.RemoveAfter = d
		if d <= 0 {
			s.NeverRemove, s.Next = true, "never removed"
			break
		}
		removedAt := l.StoppedAt.Add(d)
		s.NextAt = removedAt
		if !now.Before(removedAt) {
			s.Next, s.Soon = "removal overdue", true
		} else {
			s.Next = "removed in " + FmtDur(removedAt.Sub(now))
			s.Soon = !removedAt.After(now.Add(soonWithin))
		}
	case !has:
		// RemoveAfter and NeverRemove come from the containers' labels,
		// resolved by List after this switch.
		s.State = StateNew
		s.NextAt = now.Add(cfg.DefaultTTL)
		s.Next = "stops in " + FmtDur(cfg.DefaultTTL)
		s.Soon = cfg.DefaultTTL <= soonWithin
	default:
		// RemoveAfter and NeverRemove come from the containers' labels,
		// resolved by List after this switch.
		s.State = StateLive
		e := l.ExpiresAt()
		s.NextAt = e
		s.Next = "stops in " + FmtDur(e.Sub(now))
		s.Soon = !e.IsZero() && !e.After(now.Add(soonWithin))
		for _, h := range l.Holders {
			if h.Expires.After(now) {
				s.Holders = append(s.Holders, Holder{Name: h.Name, Until: h.Expires})
			}
		}
	}
}

// Details returns one stack's detail page. It refuses ErrNotManaged for a
// name no pattern matches (or dev-lease's own project). Memory and logs
// are read only for containers of this project; a logService that matches
// no container of the project makes no call. A failed memory read leaves
// MemoryBytes -1 (a single refused stat must not fail the page).
func Details(ctx context.Context, cli *docker.Client, st lease.Store, cfg reaper.Config, name, logService string, logLines int, now time.Time) (Detail, error) {
	if !reaper.Managed(name, cfg) {
		return Detail{}, ErrNotManaged
	}
	stacks, err := List(ctx, cli, st, cfg, now)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{}
	found := false
	for _, s := range stacks {
		if s.Name == name {
			d.Stack = s
			found = true
			break
		}
	}
	// The listing only knows projects with daemon objects (and in-flight
	// marks): a managed name with a lease record but nothing on the daemon
	// yet is the extend-before-compose-up case, so its stack comes from
	// the lease, built the same way List builds it; a managed name with
	// neither exists nowhere.
	if !found {
		leases, lerr := st.Read()
		if lerr != nil {
			return Detail{}, lerr
		}
		l, has := leases[name]
		if !has {
			return Detail{}, ErrNoStack
		}
		d.Stack = stackFromLease(name, l, has, cfg, now)
		d.Stack.DiskBytes = 0 // nothing on the daemon: no volumes, no bytes
		if d.State == StateLive {
			// No containers, so no labels: the delay is serve's, or the
			// sweep-recorded label delay when one exists.
			if l.LabelRemoveAfter != nil {
				d.RemoveAfter = *l.LabelRemoveAfter
			} else {
				d.RemoveAfter = cfg.RemoveAfter
			}
			d.NeverRemove = d.RemoveAfter == 0
		}
		if cfg.RemoveAfter == 0 {
			// The global off switch (see List) covers the lease-only
			// case too.
			d.RemoveAfter = 0
			d.NeverRemove = true
		}
	}

	conts, err := cli.Containers(ctx, name)
	if err != nil {
		return Detail{}, err
	}
	sort.Slice(conts, func(i, j int) bool { return conts[i].Name < conts[j].Name })
	for _, c := range conts {
		vc := Container{
			Service:     c.Labels[docker.ServiceLabel],
			Name:        c.Name,
			State:       "exited",
			Status:      c.Status,
			Image:       c.ImageRef,
			MemoryBytes: -1,
			Kept:        docker.Kept(c),
		}
		if vc.Image == "" {
			vc.Image = c.ImageID
		}
		if c.Running {
			vc.State = "running"
			vc.Up = now.Sub(c.Created)
			if mem, memErr := cli.ContainerMemory(ctx, c.ID); memErr == nil {
				vc.MemoryBytes = int64(mem)
			}
		}
		d.Containers = append(d.Containers, vc)
		if d.Path == "" {
			d.Path = c.Labels[workDirLabel]
		}
		if d.ConfigFiles == nil {
			if cf := c.Labels[configFilesLabel]; cf != "" {
				d.ConfigFiles = strings.Split(cf, ",")
			}
		}
	}
	d.StartCommand = startCommand(name, d.Path, d.ConfigFiles)

	vols, err := cli.Volumes(ctx, name)
	if err != nil {
		return Detail{}, err
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	sizes, err := cli.VolumeSizes(ctx)
	if err != nil {
		// Unknown sizes, not a failed page: each volume shows -1 and the
		// failure is not cached, so the next read retries.
		sizes = nil
	}
	for _, v := range vols {
		vv := Volume{Name: v.Name, Bytes: -1, Kept: docker.Kept(v)}
		if s, ok := sizes[v.Name]; ok {
			vv.Bytes = s
		}
		d.Volumes = append(d.Volumes, vv)
	}

	nets, err := cli.Networks(ctx, name)
	if err != nil {
		return Detail{}, err
	}
	keptNets := map[string]bool{}
	for _, c := range conts {
		if !docker.Kept(c) {
			continue
		}
		for _, n := range c.Networks {
			keptNets[n] = true
		}
	}
	for _, n := range nets {
		if !keptNets[n.Name] {
			d.Networks = append(d.Networks, n.Name)
		}
	}
	sort.Strings(d.Networks)

	// The image split follows the removal's rules (reaper.ImageRemovable):
	// an image a container of the project ran from, an image with a
	// <project>-<service> tag for its own service label, and an unlabelled
	// image (Compose v2 builds images without labels) with a repository
	// tag exactly <project>-<service> for one of the project's services
	// is removed; everything else stays. The listing is unfiltered, so
	// the unlabelled built images and the pulled images the containers
	// run from are in view. The images of other projects are not part of
	// this project's split at all: the removal never looks at them.
	imgs, err := cli.Images(ctx, "")
	if err != nil {
		return Detail{}, err
	}
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].Name < imgs[j].Name })
	used, keptImgs := map[string]bool{}, map[string]bool{}
	for _, c := range conts {
		if c.ImageID == "" {
			continue
		}
		if docker.Kept(c) {
			keptImgs[c.ImageID] = true
		} else {
			used[c.ImageID] = true
		}
	}
	leases, lerr := st.Read()
	if lerr != nil {
		return Detail{}, lerr
	}
	l := leases[name]
	svcs := reaper.ServicesOf(conts, l.Services)
	for _, i := range imgs {
		p := i.Labels[docker.ProjectLabel]
		if p != "" && p != name {
			continue
		}
		removable := !keptImgs[i.ID] && reaper.ImageRemovable(i, name, svcs, used)
		if p == "" && !removable && !used[i.ID] && !keptImgs[i.ID] {
			// An unlabelled image the stack neither built nor runs from:
			// some other image on the daemon, no part of this stack.
			continue
		}
		nameOf := i.Name
		if nameOf == "" {
			nameOf = i.ID
		}
		if !removable {
			d.KeptImages = append(d.KeptImages, nameOf)
		} else {
			d.RemovedImages = append(d.RemovedImages, nameOf)
		}
	}
	sort.Strings(d.KeptImages)
	sort.Strings(d.RemovedImages)

	// The plan comes from the exact listings this detail was built from,
	// so what the dialogs promise is what a stop or removal on the same
	// daemon state would do.
	d.Plan = reaper.PlanFor(name, svcs, conts, nets, vols, imgs)

	if logService != "" {
		for _, c := range conts {
			if c.Labels[docker.ServiceLabel] == logService {
				logs, logErr := cli.ContainerLogs(ctx, c.ID, logLines)
				if logErr != nil {
					return Detail{}, logErr
				}
				for i := range logs {
					logs[i].Text = CleanLogText(logs[i].Text)
				}
				d.Logs, d.LogService = logs, logService
				break
			}
		}
	}
	return d, nil
}

// startCommand is the command to bring the stack back up, as the dashboard
// shows it. The working dir and the project name are quoted when they have
// a character outside [A-Za-z0-9_./-]; each config file is passed with -f,
// shown relative to the working dir, unless the config files are exactly
// one file with a default compose name directly in the working dir
// (compose finds it).
func startCommand(name, path string, configFiles []string) string {
	var b strings.Builder
	if path != "" {
		b.WriteString("cd ")
		b.WriteString(quotePath(path))
		b.WriteString(" && ")
	}
	b.WriteString("docker compose -p ")
	b.WriteString(quotePath(name))
	if !defaultComposeFile(path, configFiles) {
		for _, f := range configFiles {
			b.WriteString(" -f ")
			b.WriteString(quotePath(relativeConfigFile(path, f)))
		}
	}
	b.WriteString(" up -d")
	return b.String()
}

// defaultComposeFile reports whether the config files are exactly one file
// with a default compose name directly in the working dir: compose finds
// such a file without -f.
func defaultComposeFile(dir string, files []string) bool {
	if len(files) != 1 {
		return false
	}
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		if files[0] == filepath.Join(dir, name) {
			return true
		}
	}
	return false
}

// relativeConfigFile shows a config file inside the working dir relative
// to it; anything else is shown as is.
func relativeConfigFile(dir, file string) string {
	if dir == "" {
		return file
	}
	if r, ok := strings.CutPrefix(file, dir+"/"); ok {
		return r
	}
	return file
}

// quotePath single-quotes a path with any character outside
// [A-Za-z0-9_./-], doubling embedded quotes the shell way.
func quotePath(p string) string {
	for _, r := range p {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '.' || r == '/' || r == '-' {
			continue
		}
		return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return p
}
