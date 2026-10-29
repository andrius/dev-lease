package reaper

// The removal's image rule for unlabelled built images (Compose v2 builds
// images without labels): an unlabelled image whose repository tag is
// exactly <project>-<service> for one of the project's services is
// removed with the rest of the stack; without a services record (no sweep
// saw the containers before the compose down) it stays, because the
// removal cannot recognise it as the stack's own build. An unlabelled
// image alone never makes a project without a lease present (it carries
// no label to find the project by), and another project's removal never
// touches it. RemoveNow shares the rule.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// The headline: a sweep between the compose up and the compose down
// records the services, so the removal recognises and removes the
// unlabelled built image together with the volume.
func TestSweepRemovalRemovesUnlabelledBuiltImage(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:unlab", nil, nil)
	srv.AddLabels("image", "sha256:unlab", "myapp-web:latest", nil)
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t) // the sweep between up and down records the services
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"web"}) {
		t.Fatalf("setup: services = %v", l.Services)
	}

	h.srv.Drop("container", "c1") // compose down
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t) // the stop: no containers left, the recorded services stay
	h.ageStopped(t)
	rep := h.pass(t) // the removal
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("image", "sha256:unlab") {
		t.Fatal("the unlabelled built image survived the removal")
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the volume survived the removal")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("the lease was not cleared")
	}
}

// The stack was up and down again before any sweep saw the containers:
// nothing recorded the services, so the removal cannot recognise the
// unlabelled built image and leaves it. The rest of the stack goes.
func TestSweepRemovalWithoutRecordLeavesUnlabelledBuiltImage(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:unlab", nil, nil)
	srv.AddLabels("image", "sha256:unlab", "myapp-web:latest", nil)
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.srv.Drop("container", "c1") // the down happened before any sweep
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the volume survived the removal")
	}
	if !h.srv.Has("image", "sha256:unlab") {
		t.Fatal("the unlabelled built image was removed without a services record")
	}
	// Without the record the image is no part of the stack, so the
	// removal does not mention it; the volume's line shows the log was
	// written.
	if strings.Contains(h.log.String(), "remove-image") || !strings.Contains(h.log.String(), "remove-volume") {
		t.Fatalf("want a remove-volume line and no remove-image line:\n%s", h.log.String())
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("the lease was not cleared")
	}
}

// An unlabelled image alone is not a stack: it carries no label to find
// the project by, so nothing manages it and nothing touches it.
func TestUnlabelledImageAloneIsNotManaged(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("image", "sha256:unlab", "myapp-web:latest", nil)
	rep := h.pass(t)
	if len(rep.Granted) != 0 {
		t.Fatalf("Granted = %v, want none: an unlabelled image alone is not a stack", rep.Granted)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("a lone unlabelled image made the project managed")
	}
	if !h.srv.Has("image", "sha256:unlab") {
		t.Fatal("the lone unlabelled image was touched")
	}
}

// Another project's removal leaves myapp's unlabelled built image alone:
// other's services can never name the myapp-web tag.
func TestOtherProjectRemovalLeavesUnlabelledProjectImage(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddContainerWithUses("o1", "web", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	}, "", nil, nil)
	srv.Add("volume", "ov", "other_data", "other")
	srv.AddLabels("image", "sha256:unlab", "myapp-web:latest", nil)
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "other")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "other" {
		t.Fatalf("Removed = %v, want [other]", rep.Removed)
	}
	if h.srv.Has("volume", "ov") {
		t.Fatal("other's volume survived the removal")
	}
	if !h.srv.Has("image", "sha256:unlab") {
		t.Fatal("other's removal took myapp's unlabelled built image")
	}
}

// RemoveNow shares the rule: the unlabelled built image goes, the pulled
// image and another project's image stay, and another project's image
// that carries this stack's own tag loses that tag (and goes with it when
// it is the image's last one).
func TestRemoveNowRemovesUnlabelledBuiltImage(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:unlab", nil, nil)
	srv.AddContainerWithUses("c2", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pg", nil, nil)
	srv.AddLabels("image", "sha256:unlab", "myapp-web:latest", nil)
	srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)
	srv.AddLabels("image", "sha256:xlab", "other-web:latest", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	})
	srv.AddLabels("image", "sha256:xmine", "myapp-web:old", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	})
	srv.Add("volume", "v1", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)

	r := h.runner()
	rep, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if err != nil {
		t.Fatalf("RemoveNow: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if srv.Has("image", "sha256:unlab") {
		t.Error("RemoveNow left the unlabelled built image")
	}
	if !srv.Has("image", "sha256:pg") {
		t.Error("RemoveNow removed the pulled image")
	}
	if !srv.Has("image", "sha256:xlab") {
		t.Error("RemoveNow removed another project's image")
	}
	if srv.Has("image", "sha256:xmine") {
		t.Error("RemoveNow left the image that carried this stack's own tag")
	}
	if srv.Has("volume", "v1") {
		t.Error("RemoveNow left the volume")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Error("the lease was not cleared")
	}
}

// Containers present at the removal speak for themselves over the
// recorded services, exactly as a live label does over the recorded
// delay: RemoveNow of a stack whose containers changed since the sweep
// removes the image the live services name and keeps the ones only the
// stale record names.
func TestRemoveNowLiveServicesSpeakOverRecord(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	// The sweep saw db and web and recorded them.
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:unweb", nil, nil)
	srv.AddContainerWithUses("c2", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:undb", nil, nil)
	srv.AddLabels("image", "sha256:unweb", "myapp-web:latest", nil)
	srv.AddLabels("image", "sha256:undb", "myapp-db:latest", nil)
	srv.AddLabels("image", "sha256:unx", "myapp-jobs:latest", nil)
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"db", "web"}) {
		t.Fatalf("setup: services = %v", l.Services)
	}

	// The stack is re-created with a different service set before the
	// removal: the record still says db, web, the containers say jobs.
	for _, id := range []string{"c1", "c2"} {
		h.srv.Drop("container", id)
	}
	srv.AddContainerWithUses("c3", "jobs", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "jobs",
	}, "sha256:unx", nil, nil)

	r := h.runner()
	// The stop alone never refreshes the record: only a sweep does.
	if _, err := r.StopNow(context.Background(), "myapp", "mcp"); err != nil {
		t.Fatalf("StopNow: %v", err)
	}
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"db", "web"}) {
		t.Fatalf("services after the stop = %v, want the record untouched", l.Services)
	}

	rep, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if err != nil {
		t.Fatalf("RemoveNow: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if srv.Has("image", "sha256:unx") {
		t.Error("RemoveNow left the image the live containers run from")
	}
	if !srv.Has("image", "sha256:unweb") || !srv.Has("image", "sha256:undb") {
		t.Error("RemoveNow removed images only the stale record names")
	}
}
