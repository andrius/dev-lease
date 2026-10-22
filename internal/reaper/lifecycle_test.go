package reaper

import (
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
)

// The stop/revive/remove lifecycle introduced with the stopped state:
// expiry stops a project and removes nothing, a running container revives
// it with the default lease, and the removal runs only once RemoveAfter
// has passed since the stop.

// Given a project whose lease expired, the pass stops its containers and
// removes nothing: containers, network, volume and image all stay, the
// lease records the stopped state with StoppedAt, and no DELETE reaches
// the daemon.
func TestPassStopsExpiredProjectAndRemovesNothing(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 || len(rep.Skipped) != 0 {
		t.Fatalf("pass = %+v, want a clean stop", rep)
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "DELETE" {
			t.Fatalf("the expiry pass deleted %s %s", call.Method, call.Path)
		}
	}
	if !h.srv.IsStopped("container", "c1") || !h.srv.IsStopped("container", "c2") {
		t.Fatal("the containers were not stopped")
	}
	for _, obj := range [][2]string{{"network", "n1"}, {"volume", "myapp_data"}, {"image", "sha256:i1"}} {
		if !h.srv.Has(obj[0], obj[1]) {
			t.Errorf("%s %s was removed at expiry", obj[0], obj[1])
		}
	}
	l, ok := h.leases()["myapp"]
	if !ok || l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want the stopped state", l)
	}
	if !l.StoppedAt.Equal(testNow) {
		t.Fatalf("StoppedAt = %v, want %v", l.StoppedAt, testNow)
	}
}

// Given a project with one container already stopped at expiry, the pass
// stops only the running one.
func TestPassStopsOnlyRunningContainers(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.StopContainer("c1") // the daemon already has it stopped
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	h.pass(t)
	stops := 0
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" && strings.HasSuffix(call.Path, "/stop") {
			stops++
			if strings.HasPrefix(call.Path, "/v1.47/containers/c1/") {
				t.Fatal("the already stopped container was stopped again")
			}
		}
	}
	if stops != 1 {
		t.Fatalf("stop calls = %d, want 1 (only the running c2)", stops)
	}
}

// Given a stopped project, the pass leaves it alone while RemoveAfter has
// not passed since StoppedAt.
func TestPassStoppedProjectWaitsForRemoveAfter(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // stop

	h.cfg.Now = func() time.Time { return testNow.Add(24 * time.Hour) } // within 168h
	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 || len(rep.Granted) != 0 {
		t.Fatalf("pass within RemoveAfter = %+v, want nothing touched", rep)
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("state = %q, want the stopped lease untouched", l.State)
	}
	if h.srv.Count("container") != 2 {
		t.Fatal("objects were touched before RemoveAfter passed")
	}

	// Once RemoveAfter has passed, the removal runs.
	h.cfg.Now = func() time.Time { return testNow }
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.StoppedAt = testNow.Add(-h.cfg.RemoveAfter)
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
}

// Given RemoveAfter 0, a stopped project is never removed.
func TestPassRemoveAfterZeroNeverRemoves(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 0
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // stop

	h.cfg.Now = func() time.Time { return testNow.Add(48 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("pass = %+v, want no removal with RemoveAfter 0", rep)
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("state = %q, want the stopped lease kept", l.State)
	}
	if h.srv.Count("container") != 2 {
		t.Fatal("objects were removed although RemoveAfter is 0")
	}
}

// Given a stopped project with a running container again (the developer
// ran compose up or start), the pass revives it: the stopped lease is
// deleted, the default lease is granted and nothing is removed.
func TestPassReviveRunningContainer(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // stop

	h.srv.StartContainer("c1")
	rep := h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]: the running container must revive the project", rep.Granted)
	}
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("pass = %+v, want a clean revival", rep)
	}
	l, ok := h.leases()["myapp"]
	if !ok || l.State != "" {
		t.Fatalf("lease = %+v, want a fresh default lease", l)
	}
	if got, want := l.ExpiresAt(), testNow.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("expiry = %v, want the default %v", got, want)
	}
	for _, obj := range [][2]string{{"container", "c1"}, {"container", "c2"}, {"network", "n1"}, {"volume", "myapp_data"}} {
		if !h.srv.Has(obj[0], obj[1]) {
			t.Errorf("%s %s was removed during the revival", obj[0], obj[1])
		}
	}
}

// Given the revival above, the default lease expires like any other: the
// pass stops the project again instead of removing it.
func TestPassRevivedLeaseReStopsAtExpiry(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // stop
	h.srv.StartContainer("c1")
	h.pass(t) // revive: default lease until testNow+2h

	h.cfg.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none: the revived lease's expiry stops, not removes", rep.Removed)
	}
	if !h.srv.IsStopped("container", "c1") {
		t.Fatal("the revived project was not stopped at its lease expiry")
	}
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped {
		t.Fatalf("state = %q, want stopped", l.State)
	}
	if !l.StoppedAt.Equal(testNow.Add(3 * time.Hour)) {
		t.Fatalf("StoppedAt = %v, want %v", l.StoppedAt, testNow.Add(3*time.Hour))
	}
}

// Given an expired project whose container listing fails during the stop
// phase, the project is reported in Failed and the stopping mark stays:
// the next pass clears it like a stale lock and treats the project like
// any other.
func TestPassFailedStopListingLeavesStoppingMark(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	// Listing order: 1 = Projects, 2 = the stop phase's listing.
	failNthList(h, "container", 2, 500)

	rep := h.pass(t)
	if len(rep.Failed) != 1 || rep.Failed[0] != "myapp" {
		t.Fatalf("Failed = %v, want [myapp]", rep.Failed)
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopping {
		t.Fatalf("state = %q, want the stopping mark kept after the failed listing", l.State)
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("the project was touched although the listing failed")
	}

	// The next pass clears the stale stopping mark and treats the project
	// like any other: the reverted lease is expired, so the same pass
	// stops it, and nothing is removed.
	h.srv.UnfailList("container")
	rep = h.pass(t)
	if len(rep.Granted) != 0 {
		t.Fatalf("Granted = %v, want none: the reverted lease stays, nothing is granted", rep.Granted)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", rep.Removed)
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("state = %q, want stopped: the expired reverted lease is stopped in the same pass", l.State)
	}
}

// Given a stopping lease at pass start (a serve died mid-stop), the pass
// clears the lock and treats the project like any other: a present
// project gets the default lease and keeps its objects.
func TestPassClearsStaleStoppingLease(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = lease.StateStopping
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("pass = %+v, want no removal", rep)
	}
	if len(rep.Granted) != 0 {
		t.Fatalf("Granted = %v, want none: the lease is reverted, not replaced", rep.Granted)
	}
	if h.srv.Count("container") != 2 {
		t.Fatal("the project's objects were removed")
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("state = %q, want stopped: the expired reverted lease is stopped in the same pass", l.State)
	}
}
