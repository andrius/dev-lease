package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// the stop dialog and the lease card carry the effective removal
// delay (the containers' label), not serve's setting.
func TestStopDialogUsesEffectiveRemoval(t *testing.T) {
	h := newDH(t)
	h.fake.AddLabels("container", "c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "1h",
	})
	ts := h.serve(t, Options{})
	stop := stopDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=stop")))
	if !strings.Contains(stop, "The stack is removed 1h after it stops unless someone starts it again or extends its lease.") {
		t.Errorf("the stop dialog does not say 1h after it stops:\n%s", stop)
	}
	card := leaseCard(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	if !strings.Contains(card, "Removed 1h after it stops.") {
		t.Errorf("the lease card does not say Removed 1h after it stops:\n%s", card)
	}
}

// a stack labelled remove-after 0 shows the never-remove
// warning in the remove dialog, the form carries a hidden override=1,
// the removal succeeds behind the typed name, and the dialogs never
// show a "<1m" duration.
func TestNeverRemoveDialogSendsOverride(t *testing.T) {
	h := newDH(t)
	h.fake.AddLabels("container", "c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	ts := h.serve(t, Options{})
	page := body(t, get(t, ts.URL+"/stacks/myapp?confirm=remove"))
	rm := removeDialog(t, page)
	if !strings.Contains(rm, "This stack is marked never remove; removing it anyway needs an override.") {
		t.Errorf("the remove dialog does not warn about never remove:\n%s", rm)
	}
	if !strings.Contains(rm, `name="override" value="1"`) {
		t.Errorf("the remove form carries no hidden override=1:\n%s", rm)
	}
	if strings.Contains(rm, "&lt;1m") {
		t.Errorf("the remove dialog shows a \"<1m\" duration for a never-remove stack:\n%s", rm)
	}
	csrf := csrfFrom(t, page)
	stop := stopDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=stop")))
	if !strings.Contains(stop, "The stack is never removed.") {
		t.Errorf("the stop dialog lacks the exact never-removed sentence:\n%s", stop)
	}
	if strings.Contains(stop, "&lt;1m") {
		t.Errorf("the stop dialog counts down in \"<1m\":\n%s", stop)
	}
	resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=myapp&override=1")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove with the override = %d, want 303", resp.StatusCode)
	}
	if h.fake.Has("container", "c1") {
		t.Error("objects left after the removal")
	}
}

// the remove dialog of a live stack says it stops the stack first
// and lists the pulled images that stay.
func TestRemoveDialogLiveStopsFirstAndListsKeptImages(t *testing.T) {
	h := newDH(t)
	srv := h.fake
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	srv.SetContainerImageRef("c1", "ghcr.io/andrius/tool:latest")
	ts := h.serve(t, Options{})
	rm := removeDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=remove")))
	if !strings.Contains(rm, "This stops the stack first.") {
		t.Errorf("the remove dialog of a live stack does not say it stops first:\n%s", rm)
	}
	if !strings.Contains(rm, "ghcr.io/andrius/tool:latest") {
		t.Errorf("the remove dialog does not list the pulled image that stays:\n%s", rm)
	}
}

// A stack that is not never remove has no override field in its remove
// dialog: the form must not silently send one.
func TestRemoveDialogNoOverrideUnlessNeverRemove(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	h.extend(t, "myapp", "alice", time.Hour)
	ts := h.serve(t, Options{})
	rm := removeDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=remove")))
	if strings.Contains(rm, `name="override"`) {
		t.Errorf("the remove dialog of a normal stack carries an override field:\n%s", rm)
	}
	if strings.Contains(rm, "marked never remove") {
		t.Errorf("the remove dialog of a normal stack shows the never-remove warning:\n%s", rm)
	}
}

// serve at remove-after 0 says never removed everywhere, not "<1m".
func TestServeZeroSaysNeverRemoved(t *testing.T) {
	h := newDH(t)
	h.runner.Cfg.RemoveAfter = 0
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	stop := stopDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=stop")))
	if !strings.Contains(stop, "The stack is never removed.") || strings.Contains(stop, "&lt;1m") {
		t.Errorf("the stop dialog = %q, want the exact never-removed sentence and no \"<1m\"", stop)
	}
	card := leaseCard(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	if !strings.Contains(card, "It is never removed.") || strings.Contains(card, "&lt;1m") {
		t.Errorf("the lease card = %q, want the exact never-removed sentence and no \"<1m\"", card)
	}
}

// A stopped stack's stop dialog still says "removed in" with the
// remaining time.
func TestStopDialogStoppedStackSaysRemovedIn(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.StopContainer("c1")
	h.seed(t, "myapp", lease.Lease{
		Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour),
	})
	ts := h.serve(t, Options{})
	stop := stopDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=stop")))
	if !strings.Contains(stop, "The stack is removed in ") {
		t.Errorf("the stopped stack's stop dialog does not say removed in:\n%s", stop)
	}
}

