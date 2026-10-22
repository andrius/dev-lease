package reaper

import (
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// A dev-lease.remove-after label below 1h is unreadable, like the flag's
// lower bound: it counts as 0 (never remove) and is logged, so a 1m that
// meant a month cannot remove a stack a minute after the stop.
func TestLabelRemoveAfterBelowOneHourMeansNever(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("image", "sha256:i1", "myapp-web:latest", "myapp")
	h.srv.AddContainerWithUses("c1", "web", map[string]string{docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "1m"}, "sha256:i1", nil, nil)
	h.srv.Add("volume", "v1", "myapp_data", "myapp")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("stored = %v, want 0 for an unreadable 1m label", l.RemoveAfter)
	}
	if !strings.Contains(h.log.String(), "ignored") || !strings.Contains(h.log.String(), "1m") {
		t.Fatalf("the unreadable label was not logged:\n%s", h.log)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(61 * time.Second) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 || !h.srv.Has("volume", "myapp_data") {
		t.Fatalf("a 1m label removed the stack 61s after the stop: %+v", rep)
	}
}
