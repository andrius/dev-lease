// Command dev-lease gives local Docker Compose projects a TTL lease: serve
// stops projects whose lease expired and removes them after a grace period,
// and extend/release/list/status manage the leases from the same state file.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
)

// version is stamped by the build (Dockerfile ldflags); "dev" for local
// builds.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usageText = `dev-lease - TTL leases for local Docker Compose projects

usage:
  dev-lease serve  --projects <p,p-*> [--interval 1m] [--default-ttl 2h]
                   [--remove-after 168h] [--stop-timeout 10s]
                   [--state /data/leases.json]
                   [--socket /var/run/docker.sock] [--exit-when-idle]
  dev-lease extend <project> <ttl> [--holder <name>]
  dev-lease release <project> (--holder <name> | --all)
  dev-lease down <project> [--holder <name>]
  dev-lease list
  dev-lease status
  dev-lease mcp     [--socket /var/run/docker.sock] [--state /data/leases.json]
  dev-lease -version
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 1
	}
	switch args[0] {
	case "-version", "--version", "version":
		fmt.Fprintln(stdout, "dev-lease "+version)
		return 0
	case "-help", "--help", "-h", "help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "serve":
		return runServeCmd(args[1:], stdout, stderr)
	case "mcp":
		return runMCPCmd(args[1:], stdout, stderr)
	case "extend", "release", "down", "list", "status":
		// These commands run as root inside the container when exec'd; drop
		// privileges the same way serve does before touching the daemon or
		// the state file.
		if err := privdrop("/var/run/docker.sock"); err != nil {
			fmt.Fprintln(stderr, "dropping privileges:", err)
			return 1
		}
		switch args[0] {
		case "extend":
			return runExtendCmd(args[1:], stdout, stderr)
		case "release":
			return runReleaseCmd(args[1:], stdout, stderr)
		case "down":
			return runDownCmd(args[1:], stdout, stderr)
		case "list":
			return runListCmd(args[1:], stdout, stderr)
		case "status":
			return runStatusCmd(args[1:], stdout, stderr)
		}
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usageText)
		return 1
	}
	return 1
}

// boolFlag is implemented by the flag package's bool values; a bool flag
// never consumes the next argument, so it must not pull one into the flag
// group during the reorder.
type boolFlag interface{ IsBoolFlag() bool }

// durationValue is a flag.Value for durations that also accepts whole days
// ("7d"), on top of everything time.ParseDuration takes.
type durationValue struct{ d *time.Duration }

func (v durationValue) String() string {
	if v.d == nil {
		return ""
	}
	return v.d.String()
}

func (v durationValue) Set(s string) error {
	d, err := parseDuration(s)
	if err != nil {
		return err
	}
	*v.d = d
	return nil
}

// parseDuration parses a Go duration or a whole number of days ("7d" =
// 168h). Days must be whole and positive: 1.5d, a bare d and -1d are
// refused.
func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// Whole days: 1d..3650d. strconv.Atoi would accept a leading +, so the
	// prefix is refused here instead of inside the number.
	if !strings.HasSuffix(s, "d") || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("cannot parse duration %q: want a Go duration like 2h or whole days like 7d", s)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n <= 0 || n > 3650 {
		return 0, fmt.Errorf("cannot parse duration %q: want a Go duration like 2h or whole days like 7d (at most 3650d)", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// reorderArgs moves flags (with their values) ahead of positional
// arguments, because the usage examples put flags after the positionals and
// Go's flag package stops parsing at the first non-flag argument.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	takesValue := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			return
		}
		takesValue["-"+f.Name] = true
		takesValue["--"+f.Name] = true
	})
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name := a
		if j := strings.IndexByte(a, '='); j >= 0 {
			name = a[:j]
		}
		flags = append(flags, a)
		if takesValue[name] && !strings.Contains(a, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, pos...)
}

