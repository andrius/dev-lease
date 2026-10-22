#!/usr/bin/env bash
# The release's docs step: on the current origin/main, point the README and
# the example compose files at REF and push one commit to main. Nothing is
# committed when the docs already name REF. When main moves between the
# fetch and the push, the push is refused and the bump is redone on the new
# main, up to three times. Prints "pushed" when it pushed a commit.
# Environment: REF (vX.Y.Z). Run from the repository root of a clone whose
# origin accepts the push.
set -euo pipefail
: "${REF:?}"
here="$(cd "$(dirname "$0")" && pwd)"
for attempt in 1 2 3; do
  git fetch -q origin main
  git checkout -q -B main origin/main
  "$here/bump-docs-version.sh" "$REF" >/dev/null
  if git diff --quiet; then
    echo "the docs already name $REF"
    exit 0
  fi
  git commit -qam "docs: point the README and the examples at $REF"
  if git push -q origin HEAD:main; then
    echo pushed
    exit 0
  fi
  echo "push attempt $attempt refused (main moved?); redoing the bump on the new main" >&2
  sleep 2
done
echo "could not push the docs bump to main" >&2
exit 1
