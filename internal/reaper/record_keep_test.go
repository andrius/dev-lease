package reaper

// The never-remove label record must survive every lease transition the
// sweep performs: a stale stopping or removing mark, a revive, and a
// failed label refresh listing. These tests pin each transition, plus the
// sweep reading a re-created stopped container's label and the steady
// pass that must not rewrite the state file.

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// A downed stack labelled never-remove whose stop's container listing
// fails once: the stopping mark stays, the next pass reverts it to the
// stopped state without losing the recorded label 0, and the volume is
// never removed.
func TestStaleStoppingMarkKeepsRecordThroughRevert(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	if l := h.leases()["myapp"]; l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("setup: record = %v", l.LabelRemoveAfter)
	}
	h.srv.Drop("container", "c1")

	var armed atomic.Bool
	armed.Store(true)
	h.srv.OnRequest(func(method, path string) {
		if !armed.Load() || method != "GET" || !strings.HasSuffix(path, "/containers/json") {
			return
		}
		m, err := h.st.Read()
		if err == nil && m["myapp"].State == lease.StateStopping {
			h.srv.FailList("container", 500)
			armed.Store(false)
		}
	})
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t)
	h.srv.UnfailList("container")
	h.srv.OnRequest(nil)
	if l := h.leases()["myapp"]; l.State != lease.StateStopping {
		t.Fatalf("setup: state after the failed stop = %q, want stopping", l.State)
	}

	// Next pass: the stale mark is reverted and the expired lease stops
	// again in the same pass, with the record intact.
	h.cfg.Now = func() time.Time { return testNow.Add(2*time.Hour + time.Minute) }
	h.pass(t)
	l := h.leases()["myapp"]
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("the recorded label 0 was lost when the stale stopping mark was cleared: %v", l.LabelRemoveAfter)
	}
	if l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("lease after the revert = %+v, want stopped with the label delay 0", l)
	}

	// The sweep never removes it.
	h.cfg.Now = func() time.Time { return testNow.Add(5*time.Hour + 169*time.Hour) }
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the never-remove volume was deleted")
	}
}

// A stale removing mark cannot erase the label record its own stop read:
// the cleared lease's record passes to the replacement lease.
func TestStaleRemovingMarkKeepsLabelRecord(t *testing.T) {
	h := newHarness(t)
	zero := time.Duration(0)
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateRemoving, LabelRemoveAfter: &zero}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]", rep.Granted)
	}
	if l := h.leases()["myapp"]; l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("record after the clear and grant = %v, want 0 kept", l.LabelRemoveAfter)
	}

	// The granted lease expires; the stop has no containers and falls back
	// to the recorded label 0, so the removal never comes.
	h.cfg.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	h.pass(t)
	if l := h.leases()["myapp"]; l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("lease after the stop = %+v, want stopped with the label delay 0", l)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(3*time.Hour + 169*time.Hour) }
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the never-remove volume was deleted")
	}
}

// A stopped lease revived by a running container keeps the recorded label
// delay: the refresh's failed listing makes the inheritance observable.
func TestReviveKeepsLabelRecord(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{docker.ProjectLabel: "myapp"})
	h.srv.StopContainer("c1")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	after := 720 * time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-time.Hour), LabelRemoveAfter: &after}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A running container re-appears; the refresh listing (the third
	// containers listing of the pass) fails, so the record the pass ends
	// with is exactly what the revive inherited.
	h.srv.Add("container", "c2", "app", "myapp")
	var n atomic.Int32
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/containers/json") && n.Add(1) == 3 {
			h.srv.FailList("container", 500)
		}
	})
	h.pass(t)
	h.srv.OnRequest(nil)
	h.srv.UnfailList("container")

	l := h.leases()["myapp"]
	if l.State != "" {
		t.Fatalf("state = %q, want the revived live lease", l.State)
	}
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != after {
		t.Fatalf("record = %v, want the recorded %v kept through the revive", l.LabelRemoveAfter, after)
	}
}

