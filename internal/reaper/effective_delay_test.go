package reaper

import (
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// A stack stopped under serve's setting with no label: restarting serve
// with a SHORTER --remove-after must not make the stopped stack go sooner;
// the longer stored value stays in force.
func TestShorterServeRemoveAfterDoesNotShortenStoppedStack(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // stopped under serve's 168h
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != 168*time.Hour || l.RemoveAfterFrom != "serve" {
		t.Fatalf("stored = %v from %q, want 168h from serve", l.RemoveAfter, l.RemoveAfterFrom)
	}
	h.cfg.RemoveAfter = time.Hour // serve restarted with a shorter setting
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("removed 2h after the stop although the stored delay is 168h and no label asked for less: %+v", rep)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the stack's volume is gone")
	}
}

// A label-set delay is the stored label value: a later serve setting,
// longer or shorter, does not change it.
func TestLabelRemoveAfterIsNotChangedByServe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve time.Duration
	}{
		{"a longer serve setting does not lengthen", 720 * time.Hour},
		{"a shorter serve setting does not shorten", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			fullProject(h.srv)
			h.srv.AddLabels("container", "c1", "web", map[string]string{
				docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "7d",
			})
			h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
			h.pass(t)
			l := h.leases()["myapp"]
			if l.RemoveAfter == nil || *l.RemoveAfter != 7*24*time.Hour || l.RemoveAfterFrom != "label" {
				t.Fatalf("stored = %v from %q, want 7d from label", l.RemoveAfter, l.RemoveAfterFrom)
			}
			h.cfg.RemoveAfter = tc.serve
			// Two hours after the stop: past a 1h serve setting, far before the
			// label's 7d. The stack must still be there.
			h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
			if rep := h.pass(t); len(rep.Removed) != 0 || !h.srv.Has("volume", "myapp_data") {
				t.Fatalf("removed 2h after the stop under serve %v although the label says 7d: %+v", tc.serve, rep)
			}
			h.cfg.Now = func() time.Time { return testNow.Add(7*24*time.Hour + time.Hour) }
			rep := h.pass(t)
			if !contains(rep.Removed, "myapp") {
				t.Fatalf("removed at 169h = %v, want the stack removed once the label's 7d passed", rep.Removed)
			}
		})
	}
}

// The global off switch always wins: serve restarted with --remove-after 0
// keeps a label-set delay from ever removing the stack.
func TestLabelRemoveAfterOverriddenByGlobalZero(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "7d",
	})
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	h.cfg.RemoveAfter = 0
	h.cfg.Now = func() time.Time { return testNow.Add(200 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 || !h.srv.Has("volume", "myapp_data") {
		t.Fatalf("serve runs with --remove-after 0 but a label-stopped stack was removed: %+v", rep)
	}
}

// A lease without a recorded delay or origin is
// serve-set with serve's current setting: it waits out the full delay and
// is removed at the deadline.
func TestOldLeaseWithoutStoredDelayUsesServeSetting(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.RemoveAfter = nil
		l.RemoveAfterFrom = ""
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(167 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("removed at 167h although the lease carries no stored delay and serve says 168h: %+v", rep)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(169 * time.Hour) }
	rep = h.pass(t)
	if !contains(rep.Removed, "myapp") {
		t.Fatalf("removed at 169h = %v, want the stack removed once serve's 168h passed", rep.Removed)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
