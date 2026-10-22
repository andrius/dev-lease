// Package mcp implements the Model Context Protocol server behind
// `dev-lease mcp`: one JSON-RPC line per message on stdin and stdout.
// It answers every read-only question from the dashboard view and
// forwards every stop or remove to the serve process over the control
// socket.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// protocolVersion is the MCP version this server speaks and the value
// the initialize answer carries, whatever the client asked for.
const protocolVersion = "2025-06-18"

// Tools carries the dependencies the server needs to answer calls.
type Tools struct {
	Cli         *docker.Client
	Store       lease.Store
	Cfg         reaper.Config
	ControlPath string
	Now         func() time.Time
	Version     string
}

// Serve runs the JSON-RPC loop: it reads one request per line, answers
// each request on its own line and returns nil when the input ends or
// the context is cancelled.
func Serve(ctx context.Context, in io.Reader, out io.Writer, tools Tools) error {
	now := tools.Now
	if now == nil {
		now = time.Now
	}
	s := &server{tools: tools, now: now}
	return s.run(ctx, in, out)
}

type server struct {
	tools Tools
	now   func() time.Time

	mu     sync.Mutex
	out    io.Writer
	wg     sync.WaitGroup
	closed bool
}

func (s *server) run(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	defer s.shutdown()

	// The input is read in its own goroutine so a cancelled context
	// returns from run even when the input stays open: the caller owns
	// the lifecycle, not the client's stdin.
	lines := make(chan lineEvent)
	go func() { s.readLines(ctx, in, lines) }()
	for {
		select {
		case ev, ok := <-lines:
			if !ok {
				return nil // input ended
			}
			if ev.tooLong {
				s.write(newRPCError(nil, codeInvalidRequest, "request too large"))
				continue
			}
			if len(bytes.TrimSpace(ev.line)) == 0 {
				continue
			}
			if !s.submit(ctx, ev.line) {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// maxRequestBytes is the longest request line the server reads; a longer
// line is drained and answered with "request too large".
const maxRequestBytes = 4 * 1024 * 1024

// lineEvent is one input line; tooLong marks a line over the limit.
type lineEvent struct {
	line    []byte
	tooLong bool
}

// readLines reads one line per event until the input ends or the context
// is cancelled, then closes the channel. A line longer than the limit is
// drained and reported as one too-long event; the session goes on with
// the next line.
func (s *server) readLines(ctx context.Context, in io.Reader, lines chan<- lineEvent) {
	br := bufio.NewReader(in)
	for {
		line, tooLong, err := nextLine(br)
		if err != nil {
			close(lines)
			return
		}
		// The slice points into the reader's buffer: it is copied before
		// the reader refills it.
		ev := lineEvent{line: append([]byte(nil), line...), tooLong: tooLong}
		select {
		case lines <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// nextLine reads one whole line from br: up to maxRequestBytes content
// bytes (the trailing newline not counted) it is returned as-is, a longer
// line is drained and reported as too long. A final line without a
// newline follows the same limit, so its answer is never dropped.
func nextLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	frag, err := br.ReadSlice('\n')
	switch {
	case err == nil:
		return frag, false, nil
	case errors.Is(err, io.EOF):
		if len(frag) > 0 {
			return frag, false, nil // the final line without a newline
		}
		return nil, false, err
	case !errors.Is(err, bufio.ErrBufferFull):
		return nil, false, err
	}
	// The line is longer than the reader's buffer: keep reading until the
	// newline or the end of the input, bounded by maxRequestBytes content
	// bytes.
	line = append([]byte(nil), frag...)
	for {
		frag, err = br.ReadSlice('\n')
		line = append(line, frag...)
		if err == nil || errors.Is(err, io.EOF) {
			content := line
			if n := len(content); n > 0 && content[n-1] == '\n' {
				content = content[:n-1]
			}
			if len(content) > maxRequestBytes {
				return nil, true, nil
			}
			return line, false, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, false, err
		}
		if len(line) > maxRequestBytes {
			// Over the limit: drain the rest of the line and report it,
			// so the next line is read from a clean position.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, false, err
			}
			return nil, true, nil
		}
	}
}

// shutdown drains the in-flight calls when the input ends: the writer
// closes after every handler is done, so no answer is ever cut in half.
func (s *server) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
	if c, ok := s.out.(io.Closer); ok {
		c.Close()
	}
}

// submit parses one line and hands the request to a handler; false
// means the input is ending and no new handler may start.
func (s *server) submit(ctx context.Context, line []byte) bool {
	var raw json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		s.write(newRPCError(nil, codeParseError, "Parse error"))
		return true
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(raw, &req); err != nil || req == nil {
		// A non-object line is an invalid request; no id can be read
		// from it, so it answers with id null.
		s.write(newRPCError(nil, codeInvalidRequest, "a request must be a JSON object"))
		return true
	}
	// The id is validated before anything else: an invalid id answers
	// with id null whatever else the request carries (a wrong jsonrpc
	// included).
	id, isNotification, ok := checkID(req["id"])
	if !ok {
		s.write(newRPCError(nil, codeInvalidRequest, "id must be a string or an integer"))
		return true
	}
	if isNotification {
		return true // notifications get no answer, also on error
	}
	version, ok := req["jsonrpc"]
	if !ok || string(version) != `"2.0"` {
		s.write(newRPCError(id, codeInvalidRequest, "jsonrpc must be exactly \"2.0\""))
		return true
	}
	methodRaw, ok := req["method"]
	if !ok {
		s.write(newRPCError(id, codeInvalidRequest, "a request needs a method"))
		return true
	}
	var method string
	if err := json.Unmarshal(methodRaw, &method); err != nil {
		s.write(newRPCError(id, codeInvalidRequest, "method must be a string"))
		return true
	}
	if !s.start(ctx, id, method, req["params"]) {
		return false
	}
	return true
}

// checkID validates the id: a request must carry a string or an integer
// id, a notification (no id) is valid and gets no answer.
func checkID(raw json.RawMessage) (id json.RawMessage, isNotification, ok bool) {
	if raw == nil {
		return nil, true, true // no id: a notification
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, false, false
	}
	switch n := v.(type) {
	case string:
		return raw, false, true
	case json.Number:
		// 1.5 and 1e3 are numbers but not integer ids.
		if strings.ContainsAny(n.String(), ".eE") {
			return nil, false, false
		}
		return raw, false, true
	}
	return nil, false, false
}

// start begins the handler goroutine unless the input is ending.
func (s *server) start(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		resp := s.dispatch(ctx, id, method, params)
		if resp != nil {
			s.write(resp)
		}
	}()
	return true
}

// dispatch routes one request; it returns nil for notifications and
// other requests that must not be answered.
func (s *server) dispatch(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) []byte {
	switch method {
	case "initialize":
		return s.initialize(id)
	case "ping":
		return response(id, emptyResult)
	case "tools/list":
		return response(id, map[string]any{"tools": toolDefs()})
	case "tools/call":
		return s.call(ctx, id, params)
	default:
		return newRPCError(id, codeMethodNotFound, fmt.Sprintf("unknown method %q", method))
	}
}

func (s *server) initialize(id json.RawMessage) []byte {
	return response(id, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "dev-lease",
			"version": s.tools.Version,
		},
	})
}

// write emits one whole answer line; callers hold no lock because each
// handler is a separate goroutine and the mutex keeps the lines whole.
func (s *server) write(payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.Write(payload)
	s.out.Write([]byte{'\n'})
}

// JSON-RPC error codes from the specification.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

var emptyResult = map[string]any{}

// newRPCError builds a JSON-RPC error object; a nil id answers null.
func newRPCError(id json.RawMessage, code int, message string) []byte {
	var idPart []byte
	if id == nil {
		idPart = []byte("null")
	} else {
		idPart = id
	}
	return fmt.Appendf(nil,
		`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%s}}`,
		idPart, code, jsonQuote(message))
}

// response builds a result object for an id; the raw id is echoed
// verbatim, so a string id comes back a string.
func response(id json.RawMessage, result any) []byte {
	var idPart []byte
	if id == nil {
		idPart = []byte("null")
	} else {
		idPart = id
	}
	payload, err := json.Marshal(result)
	if err != nil {
		// All results here are static shapes, so this cannot happen.
		payload = []byte("null")
	}
	return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":%s}`, idPart, payload)
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
