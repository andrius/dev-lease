package mcp

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

var mcpNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// session is one MCP conversation over net.Pipe connections: requests go
// into in, answers come out of out one whole JSON line at a time. Every
// send and read carries a deadline, so a broken implementation fails the
// test instead of hanging it.
type session struct {
	in   net.Conn
	outR net.Conn
	out  *bufio.Reader
	done chan error
}

// sessionDeadline bounds one send or read inside a session.
const sessionDeadline = 10 * time.Second

func startSession(t *testing.T, tools Tools) *session {
	t.Helper()
	inR, inW := net.Pipe()
	outR, outW := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), inR, outW, tools) }()
	return &session{in: inW, outR: outR, out: bufio.NewReader(outR), done: done}
}

func (s *session) send(t *testing.T, line string) {
	t.Helper()
	if err := s.in.SetWriteDeadline(time.Now().Add(sessionDeadline)); err != nil {
		t.Fatalf("set the write deadline: %v", err)
	}
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// read returns the next answer as one decoded JSON object.
func (s *session) read(t *testing.T) map[string]any {
	t.Helper()
	if err := s.outR.SetReadDeadline(time.Now().Add(sessionDeadline)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	line, err := s.out.ReadString('\n')
	if err != nil {
		t.Fatalf("read an answer: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("answer %q is not one whole JSON object: %v", line, err)
	}
	return m
}

func (s *session) close(t *testing.T) {
	t.Helper()
	if err := s.in.Close(); err != nil {
		t.Fatalf("close the input: %v", err)
	}
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the input ended")
	}
}

// tenv wires the tools: a fake daemon, a state file with a header and
// the injected clock.
type tenv struct {
	srv *fakedocker.Server
	st  lease.Store
}

func newToolEnv(t *testing.T) *tenv {
	t.Helper()
	e := &tenv{
		srv: fakedocker.New(t),
		st:  lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")},
	}
	if err := e.st.UpdateHeader(func(h *lease.Header) error {
		h.Patterns = []string{"myapp", "otherapp"}
		h.DefaultTTL = 2 * time.Hour
		h.RemoveAfter = 48 * time.Hour
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *tenv) put(t *testing.T, l lease.Lease) {
	t.Helper()
	if err := e.st.Update(func(m map[string]lease.Lease) error {
		m[l.Project] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// tools builds the server's tools the way `dev-lease mcp` does: the
// reaper config comes from the state file's header.
func (e *tenv) tools(t *testing.T, controlPath string) Tools {
	t.Helper()
	hdr, err := e.st.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	return Tools{
		Cli:         docker.New(e.srv.Socket),
		Store:       e.st,
		Cfg:         reaper.Config{Patterns: hdr.Patterns, DefaultTTL: hdr.DefaultTTL, RemoveAfter: hdr.RemoveAfter},
		ControlPath: controlPath,
		Now:         func() time.Time { return mcpNow },
		Version:     "test",
	}
}

func live(name string, expires time.Time) lease.Lease {
	return lease.Lease{Project: name, Holders: []lease.Holder{{
		Name: "ci", Expires: expires, ExtendedAt: expires.Add(-2 * time.Hour), TTL: 2 * time.Hour,
	}}}
}

// fakeControl is a control socket answering every request with ans; the
// first request is copied into got and received closes once it arrived.
func fakeControl(t *testing.T, ans control.Answer) (path string, got *control.Request, received chan struct{}) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got = &control.Request{}
	received = make(chan struct{})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var req control.Request
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				*got = req
				close(received)
				_ = json.NewEncoder(conn).Encode(ans)
			}(conn)
		}
	}()
	return path, got, received
}

func toolText(res map[string]any) string {
	content, _ := res["content"].([]any)
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

func rpcErrorCode(t *testing.T, m map[string]any) int {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("answer %v carries no JSON-RPC error", m)
	}
	code, _ := e["code"].(float64)
	return int(code)
}

func TestInitialize(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	// The client asks for an older protocol version; the answer names ours.
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"t","version":"1"},"capabilities":{}}}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("initialize answer carries an error: %v", m)
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize answer has no result object: %v", m)
	}
	if res["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s whatever the client asked for", res["protocolVersion"], protocolVersion)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != "dev-lease" {
		t.Errorf("serverInfo.name = %v, want dev-lease", info["name"])
	}
	if info["version"] != "test" {
		t.Errorf("serverInfo.version = %v, want the Tools version", info["version"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	tools, _ := caps["tools"].(map[string]any)
	if tools["listChanged"] != false {
		t.Errorf("capabilities.tools.listChanged = %v, want false", tools["listChanged"])
	}
}

func TestToolsListSchemas(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("tools/list answer carries an error: %v", m)
	}
	res := m["result"].(map[string]any)
	list, ok := res["tools"].([]any)
	if !ok || len(list) != 4 {
		t.Fatalf("tools = %v, want exactly four tools", res["tools"])
	}
	byName := map[string]map[string]any{}
	for _, tv := range list {
		tool, _ := tv.(map[string]any)
		byName[tool["name"].(string)] = tool
	}
	for _, name := range []string{"list_stacks", "stack_details", "stop_stack", "remove_stack"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("tool %s is missing from the list", name)
		}
	}
	// Every tool carries an input and an output schema, both objects, and
	// the input schemas forbid extra properties.
	for name, tool := range byName {
		in, _ := tool["inputSchema"].(map[string]any)
		out, _ := tool["outputSchema"].(map[string]any)
		if in["type"] != "object" || out["type"] != "object" {
			t.Errorf("%s: schemas are not objects: input type %v, output type %v", name, in["type"], out["type"])
		}
		if in["additionalProperties"] != false {
			t.Errorf("%s: inputSchema does not forbid extra properties", name)
		}
	}
	// The annotations the plan pins, tool by tool.
	assertAnno := func(name string, want map[string]any) {
		t.Helper()
		anno, _ := byName[name]["annotations"].(map[string]any)
		for k, v := range want {
			if anno[k] != v {
				t.Errorf("%s: annotations.%s = %v, want %v", name, k, anno[k], v)
			}
		}
	}
	assertAnno("list_stacks", map[string]any{"readOnlyHint": true})
	assertAnno("stack_details", map[string]any{"readOnlyHint": true})
	assertAnno("stop_stack", map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true})
	assertAnno("remove_stack", map[string]any{"destructiveHint": true, "idempotentHint": false})
	// Every description names what is visible: only the stacks
	// dev-lease manages.
	for name, tool := range byName {
		desc, _ := tool["description"].(string)
		if !strings.Contains(desc, "Only stacks dev-lease manages are visible") {
			t.Errorf("%s: description %q does not say only managed stacks are visible", name, desc)
		}
	}
	// The list_stacks input schema carries a properties object (empty,
	// but present), so a client can always walk it.
	if props, ok := byName["list_stacks"]["inputSchema"].(map[string]any)["properties"].(map[string]any); !ok || props == nil {
		t.Error("list_stacks inputSchema has no properties object")
	}
	// log_lines carries its bounds in the schema.
	ls := byName["stack_details"]["inputSchema"].(map[string]any)["properties"].(map[string]any)["log_lines"].(map[string]any)
	if ls["type"] != "integer" || ls["minimum"] != float64(0) || ls["maximum"] != float64(200) {
		t.Errorf("log_lines schema = %v, want integer with minimum 0 and maximum 200", ls)
	}
}

