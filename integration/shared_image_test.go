//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// Identical builds share one image id: Compose v2 builds images without
// labels, and BuildKit gives two builds of the same content the same id,
// so one image carries the tags of several stacks. Docker refuses to
// delete such an image by id without force. The removal deletes the
// stack's own tag instead: the image stays for the other stack's tag.
// The test makes the shared id itself (one plain docker build, two tags,
// no labels), so it runs the same under every Compose version.
func TestRemovalUntagsAnUnlabelledImageSharedWithAnotherStack(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Docker socket at %s: %v", socket, err)
	}
	randID := randomHex(t, 4)
	it := "devlease-it-" + randID
	other := "devlease-keep-" + randID // matches no managed pattern
	itTag, otherTag := it+"-web:latest", other+"-web:latest"

	dir := filepath.Join(t.TempDir(), "it")
	composeProject(t, dir) // the Dockerfile and the sleep binary
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(`services:
  web:
    image: `+it+`-web
    pull_policy: never
    command: ["/sleep"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", itTag, "-t", otherTag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		compose(t, dir, it, "down", "-v", "--remove-orphans")
		exec.Command("docker", "image", "rm", itTag, otherTag).Run()
	})
	inspect := func(tag string) (string, error) {
		out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}} {{json .Config}}", tag).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	before, err := inspect(itTag)
	if err != nil {
		t.Fatalf("inspecting %s: %v\n%s", itTag, err, before)
	}
	if otherBefore, _ := inspect(otherTag); otherBefore != before || strings.Contains(before, "com.docker.compose.project") {
		t.Fatalf("setup: want one unlabelled image with both tags, got %q and %q", before, otherBefore)
	}
	compose(t, dir, it, "up", "-d")

	cli := docker.New(socket)
	st := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	cfg := reaper.Config{
		Patterns:    []string{pattern},
		Own:         "dev-lease",
		DefaultTTL:  time.Hour,
		RemoveAfter: time.Hour,
		StopTimeout: 5 * time.Second,
		Log:         os.Stdout,
	}
	ctx := context.Background()
	if _, err := reaper.Pass(ctx, cli, st, cfg); err != nil { // the grant records the services
		t.Fatalf("first pass: %v", err)
	}
	expireLease(t, st, it)
	if _, err := reaper.Pass(ctx, cli, st, cfg); err != nil { // the stop
		t.Fatalf("stop pass: %v", err)
	}
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m[it]
		l.StoppedAt = time.Now().UTC().Add(-2 * cfg.RemoveAfter)
		m[it] = l
		return nil
	}); err != nil {
		t.Fatalf("backdating the stopped time: %v", err)
	}
	rep, err := reaper.Pass(ctx, cli, st, cfg) // the removal
	if err != nil {
		t.Fatalf("removal pass: %v", err)
	}
	if !contains(rep.Removed, it) {
		t.Fatalf("removal pass removed %v, want %s", rep.Removed, it)
	}

	if out, err := inspect(itTag); err == nil {
		t.Fatalf("the stack's tag %s is still there: %s", itTag, out)
	}
	if after, err := inspect(otherTag); err != nil || after != before {
		t.Fatalf("the other stack's tag %s: %q (%v), want the same image %q", otherTag, after, err, before)
	}
}

// A built image that also carries a name of your own (a release tag)
// cannot be deleted by id. The removal deletes the stack's own tag and
// the other name keeps the image, whether Compose labelled the image (v5)
// or not (v2).
func TestRemovalLeavesAnImageYourOwnTagStillNames(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Docker socket at %s: %v", socket, err)
	}
	randID := randomHex(t, 4)
	it := "devlease-it-" + randID
	itTag, releaseTag := it+"-web:latest", "devlease-release-"+randID+"/web:1"
	dir := filepath.Join(t.TempDir(), "it")
	composeProject(t, dir)
	compose(t, dir, it, "up", "-d", "--build")
	t.Cleanup(func() {
		compose(t, dir, it, "down", "-v", "--rmi", "local", "--remove-orphans")
		exec.Command("docker", "image", "rm", releaseTag).Run()
	})
	if out, err := exec.Command("docker", "tag", itTag, releaseTag).CombinedOutput(); err != nil {
		t.Fatalf("docker tag: %v\n%s", err, out)
	}
	idOf := func(tag string) (string, error) {
		out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", tag).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	before, err := idOf(itTag)
	if err != nil {
		t.Fatalf("inspecting %s: %v\n%s", itTag, err, before)
	}

	cli := docker.New(socket)
	st := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	cfg := reaper.Config{
		Patterns:    []string{pattern},
		Own:         "dev-lease",
		DefaultTTL:  time.Hour,
		RemoveAfter: time.Hour,
		StopTimeout: 5 * time.Second,
		Log:         os.Stdout,
	}
	ctx := context.Background()
	if _, err := reaper.Pass(ctx, cli, st, cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	expireLease(t, st, it)
	if _, err := reaper.Pass(ctx, cli, st, cfg); err != nil {
		t.Fatalf("stop pass: %v", err)
	}
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m[it]
		l.StoppedAt = time.Now().UTC().Add(-2 * cfg.RemoveAfter)
		m[it] = l
		return nil
	}); err != nil {
		t.Fatalf("backdating the stopped time: %v", err)
	}
	if _, err := reaper.Pass(ctx, cli, st, cfg); err != nil {
		t.Fatalf("removal pass: %v", err)
	}

	if out, err := idOf(itTag); err == nil {
		t.Fatalf("the stack's tag %s is still there: %s", itTag, out)
	}
	if after, err := idOf(releaseTag); err != nil || after != before {
		t.Fatalf("the release tag %s: %q (%v), want the same image %q", releaseTag, after, err, before)
	}
}
