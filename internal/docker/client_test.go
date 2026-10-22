package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/fakedocker"
)

func TestAllowlistRefusesEverythingElse(t *testing.T) {
	refused := []struct{ method, path string }{
		// Actions the tool must never take: create, exec, build, pull, push,
		// start, attach, events, info, and deletes without the exact flags.
		{"POST", "/v1.47/containers/create"},
		{"POST", "/v1.47/containers/abc/exec"},
		{"POST", "/v1.47/containers/abc/start"},
		{"POST", "/v1.47/build"},
		{"POST", "/v1.47/images/create"},
		{"POST", "/v1.47/images/abc/push"},
		{"GET", "/v1.47/events"},
		{"GET", "/v1.47/info"},
		{"GET", "/v1.47/networks/n1"},
		{"POST", "/v1.47/networks/create"},
		{"POST", "/v1.47/volumes/create"},
		{"GET", "/v1.47/containers/abc"},
		{"PUT", "/v1.47/containers/abc/start"},
		{"POST", "/version"},
		{"GET", "/containers/json"},
		{"GET", "/v1.47.1/containers/json"},
		{"GET", "/v1/containers/json"},
		// force must be absent or false; v=1 is mandatory.
		{"DELETE", "/v1.47/containers/abc?v=1&force=1"},
		{"DELETE", "/v1.47/containers/abc?force=1&v=1"},
		{"DELETE", "/v1.47/containers/abc?force=yes&v=1"},
		{"DELETE", "/v1.47/containers/abc?force=true&v=1"},
		{"DELETE", "/v1.47/containers/abc?force=TRUE&v=1"},
		{"DELETE", "/v1.47/containers/abc"},
		{"DELETE", "/v1.47/containers/abc?force=0"},
		// Unknown query parameters are banned on restricted endpoints.
		{"POST", "/v1.47/containers/abc/stop?t=abc"},
		{"GET", "/v1.47/containers/json?all=true"},
		{"GET", "/v1.47/containers/json?all=yes"},
		{"DELETE", "/v1.47/volumes"},
		{"DELETE", "/v1.47/networks"},
		{"GET", "/version/extra"},
	}
	for _, tc := range refused {
		if allowed(tc.method, tc.path) {
			t.Errorf("allowed(%s %s) = true, want false", tc.method, tc.path)
		}
	}

	allowedPaths := []struct{ method, path string }{
		{"GET", "/version"},
		{"GET", "/v1.47/containers/json"},
		{"GET", "/v1.47/containers/json?all=1"},
		{"GET", "/v1.47/containers/json?all=1&filters=%7B%22label%22%3A%5B%22com.docker.compose.project%3Dx%22%5D%7D"},
		{"GET", "/v1.47/containers/json?filters=%7B%22label%22%3A%5B%22com.docker.compose.project%3Dx%22%5D%7D"},
		{"GET", "/v1.47/containers/abc/json"},
		{"POST", "/v1.47/containers/abc/stop"},
		{"POST", "/v1.47/containers/abc/stop?t=10"},
		{"DELETE", "/v1.47/containers/abc?v=1&force=0"},
		{"DELETE", "/v1.47/containers/abc?force=0&v=1"},
		{"DELETE", "/v1.47/containers/abc?force=false&v=1"},
		{"DELETE", "/v1.47/containers/abc?v=1"},
		{"GET", "/v1.47/networks"},
		{"GET", "/v1.47/networks?filters=%7B%22label%22%3A%5B%22com.docker.compose.project%3Dx%22%5D%7D"},
		{"DELETE", "/v1.47/networks/n1"},
		{"GET", "/v1.47/volumes"},
		{"GET", "/v1.47/volumes?filters=%7B%22label%22%3A%5B%22com.docker.compose.project%3Dx%22%5D%7D"},
		{"DELETE", "/v1.47/volumes/vol1"},
		{"GET", "/v1.47/images/json"},
		{"GET", "/v1.47/images/json?filters=%7B%22label%22%3A%5B%22com.docker.compose.project%3Dx%22%5D%7D"},
		{"DELETE", "/v1.47/images/sha256%3A1234"},
	}
	for _, tc := range allowedPaths {
		if !allowed(tc.method, tc.path) {
			t.Errorf("allowed(%s %s) = false, want true", tc.method, tc.path)
		}
	}
}