func TestListStacksTool(t *testing.T) {
	e := newToolEnv(t)
	srv := e.srv
	// myapp: one running and one stopped container, one sized volume, a
	// live lease held by ci.
	srv.AddLabels("container", "c1", "myapp-web", map[string]string{
		docker.ProjectLabel:                       "myapp",
		"com.docker.compose.project.working_dir":  "/srv/myapp",
		"com.docker.compose.project.config_files": "/srv/myapp/compose.yaml",
	})
	srv.Add("container", "c2", "myapp-worker", "myapp")
	srv.StopContainer("c2")
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	srv.SetVolumeSize("myapp_data", 500)
	e.put(t, live("myapp", mcpNow.Add(time.Hour)))
	// otherapp: stopped and marked never remove.
	srv.Add("container", "c3", "otherapp-web", "otherapp")
	srv.StopContainer("c3")
	zero := time.Duration(0)
	e.put(t, lease.Lease{Project: "otherapp", State: lease.StateStopped, StoppedAt: mcpNow.Add(-time.Hour), RemoveAfter: &zero, RemoveAfterFrom: "label"})
	// A stack dev-lease does not manage must not appear.
	srv.Add("container", "s1", "stranger-web", "stranger")

	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	// The schema for the checker comes from the live tools/list answer.
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listRes := s.read(t)
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_stacks","arguments":{}}}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("list_stacks answered a JSON-RPC error: %v", m)
	}
	res := m["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("list_stacks failed: %v", toolText(res))
	}
	if text := toolText(res); text != "myapp: live, stops in 1h\notherapp: stopped, never removed" {
		t.Errorf("text = %q, want one line per stack", text)
	}
	sc := res["structuredContent"].(map[string]any)
	stacks := sc["stacks"].([]any)
	if len(stacks) != 2 {
		t.Fatalf("stacks = %v, want the two managed stacks in the view's order", stacks)
	}
	myapp := stacks[0].(map[string]any)
	if myapp["name"] != "myapp" || myapp["path"] != "/srv/myapp" || myapp["state"] != "live" ||
		myapp["next"] != "stops in 1h" || myapp["next_at"] != "2026-10-20T13:00:00Z" ||
		myapp["never_remove"] != false || myapp["containers_running"] != float64(1) ||
		myapp["containers_total"] != float64(2) || myapp["disk_bytes"] != float64(500) {
		t.Errorf("myapp = %v", myapp)
	}
	holders := myapp["holders"].([]any)
	if len(holders) != 1 {
		t.Fatalf("myapp holders = %v, want the ci claim", holders)
	}
	h := holders[0].(map[string]any)
	if h["name"] != "ci" || h["until"] != "2026-10-20T13:00:00Z" {
		t.Errorf("myapp holder = %v", h)
	}
	otherapp := stacks[1].(map[string]any)
	if otherapp["name"] != "otherapp" || otherapp["state"] != "stopped" ||
		otherapp["next"] != "never removed" || otherapp["never_remove"] != true {
		t.Errorf("otherapp = %v", otherapp)
	}
	if _, has := otherapp["next_at"]; has {
		t.Errorf("otherapp has next_at %v, want it omitted for a zero time", otherapp["next_at"])
	}
	// The structured content fits the output schema the client sees.
	tools := listRes["result"].(map[string]any)["tools"].([]any)
	schema := tools[0].(map[string]any)["outputSchema"].(map[string]any)
	checkSchema(t, schema, res["structuredContent"], "structuredContent")
}

