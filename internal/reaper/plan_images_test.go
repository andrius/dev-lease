package reaper

import (
	"context"
	"slices"
	"testing"

	"github.com/andrius/dev-lease/internal/docker"
)

// PlanFor's image rules around kept containers: a kept service's own
// built image (a kept container runs from the <project>-<service> tag)
// stays and is listed in Kept; a custom-named built image a non-kept
// container runs from is removed and listed in Images; an untagged image
// a non-kept container runs from is removed and listed by id.
func TestPlanForKeptAndUntaggedImages(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddContainerWithUses("ck", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web", docker.KeepLabel: "true",
	}, "sha256:kimg", nil, nil)
	srv.AddContainerWithUses("c1", "myapp_old_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "old",
	}, "sha256:cust", nil, nil)
	srv.AddContainerWithUses("c2", "myapp_job_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "job",
	}, "sha256:untag", nil, nil)
	srv.AddLabels("image", "sha256:kimg", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddLabels("image", "sha256:cust", "my-custom:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "old",
	})
	srv.AddUntaggedImage("sha256:untag", "myapp")
	// An unlabelled built image (Compose v2) tagged myapp-db: while any
	// container exists, the containers' service labels decide, and none
	// of them is db, so it stays unlisted. The unlabelled image tagged
	// for another project never matches this project's services either.
	srv.AddLabels("image", "sha256:left", "myapp-db:latest", nil)
	srv.AddLabels("image", "sha256:uother", "other-web:latest", nil)

	cli := docker.New(srv.Socket)
	ctx := context.Background()
	conts, err := cli.Containers(ctx, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := cli.Images(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanFor("myapp", ServicesOf(conts, nil), conts, nil, nil, imgs)

	if !slices.Equal(plan.Kept, []string{"myapp-web:latest", "myapp_web_1"}) {
		t.Errorf("Kept = %v, want the kept container and its own built image", plan.Kept)
	}
	if slices.Contains(plan.Images, "myapp-web:latest") {
		t.Errorf("Images = %v, contains the kept container's image", plan.Images)
	}
	if !slices.Contains(plan.Images, "my-custom:latest") {
		t.Errorf("Images = %v, want the custom-named image the non-kept container runs from", plan.Images)
	}
	if !slices.Contains(plan.Images, "sha256:untag") {
		t.Errorf("Images = %v, want the untagged image listed by id", plan.Images)
	}
	for _, stay := range []string{"myapp-db:latest", "other-web:latest"} {
		if slices.Contains(plan.Images, stay) || slices.Contains(plan.Kept, stay) {
			t.Errorf("Images = %v, Kept = %v: %s must stay unlisted", plan.Images, plan.Kept, stay)
		}
	}
	if !slices.Equal(plan.Containers, []string{"myapp_job_1", "myapp_old_1"}) {
		t.Errorf("Containers = %v, want the non-kept containers only", plan.Containers)
	}
}

// When no container is left (a compose down before the removal), the
// services the sweep recorded in the lease decide which unlabelled built
// images are removable: myapp-db matches the recorded db; a tag for
// another project and a pulled tag match nothing and stay unlisted.
func TestPlanForUnlabelledImagesUsesRecordedServices(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddLabels("image", "sha256:left", "myapp-db:latest", nil)
	srv.AddLabels("image", "sha256:uother", "other-web:latest", nil)
	srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)

	cli := docker.New(srv.Socket)
	imgs, err := cli.Images(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanFor("myapp", ServicesOf(nil, []string{"db"}), nil, nil, nil, imgs)
	if !slices.Equal(plan.Images, []string{"myapp-db:latest"}) {
		t.Errorf("Images = %v, want only the unlabelled myapp-db image the recorded services name", plan.Images)
	}
}
