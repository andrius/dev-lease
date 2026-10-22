package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
)

func TestKeepForAtLeast(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	codes := []struct {
		code string
		ttl  time.Duration
		text string
	}{
		{"2h", 2 * time.Hour, "2 hours"},
		{"8h", 8 * time.Hour, "8 hours"},
		{"1d", 24 * time.Hour, "1 day"},
		{"7d", 7 * 24 * time.Hour, "7 days"},
	}
	for _, tc := range codes {
		t.Run("keep "+tc.code, func(t *testing.T) {
			resp := post(t, ts, "/stacks/myapp/keep", "csrf="+csrf+"&ttl="+tc.code)
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("keep = %d, want 303", resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != "/stacks/myapp?done=kept&ttl="+tc.code {
				t.Errorf("Location = %q, want /stacks/myapp?done=kept&ttl=%s", loc, tc.code)
			}
			l := h.leases(t)["myapp"]
			var mine lease.Holder
			for _, hd := range l.Holders {
				if hd.Name == "dashboard" {
					mine = hd
				}
			}
			if mine.Name == "" {
				t.Fatal("keep did not record the dashboard holder")
			}
			if !mine.Expires.Equal(dashNow.Add(tc.ttl)) {
				t.Errorf("dashboard claim = %v, want %v", mine.Expires, dashNow.Add(tc.ttl))
			}
			page := body(t, get(t, ts.URL+"/stacks/myapp?done=kept&ttl="+tc.code))
			if want := "Kept myapp for at least " + tc.text + "."; !strings.Contains(page, want) {
				t.Errorf("flash misses %q", want)
			}
		})
	}
	t.Run("an unknown ttl is 400", func(t *testing.T) {
		if got := post(t, ts, "/stacks/myapp/keep", "csrf="+csrf+"&ttl=3h").StatusCode; got != http.StatusBadRequest {
			t.Errorf("ttl=3h = %d, want 400", got)
		}
	})
	t.Run("an unmanaged name is 404", func(t *testing.T) {
		if got := post(t, ts, "/stacks/other/keep", "csrf="+csrf+"&ttl=2h").StatusCode; got != http.StatusNotFound {
			t.Errorf("keep on other = %d, want 404", got)
		}
	})
	t.Run("keeping a stopped stack revives the lease and says to start it", func(t *testing.T) {
		h.fake.Add("container", "c2", "stopped_db_1", "myapp_stopped")
		h.fake.StopContainer("c2")
		h.seed(t, "myapp_stopped", lease.Lease{Project: "myapp_stopped", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
		resp := post(t, ts, "/stacks/myapp_stopped/keep", "csrf="+csrf+"&ttl=2h")
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("keep on stopped = %d, want 303", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/stacks/myapp_stopped?done=kept&ttl=2h&was=stopped" {
			t.Errorf("Location = %q, want the was=stopped marker", loc)
		}
		if l := h.leases(t)["myapp_stopped"]; l.State != "" {
			t.Errorf("revived lease state = %q, want live", l.State)
		}
		page := body(t, get(t, ts.URL+"/stacks/myapp_stopped?done=kept&ttl=2h&was=stopped"))
		if want := "still stopped; start it with: docker compose -p myapp_stopped up -d"; !strings.Contains(page, want) {
			t.Errorf("revive flash misses %q", want)
		}
	})
}

func TestStopAction(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.Add("container", "c2", "myapp_db_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	resp := post(t, ts, "/stacks/myapp/stop", "csrf="+csrf)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/stacks/myapp?done=stopped" {
		t.Fatalf("stop = %d to %q, want 303 to /stacks/myapp?done=stopped", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, id := range []string{"c1", "c2"} {
		if !h.fake.IsStopped("container", id) {
			t.Errorf("container %s still running after the stop", id)
		}
	}
	l := h.leases(t)["myapp"]
	if l.State != lease.StateStopped {
		t.Errorf("lease state = %q, want stopped", l.State)
	}
	// The stop records the holders, released: none may still be live.
	for _, hd := range l.Holders {
		if hd.Expires.After(dashNow) {
			t.Errorf("live holder left after the stop: %+v", hd)
		}
	}
	page := body(t, get(t, ts.URL+"/stacks/myapp?done=stopped"))
	if !strings.Contains(page, "Stopped myapp.") {
		t.Error("stop flash missing")
	}
	if strings.Contains(page, "Release my claim") {
		t.Error("release button shown although nobody holds the lease")
	}
}

func TestRemoveNeedsTypedName(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.StopContainer("c1")
	h.fake.Add("network", "net1", "myapp_default", "myapp")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	t.Run("a wrong typed name removes nothing", func(t *testing.T) {
		resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=myappX")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("remove with wrong name = %d, want 400", resp.StatusCode)
		}
		if !h.fake.Has("container", "c1") || !h.fake.Has("network", "net1") {
			t.Error("a refused removal deleted objects")
		}
	})
	t.Run("the typed name removes and lands on the all-stacks page", func(t *testing.T) {
		resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=myapp")
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?done=removed&stack=myapp" {
			t.Fatalf("remove = %d to %q, want 303 to /?done=removed&stack=myapp", resp.StatusCode, resp.Header.Get("Location"))
		}
		if h.fake.Has("container", "c1") || h.fake.Has("network", "net1") {
			t.Error("objects left after the removal")
		}
		if _, ok := h.leases(t)["myapp"]; ok {
			t.Error("lease left after the removal")
		}
		page := body(t, get(t, ts.URL+"/?done=removed&stack=myapp"))
		if !strings.Contains(page, "Removed myapp.") {
			t.Error("removal flash missing on the all-stacks page")
		}
	})
}

// An unmanaged name answers 404 before the typed-name check: the
// removal must refuse the name itself, never check a typed value for
// a stack the dashboard does not manage.
func TestUnmanagedRemoveWrongNameIs404(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	resp := post(t, ts, "/stacks/other/remove", "csrf="+csrf+"&confirm=otherX")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("remove of an unmanaged stack with a wrong typed name = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "there is no managed stack with that name") {
		t.Error("the refusal does not say there is no managed stack")
	}
}

func TestRemoveNeverRemoveNeedsOverride(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.StopContainer("c1")
	h.fake.Add("network", "net1", "myapp_default", "myapp")
	zero := time.Duration(0)
	h.seed(t, "myapp", lease.Lease{
		Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour),
		RemoveAfter: &zero,
	})
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	t.Run("without override the removal is refused and nothing changes", func(t *testing.T) {
		resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=myapp")
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("remove = %d, want 409", resp.StatusCode)
		}
		if !strings.Contains(body(t, resp), "never remove") {
			t.Error("the refusal does not say never remove")
		}
		if !h.fake.Has("container", "c1") {
			t.Error("a refused removal deleted objects")
		}
		if l := h.leases(t)["myapp"]; l.State != lease.StateStopped {
			t.Errorf("a refused removal changed the lease to %q", l.State)
		}
	})
	t.Run("with override=1 the removal runs", func(t *testing.T) {
		resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=myapp&override=1")
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("remove with override = %d, want 303", resp.StatusCode)
		}
		if h.fake.Has("container", "c1") {
			t.Error("objects left after the removal")
		}
	})
}

func TestActionOnVanishedStack404(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	before, err := os.ReadFile(h.state.Path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	// ghost matches a pattern but has no daemon objects and no lease.
	resp := post(t, ts, "/stacks/ghost/stop", "csrf="+csrf)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("stop on vanished stack = %d, want 404", resp.StatusCode)
	}
	resp = post(t, ts, "/stacks/ghost/remove", "csrf="+csrf+"&confirm=ghost")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("remove on vanished stack = %d, want 404", resp.StatusCode)
	}
	after, err := os.ReadFile(h.state.Path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("an action on a vanished stack changed the lease state file")
	}
}

