package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

func TestStacksPageOnlyManaged(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.Add("container", "c2", "other_db_1", "other")
	h.fake.Add("container", "c3", "dev-lease_ctl_1", "dev-lease")
	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/"))
	// The mockup's layout carries the details link twice: once in the
	// wide table row and once in the phone card.
	if n := strings.Count(b, `href="/stacks/`); n != 2 {
		t.Errorf("all-stacks page has %d stack links, want exactly myapp", n)
	}
	if !strings.Contains(b, `href="/stacks/myapp"`) {
		t.Error("all-stacks page misses the myapp link")
	}
	if strings.Contains(b, "other_db_1") {
		t.Error("an unmanaged project's container is shown")
	}
	if !strings.Contains(b, "myapp*, ghost") {
		t.Error("the matching patterns line is missing")
	}
}

func TestStacksPageOrderAndTexts(t *testing.T) {
	h := newDH(t)
	// alive: live lease held by agent-201, one running container, 5.8 GB.
	h.fake.Add("container", "c-alive", "alive_web_1", "myapp_aa")
	h.fake.Add("volume", "alive_data", "alive_data", "myapp_aa")
	h.fake.SetVolumeSize("alive_data", 5_800_000_000)
	h.extend(t, "myapp_aa", "agent-201", time.Hour)
	// stopped: stopped 3h ago, containers stopped, 1.2 GB.
	h.fake.Add("container", "c-stopped", "bstopped_db_1", "myapp_mm")
	h.fake.StopContainer("c-stopped")
	h.fake.Add("volume", "bstopped_data", "bstopped_data", "myapp_mm")
	h.fake.SetVolumeSize("bstopped_data", 1_200_000_000)
	h.seed(t, "myapp_mm", lease.Lease{Project: "myapp_mm", State: lease.StateStopped, StoppedAt: dashNow.Add(-3 * time.Hour)})
	// removing: removal running.
	h.fake.Add("container", "c-removing", "zremoving_app_1", "myapp_zz")
	h.fake.StopContainer("c-removing")
	h.seed(t, "myapp_zz", lease.Lease{Project: "myapp_zz", State: lease.StateRemoving, StoppedAt: dashNow.Add(-time.Hour)})

	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/"))

	if want := "3 stacks · 1 live · 1 stopped · 1 removing · 7.0 GB on disk"; !strings.Contains(b, want) {
		t.Errorf("summary misses %q", want)
	}
	iRemoving, iLive, iStopped := strings.Index(b, `href="/stacks/myapp_zz"`), strings.Index(b, `href="/stacks/myapp_aa"`), strings.Index(b, `href="/stacks/myapp_mm"`)
	if iRemoving < 0 || iLive < 0 || iStopped < 0 {
		t.Fatalf("links missing: %d %d %d", iRemoving, iLive, iStopped)
	}
	if !(iRemoving < iLive && iLive < iStopped) {
		t.Errorf("order = removing@%d live@%d stopped@%d, want worst first", iRemoving, iLive, iStopped)
	}
	for _, want := range []string{"removing now", "stops in 1h", "removed in 6d21h", "held by agent-201", "1 running", "· up ", "5.8 GB", "1 stopped · 1.2 GB"} {
		if !strings.Contains(b, want) {
			t.Errorf("all-stacks page misses %q", want)
		}
	}
}

func TestStackPageSections(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c-web", "myapp_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": "/srv/myapp",
	}, "sha256:imgbuilt", []string{"myapp_default"}, nil)
	h.fake.SetMemory("c-web", 300_000_000, 40_000_000)
	h.fake.AddContainerWithUses("c-db", "myapp_db_1", map[string]string{
		docker.ProjectLabel: "myapp",
		docker.ServiceLabel: "db",
	}, "", nil, nil)
	h.fake.StopContainer("c-db")
	h.fake.AddContainerWithUses("c-keep", "myapp_keep_1", map[string]string{
		docker.ProjectLabel: "myapp",
		docker.ServiceLabel: "keep",
		docker.KeepLabel:    "true",
	}, "", nil, nil)
	h.fake.AddLabels("image", "sha256:imgbuilt", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp",
		docker.ServiceLabel: "web",
	})
	h.fake.AddUntaggedImage("sha256:imgpulled", "myapp")
	h.fake.Add("volume", "myapp_pg", "myapp_pg", "myapp")
	h.fake.SetVolumeSize("myapp_pg", 40_000_000)
	h.fake.Add("volume", "myapp_es", "myapp_es", "myapp")
	h.fake.Add("network", "net1", "myapp_default", "myapp")
	h.extend(t, "myapp", "dashboard", 2*time.Hour)
	h.extend(t, "myapp", "agent-201", time.Hour)

	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/stacks/myapp"))

	for _, want := range []string{
		"running", "260.0 MB",
		"exited",
		">keep<",
		"myapp_pg", "40.0 MB",
		"myapp_es", "unknown",
		"Removed with the stack:", "myapp-web:latest",
		"Left in place:", "sha256:imgpulled",
		"myapp_default",
		"cd /srv/myapp &amp;&amp; docker compose -p myapp up -d",
		"dashboard", "agent-201",
		"2h left",
		"Stop…", "Remove…",
		`id="stop-dialog"`, `id="remove-dialog"`,
		"Release my claim",
		"Stops 1 running container:", "<span class=\"mono\">myapp_web_1</span>.",
		"Keeps running (dev-lease.keep): <span class=\"mono\">myapp_keep_1</span>.",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("stack page misses %q", want)
		}
	}
	if !strings.Contains(b, `<dialog class="dialog" id="stop-dialog" aria-labelledby="stop-title">`) {
		t.Error("the stop dialog is not rendered closed on the plain page")
	}
	t.Run("the confirm query renders the dialog open as the page", func(t *testing.T) {
		open := body(t, get(t, ts.URL+"/stacks/myapp?confirm=remove"))
		if !strings.Contains(open, `<dialog class="dialog" id="remove-dialog" open`) {
			t.Error("?confirm=remove does not open the remove dialog")
		}
	})
}

