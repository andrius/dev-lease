package reaper

// An unlabelled built image (Compose v2 builds images without labels) can
// carry the tags of several builds at once: identical builds get one id,
// across services and across projects. Docker refuses to delete such an
// image by id without force, so the removal deletes the stack's own tags
// instead: Docker untags a tag the image shares and deletes the image
// with its last tag. A kept service's tag stays. The dialogs' plan lists
// exactly the tags the removal deletes.

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// removedImageNames is the names of the remove-image log lines whose
// result is ok, sorted.
func removedImageNames(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		if l["action"] == "remove-image" && l["result"] == "ok" {
			out = append(out, l["name"].(string))
		}
	}
	sort.Strings(out)
	return out
}

// planNow is PlanFor on listings taken from the fake right now, the way
// the detail page takes them.
func (h *harness) planNow(t *testing.T, project string) Plan {
	t.Helper()
	ctx := context.Background()
	cli := docker.New(h.srv.Socket)
	conts, err := cli.Containers(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	nets, err := cli.Networks(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	vols, err := cli.Volumes(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := cli.Images(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	return PlanFor(project, ServicesOf(conts, h.leases()[project].Services), conts, nets, vols, imgs)
}

func TestRemovalOfASharedUnlabelledImage(t *testing.T) {
	t.Run("when the image also carries another project's tag, expect only the stack's tag removed", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		}, "sha256:shared", nil, nil)
		h.srv.AddLabels("image", "sha256:shared", "myapp-web:latest", nil)
		h.srv.SetImageTags("sha256:shared", "myapp-web:latest", "unmanaged-web:latest")
		h.extend("myapp", time.Hour, testNow)
		h.pass(t)
		h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		h.pass(t) // the stop
		h.ageStopped(t)
		plan := h.planNow(t, "myapp")
		rep := h.pass(t) // the removal
		if !slices.Equal(rep.Removed, []string{"myapp"}) {
			t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
		}
		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"unmanaged-web:latest"}) {
			t.Fatalf("tags left = %v, want only the other project's [unmanaged-web:latest]", got)
		}
		removed := removedImageNames(h.logLines())
		if !slices.Equal(removed, []string{"myapp-web:latest"}) {
			t.Fatalf("remove-image ok lines = %v, want [myapp-web:latest]", removed)
		}
		if !slices.Equal(plan.Images, removed) {
			t.Fatalf("plan images %v, but the removal deleted %v", plan.Images, removed)
		}
	})

	t.Run("when a kept and a removed service share the image, expect the removed service's tag gone and the kept one's in place", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		}, "sha256:shared", nil, nil)
		h.srv.AddContainerWithUses("c2", "myapp-kept-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "kept", "dev-lease.keep": "true",
		}, "sha256:shared", nil, nil)
		h.srv.AddLabels("image", "sha256:shared", "myapp-kept:latest", nil)
		h.srv.SetImageTags("sha256:shared", "myapp-kept:latest", "myapp-web:latest")
		h.extend("myapp", time.Hour, testNow)
		h.pass(t)
		h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		h.pass(t) // the stop: the kept container keeps running
		h.ageStopped(t)
		plan := h.planNow(t, "myapp")
		h.pass(t) // the removal
		if !h.srv.Has("image", "sha256:shared") {
			t.Fatal("the image the kept container runs from is gone")
		}
		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"myapp-kept:latest"}) {
			t.Fatalf("tags left = %v, want only the kept service's [myapp-kept:latest]", got)
		}
		removed := removedImageNames(h.logLines())
		if !slices.Equal(removed, []string{"myapp-web:latest"}) {
			t.Fatalf("remove-image ok lines = %v, want [myapp-web:latest]", removed)
		}
		if !slices.Equal(plan.Images, removed) {
			t.Fatalf("plan images %v, but the removal deleted %v", plan.Images, removed)
		}
		if !slices.Contains(plan.Kept, "myapp-kept:latest") {
			t.Fatalf("plan keeps %v, want myapp-kept:latest among them", plan.Kept)
		}
	})
}

