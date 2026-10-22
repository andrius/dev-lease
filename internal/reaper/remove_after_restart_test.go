package reaper

import (
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
)

// The operator turns removal off (serve restarted with --remove-after 0).
// A stack stopped before the change, with no label, must not be removed:
// serve's current 0 overrides the delay recorded at the stop.
func TestRemoveAfterZeroAfterRestartStopsRemovals(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // stopped under serve's 168h
	l := h.leases()["myapp"]
	t.Logf("stored remove_after after the stop: %v", l.RemoveAfter)
	h.cfg.RemoveAfter = 0 // serve restarted with --remove-after 0
	h.ageStopped(t)
	// age by 168h explicitly (ageStopped used cfg.RemoveAfter = 0)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		x := m["myapp"]
		x.StoppedAt = x.StoppedAt.Add(-200 * time.Hour)
		m["myapp"] = x
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rep := h.pass(t)
	if len(rep.Removed) != 0 || !h.srv.Has("volume", "myapp_data") {
		t.Fatalf("serve runs with --remove-after 0, but a stack stopped before the change was removed: %+v", rep)
	}
}

// The operator raises --remove-after from 168h to 720h; a stack stopped
// before the change follows the longer current setting, so it is not
// removed at 169h.
func TestRaisedRemoveAfterAppliesToStoppedStack(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != 168*time.Hour || l.RemoveAfterFrom != "serve" {
		t.Fatalf("stored = %v from %q, want 168h from serve", l.RemoveAfter, l.RemoveAfterFrom)
	}
	h.cfg.RemoveAfter = 720 * time.Hour
	h.cfg.Now = func() time.Time { return testNow.Add(169 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("removed at 169h although serve now says 720h and no label asked for less: %+v", rep)
	}
}
