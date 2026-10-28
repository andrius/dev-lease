#!/usr/bin/env bash
# docker-prune-guard.sh - PreToolUse/Bash hook
#
# Blocks the broad prunes that delete OTHER worktrees' Docker objects. The
# rule: a worktree's compose stack is leased while used and removed when the
# work is done - nothing broader. dev-lease already stops idle stacks and
# removes them later, so a machine-wide prune has no legitimate job left.
#
#   DENY  docker system prune, docker volume prune, docker container prune
#         (any flags, `-af` and `--all` included), docker image prune with
#         -a/--all. Sudo, env prefixes and compound commands are seen through.
#   ALLOW docker builder prune (build cache only), docker image prune without
#         -a (dangling images only), and every non-prune command. `docker
#         network prune` is deliberately not listed either way: it is not a
#         data delete, and the deny list is exactly the four above.
#
# The deny is the same shape as a secrets guard: the reason on stderr, exit 2.
# Self-test: bash docker-prune-guard.sh --self-test

# A heredoc BODY is data being written to a file, not a command being run;
# documentation that names a prune command must stay writable.
strip_heredocs() {
  awk '
    inhd { if ($0 ~ "^[ \t]*" delim "[ \t]*$") inhd = 0; next }
    { print }
    /<<</ { next }
    match($0, /<<-?[ \t]*["'"'"']?[A-Za-z_][A-Za-z0-9_]*/) {
      d = substr($0, RSTART, RLENGTH)
      sub(/^<<-?[ \t]*/, "", d)
      gsub(/["'"'"']/, "", d)
      delim = d; inhd = 1
    }
  '
}

# docker <area> prune at the START of a command segment (after splitting on
# ; | & ( ) ` and newlines, so prose and compound commands are both handled).
BROAD_PRUNE_RE='^docker[[:space:]]+(system|volume|container)[[:space:]]+prune([[:space:]]|$)'
IMAGE_PRUNE_RE='^docker[[:space:]]+image[[:space:]]+prune([[:space:]]|$)'
# An -a/--all on the image-prune segment's flag tokens: `-a`, `-af`, `--all`,
# `--all=true` - but never `--filter until=24h`.
IMAGE_ALL_RE='(^|[[:space:]])--all([[:space:]=]|$)|(^|[[:space:]])-[A-Za-z]*a[A-Za-z]*([[:space:]]|$)'

DENY_REASON="docker-prune-guard: a broad prune deletes other worktrees' data. Remove your own stack with 'dev-stack down <worktree>'; idle stacks are stopped and removed by dev-lease. 'docker builder prune' and 'docker image prune' without -a (dangling only) stay allowed."

