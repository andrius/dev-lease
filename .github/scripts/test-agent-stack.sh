#!/usr/bin/env bash
# Tests the agent example helper and its four hooks against a fake docker on
# a curated PATH. The real daemon is never touched: the fake bin dir shadows
# it, and the no-docker cases run with a PATH that has no docker at all.
#
# The helper under test is examples/agents/claude-code/dev-stack; DEV_STACK
# points the test at another copy (a stub, for a mutation test). PRUNE_HOOK
# does the same for the prune guard.
#
# Case map:
#  1-7   name: COMPOSE_PROJECT_NAME env beats .env beats the compose name:
#        which beats the normalised basename; compose.yaml beats compose.yml;
#        My_App.v2 -> my_appv2; no compose file and no name -> exit 1 with a
#        message; the env var alone makes a stack directory.
#  8-12  extend: no docker -> exit 0 and one stderr line; no serve -> same,
#        and no exec call; with serve -> the recorded
#        `docker exec <id> dev-lease extend <name> 1h --holder <h>`; the
#        holder defaults to $USER; a failed extend still exits 0.
# 13-19  down: through serve when serve knows the down command; through
#        compose when it does not (an old image) and when serve is not
#        running; the release --all after the removal; the dev-lease project
#        refused with no docker call; -p <name> with the dir gone -> down -v
#        --remove-orphans without --rmi; a gone dir without -p -> the
#        normalised basename; a dir with no stack -> one stderr line and no
#        docker call.
# 20-21  status: dev-lease list through serve, else docker compose ls -a.
# 22-27  the prune guard on the wire: system/volume/container prune and
#        `image prune -a` denied with exit 2 and a reason; ps, a dangling
#        image prune and builder prune allowed; the deny without node.
# 28-31  the worktree-removal hook: git worktree remove, git -C <x> worktree
#        remove --force and git-worktree remove each run the removal of that
#        worktree's stack and still allow the command; an unrelated command
#        and a path with no stack record nothing.
# 32-34  the extend and release hooks: holder claude-<first 8 of the session
#        id>, ttl 1h, silent on success, no call for a non-stack cwd,
#        CLAUDE_PROJECT_DIR beats the payload cwd, and the release hook
#        releases only its own claim.
# 35-39  ownership: a linked worktree whose project name is the main
#        worktree's -> exit 3 and no docker call; the same worktree with its
#        own COMPOSE_PROJECT_NAME -> its own stack goes down; the main
#        worktree -> exit 3, and with --main it goes down; down -p <the main
#        worktree's project> --repo <repo> -> exit 3, another name -> it goes
#        down; the worktree-removal hook on the shared stack -> no removal
#        call, the removal allowed, the refusal on stderr.
# 40-41  the same in a bare layout (a bare repository plus worktree
#        checkouts), where the bare repository is the first entry git prints
#        and the main worktree is the one on the main branch.
set -u
root="$(git rev-parse --show-toplevel)"
DEV=${DEV_STACK:-$root/examples/agents/claude-code/dev-stack}
AGENTS=$root/examples/agents/claude-code
EXTEND_HOOK=$AGENTS/hooks/dev-lease-extend.sh
RELEASE_HOOK=$AGENTS/hooks/dev-lease-release.sh
PRUNE_HOOK=${PRUNE_HOOK:-$AGENTS/hooks/docker-prune-guard.sh}
WT_HOOK=$AGENTS/hooks/worktree-stack-down.sh
# The hooks parse their payload with node, so node has to be on the curated
# PATH. The mise shim resolves its version from a PATH mise can read, and the
# curated bin dir is not that: the real binary behind the shim is linked in.
REALNODE=$(node -e 'process.stdout.write(process.execPath)' 2>/dev/null || true)
if [ -z "$REALNODE" ]; then
	echo "node is required to run this test: the hooks read the hook payload with it" >&2
	exit 1
fi
T=$(mktemp -d "${TMPDIR:-/tmp}/dev-stack-test.XXXXXX"); trap 'rm -rf "$T"' EXIT
fail=0
pass() { echo "case $1 PASS ($2)"; }
bad() { echo "case $1 FAIL: $2"; fail=1; }

