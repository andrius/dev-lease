#!/bin/sh
# Start the current worktree's compose project and extend its lease.
#
# The project name is <repo>-<worktree> derived from git (git's name for
# the worktree, normally its directory name), lower-cased and normalised
# the way compose normalises names, including dropping leading - and _.
# Usage: ./up.sh [ttl]  (ttl defaults to 2h)
set -eu

ttl=${1:-2h}

if ! git rev-parse --git-dir >/dev/null 2>&1; then
	echo "dev-lease: not in a git repository; nothing started" >&2
	exit 0
fi
gitdir=$(git rev-parse --git-dir)
worktree=
case "$gitdir" in
	*/worktrees/*) worktree=$(basename "$gitdir") ;;
esac
common=$(git rev-parse --git-common-dir)
if [ -d "$common" ] && [ "$gitdir" != "$common" ]; then
	# Worktree of a bare repo: the repo name is the directory holding
	# the common git dir.
	repo=$(basename "$(cd "$(dirname "$common")" && pwd)")
else
	repo=$(basename "$(git rev-parse --show-toplevel)")
fi
if [ -n "$worktree" ]; then
	project="${repo}-${worktree}"
else
	project="$repo"
fi
project=$(printf '%s' "$project" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9_-]//g; s/^[-_]*//')

docker compose -p "$project" up -d --wait

server=$(docker ps -qf label=dev-lease.role=server 2>/dev/null) || server=
if [ -z "$server" ] && [ -n "${DEV_LEASE_COMPOSE:-}" ]; then
	startout=$(docker compose -p dev-lease -f "$DEV_LEASE_COMPOSE" up -d --no-recreate --wait 2>&1) || true
	server=$(docker ps -qf label=dev-lease.role=server 2>/dev/null) || server=
	if [ -z "$server" ]; then
		echo "dev-lease serve is not running and could not be started: $startout" >&2
		exit 0
	fi
fi
if [ -z "$server" ]; then
	echo "dev-lease serve is not running; start it with: docker compose -p dev-lease -f <path-to-compose.yml> up -d --no-recreate --wait" >&2
	echo "DEV_LEASE_PROJECTS must be set (for example to '$project') in the .env file next to that compose file" >&2
	exit 0
fi
if ! out=$(docker exec "$server" dev-lease extend "$project" "$ttl" --holder "${USER:-unknown}" 2>&1); then
	echo "lease extend for $project failed: $out" >&2
fi
