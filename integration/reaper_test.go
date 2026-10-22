//go:build integration

// Package integration runs dev-lease against a real Docker daemon: two
// Compose projects are started (one managed, one not), the managed lease
// is written with a one-minute TTL and its clock injected past expiry, and
// a reaper pass must stop the managed project while the unmanaged one
// keeps running untouched. A renewed lease revives the stopped project,
// and once the stopped time is past remove-after, the next pass removes
// the managed project completely.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

const (
	socket  = "/var/run/docker.sock"
	pattern = "devlease-it-*"
)

func TestReaperLifecycleStopsRevivesRemoves(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Docker socket at %s: %v", socket, err)
	}

	randID := randomHex(t, 4)
	it := "devlease-it-" + randID
	keep := "devlease-keep-" + randID

	root := t.TempDir()
	itDir, keepDir := filepath.Join(root, "it"), filepath.Join(root, "keep")
	composeProject(t, itDir)
	composeProject(t, keepDir)
	compose(t, itDir, it, "up", "-d", "--build")
	compose(t, keepDir, keep, "up", "-d", "--build")
	// A named volume labelled dev-lease.keep that no kept service mounts:
	// the removal must leave it alone. Compose only creates volumes a
	// service mounts, so the volume is created explicitly and torn down in
	// the cleanup (compose down -v would not know it).
	keptVol := it + "_keep-data"
	if out, err := exec.Command("docker", "volume", "create",
		"--label", projectLabel(it), "--label", "dev-lease.keep=true", keptVol).CombinedOutput(); err != nil {
		t.Fatalf("creating the kept volume: %s", out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "volume", "rm", "-f", keptVol).Run()
	})
	// Whatever the outcome, both projects belong to this test and are torn
	// down by name; nothing else on the daemon is ever touched.
	t.Cleanup(func() {
		compose(t, itDir, it, "down", "-v", "--rmi", "local", "--remove-orphans")
		compose(t, keepDir, keep, "down", "-v", "--rmi", "local", "--remove-orphans")
	})

	cli := docker.New(socket)
	st := lease.Store{Path: filepath.Join(root, "leases.json")}
	cfg := reaper.Config{
		Patterns:    []string{pattern},
		Own:         "dev-lease",
		DefaultTTL:  time.Hour,
		RemoveAfter: time.Hour,
		StopTimeout: 5 * time.Second,
		Log:         os.Stdout,
	}
	ctx := context.Background()

	// First pass: the managed project gets the default lease and stays.
	rep, err := reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !contains(rep.Granted, it) {
		t.Fatalf("first pass granted %v, want %s", rep.Granted, it)
	}
	if !composeProjectPresent(t, it) {
		t.Fatal("managed project disappeared on the granting pass")
	}

	// Inject the clock: rewrite the one-minute lease so every claim expired
	// a minute ago.
	expireLease(t, st, it)

	// Second pass: at expiry the project is stopped, not removed: its
	// containers exist and are exited, and its volume, network and image
	// survive.
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if contains(rep.Removed, it) {
		t.Fatal("the project was removed at expiry; the stop must come first")
	}

	t.Run("stopped project survives the expiry pass", func(t *testing.T) {
		if got := dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(it)); len(got) != 0 {
			t.Fatalf("running containers left: %v", got)
		}
		if got := dockerIDs(t, "ps", "-aq", "--filter", "label="+projectLabel(it)); len(got) == 0 {
			t.Fatal("all containers were removed; they must exist, exited")
		}
		// The project's volume and the unmounted kept volume both survive
		// the stop.
		if got := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(it)); len(got) != 2 {
			t.Fatalf("volumes left at expiry = %v, want the project volume and the kept volume", got)
		}
		if got := dockerIDs(t, "network", "ls", "-q", "--filter", "label="+projectLabel(it)); len(got) == 0 {
			t.Fatal("network was removed at expiry; it must survive the stop")
		}
		if out, err := exec.Command("docker", "image", "inspect", it+"-web").CombinedOutput(); err != nil {
			t.Fatalf("image %s is gone at expiry: %s", it+"-web", out)
		}
		leases, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		l, ok := leases[it]
		if !ok {
			t.Fatal("lease disappeared on the stopping pass")
		}
		if l.State != lease.StateStopped || l.StoppedAt.IsZero() {
			t.Fatalf("state = %q stopped-at %v, want stopped with a time", l.State, l.StoppedAt)
		}
	})

	// Revive a stopped project by hand: clear the stopped mark and make the
	// claim live again.
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m[it]
		l.State = ""
		l.StoppedAt = time.Time{}
		var err error
		l, err = lease.Extend(l, "reviver", time.Hour, time.Now().UTC())
		if err != nil {
			return err
		}
		m[it] = l
		return nil
	}); err != nil {
		t.Fatalf("reviving the lease: %v", err)
	}

	// Third pass: a live lease leaves the (still stopped) containers alone;
	// starting them is the developer's job, dev-lease never starts.
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if contains(rep.Removed, it) || contains(rep.Failed, it) {
		t.Fatalf("revived project was touched: removed %v failed %v", rep.Removed, rep.Failed)
	}
	if got := dockerIDs(t, "ps", "-aq", "--filter", "label="+projectLabel(it)); len(got) == 0 {
		t.Fatal("revived project's containers were removed")
	}

	// Expire again: the pass marks the project stopped once more.
	expireLease(t, st, it)
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("fourth pass: %v", err)
	}
	if contains(rep.Removed, it) {
		t.Fatal("the project was removed before its remove-after elapsed")
	}

	// Backdate the stopped time past remove-after: the next pass removes
	// the project completely.
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m[it]
		l.StoppedAt = time.Now().UTC().Add(-2 * cfg.RemoveAfter)
		m[it] = l
		return nil
	}); err != nil {
		t.Fatalf("backdating the stopped time: %v", err)
	}
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("fifth pass: %v", err)
	}
	if !contains(rep.Removed, it) {
		t.Fatalf("fifth pass removed %v, want %s", rep.Removed, it)
	}

	t.Run("managed project fully gone", func(t *testing.T) {
		if got := dockerIDs(t, "ps", "-aq", "--filter", "label="+projectLabel(it)); len(got) != 0 {
			t.Fatalf("containers left: %v", got)
		}
		// The kept volume survives (nobody mounts it, the label keeps it);
		// every other volume is gone. The explicit docker volume rm -f in
		// t.Cleanup removes the kept volume (compose down -v does not know it).
		left := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(it))
		if len(left) != 1 || left[0] != it+"_keep-data" {
			t.Fatalf("volumes left = %v, want only %s", left, it+"_keep-data")
		}
		if got := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(it), "--filter", "label=dev-lease.keep=true"); len(got) != 1 {
			t.Fatalf("kept volumes with the keep label = %v, want 1", got)
		}
		if got := dockerIDs(t, "network", "ls", "-q", "--filter", "label="+projectLabel(it)); len(got) != 0 {
			t.Fatalf("networks left: %v", got)
		}
		// The project-built image must be gone too.
		if out, err := exec.Command("docker", "image", "inspect", it+"-web").CombinedOutput(); err == nil {
			t.Fatalf("image %s still present:\n%s", it+"-web", out)
		}
		leases, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := leases[it]; ok {
			t.Fatal("lease of the removed project still in the state file")
		}
	})

	t.Run("unmanaged project untouched", func(t *testing.T) {
		if got := dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(keep)); len(got) == 0 {
			t.Fatal("unmanaged project's container is gone")
		}
		if got := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(keep)); len(got) == 0 {
			t.Fatal("unmanaged project's volume is gone")
		}
		if got := dockerIDs(t, "network", "ls", "-q", "--filter", "label="+projectLabel(keep)); len(got) == 0 {
			t.Fatal("unmanaged project's network is gone")
		}
		if out, err := exec.Command("docker", "image", "inspect", keep+"-web").CombinedOutput(); err != nil {
			t.Fatalf("unmanaged project's image is gone: %s", out)
		}
		leases, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := leases[keep]; ok {
			t.Fatal("unmanaged project got a lease")
		}
	})
}

