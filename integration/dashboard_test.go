//go:build integration

package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/dashboard"
	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// TestDashboardStopAndControlRemoveAgainstRealDocker drives the dashboard
// and the control socket against a real daemon: one project with a kept
// service and two volumes is stopped through a dashboard POST (the kept
// service keeps running), then removed through the control socket, and the
// objects left behind are compared with what reaper.PlanFor promised from
// listings taken just before the call. A second project labelled never
// remove is refused through the same socket and stays untouched. The test
// ends by tearing the projects down and asserting no devlease-it object is
// left on the daemon.
func TestDashboardStopAndControlRemoveAgainstRealDocker(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Docker socket at %s: %v", socket, err)
	}

	randID := randomHex(t, 4)
	it := "devlease-it-" + randID

	root := t.TempDir()
	itDir, neverDir := filepath.Join(root, "it"), filepath.Join(root, "never")
	composeDashboardProject(t, itDir)
	composeLabelProject(t, neverDir)
	compose(t, itDir, it, "up", "-d", "--build")
	teardownIt := func() {
		compose(t, itDir, it, "down", "-v", "--rmi", "local", "--remove-orphans")
	}
	t.Cleanup(teardownIt)

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
	runner := &reaper.Runner{Cli: cli, Store: st, Cfg: cfg}
	ctx := context.Background()

	// First pass: the default lease, so the page renders the project live.
	rep, err := runner.Pass(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !contains(rep.Granted, it) {
		t.Fatalf("first pass granted %v, want the project %s", rep.Granted, it)
	}

	// The dashboard served in process, over the real runner.
	hnd, err := dashboard.New(dashboard.Options{Runner: runner})
	if err != nil {
		t.Fatalf("building the dashboard: %v", err)
	}
	ts := httptest.NewServer(hnd)
	t.Cleanup(ts.Close)

	// The stacks page links the project.
	resp := dashGet(t, ts.URL+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	if page := dashBody(t, resp); !strings.Contains(page, `href="/stacks/`+it+`"`) {
		t.Fatalf("the stacks page does not link %s:\n%s", it, page)
	}

	// The stop POST needs the page's csrf token and the server's own
	// Origin, like a same-origin form submit.
	resp = dashGet(t, ts.URL+"/stacks/"+it)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stacks/%s = %d, want 200", it, resp.StatusCode)
	}
	csrf := dashCSRF(t, dashBody(t, resp))
	resp = dashPost(t, ts, "/stacks/"+it+"/stop", "csrf="+url.QueryEscape(csrf))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST stop = %d, want 303", resp.StatusCode)
	}
	if got := strings.SplitN(resp.Header.Get("Location"), "?", 2)[0]; got != "/stacks/"+it {
		t.Fatalf("POST stop redirects to %q, want the %s page", got, it)
	}

	t.Run("the stop left the non-kept containers exited, the kept one running, the volumes in place", func(t *testing.T) {
		if got := dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(it)); len(got) != 1 {
			t.Fatalf("running containers after the stop: %v, want just the kept one", got)
		}
		if got := dockerIDs(t, "ps", "-aq", "--filter", "label="+projectLabel(it)); len(got) != 2 {
			t.Fatalf("containers after the stop: %v, want the kept and the exited web one", got)
		}
		if got := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(it)); len(got) != 2 {
			t.Fatalf("volumes after the stop: %v, want both in place", got)
		}
	})

	// Listings taken at one moment, so the plan and the removal below
	// describe the same daemon state.
	conts, err := cli.Containers(ctx, it)
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	nets, err := cli.Networks(ctx, it)
	if err != nil {
		t.Fatalf("listing networks: %v", err)
	}
	vols, err := cli.Volumes(ctx, it)
	if err != nil {
		t.Fatalf("listing volumes: %v", err)
	}
	imgs, err := cli.Images(ctx, it)
	if err != nil {
		t.Fatalf("listing images: %v", err)
	}
	plan := reaper.PlanFor(it, reaper.ServicesOf(conts, nil), conts, nets, vols, imgs)

	// The plan promises a bare removal: nothing to stop (the only running
	// container is kept), exactly the non-kept container, its volume and
	// its image, and four kept objects (container, volume, network, image).
	if len(plan.Stop) != 0 {
		t.Fatalf("plan stops %v, want nothing (the kept container runs)", plan.Stop)
	}
	if len(plan.Containers) != 1 || len(plan.Volumes) != 1 || len(plan.Images) != 1 {
		t.Fatalf("plan removes containers %v, volumes %v, images %v; want one of each",
			plan.Containers, plan.Volumes, plan.Images)
	}
	if len(plan.Kept) != 4 {
		t.Fatalf("plan keeps %v, want the kept container, its volume, the network and the kept image", plan.Kept)
	}
	if !contains(plan.Kept, it+"-kept-1") || !contains(plan.Kept, it+"_kept-data") {
		t.Fatalf("plan keeps %v, want %s-kept-1 and %s_kept-data", plan.Kept, it, it)
	}

	// The control socket on a temp path, served in process.
	sockPath := filepath.Join(root, "control.sock")
	closeFn, err := control.Listen(ctx, sockPath, runner)
	if err != nil {
		t.Fatalf("opening the control socket: %v", err)
	}
	t.Cleanup(func() { closeFn() })

	ans, err := control.Call(ctx, sockPath, control.Request{Action: "remove", Project: it, Confirm: it})
	if err != nil {
		t.Fatalf("remove over the control socket: %v", err)
	}
	if !ans.OK {
		t.Fatalf("remove refused: %s (%s)", ans.Error, ans.Code)
	}
	if ans.Report == nil || !contains(ans.Report.Removed, it) {
		t.Fatalf("remove report = %+v, want the project %s removed", ans.Report, it)
	}

	// What is left is exactly what the plan kept.
	keptSet := map[string]bool{}
	for _, n := range plan.Kept {
		keptSet[n] = true
	}
	keptNames := func(objects []docker.Object) []string {
		var out []string
		for _, o := range objects {
			if keptSet[o.Name] {
				out = append(out, o.Name)
			}
		}
		return out
	}

	t.Run("the removal followed the promised plan", func(t *testing.T) {
		left := dockerNames(t, "containers", "{{.Names}}", projectLabel(it))
		want := keptNames(conts)
		if !sameSet(left, want) {
			t.Fatalf("containers left = %v, want the kept %v", left, want)
		}
		if got := dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(it)); len(got) != len(want) {
			t.Fatalf("running containers left = %v, want the kept %v still running", got, want)
		}
		if leftV := dockerNames(t, "volumes", "{{.Name}}", projectLabel(it)); !sameSet(leftV, keptNames(vols)) {
			t.Fatalf("volumes left = %v, want the kept %v", leftV, keptNames(vols))
		}
		if leftN := dockerNames(t, "networks", "{{.Name}}", projectLabel(it)); !sameSet(leftN, keptNames(nets)) {
			t.Fatalf("networks left = %v, want the kept %v", leftN, keptNames(nets))
		}
		if leftI := dockerNames(t, "images", "{{.Repository}}:{{.Tag}}", projectLabel(it)); !sameSet(leftI, keptNames(imgs)) {
			t.Fatalf("images left = %v, want the kept %v", leftI, keptNames(imgs))
		}
		leases, err := st.Read()
		if err != nil {
			t.Fatalf("reading the store: %v", err)
		}
		if _, ok := leases[it]; ok {
			t.Fatalf("the lease of %s survived the removal", it)
		}
	})

	// A second project, labelled never remove, is refused through the same
	// socket and stays untouched.
	never := "devlease-it-" + randomHex(t, 4)
	compose(t, neverDir, never, "up", "-d", "--build")
	teardownNever := func() {
		compose(t, neverDir, never, "down", "-v", "--rmi", "local", "--remove-orphans")
	}
	t.Cleanup(teardownNever)

	rep, err = runner.Pass(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !contains(rep.Granted, never) {
		t.Fatalf("second pass granted %v, want the project %s", rep.Granted, never)
	}

	ans, err = control.Call(ctx, sockPath, control.Request{Action: "remove", Project: never, Confirm: never})
	if err != nil {
		t.Fatalf("remove of the never-remove stack over the socket: %v", err)
	}
	if ans.OK || ans.Code != control.CodeNeverRemove {
		t.Fatalf("remove of the never-remove stack = ok %v code %q, want code %q", ans.OK, ans.Code, control.CodeNeverRemove)
	}

	t.Run("the never-remove stack is untouched", func(t *testing.T) {
		if got := dockerIDs(t, "ps", "-q", "--filter", "label="+projectLabel(never)); len(got) != 1 {
			t.Fatalf("running containers = %v, want 1", got)
		}
		if got := dockerIDs(t, "volume", "ls", "-q", "--filter", "label="+projectLabel(never)); len(got) != 1 {
			t.Fatalf("volumes = %v, want 1", got)
		}
	})

	// Tear down explicitly (compose down on a missing project is a no-op,
	// so the t.Cleanup copies are harmless) and assert the daemon is back
	// as found.
	teardownNever()
	teardownIt()

	t.Run("nothing devlease-it is left on the daemon", func(t *testing.T) {
		for _, kind := range []struct {
			name string
			args []string
		}{
			{"ps", []string{"ps", "-a", "--format", `{{.Label "com.docker.compose.project"}}`}},
			{"volume", []string{"volume", "ls", "--format", "{{.Name}}"}},
			{"network", []string{"network", "ls", "--format", "{{.Name}}"}},
			{"images", []string{"images", "--format", "{{.Repository}}"}},
		} {
			for _, line := range dockerIDs(t, kind.args...) {
				if strings.HasPrefix(line, "devlease-it-") {
					t.Fatalf("docker %s left %q behind", kind.name, line)
				}
			}
		}
	})
}