// TestDialogsFollowPlan pins the dialogs to the engine's plan: the stop
// dialog lists exactly the plan's stops plus the kept containers that
// keep running, the remove dialog lists exactly the plan's deletions
// and what stays.
func TestDialogsFollowPlan(t *testing.T) {
	h := newDH(t)
	srv := h.fake
	srv.AddContainerWithUses("c-web", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:imgbuilt", []string{"myapp_default"}, []string{"app_data"})
	srv.AddLabels("image", "sha256:imgbuilt", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddContainerWithUses("c-db", "myapp_db_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db", docker.KeepLabel: "true",
	}, "sha256:pg16", []string{"myapp_default"}, []string{"db_data"})
	srv.SetContainerImageRef("c-db", "postgres:16-alpine")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "app_data", "myapp")
	srv.Add("volume", "v2", "db_data", "myapp")
	srv.SetVolumeSize("app_data", 40_000_000)
	srv.SetVolumeSize("db_data", 30_000_000)
	h.extend(t, "myapp", "alice", time.Hour)
	ts := h.serve(t, Options{})

	stop := stopDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=stop")))
	stops := between(t, stop, "<p>Stops ", ".</p>")
	if stops != `1 running container: <span class="mono">myapp_web_1</span>` {
		t.Errorf("the stop dialog's stops line = %q", stops)
	}
	keeps := between(t, stop, "<p>Keeps running (dev-lease.keep): ", ".</p>")
	if keeps != `<span class="mono">myapp_db_1</span>` {
		t.Errorf("the stop dialog's kept line = %q", keeps)
	}

	rm := removeDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=remove")))
	for _, want := range []string{
		`>Containers</div><div class="mono">myapp_web_1</div>`,
		`>Networks</div><div class="muted">none</div>`,
		`app_data</span> 40.0 MB`,
		`>Images</div><div class="mono">myapp-web:latest</div>`,
		`<span class="mono">db_data, myapp_db_1, myapp_default, postgres:16-alpine</span>`,
	} {
		if !strings.Contains(rm, want) {
			t.Errorf("the remove dialog lacks %q:\n%s", want, rm)
		}
	}
	for _, unwanted := range []string{
		// the kept container is not deleted, so it is not in a deletion
		// cell, and the kept container's volume is not promised removed
		`<div class="mono">myapp_db_1</div>`,
		`db_data</span> 30.0 MB`,
		// the kept container's network cannot be removed
		`>Networks</div><div class="mono">myapp_default</div>`,
	} {
		if strings.Contains(rm, unwanted) {
			t.Errorf("the remove dialog wrongly contains %q:\n%s", unwanted, rm)
		}
	}
}

// TestStopDialogNothingToStop: a stopped stack's dialog says nothing is
// to stop instead of promising stops the engine will not do.
func TestStopDialogNothingToStop(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.StopContainer("c1")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
	ts := h.serve(t, Options{})
	stop := stopDialog(t, body(t, get(t, ts.URL+"/stacks/myapp?confirm=stop")))
	if !strings.Contains(stop, "There is nothing to stop.") {
		t.Errorf("the stop dialog of a stopped stack lacks the nothing-to-stop line:\n%s", stop)
	}
	if strings.Contains(stop, "Stops 0") || strings.Contains(stop, "Stops 1") {
		t.Errorf("the stop dialog of a stopped stack promises stops:\n%s", stop)
	}
}

// between returns the page text between two markers, failing the test
// when either marker is missing.
func between(t *testing.T, page, start, end string) string {
	t.Helper()
	i := strings.Index(page, start)
	if i < 0 {
		t.Fatalf("page lacks %q:\n%s", start, page)
	}
	i += len(start)
	j := strings.Index(page[i:], end)
	if j < 0 {
		t.Fatalf("page lacks %q after %q:\n%s", end, start, page)
	}
	return page[i : i+j]
}