// expireLease rewrites the one-minute lease so every claim expired a minute
// ago.
func expireLease(t *testing.T, st lease.Store, project string) {
	t.Helper()
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m[project]
		for i := range l.Holders {
			l.Holders[i].TTL = time.Minute
			l.Holders[i].Expires = time.Now().UTC().Add(-time.Minute)
		}
		m[project] = l
		return nil
	}); err != nil {
		t.Fatalf("writing the expired lease: %v", err)
	}
}

func projectLabel(p string) string { return "com.docker.compose.project=" + p }

// composeProject writes a project dir with a compose file, a Dockerfile and
// the sleep binary, so `compose up --build` builds a project-owned image.
func composeProject(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(`services:
  web:
    build: .
    command: ["/sleep"]
    volumes:
      - web-data:/data
volumes:
  web-data:
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM scratch
COPY sleep /sleep
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "sleep"), filepath.Join(repoRoot, "integration", "testdata", "sleep"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the sleep binary: %v\n%s", err, out)
	}
}

// compose runs docker compose against one project dir. On failure the
// cleanup is still registered, so the daemon is left as found.
func compose(t *testing.T, dir, project string, args ...string) {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose", "-f", filepath.Join(dir, "compose.yml"), "-p", project}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %s (%s): %v\n%s", strings.Join(args, " "), project, err, out)
	}
}

func composeProjectPresent(t *testing.T, project string) bool {
	t.Helper()
	return len(dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(project))) > 0
}

func dockerIDs(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// A kept service and a kept volume survive the full lifecycle while the
// rest of the project is stopped and then removed. The remove-after label
// on the other service sets the project's removal delay. After the removal
// the project is not present any more: only kept objects are left, and
// they do not count.
func TestPolicyLabelsKeepServiceAndVolume(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Docker socket at %s: %v", socket, err)
	}

	randID := randomHex(t, 4)
	it := "devlease-it-" + randID

	root := t.TempDir()
	itDir := filepath.Join(root, "it")
	composePolicyProject(t, itDir)
	compose(t, itDir, it, "up", "-d", "--build")
	t.Cleanup(func() {
		compose(t, itDir, it, "down", "-v", "--rmi", "local", "--remove-orphans")
	})

	cli := docker.New(socket)
	st := lease.Store{Path: filepath.Join(root, "leases.json")}
	cfg := reaper.Config{
		Patterns:    []string{pattern},
		Own:         "dev-lease",
		DefaultTTL:  time.Hour,
		RemoveAfter: time.Hour,
		StopTimeout: 5 * time.Second,
		Log:         os.Stdout,
	}
	ctx := context.Background()

	rep, err := reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !contains(rep.Granted, it) {
		t.Fatalf("first pass granted %v, want %s", rep.Granted, it)
	}
	expireLease(t, st, it)

	// The stop pass: the kept service keeps running, the other one stops,
	// and the label's 2h is recorded in the lease.
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if contains(rep.Removed, it) {
		t.Fatal("the project was removed at expiry; the stop must come first")
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := leases[it]
	if !ok || l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want stopped", l)
	}
	if l.RecordedDelay(0) != 2*time.Hour {
		t.Fatalf("RemoveAfter = %v, want 2h from the service label", l.RecordedDelay(0))
	}
	running := dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(it))
	if len(running) != 1 {
		t.Fatalf("running containers after the stop = %v, want only the kept service", running)
	}
	if out, err := exec.Command("docker", "ps", "--filter", "id="+running[0], "--format", "{{.Label \"dev-lease.keep\"}}").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("the running container is not the kept service: %v, %s", err, out)
	}

	// Backdate the stopped time past the label's 2h: the next pass removes
	// everything except the kept service and what it uses. (A label below
	// 1h is unreadable, like the flag's lower bound, so the test cannot
	// shorten the delay with a 1m label.)
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m[it]
		l.StoppedAt = time.Now().UTC().Add(-3 * time.Hour)
		m[it] = l
		return nil
	}); err != nil {
		t.Fatalf("backdating the stopped time: %v", err)
	}
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if !contains(rep.Removed, it) {
		t.Fatalf("third pass removed %v, want %s", rep.Removed, it)
	}

	t.Run("kept objects survive, the rest is gone", func(t *testing.T) {
		if got := dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(it)); len(got) != 1 {
			t.Fatalf("running containers after the removal = %v, want the kept service", got)
		}
		vols := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(it))
		if len(vols) != 1 || !strings.Contains(vols[0], "kept-data") {
			t.Fatalf("volumes after the removal = %v, want only the kept volume", vols)
		}
		if got := dockerIDs(t, "network", "ls", "-q", "--filter", "label="+projectLabel(it)); len(got) == 0 {
			t.Fatal("network in use by the kept service was removed")
		}
		if out, err := exec.Command("docker", "image", "inspect", it+"-kept").CombinedOutput(); err != nil {
			t.Fatalf("kept service's image is gone: %s", out)
		}
		if out, err := exec.Command("docker", "image", "inspect", it+"-web").CombinedOutput(); err == nil {
			t.Fatalf("removed service's image still present:\n%s", out)
		}
		leases, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := leases[it]; ok {
			t.Fatal("lease of the removed project still in the state file")
		}
	})

	// Only kept objects are left: the project is not present any more, so
	// no lease is granted again.
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("fourth pass: %v", err)
	}
	if len(rep.Granted) != 0 || len(rep.Managed) != 0 {
		t.Fatalf("fourth pass = %+v, want nothing granted or managed for a kept-only project", rep)
	}
}

// composePolicyProject writes a project dir whose compose file has one kept
// service with a kept volume and one plain service with a remove-after
// label, plus the sleep binary, so `compose up --build` builds both images.
func composePolicyProject(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(`services:
  kept:
    build: .
    command: ["/sleep"]
    labels:
      dev-lease.keep: "true"
    volumes:
      - kept-data:/data
  web:
    build: .
    command: ["/sleep"]
    labels:
      dev-lease.remove-after: "2h"
    volumes:
      - web-data:/data
volumes:
  kept-data:
  web-data:
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM scratch
COPY sleep /sleep
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "sleep"), filepath.Join(repoRoot, "integration", "testdata", "sleep"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the sleep binary: %v\n%s", err, out)
	}
}

