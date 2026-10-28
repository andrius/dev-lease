---
name: dev-lease
description: Use when starting, testing in, or finishing work on a worktree that runs a Docker Compose stack, when a stack stopped by itself or must come back, when Docker on the machine looks full or needs cleaning, and before removing a worktree or a compose stack. Covers the per-worktree stack naming rule, the dev-stack helper, the lease that keeps a stack alive, and the prunes that are banned because they eat other worktrees' data.
---

# dev-lease: a compose stack holds a lease

Every worktree that needs containers runs its own compose stack, and every
stack holds a lease. dev-lease watches the stacks: when a lease runs out, the
stack is stopped and its RAM and CPU are freed while its data stays; when it
stays stopped past its removal delay, it is removed with its volumes and
built images. The lease is 1 hour by default and the removal comes 24 hours
after the stop. The helper `dev-stack` (next to this skill, or on PATH)
wraps the lease calls and the compose commands you need.

The rule in one line: extend the lease of the stack you work in, remove that
stack and only that stack when you are done, and never a broad prune.

## Stack names

One compose project per worktree, named `<repo>-<worktree>`
(`myapp-feat-login`). The main worktree may keep the bare repo name
(`myapp`). The name comes from `COMPOSE_PROJECT_NAME`, the worktree's
`.env`, the compose file's `name:`, or the normalised directory name, in that
order - `dev-stack name` prints which one holds. Never start a stack under
another worktree's name: two worktrees on one project name overwrite each
other's containers and volumes.

## The dev-stack helper

- `dev-stack name [dir]` - the compose project name of a worktree.
- `dev-stack extend [ttl] [dir]` - extend its lease; default 1h, never fails.
- `dev-stack down [dir]` - remove that stack now: through dev-lease serve
  when it runs, otherwise `docker compose -p <name> down -v
  --remove-orphans --rmi local`. The lease is released with it.
- `dev-stack status` - every lease, or plain `docker compose ls -a` where
  the dev-lease server does not run.

## While you work

Extend the stack's lease when you start using it, and again whenever a long
job would outlast the remaining time: pass a TTL that covers the whole job
(`dev-stack extend 4h`). A shorter extend never cuts a lease, so err long.
A hook can extend the worktree's lease on session start and on every prompt,
and release the claim when the session ends - your own extends still matter
for jobs that outlive the session.

## A stack that stopped by itself

A stopped stack is not lost; its data is still on disk. Bring it back in its
own worktree directory:

```bash
docker compose up -d
dev-stack extend
```

If the stack was removed (the removal deletes volumes), start over from the
compose file as on a fresh machine.

## Done with a stack

When the work in a worktree is finished, remove THAT stack and nothing else:

```bash
dev-stack down
```

Removing the git worktree itself can run `dev-stack down` on it first (a
hook does this); do not rely on that cleanup for a stack you were told to
remove. `docker compose down` without `-v` leaves the volumes behind -
`dev-stack down` is the form to use.

## What never to do

- `docker system prune`, `docker volume prune`, `docker container prune`:
  they delete OTHER worktrees' stopped containers, volumes and data. Banned;
  a PreToolUse hook denies them.
- `docker image prune -a`: same, plus every image another stack built. Banned.
- Removing, stopping or extending a stack that belongs to another worktree.
  `dev-stack status` shows whose lease is live; leave theirs alone.
- `docker compose down` in the wrong directory: check `dev-stack name` first.

Cleaning the machine is never your call: a full disk or a crowded `docker ps`
is the person's problem, and dev-lease already reaps what nobody extends.

## One-off containers and test containers

- A one-off container (a quick `psql`, a `curl` inside a network) is
  `docker run --rm <image>` - it removes itself on exit.
- Anything that creates a volume or a network belongs in a compose project,
  so dev-lease can see it and reap it; a stray `docker run` with `-v` is
  invisible to the lease system and stays until somebody prunes.
- Testcontainers and Ryuk: keep the reaper ON (never
  `TESTCONTAINERS_RYUK_DISABLED=true`). Ryuk is what removes test containers
  and their volumes when a test run dies; disabling it is how orphans pile up.

## Machines without the dev-lease server

On a machine where serve does not run, the rule still holds: `dev-stack down`
works through compose alone, `dev-stack status` falls back to `docker compose
ls -a`, and the prune ban is unchanged - other people's stacks live on those
machines too.

## dev-lease directly

Where serve runs, the same operations without the helper, inside the serve
container:

```bash
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease list
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease status
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease extend <project> <ttl>
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease release <project> --holder <name>
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease down <project>
```

To keep a service or a volume out of the sweeper's hands, see the labels
(`dev-lease.keep`, `dev-lease.remove-after`) in the project's README:
`dev-lease.keep: "true"` on a volume is how data survives whatever happens to
the stack.