func TestStackDetailsTool(t *testing.T) {
	e := newToolEnv(t)
	srv := e.srv
	srv.AddLabels("image", "sha256:i1", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)
	srv.AddContainerWithUses("c1", "myapp-web", map[string]string{
		docker.ProjectLabel:                       "myapp",
		docker.ServiceLabel:                       "web",
		"com.docker.compose.project.working_dir":  "/srv/myapp",
		"com.docker.compose.project.config_files": "/srv/myapp/compose.yaml",
	}, "sha256:i1", nil, nil)
	srv.SetMemory("c1", 1000, 200)
	srv.AddContainerWithUses("c2", "myapp-db", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pg", nil, nil)
	srv.SetContainerImageRef("c2", "postgres:16-alpine")
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	srv.SetVolumeSize("myapp_data", 500)
	line := "2026-10-20T11:59:00.000000000Z hello from myapp\n"
	raw := make([]byte, 8+len(line))
	raw[0] = 1 // stdout
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(line)))
	copy(raw[8:], line)
	srv.SetLogs("c1", raw)

	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp","log_service":"web","log_lines":10}}}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("stack_details answered a JSON-RPC error: %v", m)
	}
	res := m["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("stack_details failed: %v", toolText(res))
	}
	sc := res["structuredContent"].(map[string]any)
	if sc["name"] != "myapp" || sc["state"] != "new" || sc["next"] != "stops in 2h" ||
		sc["next_at"] != "2026-10-20T14:00:00Z" || sc["never_remove"] != false ||
		sc["containers_running"] != float64(2) || sc["containers_total"] != float64(2) {
		t.Errorf("stack fields = %v", sc)
	}
	if sc["start_command"] != "cd /srv/myapp && docker compose -p myapp up -d" {
		t.Errorf("start_command = %v", sc["start_command"])
	}
	conts := sc["containers"].([]any)
	if len(conts) != 2 {
		t.Fatalf("containers = %v, want 2", conts)
	}
	byService := map[string]map[string]any{}
	for _, cv := range conts {
		c := cv.(map[string]any)
		byService[c["service"].(string)] = c
	}
	web := byService["web"]
	if web["state"] != "running" || web["status"] != "Up" || web["image"] != "sha256:i1" ||
		web["memory_bytes"] != float64(800) || web["kept"] != false {
		t.Errorf("web container = %v", web)
	}
	db := byService["db"]
	if db["state"] != "running" || db["image"] != "postgres:16-alpine" || db["memory_bytes"] != float64(0) {
		t.Errorf("db container = %v", db)
	}
	vols := sc["volumes"].([]any)
	if len(vols) != 1 || vols[0].(map[string]any)["name"] != "myapp_data" ||
		vols[0].(map[string]any)["bytes"] != float64(500) || vols[0].(map[string]any)["kept"] != false {
		t.Errorf("volumes = %v", vols)
	}
	if !sameStringsAny(sc["removed_images"].([]any), []string{"myapp-web:latest"}) {
		t.Errorf("removed_images = %v, want the used image", sc["removed_images"])
	}
	if !sameStringsAny(sc["kept_images"].([]any), []string{"postgres:16-alpine"}) {
		t.Errorf("kept_images = %v, want the pulled image as staying", sc["kept_images"])
	}
	logs := sc["logs"].([]any)
	if len(logs) != 1 {
		t.Fatalf("logs = %v, want the one stdout line", logs)
	}
	lg := logs[0].(map[string]any)
	if lg["time"] != "2026-10-20T11:59:00Z" || lg["stream"] != "stdout" || lg["text"] != "hello from myapp" {
		t.Errorf("log line = %v", lg)
	}
	if sc["log_service"] != "web" {
		t.Errorf("log_service = %v, want web", sc["log_service"])
	}

	// The structured content fits the output schema the client sees.
	s.send(t, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	listRes := s.read(t)
	var detSchema map[string]any
	for _, tv := range listRes["result"].(map[string]any)["tools"].([]any) {
		tool := tv.(map[string]any)
		if tool["name"] == "stack_details" {
			detSchema = tool["outputSchema"].(map[string]any)
		}
	}
	checkSchema(t, detSchema, res["structuredContent"], "structuredContent")

	// Without log_service there are no logs.
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp"}}}`)
	m = s.read(t)
	sc = m["result"].(map[string]any)["structuredContent"].(map[string]any)
	if len(sc["logs"].([]any)) != 0 || sc["log_service"] != "" {
		t.Errorf("without log_service: logs = %v, log_service = %v, want none", sc["logs"], sc["log_service"])
	}
	checkSchema(t, detSchema, m["result"].(map[string]any)["structuredContent"], "structuredContent")

	// log_service with the line count omitted means no logs too.
	s.send(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp","log_service":"web"}}}`)
	m = s.read(t)
	sc = m["result"].(map[string]any)["structuredContent"].(map[string]any)
	if len(sc["logs"].([]any)) != 0 || sc["log_service"] != "" {
		t.Errorf("log_service without log_lines: logs = %v, log_service = %v, want none", sc["logs"], sc["log_service"])
	}

	// log_lines above 200 is refused as a tool error.
	s.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp","log_service":"web","log_lines":201}}}`)
	m = s.read(t)
	if m["error"] != nil {
		t.Fatalf("log_lines 201 became a JSON-RPC error: %v", m)
	}
	res = m["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("log_lines 201 answered %v, want a tool error", res)
	}
	if got := toolText(res); got != "log_lines must be a whole number between 0 and 200" {
		t.Errorf("tool error text = %q", got)
	}
}

func TestStopStackToolGoesThroughControl(t *testing.T) {
	e := newToolEnv(t)
	e.srv.AddLabels("container", "c1", "myapp-web", map[string]string{
		docker.ProjectLabel:                      "myapp",
		"com.docker.compose.project.working_dir": "/srv/myapp",
	})
	path, got, received := fakeControl(t, control.Answer{OK: true})
	s := startSession(t, e.tools(t, path))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("stop_stack answered a JSON-RPC error: %v", m)
	}
	res := m["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("stop_stack failed: %v", toolText(res))
	}
	text := toolText(res)
	if !strings.Contains(text, "stopped myapp") || !strings.Contains(text, "nothing was deleted") {
		t.Errorf("stop_stack text = %q, want what stopped and that nothing was deleted", text)
	}
	if !strings.Contains(text, "docker compose -p myapp up -d") {
		t.Errorf("stop_stack text = %q, want the start command", text)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the control socket got no request")
	}
	if *got != (control.Request{Action: "stop", Project: "myapp"}) {
		t.Errorf("control request = %+v, want exactly a stop of myapp", *got)
	}
	if sc := res["structuredContent"].(map[string]any); sc["name"] != "myapp" {
		t.Errorf("structuredContent = %v, want the stack name", sc)
	}
}

