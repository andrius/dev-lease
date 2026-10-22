package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
)

// TestMCPSessionSubprocess runs the built binary as one whole MCP
// session: initialize, tools/list, list_stacks and stop_stack over the
// real stdin and stdout, against the fake daemon and a fake control
// socket, with the same environment the client commands use.
func TestMCPSessionSubprocess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dev-lease")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	srv := fakedocker.New(t)
	srv.AddLabels("container", "c1", "myapp-web", map[string]string{
		docker.ProjectLabel:                       "myapp",
		"com.docker.compose.project.working_dir":  "/srv/myapp",
		"com.docker.compose.project.config_files": "/srv/myapp/compose.yaml",
	})

	st := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	if err := st.UpdateHeader(func(h *lease.Header) error {
		h.Patterns = []string{"myapp"}
		h.DefaultTTL = 2 * time.Hour
		h.RemoveAfter = 48 * time.Hour
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The control socket the subprocess dials for stop_stack: one request
	// in, one ok answer out.
	sock := control.SocketPath(st.Path)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	reqCh := make(chan control.Request, 1)
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
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(control.Answer{OK: true})
	}()

	cmd := exec.Command(bin, "mcp", "--socket", srv.Socket)
	cmd.Env = append(cleanEnv("DEV_LEASE_STATE", "DEV_LEASE_OWN_ID"), "DEV_LEASE_STATE="+st.Path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(stdout)
	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			t.Fatalf("write to the subprocess stdin: %v", err)
		}
	}
	read := func(id int) map[string]any {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read an answer: %v (stderr: %s)", err, stderr.String())
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line %q is not JSON: %v", line, err)
		}
		if m["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line %q is not a JSON-RPC answer", line)
		}
		if m["id"] != float64(id) {
			t.Fatalf("answer id = %v, want %d (line %q)", m["id"], id, line)
		}
		return m
	}

	// initialize: the protocol version is ours whatever the client asked.
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`)
	init := read(1)
	res, ok := init["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result = %v", init)
	}
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want 2025-06-18", res["protocolVersion"])
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != "dev-lease" {
		t.Errorf("serverInfo.name = %v, want dev-lease", info["name"])
	}

	// notifications/initialized must get no answer.
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// tools/list: the four tools.
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listRes := read(2)["result"].(map[string]any)
	tools, _ := listRes["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools = %v, want four", tools)
	}

	// list_stacks: the fake daemon's myapp.
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_stacks","arguments":{}}}`)
	call := read(3)["result"].(map[string]any)
	if call["isError"] == true {
		t.Fatalf("list_stacks failed: %v", call["content"])
	}
	sc := call["structuredContent"].(map[string]any)
	stacks, _ := sc["stacks"].([]any)
	if len(stacks) != 1 {
		t.Fatalf("stacks = %v, want exactly myapp", stacks)
	}
	stack := stacks[0].(map[string]any)
	if stack["name"] != "myapp" || stack["state"] != "new" || stack["next"] != "stops in 2h" {
		t.Errorf("stack = %v, want the new myapp with the default TTL", stack)
	}

	// stop_stack: the control socket records the request, serve answers ok.
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"stop_stack","arguments":{"name":"myapp"}}}`)
	stop := read(4)["result"].(map[string]any)
	if stop["isError"] == true {
		t.Fatalf("stop_stack failed: %v", stop["content"])
	}
	if text := toolTextAny(stop); !strings.Contains(text, "stopped myapp") {
		t.Errorf("stop_stack text = %q", text)
	}
	select {
	case req := <-reqCh:
		if req != (control.Request{Action: "stop", Project: "myapp"}) {
			t.Errorf("control request = %+v, want exactly a stop of myapp", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the control socket got no request")
	}

	// End of session: closing stdin ends the server, and stdout must hold
	// nothing but the four JSON-RPC answers already read.
	stdin.Close()
	var extraLine string
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if extraLine == "" && strings.TrimSpace(line) != "" {
				extraLine = line
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dev-lease mcp exited with %v; stderr: %s", err, stderr.String())
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("dev-lease mcp did not exit after stdin closed; stderr: %s", stderr.String())
	}
	<-readerDone
	if extraLine != "" {
		t.Fatalf("stdout carries a non-JSON-RPC line after the session: %q", extraLine)
	}
}

func cleanEnv(drop ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		dropIt := false
		for _, d := range drop {
			if strings.HasPrefix(kv, d+"=") {
				dropIt = true
				break
			}
		}
		if !dropIt {
			out = append(out, kv)
		}
	}
	return out
}

func toolTextAny(res map[string]any) string {
	content, _ := res["content"].([]any)
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

// SIGTERM ends `dev-lease mcp` even while the client keeps stdin open:
// the server's lifecycle belongs to the signal, not to the client's
// stdin.
func TestMCPExitsOnSIGTERMWithOpenStdin(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dev-lease")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	srv := fakedocker.New(t)
	st := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	if err := st.UpdateHeader(func(h *lease.Header) error {
		h.Patterns = []string{"myapp"}
		h.DefaultTTL = 2 * time.Hour
		h.RemoveAfter = 48 * time.Hour
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mcp", "--socket", srv.Socket, "--state", st.Path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// One round trip proves the server is up, then SIGTERM with stdin
	// still open must end the process.
	io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Errorf("dev-lease mcp (pid %d) still runs 3 s after SIGTERM with stdin open", cmd.Process.Pid)
		stdin.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
}

// `dev-lease mcp` against a state file without managed project patterns
// exits with code 1 and names the problem: there is nothing to manage, so
// the session must not start.
func TestMCPRefusesStateWithoutPatterns(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dev-lease")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	srv := fakedocker.New(t)
	st := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	if err := st.UpdateHeader(func(h *lease.Header) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mcp", "--socket", srv.Socket, "--state", st.Path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var exitErr *exec.ExitError
	select {
	case err := <-done:
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("exit = %v, want code 1", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("dev-lease mcp with no managed patterns kept running, want exit code 1")
	}
	if got := stderr.String(); !strings.Contains(got, "has no managed project patterns") {
		t.Errorf("stderr = %q, want the missing-patterns refusal", got)
	}
}

// A second SIGTERM kills the process even when the handler is stuck on a
// blocked stdout write: the first signal cancels the context and the
// second falls through to the default disposition.
func TestMCPSecondSIGTERMKillsBlockedStdout(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dev-lease")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	srv := fakedocker.New(t)
	st := lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	if err := st.UpdateHeader(func(h *lease.Header) error {
		h.Patterns = []string{"myapp"}
		h.DefaultTTL = 2 * time.Hour
		h.RemoveAfter = 48 * time.Hour
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mcp", "--socket", srv.Socket, "--state", st.Path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Flood the server with tools/list requests and never read stdout:
	// the answers fill the pipe (a tools/list answer is far larger than
	// its request), so the server ends up blocked mid-write.
	go func() {
		req := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
		for {
			if _, err := io.WriteString(stdin, req); err != nil {
				return
			}
		}
	}()
	time.Sleep(700 * time.Millisecond) // the pipe is full, the writer blocked
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the first signal cancels, the shutdown is stuck
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// Exited on its own: the second signal ended a process the first
		// one could not, because the shutdown was blocked on stdout.
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-done
		stdin.Close()
		stdout.Close()
		t.Fatal("dev-lease mcp survived a second SIGTERM while its stdout was blocked")
	}
}
