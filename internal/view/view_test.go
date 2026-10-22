package view

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
	"slices"
)

var viewNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

type vh struct {
	srv *fakedocker.Server
	st  lease.Store
	cfg reaper.Config
}

func newVH(t *testing.T) *vh {
	t.Helper()
	return &vh{
		srv: fakedocker.New(t),
		st:  lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")},
		cfg: reaper.Config{
			Patterns:    []string{"myapp", "other", "third", "never-rm", "rm", "fresh", "aa", "bb", "nn-1", "nn-2", "nn-3", "nn-4", "busy", "gone", "zz"},
			Own:         "dev-lease",
			DefaultTTL:  2 * time.Hour,
			RemoveAfter: 168 * time.Hour,
		},
	}
}

func (h *vh) put(t *testing.T, l lease.Lease) {
	t.Helper()
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m[l.Project] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func live(name string, expires time.Time) lease.Lease {
	return lease.Lease{Project: name, Holders: []lease.Holder{{
		Name: "ci", Expires: expires, ExtendedAt: expires.Add(-2 * time.Hour), TTL: 2 * time.Hour,
	}}}
}

func stopped(name string, at time.Time, after *time.Duration, from string) lease.Lease {
	return lease.Lease{Project: name, State: lease.StateStopped, StoppedAt: at, RemoveAfter: after, RemoveAfterFrom: from}
}

func (h *vh) cli() *docker.Client { return docker.New(h.srv.Socket) }

func TestListOnlyManagedSortedByNext(t *testing.T) {
	h := newVH(t)
	for _, p := range []string{"myapp", "other", "third", "never-rm", "rm", "stranger", "dev-lease"} {
		h.srv.Add("container", p+"-c1", p+"-web", p)
	}
	h.put(t, live("myapp", viewNow.Add(3*time.Hour)))
	h.put(t, live("other", viewNow.Add(time.Hour)))
	h.put(t, stopped("third", viewNow.Add(-30*time.Minute), nil, "serve"))
	zero := time.Duration(0)
	h.put(t, stopped("never-rm", viewNow.Add(-time.Hour), &zero, "label"))
	h.put(t, lease.Lease{Project: "rm", State: lease.StateRemoving})

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range stacks {
		got = append(got, s.Name)
	}
	want := []string{"rm", "other", "myapp", "third", "never-rm"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
	byName := map[string]Stack{}
	for _, s := range stacks {
		byName[s.Name] = s
	}
	rm := byName["rm"]
	if rm.State != "removing" || rm.Next != "removing now" || !rm.Soon {
		t.Errorf("rm = %+v, want removing now, soon", rm)
	}
	other := byName["other"]
	if other.State != "live" || other.Next != "stops in 1h" || !other.NextAt.Equal(viewNow.Add(time.Hour)) || !other.Soon {
		t.Errorf("other = %+v, want live, stops in 1h, soon", other)
	}
	if len(other.Holders) != 1 || other.Holders[0].Name != "ci" || !other.Holders[0].Until.Equal(viewNow.Add(time.Hour)) {
		t.Errorf("other.Holders = %+v, want the ci claim", other.Holders)
	}
	myapp := byName["myapp"]
	if myapp.State != "live" || myapp.Next != "stops in 3h" || myapp.Soon {
		t.Errorf("myapp = %+v, want live, stops in 3h, not soon", myapp)
	}
	third := byName["third"]
	if third.State != "stopped" || third.Next != "removed in 6d23h" || third.Soon {
		t.Errorf("third = %+v, want stopped, removed in 167h30m, not soon", third)
	}
	never := byName["never-rm"]
	if never.State != "stopped" || never.Next != "never removed" || !never.NeverRemove || !never.NextAt.IsZero() || never.Soon {
		t.Errorf("never-rm = %+v, want stopped, never removed, not soon", never)
	}
	if len(stacks) != 5 {
		t.Fatalf("List returned %d stacks, want 5 (the unmanaged one and dev-lease's own must be absent): %v", len(stacks), got)
	}
}

func TestListNextTexts(t *testing.T) {
	h := newVH(t)
	for _, p := range []string{"myapp", "other", "third", "never-rm", "rm", "fresh"} {
		h.srv.Add("container", p+"-c1", p+"-web", p)
	}
	h.put(t, live("myapp", viewNow.Add(98*time.Minute)))              // stops in 1h38m
	h.put(t, stopped("other", viewNow.Add(-time.Hour), nil, "serve")) // removed in 167h
	label168 := 168 * time.Hour
	h.put(t, stopped("third", viewNow.Add(-200*time.Hour), &label168, "label")) // overdue
	zero := time.Duration(0)
	h.put(t, stopped("never-rm", viewNow.Add(-time.Hour), &zero, "label"))
	h.put(t, lease.Lease{Project: "rm", State: lease.StateRemoving})
	// fresh has no lease at all.

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	next := map[string]Stack{}
	for _, s := range stacks {
		next[s.Name] = s
	}
	for name, want := range map[string]struct{ text, state string }{
		"myapp":    {"stops in 1h38m", "live"},
		"other":    {"removed in 6d23h", "stopped"},
		"third":    {"removal overdue", "stopped"},
		"never-rm": {"never removed", "stopped"},
		"rm":       {"removing now", "removing"},
		"fresh":    {"stops in 2h", "new"},
	} {
		s := next[name]
		if s.Next != want.text {
			t.Errorf("%s.Next = %q, want %q", name, s.Next, want.text)
		}
		if s.State != want.state {
			t.Errorf("%s.State = %q, want %q", name, s.State, want.state)
		}
	}
	if !next["third"].Soon {
		t.Error("an overdue removal is soon")
	}
	if !next["myapp"].Soon {
		t.Error("a stop within 2h is soon")
	}
	if !next["fresh"].NextAt.Equal(viewNow.Add(2 * time.Hour)) {
		t.Errorf("fresh.NextAt = %v, want %v (the default lease)", next["fresh"].NextAt, viewNow.Add(2*time.Hour))
	}
}

func TestListDiskFromVolumeSizes(t *testing.T) {
	h := newVH(t)
	h.srv.Add("container", "c1", "myapp-web", "myapp")
	h.srv.Add("volume", "v1", "myapp_data", "myapp")
	h.srv.Add("volume", "v2", "myapp_cache", "myapp")
	h.srv.SetVolumeSize("myapp_data", 1<<30)
	h.srv.SetVolumeSize("myapp_cache", 2<<30)
	h.srv.Add("container", "t1", "third-web", "third")
	h.srv.Add("volume", "v3", "third_data", "third") // no size: unknown

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	disk := map[string]int64{}
	for _, s := range stacks {
		disk[s.Name] = s.DiskBytes
	}
	if disk["myapp"] != 3<<30 {
		t.Errorf("myapp.DiskBytes = %d, want %d", disk["myapp"], 3<<30)
	}
	if disk["third"] != -1 {
		t.Errorf("third.DiskBytes = %d, want -1 (a volume with an unknown size)", disk["third"])
	}
}

func TestDetailsContainersVolumesImages(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.AddLabels("image", "sha256:i1", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddLabels("image", "sha256:i2", "myapp-db:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	})
	srv.AddLabels("image", "sha256:i3", "registry.example/team/base:1", nil)
	srv.AddLabels("image", "sha256:i4", "myapp-worker:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "worker",
	})
	srv.AddUntaggedImage("sha256:i5", "myapp")
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	srv.SetMemory("c1", 1000, 200)
	// The db container runs from a pulled base image that carries no
	// project label: the listing does not see it, the pulled loop does.
	srv.AddContainerWithUses("c2", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:i3", nil, nil)
	srv.SetContainerImageRef("c2", "registry.example/team/base:1")
	srv.StopContainer("c2")
	srv.AddContainerWithUses("c3", "worker", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "worker", docker.KeepLabel: "true",
	}, "sha256:i4", nil, nil)
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	srv.SetVolumeSize("myapp_data", 500)
	srv.AddLabels("volume", "myapp_secrets", "myapp_secrets", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	})
	srv.SetVolumeSize("myapp_secrets", 100)

	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Containers) != 3 {
		t.Fatalf("containers = %+v, want 3", d.Containers)
	}
	conts := map[string]Container{}
	for _, c := range d.Containers {
		conts[c.Name] = c
	}
	c1 := conts["web"]
	if c1.Service != "web" || c1.Name != "web" || c1.State != "running" || c1.Image != "sha256:i1" || c1.MemoryBytes != 800 || c1.Kept {
		t.Errorf("web container = %+v, want running with 800 bytes, not kept", c1)
	}
	c2 := conts["db"]
	if c2.Service != "db" || c2.State != "exited" || c2.Image != "registry.example/team/base:1" || c2.MemoryBytes != -1 || c2.Kept {
		t.Errorf("db container = %+v, want exited on the pulled image, unknown memory, not kept", c2)
	}
	c3 := conts["worker"]
	if !c3.Kept || c3.Service != "worker" {
		t.Errorf("worker container = %+v, want kept", c3)
	}
	if len(d.Volumes) != 2 {
		t.Fatalf("volumes = %+v, want 2", d.Volumes)
	}
	vols := map[string]Volume{}
	for _, v := range d.Volumes {
		vols[v.Name] = v
	}
	v1 := vols["myapp_data"]
	if v1.Bytes != 500 || v1.Kept {
		t.Errorf("myapp_data = %+v, want 500 bytes, not kept", v1)
	}
	v2 := vols["myapp_secrets"]
	if v2.Bytes != 100 || !v2.Kept {
		t.Errorf("myapp_secrets = %+v, want 100 bytes, kept", v2)
	}
	if !sameStrings(d.RemovedImages, []string{"myapp-web:latest", "myapp-db:latest"}) {
		t.Errorf("RemovedImages = %v, want the used image and the tag-matched one", d.RemovedImages)
	}
	if !sameStrings(d.KeptImages, []string{"registry.example/team/base:1", "myapp-worker:latest", "sha256:i5"}) {
		t.Errorf("KeptImages = %v, want the pulled, kept-container and untagged images", d.KeptImages)
	}
}

// sameStrings reports whether the two slices hold the same strings in any
// order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}

func TestDetailsLogsOnlyForThisProject(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	srv.AddContainerWithUses("o1", "web", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	line := "2026-10-20T11:59:00.000000000Z hello from myapp\n"
	raw := make([]byte, 8+len(line))
	raw[0] = 1 // stdout
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(line)))
	copy(raw[8:], line)
	srv.SetLogs("c1", raw)

	// A service of another project is not addressable: no log call.
	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "web", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.LogService != "web" || len(d.Logs) != 1 || d.Logs[0].Text != "hello from myapp" {
		t.Fatalf("logs = %+v, want the myapp line", d.Logs)
	}
	var logCalls int
	for _, call := range srv.Calls() {
		if strings.Contains(call.Path, "/logs") {
			logCalls++
			if !strings.Contains(call.Path, "/c1/") {
				t.Errorf("log call went to %s, want myapp's c1", call.Path)
			}
		}
	}
	if logCalls != 1 {
		t.Errorf("log calls = %d, want exactly one", logCalls)
	}

	// An unknown service of the same project: no call at all.
	d, err = Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "db", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.LogService != "" || d.Logs != nil {
		t.Fatalf("LogService = %q, Logs = %v, want none for a missing service", d.LogService, d.Logs)
	}
}

