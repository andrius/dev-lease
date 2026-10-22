package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHostAllowlist(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})

	getWithHost := func(host string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		return do(t, req).StatusCode
	}
	t.Run("default hosts are allowed with any port", func(t *testing.T) {
		for _, host := range []string{"localhost", "localhost:9999", "127.0.0.1", "127.0.0.1:1234", "[::1]"} {
			if got := getWithHost(host); got != http.StatusOK {
				t.Errorf("GET / with Host %q = %d, want 200", host, got)
			}
		}
	})
	t.Run("anything else is refused", func(t *testing.T) {
		for _, host := range []string{"evil.example", "127.0.0.1.nip.io"} {
			if got := getWithHost(host); got != http.StatusForbidden {
				t.Errorf("GET / with Host %q = %d, want 403", host, got)
			}
		}
	})
	t.Run("a custom allowlist replaces the default", func(t *testing.T) {
		ts2 := h.serve(t, Options{Hosts: []string{"dev.example"}})
		req, err := http.NewRequest(http.MethodGet, ts2.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "dev.example"
		if got := do(t, req).StatusCode; got != http.StatusOK {
			t.Errorf("custom host = %d, want 200", got)
		}
		req, err = http.NewRequest(http.MethodGet, ts2.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "localhost"
		if got := do(t, req).StatusCode; got != http.StatusForbidden {
			t.Errorf("default host with custom allowlist = %d, want 403", got)
		}
	})
}

func TestPostNeedsCSRFAndOrigin(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	valid := "csrf=" + csrf + "&ttl=2h"

	postWith := func(origin, referer, form string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/keep", strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		return do(t, req).StatusCode
	}
	t.Run("no csrf field", func(t *testing.T) {
		if got := postWith(ts.URL, "", "ttl=2h"); got != http.StatusForbidden {
			t.Errorf("POST without csrf = %d, want 403", got)
		}
	})
	t.Run("wrong csrf field", func(t *testing.T) {
		if got := postWith(ts.URL, "", "csrf=deadbeef&ttl=2h"); got != http.StatusForbidden {
			t.Errorf("POST with wrong csrf = %d, want 403", got)
		}
	})
	t.Run("foreign origin", func(t *testing.T) {
		if got := postWith("https://evil.example", "", valid); got != http.StatusForbidden {
			t.Errorf("POST with foreign Origin = %d, want 403", got)
		}
	})
	t.Run("no origin and foreign referer", func(t *testing.T) {
		if got := postWith("", "https://evil.example/x", valid); got != http.StatusForbidden {
			t.Errorf("POST with foreign Referer = %d, want 403", got)
		}
	})
	t.Run("no origin and no referer", func(t *testing.T) {
		if got := postWith("", "", valid); got != http.StatusForbidden {
			t.Errorf("POST with neither = %d, want 403", got)
		}
	})
	t.Run("same origin with the token passes", func(t *testing.T) {
		if got := postWith(ts.URL, "", valid); got != http.StatusSeeOther {
			t.Errorf("valid POST = %d, want 303", got)
		}
	})
}

func TestGetNeverMutates(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	before, err := os.ReadFile(h.state.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/stacks/myapp", "/fragments/stacks", "/fragments/stacks/myapp", "/static/app.css", "/static/theme.js", "/nope"} {
		resp := get(t, ts.URL+path)
		if resp.StatusCode >= 500 {
			t.Errorf("GET %s = %d, want a render below 500", path, resp.StatusCode)
		}
	}
	after, err := os.ReadFile(h.state.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a GET changed the lease state file:\nbefore %q\nafter %q", before, after)
	}
	for _, c := range h.fake.Calls() {
		if c.Method != http.MethodGet {
			t.Errorf("a GET caused a daemon call: %s %s", c.Method, c.Path)
		}
	}
}

func TestTokenLogin(t *testing.T) {
	loginDelay.Store(int64(time.Millisecond))
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{Token: "s3cret"})

	t.Run("no cookie is redirected to the login page", func(t *testing.T) {
		resp := get(t, ts.URL+"/")
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Fatalf("GET / = %d to %q, want 303 to /login", resp.StatusCode, resp.Header.Get("Location"))
		}
	})
	loginPage := body(t, get(t, ts.URL+"/login"))
	csrf := csrfFrom(t, loginPage)
	t.Run("the wrong token is 401", func(t *testing.T) {
		resp := post(t, ts, "/login", "csrf="+csrf+"&token=nope")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong token = %d, want 401", resp.StatusCode)
		}
	})
	t.Run("the right token sets the session cookie and lands on /", func(t *testing.T) {
		resp := post(t, ts, "/login", "csrf="+csrf+"&token=s3cret")
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("login = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
		}
		// The cookie is HMAC-SHA256(token, "dl-session:" + processNonce):
		// sessions end when serve restarts.
		mac := hmac.New(sha256.New, []byte("s3cret"))
		mac.Write([]byte(sessionLabel))
		mac.Write(processNonce)
		want := hex.EncodeToString(mac.Sum(nil))
		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == "dl_session" {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("no dl_session cookie set")
		}
		if cookie.Value != want {
			t.Errorf("cookie value = %q, want the HMAC %q", cookie.Value, want)
		}
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
			t.Errorf("cookie flags = HttpOnly:%v SameSite:%v Path:%q, want HttpOnly, Strict and /", cookie.HttpOnly, cookie.SameSite, cookie.Path)
		}
		if cookie.Secure {
			t.Errorf("cookie Secure = true over plain HTTP, want Secure only over TLS")
		}
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		if got := do(t, req).StatusCode; got != http.StatusOK {
			t.Errorf("GET / with the cookie = %d, want 200", got)
		}
	})
	t.Run("static assets stay reachable without a session", func(t *testing.T) {
		if got := get(t, ts.URL+"/static/app.css").StatusCode; got != http.StatusOK {
			t.Errorf("GET /static/app.css without cookie = %d, want 200", got)
		}
	})
}

