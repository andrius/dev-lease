// Package control is the control socket serve listens on for the dashboard
// and the MCP server: one JSON request and one JSON answer per connection,
// and the only actions it serves are stopping and removing a project right
// now. Everything a client wants to know it reads from the daemon and the
// state file itself (internal/view); the socket exists only so that stop
// and remove run serialised through the reaper with the sweep.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrius/dev-lease/internal/reaper"
)

// The refusal codes of an answer.
const (
	CodeBadRequest   = "bad_request"
	CodeNotManaged   = "not_managed"
	CodeNoStack      = "no_stack"
	CodeBusy         = "busy"
	CodeNeverRemove  = "never_remove"
	CodeNotRemoved   = "not_removed"
	CodeShuttingDown = "shutting_down"
)

// maxBody caps one request at 1 MiB.
const maxBody = 1 << 20

// callTimeout is how long Call waits for the whole exchange when its
// context carries no deadline: an action can take a while, and the client
// asked for no bound.
const callTimeout = 10 * time.Minute

// requestTimeout bounds how long a connection may take to send its
// request; answerTimeout bounds the write of one answer. Both are atomic
// package variables so tests can shorten them while handlers read them
// from their own goroutines.
var (
	requestTimeout atomic.Int64 // nanoseconds, 10 s
	answerTimeout  atomic.Int64 // nanoseconds, 10 s
)

func init() {
	requestTimeout.Store(int64(10 * time.Second))
	answerTimeout.Store(int64(10 * time.Second))
}

// Request is one control request. Confirm must repeat Project for a
// removal; a stop ignores it.
type Request struct {
	Action  string `json:"action"`
	Project string `json:"project"`
	Confirm string `json:"confirm"`
}

// Answer is one control answer. OK answers carry the runner's Report; a
// refusal carries a plain message and one of the Code* codes.
type Answer struct {
	OK     bool           `json:"ok"`
	Error  string         `json:"error,omitempty"`
	Code   string         `json:"code,omitempty"`
	Report *reaper.Report `json:"report,omitempty"`
}

// Listen serves the control socket at path until the returned close runs.
// The socket is created 0600; close removes it. A stale socket left by a
// dead serve is replaced (serve holds the serve lock, so no other serve
// can own it); any other file at path refuses with an error naming it. The
// actions run on a context derived from ctx: once ctx is done, a new
// request is refused with CodeShuttingDown and an action queued on the
// runner returns without acting.
func Listen(ctx context.Context, path string, r *reaper.Runner) (closeFn func() error, err error) {
	fi, err := os.Lstat(path)
	switch {
	case err == nil && fi.Mode()&os.ModeSocket != 0:
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove the stale control socket: %w", err)
		}
	case err == nil:
		return nil, fmt.Errorf("control socket path %s exists and is not a socket", path)
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, fmt.Errorf("chmod control socket: %w", err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the listener is closed
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				handle(conn, ctx, r)
			}()
		}
	}()
	return func() error {
		ln.Close()
		<-done // no handler can start after closeFn returns
		wg.Wait()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}, nil
}

