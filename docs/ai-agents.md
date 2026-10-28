# Setting up AI coding agents

A guide for a machine that runs many git worktrees and AI agents at once. It
covers the rule every agent follows, the project names that make the rule
work, the hooks that carry it out, and a five-minute setup for Claude Code.

The copyable files are in [examples/agents/claude-code](../examples/agents/claude-code).

## The rule

One compose project per worktree. Extend the lease of the stack you work in.
When the work is done, remove that stack and nothing else. Never a broad
prune.

In commands:

```bash
# while you work
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease extend <project> 4h --holder <your-name>

# when the work on the worktree is finished
docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease down <project>
```

Where dev-lease serve does not run, the removal goes through compose alone:

```bash
docker compose -p <project> down -v
```

Both delete the volumes for real, so a removal cannot be undone. The removal
through serve is the one a sweep runs: a volume or a service labelled
`dev-lease.keep` survives it. The compose path has no such rule: it removes
every volume of the project.

Never these:

```bash
docker system prune     # every worktree's stopped containers and volumes
docker volume prune     # the same, for volumes
docker container prune  # the same, for containers
docker image prune -a   # every image another stack built
```

They delete other worktrees' data. `docker builder prune` (build cache) and
`docker image prune` without `-a` (dangling images only) are fine.

## Project names

One compose project per worktree, named `<repo>-<worktree>`:
`myapp-feat-login`, `myapp-fix-cache`. The main worktree may keep the bare
repository name (`myapp`).

Compose reads the project name from the first of these that is there:

1. `-p <name>` on the command line.
2. `COMPOSE_PROJECT_NAME` in the environment.
3. `COMPOSE_PROJECT_NAME` in the `.env` file next to the compose file.
4. The compose file's top-level `name:`.
5. The directory name, normalised (lower case, only `a-z0-9_-` kept).

### The trap: a top-level `name:` in the compose file

A compose file with a `name:` at the top pins the project name for every
worktree of that repository, because every worktree carries the same compose
file. The branch worktree then resolves to the main worktree's project name:
two worktrees on one project name share the containers, the volumes and the
network, so one worktree's data lands in the other's, and a `down` in the
branch takes the main worktree's data with it.

Give every worktree its own name instead, in the worktree's `.env` (next to
the compose file):

```bash
COMPOSE_PROJECT_NAME=myapp-feat-login
```

The same name in every command works too (`docker compose -p myapp-feat-login
up -d`), but it is easy to forget one; the `.env` is read by every compose
command in that worktree.

Check which name is in force:

```bash
dev-stack name          # the helper prints it
```

## The patterns serve must cover

dev-lease only manages the projects whose name matches the patterns in serve's
`.env`:

```bash
DEV_LEASE_PROJECTS=myapp,myapp-*
```

Every worktree's project name has to match, or the stack is not managed at
all: it is never stopped, never removed, and `dev-lease extend` on it exits 2
("not managed"). A pattern of `myapp` alone manages the main worktree and
nothing else; `myapp-*` covers the branch worktrees.

Two more rules for the patterns:

- They must not match dev-lease's own project name (`dev-lease`): serve
  refuses to start when they do.
- Keep them narrow. A pattern that catches a project of somebody else's work
  puts that stack under the sweeper too.

## The hooks

Four hooks carry the rule for Claude Code. They live in
[examples/agents/claude-code/hooks](../examples/agents/claude-code/hooks) and
are wired in `settings.json`.

| Hook | When | What it does |
| --- | --- | --- |
| `dev-lease-extend.sh` | SessionStart, UserPromptSubmit | Extends the worktree's stack lease by 1 hour, held by `claude-<session id>`. Silent, and it never fails the session. |
| `dev-lease-release.sh` | SessionEnd | Ends that one claim. It stops and removes nothing: dev-lease stops the stack when no holder is left. |
| `docker-prune-guard.sh` | PreToolUse (Bash) | Denies the broad prunes (exit 2, the reason on stderr) and allows everything else. |
| `worktree-stack-down.sh` | PreToolUse (Bash) | Removes the stack of a worktree that is being removed, before its directory disappears. It never denies the command. |

