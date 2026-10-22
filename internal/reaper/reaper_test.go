package reaper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
)

var testNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

type harness struct {
	srv *fakedocker.Server
	st  lease.Store
	log *bytes.Buffer
	cfg Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		srv: fakedocker.New(t),
		st:  lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")},
		log: &bytes.Buffer{},
		cfg: Config{
			Patterns:    []string{"myapp", "other", "dev-lease-*"},
			Own:         "dev-lease",
			DefaultTTL:  2 * time.Hour,
			RemoveAfter: 168 * time.Hour,
			StopTimeout: 10 * time.Second,
			Now:         func() time.Time { return testNow },
			Log:         nil, // set below
		},
	}
}

func (h *harness) pass(t *testing.T) Report {
	t.Helper()
	return h.passCtx(t, context.Background())
}

func (h *harness) passCtx(t *testing.T, ctx context.Context) Report {
	t.Helper()
	h.cfg.Log = h.log
	rep, err := Pass(ctx, docker.New(h.srv.Socket), h.st, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (h *harness) extend(project string, ttl time.Duration, at time.Time) {
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l, err := lease.Extend(m[project], "ci", ttl, at)
		if err != nil {
			return err
		}
		if l.State == lease.StateStopped {
			// The client's extend on a stopped project gives a normal live
			// lease: state and StoppedAt are cleared.
			l.State = ""
			l.StoppedAt = time.Time{}
		}
		m[project] = l
		return nil
	}); err != nil {
		panic(err)
	}
}

// stopAndAge runs one pass so the expired project is stopped, then
// backdates its StoppedAt past RemoveAfter: the next pass removes it.
func (h *harness) stopAndAge(t *testing.T, project string) {
	t.Helper()
	h.pass(t)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m[project]
		if l.State != lease.StateStopped {
			return fmt.Errorf("%s: state = %q, want stopped after the expiry pass", project, l.State)
		}
		l.StoppedAt = l.StoppedAt.Add(-h.cfg.RemoveAfter)
		m[project] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ageStopped backdates the StoppedAt of every stopped managed project past
// RemoveAfter, so the next pass removes them.
func (h *harness) ageStopped(t *testing.T) {
	t.Helper()
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		for name, l := range m {
			if l.State == lease.StateStopped {
				l.StoppedAt = l.StoppedAt.Add(-h.cfg.RemoveAfter)
				m[name] = l
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) leases() map[string]lease.Lease {
	m, err := h.st.Read()
	if err != nil {
		panic(err)
	}
	return m
}

func (h *harness) logLines() []map[string]any {
	var out []map[string]any
	dec := json.NewDecoder(h.log)
	for {
		var line map[string]any
		if err := dec.Decode(&line); err != nil {
			break
		}
		out = append(out, line)
	}
	return out
}

func fullProject(srv *fakedocker.Server) {
	// Like a real compose project: the containers were created from the
	// project's image.
	srv.Add("image", "sha256:i1", "myapp-web:latest", "myapp")
	srv.AddContainerWithImage("c1", "web", "myapp", "sha256:i1")
	srv.AddContainerWithImage("c2", "db", "myapp", "sha256:i1")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")
}

// Compose cannot create a project outside its own rule
// ^[a-z0-9][a-z0-9_-]*$, so such a name can never be managed: it can never
// exist on the daemon, and refusing it early stops shell metacharacters
// from reaching later handling.
func TestManagedRefusesNamesComposeCannotCreate(t *testing.T) {
	// A catch-all pattern would match every name, so the refusals below
	// must come from the name rule itself, not the patterns.
	cfg := Config{Patterns: []string{"*"}, Own: "dev-lease"}
	for _, p := range []string{"myapp;touch", "my.app", "MyApp", "-x", ""} {
		if Managed(p, cfg) {
			t.Errorf("Managed(%q) = true, want false: compose cannot create such a project", p)
		}
	}
	if !Managed("myapp_a-1", cfg) {
		t.Errorf("Managed(%q) = false, want true with a catch-all pattern", "myapp_a-1")
	}
}

func TestManagedMatching(t *testing.T) {
	cfg := Config{Patterns: []string{"myapp", "myapp-*"}, Own: "dev-lease"}
	for _, p := range []string{"myapp", "myapp-extra"} {
		if !Managed(p, cfg) {
			t.Errorf("Managed(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"dev-lease", "dev-lease-2", "other", "", "myap"} {
		if Managed(p, cfg) {
			t.Errorf("Managed(%q) = true, want false", p)
		}
	}
	// The own project is never managed, even by a matching pattern.
	if Managed("dev-lease", Config{Patterns: []string{"*"}, Own: "dev-lease"}) {
		t.Error("Managed(own) = true, want false")
	}
	if !Managed("dev-lease-2", Config{Patterns: []string{"dev-lease-*"}, Own: "dev-lease"}) {
		t.Error("Managed(dev-lease-2) = false, want true")
	}
}

// Given a project whose lease expired, the first pass stops it and the
// removal runs once RemoveAfter has passed since the stop.
func TestPassRemovesExpiredProjectAndClearsState(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none: expiry stops, it does not remove", rep.Removed)
	}
	h.stopAndAge(t, "myapp")
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}

	for _, kind := range []string{"container", "network", "volume", "image"} {
		if got := h.srv.Count(kind); got != 0 {
			t.Errorf("fake daemon still has %d %s objects, want 0", got, kind)
		}
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after removal")
	}
}

// Given an expired project, the removal pass still stops every container
// before removing it (the stop ran once at expiry, but a container may
// have been started again since).
func TestPassStopsContainersBeforeRemoving(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	h.pass(t)

	order := map[string]int{}
	for i, call := range h.srv.Calls() {
		order[call.Method+" "+call.Path] = i
	}
	checks := [][2]string{
		{"POST /v1.47/containers/c1/stop", "DELETE /v1.47/containers/c1"},
		{"DELETE /v1.47/containers/c2", "DELETE /v1.47/networks/n1"},
		{"DELETE /v1.47/networks/n1", "DELETE /v1.47/volumes/myapp_data"},
		{"DELETE /v1.47/volumes/myapp_data", "DELETE /v1.47/images/sha256:i1"},
	}
	for _, c := range checks {
		a, okA := order[c[0]]
		b, okB := order[c[1]]
		if !okA || !okB {
			t.Errorf("call %q or %q not observed", c[0], c[1])
			continue
		}
		if a > b {
			t.Errorf("call %q (idx %d) must happen before %q (idx %d)", c[0], a, c[1], b)
		}
	}
}

func TestPassGrantsDefaultLeaseToNewManagedProject(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)

	rep := h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]", rep.Granted)
	}
	l, ok := h.leases()["myapp"]
	if !ok {
		t.Fatal("no default lease granted")
	}
	if got, want := l.ExpiresAt(), testNow.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("default lease expiry = %v, want %v", got, want)
	}

	// The default lease keeps the project alive on the next pass.
	if rep := h.pass(t); len(rep.Granted) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("second pass = %+v, want no changes", rep)
	}
	if h.srv.Count("container") != 2 {
		t.Fatal("project with a live default lease was touched")
	}
}

