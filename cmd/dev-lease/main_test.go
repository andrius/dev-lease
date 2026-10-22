package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
	"github.com/andrius/dev-lease/internal/view"
)

func tempStore(t *testing.T) lease.Store {
	t.Helper()
	return lease.Store{Path: filepath.Join(t.TempDir(), "leases.json")}
}

// writeHeader mimics what serve writes before its loop: the managed patterns
// and the default ttl, which client commands read to decide what is managed.
func writeHeader(t *testing.T, st lease.Store, patterns []string, ttl time.Duration) {
	t.Helper()
	if err := st.UpdateHeader(func(h *lease.Header) error {
		h.Patterns = patterns
		h.DefaultTTL = ttl
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func extendLease(t *testing.T, st lease.Store, project string, ttl time.Duration, at time.Time) {
	t.Helper()
	if err := st.Update(func(m map[string]lease.Lease) error {
		l, err := lease.Extend(m[project], "ci", ttl, at)
		if err != nil {
			return err
		}
		m[project] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func withOwnContainer(t *testing.T, srv *fakedocker.Server, id, project string) {
	t.Helper()
	// Compose labels the dev-lease service with the project and the
	// service name, and examples/basic/compose.yml adds the role label serve
	// checks before doing anything else.
	srv.AddLabels("container", id, "dev-lease", map[string]string{
		"com.docker.compose.project": project,
		"com.docker.compose.service": "dev-lease",
		"dev-lease.role":             "server",
	})
}

// --- own container detection ---

func TestOwnIDFromMountinfoOverlay2(t *testing.T) {
	// Synthetic mountinfo in Docker overlay2 shape: the root line carries
	// the overlay layer id first, the /containers/<id>/ binds come later.
	// The detection must take the bind source, not the first 64-hex string.
	data := `29 22 0:26 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/AAAA,upperdir=/var/lib/docker/overlay2/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/diff,workdir=/var/lib/docker/overlay2/l/WORK
40 29 0:62 /containers/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/hostname /etc/hostname rw,relatime - tmpfs tmpfs rw
41 29 0:62 /containers/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/hosts /etc/hosts rw,relatime - tmpfs tmpfs rw
42 29 0:62 /containers/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/resolv.conf /etc/resolv.conf rw,relatime - tmpfs tmpfs rw
`
	id, ok := ownIDFromMountinfo(data)
	if !ok {
		t.Fatal("container id not found in a Docker overlay2 mountinfo")
	}
	if id != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("own id = %q, want the /containers/ bind id, not the overlay layer id", id)
	}
}

func TestOwnIDFromMountinfoWithoutEtcBinds(t *testing.T) {
	// The control: a mountinfo without any of the /etc bind mounts must
	// not give an id, however many random ids appear elsewhere (the
	// snapshot layer ids of a containerd snapshotter).
	data := `1600 65 0:103 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/1/fs,upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/2/fs,workdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/2/work
1601 1600 0:105 / /proc rw,relatime - proc proc rw
`
	id, ok := ownIDFromMountinfo(data)
	if ok {
		t.Fatalf("found %q in a mountinfo without /etc binds, want no id", id)
	}
}

func TestOwnIDFromMountinfoIgnoresShortIDs(t *testing.T) {
	data := `1 2 0:1 /containers/abc123/hostname /etc/hostname rw,relatime - tmpfs tmpfs rw
`
	if id, ok := ownIDFromMountinfo(data); ok {
		t.Fatalf("accepted short id %q, want no id", id)
	}
}

func TestOwnIDFromMountinfoIgnoresNonEtcMounts(t *testing.T) {
	// A /containers/<64-hex>/ source on a mount point that is not one of
	// the etc files must not be accepted: only the etc binds are known to
	// belong to the own container.
	data := `100 50 0:70 /containers/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/data /data rw,relatime - ext4 /dev/sda1 rw
`
	if id, ok := ownIDFromMountinfo(data); ok {
		t.Fatalf("accepted %q from a non-etc mount point, want no id", id)
	}
}

func TestOwnIDIgnoresHostnameEnv(t *testing.T) {
	// A hostname is resolved by container NAME and can read another
	// container; a HOSTNAME value that happens to look like a container id
	// must never stand in for the mountinfo bind source.
	t.Setenv("HOSTNAME", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	data := `40 29 0:62 /containers/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/hostname /etc/hostname rw,relatime - tmpfs tmpfs rw
`
	id, ok := ownIDFromMountinfo(data)
	if !ok {
		t.Fatal("container id not found in the mountinfo fixture")
	}
	if id != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("own id = %q, want the mountinfo bind id, not the HOSTNAME value", id)
	}
}

func TestOwnIDDoesNotFallBackToHostnameEnv(t *testing.T) {
	// With no /etc bind mount at all, a 64-hex HOSTNAME must not rescue the
	// detection: a hostname fallback would read another container.
	t.Setenv("HOSTNAME", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	data := `1600 65 0:103 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/1/fs,upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/2/fs,workdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/2/work
`
	if id, ok := ownIDFromMountinfo(data); ok {
		t.Fatalf("fell back to the HOSTNAME value %q, want no id", id)
	}
}

// Given a mountinfo captured inside a Docker container with the /etc
// bind mounts (testdata/proc-mountinfo, ids replaced by synthetic
// values), the detection takes the id from the binds - and a 64-hex
// HOSTNAME value is never used.
func TestDetectOwnIDFromProc(t *testing.T) {
	t.Setenv("DEV_LEASE_OWN_ID", "")
	t.Setenv("HOSTNAME", strings.Repeat("b", 64))
	prev := mountinfoPath
	mountinfoPath = filepath.Join("testdata", "proc-mountinfo")
	t.Cleanup(func() { mountinfoPath = prev })

	id, err := detectOwnIDFromProc()
	if err != nil {
		t.Fatalf("detection on the real fixture: %v", err)
	}
	const want = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	if id != want {
		t.Fatalf("own id = %q, want %q from the fixture", id, want)
	}
}

// Given the control fixture (testdata/proc-mountinfo-noetcbinds, a real
// capture with the /etc binds removed) and a 64-hex HOSTNAME, the
// detection returns an error: it never falls back to the hostname, and a
// /containers/<id>/ bind on a non-etc mount does not rescue it either.
func TestDetectOwnIDFromProcWithoutEtcBinds(t *testing.T) {
	t.Setenv("DEV_LEASE_OWN_ID", "")
	hostname := strings.Repeat("b", 64)
	t.Setenv("HOSTNAME", hostname)
	prev := mountinfoPath
	mountinfoPath = filepath.Join("testdata", "proc-mountinfo-noetcbinds")
	t.Cleanup(func() { mountinfoPath = prev })

	_, err := detectOwnIDFromProc()
	if err == nil {
		t.Fatal("detection succeeded without /etc binds")
	}
	if strings.Contains(err.Error(), hostname) {
		t.Fatalf("error = %v, must not use the HOSTNAME value", err)
	}
}

// Given a mountinfo file that does not exist, the detection refuses: no id
// means no safe own-project check.
func TestDetectOwnIDFromProcMissingFile(t *testing.T) {
	t.Setenv("DEV_LEASE_OWN_ID", "")
	prev := mountinfoPath
	mountinfoPath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { mountinfoPath = prev })

	if _, err := detectOwnIDFromProc(); err == nil {
		t.Fatal("detection succeeded without a mountinfo file")
	}
}

func TestServeRefusesWhenOwnIDNotDetected(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	prev := detectOwnID
	detectOwnID = func() (string, error) {
		return "", errors.New("cannot determine dev-lease's own container id from /proc/self/mountinfo; pass --own-id")
	}
	t.Cleanup(func() { detectOwnID = prev })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var errOut syncBuf
	err := runServe(ctx, serveConfig{Projects: []string{"myapp"}, State: tempStore(t).Path,
		Socket: srv.Socket, Interval: 50 * time.Millisecond, Out: &syncBuf{}, ErrOut: &errOut})
	if err == nil || !strings.Contains(err.Error(), "own container id") {
		t.Fatalf("serve error = %v, want a refusal naming the own id", err)
	}
}

// --- flag validation ---

func TestServeRefusesInvalidConfig(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	st := tempStore(t)
	base := serveConfig{
		OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
		Socket: srv.Socket, Interval: time.Minute, DefaultTTL: 2 * time.Hour,
		ExitWhenIdle: true, Out: &syncBuf{}, ErrOut: &syncBuf{},
	}
	t.Run("when the interval is zero or negative, expect a refusal", func(t *testing.T) {
		cfg := base
		cfg.Interval = 0
		err := runServe(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "interval") {
			t.Fatalf("error = %v, want a refusal naming the interval", err)
		}
	})
	t.Run("when the default ttl is outside 1m..168h, expect a refusal", func(t *testing.T) {
		for _, ttl := range []time.Duration{30 * time.Second, 169 * time.Hour} {
			cfg := base
			cfg.DefaultTTL = ttl
			err := runServe(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "default-ttl") {
				t.Fatalf("default-ttl %s: error = %v, want a refusal naming default-ttl", ttl, err)
			}
		}
	})
	t.Run("when no projects are given, expect a refusal", func(t *testing.T) {
		cfg := base
		cfg.Projects = nil
		err := runServe(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "--projects") {
			t.Fatalf("error = %v, want a refusal naming --projects", err)
		}
	})
}

func TestNormalizePatterns(t *testing.T) {
	t.Run("when patterns carry spaces, expect them trimmed", func(t *testing.T) {
		got, err := normalizePatterns(" myapp , myapp-* ")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "myapp" || got[1] != "myapp-*" {
			t.Fatalf("normalizePatterns = %q, want [myapp myapp-*]", got)
		}
	})
	t.Run("when a pattern is empty, expect a refusal", func(t *testing.T) {
		if _, err := normalizePatterns("myapp,,myapp-*"); err == nil {
			t.Fatal("an empty pattern was accepted")
		}
	})
	t.Run("when a pattern is not a valid glob, expect a refusal", func(t *testing.T) {
		if _, err := normalizePatterns("book[forge"); err == nil {
			t.Fatal("a malformed pattern was accepted")
		}
	})
}

func TestServeCmdRefusesBadDefaultTTL(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	var errOut bytes.Buffer
	code := runServeCmd([]string{
		"--projects", "myapp", "--default-ttl", "0",
		"--socket", srv.Socket, "--state", tempStore(t).Path,
		"--own-id", "c-own", "--interval", "1s",
	}, &bytes.Buffer{}, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "default-ttl") {
		t.Fatalf("exit = %d, stderr = %q; want 1 and a default-ttl message", code, errOut.String())
	}
}

func TestServeCmdRequiresProjects(t *testing.T) {
	var errOut bytes.Buffer
	if code := runServeCmd([]string{"--interval", "1m"}, &bytes.Buffer{}, &errOut); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "--projects") {
		t.Fatalf("stderr = %q, want a message naming --projects", errOut.String())
	}
}

// Given --interval below 1s, serve refuses before any daemon call: a
// sub-second sweep makes no sense and the flag bound must hold at the CLI.
func TestServeCmdRefusesIntervalBelowOneSecond(t *testing.T) {
	var errOut bytes.Buffer
	code := runServeCmd([]string{"--projects", "myapp", "--interval", "500ms"}, &bytes.Buffer{}, &errOut)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "--interval") || !strings.Contains(errOut.String(), "1s") {
		t.Fatalf("stderr = %q, want a message naming --interval and its 1s bound", errOut.String())
	}
}

// Given --stop-timeout below 1s, serve refuses: a stop shorter than a
// second is below the daemon's own granularity and the flag bound must
// hold at the CLI.
func TestServeCmdRefusesStopTimeoutBelowOneSecond(t *testing.T) {
	var errOut bytes.Buffer
	code := runServeCmd([]string{"--projects", "myapp", "--stop-timeout", "500ms"}, &bytes.Buffer{}, &errOut)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "--stop-timeout") || !strings.Contains(errOut.String(), "1s") {
		t.Fatalf("stderr = %q, want a message naming --stop-timeout and its 1s bound", errOut.String())
	}
}

// --- extend / release / list ---

func TestExtendExitCodes(t *testing.T) {
	now := time.Now().UTC()
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp", "myapp-*"}, 2*time.Hour)

	// Managed project: exit 0, the holder is recorded, and the message
	// carries exactly one time form (RFC3339 with Z, no "UTC" suffix).
	var out bytes.Buffer
	if code := runExtend(&out, st, "myapp", "2h", "bob"); code != 0 {
		t.Fatalf("extend managed project exit code = %d, want 0", code)
	}
	if msg := out.String(); !strings.Contains(msg, "until ") || strings.Contains(msg, " UTC") {
		t.Fatalf("extend message = %q, want one time form without a UTC suffix", msg)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := leases["myapp"]
	if !ok {
		t.Fatal("no lease written")
	}
	if l.ExpiresAt().Before(now.Add(time.Hour)) {
		t.Fatalf("expiry = %v, want ~2h out", l.ExpiresAt())
	}
	found := false
	for _, h := range l.Holders {
		if h.Name == "bob" {
			found = true
		}
	}
	if !found {
		t.Fatal("holder bob not recorded")
	}

	// Unmanaged project: exit 2 with a message naming the project.
	out.Reset()
	if code := runExtend(&out, st, "ghost", "2h", "x"); code != 2 {
		t.Fatalf("extend unmanaged project exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "ghost") || !strings.Contains(out.String(), "managed") {
		t.Fatalf("exit-2 message = %q, want one naming the project and why", out.String())
	}

	// Project being removed: exit 3 with the design's message.
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = "removing"
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := runExtend(&out, st, "myapp", "2h", "x"); code != 3 {
		t.Fatalf("extend removing project exit code = %d, want 3", code)
	}
	if !strings.Contains(out.String(), "is being removed; run again when it is gone, it will start clean") {
		t.Fatalf("exit-3 message missing, got: %s", out.String())
	}

	// TTL outside 1m..168h: exit 1.
	out.Reset()
	if code := runExtend(&out, st, "myapp", "30s", "x"); code != 1 {
		t.Fatalf("extend short ttl exit code = %d, want 1", code)
	}

	// Unparseable ttl: exit 1.
	if code := runExtend(&out, st, "myapp", "soon", "x"); code != 1 {
		t.Fatalf("extend bad ttl exit code = %d, want 1", code)
	}
}

func TestExtendPrintsHolderExpiry(t *testing.T) {
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	now := time.Now().UTC()
	// alice holds a claim far out; bob extends for 2h. The message must
	// carry bob's own expiry, not the lease's overall (alice's) one.
	if err := st.Update(func(m map[string]lease.Lease) error {
		var err error
		m["myapp"], err = lease.Extend(m["myapp"], "alice", 10*time.Hour, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runExtend(&out, st, "myapp", "2h", "bob"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	leases, _ := st.Read()
	l := leases["myapp"]
	var bob time.Time
	for _, h := range l.Holders {
		if h.Name == "bob" {
			bob = h.Expires
		}
	}
	if bob.IsZero() {
		t.Fatal("bob not recorded")
	}
	if want := bob.Format(time.RFC3339); !strings.Contains(out.String(), want) {
		t.Fatalf("extend message = %q, want bob's own expiry %s in it", out.String(), want)
	}
	if got := l.ExpiresAt().Format(time.RFC3339); got != bob.Format(time.RFC3339) && strings.Contains(out.String(), got) {
		t.Fatalf("extend message = %q, carries the overall expiry %s instead of bob's %s", out.String(), got, bob.Format(time.RFC3339))
	}
}

func TestExtendNeverShortens(t *testing.T) {
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	extendLease(t, st, "myapp", 3*time.Hour, time.Now().UTC())

	var out bytes.Buffer
	if code := runExtend(&out, st, "myapp", "1h", "ci"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	leases, _ := st.Read()
	if got := time.Until(leases["myapp"].ExpiresAt()); got < 2*time.Hour {
		t.Fatalf("time left = %v, want >2h (a shorter extend must not shorten)", got)
	}
}

// Extending a project whose stop is in progress is refused: the reaper is
// mid-flight on it and a concurrent extend would race the stop.
func TestExtendRefusedWhileStopping(t *testing.T) {
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	extendLease(t, st, "myapp", time.Hour, time.Now().UTC())
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = lease.StateStopping
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runExtend(&out, st, "myapp", "2h", "x"); code != 3 {
		t.Fatalf("extend stopping project exit code = %d, want 3", code)
	}
	if !strings.Contains(out.String(), "is being stopped") {
		t.Fatalf("exit-3 message = %q, want the being-stopped notice", out.String())
	}
}

// Extending a stopped project revives it: the stopped mark and its time are
// cleared and the notice tells the developer to start the stack; dev-lease
// never starts containers itself.
func TestExtendOnStoppedClearsState(t *testing.T) {
	now := time.Now().UTC()
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	extendLease(t, st, "myapp", time.Minute, now.Add(-2*time.Minute)) // expired
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = lease.StateStopped
		l.StoppedAt = now.Add(-time.Hour)
		week := 7 * 24 * time.Hour
		l.RemoveAfter = &week
		l.RemoveAfterFrom = "label"
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runExtend(&out, st, "myapp", "2h", "bob"); code != 0 {
		t.Fatalf("extend stopped project exit code = %d, want 0 (%q)", code, out.String())
	}
	if !strings.Contains(out.String(), "docker compose up -d") {
		t.Fatalf("output = %q, want the start-the-stack notice", out.String())
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l := leases["myapp"]
	if l.State != "" || !l.StoppedAt.IsZero() {
		t.Fatalf("state = %q stopped-at %v, want the stopped mark cleared", l.State, l.StoppedAt)
	}
	if l.RemoveAfter != nil || l.RemoveAfterFrom != "" {
		t.Fatalf("recorded delay = %v from %q, want both cleared on a live lease", l.RemoveAfter, l.RemoveAfterFrom)
	}
	if l.Expired(now) {
		t.Fatal("revived lease is already expired")
	}
}

// leaseLine renders every state: live with time left and its live holders,
// stopping, stopped (with and without a removal time) and removing.
func TestLeaseLinePerState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	st := tempStore(t)
	extendLease(t, st, "myapp", 2*time.Hour, now)
	leases, _ := st.Read()
	l := leases["myapp"]

	want := fmt.Sprintf("live, 2h left (until %s; holders: ci)", l.ExpiresAt().Format(time.RFC3339))
	if got := leaseLine(l, now, 0); got != want {
		t.Fatalf("live line = %q, want %q", got, want)
	}

	// A holder whose claim expired is not listed.
	withStaleHolder := l
	withStaleHolder.Holders = append(withStaleHolder.Holders, lease.Holder{Name: "gone", Expires: now.Add(-time.Minute)})
	if got := leaseLine(withStaleHolder, now, 0); got != want {
		t.Fatalf("live line with a stale holder = %q, want %q", got, want)
	}

	stopped := l
	stopped.State = lease.StateStopped
	stopped.StoppedAt = now.Add(-2 * time.Hour)
	ra := 168 * time.Hour
	stopped.RemoveAfter = &ra
	removedAt := stopped.StoppedAt.Add(168 * time.Hour).Format(time.RFC3339)
	want = fmt.Sprintf("stopped 2h ago; removed in 6d22h (at %s)", removedAt)
	if got := leaseLine(stopped, now, 168*time.Hour); got != want {
		t.Fatalf("stopped line = %q, want %q", got, want)
	}
	// Without a stored value the fallback applies.
	never := stopped
	never.RemoveAfter = nil
	if got := leaseLine(never, now, 48*time.Hour); got == "stopped 2h ago; never removed" {
		t.Fatal("stopped line without a stored value ignored the fallback")
	}
	if got := leaseLine(never, now, 0); got != "stopped 2h ago; never removed" {
		t.Fatalf("stopped line without remove-after = %q, want 'stopped 2h ago; never removed'", got)
	}
	// The list follows the effective delay, with the header's remove-after
	// as serve's current setting: a serve-set delay is paused by a current
	// 0 and lengthened by a longer current setting; a label-set delay is
	// neither lengthened nor shortened.
	serveSet := stopped
	serveSet.RemoveAfterFrom = "serve"
	if got := leaseLine(serveSet, now, 0); got != "stopped 2h ago; never removed" {
		t.Fatalf("serve-set 168h under a current 0 = %q, want never removed", got)
	}
	longer := stopped.StoppedAt.Add(720 * time.Hour).Format(time.RFC3339)
	if got, w := leaseLine(serveSet, now, 720*time.Hour), fmt.Sprintf("stopped 2h ago; removed in 29d22h (at %s)", longer); got != w {
		t.Fatalf("serve-set 168h under a current 720h = %q, want %q", got, w)
	}
	label := stopped
	week := 7 * 24 * time.Hour
	label.RemoveAfter = &week
	label.RemoveAfterFrom = "label"
	if got, w := leaseLine(label, now, 720*time.Hour), fmt.Sprintf("stopped 2h ago; removed in 6d22h (at %s)", removedAt); got != w {
		t.Fatalf("label 7d under a current 720h = %q, want %q", got, w)
	}
	// A stored 0 (a remove-after label of 0) stays never, whatever the
	// fallback says.
	storedNever := stopped
	zero := time.Duration(0)
	storedNever.RemoveAfter = &zero
	if got := leaseLine(storedNever, now, 48*time.Hour); got != "stopped 2h ago; never removed" {
		t.Fatalf("stopped line with a stored 0 = %q, want 'stopped 2h ago; never removed'", got)
	}
	// Past the deadline (a removal that keeps failing): the line says
	// overdue instead of a negative time.
	overdue := stopped
	overdue.StoppedAt = now.Add(-10 * time.Hour)
	oneHour := time.Hour
	overdue.RemoveAfter = &oneHour
	if got := leaseLine(overdue, now, time.Hour); !strings.Contains(got, "removal overdue") {
		t.Fatalf("stopped line past the deadline = %q, want 'removal overdue'", got)
	}

	stopping := l
	stopping.State = lease.StateStopping
	if got := leaseLine(stopping, now, 0); got != "stopping" {
		t.Fatalf("stopping line = %q", got)
	}

	removing := l
	removing.State = lease.StateRemoving
	if got := leaseLine(removing, now, 0); got != "removing" {
		t.Fatalf("removing line = %q", got)
	}

	expired := tempStore(t)
	extendLease(t, expired, "myapp", time.Minute, now.Add(-2*time.Minute))
	expiredLeases, _ := expired.Read()
	if got := leaseLine(expiredLeases["myapp"], now, 0); !strings.Contains(got, "expired (last holder ci)") {
		t.Fatalf("expired line = %q", got)
	}
}

// FmtDur rounds down to minutes: <1m, 23m, 1h, 1h23m below two days,
// 2d, 6d22h from two days up.
func TestFmtDur(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "<1m"},
		{0, "<1m"},
		{59 * time.Second, "<1m"},
		{time.Minute, "1m"},
		{90 * time.Second, "1m"},
		{23 * time.Minute, "23m"},
		{time.Hour, "1h"},
		{83 * time.Minute, "1h23m"},
		{47*time.Hour + 59*time.Minute, "47h59m"},
		{48 * time.Hour, "2d"},
		{5*24*time.Hour + 13*time.Hour + 30*time.Minute, "5d13h"},
		{6*24*time.Hour + 22*time.Hour, "6d22h"},
	} {
		if got := view.FmtDur(tc.d); got != tc.want {
			t.Errorf("FmtDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestRelease(t *testing.T) {
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	extendLease(t, st, "myapp", time.Hour, time.Now().UTC())
	if err := st.Update(func(m map[string]lease.Lease) error {
		l, err := lease.Extend(m["myapp"], "other", time.Hour, time.Now().UTC())
		if err != nil {
			return err
		}
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runRelease(&out, st, "myapp", "ci", false); code != 0 {
		t.Fatalf("release one holder exit code = %d, want 0", code)
	}
	leases, _ := st.Read()
	if leases["myapp"].Expired(time.Now().UTC()) {
		t.Fatal("lease expired, but holder other is still live")
	}

	if code := runRelease(&out, st, "myapp", "", true); code != 0 {
		t.Fatalf("release --all exit code = %d, want 0", code)
	}
	leases, _ = st.Read()
	if !leases["myapp"].Expired(time.Now().UTC()) {
		t.Fatal("release --all left the lease live")
	}

	// Neither --holder nor --all is a usage error.
	if code := runRelease(&out, st, "myapp", "", false); code != 1 {
		t.Fatalf("release without flags exit code = %d, want 1", code)
	}
}

func TestReleaseWithoutLeaseIsANoOp(t *testing.T) {
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)

	var out bytes.Buffer
	if code := runRelease(&out, st, "myapp", "", true); code != 0 {
		t.Fatalf("release without a lease exit code = %d, want 0", code)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leases["myapp"]; ok {
		t.Fatal("releasing a nonexistent lease created one")
	}
	if !strings.Contains(out.String(), "nothing to release") {
		t.Fatalf("output = %q, want the nothing-to-release notice", out.String())
	}
}

func TestReleaseAllOnStoppedProjectStaysStopped(t *testing.T) {
	// release --all on a stopped project must not revive it and must say
	// when it is removed, or that it never is.
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	if err := st.UpdateHeader(func(h *lease.Header) error {
		h.RemoveAfter = 2 * time.Hour
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stopped := func(removeAfter time.Duration) {
		t.Helper()
		if err := st.Update(func(m map[string]lease.Lease) error {
			m["myapp"] = lease.Lease{
				Project:         "myapp",
				State:           lease.StateStopped,
				StoppedAt:       time.Now().UTC().Add(-5 * time.Minute),
				RemoveAfter:     &removeAfter,
				RemoveAfterFrom: "label",
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	stopped(time.Hour)
	var out bytes.Buffer
	if code := runRelease(&out, st, "myapp", "", true); code != 0 {
		t.Fatalf("release --all on a stopped project = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, "it stays stopped") {
		t.Fatalf("output = %q, missing the stays-stopped notice", got)
	}
	if !strings.Contains(got, "removed in") {
		t.Fatalf("output = %q, missing when the project is removed", got)
	}
	leases, _ := st.Read()
	if leases["myapp"].State != lease.StateStopped {
		t.Fatal("release --all revived a stopped project")
	}

	// Without a removal delay the line must say so.
	stopped(0)
	out.Reset()
	if code := runRelease(&out, st, "myapp", "", true); code != 0 {
		t.Fatalf("release --all on a never-removed project = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "never removed") {
		t.Fatalf("output = %q, missing the never-removed notice", out.String())
	}
}

func TestStatusExitsOneWhenListingFails(t *testing.T) {
	// status must exit 1 when the Docker listing cannot be read, so a
	// script can tell a healthy listing from a broken daemon.
	st := tempStore(t)
	t.Setenv("DEV_LEASE_STATE", st.Path)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	srv := fakedocker.New(t)
	srv.FailList("container", http.StatusInternalServerError)

	var out, errOut bytes.Buffer
	code := runStatusCmd([]string{"--socket", srv.Socket}, &out, &errOut)
	if code != 1 {
		t.Fatalf("status with a failing daemon listing = %d, want 1 (out=%q err=%q)", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "listing unavailable") {
		t.Fatalf("output = %q, missing the listing-unavailable notice", out.String())
	}
}

func TestParseDurationRemoveAfterBounds(t *testing.T) {
	// remove-after accepts Go durations and whole days up to 3650d; a bare
	// number, a leading + and days beyond 3650 are refused.
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"3650d", 3650 * 24 * time.Hour, true},
		{"3651d", 0, false},
		{"4000d", 0, false},
		{"5", 0, false},
		{"+2d", 0, false},
		{"0d", 0, false},
		{"7d", 7 * 24 * time.Hour, true},
		{"2h", 2 * time.Hour, true},
	} {
		got, err := parseDuration(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseDuration(%q) = %v, want an error", tc.in, got)
		}
	}
}

// A stopped lease without a recorded remove-after shows the header's
// remove-after in list, so the removal time stays truthful.
func TestListUsesHeaderRemoveAfterForStoppedLease(t *testing.T) {
	now := time.Now().UTC()
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	if err := st.UpdateHeader(func(h *lease.Header) error {
		h.RemoveAfter = 48 * time.Hour
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{
			Project:   "myapp",
			State:     lease.StateStopped,
			StoppedAt: now.Add(-2 * time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runList(&out, st); code != 0 {
		t.Fatalf("list = %d, want 0", code)
	}
	wantAt := now.Add(-2 * time.Hour).Add(48 * time.Hour).Format(time.RFC3339)
	if !strings.Contains(out.String(), "removed in") || !strings.Contains(out.String(), "(at "+wantAt+")") {
		t.Fatalf("list output = %q, want the removal time from the header (%s)", out.String(), wantAt)
	}
}

// serve writes remove_after into the state header: the flag default is
// 168h and DEV_LEASE_REMOVE_AFTER overrides it; an unparsable value is
// refused with exit 1.
func TestServeCmdEnvAndDefaultRemoveAfterInHeader(t *testing.T) {
	skipServeWhenRoot(t)
	run := func(env string) (int, time.Duration, string) {
		st := tempStore(t)
		srv := fakedocker.New(t)
		withOwnContainer(t, srv, "own", "dev-lease")
		// "" means unset for the code (the flag default applies), and
		// t.Setenv restores the inherited value afterwards.
		t.Setenv("DEV_LEASE_REMOVE_AFTER", env)
		var out, errOut bytes.Buffer
		code := runServeCmd([]string{
			"--projects", "myapp", "--state", st.Path, "--socket", srv.Socket,
			"--own-id", "own", "--exit-when-idle", "--interval", "1s",
		}, &out, &errOut)
		hdr, err := st.ReadHeader()
		if err != nil {
			return code, 0, errOut.String()
		}
		return code, hdr.RemoveAfter, errOut.String()
	}

	code, ra, errOut := run("")
	if code != 0 {
		t.Fatalf("serve with the default remove-after = %d, want 0 (%q)", code, errOut)
	}
	if ra != 168*time.Hour {
		t.Fatalf("header remove_after = %v, want the default 168h", ra)
	}

	code, ra, errOut = run("36h")
	if code != 0 {
		t.Fatalf("serve with DEV_LEASE_REMOVE_AFTER=36h = %d, want 0 (%q)", code, errOut)
	}
	if ra != 36*time.Hour {
		t.Fatalf("header remove_after = %v, want 36h from the environment", ra)
	}

	code, _, errOut = run("banana")
	if code != 1 {
		t.Fatalf("serve with DEV_LEASE_REMOVE_AFTER=banana = %d, want 1 (%q)", code, errOut)
	}
}

func TestFlagsAfterPositionals(t *testing.T) {
	st := tempStore(t)
	t.Setenv("DEV_LEASE_STATE", st.Path)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	extendLease(t, st, "myapp", time.Hour, time.Now().UTC())

	var out, errOut bytes.Buffer
	if code := runExtendCmd([]string{"myapp", "2h", "--holder", "bob"}, &out, &errOut); code != 0 {
		t.Fatalf("extend with flags after positionals = %d, want 0 (out=%q err=%q)", code, out.String(), errOut.String())
	}
	leases, _ := st.Read()
	found := false
	for _, h := range leases["myapp"].Holders {
		if h.Name == "bob" {
			found = true
		}
	}
	if !found {
		t.Fatal("holder from a flag after the positionals was not recorded")
	}

	if code := runReleaseCmd([]string{"myapp", "--all"}, &out, &errOut); code != 0 {
		t.Fatalf("release with flag after positional = %d, want 0 (out=%q err=%q)", code, out.String(), errOut.String())
	}
	leases, _ = st.Read()
	if !leases["myapp"].Expired(time.Now().UTC()) {
		t.Fatal("release --all after the positional left the lease live")
	}
}

func TestListPrintsLeases(t *testing.T) {
	st := tempStore(t)
	extendLease(t, st, "zeta", 2*time.Hour, time.Now().UTC())
	extendLease(t, st, "alpha", 30*time.Minute, time.Now().UTC())

	var out bytes.Buffer
	if code := runList(&out, st); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "zeta") {
		t.Fatalf("list output = %q, want both projects", got)
	}
	if !strings.Contains(got, "ci") {
		t.Fatalf("list output = %q, want the last holder", got)
	}
	if strings.Index(got, "alpha") > strings.Index(got, "zeta") {
		t.Fatalf("list output not sorted: %q", got)
	}
}

// --- serve ---

// skipServeWhenRoot skips a test that would reach privdrop: under root the
// privdrop setuids the whole test process, breaking every later test.
func skipServeWhenRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("privdrop would setuid the whole test process under root")
	}
}

func TestServeRefusesOwnProjectPattern(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "myapp")
	st := tempStore(t)

	var out, errOut bytes.Buffer
	err := runServe(context.Background(), serveConfig{
		OwnID:      "c-own",
		Projects:   []string{"myapp"},
		State:      st.Path,
		Socket:     srv.Socket,
		Interval:   time.Minute,
		DefaultTTL: 2 * time.Hour,
		Out:        &out,
		ErrOut:     &errOut,
	})
	if err == nil {
		t.Fatal("serve started with a pattern matching its own project")
	}
	if !strings.Contains(err.Error(), "own project") {
		t.Fatalf("error = %v, want mention of the own project", err)
	}
}

func TestServeRefusesStarPattern(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	st := tempStore(t)

	err := runServe(context.Background(), serveConfig{
		OwnID:      "c-own",
		Projects:   []string{"*"},
		State:      st.Path,
		Socket:     srv.Socket,
		Interval:   time.Minute,
		DefaultTTL: 2 * time.Hour,
		Out:        &bytes.Buffer{},
		ErrOut:     &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "own project") {
		t.Fatalf("error = %v, want refusal of the star pattern", err)
	}
}

// A serve lock that cannot even be opened is reported as the open failure,
// not as another serve holding the lock.
func TestServeLockOpenFailureIsReported(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	state := filepath.Join(t.TempDir(), "missing-dir", "leases.json")
	var out, errOut bytes.Buffer
	err := runServe(context.Background(), serveConfig{
		OwnID: "c-own", Projects: []string{"myapp"}, State: state,
		Socket: srv.Socket, Interval: time.Minute,
		Out: &out, ErrOut: &errOut,
	})
	if err == nil || !strings.Contains(err.Error(), "locking the serve lock "+state+".serve.lock") {
		t.Fatalf("error = %v, want a locking failure naming the lock file", err)
	}
	if strings.Contains(err.Error(), "another serve already holds") {
		t.Fatalf("error = %v, claims another serve holds the lock for an open failure", err)
	}
}

// Given a serve running on one state file, a second serve on the same file
// refuses to start with a message naming the lock file; after the first
// one released the lock, a serve starts again.
func TestServeLockIsExclusivePerStateFile(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	st := tempStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	var firstOut syncBuf
	go func() {
		firstDone <- runServe(ctx, serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: 10 * time.Millisecond,
			ExitWhenIdle: false, Out: &syncBuf{}, ErrOut: &firstOut,
		})
	}()
	// Wait until the first serve is running its loop: the lock is held by
	// then.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(firstOut.String(), "pass: ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first serve never reached its loop")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The first serve has written the header by now; a refused second serve
	// must leave it unchanged.
	hdrBefore, err := st.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}

	// The second serve is refused fast, not blocked on the lock: a select
	// with a timer keeps a blocking-flock regression from hanging the test.
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runServe(context.Background(), serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: time.Minute,
			ExitWhenIdle: false, Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{},
		})
	}()
	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "another serve already holds the lock") {
			t.Fatalf("second serve error = %v, want a refusal saying the lock is held", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second serve blocked on the lock instead of refusing fast")
	}
	hdrAfter, err := st.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hdrAfter, hdrBefore) {
		t.Fatalf("the refused serve changed the state header: before %+v, after %+v", hdrBefore, hdrAfter)
	}

	cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first serve: %v", err)
	}

	// With the lock released the serve starts again and exits as idle.
	restartDone := make(chan error, 1)
	var secondOut syncBuf
	go func() {
		restartDone <- runServe(context.Background(), serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: 10 * time.Millisecond,
			ExitWhenIdle: true, Out: &syncBuf{}, ErrOut: &secondOut,
		})
	}()
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("serve after the release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start after the lock was released")
	}
}

// Given a running serve, the control socket sits next to the state file,
// answers a stop, and is removed when serve exits.
func TestServeServesControlSocket(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "web", "myapp")
	st := tempStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var errOut syncBuf
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: 10 * time.Millisecond,
			ExitWhenIdle: false, Out: &syncBuf{}, ErrOut: &errOut,
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(errOut.String(), "pass: ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never reached its loop")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sock := control.SocketPath(st.Path)
	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	ans, err := control.Call(callCtx, sock, control.Request{Action: "stop", Project: "myapp"})
	if err != nil {
		t.Fatalf("control stop: %v", err)
	}
	if !ans.OK {
		t.Fatalf("answer = %+v, want ok", ans)
	}
	leases, err := st.Read()
	if err != nil || leases["myapp"].State != lease.StateStopped {
		t.Fatalf("lease = %+v (err %v), want stopped", leases["myapp"], err)
	}
	if !srv.IsStopped("container", "c1") {
		t.Fatal("the container still runs after the stop")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after serve exit: stat err %v, want it removed", err)
	}
}

// Given a container that does not carry the dev-lease.role=server label
// (nothing at all, or the client value), serve refuses: with
// network_mode: container:X the mountinfo /etc binds come from X, so only
// the role label proves the inspected container is the dev-lease service.
// The timeout makes a regression fail in seconds instead of serving.
func TestServeRefusesWithoutRoleLabel(t *testing.T) {
	skipServeWhenRoot(t)
	cases := map[string]map[string]string{
		"missing": {
			"com.docker.compose.project": "dev-lease",
			"com.docker.compose.service": "dev-lease",
		},
		"client": {
			"com.docker.compose.project": "dev-lease",
			"com.docker.compose.service": "dev-lease",
			"dev-lease.role":             "client",
		},
	}
	for name, labels := range cases {
		t.Run(name, func(t *testing.T) {
			srv := fakedocker.New(t)
			srv.AddLabels("container", "c-own", "dev-lease", labels)
			st := tempStore(t)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := runServe(ctx, serveConfig{
				OwnID:      "c-own",
				Projects:   []string{"myapp"},
				State:      st.Path,
				Socket:     srv.Socket,
				Interval:   time.Minute,
				DefaultTTL: 2 * time.Hour,
				Out:        &bytes.Buffer{},
				ErrOut:     &bytes.Buffer{},
			})
			if err == nil || !strings.Contains(err.Error(), "dev-lease.role") {
				t.Fatalf("error = %v, want a refusal naming the dev-lease.role label", err)
			}
		})
	}
}

// Given another running container that carries dev-lease.role=server on
// the same daemon, serve refuses: the state-file lock only covers one
// state file, and two serves on one daemon would stop and remove projects
// behind each other's back. Serve starts when that container is stopped
// or absent. The timeout makes a regression fail in seconds instead of
// serving.
func TestServeRefusesAlongsideAnotherServer(t *testing.T) {
	skipServeWhenRoot(t)
	ownLabels := map[string]string{
		"com.docker.compose.project": "dev-lease",
		"com.docker.compose.service": "dev-lease",
		"dev-lease.role":             "server",
	}
	run := func(srv *fakedocker.Server) error {
		st := tempStore(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return runServe(ctx, serveConfig{
			OwnID:      "c-own",
			Projects:   []string{"myapp"},
			State:      st.Path,
			Socket:     srv.Socket,
			Interval:   time.Minute,
			DefaultTTL: 2 * time.Hour,
			Out:        &bytes.Buffer{},
			ErrOut:     &bytes.Buffer{},
		})
	}

	t.Run("another running server container", func(t *testing.T) {
		srv := fakedocker.New(t)
		srv.AddLabels("container", "c-own", "dev-lease", ownLabels)
		srv.AddLabels("container", "c-other", "dev-lease-old", map[string]string{
			"com.docker.compose.project": "dev-lease-old",
			"dev-lease.role":             "server",
		})
		err := run(srv)
		if err == nil || !strings.Contains(err.Error(), "dev-lease-old") {
			t.Fatalf("error = %v, want a refusal naming the other server container", err)
		}
	})

	t.Run("the other server container is stopped", func(t *testing.T) {
		srv := fakedocker.New(t)
		srv.AddLabels("container", "c-own", "dev-lease", ownLabels)
		srv.AddLabels("container", "c-other", "dev-lease-old", map[string]string{
			"com.docker.compose.project": "dev-lease-old",
			"dev-lease.role":             "server",
		})
		srv.StopContainer("c-other")
		if err := run(srv); err != nil {
			t.Fatalf("serve with a stopped other server: %v", err)
		}
	})

	t.Run("no other server container", func(t *testing.T) {
		srv := fakedocker.New(t)
		srv.AddLabels("container", "c-own", "dev-lease", ownLabels)
		if err := run(srv); err != nil {
			t.Fatalf("serve alone on the daemon: %v", err)
		}
	})
}

// syncBuf is a mutex-guarded buffer for cross-goroutine writes and reads.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestServeRetriesWhenDockerIsDown(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	st := tempStore(t)
	extendLease(t, st, "myapp", time.Hour, time.Now().UTC())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut syncBuf
	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID:        "c-own",
			Projects:     []string{"myapp"},
			State:        st.Path,
			Socket:       srv.Socket,
			Interval:     10 * time.Millisecond,
			DefaultTTL:   time.Hour,
			ExitWhenIdle: false,
			Out:          &out,
			ErrOut:       &errOut,
		})
	}()

	// Wait until the first pass happened, then snapshot the state (the serve
	// header is written by then) and kill the daemon.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(errOut.String(), "pass:") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(errOut.String(), "pass:") {
		t.Fatal("serve never ran a pass")
	}
	before := readFileString(t, st.Path)
	srv.Close()

	// The serve loop must notice the failure, log it and keep retrying.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(errOut.String(), "pass failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(errOut.String(), "pass failed") {
		t.Fatalf("serve did not log a failed pass after the daemon went away: %q", errOut.String())
	}
	// The state file is never deleted or truncated by a failed pass.
	if got := readFileString(t, st.Path); got != before {
		t.Fatalf("state file changed across failed passes:\nbefore: %s\nafter:  %s", before, got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve exited with error: %v", err)
	}
}

// Given a socket the process cannot open (EACCES on connect), serve must
// fail with a clear error, not retry forever as if the daemon were down.
func TestServePermissionDeniedIsNotUnreachable(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	if err := os.Chmod(srv.Socket, 0o500); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var errOut syncBuf
	err := runServe(ctx, serveConfig{OwnID: "c-own", Projects: []string{"myapp"}, State: tempStore(t).Path,
		Socket: srv.Socket, Interval: 50 * time.Millisecond, Out: &syncBuf{}, ErrOut: &errOut})
	if err == nil {
		t.Fatal("serve kept retrying a permission error until the context ended")
	}
	// The exact message: the socket path, its mode and owner, and the uid
	// and gid the process runs as after the privilege drop, so the operator
	// can compare both sides of the socket permission on the host.
	want := fmt.Sprintf("permission denied on the Docker socket %s (mode %s, uid %d, gid %d): serve must be able to open it - it keeps the socket's group when dropping privileges and now runs as uid %d gid %d: Get \"http://docker/version\": dial unix %s: connect: permission denied",
		srv.Socket, os.FileMode(0o500).Perm(), os.Getuid(), os.Getgid(), os.Getuid(), os.Getgid(), srv.Socket)
	if err.Error() != want {
		t.Fatalf("serve error = %q, want %q", err.Error(), want)
	}
}

// Given --exit-when-idle=false, serve must stay up with nothing managed.
// The split-argument form (--exit-when-idle false) is refused
// below: a bool flag never consumes the next argument, so it becomes a
// positional and serve names it.
func TestServeStaysUpWithExitWhenIdleFalse(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	args := []string{"--projects", "myapp", "--interval", "1s", "--socket", srv.Socket,
		"--state", tempStore(t).Path, "--own-id", "c-own", "--exit-when-idle=false"}
	done := make(chan int, 1)
	var errOut syncBuf
	go func() { done <- runServeCmd(args, &bytes.Buffer{}, &errOut) }()
	select {
	case code := <-done:
		t.Fatalf("serve exited (code %d) although --exit-when-idle=false; stderr: %q", code, errOut.String())
	case <-time.After(1500 * time.Millisecond):
	}
	// Stop it: serve's NotifyContext catches SIGTERM for this process.
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit code %d on SIGTERM, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop on SIGTERM")
	}
}

// Always-on is the default: without --exit-when-idle, serve must stay up
// with nothing managed, so it comes back with Docker after a reboot.
func TestServeStaysUpByDefault(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	args := []string{"--projects", "myapp", "--interval", "1s", "--socket", srv.Socket,
		"--state", tempStore(t).Path, "--own-id", "c-own"}
	done := make(chan int, 1)
	var errOut syncBuf
	go func() { done <- runServeCmd(args, &bytes.Buffer{}, &errOut) }()
	select {
	case code := <-done:
		t.Fatalf("serve exited (code %d) although --exit-when-idle defaults to false; stderr: %q", code, errOut.String())
	case <-time.After(1500 * time.Millisecond):
	}
	// Stop it: serve's NotifyContext catches SIGTERM for this process.
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit code %d on SIGTERM, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop on SIGTERM")
	}
}

// Given a bool flag written as two arguments, the value lands among the
// positionals and serve refuses it, naming the argument (exit 1). A plain
// positional is refused the same way.
func TestServeRefusesPositionalArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"split bool value", []string{"--projects", "myapp", "--exit-when-idle", "false"}, "false"},
		{"plain positional", []string{"--projects", "myapp", "leftover"}, "leftover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut syncBuf
			code := runServeCmd(tc.args, &bytes.Buffer{}, &errOut)
			// 1 is a refusal to start (2 would mean a project is not
			// managed, a client-side answer that makes no sense for serve).
			if code != 1 {
				t.Fatalf("serve exit code = %d, want 1; stderr: %q", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("serve stderr = %q, want it to name %q", errOut.String(), tc.want)
			}
		})
	}
}

func TestReorderArgsSkipsValuesForBoolFlags(t *testing.T) {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.Bool("all", false, "")
	fs.String("holder", "", "")
	got := reorderArgs(fs, []string{"myapp", "--all", "--holder", "ci"})
	want := []string{"--all", "--holder", "ci", "myapp"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("reorderArgs = %q, want %q: a bool flag consumed the next flag as its value", got, want)
	}
}

func TestExitsWhenIdle(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "ghost-web", "ghost") // present but not managed
	st := tempStore(t)

	var out, errOut bytes.Buffer
	err := runServe(context.Background(), serveConfig{
		OwnID:        "c-own",
		Projects:     []string{"myapp"},
		State:        st.Path,
		Socket:       srv.Socket,
		Interval:     10 * time.Millisecond,
		ExitWhenIdle: true,
		Out:          &out,
		ErrOut:       &errOut,
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	// The idle notice is a status line, so it goes to stderr.
	if !strings.Contains(errOut.String(), "idle") {
		t.Fatalf("stderr = %q, want the idle notice", errOut.String())
	}
}

// Given no managed project and no lease, serve must still not exit before
// one full interval has passed: started on demand (up -d --wait), its first
// pass runs at once and would otherwise idle-exit before the caller's
// extend arrives.
func TestServeDoesNotExitIdleBeforeOneInterval(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	st := tempStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var errOut syncBuf
	started := time.Now()
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: 300 * time.Millisecond,
			ExitWhenIdle: true, Out: &syncBuf{}, ErrOut: &errOut,
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("serve exited after %s, before one full interval: %v", time.Since(started), err)
	case <-time.After(100 * time.Millisecond):
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		if !strings.Contains(errOut.String(), "idle") {
			t.Fatalf("stderr = %q, want the idle notice", errOut.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not idle-exit after one interval")
	}
}

func TestIdleIgnoresUnmanagedLeases(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "ghost-web", "ghost")
	st := tempStore(t)
	extendLease(t, st, "ghost", time.Hour, time.Now().UTC()) // unmanaged lease

	var out, errOut bytes.Buffer
	err := runServe(context.Background(), serveConfig{
		OwnID:        "c-own",
		Projects:     []string{"myapp"},
		State:        st.Path,
		Socket:       srv.Socket,
		Interval:     10 * time.Millisecond,
		ExitWhenIdle: true,
		Out:          &out,
		ErrOut:       &errOut,
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	// An unmanaged lease must not keep serve alive: nothing is managed, so
	// exit-when-idle applies.
	if !strings.Contains(errOut.String(), "idle") {
		t.Fatalf("stderr = %q, want the idle notice", errOut.String())
	}
}

// managedIdle: serve may exit only when no managed project has a live
// lease, none has a running container, and (with remove-after > 0) none is
// stopped waiting for its removal time.
func TestManagedIdle(t *testing.T) {
	srv := fakedocker.New(t)
	srv.Add("container", "c1", "web", "myapp")
	srv.Add("container", "c2", "db", "myapp")
	srv.StopContainer("c2")
	srv.Add("container", "c3", "ghost-web", "ghost")
	cli := docker.New(srv.Socket)
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := reaper.Config{Patterns: []string{"myapp"}}

	if managedIdle(ctx, cli, nil, cfg, now) {
		t.Fatal("running managed container without leases: want not idle")
	}
	srv.StopContainer("c1")
	if !managedIdle(ctx, cli, nil, cfg, now) {
		t.Fatal("only stopped or unmanaged containers: want idle")
	}

	st := tempStore(t)
	extendLease(t, st, "myapp", 2*time.Hour, now)
	extendLease(t, st, "ghost", 2*time.Hour, now)
	leases, _ := st.Read()
	if managedIdle(ctx, cli, leases, cfg, now) {
		t.Fatal("live managed lease: want not idle")
	}
	if !managedIdle(ctx, cli, map[string]lease.Lease{"ghost": leases["ghost"]}, cfg, now) {
		t.Fatal("only an unmanaged live lease: want idle")
	}

	cfg = reaper.Config{Patterns: []string{"myapp"}, RemoveAfter: 168 * time.Hour}
	stopped := leases["myapp"]
	stopped.Holders = []lease.Holder{{Name: "ci", ExtendedAt: now.Add(-2 * time.Minute)}}
	stopped.State = lease.StateStopped
	stopped.StoppedAt = now.Add(-time.Hour)
	ra := 168 * time.Hour
	stopped.RemoveAfter = &ra
	if managedIdle(ctx, cli, map[string]lease.Lease{"myapp": stopped}, cfg, now) {
		t.Fatal("stopped lease with remove-after > 0: want not idle")
	}
	// A lease without a recorded delay is serve-set with
	// serve's current setting: under serve's 168h it still waits, so it
	// keeps serve alive too.
	withoutStored := stopped
	withoutStored.RemoveAfter = nil
	withoutStored.RemoveAfterFrom = ""
	if managedIdle(ctx, cli, map[string]lease.Lease{"myapp": withoutStored}, cfg, now) {
		t.Fatal("stopped lease without a stored delay under serve's 168h: want not idle")
	}
	never := stopped
	zero := time.Duration(0)
	never.RemoveAfter = &zero
	if !managedIdle(ctx, cli, map[string]lease.Lease{"myapp": never}, cfg, now) {
		t.Fatal("stopped lease with stored remove-after 0: want idle")
	}
	// serve's current 0 is the global off switch: even a stored 168h does
	// not keep serve alive then.
	off := reaper.Config{Patterns: []string{"myapp"}}
	if !managedIdle(ctx, cli, map[string]lease.Lease{"myapp": stopped}, off, now) {
		t.Fatal("stopped lease under serve's remove-after 0: want idle")
	}
}

// A running kept container does not keep serve alive: it stays running by
// design, so it is not serve's business.
func TestManagedIdleIgnoresKeptContainers(t *testing.T) {
	srv := fakedocker.New(t)
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	}, "sha256:i1", nil, nil)
	cli := docker.New(srv.Socket)
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := reaper.Config{Patterns: []string{"myapp"}}

	if !managedIdle(ctx, cli, nil, cfg, now) {
		t.Fatal("only a kept running container: want idle")
	}
}

// A stopped lease with remove-after > 0 keeps serve alive while the
// project still has objects on the daemon: removing the stopped project
// once its removal time comes is still serve's work.
func TestServeStaysAliveWithStoppedLeaseAndRemoveAfter(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "web", "myapp")
	srv.StopContainer("c1")
	st := tempStore(t)
	extendLease(t, st, "myapp", time.Minute, time.Now().UTC().Add(-2*time.Minute))
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = lease.StateStopped
		l.StoppedAt = time.Now().UTC().Add(-time.Hour)
		ra := 168 * time.Hour
		l.RemoveAfter = &ra
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: 10 * time.Millisecond,
			RemoveAfter: 168 * time.Hour, ExitWhenIdle: true,
			Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{},
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("serve exited although a stopped lease awaits removal: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// With remove-after 0 (never remove) a stopped project is the operator's
// business; serve has no removal work left and exits as idle.
func TestServeExitsIdleWithStoppedLeaseAndNoRemoveAfter(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	st := tempStore(t)
	extendLease(t, st, "myapp", time.Minute, time.Now().UTC().Add(-2*time.Minute))
	if err := st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.State = lease.StateStopped
		l.StoppedAt = time.Now().UTC().Add(-time.Hour)
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var errOut bytes.Buffer
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: 10 * time.Millisecond,
			RemoveAfter: 0, ExitWhenIdle: true,
			Out: &bytes.Buffer{}, ErrOut: &errOut,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		if !strings.Contains(errOut.String(), "idle") {
			t.Fatalf("stderr = %q, want the idle notice", errOut.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not idle-exit although remove-after is 0")
	}
}

func TestServeRetriesAtStartWhenDaemonDown(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Close() // daemon is down before serve starts
	st := tempStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut syncBuf
	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID:        "c-own",
			Projects:     []string{"myapp"},
			State:        st.Path,
			Socket:       srv.Socket,
			Interval:     10 * time.Millisecond,
			ExitWhenIdle: false,
			Out:          &out,
			ErrOut:       &errOut,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(errOut.String(), "daemon unreachable") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(errOut.String(), "daemon unreachable") {
		t.Fatalf("serve did not retry at start: %q", errOut.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve exited with error after cancel: %v", err)
	}
}

func TestServeStopsOnCorruptState(t *testing.T) {
	skipServeWhenRoot(t)
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	st := tempStore(t)
	if err := os.WriteFile(st.Path, []byte("{ not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runServe(context.Background(), serveConfig{
		OwnID:    "c-own",
		Projects: []string{"myapp"},
		State:    st.Path,
		Socket:   srv.Socket,
		Out:      &bytes.Buffer{},
		ErrOut:   &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("serve started with a corrupt state file")
	}
}

func TestStartListing(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "web", "myapp")
	srv.Add("container", "c2", "db", "myapp")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")
	srv.Add("image", "sha256:i1", "myapp-web:latest", "myapp")
	srv.Add("container", "c3", "ghost-web", "ghost")
	st := tempStore(t)

	var out bytes.Buffer
	startListing(context.Background(), docker.New(srv.Socket), st, []string{"myapp"}, "dev-lease", &out)
	got := out.String()
	for _, want := range []string{"myapp", "web", "db", "myapp_default", "myapp_data", "myapp-web:latest", "no lease"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing = %q, missing %q", got, want)
		}
	}
	if !strings.Contains(got, "not managed: ghost") {
		t.Fatalf("listing = %q, missing the unmanaged line", got)
	}
	if !strings.Contains(got, "not managed: dev-lease") {
		t.Fatalf("listing = %q, missing the own project line", got)
	}
}

func TestStartListingWithLease(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "web", "myapp")
	st := tempStore(t)
	extendLease(t, st, "myapp", 2*time.Hour, time.Now().UTC())

	var out bytes.Buffer
	startListing(context.Background(), docker.New(srv.Socket), st, []string{"myapp"}, "dev-lease", &out)
	got := out.String()
	if !strings.Contains(got, "ci") {
		t.Fatalf("listing = %q, missing the holder", got)
	}
	if strings.Contains(got, "no lease") {
		t.Fatalf("listing = %q, says no lease although one exists", got)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return fmt.Sprint(string(data))
}

// A negative remove-after is refused before any daemon call.
func TestServeCmdRefusesNegativeRemoveAfter(t *testing.T) {
	var errOut bytes.Buffer
	code := runServeCmd([]string{"--projects", "myapp", "--remove-after", "-1h"}, &bytes.Buffer{}, &errOut)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "--remove-after") || !strings.Contains(errOut.String(), "negative") {
		t.Fatalf("stderr = %q, want a message naming --remove-after and that it must not be negative", errOut.String())
	}
}

// --- durations ---

// Every duration input accepts whole days (7d) on top of Go durations.
// Days must be whole and positive: fractions, a bare d and negative days
// are refused.
func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"2h", 2 * time.Hour},
		{"1m", time.Minute},
		{"7d", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"365d", 365 * 24 * time.Hour},
	} {
		got, err := parseDuration(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"1.5d", "d", "-1d", "0d", "soon"} {
		if got, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = %v, want an error", in, got)
		}
	}
}

// Given a remove-after below 1h (but above zero), serve refuses: removing
// after a shorter wait than an hour makes no sense with hour-long leases.
func TestServeCmdRefusesRemoveAfterBelowOneHour(t *testing.T) {
	var errOut bytes.Buffer
	code := runServeCmd([]string{"--projects", "myapp", "--remove-after", "30m"}, &bytes.Buffer{}, &errOut)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "--remove-after") || !strings.Contains(errOut.String(), "1h") {
		t.Fatalf("stderr = %q, want a message naming --remove-after and its 1h bound", errOut.String())
	}
}