// runServeCmd parses the serve flags and runs the loop until idle or signal.
func runServeCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projects := fs.String("projects", os.Getenv("DEV_LEASE_PROJECTS"), "comma-separated glob patterns over Compose project names")
	dashboard := fs.String("dashboard", os.Getenv("DEV_LEASE_DASHBOARD"), "serve the dashboard on this address (empty: off)")
	dashboardHosts := fs.String("dashboard-hosts", os.Getenv("DEV_LEASE_DASHBOARD_HOSTS"), "comma-separated Host allowlist for the dashboard (empty: the dashboard's default)")
	// The dashboard token comes from the env var only: never a flag, never
	// logged. It is read once here and travels no further than runServe.
	dashboardToken := os.Getenv("DEV_LEASE_DASHBOARD_TOKEN")
	interval := time.Minute
	defaultTTL := 2 * time.Hour
	stopTimeout := 10 * time.Second
	removeAfter := 168 * time.Hour
	if v := os.Getenv("DEV_LEASE_REMOVE_AFTER"); v != "" {
		d, err := parseDuration(v)
		if err != nil {
			fmt.Fprintln(stderr, "serve: --remove-after:", err)
			return 1
		}
		removeAfter = d
	}
	fs.Var(&durationValue{&interval}, "interval", "sweep interval")
	fs.Var(&durationValue{&defaultTTL}, "default-ttl", "lease a new managed project gets")
	fs.Var(&durationValue{&removeAfter}, "remove-after", "how long a stopped project is kept before removal (0: never)")
	fs.Var(&durationValue{&stopTimeout}, "stop-timeout", "container stop timeout")
	state := fs.String("state", statePath(), "path of the state file")
	socket := fs.String("socket", "/var/run/docker.sock", "Docker socket")
	exitWhenIdle := fs.Bool("exit-when-idle", false, "exit once no managed project has a live lease or a running container that is not kept, and nothing stopped waits for removal")
	ownID := fs.String("own-id", "", "override the own container id detection")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return 1
	}
	if rest := fs.Args(); len(rest) != 0 {
		fmt.Fprintf(stderr, "serve: unexpected argument(s): %s\n", strings.Join(rest, " "))
		return 1
	}
	if *projects == "" {
		fmt.Fprintln(stderr, "serve: --projects (or DEV_LEASE_PROJECTS) is required")
		return 1
	}
	patterns, err := normalizePatterns(*projects)
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	var hosts []string
	if *dashboardHosts != "" {
		hosts, err = normalizeHosts(*dashboardHosts)
		if err != nil {
			fmt.Fprintln(stderr, "serve: --dashboard-hosts:", err)
			return 1
		}
	}
	if interval < time.Second {
		fmt.Fprintln(stderr, "serve: --interval must be at least 1s")
		return 1
	}
	if stopTimeout < time.Second {
		fmt.Fprintln(stderr, "serve: --stop-timeout must be at least 1s")
		return 1
	}
	if removeAfter < 0 {
		fmt.Fprintln(stderr, "serve: --remove-after must not be negative")
		return 1
	}
	if removeAfter > 0 && removeAfter < time.Hour {
		fmt.Fprintln(stderr, "serve: --remove-after must be 0 (never) or at least 1h")
		return 1
	}
	// An explicit 0 is refused here; runServe treats a zero-value config as
	// the default, which only tests construct. --remove-after 0 is the
	// meaningful "never", so it is never defaulted.
	if err := lease.ValidateTTL(defaultTTL); err != nil {
		fmt.Fprintln(stderr, "serve: --default-ttl:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = runServe(ctx, serveConfig{
		Projects:       patterns,
		OwnID:          *ownID,
		State:          *state,
		Socket:         *socket,
		Interval:       interval,
		DefaultTTL:     defaultTTL,
		RemoveAfter:    removeAfter,
		StopTimeout:    stopTimeout,
		ExitWhenIdle:   *exitWhenIdle,
		Out:            stdout,
		ErrOut:         stderr,
		Dashboard:      *dashboard,
		DashboardHosts: hosts,
		DashboardToken: dashboardToken,
	})
	if err != nil {
		fmt.Fprintln(stderr, "serve:", err)
		return 1
	}
	return 0
}

// normalizePatterns splits the --projects value on commas, trims every
// pattern, and refuses empty or malformed ones.
func normalizePatterns(s string) ([]string, error) {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("empty pattern in --projects")
		}
		if _, err := path.Match(p, "probe"); err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", p, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// normalizeHosts splits the --dashboard-hosts value on commas, trims every
