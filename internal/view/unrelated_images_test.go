package view

import (
	"context"
	"slices"
	"testing"

	"github.com/andrius/dev-lease/internal/docker"
)

// The detail page lists the images that go with the stack and the ones its
// containers use that stay. Other images on the daemon, unlabelled ones
// included, are no part of the stack and never appear.
func TestDetailsIgnoresUnrelatedUnlabelledImages(t *testing.T) {
	h := newVH(t)
	h.srv.AddContainerWithUses("c-db", "myapp-db-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pg", nil, nil)
	h.srv.AddContainerWithUses("c-web", "myapp-web-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:built", nil, nil)
	h.srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)
	h.srv.AddLabels("image", "sha256:built", "myapp-web:latest", nil)
	h.srv.AddLabels("image", "sha256:other", "nginx:latest", nil)
	h.srv.AddLabels("image", "sha256:other2", "otherapp-web:latest", nil)

	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 0, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.RemovedImages, []string{"myapp-web:latest"}) {
		t.Errorf("RemovedImages = %v, want [myapp-web:latest]", d.RemovedImages)
	}
	if !slices.Equal(d.KeptImages, []string{"postgres:16-alpine"}) {
		t.Errorf("KeptImages = %v, want only the pulled image a container runs from", d.KeptImages)
	}
}