# The fake docker: one recorded line per call ("docker <args>"), answers from
# the test's environment. It must shadow the real one, so it is first on PATH.
mkdir -p "$T/bin"
cat > "$T/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker $*" >> "$DLOG"
case "$1" in
  ps)
    [ "$2" = "-qf" ] && [ "$3" = "label=dev-lease.role=server" ] && [ -n "$FAKE_SERVE" ] && printf '%s\n' "$FAKE_SERVE"
    exit "${FAKE_PS_RC:-0}" ;;
  exec)
    shift 2
    [ "${1:-}" = dev-lease ] && shift
    case "${1:-}" in
      --help|-h|help)
        # An image that knows the down command, or an older one that does not.
        if [ "${FAKE_SERVE_HAS_DOWN:-1}" = 1 ]; then
          printf '%s\n' "  dev-lease down <project> [--holder <name>]"
        else
          printf '%s\n' "  dev-lease extend <project> <ttl> [--holder <name>]"
        fi
        exit 0 ;;
      down)
        [ -n "${FAKE_DOWN_OUT:-}" ] && printf '%s\n' "$FAKE_DOWN_OUT"
        [ -n "${FAKE_DOWN_OUT:-}" ] || printf 'removed %s\n' "${2:-}"
        exit "${FAKE_DOWN_RC:-0}" ;;
      list)
        printf '%s\n' "no leases"
        exit 0 ;;
    esac
    exit "${FAKE_EXEC_RC:-0}" ;;
  compose)
    [ -n "$FAKE_COMPOSE_OUT" ] && printf '%s\n' "$FAKE_COMPOSE_OUT"
    exit "${FAKE_COMPOSE_RC:-0}" ;;
esac
exit 0
STUB
chmod +x "$T/bin/docker"
export DLOG="$T/calls.log"
export FAKE_SERVE=srv123
# The curated bin dir: the fake docker and the tools the tools under test
# call, and nothing else. No /usr/bin and no /bin on that PATH, so the real
# docker is out of reach.
for tool in cat tr sed head basename grep id awk git mktemp dirname bash sh; do
  ln -s "$(command -v "$tool")" "$T/bin/$tool"
done
ln -s "$REALNODE" "$T/bin/node"
P="$T/bin"                                 # the fake docker, no real one
# A PATH with no docker ANYWHERE: just the tools dev-stack itself needs.
mkdir -p "$T/nodocker-bin"
for tool in cat tr sed head basename grep id awk mktemp dirname bash sh; do
  ln -s "$(command -v "$tool")" "$T/nodocker-bin/$tool"
done
PNODOCKER="$T/nodocker-bin"

# The curated PATH must not be able to reach a real docker: the fake is the
# only docker on it, and it is a file of this test, not a link to one.
run() { : > "$DLOG"; }                  # reset the recording before a case
calls() { cat "$DLOG" 2>/dev/null; }
has() { calls | grep -Fq -- "$1"; }
nlines() { calls | grep -c . || true; }
real_docker=$(command -v docker 2>/dev/null || true)
if [ "$(PATH="$P" command -v docker)" = "$T/bin/docker" ] && [ ! -L "$T/bin/docker" ] \
   && grep -q 'dev-lease.role=server' "$T/bin/docker"; then
  pass 0 "the curated PATH holds the fake docker and no real one${real_docker:+ (the box has one at $real_docker)}"
else
  bad 0 "the curated PATH does not hold only the fake docker"
fi
if ! PATH="$PNODOCKER" command -v docker > /dev/null 2>&1; then
  pass 0b "a PATH with no docker at all finds none"
else
  bad 0b "a PATH without docker still finds one"
fi

# fixtures
mkdir -p "$T/My_App.v2" "$T/full" "$T/downstack" "$T/wtstack" "$T/plain" "$T/dev-lease-dir" "$T/order"
printf 'services:\n  web:\n    image: busybox\n' > "$T/My_App.v2/compose.yml"          # no name: -> basename
printf 'services:\n  web:\n    image: busybox\nname: curname\n' > "$T/full/compose.yaml"
printf 'services:\n  web:\n    image: busybox\nname: fromyml\n' > "$T/full/compose.yml"
printf 'COMPOSE_PROJECT_NAME=envname\nOTHER_VALUE=never-print-me\n' > "$T/full/.env"
printf 'name: fromyaml\n' > "$T/order/compose.yaml"
printf 'name: fromyml\n' > "$T/order/compose.yml"
printf 'name: dstack\nservices:\n  web:\n    image: busybox\n' > "$T/downstack/compose.yaml"
printf 'name: wtstack\nservices:\n  web:\n    image: busybox\n' > "$T/wtstack/compose.yaml"
printf 'COMPOSE_PROJECT_NAME=dev-lease\n' > "$T/dev-lease-dir/.env"

