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

	cli := docker.New(srv.Socket)
	ctx := context.Background()
	conts, err := cli.Containers(ctx, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := cli.Images(ctx, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanFor("myapp", conts, nil, nil, imgs)

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
	if !slices.Equal(plan.Containers, []string{"myapp_job_1", "myapp_old_1"}) {
		t.Errorf("Containers = %v, want the non-kept containers only", plan.Containers)
	}
}
