package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prefixDaemon answers like a real daemon: an inspect by id prefix
// returns the full id, and the container listing carries the full id.
func prefixDaemon(t *testing.T, fullID string, labels map[string]string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	js := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/version":
			js(w, map[string]string{"ApiVersion": "1.47"})
		case p == "/v1.47/containers/json":
			js(w, []map[string]any{{"Id": fullID, "Names": []string{"/dev-lease-dev-lease-1"}, "Labels": labels, "State": "running"}})
		case strings.HasPrefix(p, "/v1.47/containers/") && strings.HasSuffix(p, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.47/containers/"), "/json")
			if !strings.HasPrefix(fullID, id) {
				w.WriteHeader(404)
				return
			}
			js(w, map[string]any{"Id": fullID, "Labels": labels, "Config": map[string]any{"Labels": labels}})
		case p == "/v1.47/networks", p == "/v1.47/images/json":
			js(w, []any{})
		case p == "/v1.47/volumes":
			js(w, map[string]any{"Volumes": []any{}})
		default:
			w.WriteHeader(404)
		}
	})
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

const fullOwnID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var ownServerLabels = map[string]string{
	"com.docker.compose.project": "dev-lease",
	"dev-lease.role":             "server",
}

func runServeWithOwnID(t *testing.T, sock, ownID string) (string, error) {
	t.Helper()
	st := tempStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var errOut bytes.Buffer
	err := runServe(ctx, serveConfig{
		OwnID: ownID, Projects: []string{"myapp"}, State: st.Path, Socket: sock,
		Interval: time.Minute, DefaultTTL: 2 * time.Hour, Out: &bytes.Buffer{}, ErrOut: &errOut,
	})
	return errOut.String(), err
}

// --own-id with the short id docker ps prints: the inspect accepts a
// prefix, so the second-serve guard must compare the full id from the
// inspect, not the short --own-id, or serve refuses its own container as
// "another server".
func TestServeShortOwnIDDoesNotRefuseItself(t *testing.T) {
	skipServeWhenRoot(t)
	sock := prefixDaemon(t, fullOwnID, ownServerLabels)
	if _, err := runServeWithOwnID(t, sock, fullOwnID[:12]); err != nil {
		t.Fatalf("serve refused its own container when --own-id is the short id: %v", err)
	}
}

// Control: the full id starts (runs until the context ends).
func TestServeFullOwnIDStarts(t *testing.T) {
	skipServeWhenRoot(t)
	sock := prefixDaemon(t, fullOwnID, ownServerLabels)
	if _, err := runServeWithOwnID(t, sock, fullOwnID); err != nil {
		t.Fatalf("control: serve with the full id: %v", err)
	}
}