func TestSecurityHeaders(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
		"X-Content-Type-Options":  "nosniff",
	}
	check := func(resp *http.Response) {
		t.Helper()
		for k, v := range want {
			if got := resp.Header.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
	}
	t.Run("on pages", func(t *testing.T) {
		resp := get(t, ts.URL+"/")
		check(resp)
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("page Cache-Control = %q, want no-store", got)
		}
	})
	t.Run("on refusals too", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "evil.example"
		resp := do(t, req)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("refused request = %d, want 403", resp.StatusCode)
		}
		check(resp)
	})
	t.Run("on static assets, without no-store", func(t *testing.T) {
		resp := get(t, ts.URL+"/static/app.css")
		check(resp)
		if got := resp.Header.Get("Cache-Control"); got == "no-store" {
			t.Errorf("static Cache-Control = %q, want it cacheable", got)
		}
	})
}

// loginSession logs into ts with the token and returns the session
// cookie's value.
func loginSession(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	page := body(t, get(t, ts.URL+"/login"))
	resp := post(t, ts, "/login", "csrf="+csrfFrom(t, page)+"&token="+token)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "dl_session" {
			return c.Value
		}
	}
	t.Fatal("no dl_session cookie set")
	return ""
}

// getWithCookie is a GET carrying the given dl_session cookie.
func getWithCookie(t *testing.T, ts *httptest.Server, path, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "dl_session", Value: cookie})
	return do(t, req)
}

// postWithCookie is one form POST carrying the given dl_session cookie
// with the server's own URL as the Origin, like a same-origin submit.
func postWithCookie(t *testing.T, ts *httptest.Server, path, cookie, form string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(&http.Cookie{Name: "dl_session", Value: cookie})
	return do(t, req)
}

