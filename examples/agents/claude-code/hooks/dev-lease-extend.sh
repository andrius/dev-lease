#!/usr/bin/env bash
# dev-lease-extend.sh - SessionStart / UserPromptSubmit hook
#
# A session working in a compose-stack directory keeps that stack's lease
# alive: every start and every prompt extends it by 1h as the holder
# claude-<first 8 chars of the session id>. dev-lease stops a stack whose
# lease expired and removes it later, so the extension is what marks the stack
# as in use. Silent on success; on any failure dev-stack extend itself prints
# one short stderr line and exits 0, and so does this hook - it never blocks
# the session.
#
# The project dir is $CLAUDE_PROJECT_DIR, else the cwd from the stdin payload.
# A dir that is not a stack directory (no compose file, no project name) is not
# an error: nothing to extend, exit 0.
#
# The helper is found next to this script (../bin/dev-stack or
# ../dev-stack), else on PATH. DEV_STACK_BIN overrides both.
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

# Only a stack directory gets a lease; dev-stack extend checks that itself and
# reports on stderr, so a plain repo session stays completely quiet.
if ! "$DEV_STACK" name "$PROJDIR" > /dev/null 2>&1; then
  exit 0
fi

SID8=${SESSION_ID:0:8}
DEV_LEASE_HOLDER="claude-$SID8" "$DEV_STACK" extend 1h "$PROJDIR"

exit 0