// handle runs one request over one connection.
func handle(conn net.Conn, ctx context.Context, r *reaper.Runner) {
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(time.Duration(requestTimeout.Load()))); err != nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(conn, maxBody+1))
	if err != nil {
		return // no answer: the client went away or the deadline hit
	}
	if len(body) > maxBody {
		write(conn, refuse(CodeBadRequest, fmt.Sprintf("request larger than %d bytes", maxBody)))
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		write(conn, refuse(CodeBadRequest, "request is not JSON: "+err.Error()))
		return
	}
	// Serve is shutting down: no action starts. A request already queued
	// on the runner returns through the runner's own context check.
	if ctx.Err() != nil {
		write(conn, refuse(CodeShuttingDown, "serve is shutting down"))
		return
	}
	switch req.Action {
	case "stop":
		if req.Project == "" {
			write(conn, refuse(CodeBadRequest, "stop needs the project name"))
			return
		}
		rep, err := r.StopNow(ctx, req.Project, "mcp")
		if err != nil && ctx.Err() != nil {
			// The action failed because serve is going away: the
			// shutdown refusal is decided from the context, the same
			// check that guards new requests.
			write(conn, refuse(CodeShuttingDown, "serve is shutting down"))
			return
		}
		write(conn, answer(rep, err))
	case "remove":
		if req.Project == "" || req.Confirm != req.Project {
			write(conn, refuse(CodeBadRequest, "remove needs confirm repeating the project name"))
			return
		}
		// Never remove is refused: removing such a project must stay an
		// explicit serve-level decision, not something the socket can do.
		rep, err := r.RemoveNow(ctx, req.Project, reaper.RemoveOptions{Via: "mcp"})
		if err != nil && ctx.Err() != nil {
			write(conn, refuse(CodeShuttingDown, "serve is shutting down"))
			return
		}
		if err == nil && !reportRemoved(rep, req.Project) {
			write(conn, Answer{
				OK:     false,
				Code:   CodeNotRemoved,
				Error:  fmt.Sprintf("%s was not removed: a container is running again or a delete failed; see serve's log", req.Project),
				Report: &rep,
			})
			return
		}
		write(conn, answer(rep, err))
	default:
		write(conn, refuse(CodeBadRequest, "unknown action "+strconvQuote(req.Action)+": only stop and remove exist"))
	}
}

// reportRemoved reports whether the report's Removed names the project.
func reportRemoved(rep reaper.Report, project string) bool {
	for _, name := range rep.Removed {
		if name == project {
			return true
		}
	}
	return false
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// answer turns a runner outcome into an Answer.
func answer(rep reaper.Report, err error) Answer {
	if err == nil {
		return Answer{OK: true, Report: &rep}
	}
	return Answer{OK: false, Error: err.Error(), Code: codeFor(err)}
}

// refuse is a refusal that does not come from the runner.
func refuse(code, msg string) Answer {
	return Answer{OK: false, Error: msg, Code: code}
}

// codeFor maps the runner's refusal errors onto the answer codes. The
// shutdown decision lives in handle's ctx.Err() check, not here: a
// context error from anywhere else (an action's own deadline, a caller's
// cancel) passes through as a plain error.
func codeFor(err error) string {
	switch {
	case errors.Is(err, reaper.ErrNotManaged):
		return CodeNotManaged
	case errors.Is(err, reaper.ErrNoStack):
		return CodeNoStack
	case errors.Is(err, reaper.ErrBusy):
		return CodeBusy
	case errors.Is(err, reaper.ErrNeverRemove):
		return CodeNeverRemove
	}
	return ""
}

// SocketPath is the control socket path for the serve with the given state
// file: the state file path plus ".sock". Two serves with different state
// files in one directory never touch each other's socket.
func SocketPath(statePath string) string {
	return statePath + ".sock"
}

// write sends one answer under the answer deadline; a client that stopped
// reading gets nothing more.
func write(conn net.Conn, ans Answer) {
	if err := conn.SetWriteDeadline(time.Now().Add(time.Duration(answerTimeout.Load()))); err != nil {
		return
	}
	_ = json.NewEncoder(conn).Encode(ans)
}

// Call sends one request over the control socket and reads the answer. The
// returned error is transport: a missing socket is reported with serve
// named, so the caller can tell the user serve is not running. Closing the
// connection when the context is done makes a cancelled call return at
// once instead of waiting out the socket deadline.
func Call(ctx context.Context, path string, req Request) (Answer, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return Answer{}, fmt.Errorf("the control socket at %s: %w (is dev-lease serve running?)", path, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(callTimeout))
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Answer{}, fmt.Errorf("send the request: %w", err)
	}
	// One request per connection: closing the write side is the request's
	// end, which the handler waits for.
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	var ans Answer
	if err := json.NewDecoder(io.LimitReader(conn, maxBody+1)).Decode(&ans); err != nil {
		return Answer{}, fmt.Errorf("read the answer: %w", err)
	}
	return ans, nil
}
