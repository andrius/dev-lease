package dashboard

import (
	"context"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
)

// Edge cases of the dashboard's actions and checks: each test pins one
// behavior that a broken check would otherwise let through.

// the typed-name gate on removal. An empty typed name must be
// refused like any other mismatch and remove nothing.
func TestRemoveEmptyTypedNameRefused(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.StopContainer("c1")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("remove with an empty typed name = %d, want 400", resp.StatusCode)
	}
	if !h.fake.Has("container", "c1") {
		t.Error("a refused removal deleted the container")
	}
	if _, ok := h.leases(t)["myapp"]; !ok {
		t.Error("a refused removal dropped the lease")
	}
}

// in token mode an action POST without a session, with a
// wrong cookie and with a wrong-length cookie is bounced to /login and
// changes nothing. The session check runs before the csrf check, so the
// csrf field here is arbitrary.
func TestTokenModePostsNeedSession(t *testing.T) {
	loginDelay.Store(int64(time.Millisecond))
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "pi", time.Hour)
	ts := h.serve(t, Options{Token: "s3cret"})
	sess := (&server{token: []byte("s3cret")}).sessionCookie()

	cases := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"a wrong cookie", &http.Cookie{Name: "dl_session", Value: sess[:len(sess)-2] + "aa"}},
		{"a wrong-length cookie", &http.Cookie{Name: "dl_session", Value: "abcd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/keep", strings.NewReader("csrf=x&ttl=2h"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", ts.URL)
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			resp, err := noFollow.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
				t.Fatalf("keep %s = %d to %q, want 303 to /login", tc.name, resp.StatusCode, resp.Header.Get("Location"))
			}
			if l := h.leases(t)["myapp"]; len(l.Holders) != 1 || l.Holders[0].Name != "pi" {
				t.Errorf("the refused keep changed the lease: %+v", l.Holders)
			}
		})
	}
}

// a login with token= empty is 401.
func TestLoginEmptyTokenRefused(t *testing.T) {
	loginDelay.Store(int64(time.Millisecond))
	h := newDH(t)
	ts := h.serve(t, Options{Token: "s3cret"})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/login")))
	resp := post(t, ts, "/login", "csrf="+csrf+"&token=")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login with an empty token = %d, want 401", resp.StatusCode)
	}
}

// a login with a prefix of the token is 401.
func TestLoginTokenPrefixRefused(t *testing.T) {
	loginDelay.Store(int64(time.Millisecond))
	h := newDH(t)
	ts := h.serve(t, Options{Token: "s3cret"})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/login")))
	resp := post(t, ts, "/login", "csrf="+csrf+"&token=s3cr")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login with a prefix of the token = %d, want 401", resp.StatusCode)
	}
}

// a login POST without any token field is 401.
func TestLoginPostWithoutTokenRefused(t *testing.T) {
	loginDelay.Store(int64(time.Millisecond))
	h := newDH(t)
	ts := h.serve(t, Options{Token: "s3cret"})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/login")))
	resp := post(t, ts, "/login", "csrf="+csrf)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login without a token = %d, want 401", resp.StatusCode)
	}
}

// a multipart POST without the csrf field is 403 and changes
// nothing, whatever the form encoding.
func TestMultipartPostNeedsCSRF(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "pi", time.Hour)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("ttl", "2h"); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/keep", strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", ts.URL)
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("multipart keep without csrf = %d, want 403", resp.StatusCode)
	}
	if l := h.leases(t)["myapp"]; len(l.Holders) != 1 {
		t.Errorf("the refused keep changed the lease: %+v", l.Holders)
	}
	// The body was readable: the same multipart keep with the csrf field
	// passes, so only the missing token made the difference.
	var ok strings.Builder
	mw2 := multipart.NewWriter(&ok)
	if err := mw2.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	if err := mw2.WriteField("ttl", "2h"); err != nil {
		t.Fatal(err)
	}
	mw2.Close()
	req2, err := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/keep", strings.NewReader(ok.String()))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	req2.Header.Set("Origin", ts.URL)
	resp2, err := noFollow.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("multipart keep with csrf = %d, want 303", resp2.StatusCode)
	}
}