func TestPassSkipsUnmanagedProject(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "c1", "ghost-web", "ghost")
	rep := h.pass(t)
	if len(rep.Managed) != 0 || len(rep.Granted) != 0 {
		t.Fatalf("pass = %+v, want no managed projects", rep)
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("unmanaged project was touched")
	}
}

func TestPassSkipsOwnProject(t *testing.T) {
	h := newHarness(t)
	// A pattern that WOULD match the own project if the own guard were not
	// there.
	h.cfg.Patterns = append(h.cfg.Patterns, "dev-lease")
	h.srv.Add("container", "c1", "dev-lease", "dev-lease")
	h.extend("dev-lease", time.Minute, testNow.Add(-2*time.Minute))
	rep := h.pass(t)
	if !h.srv.Has("container", "c1") {
		t.Fatal("pass removed its own project")
	}
	if len(rep.Removed) != 0 || len(rep.Granted) != 0 {
		t.Fatalf("pass = %+v, want no changes", rep)
	}
}

// Given an expired lease for a project with no objects left, the stop
// records the stopped state on the lease; the next pass drops the stopped
// lease: with nothing on the daemon there is nothing to remove and nothing
// to wait for.
func TestPassStopsExpiredLeaseWithNoObjects(t *testing.T) {
	h := newHarness(t)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("pass = %+v, want a clean stop", rep)
	}
	l, ok := h.leases()["myapp"]
	if !ok || l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want it kept in the stopped state", l)
	}
	if !l.StoppedAt.Equal(testNow) {
		t.Fatalf("StoppedAt = %v, want %v", l.StoppedAt, testNow)
	}

	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared when nothing to remove")
	}
}

// Given an expired project whose one container stop fails transiently, the
// failed stop is reported as the container, the stopped state is recorded
// all the same, and the surviving container revives the project with the
// default lease whose expiry retries the stop and ends in a clean removal.
func TestPassRetriesContainerAfterTransientStopFailure(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.RefuseStop("container", "c1", 500)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	rep := h.pass(t)
	if len(rep.Failed) != 1 || rep.Failed[0] != "web" {
		t.Fatalf("Failed = %v, want the failed stop reported as the container", rep.Failed)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none: the stop failed", rep.Removed)
	}
	if !h.srv.Has("container", "c1") || !h.srv.Has("container", "c2") {
		t.Fatal("the stop phase removed a container")
	}
	l, ok := h.leases()["myapp"]
	if !ok || l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want stopped whatever the stop outcome", l)
	}

	// The unstopped c1 still runs, so it revives the project: the default
	// lease is granted and the stop is retried when that expires.
	rep = h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want the revived default lease", rep.Granted)
	}

	h.srv.Unrefuse("container", "c1")
	h.cfg.Now = func() time.Time { return testNow.Add(4 * time.Hour) }
	rep = h.pass(t)
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want the recovered stop to succeed", rep.Failed)
	}
	l = h.leases()["myapp"]
	if l.State != lease.StateStopped {
		t.Fatalf("state = %q, want the retried stop to stop the project", l.State)
	}

	// Past RemoveAfter the removal finishes.
	h.stopAndAge(t, "myapp")
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("final Removed = %v, want [myapp]", rep.Removed)
	}
	for _, kind := range []string{"container", "network", "volume", "image"} {
		if got := h.srv.Count(kind); got != 0 {
			t.Errorf("fake daemon still has %d %s objects, want 0", got, kind)
		}
	}
}

func TestPassLeavesOtherProjectsAlone(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Add("container", "c3", "other-web", "other")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.extend("other", time.Hour, testNow)

	h.pass(t)
	if !h.srv.Has("container", "c3") {
		t.Fatal("live other project was removed")
	}
	if !h.srv.IsStopped("container", "c1") {
		t.Fatal("the expired project's container was not stopped")
	}
}

// Given a stopped project whose only leftover is a stopped container (the
// stop succeeded), the stopped container keeps the project present but
// does not revive it: nothing runs, so nothing is granted. Once
// RemoveAfter has passed, the removal retries and succeeds.
func TestPassStoppedContainerLeftoverKeepsProjectPresent(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithImage("c1", "db", "myapp", "sha256:pulled")
	h.srv.Add("network", "n1", "myapp_default", "myapp")
	h.srv.StopContainer("c1")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // expiry pass: nothing to stop, the stopped state is recorded

	// Next pass: the stopped container keeps the project present but does
	// not revive it (nothing is running); the stopped state is untouched.
	h.cfg.Now = func() time.Time { return testNow.Add(time.Minute) }
	rep := h.pass(t)
	if len(rep.Granted) != 0 {
		t.Fatalf("Granted = %v, want none: nothing is running, a stopped container must not revive the project", rep.Granted)
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("state = %q, want the stopped lease untouched", l.State)
	}

	// Once RemoveAfter has passed the removal runs and succeeds.
	h.stopAndAge(t, "myapp")
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("container", "c1") {
		t.Fatal("the stopped container of the stopped project was never retried")
	}
}

// Given two stopped projects past RemoveAfter and a cancellation landing
// during the first one's removal, the pass stops before the second: its
// lease stays as it was, and the next pass removes it normally.
func TestPassCancelKeepsLaterProjectLeasesUntouched(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Add("container", "o1", "other-web", "other")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // expiry pass: both projects are stopped
	h.ageStopped(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.srv.BeforeRemove("container", "c1", cancel)
	h.cfg.Log = h.log
	if _, err := Pass(ctx, docker.New(h.srv.Socket), h.st, h.cfg); err != nil {
		t.Fatal(err)
	}
	l, ok := h.leases()["other"]
	if !ok {
		t.Fatal("the cancelled pass deleted the later project's lease without trying it")
	}
	if l.State != lease.StateStopped {
		t.Fatalf("other's lease was touched: %+v", l)
	}
	// The cancellation is logged as the removal's error, in the one JSON
	// line per action shape every other act carries.
	if !strings.Contains(h.log.String(), `"action":"remove"`) || !strings.Contains(h.log.String(), "context canceled") {
		t.Fatalf("log = %q, want a remove action with the cancellation", h.log.String())
	}

	// The next pass removes other normally.
	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "other" {
		t.Fatalf("Removed = %v, want [other]", rep.Removed)
	}
	if h.srv.Has("container", "o1") {
		t.Fatal("other was not removed on the next pass")
	}
}

// cancelOnLog cancels the pass the moment a log line contains needle.
type cancelOnLog struct {
	cancel func()
	needle string
}

func (c *cancelOnLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), c.needle) {
		c.cancel()
	}
	return len(p), nil
}