// The --remove-after flag accepts 0 (never), hour values and whole days;
// bad values are refused by the flag itself.
func TestRemoveAfterFlagValues(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{"1h", time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"1.5d", 0, false},
		{"x", 0, false},
	} {
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var got time.Duration
		fs.Var(&durationValue{&got}, "remove-after", "")
		err := fs.Parse([]string{"--remove-after", tc.in})
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("--remove-after %s = %v, %v; want %v", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("--remove-after %s accepted as %v, want a refusal", tc.in, got)
		}
	}
}

// A serve killed with SIGKILL leaves control.sock behind; the next serve
// replaces the stale socket and starts normally.
func TestServeStartsWithStaleControlSocket(t *testing.T) {
	for _, stale := range []bool{false, true} {
		srv := fakedocker.New(t)
		withOwnContainer(t, srv, "c-own", "dev-lease")
		st := tempStore(t)
		if stale {
			ln, err := net.Listen("unix", control.SocketPath(st.Path))
			if err != nil {
				t.Fatal(err)
			}
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			ln.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		err := runServe(ctx, serveConfig{
			OwnID: "c-own", Projects: []string{"myapp"}, State: st.Path,
			Socket: srv.Socket, Interval: 10 * time.Millisecond,
			Out: &syncBuf{}, ErrOut: &syncBuf{},
		})
		cancel()
		if err != nil {
			t.Errorf("serve refused to start (stale=%v): %v", stale, err)
		}
	}
}

// --- dashboard ---

// waitForDashboardAddr polls serve's ErrOut for the listening line and
// returns the bound address.
func waitForDashboardAddr(t *testing.T, errOut *syncBuf) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := errOut.String()
		const marker = "dashboard: listening on "
		if i := strings.Index(out, marker); i >= 0 {
			line := out[i+len(marker):]
			if j := strings.IndexByte(line, '\n'); j >= 0 {
				line = line[:j]
			}
			if line != "" {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the dashboard never listened; serve output: %q", errOut.String())
	return ""
}

// Given no --dashboard flag and no DEV_LEASE_DASHBOARD, serve runs without
// a dashboard: nothing listens on the example's published port and no
// listening line appears. The idle exit bounds the run: no managed
// projects exist, so serve leaves after the first interval.
func TestServeDashboardOffByDefault(t *testing.T) {
	t.Setenv("DEV_LEASE_DASHBOARD", "")
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	var errOut bytes.Buffer
	code := runServeCmd([]string{
		"--projects", "myapp", "--socket", srv.Socket, "--state", tempStore(t).Path,
		"--own-id", "c-own", "--interval", "1s", "--exit-when-idle",
	}, &bytes.Buffer{}, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q; want 0", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "dashboard: listening") {
		t.Fatalf("serve started a dashboard without --dashboard: %q", errOut.String())
	}
}

// Given --dashboard with an ephemeral port, serve answers on it: a known
// Host gets the listing, a foreign Host gets 403, and cancelling serve's
// context closes the listener.
func TestServeDashboardServesAndStops(t *testing.T) {
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
	base := "http://" + addr

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200; body: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `href="/stacks/myapp"`) {
		t.Fatalf("the page does not link the managed stack: %s", b)
	}

	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	refused, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET / with a foreign Host: %v", err)
	}
	refused.Body.Close()
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host = %d, want 403", refused.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Fatal("the dashboard still answers after serve stopped")
	}
}

