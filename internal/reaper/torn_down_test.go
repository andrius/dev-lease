package reaper

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// A stack an agent or the operator tore down - compose down -v, or a test
// recipe's trap: its containers and its volumes are gone - leaves its built
// images and sometimes a network. Its stopped lease must not wait for the
// removal deadline (12h on the owner's machine, 168h in this harness) to
// delete them: the next pass removes what is left and drops the lease
// (owner, 2026-10-28: "если агент или оператор снесли стэк, не стоит
// держаться его"). Nine such leases sat in `dev-lease list` for up to 11
// hours, each holding 1 to 8 built images.
//
// What still waits for the deadline: a volume left on the daemon (the data
// was kept on purpose: compose down without -v), a remove-after of 0 (never
// remove), and every active lease - a compose up --build has no containers
// while it builds, so an active lease with no containers is not torn down.

// tornDownImage is the stack's built image: the project and service labels
// and the <project>-<service> tag, so the removal deletes it.
func tornDownImage(h *harness) {
	h.srv.AddLabels("image", "sha256:i2", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
}

func hasImage(t *testing.T, h *harness, id string) bool {
	t.Helper()
	imgs, err := docker.New(h.srv.Socket).Images(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(imgs, func(o docker.Object) bool { return o.ID == id })
}

func countOf(t *testing.T, list func(context.Context, string) ([]docker.Object, error)) int {
	t.Helper()
	objs, err := list(context.Background(), "myapp")
	if err != nil {
		t.Fatal(err)
	}
	return len(objs)
}

func TestPassRemovesTornDownStackNow(t *testing.T) {
	t.Run("only the built image left: removed on the next pass, lease gone", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		tornDownImage(h)
		stopHarnessProject(t, h)
		removeEverything(t, h, "sha256:i2")
		if !hasImage(t, h, "sha256:i2") {
			t.Fatal("setup: the built image must be on the daemon before the pass")
		}
		rep := h.pass(t)
		if _, ok := h.leases()["myapp"]; ok {
			t.Fatalf("the torn-down stack's lease is still held; log:\n%s", h.log)
		}
		if hasImage(t, h, "sha256:i2") {
			t.Fatalf("the torn-down stack's built image was not removed; log:\n%s", h.log)
		}
		if !slices.Contains(rep.Removed, "myapp") {
			t.Fatalf("the pass did not report the removal: Removed=%v", rep.Removed)
		}
	})

	t.Run("an image and a network left: both removed, lease gone", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		tornDownImage(h)
		stopHarnessProject(t, h)
		removeEverything(t, h, "sha256:i2", "n1")
		h.pass(t)
		cli := docker.New(h.srv.Socket)
		if _, ok := h.leases()["myapp"]; ok {
			t.Fatalf("the lease is still held; log:\n%s", h.log)
		}
		if hasImage(t, h, "sha256:i2") || countOf(t, cli.Networks) != 0 {
			t.Fatalf("leftovers stayed: image=%v networks=%d; log:\n%s",
				hasImage(t, h, "sha256:i2"), countOf(t, cli.Networks), h.log)
		}
	})

	t.Run("a volume left: the data was kept on purpose, the deadline applies", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		tornDownImage(h)
		stopHarnessProject(t, h)
		removeEverything(t, h, "sha256:i2", "myapp_data")
		h.pass(t)
		cli := docker.New(h.srv.Socket)
		if l, ok := h.leases()["myapp"]; !ok || l.State != "stopped" {
			t.Fatalf("lease = %+v (present %v), want it still stopped", l, ok)
		}
		if !hasImage(t, h, "sha256:i2") || countOf(t, cli.Volumes) != 1 {
			t.Fatalf("something was removed before the deadline: image=%v volumes=%d",
				hasImage(t, h, "sha256:i2"), countOf(t, cli.Volumes))
		}
	})

	t.Run("a remove-after of 0 (never) is honoured", func(t *testing.T) {
		h := newHarness(t)
		tornDownImage(h)
		never := map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web", docker.RemoveAfterLabel: "0",
		}
		h.srv.AddContainerWithUses("c1", "web", never, "sha256:i2", nil, nil)
		h.srv.Add("volume", "v1", "myapp_data", "myapp")
		stopHarnessProject(t, h)
		cli := docker.New(h.srv.Socket)
		if err := cli.RemoveContainer(context.Background(), "c1"); err != nil {
			t.Fatal(err)
		}
		if err := cli.RemoveVolume(context.Background(), "myapp_data"); err != nil {
			t.Fatal(err)
		}
		h.pass(t)
		if _, ok := h.leases()["myapp"]; !ok {
			t.Fatalf("a never-remove lease was dropped; log:\n%s", h.log)
		}
		if !hasImage(t, h, "sha256:i2") {
			t.Fatal("a never-remove stack's image was removed")
		}
	})

	t.Run("an active lease with no containers is not torn down (a build in progress)", func(t *testing.T) {
		h := newHarness(t)
		h.extend("myapp", time.Hour, testNow)
		tornDownImage(h)
		h.pass(t)
		l, ok := h.leases()["myapp"]
		if !ok || l.State != "" {
			t.Fatalf("lease = %+v (present %v), want it active and untouched", l, ok)
		}
		if !hasImage(t, h, "sha256:i2") {
			t.Fatal("an active stack's freshly built image was removed")
		}
	})
}
