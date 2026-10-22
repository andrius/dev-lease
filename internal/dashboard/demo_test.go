package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// TestDemoServer serves the dashboard against internal/fakedocker on
// DEV_LEASE_DEMO_ADDR so a human (or a test browser) can
// check the pages against the mockup. It is skipped unless the address
// is set; it runs for DEV_LEASE_DEMO_SECONDS (default 900).
//
//	go test ./internal/dashboard -run TestDemoServer -count=1
//	DEV_LEASE_DEMO_ADDR=127.0.0.1:8090 DEV_LEASE_DEMO_SECONDS=300 \
//	  go test ./internal/dashboard -run TestDemoServer -count=1
func TestDemoServer(t *testing.T) {
	addr := os.Getenv("DEV_LEASE_DEMO_ADDR")
	if addr == "" {
		t.Skip("set DEV_LEASE_DEMO_ADDR to serve the demo dashboard")
	}
	secs := 900
	if v := os.Getenv("DEV_LEASE_DEMO_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("DEV_LEASE_DEMO_SECONDS = %q, want a positive number of seconds", v)
		}
		secs = n
	}

	fake := fakedocker.New(t)
	state := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	cfg := reaper.Config{
		Patterns:    []string{"myapp*"},
		Own:         "dev-lease",
		DefaultTTL:  2 * time.Hour,
		RemoveAfter: 7 * 24 * time.Hour,
		StopTimeout: 10 * time.Second,
	}
	runner := &reaper.Runner{Cli: docker.New(fake.Socket), Store: state, Cfg: cfg}
	seedDemo(t, fake, state, cfg)

	hnd, err := New(Options{Runner: runner, Hosts: []string{"127.0.0.1", "localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Addr: addr, Handler: hnd, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Printf("demo: listening on http://%s\n", addr)
	// A taken or invalid address surfaces immediately instead of after
	// the whole demo window.
	select {
	case err := <-errc:
		t.Fatalf("demo: serve: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	_ = srv.Close()
}

// seedDemo fills the fake daemon and the state file with seven stacks
// shaped like the mockup's: live with holders, live without, stopped
// and nearly due, stopped and never removed, being removed, and new.
func seedDemo(t *testing.T, fake *fakedocker.Server, state lease.Store, cfg reaper.Config) {
	t.Helper()
	now := time.Now()
	workDir := func(name string) string { return "/home/dev/src/" + name }
	logs := muxLogFrame("2026-10-20T08:41:02.000000000Z INFO  started server on 0.0.0.0:8000\n" +
		"2026-10-20T08:47:19.000000000Z INFO  GET /health 200 3ms\n" +
		"2026-10-20T08:52:40.000000000Z INFO  GET /api/items 200 18ms\n" +
		"2026-10-20T08:53:05.000000000Z INFO  GET / 200 2ms\n")

	// myapp-main: live, two holders, three running containers, volumes,
	// one built image removed with the stack, postgres kept with its image.
	fake.AddContainerWithUses("m-backend", "myapp-main_backend_1", map[string]string{
		docker.ProjectLabel:                      "myapp-main",
		docker.ServiceLabel:                      "backend",
		"com.docker.compose.project.working_dir": workDir("myapp-main"),
	}, "sha256:img-main-backend", nil, nil)
	fake.AddLabels("image", "sha256:img-main-backend", "myapp-main-backend:latest", map[string]string{
		docker.ProjectLabel: "myapp-main",
		docker.ServiceLabel: "backend",
	})
	fake.AddContainerWithUses("m-postgres", "myapp-main_postgres_1", map[string]string{
		docker.ProjectLabel:                      "myapp-main",
		docker.ServiceLabel:                      "postgres",
		"com.docker.compose.project.working_dir": workDir("myapp-main"),
		docker.KeepLabel:                         "true",
	}, "sha256:pg16", nil, []string{"postgres_data"})
	fake.SetContainerImageRef("m-postgres", "postgres:16-alpine")
	fake.AddContainerWithUses("m-redis", "myapp-main_redis_1", map[string]string{
		docker.ProjectLabel:                      "myapp-main",
		docker.ServiceLabel:                      "redis",
		"com.docker.compose.project.working_dir": workDir("myapp-main"),
		docker.KeepLabel:                         "true",
	}, "sha256:r7", nil, []string{"redis_data"})
	fake.SetContainerImageRef("m-redis", "redis:7-alpine")
	fake.Add("volume", "main_pg", "postgres_data", "myapp-main")
	fake.SetVolumeSize("postgres_data", 280_000_000)
	fake.Add("volume", "main_rd", "redis_data", "myapp-main")
	fake.SetVolumeSize("redis_data", 30_000_000)
	fake.SetLogs("m-backend", logs)
	fake.SetMemory("m-backend", 310_000_000, 40_000_000)
	fake.SetMemory("m-postgres", 120_000_000, 20_000_000)
	fake.SetMemory("m-redis", 12_000_000, 2_000_000)
	extendDemo(t, state, "myapp-main", "agent-201", 2*time.Hour, now)
	extendDemo(t, state, "myapp-main", "alice", time.Hour, now)

	// myapp-feat-search: live, one holder, two running containers.
	fake.AddContainerWithUses("s-web", "myapp-feat-search_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp-feat-search",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": workDir("myapp-feat-search"),
	}, "sha256:img-s-web", nil, []string{"search_index"})
	fake.AddLabels("image", "sha256:img-s-web", "myapp-feat-search-web:latest", map[string]string{
		docker.ProjectLabel: "myapp-feat-search",
		docker.ServiceLabel: "web",
	})
	fake.AddContainerWithUses("s-db", "myapp-feat-search_db_1", map[string]string{
		docker.ProjectLabel:                      "myapp-feat-search",
		docker.ServiceLabel:                      "db",
		"com.docker.compose.project.working_dir": workDir("myapp-feat-search"),
	}, "sha256:img-s-db", nil, nil)
	fake.AddLabels("image", "sha256:img-s-db", "myapp-feat-search-db:latest", map[string]string{
		docker.ProjectLabel: "myapp-feat-search",
		docker.ServiceLabel: "db",
	})
	fake.Add("volume", "s_idx", "search_index", "myapp-feat-search")
	fake.SetVolumeSize("search_index", 120_000_000)
	extendDemo(t, state, "myapp-feat-search", "alice", 8*time.Hour, now)

	// myapp-fix-login: stopped 6d22h ago, removed in about 2h, its
	// postgres kept and still running so the dialogs demo kept objects.
	fake.AddContainerWithUses("f-web", "myapp-fix-login_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp-fix-login",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": workDir("myapp-fix-login"),
	}, "sha256:img-f", nil, []string{"fix_data"})
	fake.StopContainer("f-web")
	fake.AddContainerWithUses("f-pg", "myapp-fix-login_postgres_1", map[string]string{
		docker.ProjectLabel:                      "myapp-fix-login",
		docker.ServiceLabel:                      "postgres",
		"com.docker.compose.project.working_dir": workDir("myapp-fix-login"),
		docker.KeepLabel:                         "true",
	}, "sha256:pg16", nil, []string{"fix_pg"})
	fake.SetContainerImageRef("f-pg", "postgres:16-alpine")
	fake.Add("volume", "f_pg", "fix_pg", "myapp-fix-login")
	fake.SetVolumeSize("fix_pg", 64_000_000)
	fake.Add("volume", "f_data", "fix_data", "myapp-fix-login")
	fake.SetVolumeSize("fix_data", 900_000_000)
	seedLease(t, state, "myapp-fix-login", lease.Lease{
		Project: "myapp-fix-login", State: lease.StateStopped,
		StoppedAt: now.Add(-(6*24*time.Hour + 22*time.Hour)),
	})

	// myapp-docs: stopped, never removed by label.
	fake.AddContainerWithUses("d-web", "myapp-docs_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp-docs",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": workDir("myapp-docs"),
		docker.RemoveAfterLabel:                  "0",
	}, "sha256:img-d", nil, nil)
	fake.StopContainer("d-web")
	seedLease(t, state, "myapp-docs", lease.Lease{
		Project: "myapp-docs", State: lease.StateStopped,
		StoppedAt:       now.Add(-3 * 24 * time.Hour),
		RemoveAfter:     ptr(0 * time.Second),
		RemoveAfterFrom: "label",
	})

	// myapp-perf: being removed right now.
	fake.AddContainerWithUses("p-web", "myapp-perf_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp-perf",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": workDir("myapp-perf"),
	}, "sha256:img-p", nil, nil)
	fake.StopContainer("p-web")
	seedLease(t, state, "myapp-perf", lease.Lease{
		Project: "myapp-perf", State: lease.StateRemoving,
		StoppedAt: now.Add(-time.Hour),
	})

	// myapp-old-api: new, containers up before the reaper's first sweep
	// recorded any lease.
	fake.AddContainerWithUses("o-api", "myapp-old-api_api_1", map[string]string{
		docker.ProjectLabel:                      "myapp-old-api",
		docker.ServiceLabel:                      "api",
		"com.docker.compose.project.working_dir": workDir("myapp-old-api"),
	}, "sha256:img-o", nil, nil)
	fake.StopContainer("o-api")

	// myapp-spike: live with no holders (every claim expired).
	fake.AddContainerWithUses("sp-web", "myapp-spike_web_1", map[string]string{
		docker.ProjectLabel:                      "myapp-spike",
		docker.ServiceLabel:                      "web",
		"com.docker.compose.project.working_dir": workDir("myapp-spike"),
	}, "sha256:img-sp", nil, nil)
}

func extendDemo(t *testing.T, state lease.Store, project, holder string, ttl time.Duration, now time.Time) {
	t.Helper()
	if err := state.Update(func(m map[string]lease.Lease) error {
		_, err := lease.ExtendProject(m, project, holder, ttl, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func seedLease(t *testing.T, state lease.Store, project string, l lease.Lease) {
	t.Helper()
	if err := state.Update(func(m map[string]lease.Lease) error {
		m[project] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func ptr(d time.Duration) *time.Duration { return &d }
