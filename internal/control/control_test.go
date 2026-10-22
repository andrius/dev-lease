package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

var ctlNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

type ch struct {
	srv    *fakedocker.Server
	runner *reaper.Runner
	log    *bytes.Buffer
	sock   string
	close  func() error
}

func newCH(t *testing.T) *ch {
	t.Helper()
	return newCHCtx(t, context.Background())
}

func newCHCtx(t *testing.T, ctx context.Context) *ch {
	t.Helper()
	srv := fakedocker.New(t)
	cli := docker.New(srv.Socket)
	log := &bytes.Buffer{}
	runner := &reaper.Runner{
		Cli:   cli,
		Store: lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")},
		Cfg: reaper.Config{
			Patterns:    []string{"myapp"},
			Own:         "dev-lease",
			RemoveAfter: 48 * time.Hour,
			Log:         log,
			Now:         func() time.Time { return ctlNow },
		},
	}
	sock := filepath.Join(t.TempDir(), "control.sock")
	closeFn, err := Listen(ctx, sock, runner)
	if err != nil {
		t.Fatal(err)
	}
	return &ch{srv: srv, runner: runner, log: log, sock: sock, close: closeFn}
}

func (h *ch) put(t *testing.T, l lease.Lease) {
	t.Helper()
	if err := h.runner.Store.Update(func(m map[string]lease.Lease) error {
		m[l.Project] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestControlStopAndRemove(t *testing.T) {
	h := newCH(t)
	defer h.close()
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{
		Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour,
	}}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"})
	if err != nil {
		t.Fatalf("Call stop: %v", err)
	}
	if !stop.OK || stop.Code != "" {
		t.Fatalf("stop answer = %+v, want ok", stop)
	}
	leases, err := h.runner.Store.Read()
	if err != nil || leases["myapp"].State != lease.StateStopped {
		t.Fatalf("lease after stop = %+v (err %v), want stopped", leases["myapp"], err)
	}
	remove, err := Call(ctx, h.sock, Request{Action: "remove", Project: "myapp", Confirm: "myapp"})
	if err != nil {
		t.Fatalf("Call remove: %v", err)
	}
	if !remove.OK || len(remove.Report.Removed) != 1 || remove.Report.Removed[0] != "myapp" {
		t.Fatalf("remove answer = %+v, want myapp removed", remove)
	}
	m, err := h.runner.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := m["myapp"]; still {
		t.Fatal("lease still present after the removal")
	}
	if !strings.Contains(h.log.String(), `"via":"mcp"`) {
		t.Errorf("log has no mcp action line: %s", h.log.String())
	}
}

func TestControlRemoveNeedsConfirm(t *testing.T) {
	h := newCH(t)
	defer h.close()
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := Call(ctx, h.sock, Request{Action: "remove", Project: "myapp", Confirm: "otherapp"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if ans.OK || ans.Code != "bad_request" {
		t.Fatalf("answer = %+v, want a bad_request refusal", ans)
	}
	m, err := h.runner.Store.Read()
	if err != nil || m["myapp"].State != "" {
		t.Fatalf("lease = %+v (err %v), want it untouched", m["myapp"], err)
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("the container must survive a refused removal")
	}
}

func TestControlNeverRemoveRefused(t *testing.T) {
	h := newCH(t)
	defer h.close()
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel:     "myapp",
		docker.RemoveAfterLabel: "0",
	}, "sha256:i1", nil, nil)
	zero := time.Duration(0)
	h.put(t, lease.Lease{
		Project: "myapp", State: lease.StateStopped, StoppedAt: ctlNow.Add(-time.Hour),
		RemoveAfter: &zero, RemoveAfterFrom: "label",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := Call(ctx, h.sock, Request{Action: "remove", Project: "myapp", Confirm: "myapp"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if ans.OK || ans.Code != "never_remove" {
		t.Fatalf("answer = %+v, want a never_remove refusal", ans)
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("the container must survive a refused removal")
	}
}

func TestControlBadRequests(t *testing.T) {
	h := newCH(t)
	defer h.close()

	send := func(payload []byte) Answer {
		t.Helper()
		conn, err := net.Dial("unix", h.sock)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		conn.(*net.UnixConn).CloseWrite()
		var ans Answer
		if err := json.NewDecoder(conn).Decode(&ans); err != nil {
			t.Fatalf("decode answer: %v", err)
		}
		return ans
	}
	for name, payload := range map[string][]byte{
		"garbage":        []byte("garbage"),
		"unknown action": []byte(`{"action":"start","project":"myapp"}`),
		"oversized body": bytes.Repeat([]byte("x"), 1<<20+1),
	} {
		t.Run(name, func(t *testing.T) {
			ans := send(payload)
			if ans.OK || ans.Code != "bad_request" {
				t.Fatalf("answer = %+v, want a bad_request refusal", ans)
			}
		})
	}
}

func TestControlSocketMode0600(t *testing.T) {
	h := newCH(t)
	defer h.close()
	info, err := os.Stat(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCallWithoutServe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Call(ctx, filepath.Join(t.TempDir(), "control.sock"), Request{Action: "stop", Project: "myapp"})
	if err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("Call error = %v, want it to name serve", err)
	}
}

// A serve killed with SIGKILL (or OOM) leaves control.sock on the state
// volume. The next serve replaces it: serve holds the serve lock, so no
// other serve can own the socket.
func TestListenReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the process "died": the file stays
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("setup: stale socket not left: %v", err)
	}
	closeFn, err := Listen(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("Listen over a stale socket left by a dead serve: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("close left the socket behind: %v", err)
	}
}

// A regular file at the socket path is not touched: Listen refuses with
// an error naming the path.
func TestListenRefusesNonSocketFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(context.Background(), path, nil)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Listen over a regular file: err = %v, want a refusal naming %s and the non-socket reason", err, path)
	}
}

// The request read deadline does not cover the action or the answer: a
// slow stop still delivers its answer.
func TestCallGetsAnswerOfSlowAction(t *testing.T) {
	h := newCH(t)
	defer h.close()
	old := requestTimeout.Load()
	requestTimeout.Store(int64(100 * time.Millisecond))
	defer requestTimeout.Store(old)
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{
		Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour,
	}}})
	h.srv.BeforeRemove("container", "c1", func() { time.Sleep(300 * time.Millisecond) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"})
	if err != nil || !ans.OK {
		t.Fatalf("Call = %+v, %v; the slow stop's answer must still arrive", ans, err)
	}
}

// A client that connects and sends nothing is dropped after the request
// timeout, and no action runs.
func TestSilentClientIsDropped(t *testing.T) {
	h := newCH(t)
	defer h.close()
	old := requestTimeout.Load()
	requestTimeout.Store(int64(100 * time.Millisecond))
	defer requestTimeout.Store(old)
	conn, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	one := make([]byte, 1)
	if _, err := conn.Read(one); err == nil {
		t.Fatal("expected the connection to close without an answer, got data")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("connection closed after %v, want about requestTimeout", d)
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" || call.Method == "DELETE" {
			t.Fatalf("a silent client ran an action: %s %s", call.Method, call.Path)
		}
	}
}

// After serve's context is cancelled, a request queued on the runner runs
// nothing, and a new request is refused with shutting_down.
func TestShutdownRefusesNewAndQueuedActions(t *testing.T) {
	serveCtx, shutdown := context.WithCancel(context.Background())
	h := newCHCtx(t, serveCtx)
	h.runner.Cfg.Patterns = []string{"myapp", "other"}
	h.srv.Add("container", "c1", "web", "myapp")
	h.srv.Add("container", "o1", "oweb", "other")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour}}})
	h.put(t, lease.Lease{Project: "other", Holders: []lease.Holder{{Name: "ci", Expires: ctlNow.Add(-time.Minute), ExtendedAt: ctlNow.Add(-time.Hour), TTL: time.Hour}}})

	inPass, release := make(chan struct{}), make(chan struct{})
	h.srv.BeforeRemove("container", "o1", func() { close(inPass); <-release })
	passDone := make(chan struct{})
	go func() { defer close(passDone); h.runner.Pass(serveCtx) }()
	<-inPass

	callDone := make(chan Answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ans, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"})
		if err != nil {
			callDone <- Answer{}
			return
		}
		callDone <- ans
	}()
	time.Sleep(200 * time.Millisecond) // the request is queued on the runner

	shutdown() // SIGTERM to serve
	close(release)
	<-passDone

	// A new request is refused with shutting_down, without acting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"})
	if err != nil {
		t.Fatalf("Call after shutdown: %v", err)
	}
	if ans.OK || ans.Code != CodeShuttingDown {
		t.Fatalf("answer after shutdown = %+v, want shutting_down", ans)
	}

	h.close() // serve's deferred controlClose waits for the queued handler
	<-callDone
	if h.srv.IsStopped("container", "c1") {
		t.Error("a stop queued before the shutdown ran after serve's context was cancelled")
	}
}

