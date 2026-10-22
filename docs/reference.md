# Reference

The README explains what dev-lease is for and how to start it. This page is
the exact behaviour: the commands, the state file, one sweep step by step,
the policy labels, the log, the dashboard, the MCP server and the control
socket.

## Commands

One binary, two roles. `serve` is the watcher; the other commands are the
client side, run inside the serve container with `docker exec`:

```bash
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease <command>
```

### serve

```
dev-lease serve --projects <patterns> [flags]
```

| Flag (environment variable) | Default | Meaning |
| --- | --- | --- |
| `--projects` (`DEV_LEASE_PROJECTS`) | required | Comma-separated glob patterns over Compose project names. A project is managed when its name matches a pattern and it is not serve's own project. |
| `--default-ttl` | `2h` | The lease a managed project gets when serve first sees it without one. |
| `--remove-after` (`DEV_LEASE_REMOVE_AFTER`) | `168h` | How long a stopped project is kept before it is removed, counted from the stop. `0` pauses every sweep removal while it is set. Otherwise at least `1h`. How a later value acts on a delay recorded at a stop: see the state table. |
| `--interval` | `1m` | Time between sweeps; at least `1s`. |
| `--stop-timeout` | `10s` | Stop timeout per container; at least `1s`. |
| `--exit-when-idle` | `false` | Exit 0 when no managed project has a live lease, none has a running container that is not kept, and none is stopped waiting for a removal time (a stack whose delay is 0 does not count). Never before one full interval after start. |
| `--state` (`DEV_LEASE_STATE`) | `/data/leases.json` | The state file. |
| `--socket` | `/var/run/docker.sock` | The Docker socket. |
| `--own-id` (`DEV_LEASE_OWN_ID`) | detected | serve's own container id. Normally read from the `/containers/<id>/` bind mounts in `/proc/self/mountinfo` and checked against the inspected container. |
| `--dashboard` (`DEV_LEASE_DASHBOARD`) | empty (off) | The dashboard's listen address inside the container, for example `:8410`. A listen error stops serve with a message naming the address. |
| `--dashboard-hosts` (`DEV_LEASE_DASHBOARD_HOSTS`) | `localhost,127.0.0.1,[::1]` | Host names the dashboard answers for: `host`, `host:port`, `[ipv6]` or `[ipv6]:port`, compared without the port and in lower case. Anything else (`*`, a scheme, a path) is refused at start. |
| `DEV_LEASE_DASHBOARD_TOKEN` | none | Environment only, never a flag, never logged: when set, the dashboard asks for it once per browser and serve start. |

Durations are Go durations (`90m`, `2h`) or whole days (`7d`, at most
`3650d`); `1.5d`, `d`, `+2d` and `-1d` are refused.

serve refuses to start, with exit code 1, when:

