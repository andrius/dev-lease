package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/dashboard"
	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

type serveConfig struct {
	Projects     []string
	OwnID        string
	State        string
	Socket       string
	Interval     time.Duration
	DefaultTTL   time.Duration
	RemoveAfter  time.Duration
	StopTimeout  time.Duration
	ExitWhenIdle bool
	Out          io.Writer
	ErrOut       io.Writer

	// Dashboard is the TCP address the dashboard listens on; empty means
	// off. DashboardHosts is the Host allowlist (empty: the dashboard's
	// default). DashboardToken is the login token read from the
	// DEV_LEASE_DASHBOARD_TOKEN env var only: it is never a flag and must
	// never appear in logs, errors or answers.
	Dashboard      string
	DashboardHosts []string
	DashboardToken string
}

// runServe starts dev-lease serve: validate patterns against the own
// project, record the header, print the start listing, then sweep on every
// interval until idle (when --exit-when-idle) or ctx is cancelled.
// permissionDenied describes a definitive socket denial with the facts the
// operator needs on the host: the socket path, its mode and owner, and the
// uid/gid serve runs as after the privilege drop.
func permissionDenied(socket string, err error) error {
	return fmt.Errorf("permission denied on the Docker socket %s (%s): serve must be able to open it - it keeps the socket's group when dropping privileges and now runs as uid %d gid %d: %w",
		socket, socketInfo(socket), os.Getuid(), os.Getgid(), err)
}

// socketInfo describes the Docker socket file for the permission message.
func socketInfo(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("cannot stat: %v", err)
	}
	if uid, gid, ok := socketUIDGID(fi); ok {
		return fmt.Sprintf("mode %s, uid %d, gid %d", fi.Mode().Perm(), uid, gid)
	}
	return fmt.Sprintf("mode %s", fi.Mode().Perm())
}

