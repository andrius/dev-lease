package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
)

// A stack with no containers left (only a volume) shows "none" in the
// remove dialog's Containers cell, like the other empty cells, instead
// of an empty mono div.
func TestRemoveDialogContainersCellSaysNoneWhenEmpty(t *testing.T) {
	h := newDH(t)
	h.fake.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
	ts := h.serve(t, Options{})

	page := body(t, get(t, ts.URL+"/stacks/myapp?confirm=remove"))
	if !strings.Contains(page, `<div class="muted small">Containers</div><div class="muted">none</div>`) {
		t.Error("the remove dialog's Containers cell does not say none")
	}
}
