package view

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// The detail page names an unlabelled image by the tags the removal
// deletes or keeps, not by whichever tag the daemon lists first: an image
// shared with another project shows the stack's own tag, a kept service's
// tag shows as kept. After a compose down the services the lease recorded
// still find the built image, and another project's labelled image shows
// only the stack's own tag it carries.
func TestDetailsNameSharedImagesByTheStacksTags(t *testing.T) {
	t.Run("when the image is shared with another project and a kept service, expect each tag on its side", func(t *testing.T) {
		h := newVH(t)
		h.srv.AddContainerWithUses("c-web", "myapp-web-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		}, "sha256:shared", nil, nil)
		h.srv.AddContainerWithUses("c-kept", "myapp-kept-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "kept", "dev-lease.keep": "true",
		}, "sha256:shared", nil, nil)
		h.srv.AddLabels("image", "sha256:shared", "unmanaged-web:latest", nil)
		h.srv.SetImageTags("sha256:shared", "unmanaged-web:latest", "myapp-kept:latest", "myapp-web:latest")

		d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 0, viewNow)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(d.RemovedImages, []string{"myapp-web:latest"}) {
			t.Errorf("RemovedImages = %v, want [myapp-web:latest]", d.RemovedImages)
		}
		if !slices.Equal(d.KeptImages, []string{"myapp-kept:latest"}) {
			t.Errorf("KeptImages = %v, want [myapp-kept:latest]", d.KeptImages)
		}
		if !slices.Equal(d.Plan.Images, d.RemovedImages) {
			t.Errorf("Plan.Images = %v, want the page's RemovedImages %v", d.Plan.Images, d.RemovedImages)
		}
	})

	t.Run("when the containers are gone, expect the recorded services to find the built image", func(t *testing.T) {
		h := newVH(t)
		h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
		h.srv.AddLabels("image", "sha256:built", "myapp-web:latest", nil)
		h.put(t, lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: viewNow.Add(-time.Hour), Services: []string{"web"}})

		d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 0, viewNow)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(d.RemovedImages, []string{"myapp-web:latest"}) {
			t.Errorf("RemovedImages = %v, want [myapp-web:latest] from the recorded services", d.RemovedImages)
		}
	})

	t.Run("when another project's labelled image carries the stack's tag shape, expect that tag removed with it", func(t *testing.T) {
		h := newVH(t)
		h.srv.AddContainerWithUses("c-web", "myapp-web-1", map[string]string{
			docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
		}, "sha256:built", nil, nil)
		h.srv.AddLabels("image", "sha256:built", "myapp-web:latest", nil)
		h.srv.AddLabels("image", "sha256:theirs", "myapp-web:old", map[string]string{
			docker.ProjectLabel: "other", docker.ServiceLabel: "web",
		})

		d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 0, viewNow)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(d.RemovedImages, []string{"myapp-web:latest", "myapp-web:old"}) {
			t.Errorf("RemovedImages = %v, want the stack's own tag and the one on the other project's image", d.RemovedImages)
		}
		if len(d.KeptImages) != 0 {
			t.Errorf("KeptImages = %v, want none", d.KeptImages)
		}
	})
}