// Given a cancellation landing right after the last container was removed,
// the networks, volume and images are never tried, the project is reported
// in Failed, not Removed, and the lease is deleted like for any failure.
func TestPassCancelledRemovalIsFailedNotRemoved(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	ctx, cancel := context.WithCancel(context.Background())
	w := &cancelOnLog{cancel: cancel, needle: `"action":"remove","id":"c2"`}
	h.cfg.Log = w
	rep, err := Pass(ctx, docker.New(h.srv.Socket), h.st, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none: the cancellation cut the removal short", rep.Removed)
	}
	if len(rep.Failed) != 1 || rep.Failed[0] != "myapp" {
		t.Fatalf("Failed = %v, want [myapp]", rep.Failed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("the lease of the cancelled removal was not deleted")
	}
	if !h.srv.Has("network", "n1") {
		t.Fatal("the network was removed although the pass was cancelled")
	}
}

// failNthList fails the n-th GET list call of one kind (the first call is
// the Projects listing; for containers the removal pass adds the revive
// check's listing between the two).
func failNthList(h *harness, kind string, n int, status int) {
	suffix := "/" + kind + "s"
	if kind == "container" {
		suffix = "/containers/json"
	}
	calls := map[string]int{}
	h.srv.OnRequest(func(method, path string) {
		if method != "GET" || !strings.HasSuffix(path, suffix) {
			return
		}
		calls[kind]++
		if calls[kind] == n {
			h.srv.FailList(kind, status)
		}
	})
}

// Given a stopped project whose container listing fails during the
// removal, the removal of that project ends for this pass: the project is
// reported in Failed, the lease is deleted and nothing is touched.
func TestPassFailedContainerListingEndsRemoval(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	// Pass order of the GET container listings: 1 = Projects, 2 = the
	// revive check for the stopped project, 3 = the removal's listing.
	failNthList(h, "container", 3, 500)

	rep := h.pass(t)
	if len(rep.Failed) != 1 || rep.Failed[0] != "myapp" {
		t.Fatalf("Failed = %v, want [myapp]", rep.Failed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the failed listing")
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("the project was touched although the listing failed")
	}
}

// Given a stopped project whose network listing fails during the removal,
// the same: the project is reported in Failed and the lease is deleted.
func TestPassFailedNetworkListingEndsRemoval(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	failNthList(h, "network", 2, 500)

	rep := h.pass(t)
	if len(rep.Failed) != 1 || rep.Failed[0] != "myapp" {
		t.Fatalf("Failed = %v, want [myapp]", rep.Failed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the failed listing")
	}
}

// Given a stopped project whose volume listing fails during the removal,
// the same: the project is reported in Failed and the lease is deleted.
func TestPassFailedVolumeListingEndsRemoval(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	failNthList(h, "volume", 2, 500)

	rep := h.pass(t)
	if len(rep.Failed) != 1 || rep.Failed[0] != "myapp" {
		t.Fatalf("Failed = %v, want [myapp]", rep.Failed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the failed listing")
	}
}

// Given a daemon that answers 409 on the stop of one container, the stop
// is reported as skipped, the container is not removed in that pass, and
// the still-running container revives the project with the default lease
// on the next pass.
func TestPassRefusedStopSkipsContainer(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.srv.RefuseStop("container", "c1", 409)

	rep := h.pass(t)
	if !slices.Contains(rep.Skipped, "web") {
		t.Fatalf("Skipped = %v, want the refused container among them", rep.Skipped)
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("the refused container was removed in the same pass")
	}
	// The stop pass removes nothing: not a single DELETE may go out.
	for _, call := range h.srv.Calls() {
		if call.Method == "DELETE" && strings.Contains(call.Path, "/containers/c1") {
			t.Fatalf("the stopping pass deleted the refused container: %s %s", call.Method, call.Path)
		}
	}
	l, ok := h.leases()["myapp"]
	if !ok || l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want the stopped state whatever the stop outcome", l)
	}

	// The refused stop left c1 running: it revives the project with the
	// default lease and the stop is retried when that expires.
	h.cfg.Now = func() time.Time { return testNow.Add(time.Minute) }
	rep = h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]: the still-running container revives the project", rep.Granted)
	}
}

// Given a daemon that answers 404 on the stop of one container, the stop
// counts as gone: it is neither skipped nor failed, and the stopped state
// is recorded.
func TestPassMissingContainerOnStopCountsAsGone(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.srv.RefuseStop("container", "c1", 404)

	rep := h.pass(t)
	if slices.Contains(rep.Skipped, "web") || slices.Contains(rep.Failed, "web") {
		t.Fatalf("the 404 stop was reported: Skipped=%v Failed=%v", rep.Skipped, rep.Failed)
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("state = %q, want the stopped state recorded", l.State)
	}
}

// Given an image tagged myapp-custom:latest whose own service label is
// web, the tag does not match the <project>-<service> rule: the image is
// neither removed nor counted, so the project disappears with its other
// objects and never shows up again because of that image.
func TestPassLeavesMismatchedServiceTagImage(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.AddLabels("image", "sha256:i9", "myapp-custom:latest", map[string]string{
		docker.ProjectLabel: "myapp",
		docker.ServiceLabel: "web",
	})
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if !h.srv.Has("image", "sha256:i9") {
		t.Fatal("the mismatched-service-tag image was removed")
	}

	h.cfg.Now = func() time.Time { return testNow.Add(time.Minute) }
	rep = h.pass(t)
	if len(rep.Granted) != 0 {
		t.Fatalf("Granted = %v, want none: the mismatched image must not count as present", rep.Granted)
	}
}

// Given a lease with an unknown state, the pass clears it like a stale
// removing mark, then treats the project like any other: a present
// project gets the default lease.
func TestPassClearsUnknownLeaseState(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = "zombie"
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp] after clearing the unknown state", rep.Granted)
	}
	l, ok := h.leases()["myapp"]
	if !ok || l.State != "" {
		t.Fatalf("lease after the pass = %+v, want a fresh default lease", l)
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("the project was removed instead of handled like any other")
	}
}

// Given an unmanaged project whose lease carries a stale removing mark,
// the pass leaves the lease alone: the clear loop only touches managed
// projects, and nothing else reads the mark of a project outside the
// patterns.
func TestPassLeavesUnmanagedRemovingLeaseAlone(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "g1", "ghost-web", "ghost")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l, err := lease.Extend(lease.Lease{}, "ci", time.Minute, testNow.Add(-2*time.Minute))
		if err != nil {
			return err
		}
		l.State = lease.StateRemoving
		m["ghost"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	h.pass(t)
	l, ok := h.leases()["ghost"]
	if !ok || l.State != lease.StateRemoving {
		t.Fatalf("lease = %+v, want the unmanaged removing mark untouched", l)
	}
}

