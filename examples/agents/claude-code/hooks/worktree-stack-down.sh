#!/usr/bin/env bash
# worktree-stack-down.sh - PreToolUse/Bash hook
#
# When a command removes a git worktree, that worktree's compose stack goes
# with it, and BEFORE the directory disappears: dev-stack down <path> tears
# the stack down (containers, volumes, the images compose built for it) and
# releases its lease, then the removal is allowed through. Without this the
# stack would linger until dev-lease's sweep, and compose could no longer
# resolve its files.
#
# The removal is NEVER denied: a path with no stack, docker being down, even a
# failed down - the hook still exits 0 and the command runs.
#
# dev-stack down refuses (exit 3) when the stack it would remove belongs to
# the repository's MAIN worktree - a linked worktree that resolves to the
# main worktree's project name, or the main worktree itself. That refusal is
# inherited here: its line goes to stderr and the removal still runs. The
# repository the removal command ran in is passed on (--repo), so dev-stack
# can tell the main worktree's stack from the branch's own even when the
# directory is already gone.
#
# Seen shapes: `git worktree remove <path>`, `git -C <x> worktree remove
# <path>`, `git-worktree remove <path|branch>`, flags or not. Known limit: a
# path with spaces (the command is tokenised on whitespace, quotes unread).
#
# The helper is found next to this script (../bin/dev-stack or ../dev-stack),
# else on PATH. DEV_STACK_BIN overrides both.
#
# Self-test: bash worktree-stack-down.sh --self-test

# Print one path per worktree-removal command in $1 (empty when none).
worktree_removal_paths() {
  # shellcheck disable=SC2020  # tr maps three chars onto the newline; that is the intent
  printf '%s\n' "$1" | tr ';|&' '\n\n\n' | while IFS= read -r seg; do
    [ -n "$seg" ] || continue
    set -f
    # shellcheck disable=SC2086  # tokenising on whitespace is the contract here
    set -- $seg
    set +f
    [ $# -gt 0 ] || continue
    case "$1" in
      git)
        shift
        # global options and their arguments before the subcommand
        while [ $# -gt 0 ]; do
          case "$1" in
            -C|-c|--namespace) shift 2 ;;
            -*) shift ;;
            *) break ;;
          esac
        done
        [ "${1:-}" = worktree ] || continue
        shift
        while [ $# -gt 0 ] && [ "$1" != remove ]; do shift; done
        [ "${1:-}" = remove ] || continue
        shift ;;
      git-worktree)
        shift
        while [ $# -gt 0 ] && [ "$1" != remove ]; do
          case "$1" in -*) shift ;; *) break ;; esac
        done
        [ "${1:-}" = remove ] || continue
        shift ;;
      *) continue ;;
    esac
    # flags after `remove`, then the first word is the path (or, for the
    # wrapper, the branch name: dev-stack down answers that with the
    # normalised basename, which matches nothing and harms nothing)
    while [ $# -gt 0 ]; do
      case "$1" in
        -*) shift ;;
        *) printf '%s\n' "$1"; break ;;
      esac
    done
  done
}

# The repository the removal command names (`git -C <dir> ...`), or nothing.
worktree_removal_repo() {
  # shellcheck disable=SC2020  # tr maps three chars onto the newline; that is the intent
  printf '%s\n' "$1" | tr ';|&' '\n\n\n' | while IFS= read -r seg; do
    [ -n "$seg" ] || continue
    set -f
    # shellcheck disable=SC2086  # tokenising on whitespace is the contract here
    set -- $seg
    set +f
    [ "${1:-}" = git ] || continue
    shift
    while [ $# -gt 0 ]; do
      case "$1" in
        -C)
          [ -n "${2:-}" ] || return 0
          printf '%s\n' "$2"
          return 0 ;;
        -c|--namespace) shift 2 ;;
        -*) shift ;;
        *) break ;;
      esac
    done
  done
}

