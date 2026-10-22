package reaper

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// The developer runs `docker compose start` after the removal listed the
// containers (all stopped) and before it stops them. The removal must not
// stop and delete the container it just saw started.
func TestStartAfterRemovalListingIsNotKilled(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "o1", "other-web", "other")
	h.srv.Add("volume", "ov", "other_data", "other")
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // stopped
	if !h.srv.IsStopped("container", "o1") {
		t.Fatal("setup: o1 not stopped")
	}
	h.ageStopped(t)
	var mu sync.Mutex
	nets := 0
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/networks") {
			mu.Lock()
			nets++
			n := nets
			mu.Unlock()
			if n == 2 { // the removal's network listing, after its container listing
				h.srv.StartContainer("o1") // docker compose -p other start
			}
		}
	})
	h.pass(t)
	if !h.srv.Has("container", "o1") {
		t.Fatalf("the container the developer started during the removal was stopped and deleted; log:\n%s", h.log.String())
	}
}

// Control: the same start before the removal's container listing is
// already handled by the running re-check (the harness can pass).
func TestControlStartBeforeListing(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "o1", "other-web", "other")
	h.srv.Add("volume", "ov", "other_data", "other")
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	h.ageStopped(t)
	stoppedAt := h.leases()["other"].StoppedAt
	var mu sync.Mutex
	conts := 0
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/containers/json") {
			mu.Lock()
			conts++
			n := conts
			mu.Unlock()
			if n == 3 { // projects, revive check, then the removal's listing
				h.srv.StartContainer("o1")
			}
		}
	})
	h.pass(t)
	if !h.srv.Has("container", "o1") {
		t.Fatalf("control failed; log:\n%s", h.log.String())
	}
	// The revert sets the state back to stopped, keeping StoppedAt: the
	// next pass revives the project instead of restarting the removal
	// clock.
	l, ok := h.leases()["other"]
	if !ok || l.State != lease.StateStopped {
		t.Fatalf("lease after the revert = %+v, want stopped", l)
	}
	if !l.StoppedAt.Equal(stoppedAt) {
		t.Fatalf("StoppedAt = %v after the revert, want %v unchanged", l.StoppedAt, stoppedAt)
	}
}

// The lock re-check of the removal must also refuse a stored delay of 0
// (never remove). Not reachable from Pass with one serve; shown by a
// direct call with a stopped lease whose stored delay is 0.
func TestLockRecheckRefusesStoredZero(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	zero := time.Duration(0)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-1000 * time.Hour), RemoveAfter: &zero}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	now := func() time.Time { return testNow }
	_, _, removed, err := removeProject(context.Background(), docker.New(h.srv.Socket), h.st, "myapp", h.cfg, now, io.Discard, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if removed || !h.srv.Has("volume", "myapp_data") {
		t.Fatal("removeProject removed a project whose stored remove_after is 0 (never)")
	}
}
