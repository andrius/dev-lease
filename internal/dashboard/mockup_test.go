package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// the app.css tokens are the mockup's light and dark values,
// verbatim, and every control that acts is at least 44px tall.
func TestMockupTokensVerbatim(t *testing.T) {
	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	for _, want := range []string{
		"--bg: hsl(210 20% 98%)",
		"--card: hsl(220 30% 99%)",
		"--ink: hsl(222 47% 11%)",
		"--fill: hsl(239 84% 60%)",
		"--link: hsl(239 84% 58%)",
		"--on-fill: hsl(0 0% 100%)",
		"--ok-bg: hsl(138 76% 97%)",
		"--warn-bg: hsl(48 96% 95%)",
		"--bad-bg: hsl(0 86% 97%)",
		"--bg: hsl(222 47% 11%)", // the dark value
		"min-height: 44px",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	for _, bad := range []string{"#fff", "min-height: 36px"} {
		if strings.Contains(s, bad) {
			t.Errorf("app.css still has %q", bad)
		}
	}
}

// the all-stacks page is the mockup's table at widths above 640px
// (name with the path under it, the mockup's columns, a 44x44 dots
// link labelled "Details of <name>") plus one card per stack below.
func TestStacksPageMockupTable(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": "/srv/myapp",
	}, "", nil, nil)
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/"))

	for _, want := range []string{
		`<table class="stacks-table">`,
		`<th scope="col">Stack</th>`,
		`<th scope="col">State</th>`,
		`<th scope="col">Next</th>`,
		`<th scope="col">Containers</th>`,
		`<th scope="col">Disk</th>`,
		`<th scope="col">Held by</th>`,
		`<div class="stack-name mono">myapp</div>`,
		`<div class="stack-path">/srv/myapp</div>`,
		`aria-label="Details of myapp"`,
		`class="dots"`,
		`<div class="stack-cards">`,
		`<article class="stack-card"`,
		`<div class="next-at">`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("all-stacks page lacks %q", want)
		}
	}
}

// the stack page shows the state badge next to the title, the
// containers table has the mockup's Service, State, Up, Memory and
// Image columns, the lease card has the mockup's eyebrow and "until"
// holder times, and the actions card says "Extend by"/"Extend".
func TestStackPageMockupSections(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": "/srv/myapp",
	}, "sha256:i1", nil, nil)
	h.fake.SetContainerImageRef("c1", "ghcr.io/andrius/tool:latest")
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	b := body(t, get(t, ts.URL+"/stacks/myapp"))

	for _, want := range []string{
		`<span class="badge badge-ok">live</span>`,
		`<th scope="col">Service</th>`,
		`<th scope="col">State</th>`,
		`<th scope="col">Up</th>`,
		`<th scope="col">Memory</th>`,
		`<th scope="col">Image</th>`,
		`<div class="eyebrow">Lease</div>`,
		`until `,
		"Keep for at least",
		`>Keep</button>`,
		`aria-label="Start again"`,
		`>Path</div>`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("stack page lacks %q", want)
		}
	}
	if strings.Contains(b, "Extend by") {
		t.Error("the actions card still says Extend by instead of Keep for at least")
	}
	if !strings.Contains(b, "ghcr.io/andrius/tool:latest") {
		t.Error("the containers table lost the image reference column")
	}
}

// The favicon is drawn in Signal Indigo and no other colour: the tile
// and the mark share the one fill, the mark cut out of the tile.
func TestFaviconInSignalIndigo(t *testing.T) {
	svg, err := assets.ReadFile("static/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.Contains(s, `fill="hsl(239 84% 60%)"`) {
		t.Error("the favicon is not drawn in Signal Indigo")
	}
	if strings.Count(s, "hsl(") != 1 {
		t.Errorf("the favicon uses %d colour tokens, want exactly one", strings.Count(s, "hsl("))
	}
	for _, bad := range []string{"#", "rgb(", "white"} {
		if strings.Contains(s, bad) {
			t.Errorf("the favicon still contains %q; only Signal Indigo may appear", bad)
		}
	}
}
