package reaper

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// Identical builds in two projects share one image id (BuildKit's cache),
// and the image keeps the compose labels of the project that built it
// first. On the owner's machine (2026-10-28) the image sha256:7a3c7a29f989
// carried the label ui-reports/asterisk and the tags ui-reports-asterisk,
// pbx-uirun-asterisk and ui-run-asterisk. The removal of pbx-uirun left its
// own tag behind because the image's project label named another project,
// and the 1.2 GB image outlived every lease that could remove it.
//
// The rule under test: a project's removal also takes its own
// <project>-<service> tags (a service of this project) off an image that is
// labelled for ANOTHER project. Only the tag goes: the image itself is
// deleted only with its last tag, and a tag that is not this project's
// shape stays. The plan the dialogs show lists the same tags.

// sharedBuild is another project's labelled image that also carries this
// project's tag for the same service. The other project, elsewhere, is not
// managed in this harness, so only myapp's removal can touch the image.
func sharedBuild(h *harness, id string, tags ...string) {
	h.srv.AddLabels("image", id, tags[0], map[string]string{
		docker.ProjectLabel: "elsewhere", docker.ServiceLabel: "web",
	})
	h.srv.SetImageTags(id, tags...)
}

func myappWebFrom(h *harness, image string) {
	h.srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, image, nil, nil)
}

func TestRemovalTakesItsOwnTagsOffAnImageLabelledForAnotherProject(t *testing.T) {
	t.Run("when the image also carries the other project's tag, expect only this stack's tag gone", func(t *testing.T) {
		h := newHarness(t)
		myappWebFrom(h, "sha256:shared")
		sharedBuild(h, "sha256:shared", "elsewhere-web:latest", "myapp-web:latest")
		h.removeToStoppedAndAged(t)
		plan := h.planNow(t, "myapp")
		h.pass(t)

		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"elsewhere-web:latest"}) {
			t.Fatalf("tags left = %v (present %v), want [elsewhere-web:latest]", got, h.srv.Has("image", "sha256:shared"))
		}
		if removed := removedImageNames(h.logLines()); !slices.Equal(removed, []string{"myapp-web:latest"}) {
			t.Fatalf("remove-image ok lines = %v, want [myapp-web:latest]", removed)
		}
		if !slices.Contains(plan.Images, "myapp-web:latest") {
			t.Fatalf("plan images = %v, want it to list myapp-web:latest", plan.Images)
		}
	})

	t.Run("when this stack's tag is the image's last tag, expect the image gone", func(t *testing.T) {
		h := newHarness(t)
		myappWebFrom(h, "sha256:shared")
		sharedBuild(h, "sha256:shared", "myapp-web:latest")
		h.removeToStoppedAndAged(t)
		h.pass(t)

		if h.srv.Has("image", "sha256:shared") {
			t.Fatalf("the image survived with tags %v", h.srv.ImageTags("sha256:shared"))
		}
	})

	t.Run("when a tag has this project's prefix but not one of its services, expect it kept", func(t *testing.T) {
		h := newHarness(t)
		myappWebFrom(h, "sha256:web")
		h.srv.AddLabels("image", "sha256:web", "myapp-web:latest", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		})
		sharedBuild(h, "sha256:shared", "elsewhere-web:latest", "myapp-cache:latest")
		h.removeToStoppedAndAged(t)
		h.pass(t)

		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"elsewhere-web:latest", "myapp-cache:latest"}) {
			t.Fatalf("tags = %v, want both kept: cache is not a service of myapp", got)
		}
	})

	t.Run("when the stack was torn down to such a tag, expect the next pass to take it and drop the lease", func(t *testing.T) {
		h := newHarness(t)
		myappWebFrom(h, "sha256:shared")
		sharedBuild(h, "sha256:shared", "elsewhere-web:latest", "myapp-web:latest")
		h.extend("myapp", time.Hour, testNow)
		h.pass(t) // records the services while the container is there
		h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		h.pass(t) // the lease expired: stopped
		if l := h.leases()["myapp"]; l.State != "stopped" {
			t.Fatalf("setup: state = %q, want stopped", l.State)
		}
		if err := docker.New(h.srv.Socket).RemoveContainer(context.Background(), "c1"); err != nil {
			t.Fatal(err) // compose down -v
		}
		h.pass(t)

		if _, ok := h.leases()["myapp"]; ok {
			t.Fatalf("the lease is still held; log:\n%s", h.log)
		}
		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"elsewhere-web:latest"}) {
			t.Fatalf("tags left = %v, want [elsewhere-web:latest]: this stack's tag must not outlive its lease", got)
		}
	})
}