func TestDisallowedRequestNeverTouchesSocket(t *testing.T) {
	srv := fakedocker.New(t)
	c := New(srv.Socket)
	_, err := c.request(context.Background(), "POST", "/v1.47/containers/create")
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("error = %v, want ErrNotAllowed", err)
	}
	if got := len(srv.Calls()); got != 0 {
		t.Fatalf("fake daemon saw %d calls, want 0", got)
	}
}

func TestListsFilterByProjectLabel(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.Add("container", "c2", "db", "other")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("network", "n2", "other_default", "other")
	srv.Add("volume", "v1", "myapp_data", "myapp")
	srv.Add("volume", "v2", "other_data", "other")
	srv.Add("image", "sha256:i1", "myapp-web:latest", "myapp")
	srv.Add("image", "sha256:i2", "other-db:latest", "other")

	c := New(srv.Socket)
	ctx := context.Background()

	projects, err := c.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !projects["myapp"] || !projects["other"] || projects["ghost"] {
		t.Fatalf("Projects() = %v, want myapp and other", projects)
	}

	conts, err := c.Containers(ctx, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(conts) != 1 || conts[0].ID != "c1" || conts[0].Name != "web" {
		t.Fatalf("Containers(myapp) = %+v, want only c1/web", conts)
	}
	nets, err := c.Networks(ctx, "myapp")
	if err != nil || len(nets) != 1 || nets[0].ID != "n1" {
		t.Fatalf("Networks(myapp) = %+v, %v", nets, err)
	}
	vols, err := c.Volumes(ctx, "myapp")
	if err != nil || len(vols) != 1 || vols[0].ID != "myapp_data" || vols[0].Name != "myapp_data" {
		t.Fatalf("Volumes(myapp) = %+v, %v", vols, err)
	}
	imgs, err := c.Images(ctx, "myapp")
	if err != nil || len(imgs) != 1 || imgs[0].ID != "sha256:i1" {
		t.Fatalf("Images(myapp) = %+v, %v", imgs, err)
	}

	// A project with no objects yields empty lists.
	if got, err := c.Containers(ctx, "ghost"); err != nil || len(got) != 0 {
		t.Fatalf("Containers(ghost) = %+v, %v", got, err)
	}
}

// Given an image that carries the project label only because it was built
// FROM a project image, Projects must not count it: an inherited label must
// not keep the project "present" after its removal.
func TestProjectsIgnoresInheritedImageLabel(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("image", "sha256:i9", "other-app:latest", "myapp")

	c := New(srv.Socket)
	projects, err := c.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if projects["myapp"] {
		t.Fatal("an image with only an inherited label made the project present")
	}
}

// Given images of the project, Projects counts the ones whose repository
// tag has the <project>-<service> shape for the image's own service label:
// the presence rule is the removal rule.
func TestProjectsCountsProjectImages(t *testing.T) {
	srv := fakedocker.New(t)
	srv.AddLabels("image", "sha256:i1", "myapp-worker:latest", map[string]string{
		ProjectLabel: "myapp",
		ServiceLabel: "worker",
	})

	c := New(srv.Socket)
	projects, err := c.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !projects["myapp"] {
		t.Fatal("a project image did not make the project present")
	}
}

// Given an image whose repository tag starts with <project>- but whose own
// service label does not match the service part of the tag (a retag of a
// project image, the label inherited), Projects does not count it: the
// presence rule is the removal rule, tag shape AND own service label.
func TestProjectsIgnoresMismatchedServiceTag(t *testing.T) {
	srv := fakedocker.New(t)
	srv.AddLabels("image", "sha256:i7", "myapp-custom:latest", map[string]string{
		ProjectLabel: "myapp",
		ServiceLabel: "web",
	})

	c := New(srv.Socket)
	projects, err := c.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if projects["myapp"] {
		t.Fatal("a retagged image with a mismatched service label made the project present")
	}
}

// Given an untagged image carrying the project label, Projects does not
// count it: an untagged leftover (compose up --build orphans the old tag)
// does not make the project present on its own.
func TestProjectsDoesNotCountUntaggedImage(t *testing.T) {
	srv := fakedocker.New(t)
	srv.AddUntaggedImage("sha256:i2", "myapp")

	c := New(srv.Socket)
	projects, err := c.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if projects["myapp"] {
		t.Fatal("an untagged leftover made the project present on its own")
	}
}

// Given an untagged image a container of the project still runs from,
// Projects counts the project: the container makes it present.
func TestProjectsCountsUntaggedImageUsedByContainer(t *testing.T) {
	srv := fakedocker.New(t)
	srv.AddUntaggedImage("sha256:i2", "myapp")
	srv.AddContainerWithImage("c1", "web", "myapp", "sha256:i2")

	c := New(srv.Socket)
	projects, err := c.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !projects["myapp"] {
		t.Fatal("a container running from an untagged image did not make the project present")
	}
}

func TestIsPermissionSocketOnly(t *testing.T) {
	if IsPermission(os.ErrPermission) {
		t.Fatal("a bare permission error is not a socket denial")
	}
	if IsPermission(fmt.Errorf("reading state file: %w", os.ErrPermission)) {
		t.Fatal("a wrapped permission error is not a socket denial")
	}
	if !IsPermission(&net.OpError{Op: "dial", Net: "unix", Err: os.ErrPermission}) {
		t.Fatal("a socket dial EACCES is a socket denial")
	}
}

// Given a daemon that accepts the connection but never answers, a request
// must fail at the client timeout instead of hanging the pass forever.
func TestClientRequestTimeout(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "hang.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Read the request, then never answer.
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	c := New(socket)
	if got := New(socket).http.Timeout; got != requestTimeout {
		t.Fatalf("client timeout = %s, want %s", got, requestTimeout)
	}
	c.http.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err = c.Projects(context.Background())
	if err == nil {
		t.Fatal("a hung daemon produced an answer")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the call ran %s, the client timeout did not fire", elapsed)
	}
	if !IsUnreachable(err) {
		t.Fatalf("a timeout error was not classified as unreachable: %v", err)
	}
}

// shrunkStopClient builds a client whose request backstop is shrunk far
// below the stop's own budget, so a stop must fail or succeed on its own
// timeout + margin and never on the backstop meant for fast calls. A zero
// backstop keeps the client default (the hung-stop test wants the normal
// 2 minute backstop so the 1 second elapsed bound still fails a stop that
// goes through the normal request path).
func shrunkStopClient(t *testing.T, socket string, backstop, margin time.Duration) *Client {
	t.Helper()
	c := New(socket)
	if backstop > 0 {
		c.http.Timeout = backstop
	}
	c.stopMargin = margin
	return c
}

// Given a daemon that answers version but never answers the stop request,
// the stop must fail on a budget derived from the stop timeout plus the
// margin, not on the 2 minute backstop that is meant for fast calls.
func TestStopRequestBoundedByStopTimeout(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "hang.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				if strings.Contains(string(buf[:n]), "/version") {
					body := `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`
					// Connection: close: the handler returns right after,
					// and the client must not pool a connection that is
					// about to die and reuse it for the stop.
					io.WriteString(conn, fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body))
					return
				}
				// The stop request: hold the connection open, answer never.
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	c := shrunkStopClient(t, socket, 0, 100*time.Millisecond)
	start := time.Now()
	err = c.StopContainer(context.Background(), "c1", 50*time.Millisecond)
	if err == nil {
		t.Fatal("a hung stop produced success")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the stop ran %s; its budget was not derived from the stop timeout", elapsed)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("the stop failed after %s; the margin was not added to the 50ms stop timeout", elapsed)
	}
	if !IsUnreachable(err) {
		t.Fatalf("a stop timeout was not classified as unreachable: %v", err)
	}
}

