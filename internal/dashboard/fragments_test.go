package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// The 15 s refresh swaps only read-only sections: the fragment carries
// the containers, volumes, logs and lease outputs, and nothing that
// acts or holds input. The Keep form, the Release button, the log
// controls and both dialogs live outside every refreshed region, so a
// refresh never discards what the user typed or chose.
func TestFragmentHasNoForms(t *testing.T) {
	h := newDH(t)
	h.fake.AddContainerWithUses("c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": "/srv/myapp",
	}, "sha256:i1", nil, nil)
	h.fake.SetContainerImageRef("c1", "ghcr.io/andrius/tool:latest")
	h.fake.SetLogs("c1", muxLogFrame("2026-10-20T12:00:00.000000000Z hello\n"))
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	frag := body(t, get(t, ts.URL+"/fragments/stacks/myapp?logs=web&lines=50"))

	for _, bad := range []string{"<form", "<select", "<input", "<dialog", "csrf"} {
		if strings.Contains(frag, bad) {
			t.Errorf("the refresh fragment contains %q; controls must live outside every refreshed region", bad)
		}
	}
	for _, want := range []string{
		`id="frag-containers"`,
		`id="frag-volumes"`,
		`id="frag-logs"`,
		`id="frag-lease"`,
		"ghcr.io/andrius/tool:latest", // containers table
		"hello",                       // the log output
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("the refresh fragment lacks %q", want)
		}
	}

	page := body(t, get(t, ts.URL+"/stacks/myapp?logs=web&lines=50"))
	if !strings.Contains(page, `id="stop-dialog"`) {
		t.Error("the stop dialog is gone from the page")
	}
	// Every section the fragment carries must exist on the page, or the
	// refresh has nothing to swap it into.
	for _, id := range []string{"frag-containers", "frag-volumes", "frag-logs", "frag-lease"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("the stack page lacks the refreshed section %q", id)
		}
	}
}
