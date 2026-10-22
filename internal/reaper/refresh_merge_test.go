package reaper

import (
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// stoppedUnderServe writes a stopped myapp lease whose exited containers
// and volume are still on the daemon. recorded is the RemoveAfter the
// stop wrote (nil for a legacy lease); label, when non-empty, is the
// containers' dev-lease.remove-after value.
func stoppedUnderServe(t *testing.T, h *harness, recorded *time.Duration, from, label string) {
	t.Helper()
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp",
	})
	h.srv.StopContainer("c1")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	if label != "" {
		h.srv.SetContainerLabels("c1", map[string]string{docker.RemoveAfterLabel: label})
	}
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{
			Project: "myapp", State: lease.StateStopped, StoppedAt: testNow,
			RemoveAfter: recorded, RemoveAfterFrom: from,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A stack stopped with serve's 168h recorded is swept by a serve running
// --remove-after 0, with the exited containers still on the daemon and no
// label on them: the refresh must not merge serve's 0 into the recorded
// stop, and once serve is back at 168h the stack is removed at its stop +
// 168h, not kept forever.
func TestPassRefreshMergeKeepsServeRecordUnderZero(t *testing.T) {
	h := newHarness(t)
	after := 168 * time.Hour
	stoppedUnderServe(t, h, &after, "serve", "")

	h.cfg.RemoveAfter = 0
	for _, at := range []time.Time{testNow.Add(2 * time.Hour), testNow.Add(30 * 24 * time.Hour)} {
		h.cfg.Now = func() time.Time { return at }
		if rep := h.pass(t); len(rep.Removed) != 0 {
			t.Fatalf("removed under serve 0: %+v", rep)
		}
	}
	if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 168*time.Hour || l.RemoveAfterFrom != "serve" {
		t.Fatalf("the serve-0 passes overwrote the recorded stop: %+v", l)
	}

	h.cfg.RemoveAfter = 168 * time.Hour
	h.cfg.Now = func() time.Time { return testNow.Add(167 * time.Hour) }
	if rep := h.pass(t); len(rep.Removed) != 0 {
		t.Fatalf("removed before the recorded 168h: %+v", rep)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(168 * time.Hour) }
	if rep := h.pass(t); len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("not removed at stop + 168h: %+v", rep)
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("volume left behind")
	}
}

// A legacy stopped lease with no recorded RemoveAfter and no label, swept
// by serve 0 and then serve 168h: a nil record follows serve's setting,
// so the stack is removed at its stop + 168h, never earlier and never
// kept forever.
func TestPassLegacyStoppedLeaseNilRecordSurvivesServeZero(t *testing.T) {
	h := newHarness(t)
	stoppedUnderServe(t, h, nil, "", "")

	h.cfg.RemoveAfter = 0
	for _, at := range []time.Time{testNow.Add(2 * time.Hour), testNow.Add(30 * 24 * time.Hour)} {
		h.cfg.Now = func() time.Time { return at }
		if rep := h.pass(t); len(rep.Removed) != 0 {
			t.Fatalf("removed under serve 0: %+v", rep)
		}
	}
	if l := h.leases()["myapp"]; l.RemoveAfter != nil {
		t.Fatalf("the serve-0 passes recorded a delay on the nil record: %+v", l)
	}

	h.cfg.RemoveAfter = 168 * time.Hour
	h.cfg.Now = func() time.Time { return testNow.Add(167 * time.Hour) }
	if rep := h.pass(t); len(rep.Removed) != 0 {
		t.Fatalf("removed before serve's 168h: %+v", rep)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(168 * time.Hour) }
	if rep := h.pass(t); len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("not removed at stop + 168h: %+v", rep)
	}
}

// A legacy stopped lease with no recorded RemoveAfter whose containers
// carry dev-lease.remove-after=10d: the refresh merge records 10d with
// origin "label", and the removal keeps following it even when serve is
// raised to 20d afterwards (a label-set delay is fixed).
func TestPassRefreshMergeRecordsLabelOnLegacyStoppedLease(t *testing.T) {
	h := newHarness(t)
	stoppedUnderServe(t, h, nil, "", "10d")

	h.cfg.Now = func() time.Time { return testNow.Add(time.Hour) }
	h.pass(t)
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != 10*24*time.Hour || l.RemoveAfterFrom != "label" {
		t.Fatalf("the merge did not record the label's value with origin label: %+v", l)
	}
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 10*24*time.Hour {
		t.Fatalf("the label record: %+v", l)
	}

	h.cfg.RemoveAfter = 20 * 24 * time.Hour
	h.cfg.Now = func() time.Time { return testNow.Add(9*24*time.Hour + 23*time.Hour) }
	if rep := h.pass(t); len(rep.Removed) != 0 {
		t.Fatalf("removed before the label's 10d: %+v", rep)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(10 * 24 * time.Hour) }
	if rep := h.pass(t); len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("not removed at stop + 10d: %+v", rep)
	}
}
