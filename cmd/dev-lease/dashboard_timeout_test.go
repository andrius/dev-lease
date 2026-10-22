package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/fakedocker"
)

// the dashboard http.Server carries a ReadHeaderTimeout, so a
// client that opens a connection and never finishes its request line is
// cut off after about 5 s instead of holding a socket forever. Without the
// setting the read hangs until this test's own deadline and fails.
func TestServeDashboardReadHeaderTimeout(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "myapp_web_1", "myapp")
	st := tempStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var errOut syncBuf
	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID:     "c-own",
			Projects:  []string{"myapp"},
			State:     st.Path,
			Socket:    srv.Socket,
			Interval:  10 * time.Millisecond,
			Dashboard: "127.0.0.1:0",
			Out:       &syncBuf{},
			ErrOut:    &errOut,
		})
	}()
	addr := waitForDashboardAddr(t, &errOut)

	// Open a connection and send a request line without the blank line
	// that ends the headers. The server must close the connection once
	// ReadHeaderTimeout passes; a missing timeout keeps the connection
	// open until our deadline and fails the test.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n")
	conn.SetReadDeadline(time.Now().Add(12 * time.Second))

	start := time.Now()
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the connection stayed open after a partial request header")
	}
	if elapsed < 4*time.Second || elapsed > 11*time.Second {
		t.Fatalf("the server cut the partial header after %v, want about 5 s (ReadHeaderTimeout)", elapsed)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