func TestDetailsPathAndStartCommand(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel:                       "myapp",
		"com.docker.compose.project.working_dir":  "/srv/app",
		"com.docker.compose.project.config_files": "/srv/app/compose.yml,/srv/app/override.yml",
	}, "sha256:i1", nil, nil)

	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != "/srv/app" {
		t.Errorf("Path = %q, want /srv/app", d.Path)
	}
	if len(d.ConfigFiles) != 2 || d.ConfigFiles[0] != "/srv/app/compose.yml" || d.ConfigFiles[1] != "/srv/app/override.yml" {
		t.Errorf("ConfigFiles = %v, want the two compose files in order", d.ConfigFiles)
	}
	if d.StartCommand != "cd /srv/app && docker compose -p myapp -f compose.yml -f override.yml up -d" {
		t.Errorf("StartCommand = %q", d.StartCommand)
	}
}

func TestDetailsRefusesUnmanaged(t *testing.T) {
	h := newVH(t)
	h.srv.Add("container", "s1", "stranger-web", "stranger")
	_, err := Details(context.Background(), h.cli(), h.st, h.cfg, "stranger", "", 10, viewNow)
	if !errors.Is(err, ErrNotManaged) {
		t.Fatalf("Details error = %v, want ErrNotManaged", err)
	}
}