// Given a lease with an unknown state that is not expired, the pass
// leaves it alone: an unknown mark from a foreign version must neither
// lock the live project nor be wiped from a lease it still manages.
func TestPassLeavesUnknownStateOnLiveLease(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", 2*time.Hour, testNow.Add(-time.Minute))
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = "zombie"
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.pass(t)
	if len(rep.Granted) != 0 || len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("pass = %+v, want nothing touched for a live lease with an unknown state", rep)
	}
	if h.srv.IsStopped("container", "c1") {
		t.Fatal("the live project's container was stopped")
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("the live project was removed")
	}
	l, ok := h.leases()["myapp"]
	if !ok || l.State != "zombie" {
		t.Fatalf("lease after the pass = %+v, want the unknown state untouched", l)
	}
	found := false
	for _, line := range h.logLines() {
		if line["action"] == "unknown-state" && line["name"] == "zombie" {
			found = true
		}
	}
	if !found {
		t.Fatal("the kept unknown state was not logged")
	}
}

// Given an image of the project that a container of another project also
// runs from, the daemon answers 409 on the removal; the pass classifies it
// as a deliberate refusal and skips it, and the other project's container
// is untouched.
func TestPassSkipsSharedImageAndReportsIt(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	// A container of another project was created from the same image.
	h.srv.AddContainerWithImage("c3", "other-web", "other", "sha256:i1")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "myapp-web:latest" {
		t.Fatalf("Skipped = %v, want [myapp-web:latest]", rep.Skipped)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want none (a skipped image is not a failure)", rep.Failed)
	}
	if !h.srv.Has("image", "sha256:i1") {
		t.Fatal("shared image was removed")
	}
	if !h.srv.Has("container", "c3") {
		t.Fatal("the other project's container was touched")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared although everything else was removed")
	}

	// The skip was logged as one JSON line.
	found := false
	for _, line := range h.logLines() {
		if line["action"] == "remove-image" && line["name"] == "myapp-web:latest" {
			found = true
			if result, _ := line["result"].(string); !strings.Contains(result, "skipped") {
				t.Errorf("skip line result = %v, want skipped", line["result"])
			}
		}
	}
	if !found {
		t.Fatal("image skip not logged")
	}
}

func TestPassRenewalBetweenReadAndMarkAborts(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Add("container", "c3", "other-web", "other")
	h.srv.Add("network", "n3", "other_default", "other")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)       // expiry pass: both projects are stopped
	h.ageStopped(t) // both past RemoveAfter: the next pass removes them

	// While myapp is being removed, a renewal lands for other - after
	// the pass read the state but before other's removing-mark update.
	// Removal order is sorted, so myapp is processed first.
	h.srv.BeforeRemove("container", "c1", func() {
		h.extend("other", time.Hour, testNow)
	})

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if !h.srv.Has("container", "c3") || !h.srv.Has("network", "n3") {
		t.Fatal("other was touched although a renewal arrived")
	}
	l, ok := h.leases()["other"]
	if !ok {
		t.Fatal("other lease vanished")
	}
	if l.State != "" {
		t.Fatalf("other state = %q, want no removing mark", l.State)
	}
	if got, want := l.ExpiresAt(), testNow.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("other expiry = %v, want the renewal %v", got, want)
	}
}

// Given a renewal that lands after the pass read the state but before the
// project's removing-mark update, the mark re-checks inside the store lock
// and aborts the removal: nothing is removed and the lease keeps the
// renewal.
func TestPassRenewalBeforeMarkAborts(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	// The renewal lands during the pass's project listing, before the
	// removal loop marks the lease.
	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/images/json") {
			once.Do(func() { h.extend("myapp", time.Hour, testNow) })
		}
	})

	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("pass = %+v, want no removal", rep)
	}
	for _, kind := range []string{"container", "network", "volume", "image"} {
		if got := h.srv.Count(kind); got == 0 {
			t.Errorf("the renewal race cost the project its %s objects", kind)
		}
	}
	l, ok := h.leases()["myapp"]
	if !ok {
		t.Fatal("lease vanished")
	}
	if l.State != "" {
		t.Fatalf("lease state = %q, want no removing mark", l.State)
	}
	if got, want := l.ExpiresAt(), testNow.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("lease expiry = %v, want the renewal %v", got, want)
	}
}

// Given a managed project that appeared on the daemon without a lease, when
// an extend for it lands while the pass is still listing projects, then the
// extend's holder must survive the default-lease grant.
func TestPassGrantDoesNotOverwriteConcurrentExtend(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/images/json") {
			once.Do(func() {
				if err := h.st.Update(func(m map[string]lease.Lease) error {
					l, err := lease.Extend(m["myapp"], "bob", 4*time.Hour, testNow)
					if err != nil {
						return err
					}
					l.Project = "myapp"
					m["myapp"] = l
					return nil
				}); err != nil {
					t.Error(err)
				}
			})
		}
	})
	h.pass(t)

	l := h.leases()["myapp"]
	if got, want := l.ExpiresAt(), testNow.Add(4*time.Hour); !got.Equal(want) {
		t.Fatalf("lease expiry = %v, want %v: the concurrent 4h extend was overwritten by the default-lease grant", got, want)
	}
	found := false
	for _, x := range l.Holders {
		if x.Name == "bob" {
			found = true
		}
	}
	if !found {
		t.Fatal("the concurrent extend's holder was lost")
	}
}

// Given an expired project whose volume is in use and whose image answers
// 409 on every remove, when the pass runs, then both are reported as
// skipped, the project is fully removed and the lease is deleted - a
// permanently refused object must not lock the project.
func TestPassSkipsInUseVolumeAndImage(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Refuse("volume", "myapp_data", 409)
	h.srv.Refuse("image", "sha256:i1", 409)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want none: in-use objects are skipped, not failures", rep.Failed)
	}
	if len(rep.Skipped) != 2 {
		t.Fatalf("Skipped = %v, want the in-use volume and image", rep.Skipped)
	}
	if !h.srv.Has("volume", "myapp_data") || !h.srv.Has("image", "sha256:i1") {
		t.Fatal("in-use objects were removed")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared although the project is done")
	}

	skipped := map[string]bool{}
	for _, line := range h.logLines() {
		if result, _ := line["result"].(string); strings.Contains(result, "skipped") {
			skipped[line["name"].(string)] = true
		}
	}
	if !skipped["myapp_data"] || !skipped["myapp-web:latest"] {
		t.Fatalf("skip log lines = %v, want both the volume and the image", skipped)
	}
}

// Given an image that carries the project label only because it was built
// FROM a project image, the pass reports it in the log and leaves it: it
// must not widen the removal, and it must not fail the project.
func TestPassReportsUntaggedLeftoverNotRemoved(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.AddUntaggedImage("sha256:i2", "myapp")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", rep.Failed)
	}
	// A policy-left image is reported in the log only, never as a daemon
	// refusal.
	if len(rep.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want none: the untagged leftover is policy-left, not refused", rep.Skipped)
	}
	if !h.srv.Has("image", "sha256:i2") {
		t.Fatal("the untagged leftover was removed")
	}
	if h.srv.Has("image", "sha256:i1") {
		t.Fatal("the project's own image was not removed")
	}
	found := false
	for _, line := range h.logLines() {
		if line["action"] == "remove-image" && line["name"] == "sha256:i2" {
			found = true
			if result, _ := line["result"].(string); !strings.Contains(result, "skipped") {
				t.Errorf("untagged leftover logged as %q, want a skip", result)
			}
		}
	}
	if !found {
		t.Fatal("the untagged leftover was not reported")
	}
}