// In token mode the action token binds to the session cookie: a token
// taken from one session's page is refused with another session's
// cookie, even though both cookies are valid sessions of their own
// dashboard.
func TestActionTokenBoundToSession(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "alice", time.Hour)
	tsA := h.serve(t, Options{Token: "token-a"})
	tsB := h.serve(t, Options{Token: "token-b"})

	cookieA := loginSession(t, tsA, "token-a")
	cookieB := loginSession(t, tsB, "token-b")

	tokenA := csrfFrom(t, body(t, getWithCookie(t, tsA, "/stacks/myapp", cookieA)))
	tokenB := csrfFrom(t, body(t, getWithCookie(t, tsB, "/stacks/myapp", cookieB)))
	if tokenA == tokenB {
		t.Fatal("the two sessions' action tokens are equal")
	}

	// Session A's token with session B's cookie is refused before any
	// action runs.
	resp := postWithCookie(t, tsB, "/stacks/myapp/keep", cookieB, "csrf="+tokenA+"&ttl=2h")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("keep with session A's token and session B's cookie = %d, want 403", resp.StatusCode)
	}
	// Control: session B's own token works.
	if resp := postWithCookie(t, tsB, "/stacks/myapp/keep", cookieB, "csrf="+tokenB+"&ttl=2h"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("keep with session B's own token = %d, want 303", resp.StatusCode)
	}
}

// The no-token mode's per-process token is not an action token in token
// mode: a token taken from a tokenless dashboard's page is refused by a
// token-mode dashboard even with a valid session cookie.
func TestNoTokenModeTokenRefusedInTokenMode(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "alice", time.Hour)
	tsNo := h.serve(t, Options{})
	tsTok := h.serve(t, Options{Token: "s3cret"})

	plain := csrfFrom(t, body(t, get(t, tsNo.URL+"/stacks/myapp")))
	cookie := loginSession(t, tsTok, "s3cret")

	resp := postWithCookie(t, tsTok, "/stacks/myapp/keep", cookie, "csrf="+plain+"&ttl=2h")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("keep with the no-token mode's token = %d, want 403", resp.StatusCode)
	}
	// Control: the token-mode page's own token works.
	token := csrfFrom(t, body(t, getWithCookie(t, tsTok, "/stacks/myapp", cookie)))
	if resp := postWithCookie(t, tsTok, "/stacks/myapp/keep", cookie, "csrf="+token+"&ttl=2h"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("keep with the session's own token = %d, want 303", resp.StatusCode)
	}
}

// A wrong login waits before the 401: guessing tokens must be slow.
func TestWrongLoginWaits(t *testing.T) {
	old := loginDelay.Load()
	loginDelay.Store(int64(200 * time.Millisecond))
	defer loginDelay.Store(old)
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{Token: "s3cret"})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/login")))
	start := time.Now()
	resp := post(t, ts, "/login", "csrf="+csrf+"&token=nope")
	waited := time.Since(start)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong login = %d, want 401", resp.StatusCode)
	}
	if waited < 150*time.Millisecond {
		t.Errorf("the 401 came after %v, want at least the 200ms delay", waited)
	}
}

// The cross-port chain: token mode, an unauthenticated reader takes the
// csrf from /login, a page on another port of the same host posts it
// with the owner's session cookie. Refused with 403 and nothing changes.
func TestTokenChainRefused(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "pi", time.Hour)
	ts := h.serve(t, Options{Token: "s3cret"})
	login := body(t, get(t, ts.URL+"/login"))
	csrf := csrfFrom(t, login)
	sess := (&server{token: []byte("s3cret")}).sessionCookie()
	hostport := strings.TrimPrefix(ts.URL, "http://")
	host := strings.Split(hostport, ":")[0]
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/stop", strings.NewReader("csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+host+":3000")
	req.AddCookie(&http.Cookie{Name: "dl_session", Value: sess})
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the chain POST = %d, want 403", resp.StatusCode)
	}
	if l := h.leases(t)["myapp"]; l.State != "" {
		t.Errorf("lease state = %q after the refused POST, want live", l.State)
	}
}