// entry and refuses empty or malformed ones: each entry must be a host,
// host:port, [ipv6] or [ipv6]:port, and is lowercased. Wildcards, paths,
// schemes and other punctuation are refused with a message naming the
// entry.
func normalizeHosts(s string) ([]string, error) {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("empty Host in --dashboard-hosts")
		}
		if !validHostEntry(p) {
			return nil, fmt.Errorf("bad Host %q in --dashboard-hosts: want host, host:port, [ipv6] or [ipv6]:port", p)
		}
		out = append(out, strings.ToLower(p))
	}
	return out, nil
}

// validHostEntry reports whether e is a Host allowlist entry the
// dashboard can compare against the Host header: a host, host:port,
// [ipv6] or [ipv6]:port. A scheme, a path, a wildcard or any other
// punctuation makes it false.
func validHostEntry(e string) bool {
	if strings.HasPrefix(e, "[") {
		// [ipv6] or [ipv6]:port
		if strings.HasSuffix(e, "]") {
			return net.ParseIP(e[1:len(e)-1]) != nil
		}
		host, port, err := net.SplitHostPort(e)
		return err == nil && net.ParseIP(host) != nil && validPort(port)
	}
	if strings.Contains(e, ":") {
		// host:port; a bare unbracketed ipv6 is refused (no host:port
		// split succeeds on it, and "::1" alone is not an accepted form)
		host, port, err := net.SplitHostPort(e)
		return err == nil && validHostName(host) && validPort(port)
	}
	return validHostName(e)
}

func validPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= 65535
}

// validHostName reports whether h is a plain hostname or IPv4 address:
// letters, digits, dots, hyphens and underscores only, with a label at
// each dot boundary.
func validHostName(h string) bool {
	if h == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return false
		}
		for _, c := range label {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-' || c == '_':
				if label[0] == '-' || label[0] == '_' {
					return false
				}
			default:
				return false
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

// runExtendCmd parses `extend <project> <ttl> [--holder <name>]`.
func runExtendCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("extend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	holder := fs.String("holder", "", "name of the holder (default: $DEV_LEASE_HOLDER or $USER)")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return 1
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(stderr, "extend: usage: dev-lease extend <project> <ttl> [--holder <name>]")
		return 1
	}
	return runExtend(stdout, lease.Store{Path: statePath()}, rest[0], rest[1], *holder)
}

// runReleaseCmd parses `release <project> (--holder <name> | --all)`.
func runReleaseCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	holder := fs.String("holder", "", "release only this holder's claim")
	all := fs.Bool("all", false, "release every claim")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return 1
	}
	rest := fs.Args()
	if len(rest) != 1 || (*holder == "") == !*all {
		fmt.Fprintln(stderr, "release: usage: dev-lease release <project> (--holder <name> | --all)")
		return 1
	}
	return runRelease(stdout, lease.Store{Path: statePath()}, rest[0], *holder, *all)
}

// runDownCmd parses `down <project> [--holder <name>]`.
func runDownCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	holder := fs.String("holder", "", "name of the holder (default: $DEV_LEASE_HOLDER or $USER)")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return 1
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "down: usage: dev-lease down <project> [--holder <name>]")
		return 1
	}
	// A removal can take a while on a big stack: Ctrl-C gives up on the
	// answer, serve may still finish the action.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDown(ctx, stdout, lease.Store{Path: statePath()}, rest[0], *holder)
}

func runListCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return 1
	}
	return runList(stdout, lease.Store{Path: statePath()})
}

func runStatusCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "/var/run/docker.sock", "Docker socket")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return 1
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintln(stderr, "status: no positional arguments")
		return 1
	}
	return runStatus(stdout, stderr, lease.Store{Path: statePath()}, *socket)
}

// statePath is the state file clients use: DEV_LEASE_STATE when set, else the
// container path /data/leases.json that the example compose file mounts.
func statePath() string {
	if v := os.Getenv("DEV_LEASE_STATE"); v != "" {
		return v
	}
	return "/data/leases.json"
}