func runServe(ctx context.Context, cfg serveConfig) error {
	if len(cfg.Projects) == 0 {
		return fmt.Errorf("no --projects given")
	}
	for _, p := range cfg.Projects {
		if _, err := path.Match(p, "probe"); err != nil {
			return fmt.Errorf("bad pattern %q: %w", p, err)
		}
	}
	if cfg.Interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s", cfg.Interval)
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 2 * time.Hour // the flag default
	}
	if err := lease.ValidateTTL(cfg.DefaultTTL); err != nil {
		return fmt.Errorf("--default-ttl: %w", err)
	}

	// Drop root before any Docker call: the daemon socket's group is kept.
	if err := privdrop(cfg.Socket); err != nil {
		return fmt.Errorf("dropping privileges: %w", err)
	}

	// One serve per state file: the exclusive flock on the serve lock
	// file is held for the whole life of this serve, and the kernel
	// releases it when serve dies, so the stale-removing rule below stays
	// sound (a removing lease can only belong to a died serve).
	releaseLock, err := lockServeFile(cfg.State + ".serve.lock")
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("refusing to start: another serve already holds the lock %s for the state file %s", cfg.State+".serve.lock", cfg.State)
		}
		return fmt.Errorf("locking the serve lock %s: %w", cfg.State+".serve.lock", err)
	}
	defer releaseLock()

	cli := docker.New(cfg.Socket)
	ownID := cfg.OwnID
	if ownID == "" {
		var err error
		ownID, err = detectOwnID()
		if err != nil {
			// Without a reliable id there is no safe own-project check:
			// refuse to start instead of guessing.
			return err
		}
	}
	cli.PinOwnID(ownID)

	// The daemon may still be booting when serve starts; retry until it
	// answers or the context ends. A definitive answer (404, permission) is
	// returned as an error instead. The inspection asks about the container
	// /proc mountinfo says dev-lease lives in, and the answer is checked
	// against that id.
	var ownLabels map[string]string
	var ownFullID string
	for {
		var err error
		ownLabels, ownFullID, err = cli.OwnContainer(ctx, ownID)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		if docker.IsPermission(err) {
			return permissionDenied(cfg.Socket, err)
		}
		if !docker.IsUnreachable(err) {
			return fmt.Errorf("cannot determine dev-lease's own project: %w", err)
		}
		fmt.Fprintf(cfg.ErrOut, "start: daemon unreachable: %v (retrying in %s)\n", err, cfg.Interval)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.Interval):
		}
	}
	own := ownLabels[docker.ProjectLabel]
	if own == "" {
		return fmt.Errorf("cannot determine dev-lease's own project: empty project name")
	}
	for _, p := range cfg.Projects {
		if ok, _ := path.Match(p, own); ok {
			return fmt.Errorf("refusing to start: pattern %q matches dev-lease's own project %q", p, own)
		}
	}
	// The container role: with network_mode: container:X the /etc binds in
	// the mountinfo come from X, so the detected own id can be another
	// container's. The dev-lease.role=server label on the inspected own
	// container breaks that spoof: examples/basic/compose.yml sets it on the
	// dev-lease service and a foreign container never carries it.
	if ownLabels["dev-lease.role"] != "server" {
		id := ownID
		if len(id) > 12 {
			id = id[:12]
		}
		return fmt.Errorf("refusing to start: the own container %s does not carry the label dev-lease.role=server; add 'dev-lease.role: server' to the dev-lease service's labels in compose.yml and recreate the container (serve never runs without it)", id)
	}

	// One serve per daemon: the state-file lock above only covers serves on
	// the same state file, and a second serve on another state volume
	// would run its own passes and stop or remove projects behind the
	// first one's back. Refuse while another running container carries
	// dev-lease.role=server. The comparison uses the full id from the own
	// inspect: --own-id may be a short prefix while the listing carries
	// full ids, so comparing --own-id itself would make serve refuse itself.
	servers, err := cli.RoleServers(ctx)
	if err != nil {
		return fmt.Errorf("listing the dev-lease server containers: %w", err)
	}
	for _, s := range servers {
		if s.ID == ownFullID {
			continue
		}
		id := s.ID
		if len(id) > 12 {
			id = id[:12]
		}
		name := s.Name
		if name == "" {
			name = id
		}
		return fmt.Errorf("refusing to start: another dev-lease server container %s (%s) is already running on this daemon; stop it first", name, id)
	}

	st := lease.Store{Path: cfg.State}
	if err := st.UpdateHeader(func(h *lease.Header) error {
		h.Patterns = cfg.Projects
		h.DefaultTTL = cfg.DefaultTTL
		h.RemoveAfter = cfg.RemoveAfter
		return nil
	}); err != nil {
		return fmt.Errorf("writing the state header: %w", err)
	}

	// The listing error is already printed; serve serves on regardless.
	_ = startListing(ctx, cli, st, cfg.Projects, own, cfg.ErrOut)

	passCfg := reaper.Config{
		Patterns:    cfg.Projects,
		Own:         own,
		DefaultTTL:  cfg.DefaultTTL,
		RemoveAfter: cfg.RemoveAfter,
		StopTimeout: cfg.StopTimeout,
		Log:         cfg.Out,
	}
	// The sweep and the control socket's stop and remove all run through
	// one runner, so the actions serialise against each pass. The socket
	// sits next to the state file, 0600, and is removed on exit. The
	// socket's actions run on serve's context: once serve shuts down, no
	// new action starts.
	runner := &reaper.Runner{Cli: cli, Store: st, Cfg: passCfg}
	controlClose, err := control.Listen(ctx, control.SocketPath(cfg.State), runner)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	defer controlClose()

	// The dashboard (off when --dashboard is empty) serves the read-only
	// view and the stop/remove actions over TCP. Its handlers run on
	// serve's context, and Shutdown closes the listener when serve exits;
	// a listen error stops serve with a message naming the address.
	if cfg.Dashboard != "" {
		handler, err := dashboard.New(dashboard.Options{
			Runner:      runner,
			Hosts:       cfg.DashboardHosts,
			Token:       cfg.DashboardToken,
			BaseContext: ctx,
		})
		if err != nil {
			return fmt.Errorf("dashboard: %w", err)
		}
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			BaseContext:       func(net.Listener) context.Context { return ctx },
		}
		ln, err := net.Listen("tcp", cfg.Dashboard)
		if err != nil {
			return fmt.Errorf("dashboard: listening on %s: %w", cfg.Dashboard, err)
		}
		fmt.Fprintf(cfg.ErrOut, "dashboard: listening on %s\n", ln.Addr())
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(cfg.ErrOut, "dashboard: %v\n", err)
			}
		}()
		defer func() {
			// Shut down with serve's context, bounded to 30 s: on the
			// normal exit ctx is already done and the listener closes at
			// once; on an error return an in-flight action cannot hold
			// serve's exit longer than the server's WriteTimeout.
			shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	started := time.Now()
	for {
		rep, err := runner.Pass(ctx)
		if errors.Is(err, lease.ErrCorrupt) {
			// A corrupt state file is unsafe to overwrite: stop instead of
			// looping on it.
			return fmt.Errorf("pass: %w", err)
		}
		if err != nil {
			if docker.IsPermission(err) {
				// A definitive denial, not a daemon that is down: retrying
				// will not help.
				return permissionDenied(cfg.Socket, err)
			}
			fmt.Fprintf(cfg.ErrOut, "pass failed: %v\n", err)
		} else {
			fmt.Fprintf(cfg.ErrOut, "pass: ok managed=%d removed=%d granted=%d failed=%d skipped=%d\n",
				len(rep.Managed), len(rep.Removed), len(rep.Granted), len(rep.Failed), len(rep.Skipped))
		}

		// The first pass runs at once, but no idle exit before one full
		// interval has passed: serve is started on demand and the caller's
		// extend may land right after the start.
		if err == nil && cfg.ExitWhenIdle && time.Since(started) >= cfg.Interval {
			leases, readErr := st.Read()
			if readErr == nil && managedIdle(ctx, cli, leases, passCfg, time.Now().UTC()) {
				fmt.Fprintln(cfg.ErrOut, "idle: no live leases, no running containers and nothing stopped, exiting")
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.Interval):
		}
	}
}