// Given an untagged image a container of the project runs from, the
// removal collects the image ids the containers ran from at removal time,
// so the untagged image is removed in the same pass.
func TestPassRemovesUntaggedImageUsedByContainer(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	// c2 runs from an untagged image: compose up --build in progress.
	h.srv.AddUntaggedImage("sha256:i2", "myapp")
	h.srv.AddContainerWithImage("c2", "db", "myapp", "sha256:i2")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("image", "sha256:i2") {
		t.Fatal("the untagged image a container ran from was left behind")
	}
	if h.srv.Has("image", "sha256:i1") {
		t.Fatal("the project's own image was left behind")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared")
	}
}

// Given an image with a custom repository name (not <project>-*), the
// removal still removes it in the same pass: the ids the containers ran
// from are collected before the containers are removed.
func TestPassRemovesCustomNamedContainerImage(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("image", "sha256:i1", "renamed-tag:latest", "myapp")
	h.srv.AddContainerWithImage("c1", "web", "myapp", "sha256:i1")
	h.srv.AddContainerWithImage("c2", "db", "myapp", "sha256:i1")
	h.srv.Add("network", "n1", "myapp_default", "myapp")
	h.srv.Add("volume", "v1", "myapp_data", "myapp")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("image", "sha256:i1") {
		t.Fatal("the custom-named image the containers ran from was left behind")
	}
}

// Given an image whose tag has the <project>-<service>:<tag> shape for its
// own service label but no container runs from it, the removal removes it
// at the first expiry, and the project is then not present at all: no
// default-lease cycle.
func TestPassRemovesServiceTaggedImageWithNoContainer(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("image", "sha256:i3", "myapp-web:latest", map[string]string{
		"com.docker.compose.project": "myapp",
		"com.docker.compose.service": "web",
	})
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", rep.Failed)
	}
	if h.srv.Has("image", "sha256:i3") {
		t.Fatal("the tagged project image without a container was not removed")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared")
	}

	// No cycle: with the image gone the project is not present, so the
	// next pass neither grants a lease nor removes anything.
	h.cfg.Now = func() time.Time { return testNow.Add(time.Minute) }
	rep = h.pass(t)
	if len(rep.Managed) != 0 || len(rep.Granted) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("second pass = %+v, want no myapp at all", rep)
	}
}

// Given a tool:latest image built FROM a project image, its inherited
// labels (project AND service) do not make it count: on its own it does
// not make the project present, and during a removal it is left alone and
// reported in the log only.
func TestPassLeavesToolImageBuiltFromProjectImage(t *testing.T) {
	h := newHarness(t)
	toolLabels := map[string]string{
		"com.docker.compose.project": "myapp",
		"com.docker.compose.service": "web",
	}

	// On its own the image does not make the project present: no grant.
	h.srv.AddLabels("image", "sha256:i2", "tool:latest", toolLabels)
	rep := h.pass(t)
	if len(rep.Managed) != 0 || len(rep.Granted) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("pass with only the tool image = %+v, want no managed project", rep)
	}

	// With the project present, the tool image is left alone at removal.
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", rep.Failed)
	}
	if len(rep.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want none: the tool image is policy-left, not refused", rep.Skipped)
	}
	if !h.srv.Has("image", "sha256:i2") {
		t.Fatal("the tool image was removed")
	}
	if h.srv.Has("image", "sha256:i1") {
		t.Fatal("the project's own image was not removed")
	}
	found := false
	for _, line := range h.logLines() {
		if line["action"] == "remove-image" && line["name"] == "tool:latest" {
			found = true
			if result, _ := line["result"].(string); !strings.Contains(result, "skipped") {
				t.Errorf("tool image logged as %q, want a skip", result)
			}
		}
	}
	if !found {
		t.Fatal("the tool image was not reported in the log")
	}
}

// Given an expired project whose network the daemon refuses with 403 (a
// container of another project is still attached to it), the pass reports
// the skip, does not fail the project, and clears the lease. The project
// stays present through the network, so a project re-created under that
// name survives the next pass and gets the default lease.
func TestPassRefusedNetworkSkipsAndRecreationGetsDefaultLease(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Refuse("network", "n1", 403)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want none: a refused network is skipped, not a failure", rep.Failed)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "myapp_default" {
		t.Fatalf("Skipped = %v, want the refused network", rep.Skipped)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared although every object was removed or skipped")
	}

	// The developer brings the stack up again without extending.
	h.srv.Add("container", "c9", "web", "myapp")
	h.srv.Add("volume", "v9", "myapp_data", "myapp")

	// Ten minutes later: the re-created objects must survive the pass and
	// the project gets a fresh default lease.
	h.cfg.Now = func() time.Time { return testNow.Add(10 * time.Minute) }
	rep = h.pass(t)
	if !h.srv.Has("container", "c9") || !h.srv.Has("volume", "myapp_data") {
		t.Fatalf("the re-created project was removed one pass later without a lease of its own (pass: %+v)", rep)
	}
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]: a re-created project gets the default lease", rep.Granted)
	}
	l := h.leases()["myapp"]
	if l.State != "" {
		t.Fatalf("state = %q, want a clean lease", l.State)
	}
	if got, want := l.ExpiresAt(), testNow.Add(10*time.Minute).Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("default lease expiry = %v, want %v", got, want)
	}
}

// Given a removal whose image listing fails (the daemon is unreachable
// again mid-removal), everything removed before the failed listing stays
// removed, the failure is reported as the project, and the lease is
// deleted. A volume re-created under the same name later survives: the
// next pass sees a fresh project and grants the default lease.
func TestPassLeaseSurvivesRemovalWithFailedImageListing(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Refuse("network", "n1", 403)
	// The image listing starts failing once the removal reaches the image
	// step, after the volume delete.
	h.srv.OnRequest(func(method, path string) {
		if method == "DELETE" && strings.Contains(path, "/volumes/") {
			h.srv.FailList("image", 500)
		}
	})
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Failed) != 1 || rep.Failed[0] != "myapp" {
		t.Fatalf("Failed = %v, want the project reported for the failed image listing", rep.Failed)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "myapp_default" {
		t.Fatalf("Skipped = %v, want the refused network", rep.Skipped)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none: the removal ended with a failure", rep.Removed)
	}
	if !h.srv.Has("network", "n1") {
		t.Fatal("the refused network was removed")
	}
	if h.srv.Has("container", "c1") || h.srv.Has("volume", "myapp_data") {
		t.Fatal("objects removed before the failed listing came back")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the failed listing")
	}

	// The re-created volume survives: the project stays present through
	// the refused network, gets the default lease, and nothing recorded
	// earlier can touch the new object.
	h.srv.UnfailList("image")
	h.srv.Add("volume", "v9", "myapp_data", "myapp")
	h.cfg.Now = func() time.Time { return testNow.Add(time.Minute) }
	rep = h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]", rep.Granted)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the re-created volume was removed by the next pass")
	}
}