// noFollow keeps redirects from being followed, so the 303 is visible.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// dashGet is one GET against the in-process dashboard.
func dashGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dashDo(t, req)
}

// dashPost is one form POST with the server's own URL as the Origin, like
// a same-origin page submit.
func dashPost(t *testing.T, ts *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	return dashDo(t, req)
}

func dashDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func dashBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var dashCSRFRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// dashCSRF pulls the page's csrf token, the one every POST must send.
func dashCSRF(t *testing.T, page string) string {
	t.Helper()
	m := dashCSRFRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token on the page")
	}
	return m[1]
}

// dockerNames lists the names of one object type under one project label:
// containers by display name, volumes by name, networks by name, images by
// repo:tag.
func dockerNames(t *testing.T, kind, format, label string) []string {
	t.Helper()
	args := []string{"--format", format, "--filter", "label=" + label}
	switch kind {
	case "containers":
		args = append([]string{"ps", "-a"}, args...)
	case "volumes":
		args = append([]string{"volume", "ls"}, args...)
	case "networks":
		args = append([]string{"network", "ls"}, args...)
	case "images":
		args = append([]string{"images"}, args...)
	default:
		t.Fatalf("unknown kind %s", kind)
	}
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

// sameSet reports whether both slices hold the same strings, ignoring
// order and duplicates.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	in := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	for _, s := range a {
		if !in(b, s) {
			return false
		}
	}
	return true
}

// composeDashboardProject writes a project dir with a kept service on its
// own volume and a plain service on another volume: the dashboard stop
// keeps the first running and the removal leaves its volume in place.
func composeDashboardProject(t *testing.T, dir string) {
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
  kept:
    build: .
    command: ["/sleep"]
    labels:
      dev-lease.keep: "true"
    volumes:
      - kept-data:/data
volumes:
  web-data:
  kept-data:
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