// managedIdle reports whether serve may exit as idle: no managed project
// has a live lease, none has a running non-kept container, and (with an
// effective removal delay > 0) none is stopped waiting for its removal
// time. A stopped project with an effective delay of 0 is never removed,
// so it is the operator's business and does not keep serve alive. A kept
// container stays running by design and does not keep serve alive either.
func managedIdle(ctx context.Context, cli *docker.Client, leases map[string]lease.Lease, cfg reaper.Config, now time.Time) bool {
	for name, l := range leases {
		if !reaper.Managed(name, cfg) {
			continue
		}
		if !l.Expired(now) {
			return false
		}
		if l.State == lease.StateStopped && l.EffectiveRemovalDelay(cfg.RemoveAfter) > 0 {
			return false
		}
	}
	conts, err := cli.Containers(ctx, "")
	if err != nil {
		return false // cannot tell: stay
	}
	for _, c := range conts {
		if !c.Running || docker.Kept(c) {
			continue
		}
		if reaper.Managed(c.Labels[docker.ProjectLabel], cfg) {
			return false
		}
	}
	return true
}

// detectOwnID returns the id of the container dev-lease runs in. It is a
// variable so tests can swap it.
var detectOwnID = detectOwnIDFromProc

// mountinfoPath is the mountinfo detectOwnIDFromProc reads; a variable so
// tests can point it at a committed fixture.
var mountinfoPath = "/proc/self/mountinfo"

// detectOwnIDFromProc reads the container's mountinfo and takes the id
// from the /containers/<id>/ bind mounts of hostname, hosts and
// resolv.conf. Nothing else is trusted: the overlay root line carries a
// random layer id, and a hostname is resolved by container NAME, so it
// could read another container. No id found means refuse to start.
func detectOwnIDFromProc() (string, error) {
	if v := os.Getenv("DEV_LEASE_OWN_ID"); v != "" {
		return v, nil
	}
	data, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return "", fmt.Errorf("cannot determine dev-lease's own container id: reading %s: %w; pass --own-id", mountinfoPath, err)
	}
	id, ok := ownIDFromMountinfo(string(data))
	if !ok {
		return "", fmt.Errorf("cannot determine dev-lease's own container id: %s has no /containers/<id>/ bind mount; pass --own-id", mountinfoPath)
	}
	return id, nil
}

