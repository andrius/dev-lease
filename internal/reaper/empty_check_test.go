package reaper

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// stopProject drives the expiry pass and leaves the project stopped with
// all its objects still on the daemon.
func stopHarnessProject(t *testing.T, h *harness) {
	t.Helper()
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.State != "stopped" {
		t.Fatalf("state = %q, want stopped", l.State)
	}
}

// removeEverything removes every object of the stopped project except the
// ids listed in keep.
func removeEverything(t *testing.T, h *harness, keep ...string) {
	t.Helper()
	cli := docker.New(h.srv.Socket)
	ctx := context.Background()
	for _, id := range []string{"c1", "c2"} {
		if err := cli.RemoveContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct {
		call func(context.Context, string) error
		id   string
	}{
		{cli.RemoveNetwork, "n1"},
		{cli.RemoveVolume, "myapp_data"},
		{cli.RemoveImage, "sha256:i1"},
	} {
		kept := false
		for _, k := range keep {
			if k == r.id {
				kept = true
			}
		}
		if kept {
			continue
		}
		if err := r.call(ctx, r.id); err != nil {
			t.Fatal(err)
		}
	}
}

// Each kind of presence-counted object keeps the stopped lease alive: a
// network, a volume and an image that matches the tag rule. Nothing left
// drops it.
func TestPassEmptyCheckPerKind(t *testing.T) {
	t.Run("a stopped container keeps the stopped lease", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddContainerWithUses("c1", "web", map[string]string{docker.ProjectLabel: "myapp"}, "", nil, nil)
		h.srv.StopContainer("c1")
		h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
		stopHarnessProject(t, h)
		// Remove everything except the stopped container itself: a
		// stopped container still counts as presence, because the
		// developer may start it again and the next pass revives the
		// project.
		cli := docker.New(h.srv.Socket)
		if err := cli.RemoveVolume(context.Background(), "myapp_data"); err != nil {
			t.Fatal(err)
		}
		h.pass(t)
		if _, ok := h.leases()["myapp"]; !ok {
			t.Fatal("stopped lease with a stopped container left on the daemon was dropped")
		}
	})
	t.Run("a network keeps the stopped lease", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		stopHarnessProject(t, h)
		removeEverything(t, h, "n1")
		h.pass(t)
		if _, ok := h.leases()["myapp"]; !ok {
			t.Fatal("stopped lease with a network left on the daemon was dropped")
		}
	})
	t.Run("a volume keeps the stopped lease", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		stopHarnessProject(t, h)
		removeEverything(t, h, "myapp_data")
		h.pass(t)
		if _, ok := h.leases()["myapp"]; !ok {
			t.Fatal("stopped lease with a volume left on the daemon was dropped")
		}
	})
	t.Run("an image matching the tag rule keeps the stopped lease", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		// The image presence counts: a <project>-<service> tag with the
		// image's own service label.
		h.srv.AddLabels("image", "sha256:i2", "myapp-web:latest", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		})
		stopHarnessProject(t, h)
		removeEverything(t, h, "sha256:i2")
		h.pass(t)
		if _, ok := h.leases()["myapp"]; !ok {
			t.Fatal("stopped lease with a tag-matching image left on the daemon was dropped")
		}
	})
	t.Run("nothing left drops the stopped lease", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		stopHarnessProject(t, h)
		removeEverything(t, h)
		h.pass(t)
		if _, ok := h.leases()["myapp"]; ok {
			t.Fatal("stopped lease with nothing left on the daemon survived the pass")
		}
	})
	t.Run("a kept volume does not keep the stopped lease", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		stopHarnessProject(t, h)
		// A kept volume stays by design and does not count as presence:
		// with only it left, the stopped lease is dropped like with
		// nothing left. AddLabels replaces the label set, so the project
		// label must be re-added for the volume to stay listed.
		h.srv.AddLabels("volume", "myapp_data", "myapp_data", map[string]string{
			docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
		})
		removeEverything(t, h, "myapp_data")
		h.pass(t)
		if _, ok := h.leases()["myapp"]; ok {
			t.Fatal("stopped lease with only a kept volume left on the daemon survived the pass")
		}
	})
}

// Images the presence rule does not count (an untagged leftover, an image
// that only inherited the project label) must not keep a stopped lease
// alive for days: the removal never removes them either.
func TestPassEmptyCheckIgnoresUncountedImages(t *testing.T) {
	for name, add := range map[string]func(*harness){
		"untagged leftover": func(h *harness) {
			h.srv.AddUntaggedImage("sha256:orphan", "myapp")
		},
		"inherited label": func(h *harness) {
			h.srv.AddLabels("image", "sha256:i3", "registry.local/tool:1", map[string]string{
				docker.ProjectLabel: "myapp",
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			fullProject(h.srv)
			add(h)
			stopHarnessProject(t, h)
			removeEverything(t, h)
			h.pass(t)
			if _, ok := h.leases()["myapp"]; ok {
				t.Fatalf("stopped lease survived although only an uncounted image is left; log:\n%s", h.log.String())
			}
		})
	}
}

// A listing failure in the empty check keeps the lease: dropping it on a
// broken daemon would silently lose the stopped state.
func TestPassEmptyCheckListingFailureKeepsLease(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	stopHarnessProject(t, h)
	removeEverything(t, h)
	// Fail the per-project volume listing (the project-wide one, the
	// first /volumes call, must work so the pass can start).
	var mu sync.Mutex
	n := 0
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/volumes") {
			mu.Lock()
			n++
			k := n
			mu.Unlock()
			if k == 2 {
				h.srv.FailList("volume", 500)
			}
		}
	})
	h.pass(t)
	if _, ok := h.leases()["myapp"]; !ok {
		t.Fatal("stopped lease was dropped although a listing in the empty check failed")
	}
	if !strings.Contains(h.log.String(), "error: docker: GET /v1.47/volumes") {
		t.Fatalf("the failed listing was not logged as an error: %s", h.log)
	}
}