# Print one cleaned command segment after another; docker must be at command
# position, so an env assignment or sudo before it is stripped word by word.
prune_segments() {
  local seg w
  # shellcheck disable=SC2020  # tr maps six chars onto the newline; that is the intent
  printf '%s\n' "$1" | tr ';|&()`' '\n\n\n\n\n\n' | while IFS= read -r seg; do
    seg=${seg#"${seg%%[![:space:]]*}"}
    while [ -n "$seg" ]; do
      w=${seg%%[[:space:]]*}
      case "$w" in
        sudo|nohup|nice|env|command|[A-Za-z_][A-Za-z0-9_]*=*)
          seg=${seg#"$w"}
          seg=${seg#"${seg%%[![:space:]]*}"} ;;
        *) break ;;
      esac
    done
    [ -n "$seg" ] && printf '%s\n' "$seg"
  done
}

# Returns 0 (and prints the reason) when $1 runs a prune this hook denies.
prune_guard_violation() {
  local cmd seg rest
  cmd="$(printf '%s' "$1" | strip_heredocs)"
  while IFS= read -r seg; do
    if [[ "$seg" =~ $BROAD_PRUNE_RE ]]; then
      echo "$DENY_REASON"
      return 0
    fi
    if [[ "$seg" =~ $IMAGE_PRUNE_RE ]]; then
      rest=${seg#*prune}
      if [[ "$rest" =~ $IMAGE_ALL_RE ]]; then
        echo "$DENY_REASON"
        return 0
      fi
    fi
  done < <(prune_segments "$cmd")
  return 1
}

self_test() {
  local fails=0
  check() { # check <expect: block|pass> <command>
    if prune_guard_violation "$2" > /dev/null; then
      [ "$1" = block ] || { echo "FAIL: expected pass, blocked: $2" >&2; fails=$((fails + 1)); }
    else
      [ "$1" = pass ] || { echo "FAIL: expected block, passed: $2" >&2; fails=$((fails + 1)); }
    fi
  }

  # The four denied prunes, in the shapes an agent actually types.
  check block 'docker system prune'
  check block 'docker system prune -af'
  check block 'docker system prune --all --force'
  check block 'docker volume prune'
  check block 'docker volume prune -f'
  check block 'docker container prune'
  check block 'docker image prune -a'
  check block 'docker image prune --all'
  check block 'docker image prune -af'
  check block 'docker image prune --filter until=24h -a'
  # Not at the start of the line, but still at command position.
  check block 'git add -A; docker volume prune'
  check block 'cd /wt && docker system prune --force'
  check block 'sudo docker volume prune'
  check block 'FOO=1 docker container prune'
  # shellcheck disable=SC2016
  check block 'x=$(docker system prune)'
  check block 'docker system prune; docker compose up -d'

  # Must keep working: the stack-local and cache-only cleanups.
  check pass 'docker builder prune'
  check pass 'docker builder prune -a'
  check pass 'docker image prune'
  check pass 'docker image prune -f'
  check pass 'docker image prune --filter until=24h'
  check pass 'docker ps -a'
  check pass 'docker compose ps'
  check pass 'docker compose down -v --remove-orphans'
  check pass 'docker compose -p myapp-feat down -v --remove-orphans --rmi local'
  check pass 'docker system df'
  check pass 'git worktree remove /x/wt'
  check pass 'dev-stack down /x/wt'
  # Named in prose, not run.
  check pass 'echo "docs: docker system prune is denied by the guard"'

  # A heredoc body is data being written, not a command being run.
  check pass "$(printf 'cat > doc.md <<EOF\ndocker system prune is denied\nEOF')"
  # ... while a real prune on the second line of a script still is.
  check block "$(printf 'cd /tmp\ndocker volume prune')"

  # The wire path, not just the predicate.
  local out rc=0
  out=$(printf '{"tool_name":"Bash","tool_input":{"command":"docker system prune"}}' | bash "$0" 2>&1) || rc=$?
  [ "$rc" -eq 2 ] || { echo "FAIL: hook stdin path exited $rc, expected 2" >&2; fails=$((fails + 1)); }
  case "$out" in
    *"docker-prune-guard:"*dev-stack*) ;;
    *) echo "FAIL: hook stdin path gave no reason on stderr: $out" >&2; fails=$((fails + 1)); ;;
  esac
  rc=0
  printf '{"tool_name":"Bash","tool_input":{"command":"docker image prune"}}' | bash "$0" > /dev/null 2>&1 || rc=$?
  [ "$rc" -eq 0 ] || { echo "FAIL: hook stdin path blocked a safe command (exit $rc)" >&2; fails=$((fails + 1)); }

  if [ "$fails" -gt 0 ]; then
    echo "docker-prune-guard self-test: $fails failure(s)" >&2
    return 1
  fi
  echo "docker-prune-guard self-test OK"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

INPUT=$(cat)

FIELDS=$(printf '%s' "$INPUT" | node -e "let d='';process.stdin.on('data',c=>d+=c);process.stdin.on('end',()=>{try{const j=JSON.parse(d),i=j.tool_input||{};process.stdout.write([j.tool_name||'',i.command||''].join('\\t'))}catch{}})" 2>/dev/null)

# Node missing or the parse thrown: fall back to the raw payload and refuse
# anything that plainly names a denied prune, else allow (the same closed-then-
# open compromise a secrets guard makes: a broken hook must not block every
# Bash call on the machine, only the ones the raw text already condemns).
if [ -z "$FIELDS" ]; then
  if printf '%s' "$INPUT" | grep -Eq 'docker[[:space:]]+(system|volume|container)[[:space:]]+prune|docker[[:space:]]+image[[:space:]]+prune.*(-a|--all)'; then
    echo "$DENY_REASON" >&2
    exit 2
  fi
  exit 0
fi

TOOL=${FIELDS%%$'\t'*}
CMD=${FIELDS#*$'\t'}

[ -n "$CMD" ] || exit 0
[ -z "$TOOL" ] || [ "$TOOL" = Bash ] || exit 0

if REASON=$(prune_guard_violation "$CMD"); then
  # exit 2 blocks the call; the reason must go to stderr (stdout is discarded on non-zero exit)
  echo "$REASON" >&2
  exit 2
fi

exit 0
