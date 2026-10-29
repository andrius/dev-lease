package reaper

// What must stay on an image labelled for another project: a kept
// service's tag of this project stays, and the whole image stays when a
// kept container of the project runs from it and taking the stack's own
// tag would take the image's last tag (Docker refuses that delete).

import (
	"slices"
	"testing"

	"github.com/andrius/dev-lease/internal/docker"
)

func TestForeignLabelledImageKeepsWhatMustStay(t *testing.T) {
	t.Run("when the stack's tag belongs to a kept service, expect the tag and the image kept", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddContainerWithUses("c1", "myapp-kept-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "kept", docker.KeepLabel: "true",
		}, "sha256:shared", nil, nil)
		sharedBuild(h, "sha256:shared", "elsewhere-web:latest", "myapp-kept:latest")
		h.removeToStoppedAndAged(t)
		plan := h.planNow(t, "myapp")
		h.pass(t)

		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"elsewhere-web:latest", "myapp-kept:latest"}) {
			t.Fatalf("tags = %v, want both kept: a kept service's tag is not the removal's", got)
		}
		if !slices.Contains(plan.Kept, "myapp-kept:latest") {
			t.Fatalf("plan kept = %v, want it to list myapp-kept:latest", plan.Kept)
		}
		if slices.Contains(plan.Images, "myapp-kept:latest") {
			t.Fatalf("plan images = %v, want it not to list the kept service's tag", plan.Images)
		}
	})

	t.Run("when a kept container of the project runs from it, expect the image whole", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddContainerWithUses("c-kept", "myapp-kept-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "kept", docker.KeepLabel: "true",
		}, "sha256:shared", nil, nil)
		myappWebFrom(h, "sha256:shared")
		sharedBuild(h, "sha256:shared", "myapp-web:latest")
		h.removeToStoppedAndAged(t)
		plan := h.planNow(t, "myapp")
		h.pass(t)

		if !h.srv.Has("image", "sha256:shared") {
			t.Fatal("the image a kept container runs from is gone")
		}
		if got := h.srv.ImageTags("sha256:shared"); !slices.Equal(got, []string{"myapp-web:latest"}) {
			t.Fatalf("tags = %v, want the stack's tag to stay with the image", got)
		}
		if !slices.Contains(plan.Kept, "myapp-web:latest") {
			t.Fatalf("plan kept = %v, want it to list myapp-web:latest", plan.Kept)
		}
	})
}