func TestRemoveStackNeedsConfirm(t *testing.T) {
	path, dialed := silentSocket(t)
	s := startSession(t, newToolEnv(t).tools(t, path))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remove_stack","arguments":{"name":"myapp","confirm":"otherapp"}}}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("the confirm mismatch became a JSON-RPC error: %v", m)
	}
	res := m["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("remove_stack with a wrong confirm answered %v, want a tool error", res)
	}
	text := toolText(res)
	if !strings.Contains(text, "confirm") || !strings.Contains(text, "myapp") {
		t.Errorf("tool error text = %q, want the confirm refusal", text)
	}
	select {
	case <-dialed:
		t.Fatal("the control socket got a connection for a remove with a wrong confirm")
	case <-time.After(500 * time.Millisecond):
		// No connection: the refusal happened before any control call.
	}
}

// TestToolArgsValidation pins the argument validation: a call without a
// name is a tool error, never a panic or a control request, and a null or
// missing arguments object behaves like an empty one.
func TestToolArgsValidation(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"stop with empty arguments", "stop_stack", `{}`, "stop_stack needs the stack name"},
		{"stop without arguments", "stop_stack", "", "stop_stack needs the stack name"},
		{"stop with null arguments", "stop_stack", `null`, "stop_stack needs the stack name"},
		{"details with empty arguments", "stack_details", `{}`, "stack_details needs the stack name"},
		{"remove with empty arguments", "remove_stack", `{}`, "remove_stack needs the stack name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, dialed := silentSocket(t)
			s := startSession(t, newToolEnv(t).tools(t, path))
			defer s.close(t)
			var call string
			if tc.args == "" {
				call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q}}`, tc.tool)
			} else {
				call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tc.tool, tc.args)
			}
			s.send(t, call)
			m := s.read(t)
			if m["error"] != nil {
				t.Fatalf("the call became a JSON-RPC error: %v", m)
			}
			res := m["result"].(map[string]any)
			if res["isError"] != true {
				t.Fatalf("call answered %v, want a tool error", res)
			}
			if got := toolText(res); got != tc.want {
				t.Errorf("tool error text = %q, want %q", got, tc.want)
			}
			select {
			case <-dialed:
				t.Fatal("the control socket got a connection for a call with no name")
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// silentSocket is a control socket nobody listens on: a dial records the
// attempt, so tests can prove an action never reached the socket.
func silentSocket(t *testing.T) (path string, dialed chan struct{}) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	dialed = make(chan struct{}, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		conn.Close()
		dialed <- struct{}{}
	}()
	return path, dialed
}

func TestRemoveStackNeverRemoveIsError(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	path, _, received := fakeControl(t, control.Answer{OK: false, Code: control.CodeNeverRemove, Error: "refused by serve"})
	s := startSession(t, e.tools(t, path))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remove_stack","arguments":{"name":"myapp","confirm":"myapp"}}}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("the never-remove refusal became a JSON-RPC error: %v", m)
	}
	res := m["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("remove_stack on a never-remove stack answered %v, want a tool error", res)
	}
	if got := toolText(res); got != "myapp is marked never remove; remove it from the dashboard if you mean it" {
		t.Errorf("tool error text = %q", got)
	}
	<-received
}

// One case per answer code of the design, each with the exact text.
func TestRemoveRefusalCodes(t *testing.T) {
	cases := []struct {
		name      string
		code      string
		answerErr string
		want      string
	}{
		{"busy", control.CodeBusy, "busy now",
			"myapp is being stopped or removed right now; try again in a minute"},
		{"no_stack", control.CodeNoStack, "no stack",
			"dev-lease does not manage a stack named myapp"},
		{"not_managed", control.CodeNotManaged, "not managed",
			"dev-lease does not manage a stack named myapp"},
		{"not_removed", control.CodeNotRemoved, "myapp was not removed: a container is running again; see serve's log",
			"myapp was not removed: a container is running again; see serve's log"},
		{"shutting_down", control.CodeShuttingDown, "serve is going away",
			"dev-lease serve is shutting down; try again when it is back"},
		{"bad_request", control.CodeBadRequest, "stop needs the project name",
			"stop needs the project name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			e.srv.Add("container", "c1", "myapp-web", "myapp")
			path, _, received := fakeControl(t, control.Answer{OK: false, Code: tc.code, Error: tc.answerErr})
			s := startSession(t, e.tools(t, path))
			defer s.close(t)
			s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remove_stack","arguments":{"name":"myapp","confirm":"myapp"}}}`)
			m := s.read(t)
			if m["error"] != nil {
				t.Fatalf("the %s refusal became a JSON-RPC error: %v", tc.code, m)
			}
			res := m["result"].(map[string]any)
			if res["isError"] != true {
				t.Fatalf("remove_stack answered %v, want a tool error", res)
			}
			if got := toolText(res); got != tc.want {
				t.Errorf("tool error text = %q, want %q", got, tc.want)
			}
			<-received
		})
	}
}