self_test() {
  local fails=0 out
  check() { # check <expected paths, space-separated> <command>
    out=$(worktree_removal_paths "$2" | tr '\n' ' ')
    out=${out% }
    if [ "$out" != "$1" ]; then
      echo "FAIL: [$2] gave [$out], want [$1]" >&2
      fails=$((fails + 1))
    fi
  }

  check '/x/wt' 'git worktree remove /x/wt'
  check '/x/wt' 'git worktree remove --force /x/wt'
  check '/x/wt' 'git -C /y worktree remove -f /x/wt'
  check '/x/wt' 'git --git-dir=/y/.git worktree remove /x/wt'
  check 'feat-x' 'git-worktree remove feat-x'
  check 'feat-x' 'git-worktree remove -f feat-x'
  check '/a /b' 'git worktree add foo; git worktree remove /a && git worktree remove /b'
  check '' 'git worktree list'
  check '' 'git worktree prune'
  check '' 'git worktree add feat-x --from main'
  check '' 'git-worktree sync'
  check '' 'rm -rf /x/wt'
  check '' 'git checkout main'
  check '' 'git commit -m "worktree remove /x/wt"'

  # The wire path with a recording dev-stack: the down runs, the removal is
  # still allowed (exit 0), and an unrelated command records nothing.
  local t rc=0
  t=$(mktemp -d "${TMPDIR:-/tmp}/wt-stack-down-test.XXXXXX")
  printf '#!/bin/bash\necho "$*" >> %s/calls\nexit 0\n' "$t" > "$t/dev-stack"
  chmod +x "$t/dev-stack"
  printf '{"tool_name":"Bash","tool_input":{"command":"git worktree remove /x/wt"}}' \
    | DEV_STACK_BIN="$t/dev-stack" bash "$0" > /dev/null 2>&1; rc=$?
  { [ "$rc" = 0 ] && grep -q '^down /x/wt --repo .' "$t/calls" 2>/dev/null; } \
    || { echo "FAIL: wire path rc=$rc calls=[$(cat "$t/calls" 2>/dev/null)]" >&2; fails=$((fails + 1)); }
  : > "$t/calls"
  printf '{"tool_name":"Bash","tool_input":{"command":"ls -la"}}' \
    | DEV_STACK_BIN="$t/dev-stack" bash "$0" > /dev/null 2>&1; rc=$?
  { [ "$rc" = 0 ] && [ ! -s "$t/calls" ]; } \
    || { echo "FAIL: unrelated command recorded something (rc=$rc)" >&2; fails=$((fails + 1)); }
  rm -rf "$t"

  if [ "$fails" -gt 0 ]; then
    echo "worktree-stack-down self-test: $fails failure(s)" >&2
    return 1
  fi
  echo "worktree-stack-down self-test OK"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

INPUT=$(cat)

FIELDS=$(printf '%s' "$INPUT" | node -e "let d='';process.stdin.on('data',c=>d+=c);process.stdin.on('end',()=>{try{const j=JSON.parse(d),i=j.tool_input||{};process.stdout.write((j.tool_name&&j.tool_name!=='Bash'?'':i.command||'')+'\t'+(j.cwd||''))}catch{}})" 2>/dev/null)
CMD=${FIELDS%%$'\t'*}
CWD=${FIELDS#*$'\t'}

# No node, no payload or not a Bash call: nothing to do, and never a denial.
[ -n "$CMD" ] || exit 0

# The repository the removal command ran in: the one it names with -C, else
# the directory the command ran in.
REPO=$(worktree_removal_repo "$CMD")
[ -n "$REPO" ] || REPO=${CWD:-$PWD}

HOOK_DIR=$(cd "$(dirname "$0")" && pwd)
if [ -n "${DEV_STACK_BIN:-}" ]; then
  DEV_STACK=$DEV_STACK_BIN
elif [ -f "$HOOK_DIR/../bin/dev-stack" ]; then
  DEV_STACK="$HOOK_DIR/../bin/dev-stack"
elif [ -f "$HOOK_DIR/../dev-stack" ]; then
  DEV_STACK="$HOOK_DIR/../dev-stack"
else
  DEV_STACK=dev-stack
fi

worktree_removal_paths "$CMD" | while IFS= read -r path; do
  [ -n "$path" ] || continue
  "$DEV_STACK" down "$path" --repo "$REPO" || true
done

exit 0