func TestActionWhileBusy409(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopping})
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	cases := map[string]struct{ path, form string }{
		"stop":   {"/stacks/myapp/stop", "csrf=" + csrf},
		"remove": {"/stacks/myapp/remove", "csrf=" + csrf + "&confirm=myapp"},
		"keep":   {"/stacks/myapp/keep", "csrf=" + csrf + "&ttl=2h"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp := post(t, ts, tc.path, tc.form)
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("%s on a stopping stack = %d, want 409", name, resp.StatusCode)
			}
		})
	}
}

func TestSlowDaemonPageTimesOut(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	release := make(chan struct{})
	h.fake.OnRequest(func(method, path string) {
		if method == http.MethodGet && path == "/v1.47/containers/json" {
			<-release
		}
	})
	ts := h.serve(t, Options{})
	pageTimeout.Store(int64(200 * time.Millisecond))
	t.Cleanup(func() { pageTimeout.Store(int64(10 * time.Second)) })

	start := time.Now()
	resp := get(t, ts.URL+"/stacks/myapp")
	elapsed := time.Since(start)
	close(release)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("page on a blocked daemon = %d, want 500", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("page answered after %v, want the page timeout to cut it", elapsed)
	}
}

func TestSlowActionStillAnswers(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	release := make(chan struct{})
	h.fake.OnRequest(func(method, path string) {
		if method == http.MethodPost && path == "/v1.47/containers/c1/stop" {
			<-release
		}
	})
	hnd, err := New(Options{Runner: h.runner})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(hnd)
	ts.Config.WriteTimeout = 200 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	done := make(chan *http.Response, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/stop", strings.NewReader("csrf="+csrf))
		if err != nil {
			t.Errorf("building the POST: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Errorf("POST: %v", err)
			close(done)
			return
		}
		done <- resp
	}()

	// The stop is blocked in the fake; the 200 ms WriteTimeout must not
	// have cut the answer.
	select {
	case resp := <-done:
		t.Fatalf("the blocked action answered after less than 400 ms: %d", resp.StatusCode)
	case <-time.After(400 * time.Millisecond):
	}
	close(release)
	select {
	case resp := <-done:
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("stop = %d, want 303", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the action never answered after the fake was released")
	}
}