func TestServeNotRunning(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	path := filepath.Join(t.TempDir(), "no-control.sock") // nothing listens here
	s := startSession(t, e.tools(t, path))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
	m := s.read(t)
	res := m["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("stop_stack without serve answered %v, want a tool error", res)
	}
	want := fmt.Sprintf("dev-lease serve is not running (no control socket at %s); start it and try again", path)
	if got := toolText(res); got != want {
		t.Errorf("tool error text = %q, want %q", got, want)
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	// An unknown tool name is invalid params.
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool"}}`)
	if code := rpcErrorCode(t, s.read(t)); code != -32602 {
		t.Errorf("unknown tool error code = %d, want -32602", code)
	}
	// An unknown method is -32601.
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"no_such_method"}`)
	if code := rpcErrorCode(t, s.read(t)); code != -32601 {
		t.Errorf("unknown method error code = %d, want -32601", code)
	}
	// A request without jsonrpc 2.0 is an invalid request.
	s.send(t, `{"id":3,"method":"ping"}`)
	if code := rpcErrorCode(t, s.read(t)); code != -32600 {
		t.Errorf("missing jsonrpc error code = %d, want -32600", code)
	}
	// A request without a method is an invalid request.
	s.send(t, `{"jsonrpc":"2.0","id":4}`)
	if code := rpcErrorCode(t, s.read(t)); code != -32600 {
		t.Errorf("missing method error code = %d, want -32600", code)
	}
	// tools/call params that are not an object are invalid params.
	s.send(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":"x"}`)
	if code := rpcErrorCode(t, s.read(t)); code != -32602 {
		t.Errorf("non-object params error code = %d, want -32602", code)
	}
}

func TestMalformedJSON(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	s.send(t, "this is not json")
	m := s.read(t)
	e, ok := m["error"].(map[string]any)
	if !ok || int(e["code"].(float64)) != -32700 {
		t.Fatalf("malformed line answered %v, want -32700", m)
	}
	if m["id"] != nil {
		t.Errorf("parse error id = %v, want null", m["id"])
	}
	// The next good line still works.
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	m = s.read(t)
	if m["id"] != float64(2) || m["error"] != nil {
		t.Errorf("ping after a bad line answered %v", m)
	}
}

func TestNotificationsGetNoAnswer(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	m := s.read(t)
	if m["id"] != float64(1) {
		t.Fatalf("first answer = %v, want the ping: a notification must never be answered", m)
	}
}

func TestConcurrentCallsAnswerEach(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	path := filepath.Join(t.TempDir(), "control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	release := make(chan struct{})
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req control.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		<-release // the stop stays in flight until the test says so
		_ = json.NewEncoder(conn).Encode(control.Answer{OK: true})
	}()

	s := startSession(t, e.tools(t, path))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	first := s.read(t)
	if first["id"] != float64(2) {
		t.Fatalf("first answer = %v, want the ping while the stop is in flight", first)
	}
	close(release)
	second := s.read(t)
	if second["id"] != float64(1) {
		t.Fatalf("second answer = %v, want the stop's answer", second)
	}
	if res, ok := second["result"].(map[string]any); !ok || res["isError"] == true {
		t.Fatalf("stop_stack answer = %v, want success", second)
	}
}

// checkSchema walks one JSON Schema object and checks the value against
// it: every required key is present and every value's type matches the
// schema's properties, recursing into arrays and objects.
func checkSchema(t *testing.T, schema map[string]any, value any, path string) {
	t.Helper()
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %v is not an object", path, value)
	}
	props, _ := schema["properties"].(map[string]any)
	for _, r := range stringsAny(schema["required"]) {
		if _, ok := obj[r]; !ok {
			t.Errorf("%s: required key %q is missing", path, r)
		}
	}
	for key, pv := range obj {
		ps, ok := props[key].(map[string]any)
		if !ok {
			continue
		}
		checkType(t, ps, pv, path+"."+key)
	}
}

func checkType(t *testing.T, schema map[string]any, value any, path string) {
	t.Helper()
	switch schema["type"] {
	case "string":
		if _, ok := value.(string); !ok {
			t.Errorf("%s = %v is not a string", path, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			t.Errorf("%s = %v is not a boolean", path, value)
		}
	case "integer":
		if _, ok := value.(float64); !ok {
			t.Errorf("%s = %v is not a number", path, value)
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			t.Errorf("%s = %v is not an array", path, value)
			return
		}
		items, _ := schema["items"].(map[string]any)
		for i, v := range arr {
			checkType(t, items, v, fmt.Sprintf("%s[%d]", path, i))
		}
	case "object":
		checkSchema(t, schema, value, path)
	}
}

func stringsAny(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.(string))
	}
	return out
}

func sameStringsAny(got []any, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, w := range want {
		seen[w]++
	}
	for _, g := range got {
		s, _ := g.(string)
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}

// The serve context cancels a tool in flight: a list against a daemon
// that never answers ends instead of hanging.
func TestServeContextCancelsTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// A control socket that accepts but never answers: the stop blocks.
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	path := filepath.Join(t.TempDir(), "control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req control.Request
		_ = json.NewDecoder(conn).Decode(&req)
		<-stop // never answers until the test ends
	}()
	inR, inW := io.Pipe()
	stopped := make(chan error, 1)
	go func() { stopped <- Serve(ctx, inR, io.Discard, e.tools(t, path)) }()
	t.Cleanup(func() {
		cancel()
		inW.Close()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
		}
	})
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	// Give the stop time to reach the socket, then cancel everything.
	time.Sleep(300 * time.Millisecond)
	cancel()
	inW.Close()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the serve context was cancelled")
	}
}

// delayedSocket is a control socket that answers one request after d.
func delayedSocket(t *testing.T, d time.Duration, ans control.Answer) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req control.Request
		_ = json.NewDecoder(conn).Decode(&req)
		time.Sleep(d)
		_ = json.NewEncoder(conn).Encode(ans)
	}()
	return path
}

