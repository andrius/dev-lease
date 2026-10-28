# Examples

Self-contained example directories; copy the one you need:

- `basic/compose.yml` - run dev-lease serve next to the managed projects.
  Always-on: serve stays up and comes back with Docker after a reboot.
- `on-demand/compose.yml` - the same compose file for a laptop:
  `--exit-when-idle=true` and no restart. Serve exits once no managed
  project has a live lease or a running container that is not kept and, with a removal
  delay above 0, nothing stopped is waiting for removal.
- `worktrees/` - `up.sh` (and a `justfile`) for one-checkout-per-branch
  layouts: derives the `<repo>-<worktree>` compose project name, starts
  the project and extends its lease.
- `agents/` - `lease-extend.sh` as a SessionStart hook, the matching
  `claude-code-settings.json` (copy the script to `.claude/hooks/` in your
  repo), an `AGENTS-snippet.md` that explains the lease rules to agents
  (extend while you work, `dev-lease down` when your work on the worktree
  is finished), and `mcp.md` - the MCP server: the `claude mcp add
  dev-lease` line and what each tool does.
- `agents/claude-code/` - the fuller setup for Claude Code: the `dev-stack`
  helper (name, extend, down, status), four hooks (extend the lease,
  release the claim, deny the broad prunes, remove a worktree's stack with
  the worktree), the hook wiring in `settings.json`, the `dev-lease` skill
  and a README with the install. The guide behind it is
  [docs/ai-agents.md](../docs/ai-agents.md).
- `ci/extend.sh` - extend the lease from a self-hosted runner or a local
  CI loop for the length of a test run.

Both compose examples take their variables from a `.env` file next to the
compose file (Compose reads `.env` from the compose file's directory):
copy `.env.example` to `.env` and edit it, do not set the variables
inline in the command.

The example scripts and the commented commands start serve with
`up -d --no-recreate --wait`: an exited serve container is restarted as
it is and is never recreated by a script. That means an edited `.env` is
not picked up by the scripts; after changing a variable, recreate the
container once yourself:
`docker compose -p dev-lease -f <compose.yml> up -d --force-recreate`.
