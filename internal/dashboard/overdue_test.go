package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// A stopped stack whose removal is overdue (it stopped longer ago than
// its remove-after) says so on the lease card and in the stop dialog
// with the mockup's texts, never a "<1m" countdown.
func TestOverdueRemovalTexts(t *testing.T) {
	h := newDH(t)
	h.fake.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.seed(t, "myapp", lease.Lease{
		Project:     "myapp",
		State:       lease.StateStopped,
		StoppedAt:   dashNow.Add(-8 * 24 * time.Hour),
		RemoveAfter: ptr(7 * 24 * time.Hour),
	})
	ts := h.serve(t, Options{})

	page := body(t, get(t, ts.URL+"/stacks/myapp"))
	leaseCard := between(t, page, `<section class="card" aria-label="Lease">`, `</section>`)
	if !strings.Contains(leaseCard, "<p class=\"lease-line\">Next: removed at the next sweep.</p>") {
		t.Errorf("the lease card does not say the removal is overdue:\n%s", leaseCard)
	}
	if strings.Contains(leaseCard, "removal overdue") {
		t.Error("the lease card leaks the internal removal overdue state")
	}

	stop := between(t, page, `<dialog class="dialog" id="stop-dialog"`, `</dialog>`)
	want := "The stack is removed at the next sweep unless someone starts it again or extends its lease."
	if !strings.Contains(stop, want) {
		t.Errorf("the stop dialog lacks the exact overdue sentence %q:\n%s", want, stop)
	}
	if strings.Contains(stop, "&lt;1m") {
		t.Error("the stop dialog still counts down in <1m for an overdue removal")
	}
}

// The stop dialog's Keeps running lists only the kept containers that
// still run: a kept container that already stopped stays in the remove
// dialog's left-in-place list and out of the keeps-running list.
func TestKeepsRunningOnlyRunning(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web", docker.KeepLabel: "true",
	}, "sha256:i1", nil, nil)
	h.fake.AddContainerWithUses("c2", "myapp_db_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db", docker.KeepLabel: "true",
	}, "sha256:i2", nil, nil)
	h.fake.StopContainer("c2")
	h.seed(t, "myapp", lease.Lease{
		Project:     "myapp",
		State:       lease.StateStopped,
		StoppedAt:   dashNow.Add(-2 * time.Hour),
		RemoveAfter: ptr(7 * 24 * time.Hour),
	})
	ts := h.serve(t, Options{})

	page := body(t, get(t, ts.URL+"/stacks/myapp"))
	stop := between(t, page, `<dialog class="dialog" id="stop-dialog"`, `</dialog>`)
	if !strings.Contains(stop, `Keeps running (dev-lease.keep): <span class="mono">myapp_web_1</span>.`) {
		t.Errorf("the stop dialog's keeps-running list is wrong:\n%s", stop)
	}
	if strings.Contains(stop, "myapp_db_1") {
		t.Error("the stop dialog lists the stopped kept container as keeps running")
	}

	remove := between(t, page, `<dialog class="dialog" id="remove-dialog"`, `</dialog>`)
	if !strings.Contains(remove, "myapp_db_1") {
		t.Error("the stopped kept container is not in the remove dialog's left-in-place list")
	}
}

// Holder times are UTC clocks: a live holder shows "until 11:00 UTC".
func TestHolderTimesUTC(t *testing.T) {
	// A non-UTC local zone, so a local-time clock cannot pass by luck on a
	// UTC host.
	old := time.Local
	time.Local = time.FixedZone("test+3", 3*3600)
	t.Cleanup(func() { time.Local = old })
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp2_web_1", map[string]string{
		docker.ProjectLabel: "myapp2", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	h.extend(t, "myapp2", "agent-201", time.Hour)
	ts := h.serve(t, Options{})

	page := body(t, get(t, ts.URL+"/stacks/myapp2"))
	leaseCard := between(t, page, `<section class="card" aria-label="Lease">`, `</section>`)
	if !strings.Contains(leaseCard, `<span class="muted">until 11:00 UTC</span>`) {
		t.Errorf("the holder time is not a UTC clock:\n%s", leaseCard)
	}
}

// The app.css section comment says Keep for at least + Keep, the
// mockup's wording, not the old Extend by.
func TestActionsCardCSSComment(t *testing.T) {
	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(css), "Extend by") {
		t.Error("app.css still names the actions Extend by")
	}
}