// Given a daemon that answers the stop after a delay longer than the shrunk
// request timeout, the stop must still succeed: it runs on its own budget
// (stop timeout + margin), not on the client backstop meant for fast calls.
func TestStopSurvivesRequestTimeout(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "slow.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				if strings.Contains(string(buf[:n]), "/version") {
					body := `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`
					// Connection: close: the handler returns right after,
					// and the client must not pool a connection that is
					// about to die and reuse it for the stop.
					io.WriteString(conn, fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body))
					return
				}
				// The stop request: answer after a delay that outlives the
				// shrunk request timeout but fits the stop's own budget.
				time.Sleep(120 * time.Millisecond)
				io.WriteString(conn, "HTTP/1.1 204 No Content\r\n\r\n")
			}()
		}
	}()

	// Shrink the backstop below the stop's own duration: a stop that ran
	// on this client would fail long before the daemon answers.
	c := shrunkStopClient(t, socket, 100*time.Millisecond, 200*time.Millisecond)
	start := time.Now()
	err = c.StopContainer(context.Background(), "c1", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("a stop that outlives the shrunk request timeout failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("the stop returned after %s, before the daemon's 120ms delay", elapsed)
	}
}

func TestOwnProjectReadsOnlyOwnID(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "dev-lease", "dev-lease")
	srv.Add("container", "c2", "web", "myapp")

	c := New(srv.Socket)
	own, err := c.OwnProject(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if own != "dev-lease" {
		t.Fatalf("OwnProject(c1) = %q, want dev-lease", own)
	}

	for _, call := range srv.Calls() {
		if call.Path == "/v1.47/containers/c2/json" {
			t.Fatal("client inspected a container other than its own")
		}
	}

	_, err = c.OwnProject(context.Background(), "missing")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("OwnProject(missing) error = %v, want StatusError 404", err)
	}
}

