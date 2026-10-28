#!/bin/sh
# Extend the lease of the current project for the length of a test run, on
# a self-hosted runner or in a local CI loop. Same contract as
# examples/agents/lease-extend.sh: never blocks the pipeline, exits 0.
#
# Usage: ./extend.sh [ttl]   (ttl defaults to 1h)
set -eu

ttl=${1:-1h}

# The CI job's project: the job can pass its own name via CI_PROJECT,
# otherwise <repo>-<worktree> is derived from git the same way the other
# scripts do it: lower-cased, characters outside [a-z0-9_-] dropped,
# leading - and _ dropped.
project=${CI_PROJECT:-}
if [ -z "$project" ]; then
	if ! git rev-parse --git-dir >/dev/null 2>&1; then
		echo "dev-lease: not in a git repository; lease not extended" >&2
		exit 0
	fi
	gitdir=$(git rev-parse --git-dir)
	worktree=
	case "$gitdir" in
		*/worktrees/*) worktree=$(basename "$gitdir") ;;
	esac
	common=$(git rev-parse --git-common-dir)
	if [ -d "$common" ] && [ "$gitdir" != "$common" ]; then
		# Worktree of a bare repo: the repo name is the directory
		# holding the common git dir.
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
fi
holder=${GITHUB_ACTOR:-ci}

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
if ! out=$(docker exec "$server" dev-lease extend "$project" "$ttl" --holder "$holder" 2>&1); then
	echo "lease extend for $project failed: $out" >&2
fi