func TestNamesAreEscaped(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "web<b>1", map[string]string{
		docker.ProjectLabel:                      "myapp_a-1",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": "<script>alert(1)</script>",
	}, "", nil, nil)
	h.fake.SetLogs("c1", muxLogFrame("2026-10-20T12:00:00.000000000Z started <b>fast</b> and won\n"))
	ts := h.serve(t, Options{})

	b := body(t, get(t, ts.URL+"/stacks/myapp_a-1?logs=web"))
	for _, want := range []string{
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"started &lt;b&gt;fast&lt;/b&gt; and won",
		"docker compose -p myapp_a-1 up -d",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("escaped page misses %q", want)
		}
	}
	for _, bad := range []string{"<script>alert", "<b>fast</b>"} {
		if strings.Contains(b, bad) {
			t.Errorf("raw HTML leaked into the page: %q", bad)
		}
	}
	// The keep action round-trips through the escaped path.
	resp := post(t, ts, "/stacks/myapp_a-1/keep", "csrf="+csrfFrom(t, b)+"&ttl=2h")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/stacks/myapp_a-1?done=kept&ttl=2h" {
		t.Errorf("keep on escaped name = %d to %q, want 303 to /stacks/myapp_a-1?done=kept&ttl=2h", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLogTailEscapedAndLimited(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c-web", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp",
		docker.ServiceLabel: "web",
	}, "", nil, nil)
	h.fake.SetLogs("c-web", muxLogFrame("2026-10-20T12:00:00.000000000Z hello <world>\n"))
	ts := h.serve(t, Options{})

	t.Run("an out-of-range lines value is refused", func(t *testing.T) {
		if got := get(t, ts.URL+"/stacks/myapp?logs=web&lines=500").StatusCode; got != 400 {
			t.Errorf("lines=500 = %d, want 400", got)
		}
	})
	t.Run("an unknown service shows no logs and makes no daemon call", func(t *testing.T) {
		b := body(t, get(t, ts.URL+"/stacks/myapp?logs=nope"))
		if !strings.Contains(b, "Choose a service to see its logs.") {
			t.Error("unknown service page misses the hint")
		}
		for _, c := range h.fake.Calls() {
			if strings.Contains(c.Path, "/logs") {
				t.Errorf("unknown service caused a logs call: %s", c.Path)
			}
		}
	})
	t.Run("the default tail is 50 and the lines are escaped", func(t *testing.T) {
		b := body(t, get(t, ts.URL+"/stacks/myapp?logs=web"))
		if !strings.Contains(b, "hello &lt;world&gt;") {
			t.Error("log line not shown escaped")
		}
		found := false
		for _, c := range h.fake.Calls() {
			if strings.HasSuffix(c.Path, "/logs") && c.Query.Get("tail") == "50" {
				found = true
			}
		}
		if !found {
			t.Error("no logs call with tail=50")
		}
	})
	t.Run("an explicit lines value reaches the daemon", func(t *testing.T) {
		get(t, ts.URL+"/stacks/myapp?logs=web&lines=100")
		found := false
		for _, c := range h.fake.Calls() {
			if strings.HasSuffix(c.Path, "/logs") && c.Query.Get("tail") == "100" {
				found = true
			}
		}
		if !found {
			t.Error("no logs call with tail=100")
		}
	})
}

// a closed native dialog takes no space at all. The browser check
// comes in Task 8; the Go side asserts the rule exists.
func TestDialogClosedDisplayNone(t *testing.T) {
	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	if !strings.Contains(s, "dialog:not([open]) { display: none; }") {
		t.Errorf("app.css lacks the dialog:not([open]) { display: none; } rule")
	}
	if !strings.Contains(s, ".dialog[open]") {
		t.Errorf("app.css lacks the .dialog[open] flex layout")
	}
}

// the markup keeps the Remove button enabled so pages without
// JavaScript still work (the server check is the protection), while
// app.js disables it until the typed name matches the stack name, and
// reloads the whole page when a fragment fetch was redirected (a stale
// session) instead of swapping foreign markup in place.
func TestRemoveButtonEnabledInMarkupDisabledByJS(t *testing.T) {
	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, want := range []string{"button.disabled = true", "input.getAttribute('placeholder')", "resp.redirected", "location.reload()"} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
	ts := h.serve(t, Options{})
	page := body(t, get(t, ts.URL+"/stacks/myapp?confirm=remove"))
	if strings.Contains(page, `type="submit" disabled`) {
		t.Error("the markup disables the Remove button; no-JS pages must keep it enabled")
	}
	if !strings.Contains(page, `type="submit"`) {
		t.Error("the Remove button is missing from the remove dialog")
	}
}

// the stack page's fragment URL keeps the log query, so the 15 s
// refresh re-renders the same service and line count instead of
// dropping the logs selection.
func TestStackFragmentURLKeepsLogQuery(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.AddLabels("container", "c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	h.fake.SetLogs("c1", muxLogFrame("2026-10-20T12:00:00.000000000Z hello\n"))
	ts := h.serve(t, Options{})
	page := body(t, get(t, ts.URL+"/stacks/myapp?logs=web&lines=100"))
	for _, want := range []string{`data-refresh-url="/fragments/stacks/myapp?lines=100&amp;logs=web"`, `value="web" selected`, `value="100" selected`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
}