// Given an inspect answer whose Id does not match the detected own id, the
// client must refuse it: it would otherwise read another container's
// project label.
func TestOwnProjectRefusesMismatchingID(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "dev-lease", "dev-lease")
	srv.SetInspectID("c1", "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")

	c := New(srv.Socket)
	_, err := c.OwnProject(context.Background(), "c1")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("OwnProject with a mismatching inspect id = %v, want a refusal", err)
	}
}

func TestRemoveCallsUseNoForce(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")
	srv.Add("image", "sha256:i1", "myapp-web:latest", "myapp")

	c := New(srv.Socket)
	ctx := context.Background()
	if err := c.StopContainer(ctx, "c1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveContainer(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveNetwork(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveVolume(ctx, "myapp_data"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveImage(ctx, "sha256:i1"); err != nil {
		t.Fatal(err)
	}

	var stopSeen, removeSeen bool
	for _, call := range srv.Calls() {
		if call.Method == "POST" && call.Path == "/v1.47/containers/c1/stop" {
			stopSeen = true
			if got := call.Query.Get("t"); got != "10" {
				t.Errorf("stop t param = %q, want 10", got)
			}
		}
		if call.Method == "DELETE" && call.Path == "/v1.47/containers/c1" {
			removeSeen = true
			if got := call.Query.Get("v"); got != "1" {
				t.Errorf("remove v param = %q, want 1", got)
			}
			if got := call.Query.Get("force"); got != "0" {
				t.Errorf("remove force param = %q, want 0", got)
			}
		}
	}
	if !stopSeen || !removeSeen {
		t.Fatalf("stop seen = %v, remove seen = %v, want both", stopSeen, removeSeen)
	}
}

func TestContainersIncludesStopped(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.StopContainer("c1")

	c := New(srv.Socket)
	conts, err := c.Containers(context.Background(), "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(conts) != 1 || conts[0].ID != "c1" {
		t.Fatalf("Containers(myapp) = %+v, want the stopped c1", conts)
	}
	for _, call := range srv.Calls() {
		if call.Path == "/v1.47/containers/json" {
			if got := call.Query.Get("all"); got != "1" {
				t.Errorf("list call all = %q, want 1", got)
			}
		}
	}
}

