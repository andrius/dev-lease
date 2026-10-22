package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// the all-stacks page has no heading of its own; the top bar's
// brand is the only dev-lease heading, with the page summary next to it.
func TestHeaderOnce(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/"))
	if strings.Contains(b, "<h1>dev-lease</h1>") {
		t.Error("the all-stacks page still has its own dev-lease heading")
	}
	if got := strings.Count(b, `class="brand" href="/">dev-lease</a>`); got != 1 {
		t.Errorf("the top bar brand appears %d times, want 1", got)
	}
	for _, want := range []string{
		`<header class="topbar">`,
		`class="page-summary"`,
		`id="theme-toggle"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the all-stacks page lacks %q", want)
		}
	}
}

// the dialogs are centered with a capped width and a dimmed
// backdrop, and they do not pin themselves to the top.
func TestDialogCenteredWithBackdrop(t *testing.T) {
	css := string(mustAsset(t, "static/app.css"))
	for _, want := range []string{
		`max-width: 640px;`,
		`inset: 0`,
		`margin: auto`,
		`.dialog::backdrop`,
		`hsl(222 47% 11% / 0.6)`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

// on phones the volumes-and-images grid stacks into one column.
func TestVolGridSingleColumnOnPhones(t *testing.T) {
	css := string(mustAsset(t, "static/app.css"))
	i := strings.Index(css, "@media (max-width: 640px)")
	if i < 0 {
		t.Fatal("app.css has no @media (max-width: 640px) block")
	}
	phone := css[i:]
	if !strings.Contains(phone, `.vol-grid { grid-template-columns: 1fr; }`) {
		t.Errorf("the phone media block lacks the one-column vol-grid rule:\n%s", phone)
	}
}

// the containers table and the phone cards show the computed
// uptime, not the bare daemon status string.
func TestContainersShowUptime(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "", nil, nil)
	h.extend(t, "myapp", "alice", time.Hour)
	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/stacks/myapp"))
	// dashNow - createdAt = 22h.
	for _, want := range []string{
		`<td class="nowrap">up 22h</td>`,
		`<div>running, up 22h</div>`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the stack page lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "Up 22 hours") {
		t.Error("the bare daemon status string leaked into the page")
	}
}

// the actions card says "Keep for at least", the button "Keep".
func TestActionsCardKeepForAtLeast(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/stacks/myapp"))
	for _, want := range []string{
		`<label>Keep for at least`,
		`>Keep</button>`,
		`value="2h" selected`, // the mockup's first option is the default
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the actions card lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "Extend by") {
		t.Error("the actions card still says Extend by")
	}
}

// the path line shows the config-files separator only when there
// are config files to list.
func TestPathLineNoTrailingSeparator(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": "/srv/myapp",
	}, "", nil, nil)
	h.extend(t, "myapp", "alice", time.Hour)
	ts := h.serve(t, Options{})

	b := body(t, get(t, ts.URL+"/stacks/myapp"))
	if strings.Contains(b, `<span class="mono">/srv/myapp</span> · `) {
		t.Errorf("the path line has a trailing separator with no config files:\n%s", b)
	}
	if !strings.Contains(b, `<p class="stack-sub"><span class="mono">/srv/myapp</span></p>`) {
		t.Errorf("the path line lost the path:\n%s", b)
	}

	// With config files the separator and the list stay.
	h.fake.SetContainerLabels("c1", map[string]string{
		"com.docker.compose.project.config_files": "/srv/myapp/compose.yml",
	})
	b = body(t, get(t, ts.URL+"/stacks/myapp"))
	if !strings.Contains(b, `<span class="mono">/srv/myapp</span> · <span class="mono">/srv/myapp/compose.yml</span>`) {
		t.Errorf("the path line lost the separator or the config files:\n%s", b)
	}
}

// the log Lines select offers 20/50/100/200, shows the URL's value
// as the selected option, and adds a value outside the four as its own
// selected option instead of snapping it onto a neighbour.
func TestLogLinesSelectReflectsURL(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.AddLabels("container", "c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	h.fake.SetLogs("c1", muxLogFrame("2026-10-20T12:00:00.000000000Z hello\n"))
	ts := h.serve(t, Options{})

	page := body(t, get(t, ts.URL+"/stacks/myapp?logs=web&lines=100"))
	for _, want := range []string{
		`value="20"`, `value="50"`, `value="100" selected`, `value="200"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the lines select lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, `value="150"`) {
		t.Error("the lines select offers a value outside 20/50/100/200")
	}

	// No lines in the URL: the default option is selected.
	page = body(t, get(t, ts.URL+"/stacks/myapp"))
	if !strings.Contains(page, `value="50" selected`) {
		t.Error("the lines select does not select the default 50")
	}

	// A value outside the four becomes its own selected option, and the
	// refresh URL carries the exact value.
	page = body(t, get(t, ts.URL+"/stacks/myapp?logs=web&lines=35"))
	if !strings.Contains(page, `value="35" selected`) {
		t.Errorf("lines=35 is not the selected option:\n%s", page)
	}
	if !strings.Contains(page, "lines=35") {
		t.Error("the refresh URL does not carry lines=35")
	}
}

// the page links a favicon and /favicon.ico serves it.
func TestFavicon(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/"))
	if !strings.Contains(b, `<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`) {
		t.Error("the page lacks the favicon link")
	}
	for _, path := range []string{"/favicon.ico", "/static/favicon.svg"} {
		resp := get(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
			t.Errorf("%s Content-Type = %q, want image/svg+xml", path, ct)
		}
		if got := body(t, resp); !strings.Contains(got, "<svg") {
			t.Errorf("%s does not serve an svg", path)
		}
	}
}

// mustAsset reads one embedded asset, failing the test when missing.
func mustAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := assets.ReadFile(name)
	if err != nil {
		t.Fatalf("asset %s: %v", name, err)
	}
	return b
}

// The log block's clock is UTC: logTime renders the UTC wall time of
// the instant (12:00 at +8 is 04:00 UTC), the page shows the UTC time
// of a UTC line, and the block says so.
func TestLogTimesRenderedInUTC(t *testing.T) {
	// A non-UTC local zone, so a local-time clock cannot pass by luck on a
	// UTC host.
	old := time.Local
	time.Local = time.FixedZone("test+3", 3*3600)
	t.Cleanup(func() { time.Local = old })
	s := &server{}
	f := s.funcs()["logTime"].(func(time.Time) string)
	got := f(time.Date(2026, 10, 20, 12, 0, 0, 0, time.FixedZone("somewhere+8", 8*3600)))
	if got != "04:00:00" {
		t.Errorf("logTime = %q, want the UTC wall time %q", got, "04:00:00")
	}
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.AddLabels("container", "c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	h.fake.SetLogs("c1", muxLogFrame("2026-10-20T12:00:00.000000000Z hello\n"))
	ts := h.serve(t, Options{})
	page := body(t, get(t, ts.URL+"/stacks/myapp?logs=web"))
	if !strings.Contains(page, "times in UTC") {
		t.Error("the log block does not say the times are UTC")
	}
	if !strings.Contains(page, `<span class="log-time">12:00:00</span>`) {
		t.Error("the log line is not rendered in UTC")
	}
}