- its own project matches a managed pattern;
- it cannot determine its own container or project;
- its container does not carry the label `dev-lease.role=server` (the
  message says what to add; with `network_mode: container:X` the detected
  id is X's, so this refuses it unless X is itself a dev-lease server);
- another serve holds the lock on the same state file;
- another running container carries `dev-lease.role=server` (a second
  dev-lease server on the same Docker);
- the state file cannot be parsed or has a version other than 1;
- a flag value is out of range, or a positional argument is left over.

At start serve prints the same listing as `status`. If the Docker daemon
does not answer, serve retries every interval; a permission error on the
socket stops it with a message naming the socket, its owner and mode, and
the identity serve runs as.

### Client commands

| Command | What it does |
| --- | --- |
| `extend <project> <ttl> [--holder <name>]` | Renews that holder's claim to `max(current expiry, now + ttl)`, so it never shortens a lease, and prints the holder's new expiry. `ttl` from `1m` to `168h` (`7d`). On a `stopped` project the lease is live again, and the command prints that the stack must be started with `docker compose up -d`: dev-lease never starts containers. |
| `release <project> --holder <name>` | Ends that holder's claim. |
| `release <project> --all` | Ends every claim; the next sweep stops the project. Releasing a project without a lease changes nothing. |
| `list` | One line per lease (formats below). |
| `status` | For every managed project on the daemon: its containers, networks, volumes, images and lease; one `not managed: <name>` line for every other project. |
| `mcp [--state <file>]` | The MCP server for agents, on stdin and stdout (see below). Run with `docker exec -i`. |
| `-version` | The build version. |

The default holder is `$DEV_LEASE_HOLDER`, else `$USER`, else `local`.
Inside the container neither variable is set, so pass `--holder` or
`docker exec -e DEV_LEASE_HOLDER=<name>`.

`list` prints, per project, one of:

```
myapp-a: live, 1h28m left (until 2026-10-21T10:31:00Z; holders: agent, alice)
myapp-b: stopped 3h ago; removed in 6d20h (at 2026-10-28T06:02:00Z)
myapp-c: stopped 2d ago; never removed
myapp-d: stopped 7d1h ago; removal overdue (at 2026-10-21T08:00:00Z)
myapp-e: stopping
myapp-f: removing
myapp-g: expired (last holder ci)
```

Only holders whose claim is still live are listed. Times are UTC; time
left is rounded down: to minutes below two days (`<1m`, `23m`, `1h23m`), to
hours from two days (`6d22h`).

Exit codes of the client commands: 0 ok; 1 a usage error, a bad TTL, an
unreadable state file, or (for `status`) a failed Docker listing; 2 not a
managed project; 3 the project is being stopped or removed right now.

The client commands share the state file with serve. Outside the container,
point `DEV_LEASE_STATE` at the same file.

## The state file

One JSON file, replaced atomically (a temporary file, then rename) under an
exclusive `flock`. serve and every client command take the same lock, and
serve holds a second lock, `<state file>.serve.lock`, for its whole life:
one serve per state file. Builds without `flock` (native Windows) enforce
neither lock.

```json
{
  "version": 1,
  "header": {
    "patterns": ["myapp", "myapp-*"],
    "default_ttl": 7200000000000,
    "remove_after": 604800000000000
  },
  "leases": {
    "myapp-a": {
      "project": "myapp-a",
      "label_remove_after": 0,
      "services": ["db", "web"],
      "holders": [
        {"name": "agent", "extended_at": "2026-10-21T08:31:00Z", "expires": "2026-10-21T10:31:00Z", "ttl": 7200000000000},
        {"name": "alice", "extended_at": "2026-10-21T08:20:00Z", "expires": "2026-10-21T10:20:00Z", "ttl": 7200000000000}
      ]
    },
    "myapp-b": {
      "project": "myapp-b",
      "state": "stopped",
      "stopped_at": "2026-10-21T06:02:00Z",
      "remove_after": 604800000000000,
      "remove_after_from": "serve",
      "holders": [
        {"name": "ci", "extended_at": "2026-10-21T04:00:00Z", "expires": "2026-10-21T06:00:00Z", "ttl": 7200000000000}
      ]
    }
  }
}
```

Durations are nanoseconds. A lease expires at its latest holder's expiry;
no expiry is stored per lease. Every live holder is kept, plus the last 20
expired ones as history. A stopped lease records the delay in force when it
stopped (`remove_after`) and where it came from (`remove_after_from`:
`label` or `serve`). `label_remove_after` is the delay the project's
`dev-lease.remove-after` labels gave the last time a sweep saw its
containers (absent when they carried none); it outlives the containers, so
it still applies after `docker compose down`. `services` lists the
project's `com.docker.compose.service` labels from the same sweep, so its
unlabelled built images are still recognised once the containers are gone.
The header carries serve's patterns and settings, so the client commands
know them.

| `state` | Meaning | What a sweep does | `extend` |
| --- | --- | --- | --- |
| absent | A lease, live or expired. | Nothing while a holder is live; at expiry, stop. | Extends the claim. |
| `stopping` | A stop is under way. | Found at the start of a sweep, it was left by a failed container listing or a serve that died: the lease goes back to what it was (`stopped` when it has a `stopped_at`, otherwise an expired lease that the same sweep stops again), with every field kept. | Refused, exit 3. |
| `stopped` | Stopped at `stopped_at`. Its delay now: `0` (never) when the recorded delay or serve's current setting is 0; else a label's recorded value; else the longer of the recorded and the current setting. | Revive when one of its containers runs again; remove once the delay has passed; drop the lease when nothing of the project is left on the daemon (an unlabelled built image found through `services` counts). | A live lease again. |
| `removing` | A removal is under way. | Found at the start of a sweep: the lease is deleted, and a project still present gets the default lease, which keeps `label_remove_after`. | Refused, exit 3. |
| anything else | Written by another version. | Deleted only when the lease has expired; a live lease with an unknown state is left alone and logged. | Extends the claim. |

So a later, shorter `--remove-after` never cuts a recorded delay; a longer
one lengthens a serve-set delay while it is set, never a label's; and when a
`0` is lifted, the delays apply again under the new setting, counted from
each stop, so stacks already past theirs go at the next sweep. A stack
without a label that stopped while serve ran with `0` records `0` and stays
until it is started or extended.

A new state value needs a new `version`: serve refuses any version but 1,
so a state file from a newer format is never misread.

## One sweep

1. **Clear stale locks.** For managed projects only, as in the state table:
   a `stopping` lease goes back to what it was, a `removing` lease is
   deleted (unknown states as in the table).
2. **Grant and revive.** A managed project present on the daemon without a
   lease gets the default lease. A `stopped` project with a running
   container that is not kept (`docker compose up` or `start`) loses its
   stopped lease and gets the default lease, which keeps
   `label_remove_after`. Present means: a container of the project,
   running or stopped, a network, a volume, or an image that matches the
   tag rule; kept objects and what kept containers use do not count.
3. **Record the label delays.** For every managed project whose containers
   the sweep lists, the delay its `dev-lease.remove-after` labels give is
   written to `label_remove_after` (removed when the containers carry no such
   label; kept when there are no containers or the listing failed). On a
   `stopped` lease, also one whose removal is due in this sweep, the same
   delay is also merged into the recorded delay: a label `0` wins,
   otherwise the longer effective delay stays (a delay from serve counts as
   the larger of its recorded value and serve's current setting). The state
   file is written only when a value changed.
4. **Stop.** A lease that expired: under the store lock serve re-checks the
   expiry and sets `stopping`, stops every running container of the
   project that is not kept, and deletes nothing. It then sets `stopped`
   with `stopped_at` and the delay in force (the stack's label; with no
   container left, `label_remove_after`; else serve's setting), whatever the
   stop outcome (a failed container listing leaves it `stopping`, and the
   next sweep puts it back and stops it again). A container whose stop failed still runs, so the next sweep
   revives the project, and the stop is tried again when that lease
   expires.
5. **Remove.** A `stopped` project whose delay (as in the state table) is
   above 0 and has passed since `stopped_at`. Under the store lock serve re-checks that the lease is still
   `stopped` and that its delay is above 0 and has passed, and sets
   `removing`. It then lists
   the project's objects again. If any container that is not kept runs, it
   sets the lease back to `stopped` (with the same `stopped_at`) and removes nothing (the next sweep
   revives it). Otherwise, in order:
   1. containers that are not kept: removed with their anonymous volumes
      (they are all stopped by then; Docker refuses one that someone started
      meanwhile, and it stays, with its volumes);
   2. networks, except those a kept container uses;
   3. volumes, except kept ones and those a kept container uses;
   4. images: of the project-labelled images, those a container of the
      project ran from and those with a tag `<project>-<service>:<tag>`
      (the whole repository, registry and path included, equal to the
      project name, a dash and the image's own
      `com.docker.compose.service` label); of the images without a
      `com.docker.compose.project` label (Compose v2 builds them that
      way), those with a tag whose whole repository is
      `<project>-<service>` for one of the project's services (from its
      containers, or from `services` in the lease when none is left). An
      image labelled for another project never matches, and pulled images
      such as `postgres:16-alpine` never match either. Images are deleted
      without `force`. A project-labelled image is deleted by id: Docker
      refuses an image another container uses, and an image a kept
      container runs from stays. An unlabelled image is deleted tag by tag,
      one `DELETE /images/<project>-<service>:<tag>` per matching tag:
      identical builds share one image id, across services and projects,
      and Docker refuses to delete such an image by id, while deleting a
      tag only untags it as long as other tags remain and deletes the image
      with its last tag (refused while a container uses it). The tags of a
      kept service stay, and so does the whole image when a kept container
      runs from it and its last tag would go. An unlabelled image alone never makes a project
      without a lease present.

   Whatever the outcome, the lease is deleted at the end. If objects are
   left, the project is present at the next sweep and is treated as a new
   one: the default lease, a stop at its expiry, a removal after its delay.

A failure of one object (a 5xx answer, a timeout, a lost connection) is
reported and the sweep goes on with the next object. A 4xx refusal other
than 404 is reported as skipped. A 404 means already gone. A failed listing
ends that project's removal for this sweep, as does a cancellation (serve
stopping); both are reported as failed and the lease is deleted. A
cancellation between projects leaves the later projects untouched.

An `extend` that lands while serve reads the state wins: the re-checks
under the lock in steps 4 and 5 see the new claim and leave the project
alone, and from the moment `stopping` or `removing` is set, `extend` is
refused.

## Policy labels

Set in the project's own compose file (an image can carry them too, see below):

| Label | On | Effect |
| --- | --- | --- |
| `dev-lease.keep: "true"` | a service (container) | Never stopped, never removed, never counted as presence. The networks, volumes and images it uses are left in place. |
| `dev-lease.keep: "true"` | a named volume | Never removed, never counted as presence. |
| `dev-lease.remove-after: "<duration>"` | any service | The project's removal delay instead of `--remove-after`: a Go duration of at least `1h`, whole days up to `3650d`, or `0` for never. When containers disagree, `0` wins, else the longest value. serve's `--remove-after 0` still overrides it. |

A `keep` value other than `true` is ignored; on a container it is also logged. A `remove-after`
value that cannot be read, or one under an hour, counts as `0` (never
remove) and is logged when the project is stopped. The delay is read from
the project's containers at every sweep and recorded in its lease
(`label_remove_after`), so it still applies after `docker compose down`,
and `list` and the dashboard can show the removal time. A stack taken down
before any sweep saw its containers has no record and gets serve's
setting. Docker copies an image's labels to its containers, so a base image
can set these labels too, and it cannot change a volume's labels after the
volume is created. `dev-lease.keep` on a network has no effect.

## The log

serve writes one JSON line per action to stdout:

```json
{"time":"2026-10-21T08:53:05Z","project":"myapp-b","action":"stop","id":"3f2a...","name":"myapp-b-web-1","result":"ok"}
```

`action` is one of `grant`, `revive`, `clear-state`, `unknown-state`,
`label`, `stop`, `remove`, `remove-network`, `remove-volume`,
`remove-image`. `result` is `ok`, `error: <reason>` or
`skipped: <reason>`. A line caused by the dashboard or by MCP carries
`"via": "dashboard"` or `"via": "mcp"`.

## Running serve on demand

With `--exit-when-idle=true` and `restart: "no"` (examples/on-demand),
serve exits when nothing is left to watch and callers start it before they
extend:

```bash
docker compose -p dev-lease -f compose.yml up -d --no-recreate --wait
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease extend <project> 2h --holder <name>
```

`up -d --no-recreate --wait` does nothing while serve runs and never
replaces it, so a serve that has been up for at least one interval can exit
between the two commands. When the
`extend` fails that way, run both again once. The example scripts start
serve themselves when it is not running and `DEV_LEASE_COMPOSE` is set.

## The dashboard

Off unless `--dashboard` is set. Only managed projects appear, and only
they can be acted on.

| Route | What it does |
| --- | --- |
| `GET /` | All stacks, soonest next event first. |
| `GET /stacks/<name>` | One stack; `?logs=<service>&lines=<1-200>` adds the last log lines of that service (times in UTC); `?confirm=stop` or `?confirm=remove` renders the dialog as a page for browsers without JavaScript. |
| `GET /fragments/stacks`, `GET /fragments/stacks/<name>` | The read-only parts of the same pages, for the 15 s refresh. |
| `POST /stacks/<name>/keep` | `ttl` one of `2h`, `8h`, `1d`, `7d`: the holder `dashboard` keeps the stack for at least that long, like `extend`. On a stopped stack the lease is live again; start the stack yourself. |
| `POST /stacks/<name>/release` | Ends the `dashboard` holder's claim. |
| `POST /stacks/<name>/stop` | Stops the stack now and ends every claim. |
| `POST /stacks/<name>/remove` | `confirm` must equal the name; for a stack marked never remove also `override=1`. Stops the stack if needed, then removes it now, whatever its delay. |
| `GET /login`, `POST /login` | The token page, when `DEV_LEASE_DASHBOARD_TOKEN` is set. |

Every request is checked in this order: the `Host` header must be in
`--dashboard-hosts` (else 403); with a token set, the session cookie must be
valid (else a redirect to `/login`); every POST must carry the page's token
(tied to the session when a token is set) and an `Origin` (or, without one,
a `Referer`) whose host and port equal the request's `Host` (else 403).
Every answer carries `Content-Security-Policy: default-src 'self'; img-src
'self' data:; frame-ancestors 'none'`, `X-Frame-Options: DENY`,
`Referrer-Policy: same-origin`, `X-Content-Type-Options: nosniff`, and
pages `Cache-Control: no-store`. The session cookie is `HttpOnly`,
`SameSite=Strict` and ends when serve restarts. Browsers send cookies to
every port of a host name, so any other web server you open under the same
name receives it.

Actions answer 303 back to the page with a fixed message (also when a stop
left some containers running: the message says so); a stack that is gone
404; a typed name that does not match 400; one being stopped or removed, a
never-remove removal without `override=1`, or a removal that did not
complete 409; a serve that is shutting down 503; a page older than serve's
last start 403 with a note to reload it. Actions run on serve's own context: closing the tab does not abort
them.

## The MCP server

`dev-lease mcp` speaks MCP (protocol version `2025-06-18`) as
newline-delimited JSON-RPC 2.0 on stdin and stdout, and logs nothing to
stdout. It drops privileges like serve, reads the patterns from the state
file's header (it refuses to start without them), lists stacks itself, and
sends stop and remove to serve over the control socket. Requests are
handled concurrently; a line over 4 MiB is answered with `-32600` and the
session goes on.

| Tool | Input | Notes |
| --- | --- | --- |
| `list_stacks` | none | Read-only. `structuredContent` is `{"stacks": [...]}`. |
| `stack_details` | `name`, optional `log_service`, `log_lines` (0-200, 0 = no logs) | Read-only. |
| `stop_stack` | `name` | Not destructive, idempotent. |
| `remove_stack` | `name`, `confirm` (equal to `name`) | Destructive. A stack marked never remove is refused. |

Failures are tool results with `isError` and one sentence: the stack is not
managed or not there; it is being stopped or removed (try again); it is
marked never remove; serve is not running, or shutting down; the action did
not answer within the limit (30 s for reads, 10 minutes for stop and
remove; serve may still finish it). Every successful result also carries
`structuredContent` that matches the tool's `outputSchema`; a failure
carries only its sentence.

## The control socket

serve listens on a Unix socket next to the state file, `<state file>.sock`
(`/data/leases.json.sock` in the examples), mode 0600, owned by serve's
user. One JSON request and one JSON answer per connection:

```json
{"action": "remove", "project": "myapp-b", "confirm": "myapp-b"}
{"ok": false, "code": "never_remove", "error": "..."}
```

`action` is `stop` or `remove`; a remove needs `confirm` equal to
`project`, and a stack marked never remove is always refused here. Answer
codes: `bad_request`, `not_managed`, `no_stack`, `busy`, `never_remove`,
`not_removed`, `shutting_down`. A socket left by a serve that died is
replaced when serve starts.