The lease a hook extends is one hour, so a long job needs its own extend with
a TTL that covers it (`dev-stack extend 4h`). A shorter extend never cuts a
lease, so err long.

## The main worktree is protected

The main worktree's stack holds the developer's data: its database volume is
usually the only copy. So `dev-stack down` refuses (exit 3, one line on
stderr, nothing run) in two cases:

- A linked worktree whose project name is still the main worktree's. The fix
  is the one from the naming section: give the branch its own
  `COMPOSE_PROJECT_NAME` in its `.env`.
- The main worktree itself, unless you pass `--main` on purpose.

A directory that is not a git worktree at all keeps the plain behaviour.

## Testcontainers: keep the reaper on

Testcontainers starts its own containers, outside compose, so dev-lease does
not see them: they carry no compose project label and their names do not
match your patterns. Ryuk is the reaper that removes them and their volumes
when a test run dies.

So keep Ryuk on. Never set `TESTCONTAINERS_RYUK_DISABLED=true`: that is how
orphaned test containers pile up on a machine with many agents. When a test
run leaves containers behind, look for the Ryuk container first.

## One-off containers

A container that does nothing but answer one question is a `docker run --rm`:

```bash
docker run --rm --network myapp_default postgres:16-alpine psql -h db -U app
```

`--rm` removes the container on exit. Anything that creates a volume or a
network belongs in a compose project instead: a stray `docker run -v ...`
is invisible to the lease system and stays on the machine until somebody
prunes it.

## Other agents

Every agent, not only Claude Code, gets the same three things:

1. **The rule.** Paste
   [examples/agents/AGENTS-snippet.md](../examples/agents/AGENTS-snippet.md)
   into the repository's `AGENTS.md`: when to extend, how to bring a stopped
   stack back, and how to remove it when the work is done.
2. **The helper.** `dev-stack` (name, extend, down, status) is plain shell;
   copy it to any agent's tools directory, or put it on PATH.
3. **MCP.** `dev-lease mcp` is an MCP server over stdio inside the serve
   container: an agent sees the stacks and stops or removes one without
   shelling out. Register it with

   ```bash
   claude mcp add dev-lease -- sh -c 'docker exec -i "$(docker ps -qf label=dev-lease.role=server)" dev-lease mcp'
   ```

   What each tool does is in
   [examples/agents/mcp.md](../examples/agents/mcp.md). `remove_stack` deletes
   data for real and needs the stack's name as the confirmation: ask the
   person before an agent removes a stack.

## Claude Code in five minutes

1. Copy the helper and the hooks (per user, or per repository):

   ```bash
   mkdir -p ~/.claude/bin ~/.claude/hooks
   cp examples/agents/claude-code/dev-stack ~/.claude/bin/dev-stack
   cp examples/agents/claude-code/hooks/*.sh ~/.claude/hooks/
   chmod +x ~/.claude/bin/dev-stack ~/.claude/hooks/*.sh
   ```

   Put `~/.claude/bin` on your PATH to run `dev-stack` yourself; the hooks
   find the helper next to themselves either way.

2. Merge [examples/agents/claude-code/settings.json](../examples/agents/claude-code/settings.json)
   into `~/.claude/settings.json`. The paths in it are for a per-repository
   install; for a per-user install they become `~/.claude/hooks/`.

3. Copy the skill, so the agent knows the rules:

   ```bash
   mkdir -p ~/.claude/skills/dev-lease
   cp examples/agents/claude-code/skills/dev-lease/SKILL.md ~/.claude/skills/dev-lease/SKILL.md
   ```

4. Check it:

   ```bash
   dev-stack name      # the project name of this worktree
   dev-stack status    # every lease, or the compose projects
   ```

   In a worktree of a repository whose compose file has a top-level `name:`,
   `dev-stack name` prints the main worktree's name: give the branch its own
   `COMPOSE_PROJECT_NAME` in its `.env` before you start anything.

The step-by-step version of this, with what each file does and how it differs
from the one-script setup, is in
[examples/agents/claude-code/README.md](../examples/agents/claude-code/README.md).
