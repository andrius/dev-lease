package reaper

import (
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// Given a live stack whose container carries a remove-after label, every
// sweep records the label's delay in the lease while the containers are
// there, so a later compose down cannot erase the label before the stop.
// The record follows the current labels: it changes with them and is
// cleared when no container carries the label any more.
func TestLabelDelayFollowsTheLabel(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	})
	h.extend("myapp", time.Hour, testNow)

	h.pass(t)
	l := h.leases()["myapp"]
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 720*time.Hour {
		t.Fatalf("LabelRemoveAfter = %v, want 720h recorded on the first sweep", l.LabelRemoveAfter)
	}

	// The label changes: the next sweep re-reads it.
	h.srv.SetContainerLabels("c1", map[string]string{docker.RemoveAfterLabel: "0"})
	h.pass(t)
	l = h.leases()["myapp"]
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("LabelRemoveAfter = %v, want 0 after the label changed", l.LabelRemoveAfter)
	}

	// The label is dropped: the record is cleared, not frozen.
	h.srv.SetContainerLabels("c1", map[string]string{docker.RemoveAfterLabel: ""})
	h.pass(t)
	l = h.leases()["myapp"]
	if l.LabelRemoveAfter != nil {
		t.Fatalf("LabelRemoveAfter = %v, want nil once no container carries the label", *l.LabelRemoveAfter)
	}
}

// Given a live stack labelled never-remove (0) with a project volume, a
// sweep records the label while the containers are there. After a compose
// down (the containers are gone, the volume keeps the project present),
// the stop at expiry keeps the recorded label delay instead of falling
// back to serve's setting: the stopped lease never removes, so a sweep
// thirty days later still leaves the volume alone.
func TestLabelDelaySurvivesComposeDown(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)

	// While the containers are there, a sweep records the label delay.
	h.pass(t)
	l := h.leases()["myapp"]
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("LabelRemoveAfter = %v, want 0 recorded on the live stack", l.LabelRemoveAfter)
	}

	// compose down: the containers are gone, the volume keeps the project
	// present.
	h.srv.Drop("container", "c1")
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("setup: the volume must survive the down")
	}

	// The lease expires: the stop has no containers to read the label
	// from, so it keeps the recorded one.
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("expiry pass = %+v, want a clean stop", rep)
	}
	l = h.leases()["myapp"]
	if l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("lease after the stop = %+v, want the recorded label 0 kept", l)
	}

	// Thirty days later the sweep still leaves the never-remove volume
	// alone: the label survived the down.
	h.cfg.Now = func() time.Time { return testNow.Add(30 * 24 * time.Hour) }
	rep = h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none: the label 0 means never remove", rep.Removed)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the sweep removed the never-remove volume")
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want the stopped lease kept", l)
	}
}

// Given a stack whose recorded label delay is 720h with the containers
// gone, the stop at expiry takes the recorded value, and the removal runs
// once it has passed: the recorded delay is not just the never-remove 0
// case.
func TestLabelDelayAppliesAfterComposeDown(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)

	h.pass(t)
	h.srv.Drop("container", "c1")

	// Expiry: the stop records the 720h from the recorded label.
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("expiry pass removed the project: %+v", rep)
	}
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 720*time.Hour || l.RemoveAfterFrom != "label" {
		t.Fatalf("lease after the stop = %+v, want the recorded 720h label kept", l)
	}

	// Within the 720h nothing is removed.
	h.cfg.Now = func() time.Time { return testNow.Add(700 * time.Hour) }
	rep = h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none within the recorded 720h", rep.Removed)
	}

	// Past the recorded 720h the removal runs: the volume is gone and the
	// lease cleared. The stop ran at +2h, so the deadline is +722h.
	h.cfg.Now = func() time.Time { return testNow.Add(722 * time.Hour) }
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp] past the recorded 720h", rep.Removed)
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the volume survived past the recorded 720h")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the removal")
	}
}
