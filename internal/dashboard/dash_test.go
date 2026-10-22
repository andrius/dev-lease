package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// dashNow is the fixed clock every test renders and acts against.
var dashNow = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

type dh struct {
	fake   *fakedocker.Server
	runner *reaper.Runner
	state  lease.Store
}

// newDH wires a fake daemon, a lease store and a Runner with the fixed
// clock, like serve does: patterns myapp* and ghost are managed, the
// project dev-lease is its own and never managed.
func newDH(t *testing.T) *dh {
	t.Helper()
	fake := fakedocker.New(t)
	state := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	cfg := reaper.Config{
		Patterns:    []string{"myapp*", "ghost"},
		Own:         "dev-lease",
		DefaultTTL:  2 * time.Hour,
		RemoveAfter: 7 * 24 * time.Hour,
		Now:         func() time.Time { return dashNow },
	}
	runner := &reaper.Runner{Cli: docker.New(fake.Socket), Store: state, Cfg: cfg}
	return &dh{fake: fake, runner: runner, state: state}
}

// serve builds the dashboard and serves it on a fresh httptest server.
func (h *dh) serve(t *testing.T, o Options) *httptest.Server {
	t.Helper()
	if o.Runner == nil {
		o.Runner = h.runner
	}
	hnd, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(hnd)
	t.Cleanup(ts.Close)
	return ts
}

// seed stores one lease.
func (h *dh) seed(t *testing.T, name string, l lease.Lease) {
	t.Helper()
	if err := h.state.Update(func(m map[string]lease.Lease) error {
		m[name] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// extend gives holder a claim on project through the lease API.
func (h *dh) extend(t *testing.T, project, holder string, ttl time.Duration) {
	t.Helper()
	if err := h.state.Update(func(m map[string]lease.Lease) error {
		_, err := lease.ExtendProject(m, project, holder, ttl, dashNow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// leases reads the store.
func (h *dh) leases(t *testing.T) map[string]lease.Lease {
	t.Helper()
	m, err := h.state.Read()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// noFollow keeps redirects visible instead of following them.
var noFollow = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, req)
}

// post is one form POST with the server's own URL as the Origin, like a
// same-origin page submit.
func post(t *testing.T, ts *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	return do(t, req)
}

func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// element extracts one element's markup: from the first occurrence of
// open through the next occurrence of close. Assertions on the result
// cannot accidentally match other parts of the page.
func element(t *testing.T, page, open, close string) string {
	t.Helper()
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatalf("page lacks %q:\n%s", open, page)
	}
	rest := page[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("page lacks %q after %q:\n%s", close, open, page)
	}
	return page[i : i+len(open)+j+len(close)]
}

// stopDialog returns the stop dialog's markup alone.
func stopDialog(t *testing.T, page string) string {
	t.Helper()
	return element(t, page, `<dialog class="dialog" id="stop-dialog"`, `</dialog>`)
}

// removeDialog returns the remove dialog's markup alone.
func removeDialog(t *testing.T, page string) string {
	t.Helper()
	return element(t, page, `<dialog class="dialog" id="remove-dialog"`, `</dialog>`)
}

// leaseCard returns the lease card's markup alone.
func leaseCard(t *testing.T, page string) string {
	t.Helper()
	return element(t, page, `<section class="card" aria-label="Lease">`, `</section>`)
}

// muxLogFrame builds one multiplexed stream frame around one log line,
// like the docker client's own tests do.
func muxLogFrame(line string) []byte {
	frame := make([]byte, 8+len(line))
	frame[0] = 1
	frame[4] = byte(len(line) >> 24)
	frame[5] = byte(len(line) >> 16)
	frame[6] = byte(len(line) >> 8)
	frame[7] = byte(len(line))
	copy(frame[8:], line)
	return frame
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// csrfFrom pulls the page's csrf token, the one every POST must send.
func csrfFrom(t *testing.T, page string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token on the page")
	}
	return m[1]
}
