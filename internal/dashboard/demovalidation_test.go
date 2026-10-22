package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// The demo data validates itself: a live demo stack's stop dialog shows
// its kept containers, and a stopped demo stack's remove dialog shows
// the kept objects left in place. Without the kept seed data the demo
// would show both dialogs empty.
func TestDemoSeedValidatesKeptObjects(t *testing.T) {
	fake := fakedocker.New(t)
	state := lease.Store{Path: t.TempDir() + "/leases.json"}
	cfg := reaper.Config{
		Patterns:    []string{"myapp*"},
		Own:         "dev-lease",
		DefaultTTL:  2 * time.Hour,
		RemoveAfter: 7 * 24 * time.Hour,
		StopTimeout: 10 * time.Second,
	}
	seedDemo(t, fake, state, cfg)
	runner := &reaper.Runner{Cli: docker.New(fake.Socket), Store: state, Cfg: cfg}
	hnd, err := New(Options{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(hnd)
	t.Cleanup(ts.Close)

	page := body(t, get(t, ts.URL+"/stacks/myapp-main"))
	stop := between(t, page, `<dialog class="dialog" id="stop-dialog"`, `</dialog>`)
	if !strings.Contains(stop, "myapp-main_postgres_1, myapp-main_redis_1") {
		t.Errorf("the demo stop dialog does not list the kept containers:\n%s", stop)
	}
	remove := between(t, page, `<dialog class="dialog" id="remove-dialog"`, `</dialog>`)
	if !strings.Contains(remove, "postgres_data") {
		t.Errorf("the demo remove dialog does not list the kept volume:\n%s", remove)
	}

	page = body(t, get(t, ts.URL+"/stacks/myapp-fix-login"))
	remove = between(t, page, `<dialog class="dialog" id="remove-dialog"`, `</dialog>`)
	if !strings.Contains(remove, "myapp-fix-login_postgres_1") {
		t.Errorf("the stopped demo stack's remove dialog does not list its kept container:\n%s", remove)
	}
}

// The demo seed keeps the demo shapes honest: the kept postgres of the
// stopped demo stack still runs while the rest of the stack stopped.
func TestDemoSeedFixLoginHasRunningKeptPostgres(t *testing.T) {
	fake := fakedocker.New(t)
	state := lease.Store{Path: t.TempDir() + "/leases.json"}
	cfg := reaper.Config{Patterns: []string{"myapp*"}, DefaultTTL: 2 * time.Hour, RemoveAfter: 7 * 24 * time.Hour}
	seedDemo(t, fake, state, cfg)

	conts, err := docker.New(fake.Socket).Containers(t.Context(), "myapp-fix-login")
	if err != nil {
		t.Fatal(err)
	}
	var pg *docker.Object
	for i := range conts {
		if conts[i].Name == "myapp-fix-login_postgres_1" {
			pg = &conts[i]
		}
	}
	if pg == nil {
		t.Fatalf("the demo seed has no myapp-fix-login_postgres_1 container")
	}
	if !docker.Kept(*pg) {
		t.Error("the demo postgres is not kept")
	}
	if !pg.Running {
		t.Error("the demo postgres is not running (the stack is stopped, the kept container runs)")
	}
}

var _ = lease.StateStopped

// The demo's log lines are neutral web-server lines: generic routes
// only, none that could name a real product.
func TestDemoSeedLogLinesNeutral(t *testing.T) {
	fake := fakedocker.New(t)
	state := lease.Store{Path: t.TempDir() + "/leases.json"}
	cfg := reaper.Config{Patterns: []string{"myapp*"}, DefaultTTL: 2 * time.Hour, RemoveAfter: 7 * 24 * time.Hour}
	seedDemo(t, fake, state, cfg)
	runner := &reaper.Runner{Cli: docker.New(fake.Socket), Store: state, Cfg: cfg}
	hnd, err := New(Options{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(hnd)
	t.Cleanup(ts.Close)

	page := body(t, get(t, ts.URL+"/stacks/myapp-main?logs=backend&lines=50"))
	logs := between(t, page, `<pre class="logs"`, `</pre>`)
	for _, route := range []string{"/config", "/proofreading", "/overrides", "/admin"} {
		if strings.Contains(logs, route) {
			t.Errorf("the demo logs name a product route %q:\n%s", route, logs)
		}
	}
	if !strings.Contains(logs, "GET /api/items 200 18ms") {
		t.Errorf("the demo logs lack the neutral api line:\n%s", logs)
	}
	if !strings.Contains(logs, "GET / 200 2ms") {
		t.Errorf("the demo logs lack the neutral root line:\n%s", logs)
	}
}
