# dev-lease: design decisions

This page records what dev-lease is meant to do, what it must never do, and
why it works the way it does. [reference.md](reference.md) has the exact
behaviour.

## Goal

Development machines collect Docker Compose stacks: one per git worktree,
plus the ones test runs and AI agents start and forget. dev-lease gives each
managed stack a lease. Whoever uses a stack renews the lease; a stack nobody
renews is stopped, and removed if nobody comes back to it.

## Non-goals

- Not a scheduler, a process supervisor or a backup tool.
- No remote Docker hosts, Kubernetes or Swarm.
- Not for production machines.

## Never

- Touch anything outside the managed projects, or dev-lease's own project.
- Start, build, pull or execute anything through the Docker API.
- Remove a stack while someone is renewing its lease.
- Delete a stopped stack sooner than the delay recorded when it stopped, or while
  `--remove-after 0` is set, on its own. A removal the owner asks for in the
  dashboard, behind the stack's typed name, is the owner's decision; MCP
  never removes a stack marked never remove.

## Decisions

**Ownership is the Compose label, scoped by name patterns.** A project is
what carries `com.docker.compose.project=<name>`, and only names matching
`--projects` are managed. That covers every stack Compose creates, with no
change to the app's compose file, and nothing else on the machine. serve
reads its own project from its own container and refuses to start when a
pattern would match it, or when its container lacks the
`dev-lease.role=server` label, which also stops a shared network namespace
from making it mistake another container for itself.

**Stop first, remove much later.** An idle stack costs RAM and CPU at once,
and disk slowly. Stopping at expiry frees the first without losing anything,
and `docker compose up` brings the stack back with its data. Removal,
which deletes volumes, waits `--remove-after` (24 hours by default, or never).
A stopped stack's delay is recorded at the stop and never cut below that by
later settings; a longer serve setting lengthens a delay that came from
serve's setting (a label's delay stays as it is), and `0` pauses every
sweep removal. A serve restarted with a shorter value, for example by
a script with a different environment, cannot remove a stack sooner than its
recorded delay.

**One sweep removes in one pass, and only the lease is kept between passes.**
The lease holds its holders and a short lock (`stopping`, `removing`) or
`stopped` with its time and the removal delay in force at the stop. What to
stop or remove is listed from the daemon at that moment. Earlier designs
that remembered object ids across passes kept producing cases where a later
pass touched a stack the developer had just re-created; with no object ids
remembered, an object that could not be removed simply makes the stack
present again, and it gets a new lease.

**Re-check under the lock, refuse during the action.** The stop and the
removal re-check the lease under the store lock before they start, and
`extend` is refused (exit 3) while either runs. A renewal that lands before
the re-check wins; none can land in the middle.

**Images are narrower than other objects.** An image built `FROM` a project
image inherits its label, so the label alone is not enough. An image is the
project's when a container of the project ran from it, or when it is tagged
`<project>-<service>`, the name Compose gives a built image (compared on the
whole repository, so a registry image of the same name is not matched).
Compose v2 builds images without the project label, so an unlabelled image
counts when its name is exactly `<project>-<service>` for one of the
project's own services, which every sweep records in the lease. Whatever its
labels, an image goes by id when it has one tag. With several tags
(identical builds share one image id across services and projects, and you
may have tagged it yourself) Docker refuses a delete by id, so the stack's
own tags are deleted one by one, each first checked to still name the listed
image, and the image stays for whatever tags remain. The tags of a kept
service stay. Pulled images never match and are never touched. Deletes run
without `force`, so Docker itself protects an image another container uses.

**Policy lives in the app's compose file.** `dev-lease.keep` on a service or
a volume, and `dev-lease.remove-after` on a service, let a stack say what
must survive. Unclear removal delays resolve to keeping data: `0` wins between
conflicting delays, and an unreadable delay or one under an hour means never
remove. `keep` must
be exactly `true`; any other value is ignored (and logged when it is on a
container). A kept service
never keeps the rest of its stack alive.

