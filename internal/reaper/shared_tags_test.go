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

// A kept container runs from an unlabelled image whose only tag is a
// non-kept service's (a kept worker with `image: myapp-web`): Docker
// would refuse to delete that last tag, so the image stays whole, the
// plan lists it as kept and the removal does not try.
func TestRemovalKeepsTheImageAKeptContainerRunsFromWhenNoTagWouldRemain(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:img", nil, nil)
	h.srv.AddContainerWithUses("c2", "myapp-worker-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "worker", "dev-lease.keep": "true",
	}, "sha256:img", nil, nil)
	h.srv.AddLabels("image", "sha256:img", "myapp-web:latest", nil)
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t)
	h.ageStopped(t)
	plan := h.planNow(t, "myapp")
	h.pass(t)

	if len(plan.Images) != 0 || !slices.Contains(plan.Kept, "myapp-web:latest") {
		t.Fatalf("plan images %v, kept %v; want no image removed and myapp-web:latest kept", plan.Images, plan.Kept)
	}
	if got := h.srv.ImageTags("sha256:img"); !slices.Equal(got, []string{"myapp-web:latest"}) {
		t.Fatalf("tags left = %v, want [myapp-web:latest]", got)
	}
	var results []string
	for _, l := range h.logLines() {
		if l["action"] == "remove-image" {
			results = append(results, l["name"].(string)+" "+l["result"].(string))
		}
	}
	if !slices.Equal(results, []string{"myapp-web:latest skipped: a kept container of the project runs from it"}) {
		t.Fatalf("remove-image lines = %q, want one skip for the kept container", results)
	}
}

// armMoveBeforeTagCall makes the fake move tag to the image toID just
// before the first request that names the tag is routed: after the
// removal's listing, before its delete (or its check), the way a build
// landing at that moment would move it.
func (h *harness) armMoveBeforeTagCall(tag, toID string) {
	moved := false
	h.srv.OnRequest(func(method, path string) {
		if !moved && strings.Contains(path, "/images/"+tag) {
			h.srv.MoveTag(tag, toID)
			moved = true
		}
	})
}

func TestRemovalChecksATagStillNamesTheListedImage(t *testing.T) {
	t.Run("when the stack's tag moved to a fresh build after the listing, expect the fresh build untouched", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		}, "sha256:shared", nil, nil)
		h.srv.AddLabels("image", "sha256:shared", "myapp-web:latest", nil)
		h.srv.SetImageTags("sha256:shared", "myapp-web:latest", "unmanaged-web:latest")
		h.srv.AddLabels("image", "sha256:fresh", "sha256:fresh", nil)
		h.srv.SetImageTags("sha256:fresh")
		h.extend("myapp", time.Hour, testNow)
		h.pass(t)
		h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		h.pass(t)
		h.ageStopped(t)
		h.armMoveBeforeTagCall("myapp-web:latest", "sha256:fresh")
		h.pass(t) // the removal

		if got := h.srv.ImageTags("sha256:fresh"); !slices.Equal(got, []string{"myapp-web:latest"}) {
			t.Fatalf("the fresh build's tags = %v (present %v), want it untouched with [myapp-web:latest]", got, h.srv.Has("image", "sha256:fresh"))
		}
		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"unmanaged-web:latest"}) {
			t.Fatalf("the listed image's tags = %v, want [unmanaged-web:latest]", got)
		}
		var results []string
		for _, l := range h.logLines() {
			if l["action"] == "remove-image" {
				results = append(results, l["name"].(string)+" "+l["result"].(string))
			}
		}
		if !slices.Equal(results, []string{"myapp-web:latest skipped: the tag names another image since the listing"}) {
			t.Fatalf("remove-image lines = %q, want one skip for the moved tag", results)
		}
	})

	t.Run("when the image has one tag, expect it deleted by id", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		}, "sha256:one", nil, nil)
		h.srv.AddLabels("image", "sha256:one", "myapp-web:latest", nil)
		h.extend("myapp", time.Hour, testNow)
		h.pass(t)
		h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		h.pass(t)
		h.ageStopped(t)
		h.pass(t)

		if h.srv.Has("image", "sha256:one") {
			t.Fatal("the image survived the removal")
		}
		var deletes []string
		for _, c := range h.srv.Calls() {
			if c.Method == "DELETE" && strings.Contains(c.Path, "/images/") {
				deletes = append(deletes, c.Path[strings.Index(c.Path, "/images/")+len("/images/"):])
			}
		}
		if !slices.Equal(deletes, []string{"sha256:one"}) {
			t.Fatalf("image deletes = %v, want exactly [sha256:one]: a one-tag image goes by id", deletes)
		}
	})
}

// A labelled image (Compose v5) with more than one tag cannot be deleted by
// id either: the removal deletes the stack's own tags, and a tag that is
// not the stack's keeps the image.
func TestRemovalOfALabelledImageWithSeveralTags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tags     []string
		wantLeft []string // nil: the image is gone
		wantPlan []string
	}{
		{"when one tag is not the stack's, expect it to keep the image", []string{"myapp-web:latest", "myorg/web:release"}, []string{"myorg/web:release"}, []string{"myapp-web:latest"}},
		{"when every tag is the stack's, expect the image gone", []string{"myapp-web:latest", "myapp-web:v2"}, nil, []string{"myapp-web:latest", "myapp-web:v2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
				docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
			}, "sha256:img", nil, nil)
			h.srv.AddLabels("image", "sha256:img", tc.tags[0], map[string]string{
				docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
			})
			h.srv.SetImageTags("sha256:img", tc.tags...)
			h.extend("myapp", time.Hour, testNow)
			h.pass(t)
			h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
			h.pass(t)
			h.ageStopped(t)
			plan := h.planNow(t, "myapp")
			h.pass(t)

			if tc.wantLeft == nil {
				if h.srv.Has("image", "sha256:img") {
					t.Fatalf("the image survived with tags %v", h.srv.ImageTags("sha256:img"))
				}
			} else if got := h.srv.ImageTags("sha256:img"); !slices.Equal(got, tc.wantLeft) {
				t.Fatalf("tags left = %v (present %v), want %v", got, h.srv.Has("image", "sha256:img"), tc.wantLeft)
			}
			if !slices.Equal(plan.Images, tc.wantPlan) {
				t.Fatalf("plan images = %v, want %v", plan.Images, tc.wantPlan)
			}
			if removed := removedImageNames(h.logLines()); !slices.Equal(removed, tc.wantPlan) {
				t.Fatalf("remove-image ok lines = %v, want the plan's %v", removed, tc.wantPlan)
			}
		})
	}
}