func TestStopAlreadyStoppedIsSuccess(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")

	c := New(srv.Socket)
	ctx := context.Background()
	if err := c.StopContainer(ctx, "c1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// A second stop answers 304 (already stopped); that must not be an error.
	if err := c.StopContainer(ctx, "c1", 10*time.Second); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

func TestClientRefusesRedirect(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Redirect("/v1.47/containers/json", "/v1.47/containers/abc/json")

	c := New(srv.Socket)
	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://docker/v1.47/containers/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	// On a CheckRedirect failure Do returns the 302 response with a closed
	// body alongside the error; only the error matters here.
	_, err = c.http.Do(req)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error = %v, want the redirect refusal", err)
	}
}

// Given an image tagged for a registry whose host name is <project>-<service>
// with a port, the tag must not match: the rule compares the repository
// part of the tag, not a prefix of the whole string.
func TestImageTagMatchesIgnoresRegistryPort(t *testing.T) {
	img := Object{RepoTags: []string{"myapp-web:5000/team/tool:1"}, Labels: map[string]string{
		ProjectLabel: "myapp", ServiceLabel: "web"}}
	if ImageTagMatches("myapp", img) {
		t.Fatal("a registry-qualified tag with host myapp-web:5000 matched the <project>-<service> rule")
	}
}

// The repository of a tag is the WHOLE part before the tag colon (its last
// colon when that comes after the last slash), registry host and path
// included: only a plain <project>-<service>:<tag> matches the rule.
func TestImageTagMatches(t *testing.T) {
	img := func(tags ...string) Object {
		return Object{RepoTags: tags, Labels: map[string]string{
			ProjectLabel: "myapp", ServiceLabel: "web"}}
	}
	for _, tc := range []struct {
		name string
		tags []string
		want bool
	}{
		{"plain tag", []string{"myapp-web:latest"}, true},
		{"plain tag among others", []string{"ghcr.io/andrius/myapp-web:dev", "myapp-web:latest"}, true},
		{"registry-qualified", []string{"ghcr.io/andrius/myapp-web:dev"}, false},
		{"registry with port", []string{"localhost:5000/team/myapp-web:dev"}, false},
		{"project-shaped host with port", []string{"myapp-web:5000/team/tool:1"}, false},
		{"longer name", []string{"myapp-webapp:latest"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ImageTagMatches("myapp", img(tc.tags...))
			if got != tc.want {
				t.Fatalf("ImageTagMatches(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

// Given a daemon that answers a partial body and then stalls on one
// connection while answering properly on the next, the client timeout must
// not poison the pooled connection: the error belongs to the timed-out call
// only, and the next call succeeds on a fresh connection.
func TestTimeoutMidBodyDoesNotPoisonNextRequest(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "stall.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		// Connection 1: partial body, then hold the connection open.
		conn1, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			buf := make([]byte, 4096)
			if _, err := conn.Read(buf); err != nil {
				return
			}
			partial := "HTTP/1.1 200 OK\r\nContent-Length: 100000\r\n\r\n{\"x\":"
			if _, err := io.WriteString(conn, partial); err != nil {
				return
			}
			// Stall: the client deadline must fire mid-body.
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		}(conn1)
		// Later connections answer properly.
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				body := `[]`
				if strings.Contains(string(buf[:n]), "/version") {
					body = `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`
				} else if strings.Contains(string(buf[:n]), "/volumes") {
					body = `{"Volumes":[]}`
				}
				io.WriteString(conn, fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
			}(conn)
		}
	}()

	c := New(socket)
	c.http.Timeout = 100 * time.Millisecond

	// First call: the timeout fires mid-body.
	_, err = c.Projects(context.Background())
	if err == nil {
		t.Fatal("a partial body produced an answer")
	}
	if !IsUnreachable(err) {
		t.Fatalf("the timed-out call was not classified as unreachable: %v", err)
	}

	// Second call: it must succeed on a fresh connection.
	if _, err := c.Projects(context.Background()); err != nil {
		t.Fatalf("the timed-out call poisoned the next request: %v", err)
	}
}

func TestPinOwnIDBlocksOtherInspections(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "dev-lease", "dev-lease")
	srv.Add("container", "c2", "web", "myapp")

	c := New(srv.Socket)
	c.PinOwnID("c1")
	// The own container still inspects fine.
	if _, err := c.OwnProject(context.Background(), "c1"); err != nil {
		t.Fatalf("own inspect: %v", err)
	}
	// Any other container is refused before socket I/O.
	if _, err := c.OwnProject(context.Background(), "c2"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("inspect of c2 error = %v, want ErrNotAllowed", err)
	}
}

// muxFrame builds one 8-byte multiplexed stream frame around payload, the
// format Docker uses when both stdout and stderr are requested.
func muxFrame(stream byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

// fakeCreatedAt is the fixed creation time every container in the fake daemon
// reports in its listing; keep the two in sync.
var fakeCreatedAt = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestContainerLogsDecodesMultiplexedStream(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	stdout := []byte("2026-10-20T12:00:01.000000000Z hello from stdout\n")
	stderr := []byte("2026-10-20T12:00:02.500000000Z hello from stderr\n")
	// One line split across two frames of the same stream must reassemble.
	split := append(
		muxFrame(1, []byte("2026-10-20T12:00:03.000000000Z split ")),
		muxFrame(1, []byte("line\n"))...,
	)
	raw := append(muxFrame(1, stdout), muxFrame(2, stderr)...)
	raw = append(raw, split...)
	srv.SetLogs("c1", raw)

	c := New(srv.Socket)
	lines, err := c.ContainerLogs(context.Background(), "c1", 50)
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	want := []LogLine{
		{Time: time.Date(2026, 10, 20, 12, 0, 1, 0, time.UTC), Stream: "stdout", Text: "hello from stdout"},
		{Time: time.Date(2026, 10, 20, 12, 0, 2, int(500*time.Millisecond), time.UTC), Stream: "stderr", Text: "hello from stderr"},
		{Time: time.Date(2026, 10, 20, 12, 0, 3, 0, time.UTC), Stream: "stdout", Text: "split line"},
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v, want %d lines", lines, len(want))
	}
	for i, w := range want {
		got := lines[i]
		if !got.Time.Equal(w.Time) || got.Stream != w.Stream || got.Text != w.Text {
			t.Errorf("line %d = %+v, want %+v", i, got, w)
		}
	}
	// The request must carry exactly the parameters dev-lease generates.
	var asked bool
	for _, call := range srv.Calls() {
		if call.Path == "/v1.47/containers/c1/logs" {
			asked = true
			for k, v := range map[string]string{"stdout": "1", "stderr": "1", "timestamps": "1", "tail": "50"} {
				if got := call.Query.Get(k); got != v {
					t.Errorf("logs query %s = %q, want %q", k, got, v)
				}
			}
		}
	}
	if !asked {
		t.Fatal("no logs request reached the daemon")
	}
}

func TestContainerLogsCutsLongLinesAndBadUTF8(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	long := "2026-10-20T12:00:01.000000000Z " + strings.Repeat("a", 5000) + "\n"
	bad := "2026-10-20T12:00:02.000000000Z ab\xff\xfe\n"
	srv.SetLogs("c1", muxFrame(1, []byte(long+bad)))

	c := New(srv.Socket)
	lines, err := c.ContainerLogs(context.Background(), "c1", 100)
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %+v, want 2 lines", lines)
	}
	if got := len(lines[0].Text); got != 2000 {
		t.Errorf("long line length = %d, want 2000", got)
	}
	if got, want := lines[1].Text, "ab\uFFFD"; got != want {
		t.Errorf("bad UTF-8 line = %q, want %q", got, want)
	}
}

func TestContainerLogsClampsTail(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.SetLogs("c1", nil)

	c := New(srv.Socket)
	if _, err := c.ContainerLogs(context.Background(), "c1", 0); err != nil {
		t.Fatalf("tail 0: %v", err)
	}
	if _, err := c.ContainerLogs(context.Background(), "c1", 500); err != nil {
		t.Fatalf("tail 500: %v", err)
	}
	var tails []string
	for _, call := range srv.Calls() {
		if call.Path == "/v1.47/containers/c1/logs" {
			tails = append(tails, call.Query.Get("tail"))
		}
	}
	if len(tails) != 2 || tails[0] != "1" || tails[1] != "200" {
		t.Fatalf("tail values = %v, want [1 200]", tails)
	}
}

func TestContainerMemoryUsesUsageMinusInactive(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.Add("container", "c2", "db", "myapp")
	srv.SetMemory("c1", 1000, 300)

	c := New(srv.Socket)
	mem, err := c.ContainerMemory(context.Background(), "c1")
	if err != nil {
		t.Fatalf("ContainerMemory: %v", err)
	}
	if mem != 700 {
		t.Errorf("memory = %d, want 700 (usage 1000 minus inactive 300)", mem)
	}
	for _, call := range srv.Calls() {
		if call.Path == "/v1.47/containers/c1/stats" {
			if got := call.Query.Get("stream"); got != "false" {
				t.Errorf("stats query stream = %q, want false", got)
			}
			if got := call.Query.Get("one-shot"); got != "true" {
				t.Errorf("stats query one-shot = %q, want true", got)
			}
		}
	}
	// A container with no stats data at all: 0, nothing to subtract.
	mem, err = c.ContainerMemory(context.Background(), "c2")
	if err != nil {
		t.Fatalf("ContainerMemory(c2): %v", err)
	}
	if mem != 0 {
		t.Errorf("memory(c2) = %d, want 0", mem)
	}
}

func TestVolumeSizesSkipsUnknown(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("volume", "v1", "myapp_data", "myapp")
	srv.Add("volume", "v2", "other_data", "other")
	srv.Add("volume", "v3", "unset_data", "myapp")
	srv.SetVolumeSize("myapp_data", 42)
	srv.SetVolumeSize("other_data", -1)

	c := New(srv.Socket)
	sizes, err := c.VolumeSizes(context.Background())
	if err != nil {
		t.Fatalf("VolumeSizes: %v", err)
	}
	want := map[string]int64{"myapp_data": 42}
	if len(sizes) != len(want) {
		t.Fatalf("sizes = %v, want %v", sizes, want)
	}
	for name, size := range want {
		if sizes[name] != size {
			t.Errorf("sizes[%s] = %d, want %d", name, sizes[name], size)
		}
	}
	var asked bool
	for _, call := range srv.Calls() {
		if call.Path == "/v1.47/system/df" {
			asked = true
			if got := call.Query.Get("type"); got != "volume" {
				t.Errorf("df query type = %q, want volume", got)
			}
		}
	}
	if !asked {
		t.Fatal("no system/df request reached the daemon")
	}
}

func TestContainersListingCarriesStatus(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")

	c := New(srv.Socket)
	conts, err := c.Containers(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(conts) != 1 {
		t.Fatalf("containers = %+v, want 1", conts)
	}
	if got := conts[0].Status; got != "Up" {
		t.Errorf("running status = %q, want Up", got)
	}
	if !conts[0].Created.Equal(fakeCreatedAt) {
		t.Errorf("created = %v, want %v", conts[0].Created, fakeCreatedAt)
	}

	srv.StopContainer("c1")
	conts, err = c.Containers(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("Containers after stop: %v", err)
	}
	if got := conts[0].Status; got != "Exited (0)" {
		t.Errorf("stopped status = %q, want Exited (0)", got)
	}
}

// A container with tty: true: the daemon sends a raw stream with no frame
// headers, and the client splits it on newlines.
func TestContainerLogsTTY(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.SetTTY("c1")
	srv.SetLogs("c1", []byte("2026-10-20T12:00:01.000000000Z hello from a tty\n2026-10-20T12:00:02.000000000Z second line\n"))

	c := New(srv.Socket)
	lines, err := c.ContainerLogs(context.Background(), "c1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %+v, want 2 lines", lines)
	}
	if lines[0].Stream != "stdout" || lines[0].Text != "hello from a tty" {
		t.Errorf("line 0 = %+v, want the first TTY line", lines[0])
	}
	if lines[1].Text != "second line" {
		t.Errorf("line 1 = %+v, want the second line", lines[1])
	}
}

// A TTY stream ends its lines with \r\n; the text must not keep the
// carriage return.
func TestContainerLogsTTYCarriageReturn(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.SetTTY("c1")
	srv.SetLogs("c1", []byte("2026-10-20T12:00:01.000000000Z hello\r\n"))

	lines, err := New(srv.Socket).ContainerLogs(context.Background(), "c1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Text != "hello" {
		t.Fatalf("TTY lines = %+v, want the single line without the trailing \\r", lines)
	}
}

// A frame with a stream byte other than 1 or 2 is skipped with its whole
// payload: its bytes never leak into a line.
func TestContainerLogsSkipsUnknownStreamPayload(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	raw := muxFrame(3, []byte("2026-10-20T12:00:01.000000000Z not a stream\n"))
	raw = append(raw, muxFrame(1, []byte("2026-10-20T12:00:02.000000000Z real line\n"))...)
	srv.SetLogs("c1", raw)

	c := New(srv.Socket)
	lines, err := c.ContainerLogs(context.Background(), "c1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Text != "real line" {
		t.Fatalf("lines = %+v, want only the stream-1 line", lines)
	}
}

// A frame header in the middle of a line: two streams interleaved, the
// split line reassembles.
func TestContainerLogsHeaderMidLine(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	raw := muxFrame(1, []byte("2026-10-20T12:00:01.000000000Z split "))
	raw = append(raw, muxFrame(2, []byte("2026-10-20T12:00:02.000000000Z interleaved\n"))...)
	raw = append(raw, muxFrame(1, []byte("line\n"))...)
	srv.SetLogs("c1", raw)

	c := New(srv.Socket)
	lines, err := c.ContainerLogs(context.Background(), "c1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %+v, want 2 lines", lines)
	}
	if lines[0].Stream != "stderr" || lines[0].Text != "interleaved" {
		t.Errorf("line 0 = %+v, want the interleaved stderr line", lines[0])
	}
	if lines[1].Stream != "stdout" || lines[1].Text != "split line" {
		t.Errorf("line 1 = %+v, want the reassembled stdout line", lines[1])
	}
}

// A body ending inside a frame: no panic, the complete lines come back,
// and the truncated frame's partial line is kept.
func TestContainerLogsTruncatedFrame(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	raw := muxFrame(1, []byte("2026-10-20T12:00:01.000000000Z complete\n"))
	frame := muxFrame(1, []byte("2026-10-20T12:00:02.000000000Z cut off"))
	raw = append(raw, frame[:len(frame)-3]...) // the body ends inside the payload
	srv.SetLogs("c1", raw)

	c := New(srv.Socket)
	lines, err := c.ContainerLogs(context.Background(), "c1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Text != "complete" || lines[1].Text != "cut " {
		t.Fatalf("lines = %+v, want the complete line and the truncated frame's partial line", lines)
	}
}

// One 64 MiB log line: the client keeps the first 2000 bytes and never
// buffers the whole line, and stops reading the body after 16 MiB (the
// daemon's write fails mid-body).
func TestContainerLogsLongLineBoundedMemory(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	payload := []byte("2026-10-20T12:00:01.000000000Z " + strings.Repeat("a", 64<<20) + "\n")
	srv.SetLogs("c1", muxFrame(1, payload))
	payload = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	c := New(srv.Socket)
	lines, err := c.ContainerLogs(context.Background(), "c1", 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	if len(lines) != 1 || len(lines[0].Text) != 2000 {
		t.Fatalf("lines = %+v, want one 2000-byte line", lines)
	}
	if alloc > 8<<20 {
		t.Errorf("reading one tail line allocated %d MiB; the read is not bounded", alloc>>20)
	}
	// The client closes the body at the 16 MiB cap, so the daemon's write
	// of the remaining 48 MiB fails: it stays blocked until the close
	// lands, then errors. Without the cap the daemon writes everything.
	deadline := time.Now().Add(5 * time.Second)
	for !srv.LogsWriteFailed("c1") {
		if time.Now().After(deadline) {
			t.Fatal("the daemon wrote the whole 64 MiB: the client read past the 16 MiB body cap")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The container listing carries the image reference the container was
// created from.
func TestContainerListingCarriesImageRef(t *testing.T) {
	srv := fakedocker.New(t)
	srv.AddContainerWithUses("c1", "db", map[string]string{ProjectLabel: "myapp"}, "sha256:pg", nil, nil)
	srv.SetContainerImageRef("c1", "postgres:16-alpine")

	c := New(srv.Socket)
	conts, err := c.Containers(context.Background(), "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(conts) != 1 {
		t.Fatalf("containers = %+v, want 1", conts)
	}
	if conts[0].ImageRef != "postgres:16-alpine" {
		t.Errorf("ImageRef = %q, want postgres:16-alpine", conts[0].ImageRef)
	}
}

// Without a multiplexed content type the first 8 bytes decide: a header
// means multiplexed, anything else is a raw stream split on newlines.
func TestDecodeLogsSniffsMultiplexedWithoutContentType(t *testing.T) {
	muxed := decodeLogs(bytes.NewReader(muxFrame(1, []byte("2026-10-20T12:00:01.000000000Z sniffed\n"))), "", 10)
	if len(muxed) != 1 || muxed[0].Text != "sniffed" {
		t.Fatalf("lines = %+v, want the sniffed multiplexed line", muxed)
	}
	raw := decodeLogs(bytes.NewReader([]byte("2026-10-20T12:00:01.000000000Z raw line\n")), "", 10)
	if len(raw) != 1 || raw[0].Text != "raw line" {
		t.Fatalf("lines = %+v, want the raw line", raw)
	}
}