// A refused /system/df must not fail the listing: every volume's size is
// unknown (-1), and once the daemon answers again the next listing sees
// the real sizes (the failure is not cached).
func TestListVolumeSizeFailureShowsUnknown(t *testing.T) {
	h := newVH(t)
	h.srv.Add("container", "myapp-c1", "myapp-web", "myapp")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.srv.SetVolumeSize("myapp_data", 500)
	h.srv.FailSystemDf(500)

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatalf("List with a refused system/df = %v, want the listing", err)
	}
	if len(stacks) != 1 || stacks[0].DiskBytes != -1 {
		t.Fatalf("stacks = %+v, want myapp with unknown disk", stacks)
	}

	h.srv.UnfailSystemDf()
	stacks, err = List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 || stacks[0].DiskBytes != 500 {
		t.Fatalf("stacks = %+v, want myapp with 500 bytes after the daemon recovered", stacks)
	}
}

func TestStartCommand(t *testing.T) {
	cases := []struct {
		name    string
		project string
		path    string
		files   []string
		want    string
	}{
		{"plain path and default file", "myapp", "/srv/app", []string{"/srv/app/compose.yaml"},
			"cd /srv/app && docker compose -p myapp up -d"},
		{"docker-compose.yml defaults too", "myapp", "/srv/app", []string{"/srv/app/docker-compose.yml"},
			"cd /srv/app && docker compose -p myapp up -d"},
		{"docker-compose.yaml defaults too", "myapp", "/srv/app", []string{"/srv/app/docker-compose.yaml"},
			"cd /srv/app && docker compose -p myapp up -d"},
		{"path with a space", "myapp", "/srv/app dir", []string{"/srv/app dir/compose.yml"},
			"cd '/srv/app dir' && docker compose -p myapp up -d"},
		{"path with a quote", "myapp", "/srv/it's", []string{"/srv/it's/compose.yaml"},
			"cd '/srv/it'\\''s' && docker compose -p myapp up -d"},
		{"two config files", "myapp", "/srv/app", []string{"/srv/app/compose.yaml", "/srv/app/override.yml"},
			"cd /srv/app && docker compose -p myapp -f compose.yaml -f override.yml up -d"},
		{"one non-default file name", "myapp", "/srv/app", []string{"/srv/app/custom.yml"},
			"cd /srv/app && docker compose -p myapp -f custom.yml up -d"},
		{"no path", "myapp", "", nil, "docker compose -p myapp up -d"},
		{"a project name that needs no quoting", "myapp_a-1", "", nil,
			"docker compose -p myapp_a-1 up -d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := startCommand(tc.project, tc.path, tc.files); got != tc.want {
				t.Fatalf("startCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

// The project name is quoted with the same rule as the path: a hostile
// name cannot break out of -p when the command is pasted into a shell.
func TestStartCommandQuotesProjectName(t *testing.T) {
	got := startCommand("myapp;touch PWNED;#", "", nil)
	if want := "docker compose -p 'myapp;touch PWNED;#' up -d"; got != want {
		t.Fatalf("startCommand = %q, want %q", got, want)
	}
}

func TestFmtDurUnchanged(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "<1m"},
		{0, "<1m"},
		{59 * time.Second, "<1m"},
		{time.Minute, "1m"},
		{90 * time.Second, "1m"},
		{23 * time.Minute, "23m"},
		{time.Hour, "1h"},
		{83 * time.Minute, "1h23m"},
		{47*time.Hour + 59*time.Minute, "47h59m"},
		{48 * time.Hour, "2d"},
		{5*24*time.Hour + 13*time.Hour + 30*time.Minute, "5d13h"},
		{6*24*time.Hour + 22*time.Hour, "6d22h"},
	} {
		if got := FmtDur(tc.d); got != tc.want {
			t.Errorf("FmtDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestListOrderStable pins the listing's sort: a removal in flight comes
// first (a lease-only removing stack is listed too), never-remove stacks
// come last, and ties are broken by name; the order never changes between
// calls.
func TestListOrderStable(t *testing.T) {
	h := newVH(t)
	for _, p := range []string{"aa", "bb"} {
		h.srv.Add("container", p+"-c1", p+"-web", p)
	}
	zero := time.Duration(0)
	for _, p := range []string{"nn-1", "nn-2", "nn-3", "nn-4"} {
		h.srv.Add("container", p+"-c1", p+"-web", p)
		h.put(t, stopped(p, viewNow.Add(-time.Hour), &zero, "label"))
	}
	// One removal running with the objects already gone (the lease mark
	// is all that is left) and one with the objects still there: both
	// come first, ahead of an overdue stopped stack whose removal
	// deadline lies in the past (NextAt alone would sort it first).
	h.put(t, lease.Lease{Project: "gone", State: lease.StateRemoving})
	h.srv.Add("container", "busy-c1", "busy-web", "busy")
	h.put(t, lease.Lease{Project: "busy", State: lease.StateRemoving})
	h.srv.Add("container", "zz-c1", "zz-web", "zz")
	h.put(t, stopped("zz", viewNow.Add(-170*time.Hour), nil, "label"))

	want := "busy,gone,zz,aa,bb,nn-1,nn-2,nn-3,nn-4"
	for i := 0; i < 20; i++ {
		stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range stacks {
			got = append(got, s.Name)
		}
		if strings.Join(got, ",") != want {
			t.Fatalf("call %d: order = %v, want %s", i, got, want)
		}
	}
}

// A managed name with no daemon objects and no lease record is nowhere:
// the detail page refuses it with ErrNoStack instead of inventing a new
// stack.
func TestDetailsUnknownNameIsErrNoStack(t *testing.T) {
	h := newVH(t)
	_, err := Details(context.Background(), h.cli(), h.st, h.cfg, "fresh", "", 10, viewNow)
	if !errors.Is(err, ErrNoStack) {
		t.Fatalf("Details(fresh) error = %v, want ErrNoStack", err)
	}
}

// A lease with nothing on the daemon yet (an extend before compose up):
// the page shows the lease's state, not a new stack.
func TestDetailsLeaseOnlyStackFromLease(t *testing.T) {
	h := newVH(t)
	h.put(t, live("myapp", viewNow.Add(5*time.Hour)))
	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateLive || d.Next != "stops in 5h" || !d.NextAt.Equal(viewNow.Add(5*time.Hour)) {
		t.Errorf("live lease only: state %q next %q at %v, want live, stops in 5h", d.State, d.Next, d.NextAt)
	}
	if len(d.Holders) != 1 || d.Holders[0].Name != "ci" {
		t.Errorf("holders = %+v, want the ci claim", d.Holders)
	}

	h.put(t, stopped("myapp", viewNow.Add(-time.Hour), nil, "label"))
	d, err = Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateStopped || d.Next != "removed in 6d23h" {
		t.Errorf("stopped lease only: state %q next %q, want stopped, removed in 6d23h", d.State, d.Next)
	}
}

// A lease-only live stack has nothing on the daemon, so it has no
// volumes: its disk use is 0, not unknown.
func TestLeaseOnlyDiskUnknown(t *testing.T) {
	h := newVH(t)
	h.put(t, live("myapp", viewNow.Add(5*time.Hour)))
	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.DiskBytes != 0 || len(d.Volumes) != 0 {
		t.Errorf("lease-only Details: DiskBytes=%d volumes=%d, want 0 and none", d.DiskBytes, len(d.Volumes))
	}
}

// A failed volume size read must not fail the page: each volume shows its
// size as unknown (-1).
func TestDetailsVolumeSizeFailureShowsUnknown(t *testing.T) {
	h := newVH(t)
	h.srv.Add("volume", "v1", "myapp-data", "myapp")
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	h.srv.FailSystemDf(500)
	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Volumes) != 1 || d.Volumes[0].Bytes != -1 {
		t.Fatalf("volumes = %+v, want the single volume with an unknown size", d.Volumes)
	}
}

// A pulled image the project's db container runs from has no project
// label, so the project image listing never sees it: it still appears,
// as kept (design 6a), and the container shows the pulled reference.
func TestDetailsPulledImageKept(t *testing.T) {
	h := newVH(t)
	h.srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)
	h.srv.AddContainerWithUses("c1", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pg", nil, nil)
	h.srv.SetContainerImageRef("c1", "postgres:16-alpine")

	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.RemovedImages) != 0 {
		t.Errorf("RemovedImages = %v, want none", d.RemovedImages)
	}
	if len(d.KeptImages) != 1 || d.KeptImages[0] != "postgres:16-alpine" {
		t.Errorf("KeptImages = %v, want exactly the pulled postgres:16-alpine as staying", d.KeptImages)
	}
	if len(d.Containers) != 1 || d.Containers[0].Image != "postgres:16-alpine" {
		t.Errorf("containers = %+v, want the db container showing the pulled reference", d.Containers)
	}
}

// The dashboard shows sizes in decimal units with one decimal; an unknown
// size (-1, from a failed volume-size read or an unstatable container) is
// the word "unknown".
func TestFmtSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{-1, "unknown"},
		{0, "0 B"},
		{512, "512 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{1200, "1.2 kB"},
		{1000000, "1.0 MB"},
		{340500000, "340.5 MB"},
		{5800000000, "5.8 GB"},
		{2000000000000, "2.0 TB"},
	}
	for _, c := range cases {
		if got := FmtSize(c.n); got != c.want {
			t.Errorf("FmtSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// The removal lists every network of the project except the ones a kept
// container uses: the kept container survives, and a network it is the
// only user of cannot be removed.
func TestDetailNetworksExcludeKeptUses(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", []string{"myapp_default"}, nil)
	srv.AddContainerWithUses("c2", "helper", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "helper", docker.KeepLabel: "true",
	}, "sha256:i2", []string{"myapp_keptnet"}, nil)
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("network", "n2", "myapp_keptnet", "myapp")
	srv.Add("network", "n3", "myapp_shared", "myapp")

	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Networks) != 2 || d.Networks[0] != "myapp_default" || d.Networks[1] != "myapp_shared" {
		t.Fatalf("Networks = %v, want myapp_default and myapp_shared (keptnet is used by the kept container)", d.Networks)
	}
}

// The detail of a stopped stack carries when it stopped, so the dashboard
// can say "stopped 3h ago" without date logic of its own.
func TestDetailStoppedAtExposed(t *testing.T) {
	h := newVH(t)
	h.srv.Add("container", "c1", "third-web", "third")
	at := viewNow.Add(-3 * time.Hour)
	h.put(t, stopped("third", at, nil, "serve"))

	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "third", "", 10, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.StoppedAt.IsZero() || !d.StoppedAt.Equal(at) {
		t.Errorf("StoppedAt = %v, want %v", d.StoppedAt, at)
	}
}

// --- The right removal delay everywhere ---

// A live stack's effective removal delay comes from its containers'
// labels, the same rule the reaper applies at stop time: a 1h label
// wins over serve's 7d, a 0 label means never remove, and no label
// means the serve setting.
func TestListLiveRemoveAfterFromLabels(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.AddLabels("container", "c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "1h",
	})
	srv.AddLabels("container", "c2", "other_web_1", map[string]string{
		docker.ProjectLabel: "other", docker.RemoveAfterLabel: "0",
	})
	srv.Add("container", "c3", "third_web_1", "third")
	exp := viewNow.Add(30 * time.Minute)
	h.put(t, live("myapp", exp))
	h.put(t, live("other", exp))
	h.put(t, live("third", exp))

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Stack{}
	for _, s := range stacks {
		got[s.Name] = s
	}
	if got["myapp"].RemoveAfter != time.Hour || got["myapp"].NeverRemove {
		t.Errorf("myapp: RemoveAfter = %v, NeverRemove = %v; want 1h, false", got["myapp"].RemoveAfter, got["myapp"].NeverRemove)
	}
	if !got["other"].NeverRemove {
		t.Errorf("other: NeverRemove = false; the 0 label means never remove")
	}
	if got["third"].RemoveAfter != 168*time.Hour || got["third"].NeverRemove {
		t.Errorf("third: RemoveAfter = %v, NeverRemove = %v; want serve's 168h, false", got["third"].RemoveAfter, got["third"].NeverRemove)
	}
}

// A new stack (no lease record yet) takes its removal delay from the
// labels the same way; serve's --remove-after 0 is the engine's global
// off switch, so every stack - labelled or not - shows as never removed.
func TestListNewStackRemoveAfterFromLabels(t *testing.T) {
	h := newVH(t)
	h.cfg.RemoveAfter = 0
	srv := h.srv
	srv.AddLabels("container", "c1", "rm_web_1", map[string]string{
		docker.ProjectLabel: "rm", docker.RemoveAfterLabel: "1h",
	})
	srv.AddLabels("container", "c2", "nn-1_web_1", map[string]string{
		docker.ProjectLabel: "nn-1", docker.RemoveAfterLabel: "0",
	})
	srv.Add("container", "c3", "zz_web_1", "zz")

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Stack{}
	for _, s := range stacks {
		got[s.Name] = s
	}
	if got["rm"].RemoveAfter != 0 || !got["rm"].NeverRemove {
		t.Errorf("rm: RemoveAfter = %v, NeverRemove = %v; serve 0 overrides the 1h label", got["rm"].RemoveAfter, got["rm"].NeverRemove)
	}
	if !got["nn-1"].NeverRemove {
		t.Errorf("nn-1: NeverRemove = false; the 0 label means never remove")
	}
	if got["zz"].RemoveAfter != 0 || !got["zz"].NeverRemove {
		t.Errorf("zz: RemoveAfter = %v, NeverRemove = %v; serve 0 means never remove", got["zz"].RemoveAfter, got["zz"].NeverRemove)
	}
}

// The listing carries the age of the longest-running container, so the
// table can say "2 running · up 1h50m".
func TestListUptime(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.Add("container", "c1", "myapp_web_1", "myapp")
	srv.Add("container", "c2", "myapp_db_1", "myapp")
	srv.StopContainer("c2")

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow.Add(time.Hour+50*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var s Stack
	for _, s2 := range stacks {
		if s2.Name == "myapp" {
			s = s2
			break
		}
	}
	if s.Up != time.Hour+50*time.Minute {
		t.Errorf("Up = %v, want 1h50m", s.Up)
	}
}

// --- Size rounding and log control characters ---

// FmtSize rounds the one decimal: 999950 B is 1.0 MB, not "1000.0 kB".
func TestFmtSizeRoundsUp(t *testing.T) {
	if got := FmtSize(999950); got != "1.0 MB" {
		t.Errorf("FmtSize(999950) = %q, want %q", got, "1.0 MB")
	}
	if got := FmtSize(999949); got != "999.9 kB" {
		t.Errorf("FmtSize(999949) = %q, want %q", got, "999.9 kB")
	}
}

// Control characters other than tab are removed from log lines: ANSI
// escape sequences, the bell, carriage returns, C1 controls and DEL;
// tabs survive.
func TestDetailsLogsStripControlChars(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	lines := []struct {
		raw, want string
	}{
		{
			raw:  "hello \x1b[31mred\x1b[0m \a bell\ttab\r",
			want: "hello red  bell\ttab",
		},
		{
			raw:  "c1 \x9d\x9f and del \x7f kept",
			want: "c1  and del  kept",
		},
		{
			raw:  "next\u0085line \u009b31m",
			want: "nextline 31m",
		},
	}
	for _, tc := range lines {
		line := "2026-10-20T12:00:00.000000000Z " + tc.raw + "\n"
		raw := make([]byte, 8+len(line))
		raw[0] = 1 // stdout
		binary.BigEndian.PutUint32(raw[4:8], uint32(len(line)))
		copy(raw[8:], line)
		srv.SetLogs("c1", raw)

		d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "web", 10, viewNow)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Logs) != 1 {
			t.Fatalf("logs = %v, want one line", d.Logs)
		}
		if d.Logs[0].Text != tc.want {
			t.Errorf("log text = %q, want %q", d.Logs[0].Text, tc.want)
		}
	}
}

// A live stack with no containers (compose down, the project present
// through a volume alone) and a lease with nothing on the daemon at all
// both take NeverRemove and their delay from the sweep-recorded label
// delay when one is recorded.
func TestDetailsNeverRemoveFromRecordedLabel(t *testing.T) {
	h := newVH(t)
	zero := time.Duration(0)
	l := live("myapp", viewNow.Add(time.Hour))
	l.LabelRemoveAfter = &zero
	h.put(t, l)

	// The project is present through its volume alone: the containers
	// were dropped by a compose down.
	h.srv.Add("volume", "v1", "myapp_data", "myapp")

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	var s Stack
	for _, s2 := range stacks {
		if s2.Name == "myapp" {
			s = s2
			break
		}
	}
	if s.Name != "myapp" {
		t.Fatal("myapp missing from the listing")
	}
	if !s.NeverRemove || s.RemoveAfter != 0 {
		t.Errorf("myapp = %+v, want NeverRemove with 0 from the recorded label", s)
	}

	// The extend-before-compose-up case: a lease with the recorded label
	// and nothing on the daemon at all.
	h2 := newVH(t)
	l2 := live("other", viewNow.Add(time.Hour))
	l2.LabelRemoveAfter = &zero
	h2.put(t, l2)
	d, err := Details(context.Background(), h2.cli(), h2.st, h2.cfg, "other", "", 0, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if !d.NeverRemove || d.RemoveAfter != 0 {
		t.Errorf("details = %+v, want NeverRemove with 0 from the recorded label", d.Stack)
	}
}

// TestDetailsExposesPlan pins the detail page to the engine's plan: the
// stop and remove dialogs render from Detail.Plan, and each container
// carries its uptime.
func TestDetailsExposesPlan(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	srv.AddContainerWithUses("c-web", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", []string{"myapp_default"}, []string{"app_data"})
	srv.AddContainerWithUses("c-db", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db", docker.KeepLabel: "true",
	}, "sha256:pg16", []string{"myapp_default"}, []string{"db_data"})
	srv.SetContainerImageRef("c-db", "postgres:16-alpine")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "app_data", "myapp")
	srv.Add("volume", "v2", "db_data", "myapp")
	srv.AddLabels("image", "sha256:i1", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})

	d, err := Details(context.Background(), h.cli(), h.st, h.cfg, "myapp", "", 10, viewNow.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := reaper.Plan{
		Stop:       []string{"web"},
		Containers: []string{"web"},
		Networks:   []string{},
		Volumes:    []string{"app_data"},
		Images:     []string{"myapp-web:latest"},
		Kept:       []string{"db", "db_data", "myapp_default"},
	}
	for _, field := range []struct {
		name string
		got  []string
		want []string
	}{
		{"Stop", d.Plan.Stop, want.Stop},
		{"Containers", d.Plan.Containers, want.Containers},
		{"Networks", d.Plan.Networks, want.Networks},
		{"Volumes", d.Plan.Volumes, want.Volumes},
		{"Images", d.Plan.Images, want.Images},
		{"Kept", d.Plan.Kept, want.Kept},
	} {
		if !slices.Equal(field.got, field.want) {
			t.Errorf("Plan.%s = %v, want %v", field.name, field.got, field.want)
		}
	}
	// Up: both containers run since createdAt, fixed at viewNow in the
	// fake daemon, so 90 minutes at the shifted now.
	byService := map[string]Container{}
	for _, c := range d.Containers {
		byService[c.Service] = c
	}
	for _, service := range []string{"web", "db"} {
		if got := byService[service].Up; got != 90*time.Minute {
			t.Errorf("%s container Up = %v, want 1h30m", service, got)
		}
	}
}

// Serve's --remove-after 0 is the engine's global off switch, and it wins
// over the recorded label delay too: a stopped stack whose lease recorded
// 720h still shows as never removed.
func TestListServeZeroOverridesRecordedLabel(t *testing.T) {
	h := newVH(t)
	h.cfg.RemoveAfter = 0
	after := 720 * time.Hour
	l := stopped("myapp", viewNow.Add(-time.Hour), &after, "label")
	l.LabelRemoveAfter = &after
	h.put(t, l)
	h.srv.Add("volume", "v1", "myapp_data", "myapp")

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 {
		t.Fatalf("stacks = %+v, want only myapp", stacks)
	}
	if stacks[0].RemoveAfter != 0 || !stacks[0].NeverRemove {
		t.Errorf("myapp = %+v, want never removed: serve 0 beats the recorded 720h", stacks[0])
	}

	// The lease-only case (extend before compose up) behaves the same.
	h2 := newVH(t)
	h2.cfg.RemoveAfter = 0
	l2 := live("other", viewNow.Add(time.Hour))
	l2.LabelRemoveAfter = &after
	h2.put(t, l2)
	d, err := Details(context.Background(), h2.cli(), h2.st, h2.cfg, "other", "", 0, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.RemoveAfter != 0 || !d.NeverRemove {
		t.Errorf("details = %+v, want never removed: serve 0 beats the recorded 720h", d.Stack)
	}
}

// A live stack's containers win over the record: when the lease still
// carries an older recorded label value, the current container labels are
// what the listing shows.
func TestListLiveUsesContainerLabelsNotRecord(t *testing.T) {
	h := newVH(t)
	srv := h.srv
	// The containers now carry 720h...
	srv.AddLabels("container", "c1", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	})
	// ...but the lease record says 0 from an earlier state.
	zero := time.Duration(0)
	l := live("myapp", viewNow.Add(30*time.Minute))
	l.LabelRemoveAfter = &zero
	h.put(t, l)

	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 {
		t.Fatalf("stacks = %+v, want only myapp", stacks)
	}
	if stacks[0].RemoveAfter != 720*time.Hour || stacks[0].NeverRemove {
		t.Errorf("myapp = %+v, want the containers' 720h: a stale record must not hide the live labels", stacks[0])
	}
}
