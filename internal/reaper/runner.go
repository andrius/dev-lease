// Runner wraps one dev-lease sweep and the interactive actions a control
// client (the dashboard, the MCP server) can run on single projects:
// stopping and removing immediately. The mutex serialises a sweep against
// the actions and the actions against each other; whoever holds it is the
// only one touching the daemon. A second caller waits for the first
// instead of racing it, and a caller whose context was cancelled meanwhile
// does nothing.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

var (
	// ErrNotManaged reports an action on a project dev-lease does not
	// manage: its name matches no pattern, or it is dev-lease's own
	// project.
	ErrNotManaged = errors.New("the project is not managed by dev-lease")
	// ErrBusy reports that the project is being stopped or removed right
	// now (a sweep or another action marked the lease) and the caller
	// should try again later.
	ErrBusy = errors.New("the project is being stopped or removed; try again later")
	// ErrNeverRemove reports a removal that was refused because the
	// project is never remove: serve's remove-after setting is 0, the
	// live stack's labels say 0, or the recorded delay is 0. Removing
	// needs RemoveOptions.AllowNeverRemove.
	ErrNeverRemove = errors.New("the project is never remove (serve's remove-after setting is 0, its labels say 0, or the recorded delay is 0); not removed without AllowNeverRemove")
	// ErrNoStack reports an action on a project with no lease and nothing
	// of it on the daemon: there is no stack to stop or remove.
	ErrNoStack = errors.New("no stack for the project is present on the daemon")
)

// RemoveOptions tweaks RemoveNow.
type RemoveOptions struct {
	// Via tags the action's log lines (the sweep leaves it empty).
	Via string
	// AllowNeverRemove removes a project that is never remove: serve's
	// remove-after setting is 0, its containers' labels say 0, or its
	// recorded removal delay is 0. The dashboard must let the user pick
	// this explicitly.
	AllowNeverRemove bool
}

// Runner runs sweeps and per-project actions on one daemon and store.
type Runner struct {
	// mu serialises Pass, StopNow and RemoveNow; held with Lock for the
	// whole duration of each, so a second caller waits for the first
	// instead of racing it.
	mu sync.Mutex

	Cli   *docker.Client
	Store lease.Store
	Cfg   Config
}

// Pass runs one sweep. It waits for an action or another pass that holds
// the runner.
func (r *Runner) Pass(ctx context.Context) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return PassWith(ctx, r.Cli, r.Store, r.Cfg, "")
}

// StopNow stops the project immediately: every claim is released, the
// lease goes to stopped (StoppedAt kept from an earlier stop) and the
// containers are stopped; nothing is removed. A project with no lease but
// a stack on the daemon is stopped and recorded like any other; a name
// with neither reports ErrNoStack. It refuses ErrBusy when the project is
// already being stopped or removed, and reports errors from the store or
// a failed container listing.
func (r *Runner) StopNow(ctx context.Context, project, via string) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A caller that gave up while queued behind a sweep must not run the
	// action when its turn comes.
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	return r.stopNowLocked(ctx, project, via)
}

// RemoveNow stops the project (like StopNow) and removes it from the
// daemon in this one call, whatever the recorded removal deadline - except
// when the project is never remove: serve's remove-after setting is 0, the
// live stack's labels say 0, or the recorded delay is 0. Then the removal
// is refused (before the stop, when the refusal is known up front) and
// needs AllowNeverRemove. The lease entry is deleted like on a sweep
// removal. A project with no lease and nothing on the daemon reports
// ErrNoStack.
func (r *Runner) RemoveNow(ctx context.Context, project string, opts RemoveOptions) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A caller that gave up while queued behind a sweep must not run the
	// action when its turn comes.
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	return r.removeNowLocked(ctx, project, opts)
}

// stopNowLocked runs a StopNow with the runner's mutex held.
func (r *Runner) stopNowLocked(ctx context.Context, project, via string) (Report, error) {
	if !Managed(project, r.Cfg) {
		return Report{}, ErrNotManaged
	}
	cfg := r.cfg()
	now, log := cfg.Now, cfg.Log
	var rep Report
	// The stack-existence check runs before the never-remove one and
	// before the store is touched, so a project with no lease but a
	// stack on the daemon gets a fresh lease (state stopping, no claims)
	// and runs through the stop below, while a name with neither reports
	// ErrNoStack.
	leases, err := r.Store.Read()
	if err != nil {
		return rep, err
	}
	if _, hasLease := leases[project]; !hasLease {
		empty, err := projectEmpty(ctx, r.Cli, project, nil)
		if err != nil {
			return rep, err
		}
		if empty {
			return rep, ErrNoStack
		}
	}
	// Mark the lease stopping and release every claim under the store
	// lock: from here on extend is refused, and the action's outcome is
	// the only thing that can change the lease. A stopped project is
	// stopped again (its containers may have been started since); its
	// StoppedAt stays. An unknown mark is left alone: it is not this
	// action's to clear.
	if err := r.Store.Update(func(m map[string]lease.Lease) error {
		l, ok := m[project]
		if !ok {
			m[project] = lease.Lease{Project: project, State: lease.StateStopping}
			return nil
		}
		switch l.State {
		case lease.StateStopping, lease.StateRemoving:
			return ErrBusy
		case "", lease.StateStopped:
			// handled below
		default:
			return fmt.Errorf("%s: lease is marked %q, not by this serve", project, l.State)
		}
		l = lease.Release(l, "", true, now())
		l.State = lease.StateStopping
		m[project] = l
		return nil
	}); err != nil {
		return rep, err
	}
	skipped, failed, err := stopMarked(ctx, r.Cli, r.Store, project, cfg, now, log, via)
	if err != nil {
		return rep, err
	}
	rep.Skipped = append(rep.Skipped, skipped...)
	rep.Failed = append(rep.Failed, failed...)
	return rep, nil
}