// A stack labelled dev-lease.remove-after=0 keeps never-remove through a
// compose down: the sweep records the label while the containers are
// there, the stop after the down uses the recorded value, and no pass
// ever removes the volume.
func TestLabelZeroSurvivesComposeDown(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Docker socket at %s: %v", socket, err)
	}

	randID := randomHex(t, 4)
	it := "devlease-it-" + randID

	root := t.TempDir()
	itDir := filepath.Join(root, "it")
	composeLabelProject(t, itDir)
	compose(t, itDir, it, "up", "-d", "--build")
	t.Cleanup(func() {
		compose(t, itDir, it, "down", "-v", "--rmi", "local", "--remove-orphans")
	})

	cli := docker.New(socket)
	st := lease.Store{Path: filepath.Join(root, "leases.json")}
	cfg := reaper.Config{
		Patterns:    []string{pattern},
		Own:         "dev-lease",
		DefaultTTL:  time.Hour,
		RemoveAfter: time.Hour,
		StopTimeout: 5 * time.Second,
		Log:         os.Stdout,
	}
	ctx := context.Background()

	// First pass: the default lease and the recorded label delay.
	rep, err := reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !contains(rep.Granted, it) {
		t.Fatalf("first pass granted %v, want %s", rep.Granted, it)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := leases[it]
	if !ok || l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("lease = %+v, want the 0 label recorded on the first pass", l)
	}

	// compose down: the containers are gone, the volume keeps the project
	// present.
	compose(t, itDir, it, "down")
	if got := dockerIDs(t, "ps", "-aq", "--filter", "label="+projectLabel(it)); len(got) != 0 {
		t.Fatalf("containers left after the down: %v", got)
	}
	if got := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(it)); len(got) != 1 {
		t.Fatalf("volumes after the down = %v, want the project volume", got)
	}

	// Expiry: the stop has no containers to read the label from and keeps
	// the recorded 0.
	expireLease(t, st, it)
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("expiry pass: %v", err)
	}
	if contains(rep.Removed, it) {
		t.Fatal("the project was removed at expiry")
	}
	leases, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, ok = leases[it]
	if !ok || l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("lease after the stop = %+v, want stopped with 0 from the label", l)
	}

	// Thirty days later no pass removes the volume.
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m[it]
		l.StoppedAt = time.Now().UTC().Add(-30 * 24 * time.Hour)
		m[it] = l
		return nil
	}); err != nil {
		t.Fatalf("backdating the stopped time: %v", err)
	}
	rep, err = reaper.Pass(ctx, cli, st, cfg)
	if err != nil {
		t.Fatalf("final pass: %v", err)
	}
	if contains(rep.Removed, it) {
		t.Fatalf("final pass removed %s: the recorded 0 label must keep the volume", it)
	}
	if got := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(it)); len(got) != 1 {
		t.Fatalf("volumes after the final pass = %v, want the project volume", got)
	}
	leases, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leases[it]; !ok {
		t.Fatal("the never-remove stopped lease was dropped")
	}
}

// composeLabelProject writes a project dir whose web service carries
// dev-lease.remove-after=0 and mounts a volume, so the volume survives a
// compose down and keeps the project present. Like composeProject it
// builds the sleep binary.
func composeLabelProject(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(`services:
  web:
    build: .
    command: ["/sleep"]
    labels:
      dev-lease.remove-after: "0"
    volumes:
      - web-data:/data
volumes:
  web-data:
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM scratch
COPY sleep /sleep
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "sleep"), filepath.Join(repoRoot, "integration", "testdata", "sleep"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the sleep binary: %v\n%s", err, out)
	}
}
