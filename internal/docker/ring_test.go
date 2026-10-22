package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/andrius/dev-lease/internal/fakedocker"
)

// One frame carrying 300 lines (the fake cuts on frame
// boundaries only), tail 200: the client's own ring keeps the last 200.
func TestContainerLogsRingCutsMultiLineFrame(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "2026-10-20T12:00:00.000000000Z line %03d\n", i)
	}
	p := []byte(b.String())
	f := make([]byte, 8+len(p))
	f[0] = 1
	n := len(p)
	f[4], f[5], f[6], f[7] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(f[8:], p)
	srv.SetLogs("c1", f)
	lines, err := New(srv.Socket).ContainerLogs(context.Background(), "c1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 200 || lines[0].Text != "line 101" || lines[199].Text != "line 300" {
		t.Fatalf("got %d lines (first %q), want exactly lines 101..300", len(lines), lines[0].Text)
	}
}
