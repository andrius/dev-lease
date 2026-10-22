// runMCPCmd is the `dev-lease mcp` command: the MCP server of Task 7.
//
// It reads one JSON-RPC line per message on stdin, answers on stdout and
// writes nothing else to stdout. Like serve, it drops privileges before
// touching the Docker socket or the state file. It only reads the daemon
// and the lease store itself; a stop or remove request is forwarded to
// the serve process over the control socket next to the state file. The
// session ends when stdin ends (the MCP client closed it) or when the
// stop signal arrives.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/mcp"
	"github.com/andrius/dev-lease/internal/reaper"
)

// runMCPCmd parses the mcp flags and serves one session. The protocol
// runs on the real stdin and stdout: an MCP client speaks stdio, so the
// session never goes through the run() writers.
func runMCPCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "/var/run/docker.sock", "Docker socket")
	state := fs.String("state", statePath(), "path of the state file")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return 1
	}
	if rest := fs.Args(); len(rest) != 0 {
		fmt.Fprintf(stderr, "mcp: unexpected argument(s): %s\n", strings.Join(rest, " "))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// From the moment the context is done, a further signal kills the
	// process with its default disposition: a handler stuck on a blocked
	// stdout write must not keep the process alive through a second
	// SIGTERM.
	context.AfterFunc(ctx, stop)
	defer stop()
	if err := runMCP(ctx, *socket, *state); err != nil {
		fmt.Fprintln(stderr, "mcp:", err)
		return 1
	}
	return 0
}

// runMCP wires the tools and serves one MCP session until the input ends
// or ctx is cancelled.
func runMCP(ctx context.Context, socket, state string) error {
	// Drop root before any Docker call: the daemon socket's group is kept,
	// exactly as serve does.
	if err := privdrop(socket); err != nil {
		return fmt.Errorf("dropping privileges: %w", err)
	}

	st := lease.Store{Path: state}
	hdr, err := st.ReadHeader()
	if err != nil {
		return fmt.Errorf("cannot read the state file %s: %v; start dev-lease serve first", state, err)
	}
	if len(hdr.Patterns) == 0 {
		return fmt.Errorf("the state file %s has no managed project patterns; start dev-lease serve first", state)
	}

	cli := docker.New(socket)
	// The own-id is pinned only when it is already known (the env var):
	// unlike serve, mcp never refuses to start over an undetectable own
	// container, and in practice serve refuses patterns matching its own
	// project anyway.
	if ownID := os.Getenv("DEV_LEASE_OWN_ID"); ownID != "" {
		cli.PinOwnID(ownID)
	}
	tools := mcp.Tools{
		Cli:   cli,
		Store: st,
		Cfg: reaper.Config{
			Patterns:    hdr.Patterns,
			DefaultTTL:  hdr.DefaultTTL,
			RemoveAfter: hdr.RemoveAfter,
		},
		ControlPath: control.SocketPath(state),
		Version:     version,
	}
	return mcp.Serve(ctx, os.Stdin, os.Stdout, tools)
}