// removeNowLocked runs a RemoveNow with the runner's mutex held.
func (r *Runner) removeNowLocked(ctx context.Context, project string, opts RemoveOptions) (Report, error) {
	if !Managed(project, r.Cfg) {
		return Report{}, ErrNotManaged
	}
	cfg := r.cfg()
	now, log := cfg.Now, cfg.Log
	var rep Report
	// Read the lease first, like the sweep's removal does: a busy one is
	// ErrBusy.
	leases, err := r.Store.Read()
	if err != nil {
		return rep, err
	}
	l, hasLease := leases[project]
	if hasLease {
		switch l.State {
		case lease.StateStopping, lease.StateRemoving:
			return rep, ErrBusy
		}
	}

	// A name with neither a lease nor a stack is ErrNoStack before the
	// never-remove check: nothing exists, so nothing can be never-remove.
	if !hasLease {
		empty, err := projectEmpty(ctx, r.Cli, project, nil)
		if err != nil {
			return rep, err
		}
		if empty {
			return rep, ErrNoStack
		}
	}

	// Never remove is decided before anything changes: the serve setting,
	// the labels and the recorded stop are read first, so a refusal leaves
	// claims, state and the daemon untouched.
	if !opts.AllowNeverRemove {
		if cfg.RemoveAfter == 0 {
			act(log, opts.Via, now(), project, "remove", "", "", ErrNeverRemove)
			return rep, ErrNeverRemove
		}
		conts, err := r.Cli.Containers(ctx, project)
		if err != nil {
			return rep, err
		}
		d, _ := removeAfterFor(conts, cfg.RemoveAfter)
		// The recorded label counts only when no container is left to
		// read it from, as in the views; present containers speak for
		// themselves through d.
		labelZero := hasLease && len(conts) == 0 && l.LabelRemoveAfter != nil && *l.LabelRemoveAfter == 0
		recordedZero := hasLease && !l.StoppedAt.IsZero() && l.RemoveAfter != nil &&
			*l.RemoveAfter == 0
		effectiveZero := hasLease && l.EffectiveRemovalDelay(cfg.RemoveAfter) == 0
		if d == 0 || labelZero || recordedZero || effectiveZero {
			act(log, opts.Via, now(), project, "remove", "", "", ErrNeverRemove)
			return rep, ErrNeverRemove
		}
	}

	// Stop first, whatever the recorded state: the removal must never
	// delete a running container, and a container may have been started
	// since the recorded stop.
	stopRep, err := r.stopNowLocked(ctx, project, opts.Via)
	if err != nil {
		return rep, err
	}
	rep.Skipped = append(rep.Skipped, stopRep.Skipped...)
	rep.Failed = append(rep.Failed, stopRep.Failed...)

	// The stop recorded the effective removal delay. A 0 (never remove)
	// refuses the removal unless the caller allowed it explicitly. The
	// error goes to the caller; the project stays stopped.
	leases, err = r.Store.Read()
	if err != nil {
		return rep, err
	}
	l, ok := leases[project]
	if !ok {
		return rep, ErrNoStack
	}
	if l.State != lease.StateStopped {
		return rep, fmt.Errorf("%s: lease is %q after the stop, cannot remove", project, l.State)
	}
	if d := l.EffectiveRemovalDelay(cfg.RemoveAfter); d <= 0 && !opts.AllowNeverRemove {
		act(log, opts.Via, now(), project, "remove", "", "", ErrNeverRemove)
		return rep, ErrNeverRemove
	}

	// Remove with the due check forced off: RemoveNow removes whatever
	// the recorded deadline says, and the never-remove refusal ran above.
	skipped, failed, removed, err := removeProject(ctx, r.Cli, r.Store, project, cfg, now, log, opts.Via, true)
	if err != nil {
		return rep, err
	}
	rep.Skipped = append(rep.Skipped, skipped...)
	rep.Failed = append(rep.Failed, failed...)
	if removed {
		rep.Removed = append(rep.Removed, project)
	}
	return rep, nil
}

// cfg returns the runner's config with the defaults PassWith applies, so
// the actions and the sweep agree on every setting.
func (r *Runner) cfg() Config {
	cfg := r.Cfg
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 2 * time.Hour
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	return cfg
}