// the link checks fail on an empty daemon, so the smoke step and
// this test cannot pass vacuously when nothing is listed.
func TestServeDashboardLinkCheckFailsOnEmptyDaemon(t *testing.T) {
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
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
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := t.TempDir() + "/empty.html"
	if err := os.WriteFile(page, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// The check the CI smoke step runs: the link, not the name.
	cmd := exec.Command("grep", "-q", `href="/stacks/devlease-ci-target"`, page)
	if err := cmd.Run(); err == nil {
		t.Fatal("the link grep passed on an empty daemon; the smoke check cannot fail")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// Given a --dashboard address that cannot be listened on, serve refuses
// with a message naming the dashboard and the address; the env variable
// is the same refusal.
func TestServeDashboardBadAddressRefuses(t *testing.T) {
	t.Run("the flag", func(t *testing.T) {
		srv := fakedocker.New(t)
		withOwnContainer(t, srv, "c-own", "dev-lease")
		var errOut bytes.Buffer
		code := runServeCmd([]string{
			"--projects", "myapp", "--socket", srv.Socket, "--state", tempStore(t).Path,
			"--own-id", "c-own", "--interval", "1s", "--dashboard", "127.0.0.1:99999",
		}, &bytes.Buffer{}, &errOut)
		if code != 1 {
			t.Fatalf("exit = %d, stderr = %q; want 1", code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "dashboard") || !strings.Contains(errOut.String(), "127.0.0.1:99999") {
			t.Fatalf("stderr = %q, want a refusal naming the dashboard and the address", errOut.String())
		}
	})
	t.Run("the env variable", func(t *testing.T) {
		t.Setenv("DEV_LEASE_DASHBOARD", "127.0.0.1:99999")
		srv := fakedocker.New(t)
		withOwnContainer(t, srv, "c-own", "dev-lease")
		var errOut bytes.Buffer
		code := runServeCmd([]string{
			"--projects", "myapp", "--socket", srv.Socket, "--state", tempStore(t).Path,
			"--own-id", "c-own", "--interval", "1s",
		}, &bytes.Buffer{}, &errOut)
		if code != 1 {
			t.Fatalf("exit = %d, stderr = %q; want 1", code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "127.0.0.1:99999") {
			t.Fatalf("stderr = %q, want a refusal naming the address", errOut.String())
		}
	})
}

// Given a token, serve never writes it to its log, its errors or any
// answer: the refusal for a foreign Host, the redirect without a session,
// the login page, a wrong login and the successful login all stay free of
// the token's value.
func TestServeNeverLogsToken(t *testing.T) {
	const token = "dz-token-7f3a-4c21-dashboard"
	srv := fakedocker.New(t)
	withOwnContainer(t, srv, "c-own", "dev-lease")
	srv.Add("container", "c1", "myapp_web_1", "myapp")
	st := tempStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut syncBuf
	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, serveConfig{
			OwnID:          "c-own",
			Projects:       []string{"myapp"},
			State:          st.Path,
			Socket:         srv.Socket,
			Interval:       10 * time.Millisecond,
			Dashboard:      "127.0.0.1:0",
			DashboardToken: token,
			Out:            &out,
			ErrOut:         &errOut,
		})
	}()
	addr := waitForDashboardAddr(t, &errOut)
	base := "http://" + addr

	leaks := map[string]string{}
	check := func(what, s string) {
		t.Helper()
		if strings.Contains(s, token) {
			leaks[what] = s
		}
	}
	foreign, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Host = "evil.example"
	if resp, err := http.DefaultClient.Do(foreign); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		check("the foreign Host refusal", string(b))
	} else {
		t.Fatal(err)
	}

	// The session redirect must be observed, not followed: the login page
	// is a 200 behind it.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	root, err := noRedirect.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	root.Body.Close()
	check("the session redirect Location", root.Header.Get("Location"))
	if root.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / without a cookie = %d, want 303 to /login", root.StatusCode)
	}
	login, err := http.Get(base + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	loginBody, _ := io.ReadAll(login.Body)
	login.Body.Close()
	check("the login page", string(loginBody))
	csrfRE := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)
	m := csrfRE.FindSubmatch(loginBody)
	if m == nil {
		t.Fatal("the login page carries no csrf field")
	}
	csrf := string(m[1])

	post := func(form string, host string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+"/login", strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", base)
		if host != "" {
			req.Host = host
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}
	if resp, b := post("csrf="+csrf+"&token=wrong-token", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong login = %d, want 401", resp.StatusCode)
	} else {
		check("the wrong-login answer", string(b))
	}
	if resp, b := post("csrf="+csrf+"&token="+token, ""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("right login = %d, want 303", resp.StatusCode)
	} else {
		check("the right-login answer", string(b))
		check("the right-login Location", resp.Header.Get("Location"))
		for _, c := range resp.Cookies() {
			check("a login cookie", c.Value)
		}
	}

	cancel()
	serveErr := <-done
	if serveErr != nil {
		t.Fatalf("serve: %v", serveErr)
	}
	check("serve's log", errOut.String())
	check("serve's stdout", out.String())
	if len(leaks) > 0 {
		for what, s := range leaks {
			t.Errorf("the token appears in %s: %q", what, s)
		}
	}
}

// every --dashboard-hosts entry is a host, host:port, [ipv6] or
// [ipv6]:port, lowercased; wildcards, paths, schemes and other
// punctuation are refused with a message naming the entry.
func TestNormalizeHostsValidation(t *testing.T) {
	valid := []struct{ in, want string }{
		{"127.0.0.1", "127.0.0.1"},
		{"127.0.0.1:3000", "127.0.0.1:3000"},
		{"[::1]", "[::1]"},
		{"[::1]:3000", "[::1]:3000"},
		{"LocalHost", "localhost"},
		{"100.101.102.103:8410", "100.101.102.103:8410"},
		{"myapp.tail1234.ts.net", "myapp.tail1234.ts.net"},
	}
	for _, tc := range valid {
		got, err := normalizeHosts(tc.in)
		if err != nil || len(got) != 1 || got[0] != tc.want {
			t.Errorf("normalizeHosts(%q) = %v, %v; want [%q]", tc.in, got, err, tc.want)
		}
	}
	refused := []string{
		"*", "evil.example/foo", "http://evil.example", "https://evil.example",
		"127.0.0.1:99999", "host:port", "::1", "[::1",
		"evil.example/path:80", "-evil.example", "evil..example:80",
	}
	for _, in := range refused {
		_, err := normalizeHosts(in)
		if err == nil || !strings.Contains(err.Error(), in) {
			t.Errorf("normalizeHosts(%q) err = %v; want a refusal naming the entry", in, err)
		}
	}
}