# --- 1-7: name ---
run; out=$(env COMPOSE_PROJECT_NAME=fromenv PATH="$P" "$DEV" name "$T/full" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$out" = "fromenv" ] && [ ! -s "$T/err" ]; then pass 1 "the env var beats .env and the compose name"; else bad 1 "rc=$rc out=[$out] err=[$(cat "$T/err")]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" name "$T/full" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$out" = "envname" ] && [ ! -s "$T/err" ]; then pass 2 ".env beats the compose name, and no other .env value is printed"; else bad 2 "rc=$rc out=[$out] err=[$(cat "$T/err")]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" name "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$out" = "dstack" ]; then pass 3 "the compose top-level name"; else bad 3 "rc=$rc out=[$out]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" name "$T/My_App.v2" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$out" = "my_appv2" ]; then pass 4 "the normalised basename: My_App.v2 -> my_appv2"; else bad 4 "rc=$rc out=[$out]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" name "$T/plain" 2>"$T/err"); rc=$?
if [ "$rc" = 1 ] && grep -q "not a stack" "$T/err"; then pass 5 "no compose file and no name -> exit 1 with a message"; else bad 5 "rc=$rc err=[$(cat "$T/err")]"; fi
run; out=$(env COMPOSE_PROJECT_NAME=loneenv PATH="$P" "$DEV" name "$T/plain" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$out" = "loneenv" ]; then pass 6 "the env var alone makes a stack directory"; else bad 6 "rc=$rc out=[$out]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" name "$T/order" 2>/dev/null); rc=$?
if [ "$rc" = 0 ] && [ "$out" = "fromyaml" ]; then pass 7 "compose.yaml is read before compose.yml"; else bad 7 "rc=$rc out=[$out]"; fi

# --- 8-12: extend ---
run; out=$(env PATH="$PNODOCKER" DEV_LEASE_HOLDER=h1 "$DEV" extend 1h "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && grep -q "not extended" "$T/err" && [ ! -s "$DLOG" ]; then pass 8 "no docker -> exit 0 and one stderr line"; else bad 8 "rc=$rc err=[$(cat "$T/err")] calls=$(nlines)"; fi
run; out=$(env PATH="$P" FAKE_SERVE= DEV_LEASE_HOLDER=h1 "$DEV" extend 1h "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && grep -q "not extended" "$T/err" && ! grep -q " exec " "$DLOG"; then pass 9 "no serve -> exit 0, one stderr line, no exec"; else bad 9 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi
run; out=$(env PATH="$P" DEV_LEASE_HOLDER=h1 "$DEV" extend 1h "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ -z "$out" ] && [ "$(calls | tail -1)" = "docker exec srv123 dev-lease extend dstack 1h --holder h1" ]; then pass 10 "serve -> docker exec ... dev-lease extend dstack 1h --holder h1"; else bad 10 "rc=$rc out=[$out] calls=[$(calls)]"; fi
run; out=$(env PATH="$P" "$DEV" extend 1h "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$(calls | tail -1)" = "docker exec srv123 dev-lease extend dstack 1h --holder ${USER:-$(id -un)}" ]; then pass 11 "the holder defaults to \$USER"; else bad 11 "calls=[$(calls)]"; fi
run; out=$(env PATH="$P" DEV_LEASE_HOLDER=h1 FAKE_EXEC_RC=1 "$DEV" extend 1h "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && grep -q "extend dstack failed" "$T/err"; then pass 12 "a failed extend still exits 0 with one stderr line"; else bad 12 "rc=$rc err=[$(cat "$T/err")]"; fi

# --- 13-19: down ---
run; out=$(env PATH="$P" "$DEV" down "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "docker exec srv123 dev-lease down dstack" && ! has "compose -p dstack down" \
   && has "dev-lease release dstack --all" && [ "$out" = "removed dstack" ]; then
  pass 13 "serve knows the down command -> the removal goes through serve, then the release"
else bad 13 "rc=$rc out=[$out] calls=[$(calls)] err=[$(cat "$T/err")]"; fi
run; out=$(env PATH="$P" FAKE_SERVE_HAS_DOWN=0 FAKE_COMPOSE_OUT=$' Container dstack-web-1  Stopping \n Container dstack-web-1  Stopped \n Container dstack-web-1  Removing \n Container dstack-web-1  Removed \n Network dstack_default  Removing \n Network dstack_default  Removed ' \
  "$DEV" down "$T/downstack" 2>"$T/err"); rc=$?
downline=$(calls | grep -Fn "down -v --remove-orphans --rmi local" | head -n 1)
relline=$(calls | grep -Fn "dev-lease release dstack --all" | head -n 1)
want_out=$'Container dstack-web-1 Removed\nNetwork dstack_default Removed'
if [ "$rc" = 0 ] && [ -n "$downline" ] && [ -n "$relline" ] && [ "${downline%%:*}" -lt "${relline%%:*}" ] && [ "$out" = "$want_out" ]; then
  pass 14 "an older serve image -> compose -p dstack down -v --remove-orphans --rmi local, then release --all, one line per removed thing"
else bad 14 "rc=$rc out=[$out] calls=[$(calls)]"; fi
run; out=$(env PATH="$P" FAKE_SERVE= FAKE_COMPOSE_OUT=" Container dstack-web-1  Removed" "$DEV" down "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "compose -p dstack down -v --remove-orphans --rmi local" && ! has "dev-lease down"; then
  pass 15 "no serve -> the removal goes through compose alone"
else bad 15 "rc=$rc out=[$out] calls=[$(calls)]"; fi
run; out=$(env PATH="$P" "$DEV" down "$T/dev-lease-dir" 2>"$T/err"); rc=$?
if [ "$rc" != 0 ] && grep -q "refus" "$T/err" && [ ! -s "$DLOG" ]; then pass 16 "the dev-lease project itself is refused, no docker call"; else bad 16 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi
run; out=$(env PATH="$P" FAKE_SERVE= "$DEV" down -p orphancode 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$(calls | grep -Fc 'down -v --remove-orphans')" = 1 ] && ! has "--rmi" && ! has "dev-lease down"; then pass 17 "-p <name>, dir gone, no serve -> down -v --remove-orphans (no --rmi)"; else bad 17 "rc=$rc calls=[$(calls)]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" FAKE_SERVE= "$DEV" down "$T/gone/My_Old.WT" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "compose -p my_oldwt down -v --remove-orphans"; then pass 18 "a gone dir -> the normalised basename my_oldwt"; else bad 18 "rc=$rc calls=[$(calls)]"; fi
run; out=$(env PATH="$P" "$DEV" down -p orphancode 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "dev-lease down orphancode" && has "dev-lease release orphancode --all"; then pass 18b "a gone dir with serve -> the removal goes through serve by name"; else bad 18b "rc=$rc calls=[$(calls)]"; fi
run; out=$(env PATH="$P" FAKE_DOWN_RC=2 "$DEV" down "$T/downstack" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "compose -p dstack down -v --remove-orphans --rmi local" && grep -q "not managed by dev-lease" "$T/err"; then
  pass 18c "a project serve does not manage -> the removal falls back to compose"
else bad 18c "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down "$T/plain" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && [ ! -s "$DLOG" ]; then pass 19 "a dir with no stack -> one stderr line, no docker call"; else bad 19 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi

# --- 20-21: status ---
run; out=$(env PATH="$P" "$DEV" status 2>/dev/null); rc=$?
if [ "$rc" = 0 ] && [ "$(calls | tail -1)" = "docker exec srv123 dev-lease list" ]; then pass 20 "status -> dev-lease list through serve"; else bad 20 "calls=[$(calls)]"; fi
run; out=$(env PATH="$P" FAKE_SERVE= "$DEV" status 2>/dev/null); rc=$?
if [ "$rc" = 0 ] && [ "$(calls | tail -1)" = "docker compose ls -a" ]; then pass 21 "no serve -> docker compose ls -a"; else bad 21 "calls=[$(calls)]"; fi

# --- 22-27: the prune guard on the wire ---
wired() { # wired <expect: deny|allow> <command>
  local rc=0
  printf '{"tool_name":"Bash","tool_input":{"command":"%s"}}' "$2" \
    | env PATH="$P" bash "$PRUNE_HOOK" > "$T/out" 2>"$T/err" || rc=$?
  if [ "$1" = deny ]; then
    [ "$rc" = 2 ] && grep -q "docker-prune-guard:" "$T/err" && grep -q "dev-stack down" "$T/err"
  else
    [ "$rc" = 0 ]
  fi
}
if wired deny 'docker system prune'; then pass 22 "docker system prune denied, the reason names dev-stack down"; else bad 22 "the deny did not happen"; fi
if wired deny 'docker volume prune -f'; then pass 23 "docker volume prune denied"; else bad 23 "the deny did not happen"; fi
if wired deny 'docker container prune'; then pass 24 "docker container prune denied"; else bad 24 "the deny did not happen"; fi
if wired deny 'docker image prune -a'; then pass 25 "docker image prune -a denied"; else bad 25 "the deny did not happen"; fi
if wired allow 'docker ps'; then pass 26 "docker ps allowed"; else bad 26 "a safe command was blocked"; fi
if wired allow 'docker image prune'; then pass 27 "a dangling image prune allowed"; else bad 27 "a safe command was blocked"; fi
# Without node the hook cannot parse the payload: it falls back to a plain
# text check and still denies the prunes it can see.
rc=0
printf '{"tool_name":"Bash","tool_input":{"command":"docker system prune"}}' \
  | env PATH="$PNODOCKER" bash "$PRUNE_HOOK" > "$T/out" 2>"$T/err" || rc=$?
if [ "$rc" = 2 ] && grep -q "docker-prune-guard:" "$T/err"; then pass 27b "without node the guard still denies a plainly named prune"; else bad 27b "rc=$rc err=[$(cat "$T/err")]"; fi

# --- 28-31: the worktree-removal hook ---
wtcase() { # wtcase <id> <command>
  local rc=0
  run
  printf '{"tool_name":"Bash","tool_input":{"command":"%s"}}' "$2" \
    | env PATH="$P" DEV_STACK_BIN="$DEV" bash "$WT_HOOK" > "$T/out" 2>"$T/err" || rc=$?
  [ "$rc" = 0 ] || { bad "$1" "the hook exited $rc (a removal must never be denied)"; return; }
  if has "dev-lease down wtstack" && has "dev-lease release wtstack --all"; then
    pass "$1" "the worktree's stack was removed, then the command was allowed"
  else
    bad "$1" "calls=[$(calls)]"
  fi
}
wtcase 28 "git worktree remove $T/wtstack"
wtcase 29 "git -C $T/other worktree remove --force $T/wtstack"
wtcase 30 "git-worktree remove $T/wtstack"
run; printf '{"tool_name":"Bash","tool_input":{"command":"ls -la"}}' | env PATH="$P" DEV_STACK_BIN="$DEV" bash "$WT_HOOK" >/dev/null 2>&1; rc=$?
if [ "$rc" = 0 ] && [ ! -s "$DLOG" ]; then pass 31 "an unrelated command records nothing"; else bad 31 "rc=$rc calls=[$(calls)]"; fi
run; printf '{"tool_name":"Bash","tool_input":{"command":"git worktree remove %s"}}' "$T/plain" | env PATH="$P" DEV_STACK_BIN="$DEV" bash "$WT_HOOK" >/dev/null 2>&1; rc=$?
if [ "$rc" = 0 ] && [ ! -s "$DLOG" ]; then pass 31b "a path with no stack records nothing"; else bad 31b "rc=$rc calls=[$(calls)]"; fi
# The hook finds the helper next to itself, without DEV_STACK_BIN.
run; printf '{"tool_name":"Bash","tool_input":{"command":"git worktree remove %s"}}' "$T/wtstack" | env PATH="$P" bash "$WT_HOOK" >/dev/null 2>&1; rc=$?
if [ "$rc" = 0 ] && has "dev-lease down wtstack"; then pass 31c "the hook finds the helper next to itself"; else bad 31c "rc=$rc calls=[$(calls)]"; fi

# --- 32-34: the extend and release hooks ---
run; printf '{"session_id":"abcdefgh1234","cwd":"%s"}' "$T/wtstack" | env PATH="$P" DEV_STACK_BIN="$DEV" bash "$EXTEND_HOOK" > "$T/out" 2>"$T/err"; rc=$?
if [ "$rc" = 0 ] && [ ! -s "$T/out" ] && [ ! -s "$T/err" ] && [ "$(calls | tail -1)" = "docker exec srv123 dev-lease extend wtstack 1h --holder claude-abcdefgh" ]; then
  pass 32 "holder claude-<sid8>, ttl 1h, silent on success"
else bad 32 "rc=$rc out=[$(cat "$T/out")] err=[$(cat "$T/err")] calls=[$(calls)]"; fi
run; printf '{"session_id":"abcdefgh1234","cwd":"%s"}' "$T/plain" | env PATH="$P" DEV_STACK_BIN="$DEV" bash "$EXTEND_HOOK" >/dev/null 2>&1; rc=$?
if [ "$rc" = 0 ] && [ ! -s "$DLOG" ]; then pass 33 "a non-stack cwd -> no call"; else bad 33 "rc=$rc calls=[$(calls)]"; fi
run; printf '{"session_id":"abcdefgh1234","cwd":"%s"}' "$T/plain" | env PATH="$P" DEV_STACK_BIN="$DEV" CLAUDE_PROJECT_DIR="$T/wtstack" bash "$EXTEND_HOOK" >/dev/null 2>&1; rc=$?
if [ "$rc" = 0 ] && has "dev-lease extend wtstack 1h --holder claude-abcdefgh"; then pass 34 "CLAUDE_PROJECT_DIR beats the payload cwd"; else bad 34 "calls=[$(calls)]"; fi
run; printf '{"session_id":"abcdefgh1234","cwd":"%s"}' "$T/wtstack" | env PATH="$P" DEV_STACK_BIN="$DEV" bash "$RELEASE_HOOK" >/dev/null 2>&1; rc=$?
if [ "$rc" = 0 ] && [ "$(nlines)" = 2 ] && [ "$(calls | tail -1)" = "docker exec srv123 dev-lease release wtstack --holder claude-abcdefgh" ] \
   && ! calls | grep -Eq "down|stop|rm"; then
  pass 34b "exactly dev-lease release <name> --holder claude-<sid8>, nothing stops or is removed"
else bad 34b "rc=$rc calls=[$(calls)]"; fi

# --- 35-39: ownership - a stack belongs to the worktree that owns it ---
# A scratch repository with a real linked worktree: the main worktree holds
# the compose file `name: app`, the linked worktree is a checkout of the same
# file, so it resolves to the same project name - the main worktree's stack.
GITREPO=$T/gitrepo
GITFEAT=$T/gitrepo-feat
mkdir -p "$GITREPO"
# The initial branch is pinned and every branch worktree names its start
# point: the fixture must not depend on the user's init.defaultBranch.
git -C "$GITREPO" init -q -b main
printf 'name: app\nservices:\n  web:\n    image: busybox\n' > "$GITREPO/compose.yml"
git -C "$GITREPO" add -A
# git commit starts auto maintenance in the background (--detach); it can
# still write into .git while the cleanup deletes the directory.
git -C "$GITREPO" config maintenance.auto false
git -C "$GITREPO" config gc.auto 0
git -C "$GITREPO" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false -c core.hooksPath=/dev/null commit -q -m "init"
git -C "$GITREPO" -c core.hooksPath=/dev/null worktree add -q -b feat "$GITFEAT" main

run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down "$GITFEAT" 2>"$T/err"); rc=$?
if [ "$rc" = 3 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && grep -q "main worktree" "$T/err" \
   && grep -q "COMPOSE_PROJECT_NAME=" "$T/err" && [ ! -s "$DLOG" ]; then
  pass 35 "a linked worktree on the main worktree's stack -> exit 3, one line, no docker call"
else bad 35 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi
printf 'COMPOSE_PROJECT_NAME=app-feat\n' > "$GITFEAT/.env"
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down "$GITFEAT" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "dev-lease down app-feat" && has "dev-lease release app-feat --all"; then
  pass 36 "the same worktree with its own COMPOSE_PROJECT_NAME -> its own stack goes down"
else bad 36 "rc=$rc calls=[$(calls)] err=[$(cat "$T/err")]"; fi
rm -f "$GITFEAT/.env"
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down "$GITREPO" 2>"$T/err"); rc=$?
if [ "$rc" = 3 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && grep -q "main worktree" "$T/err" && [ ! -s "$DLOG" ]; then
  pass 37 "the main worktree itself -> exit 3, no docker call"
else bad 37 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down "$GITREPO" --main 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "dev-lease down app"; then
  pass 38 "the main worktree with --main -> the stack goes down"
else bad 38 "rc=$rc calls=[$(calls)] err=[$(cat "$T/err")]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down -p app --repo "$GITREPO" 2>"$T/err"); rc=$?
if [ "$rc" = 3 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && [ ! -s "$DLOG" ]; then
  pass 39 "down -p <the main worktree's project> --repo <repo> -> exit 3, no docker call"
else bad 39 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down -p app-feat --repo "$GITREPO" 2>"$T/err"); rc=$?
if [ "$rc" = 0 ] && has "dev-lease down app-feat" && has "dev-lease release app-feat --all"; then
  pass 39b "down -p <another project> --repo <repo> -> it goes down"
else bad 39b "rc=$rc calls=[$(calls)] err=[$(cat "$T/err")]"; fi
run; printf '{"tool_name":"Bash","tool_input":{"command":"git worktree remove %s"}}' "$GITFEAT" \
  | env PATH="$P" DEV_STACK_BIN="$DEV" bash "$WT_HOOK" > "$T/out" 2>"$T/err"; rc=$?
if [ "$rc" = 0 ] && [ ! -s "$DLOG" ] && grep -q "main worktree" "$T/err"; then
  pass 39c "the worktree-removal hook on the shared stack: no removal call, the removal allowed, the refusal on stderr"
else bad 39c "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi

# The bare layout: a bare repository plus worktree checkouts. There the bare
# repository is the first entry git prints, and the main worktree is the one
# on the main branch - the guard has to find it all the same.
BARER=$T/bare
mkdir -p "$BARER/.bare"
# The bare repository starts on main too, so the branch the main worktree
# checks out is the branch the bare repository points at.
git -C "$BARER/.bare" init -q --bare -b main
git -C "$BARER/.bare" config maintenance.auto false
git -C "$BARER/.bare" config gc.auto 0
printf 'gitdir: ./.bare\n' > "$BARER/.git"
git -C "$BARER/.bare" -c core.hooksPath=/dev/null worktree add -q -b main "$BARER/main"
printf 'name: bareapp\nservices:\n  web:\n    image: busybox\n' > "$BARER/main/compose.yml"
git -C "$BARER/main" add -A
git -C "$BARER/main" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false -c core.hooksPath=/dev/null commit -q -m "init"
git -C "$BARER/.bare" -c core.hooksPath=/dev/null worktree add -q -b feat "$BARER/feat" main
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down "$BARER/feat" 2>"$T/err"); rc=$?
if [ "$rc" = 3 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && grep -q "main worktree" "$T/err" && [ ! -s "$DLOG" ]; then
  pass 40 "the bare layout: a branch worktree on the main worktree's stack -> exit 3, no docker call"
else bad 40 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi
run; out=$(env -u COMPOSE_PROJECT_NAME PATH="$P" "$DEV" down "$BARER/main" 2>"$T/err"); rc=$?
if [ "$rc" = 3 ] && [ "$(grep -c . "$T/err" || true)" = 1 ] && grep -q "main worktree" "$T/err" && [ ! -s "$DLOG" ]; then
  pass 41 "the bare layout: the main worktree itself -> exit 3, no docker call"
else bad 41 "rc=$rc err=[$(cat "$T/err")] calls=[$(calls)]"; fi

exit $fail
