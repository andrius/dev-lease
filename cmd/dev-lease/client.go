package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
	"github.com/andrius/dev-lease/internal/view"
)

// exitErr carries a non-zero exit code and its message.
type exitErr struct {
	code int
	msg  string
}

func (e exitErr) Error() string { return e.msg }

// managedByHeader decides whether the client may touch a project: the
// patterns live in the state file's header, written by serve. It returns 0
// when the project is managed, 1 when the state file cannot be read, and 2
// when the project is not managed (or no patterns exist).
func managedByHeader(st lease.Store, project string, w io.Writer) int {
	hdr, err := st.ReadHeader()
	if err != nil {
		fmt.Fprintf(w, "cannot read the state file %s: %v\n", st.Path, err)
		return 1
	}
	if len(hdr.Patterns) == 0 {
		fmt.Fprintln(w, "no managed project patterns in the state file; run dev-lease serve first")
		return 2
	}
	if !reaper.Managed(project, reaper.Config{Patterns: hdr.Patterns}) {
		fmt.Fprintf(w, "%s does not match any managed pattern (%s)\n", project, strings.Join(hdr.Patterns, ", "))
		return 2
	}
	return 0
}

func defaultHolder() string {
	if v := os.Getenv("DEV_LEASE_HOLDER"); v != "" {
		return v
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "local"
}

// runExtend extends the lease of a managed project. Exit codes: 0 ok, 1 bad
// ttl or unreadable state file, 2 not managed, 3 the project is being
// stopped or removed (the reaper is mid-flight on it). Extending a stopped
// project revives it: the stopped mark is cleared and the lease is live
// again; the message tells the developer to start the stack, because
// dev-lease never starts containers.
func runExtend(w io.Writer, st lease.Store, project, ttlStr, holder string) int {
	ttl, err := parseDuration(ttlStr)
	if err != nil {
		fmt.Fprintf(w, "cannot parse ttl %q: %v\n", ttlStr, err)
		return 1
	}
	if err := lease.ValidateTTL(ttl); err != nil {
		fmt.Fprintln(w, err)
		return 1
	}
	if code := managedByHeader(st, project, w); code != 0 {
		return code
	}
	if holder == "" {
		holder = defaultHolder()
	}
	var revived bool
	err = st.Update(func(m map[string]lease.Lease) error {
		var err error
		revived, err = lease.ExtendProject(m, project, holder, ttl, time.Now().UTC())
		if errors.Is(err, lease.ErrBeingStopped) {
			return exitErr{code: 3, msg: fmt.Sprintf("%s is being stopped; retry in a moment", project)}
		}
		if errors.Is(err, lease.ErrBeingRemoved) {
			return exitErr{code: 3, msg: fmt.Sprintf("%s is being removed; run again when it is gone, it will start clean", project)}
		}
		return err
	})
	var ee exitErr
	if errors.As(err, &ee) {
		fmt.Fprintln(w, ee.msg)
		return ee.code
	}
	if err != nil {
		fmt.Fprintln(w, err)
		return 1
	}
	if revived {
		fmt.Fprintln(w, "the project was stopped; dev-lease never starts containers - run docker compose up -d to start the stack")
	}
	fmt.Fprintf(w, "extended %s until %s (holder %s)\n",
		project, holderExpiry(st, project, holder).Format(time.RFC3339), holder)
	return 0
}

// headerRemoveAfter reads the header's remove-after best-effort; 0 when
// the header is unreadable.
func headerRemoveAfter(st lease.Store) time.Duration {
	if hdr, err := st.ReadHeader(); err == nil {
		return hdr.RemoveAfter
	}
	return 0
}

// runRelease ends one holder's claim or every claim of a managed project.
// Releasing a project without a lease changes nothing and succeeds. A
// stopped project stays stopped: release never revives it, and the message
// says when (or that never) it is removed.
func runRelease(w io.Writer, st lease.Store, project, holder string, all bool) int {
	if holder == "" && !all {
		fmt.Fprintln(w, "release: give --holder <name> or --all")
		return 1
	}
	if code := managedByHeader(st, project, w); code != 0 {
		return code
	}
	now := time.Now().UTC()
	var ee exitErr
	var released lease.Lease
	err := st.Update(func(m map[string]lease.Lease) error {
		l, ok := m[project]
		if !ok {
			// Gone between the check and this update: nothing to release.
			return exitErr{code: 0, msg: fmt.Sprintf("no lease for %s; nothing to release", project)}
		}
		l.Project = project
		released = lease.Release(l, holder, all, now)
		m[project] = released
		return nil
	})
	if errors.As(err, &ee) {
		fmt.Fprintln(w, ee.msg)
		return ee.code
	}
	if err != nil {
		fmt.Fprintln(w, err)
		return 1
	}
	if all {
		if released.State == lease.StateStopped {
			fmt.Fprintf(w, "released every claim of %s; it stays stopped (%s)\n", project, leaseLine(released, now, headerRemoveAfter(st)))
		} else {
			fmt.Fprintf(w, "released every claim of %s; the next pass stops the project\n", project)
		}
	} else {
		fmt.Fprintf(w, "released claim of %s by %s\n", project, holder)
	}
	return 0
}

// runList prints the leases with time left and the holders. A stopped
// project's removal time needs serve's current setting from the header,
// read best-effort: without it the setting counts as 0 and a serve-set
// stopped project shows as never removed.
func runList(w io.Writer, st lease.Store) int {
	leases, err := st.Read()
	if err != nil {
		fmt.Fprintln(w, err)
		return 1
	}
	fallback := headerRemoveAfter(st)
	projects := make([]string, 0, len(leases))
	for p := range leases {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	now := time.Now().UTC()
	for _, p := range projects {
		fmt.Fprintf(w, "%s: %s\n", p, leaseLine(leases[p], now, fallback))
	}
	return 0
}

// runStatus prints the same listing serve prints at start. It exits 1 when
// the daemon listing, a per-project listing or the state file cannot be
// read, so scripts can tell a broken listing from an empty one.
func runStatus(w, errW io.Writer, st lease.Store, socket string) int {
	hdr, err := st.ReadHeader()
	if err != nil {
		fmt.Fprintln(errW, err)
		return 1
	}
	if err := startListingWithSocket(socket, st, hdr.Patterns, w); err != nil {
		return 1
	}
	return 0
}

// holderExpiry returns the given holder's own claim expiry: what the
// extend actually grants that holder. The zero time when the holder or
// the lease is missing.
func holderExpiry(st lease.Store, project, holder string) time.Time {
	leases, err := st.Read()
	if err != nil {
		return time.Time{}
	}
	for _, h := range leases[project].Holders {
		if h.Name == holder {
			return h.Expires
		}
	}
	return time.Time{}
}

// leaseLine renders the human part of a listing line: the live form (time
// left until expiry, with the holders whose claim is still live), the
// stopping and removing locks, the stopped form (how long ago, removed in
// how long or never) and the expired form naming the last holder. The
// fallback is the header's remove-after, serve's current setting.
func leaseLine(l lease.Lease, now time.Time, removeAfterFallback time.Duration) string {
	switch l.State {
	case lease.StateRemoving:
		return "removing"
	case lease.StateStopping:
		return "stopping"
	case lease.StateStopped:
		ago := view.FmtDur(now.Sub(l.StoppedAt))
		ra := l.EffectiveRemovalDelay(removeAfterFallback)
		if ra <= 0 {
			return fmt.Sprintf("stopped %s ago; never removed", ago)
		}
		removedAt := l.StoppedAt.Add(ra)
		if !now.Before(removedAt) {
			return fmt.Sprintf("stopped %s ago; removal overdue (at %s)", ago, removedAt.Format(time.RFC3339))
		}
		return fmt.Sprintf("stopped %s ago; removed in %s (at %s)", ago,
			view.FmtDur(removedAt.Sub(now)), removedAt.Format(time.RFC3339))
	}
	if !l.Expired(now) {
		left := view.FmtDur(l.ExpiresAt().Sub(now))
		if holders := liveHolderNames(l, now); len(holders) > 0 {
			return fmt.Sprintf("live, %s left (until %s; holders: %s)",
				left, l.ExpiresAt().Format(time.RFC3339), strings.Join(holders, ", "))
		}
		return fmt.Sprintf("live, %s left (until %s)", left, l.ExpiresAt().Format(time.RFC3339))
	}
	holder := lastHolder(l)
	if holder == "" {
		return "expired"
	}
	return fmt.Sprintf("expired (last holder %s)", holder)
}

// liveHolderNames lists the names of the holders whose claim is still live,
// in extension order.
func liveHolderNames(l lease.Lease, now time.Time) []string {
	var out []string
	for _, h := range l.Holders {
		if h.Expires.After(now) {
			out = append(out, h.Name)
		}
	}
	return out
}

func lastHolder(l lease.Lease) string {
	var last time.Time
	var name string
	for _, h := range l.Holders {
		if h.ExtendedAt.After(last) {
			last = h.ExtendedAt
			name = h.Name
		}
	}
	return name
}

func startListingWithSocket(socket string, st lease.Store, patterns []string, w io.Writer) error {
	return startListing(context.Background(), docker.New(socket), st, patterns, "", w)
}
