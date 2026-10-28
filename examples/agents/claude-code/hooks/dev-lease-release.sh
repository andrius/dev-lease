#!/usr/bin/env bash
# dev-lease-release.sh - SessionEnd hook
#
# The session is over, so its claim on the worktree's stack ends with it:
# `dev-lease release <name> --holder claude-<first 8 chars of the session id>`.
# It never stops or removes anything - dev-lease stops the stack when no
# holder is left, and removes it after the removal delay. Silent in every
# case: no serve, not a managed project, a non-stack dir - nothing to report
# at session end, exit 0.
#
# The project dir is $CLAUDE_PROJECT_DIR, else the cwd from the stdin payload.
# The helper is found next to this script (../bin/dev-stack or ../dev-stack),
# else on PATH. DEV_STACK_BIN overrides both.
INPUT=$(cat)

FIELDS=$(printf '%s' "$INPUT" | node -e "let d='';process.stdin.on('data',c=>d+=c);process.stdin.on('end',()=>{try{const j=JSON.parse(d);process.stdout.write((j.session_id||'')+'\t'+(j.cwd||''))}catch{}})" 2>/dev/null)
SESSION_ID=${FIELDS%%$'\t'*}
CWD=${FIELDS#*$'\t'}

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

PROJDIR=${CLAUDE_PROJECT_DIR:-$CWD}
[ -n "$PROJDIR" ] || exit 0

NAME=$("$DEV_STACK" name "$PROJDIR" 2> /dev/null) || exit 0
[ -n "$NAME" ] || exit 0

SID8=${SESSION_ID:0:8}
SERVER=$(docker ps -qf label=dev-lease.role=server 2> /dev/null | head -n 1)
[ -n "$SERVER" ] || exit 0
docker exec "$SERVER" dev-lease release "$NAME" --holder "claude-$SID8" > /dev/null 2>&1

exit 0