// A failed label refresh listing keeps the recorded value and does not
// fail the pass.
func TestLabelRefreshListFailureKeepsRecord(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	if l := h.leases()["myapp"]; l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 720*time.Hour {
		t.Fatalf("setup: record = %v", l.LabelRemoveAfter)
	}

	// The refresh listing (the second containers listing of a steady
	// pass) fails: the pass completes and the record stays.
	var n atomic.Int32
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/containers/json") && n.Add(1) == 2 {
			h.srv.FailList("container", 500)
		}
	})
	h.pass(t)
	h.srv.OnRequest(nil)
	h.srv.UnfailList("container")

	if l := h.leases()["myapp"]; l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 720*time.Hour {
		t.Fatalf("record = %v, want 720h kept through the failed refresh listing", l.LabelRemoveAfter)
	}
}

// A stopped stack whose containers are re-created with the never-remove
// label without being started: the sweep reads the label, records 0 and
// never removes the stack.
func TestStoppedRecreatedZeroLabelCountsForSweep(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{docker.ProjectLabel: "myapp"})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t) // stop with serve's 168h
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("setup: %+v", l)
	}

	// compose up --no-start with a new label 0: a stopped container with
	// the label.
	h.srv.SetContainerLabels("c1", map[string]string{docker.RemoveAfterLabel: "0"})
	h.cfg.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	h.pass(t)
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" || l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("lease after the re-created label = %+v, want the merged label delay 0", l)
	}

	h.cfg.Now = func() time.Time { return testNow.Add(2*time.Hour + 169*time.Hour) }
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the sweep removed a stack whose containers carry the never-remove label")
	}
}

// A pass with nothing to change must not rewrite the state file.
func TestSteadyPassDoesNotRewriteState(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "c1", "myapp-web", "myapp")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", 10*time.Hour, testNow)
	h.pass(t) // the grant of the extended lease and the first refresh

	before, err := os.Stat(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	h.pass(t)
	after, err := os.Stat(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("a pass with nothing to change rewrote the state file (mtime %v -> %v)", before.ModTime(), after.ModTime())
	}
	if l := h.leases()["myapp"]; l.State != "" || len(l.Holders) != 1 {
		t.Fatalf("lease = %+v, want the live claim untouched", l)
	}
}

// The first pass both grants the default lease and records the label in
// it, so a compose down before the second pass loses nothing.
func TestLabelRecordedOnFirstPassForNewGrant(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.pass(t)
	l := h.leases()["myapp"]
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("record = %v, want 0 recorded on the granting pass", l.LabelRemoveAfter)
	}
	// The containers go away before any later pass; the recorded 0 is what
	// the stop applies, so the removal never comes.
	h.srv.Drop("container", "c1")
	h.cfg.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	h.pass(t)
	if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("lease after the stop = %+v, want the recorded label 0", l)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(3*time.Hour + 169*time.Hour) }
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the never-remove volume was deleted")
	}
}

// A stale stopping mark on an already stopped project must not reset the
// removal deadline: the revert keeps the original StoppedAt, so the
// removal runs at the original deadline, not a freshly granted one.
func TestStaleStoppingMarkKeepsRemovalDeadline(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	oneHour := time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow, RemoveAfter: &oneHour, RemoveAfterFrom: "label",
			LabelRemoveAfter: &oneHour}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// StopNow fails its container listing: the stopping mark stays.
	h.srv.FailList("container", 500)
	if _, err := h.runner().StopNow(context.Background(), "myapp", "ci"); err != nil {
		t.Fatalf("StopNow: %v", err)
	}
	h.srv.UnfailList("container")
	if l := h.leases()["myapp"]; l.State != lease.StateStopping {
		t.Fatalf("state = %q, want the stopping mark", l.State)
	}

	// The next pass reverts the stale mark and removes the stack at the
	// original deadline (testNow+1h), not a reset one.
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]: the stale mark must not reset the removal deadline", rep.Removed)
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the volume should be gone at the original deadline")
	}
}