// The answer a request queued on the runner gets at shutdown carries
// CodeShuttingDown, so the dashboard can tell shutdown apart from a
// refusal.
func TestQueuedAnswerAtShutdownHasCode(t *testing.T) {
	serveCtx, shutdown := context.WithCancel(context.Background())
	h := newCHCtx(t, serveCtx)
	h.runner.Cfg.Patterns = []string{"myapp", "other"}
	h.srv.Add("container", "c1", "web", "myapp")
	h.srv.Add("container", "o1", "oweb", "other")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour}}})
	h.put(t, lease.Lease{Project: "other", Holders: []lease.Holder{{Name: "ci", Expires: ctlNow.Add(-time.Minute), ExtendedAt: ctlNow.Add(-time.Hour), TTL: time.Hour}}})
	inPass, release := make(chan struct{}), make(chan struct{})
	h.srv.BeforeRemove("container", "o1", func() { close(inPass); <-release })
	passDone := make(chan struct{})
	go func() { defer close(passDone); h.runner.Pass(serveCtx) }()
	<-inPass
	got := make(chan Answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ans, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"})
		if err != nil {
			t.Errorf("Call: %v", err)
		}
		got <- ans
	}()
	time.Sleep(300 * time.Millisecond) // the request is queued on the runner
	shutdown()
	close(release)
	<-passDone
	h.close()
	ans := <-got
	if ans.OK || ans.Code != CodeShuttingDown {
		t.Fatalf("queued request's answer at shutdown = %+v, want code shutting_down", ans)
	}
}