**Wall-clock time.** Leases expire by UTC wall-clock time, also while the
machine sleeps or Docker is down: agents work at night too. After a long
sleep the first sweep stops an expired stack; it never stops and removes in
the same sweep.

**Always on by default.** serve sleeps between sweeps and costs nothing
noticeable, and running all the time with `restart: unless-stopped` means it
comes back after a reboot and `docker exec` always finds it. Laptops can use
the on-demand example, which exits when nothing is left to watch.

**docker exec first, a listener only when asked.** The client commands are
the same binary run through `docker exec`, so access control is Docker's
own. The state is one JSON file on a volume, replaced atomically under a
file lock, with one serve per state file; serve also refuses to start while
another dev-lease server runs on the same Docker. A new state value needs a
new file version, and serve refuses versions it does not know.

The dashboard is the one exception, and it is off unless `--dashboard` is
given. Where it can be reached is decided by the Compose port mapping, not
by dev-lease: `127.0.0.1` in the examples, a VPN or docker0 address when the
owner chooses. On top of that it answers only allowed host names (against
DNS rebinding), and every action is a POST carrying a token from the page
(tied to the login session when a token is set) with a same-origin
`Origin`, so a page anywhere else cannot act through the owner's browser.
The page loads nothing from outside (`default-src 'self'`).

**Only serve stops and removes.** The engine assumes one process does that.
Dashboard actions run inside serve; `dev-lease mcp` reads on its own but
sends stop and remove to serve over a Unix socket next to the state file
(mode 0600). Every sweep and action holds one mutex, so they wait for each
other and nothing runs twice. MCP never removes a stack marked never remove;
the dashboard can, behind the typed name and a warning.

**A dialog promises what the engine does.** The stop and remove dialogs and
MCP's details are built from the same listing rules the engine uses to stop
and delete (`reaper.PlanFor`), and a test runs real removals on the fake
daemon to check the deleted set equals the plan. An object another project
still uses is listed, and Docker refuses to delete it.

**The removal delay survives `docker compose down`.** A stack's
`dev-lease.remove-after` label lives on its containers, which `down`
deletes. Every sweep therefore records the label's delay in the lease while
the containers exist, and the stop, the removal and the views use the
record when no container carries the label. A failed or interrupted stop
puts the lease back as it was, so the record is never lost to a retry.

**Standard library and an allowlisted API.** One static Go binary with no
dependencies. The Docker client can only make the calls it needs (the version
check, the listings, an inspect of its own container, an inspect of an image
by tag to check the tag right before deleting it, stop and the four deletes,
and for the dashboard three reads: a container's last log lines, its memory,
and the volume sizes); anything else is refused in code, down to the query
parameters. The process starts as root only to switch to the socket's
group and then to uid 65532.

**Mounting the socket is root on the host.** The README says so before
anything else in its safety section. dev-lease limits itself; it cannot
limit a compromised image, so releases carry a build provenance
attestation.

## Testing

Unit tests run against a fake Docker daemon on a Unix socket that keeps
containers, networks, volumes and images in memory and records every call,
with an injected clock. They cover the lease arithmetic, every state and
its transitions, the re-checks and races, the image rule, the labels and
each failure path; the important rules were also checked with mutation
testing during development. The
integration test runs the whole lifecycle against a real daemon: a stack is
granted a lease, stopped at expiry with its data intact, and removed after
the delay, while a kept service, a kept volume and an unmanaged stack
survive; a second one drives the dashboard and the control socket against
a real stack. Browser tests (Playwright, Chromium) open both pages at
desktop and phone size in both themes, check for horizontal scroll and
accessibility problems, and run the stop and remove flows on demo stacks.
CI also runs `go vet`, staticcheck, the race detector and checks on every
example.

## Platforms

CI runs on Linux. macOS and Windows with Docker Desktop are checked by hand
before a release ([release-checklist.md](release-checklist.md)). Docker
Desktop's Enhanced Container Isolation blocks socket mounts and is not
supported. The file locks use `flock`, so a native Windows client must not
share a state file with a running serve; use `docker exec` there.
