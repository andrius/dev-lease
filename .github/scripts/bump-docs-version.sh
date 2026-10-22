#!/usr/bin/env bash
# Points the README and the example compose files at a released image:
# every ghcr.io/andrius/dev-lease:vX.Y.Z (or vX.Y.Z-pre) reference in them
# becomes the given tag; nothing else changes. Prints the files it changed.
# Run from the repository root of a git checkout. Usage:
#   bump-docs-version.sh vX.Y.Z
set -euo pipefail
tag=${1:?usage: bump-docs-version.sh vX.Y.Z}
# Only a stable release tag: the docs never point at a pre-release, and
# the tag goes into the sed expression below.
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "tag $tag is not a release tag vX.Y.Z" >&2
  exit 1
fi
files=(README.md examples/*/compose.yml)
for f in "${files[@]}"; do
  sed -E -i "s#(ghcr\.io/andrius/dev-lease:)v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?#\1$tag#g" "$f"
done
git diff --name-only -- "${files[@]}"