// close waits for a handler mid-action: it returns only after the handler
// has run to completion.
func TestCloseWaitsForHandlers(t *testing.T) {
	h := newCH(t)
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour}}})
	inStop, releaseStop := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseStop) })
	defer release() // a Fatal while the stop is blocked must not deadlock the fake's cleanup
	h.srv.BeforeRemove("container", "c1", func() { close(inStop); <-releaseStop })
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"}); err != nil {
			t.Errorf("Call stop: %v", err)
		}
	}()
	<-inStop
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		if err := h.close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	select {
	case <-closeDone:
		t.Fatal("close returned while a handler was still running")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	<-closeDone
	<-callDone
}

// A remove queued on the runner at shutdown runs on Listen's context and
// does nothing: no state change, no daemon delete.
func TestRemoveQueuedAtShutdownDoesNothing(t *testing.T) {
	serveCtx, shutdown := context.WithCancel(context.Background())
	h := newCHCtx(t, serveCtx)
	h.runner.Cfg.Patterns = []string{"myapp", "other"}
	h.srv.Add("container", "c1", "web", "myapp")
	h.srv.Add("container", "o1", "oweb", "other")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour}}})
	h.put(t, lease.Lease{Project: "other", Holders: []lease.Holder{{Name: "ci", Expires: ctlNow.Add(-time.Minute), ExtendedAt: ctlNow.Add(-time.Hour), TTL: time.Hour}}})
	inPass, release := make(chan struct{}), make(chan struct{})
	h.srv.BeforeRemove("container", "o1", func() { close(inPass); <-release })
	passDone := make(chan struct{})
	go func() { defer close(passDone); h.runner.Pass(serveCtx) }()
	<-inPass
	callDone := make(chan Answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ans, err := Call(ctx, h.sock, Request{Action: "remove", Project: "myapp", Confirm: "myapp"})
		if err != nil {
			t.Errorf("Call remove: %v", err)
		}
		callDone <- ans
	}()
	time.Sleep(200 * time.Millisecond) // the request is queued on the runner
	shutdown()
	close(release)
	<-passDone
	h.close()
	ans := <-callDone
	if ans.OK || ans.Code != CodeShuttingDown {
		t.Fatalf("queued remove's answer at shutdown = %+v, want code shutting_down", ans)
	}
	if !h.srv.Has("container", "c1") {
		t.Fatal("a remove queued before the shutdown ran after serve's context was cancelled")
	}
	leases, err := h.runner.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if l := leases["myapp"]; l.State != "" || len(l.Holders) != 1 {
		t.Fatalf("lease after the queued remove = %+v, want the live claim untouched", l)
	}
}

// A symlink at the socket path is refused as a non-socket, and the
// symlink's target is left alone.
func TestListenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.sock")
	ln, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	defer ln.Close()
	sock := filepath.Join(dir, "control.sock")
	if err := os.Symlink(target, sock); err != nil {
		t.Fatal(err)
	}
	_, err = Listen(context.Background(), sock, &reaper.Runner{})
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Listen on a symlink: err = %v, want a refusal naming the non-socket reason", err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatal("the symlink's target socket was removed")
	}
}

// SocketPath is the state file path with .sock appended, so two serves
// with different state files in one directory use different sockets.
// codeFor must not guess shutting_down from an error type: the shutdown
// decision belongs to handle's ctx.Err() check, so a deadline exceeded
// coming from anything else passes through as a plain error.
func TestCodeForPassesDeadlineThrough(t *testing.T) {
	if code := codeFor(fmt.Errorf("read the answer: %w", context.DeadlineExceeded)); code != "" {
		t.Fatalf("codeFor(deadline exceeded) = %q, want no code", code)
	}
	if code := codeFor(fmt.Errorf("aborted: %w", context.Canceled)); code != "" {
		t.Fatalf("codeFor(canceled) = %q, want no code", code)
	}
}

