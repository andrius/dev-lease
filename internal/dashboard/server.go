// Package dashboard is the HTML dashboard serve runs at --dashboard: one
// listing of the managed stacks, one detail page per stack, and the keep,
// release, stop and remove actions. Everything it shows comes from
// internal/view; every action runs in process through reaper.Runner, which
// serialises it with the sweeps.
package dashboard

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andrius/dev-lease/internal/reaper"
	"github.com/andrius/dev-lease/internal/view"
)

//go:embed templates static
var assets embed.FS

// Options builds the dashboard.
type Options struct {
	// Runner serves every action, and its store serves keep and release.
	Runner *reaper.Runner
	// Hosts is the Host header allowlist; empty means the default
	// localhost,127.0.0.1,[::1]. Ports are ignored.
	Hosts []string
	// Token, when set, turns the login on: a valid dl_session cookie
	// (HMAC-SHA256 of a fixed label with the token) or a redirect to
	// /login.
	Token string
	// Now is the clock the pages render against; nil means time.Now.
	Now func() time.Time
	// BaseContext is the context actions run on (serve's own context):
	// a closed browser tab must not abort a half-done stop or removal.
	// nil means context.Background().
	BaseContext context.Context
}

// New builds the dashboard handler. It fails when Runner is nil.
func New(o Options) (http.Handler, error) {
	if o.Runner == nil {
		return nil, errors.New("dashboard: Options.Runner is required")
	}
	s := &server{
		runner: o.Runner,
		token:  []byte(o.Token),
		now:    o.Now,
		base:   o.BaseContext,
	}
	if s.now == nil {
		s.now = o.Runner.Cfg.Now // tests pin the runner's clock
	}
	if s.now == nil {
		s.now = time.Now
	}
	hosts := o.Hosts
	if len(hosts) == 0 {
		hosts = defaultHosts
	}
	for _, h := range hosts {
		s.hosts = append(s.hosts, normHost(h))
	}
	csrf := make([]byte, 32)
	if _, err := rand.Read(csrf); err != nil {
		return nil, fmt.Errorf("dashboard: csrf token: %w", err)
	}
	s.csrf = hex.EncodeToString(csrf)
	login := make([]byte, 32)
	if _, err := rand.Read(login); err != nil {
		return nil, fmt.Errorf("dashboard: login csrf token: %w", err)
	}
	s.csrfLogin = hex.EncodeToString(login)

	t, err := template.New("dashboard").Funcs(s.funcs()).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: templates: %w", err)
	}
	s.tmpl = t
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("dashboard: static: %w", err)
	}
	s.static = http.FileServerFS(staticFS)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.stacks)
	mux.HandleFunc("GET /stacks/{name}", s.stack)
	mux.HandleFunc("GET /fragments/stacks", s.fragmentStacks)
	mux.HandleFunc("GET /fragments/stacks/{name}", s.fragmentStack)
	mux.HandleFunc("POST /stacks/{name}/keep", s.keep)
	mux.HandleFunc("POST /stacks/{name}/release", s.release)
	mux.HandleFunc("POST /stacks/{name}/stop", s.stop)
	mux.HandleFunc("POST /stacks/{name}/remove", s.remove)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("GET /favicon.ico", s.favicon)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))
	return s.security(mux), nil
}

type server struct {
	runner    *reaper.Runner
	hosts     []string
	token     []byte
	csrf      string
	csrfLogin string
	now       func() time.Time
	base      context.Context
	tmpl      *template.Template
	static    http.Handler
}

// funcs are the template functions: every format lives in internal/view.
func (s *server) funcs() template.FuncMap {
	return template.FuncMap{
		"fmtDur":  view.FmtDur,
		"fmtSize": view.FmtSize,
		// utcTime is the <time datetime> instant (decision: no other date
		// logic on the server); logTime is a log line's UTC wall time;
		// clock is the mockup's UTC clock text (hours and minutes).
		"utcTime": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"logTime": func(t time.Time) string {
			return t.UTC().Format("15:04:05")
		},
		"clock": func(t time.Time) string { return t.UTC().Format("15:04") + " UTC" },
		"badge": badgeFor,
		"join":  strings.Join,
		"path":  url.PathEscape,
		// volSize is a volume's size in the remove dialog's list ("40.0 MB").
		"volSize": func(d *view.Detail, name string) string {
			for _, v := range d.Volumes {
				if v.Name == name {
					if v.Bytes < 0 {
						return "unknown"
					}
					return view.FmtSize(v.Bytes)
				}
			}
			return "unknown"
		},
	}
}

// cfg returns the runner's config with the defaults serve's passes apply,
// so the pages read the same values the sweeps use.
func (s *server) cfg() reaper.Config {
	cfg := s.runner.Cfg
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 2 * time.Hour
	}
	return cfg
}

// pageTimeout bounds one page render: the page answers with a visible
// error state instead of hanging on a slow or unreachable daemon. It is
// an atomic package variable so tests can shorten it while handlers read
// it from their own goroutines.
var pageTimeout atomic.Int64 // nanoseconds, 10 s

func init() {
	pageTimeout.Store(int64(10 * time.Second))
}

// actionWriteDeadline is how long an action's answer may take: a stop or
// removal can outlast the server's WriteTimeout, and the handler extends
// its own deadline before calling the runner.
const actionWriteDeadline = 10 * time.Minute