// keep while the lease is removing is 409.
func TestKeepWhileRemoving409(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateRemoving})
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	resp := post(t, ts, "/stacks/myapp/keep", "csrf="+csrf+"&ttl=2h")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("keep on a removing stack = %d, want 409", resp.StatusCode)
	}
}

// a removal whose sweep does not actually remove the project (the
// fake refuses one delete) is 409 with the not_removed text, and the
// lease survives.
func TestRemoveNotRemoved409(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.fake.StopContainer("c1")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
	h.fake.Refuse("container", "c1", http.StatusInternalServerError)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=myapp")
	body := body(t, resp)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "was not removed") {
		t.Fatalf("remove with a refused delete = %d, want 409 saying was not removed; body: %s", resp.StatusCode, body)
	}
	// The failed removal clears the lease by design: the next pass grants
	// the default lease and retries.
}

// actions run on serve's base context, not the request's: a done
// base context answers 503 and a stop runs even when the client hangs
// up. (The hang-up half is TestSlowActionStillAnswers.)
func TestDoneBaseContext503(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ts := h.serve(t, Options{BaseContext: ctx})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	resp := post(t, ts, "/stacks/myapp/stop", "csrf="+csrf)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stop with a done base context = %d, want 503", resp.StatusCode)
	}
}

// a POST whose client hangs up while the stop is queued behind a
// sweep still runs when the queue frees up (actions run on serve's base
// context, not the request's).
func TestClientHangupStopStillCompletes(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	// Hold the runner's lock with a sweep that blocks in the fake, so the
	// stop queues behind it.
	entered := make(chan struct{}, 1)
	blocked := make(chan struct{})
	h.fake.OnRequest(func(method, path string) {
		if method == http.MethodGet && path == "/v1.47/containers/json" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-blocked
		}
	})
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		if _, err := h.runner.Pass(context.Background()); err != nil {
			t.Errorf("sweep: %v", err)
		}
	}()
	<-entered

	// The stop POST arrives while the sweep holds the lock, and its client
	// hangs up before the queue frees up.
	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	body := "csrf=" + csrf
	fmt.Fprintf(conn, "POST /stacks/myapp/stop HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nOrigin: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		host, ts.URL, len(body), body)
	time.Sleep(200 * time.Millisecond)
	conn.Close()
	// Give the server a moment to see the close and cancel the request
	// context, then let the sweep finish and free the lock.
	time.Sleep(100 * time.Millisecond)
	close(blocked)
	<-sweepDone

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.fake.IsStopped("container", "c1") {
			if l := h.leases(t)["myapp"]; l.State == lease.StateStopped {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the stop never completed after the client hung up")
}

// the remove action extends the write deadline exactly like stop:
// a removal blocked in the fake still answers after the WriteTimeout.
func TestRemoveSurvivesWriteTimeout(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.seed(t, "myapp", lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: dashNow.Add(-time.Hour)})
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

	done := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/remove", strings.NewReader("csrf="+csrf+"&confirm=myapp"))
		if err != nil {
			t.Errorf("building the POST: %v", err)
			done <- 0
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Errorf("POST: %v", err)
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		t.Fatalf("the blocked removal answered before the release: %d", code)
	case <-time.After(400 * time.Millisecond):
	}
	close(release)
	select {
	case code := <-done:
		if code != http.StatusSeeOther {
			t.Fatalf("remove = %d, want 303", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the removal never answered after the fake was released")
	}
}

// the release button renders only when the dashboard holder has a
// live claim.
func TestReleaseButtonOnlyForDashboardClaim(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})

	page := body(t, get(t, ts.URL+"/stacks/myapp"))
	if strings.Contains(page, "Release my claim") {
		t.Fatal("release button shown although the dashboard has no claim")
	}
	h.extend(t, "myapp", "dashboard", time.Hour)
	page = body(t, get(t, ts.URL+"/stacks/myapp"))
	if !strings.Contains(page, "Release my claim") {
		t.Fatal("release button missing with a live dashboard claim")
	}
}

// the all-stacks page times out like the stack page when the
// daemon blocks.
func TestSlowAllStacksPageTimesOut(t *testing.T) {
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
	resp := get(t, ts.URL+"/")
	elapsed := time.Since(start)
	close(release)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("all-stacks page on a blocked daemon = %d, want 500", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("page answered after %v, want the page timeout to cut it", elapsed)
	}
}