func TestSocketPath(t *testing.T) {
	if got, want := SocketPath("/data/state.json"), "/data/state.json.sock"; got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

// A removal that ran but removed nothing (a failed delete) answers
// not_removed with the report.
func TestRemoveNotRemovedAnswer(t *testing.T) {
	h := newCH(t)
	defer h.close()
	h.srv.Add("container", "c1", "web", "myapp")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.srv.Refuse("volume", "myapp_data", 500)
	delay := 48 * time.Hour
	h.put(t, lease.Lease{Project: "myapp", State: lease.StateStopped,
		StoppedAt: ctlNow.Add(-time.Hour), RemoveAfter: &delay, RemoveAfterFrom: "serve"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := Call(ctx, h.sock, Request{Action: "remove", Project: "myapp", Confirm: "myapp"})
	if err != nil {
		t.Fatalf("Call remove: %v", err)
	}
	if ans.OK || ans.Code != CodeNotRemoved {
		t.Fatalf("answer = %+v, want a not_removed refusal", ans)
	}
	if ans.Report == nil {
		t.Fatal("the not_removed answer carries no report")
	}
}

// A busy project (marked stopping in the store) answers busy through the
// socket.
func TestControlBusyThroughSocket(t *testing.T) {
	h := newCH(t)
	defer h.close()
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp", State: lease.StateStopping})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"})
	if err != nil {
		t.Fatalf("Call stop: %v", err)
	}
	if ans.OK || ans.Code != CodeBusy {
		t.Fatalf("answer = %+v, want busy", ans)
	}
}

// An unmanaged project answers not_managed through the socket.
func TestControlNotManagedThroughSocket(t *testing.T) {
	h := newCH(t)
	defer h.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans, err := Call(ctx, h.sock, Request{Action: "stop", Project: "other"})
	if err != nil {
		t.Fatalf("Call stop: %v", err)
	}
	if ans.OK || ans.Code != CodeNotManaged {
		t.Fatalf("answer = %+v, want not_managed", ans)
	}
}

// A stop run over the socket tags its action line with via mcp.
func TestControlStopActionLineTagsViaMCP(t *testing.T) {
	h := newCH(t)
	defer h.close()
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{
		Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour,
	}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ans, err := Call(ctx, h.sock, Request{Action: "stop", Project: "myapp"}); err != nil || !ans.OK {
		t.Fatalf("Call stop = %+v, %v", ans, err)
	}
	if !strings.Contains(h.log.String(), `"via":"mcp"`) {
		t.Fatalf("the stop's action line does not carry via mcp:\n%s", h.log.String())
	}
}

// A valid request padded with whitespace past 1 MiB is refused bad_request
// and runs no action.
func TestControlOversizedBodyNoAction(t *testing.T) {
	h := newCH(t)
	defer h.close()
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{
		Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour,
	}}})
	conn, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	body := `{"action":"stop","project":"myapp"}` + strings.Repeat(" ", maxBody)
	if _, err := conn.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	conn.(*net.UnixConn).CloseWrite()
	var ans Answer
	if err := json.NewDecoder(conn).Decode(&ans); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if ans.OK || ans.Code != CodeBadRequest {
		t.Fatalf("answer = %+v, want a bad_request refusal", ans)
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" || call.Method == "DELETE" {
			t.Fatalf("an oversized request ran an action: %s %s", call.Method, call.Path)
		}
	}
}

// A stop that already finished is answered as the success it was even
// when the serve context is cancelled by the time the answer goes out:
// the shutdown refusal is for requests that fail because serve is going
// away, never for a stop that completed.
func TestStopFinishedBeforeShutdownAnswersOK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newCHCtx(t, ctx)
	defer h.close()
	h.srv.Add("container", "c1", "web", "myapp")
	h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{
		Name: "ci", Expires: ctlNow.Add(time.Hour), ExtendedAt: ctlNow, TTL: time.Hour,
	}}})

	// The serve context is cancelled while the stop is under way, right
	// before the daemon stops the container. The stop itself returns
	// without an error, so its answer must be the ok answer, not the
	// shutdown refusal.
	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method == "POST" && strings.HasSuffix(path, "/stop") {
			once.Do(cancel)
		}
	})

	callCtx, ccall := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccall()
	stop, err := Call(callCtx, h.sock, Request{Action: "stop", Project: "myapp"})
	if err != nil {
		t.Fatalf("Call stop: %v", err)
	}
	h.srv.OnRequest(nil)
	if !stop.OK || stop.Code == CodeShuttingDown {
		t.Fatalf("stop answer = %+v, want ok: the stop had already run through, the cancellation must not turn it into a shutdown refusal", stop)
	}
}
