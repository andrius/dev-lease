package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// defaultHosts is the Host allowlist when Options.Hosts is empty.
var defaultHosts = []string{"localhost", "127.0.0.1", "[::1]"}

// sessionLabel is the message label of the session cookie's HMAC.
const sessionLabel = "dl-session:"

// The per-process secrets: processNonce salts the session cookie (so
// sessions end when serve restarts) and processKey derives the action
// csrf token from the session cookie.
var (
	processNonceOnce sync.Once
	processNonce     []byte
	processKeyOnce   sync.Once
	processKey       []byte
)

func initProcessNonce() {
	processNonce = processRandom32("session nonce")
}

func initProcessKey() {
	processKey = processRandom32("csrf key")
}

func processRandom32(what string) []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("dashboard: " + what + ": " + err.Error())
	}
	return b
}

// maxPostBody caps one form body at 1 MiB.
const maxPostBody = 1 << 20

// security wraps the mux with the checks, in this order on every request:
// the Host header in the allowlist (port ignored) or 403; with a token
// set, a valid dl_session cookie or a redirect to /login (login and
// static are exempt, or the login page could not load); for POSTs, the
// csrf form field in constant time plus an Origin whose host is the
// request's host (without Origin, the Referer's host), else 403. The
// security headers go on every answer, refusals included; Cache-Control
// no-store on every generated (non-static) answer.
func (s *server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		static := strings.HasPrefix(r.URL.Path, "/static/")
		s.headers(w)
		refuse := func(code int, text string) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(code)
			io.WriteString(w, text)
		}
		host := normHost(r.Host)
		allowed := false
		for _, h := range s.hosts {
			if h == host {
				allowed = true
				break
			}
		}
		if !allowed {
			refuse(http.StatusForbidden, "forbidden: this Host is not in the dashboard's allowlist\n")
			return
		}
		if len(s.token) > 0 && r.URL.Path != "/login" && !static {
			if !s.validSession(r) {
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxPostBody)
			if !s.validOrigin(r) {
				refuse(http.StatusForbidden, "forbidden: csrf token or Origin missing or wrong\n")
				return
			}
			want := s.csrfAction(r)
			if r.URL.Path == "/login" {
				want = s.csrfLogin
			}
			// The Origin matched, so this is the dashboard's own page,
			// but its token belongs to an earlier serve process: answer
			// a page that says so instead of plain text.
			if !s.csrfMatches(r, want) {
				s.stalePage(w, r)
				return
			}
		}
		if !static {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// headers is the security header set on every answer.
func (s *server) headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
}

// normHost strips the port and the IPv6 brackets from a Host value.
func normHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	return strings.ToLower(h)
}

// sessionCookie is the value of a valid dl_session cookie:
// HMAC-SHA256 of "dl-session:" and the process nonce keyed with the
// token, so sessions end when serve restarts.
func (s *server) sessionCookie() string {
	processNonceOnce.Do(initProcessNonce)
	mac := hmac.New(sha256.New, s.token)
	mac.Write([]byte(sessionLabel))
	mac.Write(processNonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// csrfAction is the csrf token an action page carries and an action
// POST must repeat. Without a token it is the per-process random token;
// with a token it derives from the session cookie, so the login page's
// token (s.csrfLogin) can never be replayed into an action.
func (s *server) csrfAction(r *http.Request) string {
	if len(s.token) == 0 {
		return s.csrf
	}
	processKeyOnce.Do(initProcessKey)
	value := ""
	if c, err := r.Cookie("dl_session"); err == nil {
		value = c.Value
	}
	mac := hmac.New(sha256.New, processKey)
	mac.Write([]byte("csrf:"))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// validSession reports a dl_session cookie equal to sessionCookie.
func (s *server) validSession(r *http.Request) bool {
	c, err := r.Cookie("dl_session")
	if err != nil {
		return false
	}
	want := s.sessionCookie()
	if len(c.Value) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// validOrigin reports an Origin header (or the Referer's, without
// Origin) whose URL is http or https and whose host:port is exactly the
// request's host:port, case-insensitive. The null Origin, another port,
// a suffix or prefix match, a userinfo prefix and other schemes are all
// refused.
func (s *server) validOrigin(r *http.Request) bool {
	ref := r.Header.Get("Origin")
	if ref == "" {
		ref = r.Header.Get("Referer")
	}
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// csrfMatches reports a csrf form field equal to want in constant time.
func (s *server) csrfMatches(r *http.Request, want string) bool {
	got := r.PostFormValue("csrf")
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// stalePage answers a POST whose Origin was fine but whose csrf token
// belongs to an earlier serve process: a page that says the page is out
// of date, with a link back.
func (s *server) stalePage(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusForbidden, "This page is out of date (dev-lease restarted); go back and reload it.")
}