// ownIDRe matches the bind source of the hostname/hosts/resolv.conf mounts.
var ownIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// etcMounts are the mount points whose bind sources carry the container id.
var etcMounts = map[string]bool{
	"/etc/hostname":    true,
	"/etc/hosts":       true,
	"/etc/resolv.conf": true,
}

// ownIDFromMountinfo returns the container id from one mountinfo file, or
// false when no line is a bind mount of /etc/hostname, /etc/hosts or
// /etc/resolv.conf whose source is /containers/<64-hex>/. Other lines are
// ignored (the overlay root line carries a random layer id).
func ownIDFromMountinfo(data string) (string, bool) {
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || !etcMounts[fields[4]] {
			continue
		}
		if m := ownIDRe.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// startListing prints the status listing: for every managed project its
// objects and lease, one "not managed" line for everything else. This is
// serve's start output and the output of `dev-lease status`. It returns the
// listing error after printing it (the first one, per-project included), so
// status can exit 1 on a broken daemon while serve just continues.
func startListing(ctx context.Context, cli *docker.Client, st lease.Store, patterns []string, own string, w io.Writer) error {
	projects, err := cli.Projects(ctx)
	if err != nil {
		fmt.Fprintf(w, "listing unavailable: %v\n", err)
		return err
	}
	leases, err := st.Read()
	if err != nil {
		fmt.Fprintf(w, "listing unavailable: %v\n", err)
		return err
	}
	// The header's remove-after is serve's current setting, which every
	// stopped lease's effective removal delay needs.
	fallback := headerRemoveAfter(st)
	names := func(objects []docker.Object) string {
		var out []string
		for _, o := range objects {
			name := o.Name
			if name == "" {
				name = o.ID
			}
			out = append(out, name)
		}
		return strings.Join(out, " ")
	}
	var listErr error
	// One listing of every image: a project's images are its labelled
	// ones plus the unlabelled ones the removal rule counts as its own
	// (Compose v2 builds images without labels).
	allImgs, allImgsErr := cli.Images(ctx, "")
	for _, p := range docker.SortedProjects(projects) {
		if !reaper.Managed(p, reaper.Config{Patterns: patterns, Own: own}) {
			fmt.Fprintf(w, "not managed: %s\n", p)
			continue
		}
		conts, err1 := cli.Containers(ctx, p)
		nets, err2 := cli.Networks(ctx, p)
		vols, err3 := cli.Volumes(ctx, p)
		err4 := allImgsErr
		var imgs []docker.Object
		if err4 == nil {
			// The stack's images by the removal's rule, under the names the
			// plan uses: what the removal deletes, what it keeps for a kept
			// service or container, and a project-labelled image that stays
			// for another reason under its own name.
			scope := reaper.NewImageScope(p, conts, leases[p].Services)
			for _, i := range allImgs {
				labelled := i.Labels[docker.ProjectLabel] == p
				if !labelled && i.Labels[docker.ProjectLabel] != "" {
					continue
				}
				step := scope.Step(i)
				if labelled && len(step.Remove) == 0 && len(step.Kept) == 0 {
					imgs = append(imgs, i)
					continue
				}
				for _, r := range step.Remove {
					imgs = append(imgs, docker.Object{ID: i.ID, Name: r.Name})
				}
				for _, k := range step.Kept {
					// An unlabelled image only by the stack's own tags: a
					// pulled image a kept container runs from is not the
					// stack's.
					if _, own := docker.TagService(p, scope.Services, k); labelled || own {
						imgs = append(imgs, docker.Object{ID: i.ID, Name: k})
					}
				}
			}
		}
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			fmt.Fprintf(w, "%s: listing unavailable: %v\n", p, firstErr(err1, err2, err3, err4))
			listErr = firstErr(err1, err2, err3, err4)
			continue
		}
		leasePart := "no lease (gets the default lease now)"
		if l, ok := leases[p]; ok {
			leasePart = leaseLine(l, time.Now().UTC(), fallback)
		}
		fmt.Fprintf(w, "%s: containers=[%s] networks=[%s] volumes=[%s] images=[%s] lease=%s\n",
			p, names(conts), names(nets), names(vols), names(imgs), leasePart)
	}
	return listErr
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
