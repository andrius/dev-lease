# Claude Code: the helper, the hooks and the skill

A copyable set for a machine that runs many git worktrees and AI agents at
once. The setup guide that explains the rules behind these files is
[docs/ai-agents.md](../../../docs/ai-agents.md).

Everything here is plain shell and JSON: no dependencies beyond bash, git,
docker and node (the hooks read the hook payload with node; without node they
stay quiet).

## Install in three steps

1. Copy the helper and the hooks next to your Claude Code configuration:

   ```bash
   mkdir -p ~/.claude/bin ~/.claude/hooks
   cp dev-stack ~/.claude/bin/dev-stack
   cp hooks/*.sh ~/.claude/hooks/
   chmod +x ~/.claude/bin/dev-stack ~/.claude/hooks/*.sh
   ```

   For one repository only, use `<repo>/.claude/bin/` and
   `<repo>/.claude/hooks/` instead. The hooks find the helper next to
   themselves (`../bin/dev-stack`, else `../dev-stack`), so nothing has to be
   on PATH; `DEV_STACK_BIN` overrides the lookup. To run `dev-stack`
   yourself, put that bin directory on your PATH.

2. Merge `settings.json` into `~/.claude/settings.json` (or
   `<repo>/.claude/settings.json`). The paths in it are for a per-repository
   install (`$CLAUDE_PROJECT_DIR/.claude/hooks/`); for a per-user install
   change them to `~/.claude/hooks/`. The settings:

   | Hook | When it runs | What it does |
   | --- | --- | --- |
   | `dev-lease-extend.sh` | SessionStart, UserPromptSubmit | Extends the worktree's stack lease by 1 hour, held by `claude-<session id>`. |
   | `dev-lease-release.sh` | SessionEnd | Ends that claim. It stops and removes nothing. |
   | `docker-prune-guard.sh` | PreToolUse (Bash) | Denies `docker system\|volume\|container prune` and `docker image prune -a` (exit 2, with the reason on stderr). |
   | `worktree-stack-down.sh` | PreToolUse (Bash) | Removes the stack of a worktree that is being removed, before the directory goes. It never denies the command. |

3. Copy the skill, so the agent knows the rules without the hooks:

   ```bash
   mkdir -p ~/.claude/skills/dev-lease
   cp skills/dev-lease/SKILL.md ~/.claude/skills/dev-lease/SKILL.md
   ```

## What each file does

- `dev-stack` - the helper around one worktree's compose stack:
  `dev-stack name` (the project name), `dev-stack extend [ttl]` (extend the
  lease; never fails), `dev-stack down` (remove that stack now, and only
  that stack), `dev-stack status` (every lease). `down` refuses to remove the
  main worktree's stack unless you pass `--main`.
- `hooks/dev-lease-extend.sh` - extends the lease on session start and on
  every prompt.
- `hooks/dev-lease-release.sh` - ends the session's claim at session end.
- `hooks/docker-prune-guard.sh` - denies the broad prunes that delete other
  worktrees' data. `bash hooks/docker-prune-guard.sh --self-test` runs its
  own deny/allow matrix.
- `hooks/worktree-stack-down.sh` - removes a worktree's stack when the
  worktree is removed. `bash hooks/worktree-stack-down.sh --self-test` runs
  its own checks.
- `settings.json` - the hook wiring.
- `skills/dev-lease/SKILL.md` - the skill: the naming rule, the helper, the
  lease, and what never to do.

## Which set to use

There are two ways to give an agent a lease, and they do different jobs.

- **This set** (`claude-code/`): the full setup. The lease is extended on
  every prompt, the broad prunes are denied, a worktree's stack goes down
  with the worktree, and the agent knows the rules. Use it when the agent
  runs commands on the machine.
- **The one script** ([`../lease-extend.sh`](../lease-extend.sh) with
  [`../claude-code-settings.json`](../claude-code-settings.json)): one hook
  that extends the lease on session start, nothing else. Use it when you
  want the lease and nothing more, for example in a repository that only
  runs an agent for a while.

The two can live together: the one script extends the lease, this set adds
the guard, the worktree cleanup and the skill. The project name each of them
uses is the same one: `COMPOSE_PROJECT_NAME`, the worktree's `.env`, the
compose file's `name:`, or the directory name - in that order.