// Given a volume delete that is cancelled mid-flight (the daemon may or
// may not finish it), the pass reports the failure and deletes the lease
// all the same. A volume re-created under the same name survives the next
// pass: nothing recorded earlier can touch it.
func TestPassRecreatedVolumeSurvivesAfterCancelledVolumeDelete(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method == "DELETE" && strings.Contains(path, "/volumes/") {
			once.Do(cancel)
		}
	})
	h.cfg.Log = h.log
	rep, err := Pass(ctx, docker.New(h.srv.Socket), h.st, h.cfg)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(rep.Failed) == 0 || rep.Failed[len(rep.Failed)-1] != "myapp" {
		// The cancelled volume delete may or may not have reached the
		// daemon: the volume name is in Failed only when it did not.
		// Either way the cancellation is reported as the project.
		t.Fatalf("Failed = %v, want the cancelled removal reported as the project", rep.Failed)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want none: the removal was cancelled", rep.Removed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the interrupted removal")
	}

	h.srv.Add("volume", "v9", "myapp_data", "myapp")
	h.cfg.Now = func() time.Time { return testNow.Add(time.Minute) }
	rep = h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]", rep.Granted)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the re-created volume was removed by the next pass")
	}
}

// Given a stale removing lease left by a serve that died mid-removal, the
// pass clears the lock and treats the project like any other: a present
// project gets the default lease and keeps its objects.
func TestPassClearsStaleRemovingLease(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = "removing"
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.pass(t)
	if len(rep.Removed) != 0 || len(rep.Failed) != 0 {
		t.Fatalf("pass = %+v, want no removal", rep)
	}
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]", rep.Granted)
	}
	if h.srv.Count("container") != 2 {
		t.Fatal("the project's objects were touched")
	}
	l, ok := h.leases()["myapp"]
	if !ok {
		t.Fatal("no default lease granted")
	}
	if l.State != "" {
		t.Fatalf("state = %q, want a clean default lease", l.State)
	}
	if got, want := l.ExpiresAt(), testNow.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("default lease expiry = %v, want %v", got, want)
	}
}

// Given a network that answers 404 (already gone), the pass counts it as
// removed: no skip, no failure, the project is fully removed.
func TestPassTreatsMissingNetworkAsGone(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Refuse("network", "n1", 404)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if len(rep.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want none: a 404 is already gone, not a refusal", rep.Skipped)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", rep.Failed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared")
	}
}

// Given a project that fails on the network and has an in-use volume, the
// pass reports both: the failure as the failed object, the refusal as a
// skip. The lease is deleted whatever the outcome.
func TestPassReportsSkipsAlongsideFailure(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Refuse("network", "n1", 500)
	h.srv.Refuse("volume", "myapp_data", 409)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")

	rep := h.pass(t)
	if len(rep.Failed) != 1 || rep.Failed[0] != "myapp_default" {
		t.Fatalf("Failed = %v, want the failed network", rep.Failed)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "myapp_data" {
		t.Fatalf("Skipped = %v, want the in-use volume even though the project failed", rep.Skipped)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the in-use volume was removed")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the removal attempt")
	}
}

// Given any pass, the removal never lists containers outside the project:
// every /containers/json request carries the project label filter; the
// daemon's 409 answers classify shared and in-use objects.
func TestPassNeverListsContainersUnfiltered(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	h.pass(t)
	for _, call := range h.srv.Calls() {
		if call.Method == "GET" && strings.HasPrefix(call.Path, "/v1.47/containers/json") {
			// The label filter travels in the filters query parameter;
			// an unfiltered listing has
			// no filters parameter at all.
			if !strings.Contains(call.Query.Get("filters"), "label") {
				t.Fatalf("unfiltered container listing during the pass: %s %s", call.Method, call.Path)
			}
		}
	}
}

func TestEveryActionIsOneJSONLine(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	h.stopAndAge(t, "myapp")
	h.pass(t)
	lines := h.logLines()
	if len(lines) != 7 { // expiry pass: 2 stops; removal pass: 2 removes + network + volume + image (no stop before remove)
		t.Fatalf("log has %d lines, want 7: %v", len(lines), h.log.String())
	}
	for i, line := range lines {
		for _, key := range []string{"time", "project", "action", "id", "name", "result"} {
			if _, ok := line[key]; !ok {
				t.Fatalf("log line %d (%v) lacks key %q", i, line, key)
			}
		}
		if line["project"] != "myapp" {
			t.Fatalf("log line %d project = %v, want myapp", i, line["project"])
		}
		if line["result"] != "ok" {
			t.Fatalf("log line %d result = %v, want ok", i, line["result"])
		}
	}
}

// A stopped project due for removal is extended with a short TTL while the
// pass is busy stopping an earlier project; by the time the removal takes
// its lock, the new claim has expired again. The removal must not run on a
// lease that went live (state "") since the snapshot: the project never
// had its stopped period.
func TestPassExtendThatExpiresDuringPassMustNotRemove(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "o1", "other-web", "other")
	h.srv.StopContainer("o1")
	h.srv.Add("volume", "ov", "other_data", "other")
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // other: stopped
	h.ageStopped(t)
	fullProject(h.srv) // myapp: sorted before other, expired, stopped in the same pass
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	var mu sync.Mutex
	clock := testNow
	h.cfg.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method == "POST" && strings.HasSuffix(path, "/containers/c1/stop") {
			once.Do(func() {
				// dev-lease extend other 1m (the client clears the stopped mark)
				h.extend("other", time.Minute, testNow)
				// the stops of myapp's containers take two minutes
				mu.Lock()
				clock = testNow.Add(2 * time.Minute)
				mu.Unlock()
			})
		}
	})
	h.pass(t)
	if !h.srv.Has("volume", "other_data") {
		t.Fatalf("other's volume was removed although its lease went live during the pass and was never stopped again; log:\n%s", h.log.String())
	}
}

// The renewal guard's control: the removal aborts when the renewal is still
// live at removal time.
func TestPassControlLiveRenewalAborts(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "o1", "other-web", "other")
	h.srv.Add("volume", "ov", "other_data", "other")
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	h.ageStopped(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method == "POST" && strings.HasSuffix(path, "/containers/c1/stop") {
			once.Do(func() { h.extend("other", time.Hour, testNow) })
		}
	})
	h.pass(t)
	if !h.srv.Has("volume", "other_data") {
		t.Fatal("control: a live renewal did not abort the removal")
	}
}