// The action csrf derives from the session; the login page's token is
// accepted only by POST /login. Posting the login token to an action
// with the right Origin and a valid session is refused.
func TestLoginTokenIsNotAnActionToken(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "pi", time.Hour)
	ts := h.serve(t, Options{Token: "s3cret"})
	login := body(t, get(t, ts.URL+"/login"))
	loginCSRF := csrfFrom(t, login)
	sess := (&server{token: []byte("s3cret")}).sessionCookie()

	postKeep := func(csrf string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/keep", strings.NewReader("csrf="+csrf+"&ttl=2h"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		req.AddCookie(&http.Cookie{Name: "dl_session", Value: sess})
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := postKeep(loginCSRF); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("keep with the login page's token = %d, want 403", resp.StatusCode)
	}
	if l := h.leases(t)["myapp"]; len(l.Holders) != 1 {
		t.Fatalf("holders = %v after the refused keep, want the one original claim", l.Holders)
	}
	// The action token from the stack page works.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/stacks/myapp", nil)
	req.AddCookie(&http.Cookie{Name: "dl_session", Value: sess})
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `name="csrf"`) {
		t.Fatal("the stack page carries no csrf field")
	}
	if resp := postKeep(csrfFrom(t, string(page))); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("keep with the action token = %d, want 303", resp.StatusCode)
	}
}

// The Origin check compares the URL's host:port exactly with r.Host:
// another port, the null Origin, a suffix or prefix host, a userinfo
// prefix and a non-http scheme are all refused.
func TestOriginExactHostPort(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	hostport := strings.TrimPrefix(ts.URL, "http://")
	host := strings.Split(hostport, ":")[0]

	postWith := func(origin, referer string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/keep", strings.NewReader("csrf="+csrf+"&ttl=2h"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	cases := []struct {
		origin, referer string
	}{
		{"http://" + host + ":1", ""},                // another port
		{"https://" + host + ":3000", ""},            // https, another port
		{"null", ""},                                 // the null Origin
		{"http://evil" + hostport, ""},               // a suffix host
		{"http://" + hostport + ".evil.example", ""}, // a prefix host
		{"http://" + hostport + "@evil.example", ""}, // userinfo, foreign host
		{"ftp://" + hostport + "/", ""},              // not http(s)
		{"", "http://" + host + ":3000/evil"},        // a foreign-port Referer
		{"", "ftp://" + hostport + "/"},              // a non-http Referer
	}
	for _, c := range cases {
		if got := postWith(c.origin, c.referer); got != http.StatusForbidden {
			t.Errorf("Origin %q Referer %q = %d, want 403", c.origin, c.referer, got)
		}
	}
	// The exact origin passes.
	if got := postWith(ts.URL, ""); got != http.StatusSeeOther {
		t.Errorf("the exact Origin = %d, want 303", got)
	}
}

// A POST whose csrf no longer matches (serve restarted while the page
// sat open) answers a small HTML page that says the page is out of date
// and links back, instead of the plain-text refusal an attacker gets.
func TestStaleCSRFServesOutOfDatePage(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "pi", time.Hour)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	postWith := func(form string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/stacks/myapp/keep", strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		return do(t, req)
	}
	// Control: the page's own token still works.
	if resp := postWith("csrf=" + csrf + "&ttl=2h"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("keep with the page's token = %d, want 303", resp.StatusCode)
	}
	before := len(h.leases(t)["myapp"].Holders)
	// The stale token answers the out-of-date page, not plain text.
	resp := postWith("csrf=deadbeef&ttl=2h")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("keep with a stale token = %d, want 403", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "This page is out of date (dev-lease restarted); go back and reload it.") {
		t.Errorf("the stale POST answer lacks the out-of-date message: %q", b)
	}
	if !strings.Contains(string(b), `<a href="/">Back to the stacks</a>`) {
		t.Error("the stale POST answer lacks the link back")
	}
	if l := h.leases(t)["myapp"]; len(l.Holders) != before {
		t.Errorf("the refused keep changed the lease: %+v", l.Holders)
	}
}