// The action's only limit is the tool context: a stop answered inside it
// succeeds, one answered after it reports a timeout, and the text never
// claims serve is not running.
func TestStopStackTimeoutTexts(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	old := actionTimeout
	actionTimeout = 2 * time.Second
	defer func() { actionTimeout = old }()

	t.Run("answer after 1s gives the stop success", func(t *testing.T) {
		path := delayedSocket(t, time.Second, control.Answer{OK: true})
		s := startSession(t, e.tools(t, path))
		defer s.close(t)
		s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
		res := s.read(t)["result"].(map[string]any)
		if res["isError"] == true {
			t.Fatalf("stop_stack failed: %v", toolText(res))
		}
		if text := toolText(res); !strings.Contains(text, "stopped myapp") {
			t.Errorf("text = %q, want the stop success", text)
		}
	})

	t.Run("answer after 3s gives the timeout text", func(t *testing.T) {
		path := delayedSocket(t, 3*time.Second, control.Answer{OK: true})
		s := startSession(t, e.tools(t, path))
		defer s.close(t)
		s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
		res := s.read(t)["result"].(map[string]any)
		if res["isError"] != true {
			t.Fatalf("stop_stack after 3s with a 2s limit answered %v, want a tool error", res)
		}
		text := toolText(res)
		if !strings.Contains(text, "did not answer within") || strings.Contains(text, "not running") {
			t.Errorf("tool error text = %q, want the timeout text, not the not-running one", text)
		}
	})
}

// A client may add keys next to name and arguments (the MCP _meta
// convention): the strict decode applies to arguments only.
func TestCallWithMetaRuns(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_stacks","arguments":{},"_meta":{"progressToken":7}}}`)
	m := s.read(t)
	if m["error"] != nil {
		t.Fatalf("tools/call with _meta answered a JSON-RPC error: %v", m)
	}
	if res := m["result"].(map[string]any); res["isError"] == true {
		t.Fatalf("tools/call with _meta failed: %v", toolText(res))
	}
}

// The strict decode of arguments stays: an unknown key inside arguments
// is invalid params. A real tool environment backs the call, so a lenient
// decode that ran the tool instead of refusing would answer success and
// fail the check below.
func TestCallRefusesUnknownArgument(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_stacks","arguments":{"stacks":true}}}`)
	if code := rpcErrorCode(t, s.read(t)); code != -32602 {
		t.Errorf("error code = %d, want -32602", code)
	}
}

// One line longer than the 4 MiB limit is answered with -32600 (id null,
// "request too large") and the session goes on with the next line.
func TestOverlongLineAnsweredAndSessionGoesOn(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+strings.Repeat("a", 5<<20)+`"}}`)
	m := s.read(t)
	if code := rpcErrorCode(t, m); code != -32600 {
		t.Errorf("error code = %d, want -32600", code)
	}
	if m["id"] != nil {
		t.Errorf("id = %v, want null", m["id"])
	}
	if msg := m["error"].(map[string]any)["message"]; msg != "request too large" {
		t.Errorf("error message = %v, want \"request too large\"", msg)
	}
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if m := s.read(t); m["id"] != float64(2) || m["error"] != nil {
		t.Errorf("ping after the over-long line answered %v", m)
	}
}

// log_lines omitted (0) means no logs, also when log_service is given.
func TestLogServiceWithoutLinesGivesNoLogs(t *testing.T) {
	e := newToolEnv(t)
	e.srv.AddLabels("container", "c1", "myapp-web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	line := "2026-10-20T11:59:00.000000000Z one\n"
	raw := make([]byte, 8+len(line))
	raw[0] = 1
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(line)))
	copy(raw[8:], line)
	e.srv.SetLogs("c1", raw)
	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp","log_service":"web"}}}`)
	sc := s.read(t)["result"].(map[string]any)["structuredContent"].(map[string]any)
	if logs := sc["logs"].([]any); len(logs) != 0 {
		t.Errorf("log_lines omitted but logs = %v, want none", logs)
	}
}

// The stack's compose networks appear in the details.
func TestStackDetailsNetworks(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	e.srv.Add("network", "myapp_default", "myapp_default", "myapp")
	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp"}}}`)
	sc := s.read(t)["result"].(map[string]any)["structuredContent"].(map[string]any)
	if !sameStringsAny(sc["networks"].([]any), []string{"myapp_default"}) {
		t.Errorf("networks = %v, want the project network", sc["networks"])
	}
}

// Every array in the structured content is an array on the wire, never
// null: the output schema says type array.
func TestDetailsNullArraysNeverReachTheClient(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp") // no images, no volumes, no networks
	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp"}}}`)
	sc := s.read(t)["result"].(map[string]any)["structuredContent"].(map[string]any)
	for _, k := range []string{"removed_images", "kept_images", "networks", "containers", "volumes", "logs", "holders"} {
		if _, ok := sc[k].([]any); !ok {
			b, _ := json.Marshal(sc[k])
			t.Errorf("structuredContent.%s = %s; outputSchema says type array", k, b)
		}
	}
}

