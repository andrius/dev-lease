#!/usr/bin/env bash
# Tests bump-docs-version.sh on a scratch copy of the committed tree: a
# release tag rewrites exactly the image references (the same count, only
# those lines, the release checklist untouched), a second run changes
# nothing, and an invalid tag is refused without touching a file.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
# BUMP lets a mutation test point the checks at a changed copy.
bump="${BUMP:-$root/.github/scripts/bump-docs-version.sh}"
tmp="$(mktemp -d)"
trap 'rm -rf -- "${tmp:?}"' EXIT
git -C "$root" archive HEAD | tar -x -C "$tmp"
cd "$tmp"
# Another version in the same file, not an image reference: it must stay.
printf '\nNot an image: some-tool v2.38.2 and dev-lease v3.2.1.\n' >> README.md
git init -q
git add -A
git -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false commit -qm base
fail=0
check() { # description, then a command that must succeed
  local what=$1
  shift
  if "$@"; then echo "ok   $what"; else echo "FAIL $what"; fail=1; fi
}
count() { grep -rhoE "ghcr\\.io/andrius/dev-lease:$1" README.md examples/*/compose.yml | wc -l; }
# shellcheck disable=SC2329 # called through check
refused() { ! "$bump" "$1" >/dev/null 2>&1; }
other_lines() { git diff -U0 | grep -E '^[-+][^-+]' | grep -vc 'ghcr\.io/andrius/dev-lease:' || true; }

before="$(count 'v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?')"
check "the docs name the image ($before references)" test "$before" -gt 0
changed="$("$bump" v9.8.7 | LC_ALL=C sort | tr '\n' ' ')"
check "it changed the README and both example compose files ($changed)" \
  test "$changed" = "README.md examples/basic/compose.yml examples/on-demand/compose.yml "
check "every reference now names v9.8.7" test "$(count 'v9\.8\.7')" = "$before"
check "no other line changed" test "$(other_lines)" = 0
check "an unrelated version stays" grep -q 'some-tool v2.38.2 and dev-lease v3.2.1' README.md
check "the release checklist is untouched" git diff --quiet -- docs/release-checklist.md
git -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false commit -qam bumped
again="$("$bump" v9.8.7)"
check "a second run reports no file" test -z "$again"
check "a second run changes nothing" git diff --quiet
for t in v9.8 9.8.7 v9.8.7-rc1 'v9.8.7#x' ''; do
  check "tag '$t' is refused" refused "$t"
done
check "the refused tags touched no file" git diff --quiet
exit "$fail"
