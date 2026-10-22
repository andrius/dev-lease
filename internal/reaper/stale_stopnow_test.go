package reaper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// A StopNow whose container listing fails because docker compose down
// removed the containers: the stale stopping mark is reverted and the
// same pass re-stops the expired lease, falling back to the recorded
// label 0. RemoveNow without AllowNeverRemove is then refused and the
// volume stays.
func TestStaleStopNowMarkComposeDownKeepsNeverRemove(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", 5*time.Hour, testNow)
	h.pass(t) // records the label 0
	h.srv.Drop("container", "c1")
	h.srv.FailList("container", 500)
	if _, err := h.runner().StopNow(context.Background(), "myapp", "mcp"); err != nil {
		t.Fatalf("StopNow: %v", err)
	}
	h.srv.UnfailList("container")
	if l := h.leases()["myapp"]; l.State != lease.StateStopping {
		t.Fatalf("setup: %+v", l)
	}

	h.cfg.Now = func() time.Time { return testNow.Add(time.Minute) }
	h.pass(t)
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("after the revert pass: %+v", l)
	}
	if !l.StoppedAt.Equal(testNow.Add(time.Minute)) {
		t.Fatalf("StoppedAt = %v", l.StoppedAt)
	}

	if _, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"}); !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow err = %v, want ErrNeverRemove", err)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("RemoveNow deleted the volume")
	}
}