// next_at, until and log times are UTC on the wire whatever zone the
// clock and the daemon's log timestamps use.
func TestTimesInUTC(t *testing.T) {
	e := newToolEnv(t)
	e.srv.AddLabels("container", "c1", "myapp-web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	line := "2026-10-20T11:59:00.000000000+03:00 one\n"
	raw := make([]byte, 8+len(line))
	raw[0] = 1
	binary.BigEndian.PutUint32(raw[4:8], uint32(len(line)))
	copy(raw[8:], line)
	e.srv.SetLogs("c1", raw)
	e.put(t, live("myapp", time.Date(2026, 10, 20, 16, 0, 0, 0, time.FixedZone("EEST", 3*3600))))
	tools := e.tools(t, "")
	tools.Now = func() time.Time { return mcpNow.In(time.FixedZone("EEST", 3*3600)) }
	s := startSession(t, tools)
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_stacks","arguments":{}}}`)
	st := s.read(t)["result"].(map[string]any)["structuredContent"].(map[string]any)["stacks"].([]any)[0].(map[string]any)
	if na, _ := st["next_at"].(string); !strings.HasSuffix(na, "Z") {
		t.Errorf("next_at = %q, want UTC", na)
	}
	if hu := st["holders"].([]any)[0].(map[string]any)["until"].(string); hu != "2026-10-20T13:00:00Z" {
		t.Errorf("holder until = %q, want the UTC rendering", hu)
	}
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stack_details","arguments":{"name":"myapp","log_service":"web","log_lines":10}}}`)
	lg := s.read(t)["result"].(map[string]any)["structuredContent"].(map[string]any)["logs"].([]any)[0].(map[string]any)
	if lt, _ := lg["time"].(string); lt != "2026-10-20T08:59:00Z" {
		t.Errorf("log time = %q, want the UTC rendering", lt)
	}
}

// Valid JSON that is not an object, and ids that are neither a string
// nor an integer, are invalid requests answered with id null.
func TestNonObjectAndBadIDsAreInvalidRequests(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	cases := []struct {
		name string
		line string
	}{
		{"an array", `[1,2]`},
		{"a number", `7`},
		{"null", `null`},
		{"id null", `{"jsonrpc":"2.0","id":null,"method":"ping"}`},
		{"id an object", `{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`},
		{"id an array", `{"jsonrpc":"2.0","id":[1],"method":"ping"}`},
		{"id a fraction", `{"jsonrpc":"2.0","id":1.5,"method":"ping"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.send(t, tc.line)
			m := s.read(t)
			if code := rpcErrorCode(t, m); code != -32600 {
				t.Errorf("error code = %d, want -32600", code)
			}
			if m["id"] != nil {
				t.Errorf("id = %v, want null", m["id"])
			}
		})
	}
	// A string id is valid and echoed.
	s.send(t, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	if m := s.read(t); m["id"] != "abc" || m["error"] != nil {
		t.Errorf("string id answer = %v, want the ping result", m)
	}
}

// Stop results get their own texts: failed container stops are named,
// and a stop whose listing failed says it did not finish and that the
// next sweep retries it.
func TestStopReportTexts(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  reaper.Report
		want []string
	}{
		{"failed container stop", reaper.Report{Failed: []string{"web"}}, []string{"stopped myapp", "could not stop: web"}},
		{"container listing failed", reaper.Report{Failed: []string{"myapp"}}, []string{"the stop of myapp did not finish", "the next sweep retries it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			e.srv.Add("container", "c1", "myapp-web", "myapp")
			path, _, received := fakeControl(t, control.Answer{OK: true, Report: &tc.rep})
			s := startSession(t, e.tools(t, path))
			defer s.close(t)
			s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
			res := s.read(t)["result"].(map[string]any)
			text := toolText(res)
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("text = %q, want %q in it", text, w)
				}
			}
			<-received
		})
	}
}

// Unmanaged names and names nothing exists for are refused before any
// control call: no connection to the socket is made.
func TestUnmanagedAndUnknownMakeNoControlCall(t *testing.T) {
	for _, tc := range []struct{ name, call string }{
		{"unmanaged stop", `{"name":"stop_stack","arguments":{"name":"stranger"}}`},
		{"unmanaged remove", `{"name":"remove_stack","arguments":{"name":"stranger","confirm":"stranger"}}`},
		{"no stack stop", `{"name":"stop_stack","arguments":{"name":"otherapp"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			e.srv.Add("container", "s1", "stranger-web", "stranger")
			path, dialed := silentSocket(t)
			s := startSession(t, e.tools(t, path))
			defer s.close(t)
			s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+tc.call+`}`)
			res := s.read(t)["result"].(map[string]any)
			if res["isError"] != true {
				t.Fatalf("%s answered %v, want a tool error", tc.name, res)
			}
			if got := toolText(res); !strings.Contains(got, "does not manage a stack named") {
				t.Errorf("%s text = %q, want the not-managed refusal", tc.name, got)
			}
			select {
			case <-dialed:
				t.Errorf("the control socket got a connection for %s", tc.name)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// Cancelling Serve's context returns from Serve even with the input
// open: the stdin reader runs in its own goroutine.
func TestServeReturnsOnCancelWithOpenInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	defer inW.Close()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, inR, io.Discard, Tools{Version: "t"}) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return within 1s of its context being cancelled (input still open)")
	}
}

// EOF with requests in flight: every request gets exactly one whole
// answer line and Serve returns nil; three runs to catch orderings,
// under -race.
func TestEOFDrainsAnswersInFlight(t *testing.T) {
	for run := 1; run <= 3; run++ {
		t.Run(fmt.Sprintf("run %d", run), func(t *testing.T) {
			var in strings.Builder
			for i := 0; i < 300; i++ {
				fmt.Fprintf(&in, `{"jsonrpc":"2.0","id":%d,"method":"tools/list"}`+"\n", i)
			}
			outR, outW := io.Pipe()
			done := make(chan error, 1)
			go func() { done <- Serve(context.Background(), strings.NewReader(in.String()), outW, Tools{Version: "t"}) }()
			seen := map[int]bool{}
			sc := bufio.NewScanner(outR)
			sc.Buffer(nil, 1<<20)
			for sc.Scan() {
				var m struct {
					ID int `json:"id"`
				}
				if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
					t.Fatalf("not one whole JSON line: %v", err)
				}
				if seen[m.ID] {
					t.Fatalf("id %d answered twice", m.ID)
				}
				seen[m.ID] = true
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(seen) != 300 {
				t.Fatalf("answered %d of 300", len(seen))
			}
		})
	}
}

// A socket file with nothing listening behind it is the same "serve is
// not running" as a missing one: dialling refuses with ECONNREFUSED and
// the text stays the same.
func TestServeNotRunningDeadSocket(t *testing.T) {
	e := newToolEnv(t)
	e.srv.Add("container", "c1", "myapp-web", "myapp")
	path := filepath.Join(t.TempDir(), "dead.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.Close() // the socket file stays, the listener is gone
	s := startSession(t, e.tools(t, path))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
	m := s.read(t)
	res := m["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("stop_stack against a dead socket answered %v, want a tool error", res)
	}
	want := fmt.Sprintf("dev-lease serve is not running (no control socket at %s); start it and try again", path)
	if got := toolText(res); got != want {
		t.Errorf("tool error text = %q, want %q", got, want)
	}
}

// The id is validated before the version: a request with an invalid id
// answers with id null even when its jsonrpc is wrong too, and the wrong
// version is never used to echo an id that is not a string or an integer.
func TestInvalidIDAnswersNullEvenWithBadVersion(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	s.send(t, `{"jsonrpc":"1.0","id":[1,2],"method":"ping"}`)
	m := s.read(t)
	if code := rpcErrorCode(t, m); code != -32600 {
		t.Errorf("error code = %d, want -32600", code)
	}
	if m["id"] != nil {
		t.Errorf("id = %v, want null: an invalid id must never be echoed", m["id"])
	}
	// A valid id with a bad version still comes back in the error.
	s.send(t, `{"jsonrpc":"1.0","id":"abc","method":"ping"}`)
	m = s.read(t)
	if code := rpcErrorCode(t, m); code != -32600 {
		t.Errorf("error code = %d, want -32600", code)
	}
	if m["id"] != "abc" {
		t.Errorf("id = %v, want \"abc\" echoed", m["id"])
	}
}

// rawSession serves one whole raw input (possibly ending without a
// newline) and returns every answer line as a decoded object. Each read
// carries a deadline, so a dropped answer fails the test instead of
// hanging it.
func pipeSession(t *testing.T, tools Tools, input string) []map[string]any {
	t.Helper()
	inR, inW := net.Pipe()
	outR, outW := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), inR, outW, tools) }()
	if _, err := io.WriteString(inW, input); err != nil {
		t.Fatalf("write the input: %v", err)
	}
	if err := inW.Close(); err != nil {
		t.Fatalf("close the input: %v", err)
	}
	out := bufio.NewReader(outR)
	var ms []map[string]any
	for {
		// A closed pipe answers the deadline set with an error: the loop
		// ends through ReadString's EOF in that case.
		_ = outR.SetReadDeadline(time.Now().Add(sessionDeadline))
		line, err := out.ReadString('\n')
		if err != nil {
			break
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("answer %q is not one whole JSON object: %v", line, err)
		}
		ms = append(ms, m)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the input ended")
	}
	return ms
}

// pingWithID builds a ping request exactly n bytes long (content bytes,
// no trailing newline), padded into params.
func pingWithID(id, n int) string {
	prefix := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping","pad":`, id)
	// The suffix after the pad is the closing quote and brace: 3 bytes.
	if len(prefix)+3 > n {
		panic("ping prefix alone is longer than the requested line")
	}
	return prefix + `"` + strings.Repeat("a", n-len(prefix)-3) + `"}`
}

// The 4 MiB line limit counts content bytes: the longest allowed line
// (the trailing newline included and the final line without one included)
// is answered, one byte more is "request too large", and the session goes
// on with the next line either way.
func TestLineLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		wantIDs []any // nil marks the "request too large" error answer
	}{
		{"content max-1 then newline", pingWithID(1, maxRequestBytes-1) + "\n", []any{float64(1)}},
		{"content exactly max then newline", pingWithID(2, maxRequestBytes) + "\n", []any{float64(2)}},
		{"content max+1 then newline", pingWithID(3, maxRequestBytes+1) + "\n", []any{nil}},
		{"content exactly max at EOF", pingWithID(4, maxRequestBytes), []any{float64(4)}},
		{"overlong at EOF, no newline", pingWithID(5, maxRequestBytes+100), []any{nil}},
		{"overlong then a ping", pingWithID(6, maxRequestBytes+100) + "\n" + `{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n", []any{nil, float64(7)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := pipeSession(t, Tools{Version: "test"}, tc.input)
			if len(ms) != len(tc.wantIDs) {
				t.Fatalf("answers = %v, want %d", ms, len(tc.wantIDs))
			}
			for i, m := range ms {
				if tc.wantIDs[i] == nil {
					if code := rpcErrorCode(t, m); code != -32600 {
						t.Errorf("answer %d error code = %d, want -32600", i, code)
					}
					if m["id"] != nil {
						t.Errorf("answer %d id = %v, want null", i, m["id"])
					}
					if msg := m["error"].(map[string]any)["message"]; msg != "request too large" {
						t.Errorf("answer %d error message = %v, want \"request too large\"", i, msg)
					}
					continue
				}
				if m["id"] != tc.wantIDs[i] {
					t.Errorf("answer %d id = %v, want %v", i, m["id"], tc.wantIDs[i])
				}
				if m["error"] != nil {
					t.Errorf("answer %d error = %v, want none", i, m["error"])
				}
			}
		})
	}
}

// A managed name with a lease but nothing on the daemon (extend ran
// before compose up) is not unmanaged: the tool says nothing is on the
// daemon. A name with neither lease nor daemon objects is the unmanaged
// text.
func TestActLeaseOnlyAndUnknownNames(t *testing.T) {
	e := newToolEnv(t)
	e.put(t, live("otherapp", mcpNow.Add(time.Hour)))
	s := startSession(t, e.tools(t, ""))
	defer s.close(t)
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"otherapp"}}}`)
	res := s.read(t)["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("stop_stack of a lease-only name answered %v, want a tool error", res)
	}
	if got := toolText(res); got != "nothing of otherapp is on the daemon; there is nothing to stop" {
		t.Errorf("tool error text = %q", got)
	}
	s.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remove_stack","arguments":{"name":"otherapp","confirm":"otherapp"}}}`)
	res = s.read(t)["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("remove_stack of a lease-only name answered %v, want a tool error", res)
	}
	if got := toolText(res); got != "nothing of otherapp is on the daemon; there is nothing to remove" {
		t.Errorf("tool error text = %q", got)
	}
	s.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
	res = s.read(t)["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("stop_stack of a nowhere name answered %v, want a tool error", res)
	}
	if got := toolText(res); got != "dev-lease does not manage a stack named myapp" {
		t.Errorf("tool error text = %q", got)
	}
}