// A stopped project due for removal is started by the developer while the
// pass is busy with an earlier project (after the revive check, before the
// removal lists the containers). The removal must not stop and delete a
// stack that is running again.
func TestPassStackStartedDuringPassMustNotBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "o1", "other-web", "other")
	h.srv.Add("volume", "ov", "other_data", "other")
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // other: stopped (o1 stopped by the pass)
	h.ageStopped(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method == "POST" && strings.HasSuffix(path, "/containers/c1/stop") {
			once.Do(func() { h.srv.StartContainer("o1") }) // docker compose -p other up -d
		}
	})
	h.pass(t)
	if !h.srv.Has("container", "o1") || !h.srv.Has("volume", "other_data") {
		t.Fatalf("the stack the developer just started was removed; log:\n%s", h.log.String())
	}
	if l := h.leases()["other"]; l.State == lease.StateRemoving {
		t.Fatal("removing mark left behind")
	}
}

// Removal exactly one second before StoppedAt + RemoveAfter must not run.
func TestPassRemovalNotOneSecondEarly(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.StoppedAt = testNow.Add(-h.cfg.RemoveAfter).Add(time.Second)
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rep := h.pass(t); len(rep.Removed) != 0 {
		t.Fatalf("removed one second before the deadline: %+v", rep)
	}
}

// After a 30-day machine sleep the expired lease is stopped, not removed, in
// the first pass; the removal waits a full RemoveAfter from the wake-up.
func TestPassLongSleepStopsFirst(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Hour, testNow.Add(-30*24*time.Hour))
	h.pass(t)
	for _, c := range h.srv.Calls() {
		if c.Method == "DELETE" {
			t.Fatalf("first pass after the sleep deleted %s", c.Path)
		}
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped || !l.StoppedAt.Equal(testNow) {
		t.Fatalf("lease = %+v", l)
	}
	h.cfg.Now = func() time.Time { return testNow.Add(h.cfg.RemoveAfter - time.Minute) }
	if rep := h.pass(t); len(rep.Removed) != 0 {
		t.Fatalf("removed before RemoveAfter from the wake-up: %+v", rep)
	}
}

// Clock jumps back a day after the stop: nothing is removed early.
func TestPassClockBackDelaysRemoval(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	h.cfg.Now = func() time.Time { return testNow.Add(-24 * time.Hour).Add(h.cfg.RemoveAfter) }
	if rep := h.pass(t); len(rep.Removed) != 0 {
		t.Fatalf("removed although the clock went back: %+v", rep)
	}
}

// A stopped lease whose project has nothing left on the daemon is deleted
// at the pass, long before the removal deadline: there is nothing to
// remove, so nothing keeps serve from going idle.
func TestPassDropsStoppedLeaseWithoutDaemonObjects(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // myapp: stopped
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("state = %q, want stopped", l.State)
	}
	// The developer removes everything outside dev-lease (compose down
	// with volumes and images): nothing is left to remove.
	cli := docker.New(h.srv.Socket)
	ctx := context.Background()
	for _, id := range []string{"c1", "c2"} {
		if err := cli.RemoveContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct {
		call func(context.Context, string) error
		id   string
	}{
		{cli.RemoveNetwork, "n1"},
		{cli.RemoveVolume, "myapp_data"},
		{cli.RemoveImage, "sha256:i1"},
	} {
		if err := r.call(ctx, r.id); err != nil {
			t.Fatal(err)
		}
	}
	h.pass(t)
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("stopped lease with nothing left on the daemon survived the pass")
	}
}

// A kept service survives the stop and the removal, and the network,
// volume and image it uses are not removed either. The running kept
// container does not revive the stopped project and does not count for
// the removal's running re-check.
func TestPassKeptContainerSurvivesStopAndRemoval(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddLabels("image", "sha256:i1", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	}, "sha256:i1", []string{"myapp_default"}, []string{"myapp_data"})
	srv.AddContainerWithImage("c2", "db", "myapp", "sha256:i1")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))

	rep := h.pass(t)
	if len(rep.Failed) != 0 {
		t.Fatalf("stop pass failed: %v", rep.Failed)
	}
	if !srv.Has("container", "c1") || srv.IsStopped("container", "c1") {
		t.Fatal("kept container stopped at the stop pass")
	}
	if !srv.IsStopped("container", "c2") {
		t.Fatal("db still running after the stop pass")
	}
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want stopped (a running kept container must not revive)", l)
	}

	h.ageStopped(t)
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if !srv.Has("container", "c1") || srv.IsStopped("container", "c1") {
		t.Fatal("kept container removed or stopped at the removal")
	}
	if srv.Has("container", "c2") {
		t.Fatal("db not removed")
	}
	if !srv.Has("network", "n1") {
		t.Fatal("network in use by the kept container was removed")
	}
	if !srv.Has("volume", "myapp_data") {
		t.Fatal("volume mounted by the kept container was removed")
	}
	if !srv.Has("image", "sha256:i1") {
		t.Fatal("image the kept container runs from was removed")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease not deleted")
	}
}

// A project whose only objects are a kept service and what it uses is not
// present: no default lease is granted and nothing is managed.
func TestPassKeptOnlyProjectNotPresent(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddLabels("image", "sha256:i1", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	}, "sha256:i1", []string{"myapp_default"}, []string{"myapp_data"})
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")

	rep := h.pass(t)
	if len(rep.Granted) != 0 || len(rep.Managed) != 0 {
		t.Fatalf("pass = %+v, want nothing managed or granted for a kept-only project", rep)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("kept-only project got a lease")
	}
}

// A kept volume survives the removal of the rest of the project.
func TestPassKeptVolumeSurvivesRemoval(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	fullProject(srv)
	srv.AddLabels("volume", "v1", "myapp_data", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if !srv.Has("volume", "myapp_data") {
		t.Fatal("kept volume was removed")
	}
	if srv.Has("container", "c1") || srv.Has("network", "n1") {
		t.Fatal("non-kept objects survived the removal")
	}
}

// A stopped lease whose project has only kept objects is dropped at the
// pass: kept objects do not make the project present, so there is nothing
// to remove and nothing to wait for.
func TestPassKeptOnlyStoppedLeaseDropped(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddLabels("image", "sha256:i1", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	}, "sha256:i1", []string{"myapp_default"}, []string{"myapp_data"})
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want stopped after the expiry pass", l)
	}

	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp] on the very next pass", rep.Removed)
	}
	if !srv.Has("container", "c1") {
		t.Fatal("kept container was removed with the lease")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("stopped lease for a kept-only project not dropped")
	}
}

