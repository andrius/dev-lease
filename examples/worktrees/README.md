# Worktrees

One checkout per branch means many compose projects: the project name must
say which worktree it is, so a lease holds the right one. `up.sh` derives
`<repo>-<worktree>` from git, starts the project and extends its lease; the
`justfile` recipe does the same (`just up 45m` for a 45-minute lease).

Copy `up.sh` and the `justfile` into the repo, tell serve to manage the
derived names with a wildcard, e.g. `DEV_LEASE_PROJECTS=myapp-*` in serve's `.env`, and run
`./up.sh` from the worktree instead of a bare `docker compose up -d`.

The project was started with `docker compose -p <name>`: every later
compose command in that worktree (`down`, `logs`, `ps`, ...) needs the same
`-p <name>` or `COMPOSE_PROJECT_NAME` set, or compose would fall back to
the directory name and address a different project.

An extend shorter than the current lease does not shorten it: dev-lease
takes the later of the current expiry and the requested one, per holder.