// A stack taken down with `docker compose down` after its stop, with no
// named volume: only its unlabelled built image is left on the daemon.
// The image keeps the stopped stack present, like a labelled built image
// does under Compose v5, so the removal deletes it at the removal time
// instead of the lease being dropped and the image left behind.
func TestStoppedStackStaysWhileItsUnlabelledBuiltImageRemains(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:unlab", []string{"myapp_default"}, nil)
	h.srv.Add("network", "n1", "myapp_default", "myapp")
	h.srv.AddLabels("image", "sha256:unlab", "myapp-web:latest", nil)
	h.extend("myapp", time.Hour, testNow)
	h.pass(t) // records the services
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t)                     // the stop
	h.srv.Drop("container", "c1") // compose down
	h.srv.Drop("network", "n1")

	h.pass(t)
	if l, ok := h.leases()["myapp"]; !ok || l.State != lease.StateStopped {
		t.Fatalf("lease after the compose down = %+v (present %v), want it still stopped", l, ok)
	}
	if !h.srv.Has("image", "sha256:unlab") {
		t.Fatal("setup: the built image is gone before the removal")
	}

	h.ageStopped(t)
	rep := h.pass(t)
	if !slices.Equal(rep.Removed, []string{"myapp"}) {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("image", "sha256:unlab") {
		t.Fatal("the unlabelled built image survived the removal")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("the lease was not cleared")
	}
}

// The removal logs only the images that are the stack's: it says nothing
// about an unrelated pulled image or a dangling image of no project, and
// it still reports the pulled image its own container ran from.
func TestRemovalLogsOnlyTheStacksImages(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:built", nil, nil)
	h.srv.AddContainerWithUses("c2", "myapp-db-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pg", nil, nil)
	h.srv.AddLabels("image", "sha256:built", "myapp-web:latest", nil)
	h.srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)
	h.srv.AddLabels("image", "sha256:node", "node:20", nil)
	h.srv.AddLabels("image", "sha256:dangling", "sha256:dangling", nil)
	h.srv.SetImageTags("sha256:dangling")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t)
	h.ageStopped(t)
	h.pass(t)

	var imageLines []string
	for _, l := range h.logLines() {
		if l["action"] == "remove-image" {
			imageLines = append(imageLines, l["name"].(string)+" "+l["result"].(string))
		}
	}
	joined := strings.Join(imageLines, "\n")
	for _, unrelated := range []string{"node:20", "sha256:dangling"} {
		if strings.Contains(joined, unrelated) {
			t.Errorf("the removal logged the unrelated image %s:\n%s", unrelated, joined)
		}
	}
	if !strings.Contains(joined, "myapp-web:latest ok") {
		t.Errorf("no ok line for the stack's built image:\n%s", joined)
	}
	if !strings.Contains(joined, "postgres:16-alpine skipped") {
		t.Errorf("no skipped line for the pulled image the stack ran from:\n%s", joined)
	}
	if !h.srv.Has("image", "sha256:node") || !h.srv.Has("image", "sha256:pg") {
		t.Fatal("a pulled image was removed")
	}
}

// A revive whose containers carry the same remove-after label the lease
// recorded still records the services the containers have now.
func TestReviveRecordsTheServicesWhenTheLabelIsUnchanged(t *testing.T) {
	h := newHarness(t)
	labels := func(service string) map[string]string {
		return map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: service, "dev-lease.remove-after": "48h",
		}
	}
	h.srv.AddContainerWithUses("c1", "myapp-web-1", labels("web"), "", nil, nil)
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t) // the stop
	if l := h.leases()["myapp"]; l.State != lease.StateStopped || !slices.Equal(l.Services, []string{"web"}) {
		t.Fatalf("setup: lease = %+v, want stopped with services [web]", l)
	}

	h.srv.AddContainerWithUses("c2", "myapp-worker-1", labels("worker"), "", nil, nil) // compose up with a new service
	h.pass(t)                                                                          // the revive
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"web", "worker"}) {
		t.Fatalf("services after the revive = %v, want [web worker]", l.Services)
	}
}
