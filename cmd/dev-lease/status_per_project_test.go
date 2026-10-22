package main

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/fakedocker"
)

// status exits 1 when the Docker listing fails: here the project-wide
// listing works and a per-project listing fails.
func TestStatusPerProjectListingFailureExitsOne(t *testing.T) {
	st := tempStore(t)
	t.Setenv("DEV_LEASE_STATE", st.Path)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	var mu sync.Mutex
	n := 0
	srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/containers/json") {
			mu.Lock()
			n++
			k := n
			mu.Unlock()
			if k == 2 {
				srv.FailList("container", http.StatusInternalServerError)
			}
		}
	})
	var out, errOut bytes.Buffer
	code := runStatusCmd([]string{"--socket", srv.Socket}, &out, &errOut)
	t.Logf("out=%q", out.String())
	if code != 1 {
		t.Fatalf("status with a failed per-project listing = %d, want 1", code)
	}
}