// A dev-lease.remove-after=0 label means never remove: the stop records 0
// in the stopped lease and no removal runs.
func TestPassRemoveAfterLabelNeverRemoves(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	fullProject(srv)
	srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("RemoveAfter = %v, want a stored 0 from the label", l.RemoveAfter)
	}

	h.ageStopped(t)
	stopped := h.leases()["myapp"]
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want nothing with remove-after 0", rep.Removed)
	}
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want it kept stopped", l)
	}
	// The re-check under the lock refuses the removal without touching
	// the stored lease: the stopped time must survive.
	if !l.StoppedAt.Equal(stopped.StoppedAt) {
		t.Fatalf("StoppedAt = %v, want %v untouched by the refused removal", l.StoppedAt, stopped.StoppedAt)
	}
	if !srv.Has("container", "c1") || !srv.Has("container", "c2") {
		t.Fatal("project objects removed although remove-after is 0")
	}
}

// Values that the days form cannot carry are refused like any other
// invalid value: counted as 0 (never) and logged. Without the bounds a
// days value that overflows time.Duration would wrap to a small positive
// delay and the project would be removed early.
func TestPassRemoveAfterLabelBounds(t *testing.T) {
	for _, tc := range []string{"213504d", "-1h", "+7d"} {
		t.Run(tc, func(t *testing.T) {
			h := newHarness(t)
			srv := h.srv
			fullProject(srv)
			srv.AddLabels("container", "c1", "web", map[string]string{
				docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: tc,
			})

			h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
			h.pass(t)
			if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 0 {
				t.Fatalf("RemoveAfter = %v, want a stored 0 for %q", l.RemoveAfter, tc)
			}
			if got := strings.Count(h.log.String(), tc); got != 1 {
				t.Fatalf("log lines with %q = %d, want 1\n%s", tc, got, h.log)
			}
		})
	}
}

// An invalid value next to a valid one counts as 0, and 0 wins over the
// valid value, so the project is never removed.
func TestPassRemoveAfterLabelInvalidNextToValid(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "banana",
	})
	h.srv.AddLabels("container", "c2", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "7d",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("RemoveAfter = %v, want a stored 0 (invalid counts as 0 and wins)", l.RemoveAfter)
	}

	h.ageStopped(t)
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want nothing with an invalid label next to a valid one", rep.Removed)
	}
}

// A volume labelled dev-lease.keep as the project's only object: the
// project is not present (kept objects never count as presence), so no
// lease is granted, and the volume is never removed.
func TestPassKeptVolumeAloneIsNotPresent(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("volume", "v1", "myapp_data", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	})
	rep := h.pass(t)
	if len(rep.Managed) != 0 || len(rep.Granted) != 0 {
		t.Fatalf("pass = %+v, want no managed project for a kept volume alone", rep)
	}
	if l := h.leases()["myapp"]; l.Project != "" {
		t.Fatalf("lease = %+v, want none for a project that is only a kept volume", l)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the kept volume was removed")
	}
}

// Several containers with different labels: the longest value wins.
func TestPassRemoveAfterLabelLongestWins(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "1h",
	})
	h.srv.AddLabels("container", "c2", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "7d",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.RecordedDelay(0) != 7*24*time.Hour {
		t.Fatalf("RemoveAfter = %v, want 7d (the longest label)", l.RemoveAfter)
	}
}

// A 0 wins over any value, whatever the other containers say.
func TestPassRemoveAfterLabelZeroWins(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	h.srv.AddLabels("container", "c2", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "7d",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("RemoveAfter = %v, want a stored 0 (a 0 label wins)", l.RemoveAfter)
	}
}

// An invalid value counts as 0 (never remove) and is logged once per
// pass.
func TestPassRemoveAfterLabelInvalidNeverRemoves(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	fullProject(srv)
	srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "banana",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("RemoveAfter = %v, want a stored 0 for an invalid label", l.RemoveAfter)
	}
	if got := strings.Count(h.log.String(), "banana"); got != 1 {
		t.Fatalf("log lines with the invalid value = %d, want 1\n%s", got, h.log)
	}

	h.ageStopped(t)
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v, want nothing with an invalid label", rep.Removed)
	}
}

// A smaller explicit label than serve's setting removes sooner, at the
// label's deadline.
func TestPassRemoveAfterLabelShorterThanServe(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "2h",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	if l := h.leases()["myapp"]; l.RecordedDelay(0) != 2*time.Hour {
		t.Fatalf("RemoveAfter = %v, want 2h", l.RecordedDelay(0))
	}

	// One hour later: within the label's deadline, nothing is removed.
	h.cfg.Now = func() time.Time { return testNow.Add(time.Hour) }
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v 1h in, want nothing within the label's 2h", rep.Removed)
	}
	// Past the label's deadline: removed, although serve's 168h is far
	// off.
	h.cfg.Now = func() time.Time { return testNow.Add(2*time.Hour + time.Minute) }
	rep = h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp] past the label's deadline", rep.Removed)
	}
}

// A keep value other than true is ignored: the container is stopped and
// removed like any other, and the value is logged once per pass.
func TestPassKeepLabelBadValueIgnored(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	fullProject(srv)
	srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "yes",
	})

	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.stopAndAge(t, "myapp")
	rep := h.pass(t)
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if srv.Has("container", "c1") {
		t.Fatal("container with keep=yes was kept")
	}
	if got := strings.Count(h.log.String(), "yes"); got < 1 {
		t.Fatalf("bad keep value not logged\n%s", h.log)
	}
}

// A lease renewed after the pass decided to remove it must still refuse
// the removal: the decision is re-checked under the store lock. While
// aaa's removal runs, bbb is re-stopped; its deadline moves into the
// future and the re-check refuses it.
func TestRemovalRecheckRefusesRenewedLease(t *testing.T) {
	h := newHarness(t)
	h.cfg.Patterns = append(h.cfg.Patterns, "aaa", "bbb")
	for _, p := range []string{"aaa", "bbb"} {
		h.srv.Add("image", "sha256:"+p, p+"-web:latest", p)
		h.srv.AddContainerWithImage(p+"-c1", "web", p, "sha256:"+p)
		h.extend(p, time.Minute, testNow.Add(-2*time.Minute))
	}
	h.pass(t)       // stops both
	h.ageStopped(t) // backdates both past RemoveAfter: the next pass removes them

	// While aaa's container delete runs, re-stop bbb: the renewal happens
	// after the pass collected bbb as due but before its removal.
	h.srv.OnRequest(func(method, path string) {
		if method != "DELETE" || !strings.Contains(path, "aaa-c1") {
			return
		}
		if err := h.st.Update(func(m map[string]lease.Lease) error {
			l := m["bbb"]
			l.StoppedAt = testNow
			m["bbb"] = l
			return nil
		}); err != nil {
			panic(err)
		}
	})

	h.pass(t)
	if h.srv.Count("container") != 1 || !h.srv.Has("container", "bbb-c1") {
		t.Fatalf("bbb was removed despite the renewal: %+v", h.srv.Calls())
	}
	if l := h.leases()["bbb"]; l.State != lease.StateStopped {
		t.Fatalf("bbb lease = %+v, want stopped with the new deadline", l)
	}
	if _, ok := h.leases()["aaa"]; ok {
		t.Fatal("aaa's lease was not cleared")
	}
}
