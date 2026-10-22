#!/usr/bin/env bash
# Image metadata for a release: tags, labels and annotations, written one
# per line to $OUT_DIR/tags, $OUT_DIR/labels and $OUT_DIR/annotations, and
# $OUT_DIR/latest ("true" when :latest moves to this tag; the release's
# docs job follows the same rule).
#
# The static values (title, description, url, source, documentation,
# licenses) are read from the Dockerfile's LABEL, so the image config, the
# platform manifests and the index carry the same text. version, revision
# and created come from the release. GitHub's package page reads the
# description of a multi-arch image from the index annotation.
#
# Environment: REF (the tag, vX.Y.Z or vX.Y.Z-pre), SHA (the full commit
# id), OUT_DIR (an existing directory). Run from the repository root.
set -euo pipefail
: "${REF:?}" "${SHA:?}" "${OUT_DIR:?}"

if ! printf '%s\n' "$REF" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "ref name $REF does not match ^v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?\$" >&2
  exit 1
fi
if ! printf '%s\n' "$SHA" | grep -Eq '^[0-9a-f]{40}$'; then
  echo "SHA $SHA is not a full commit id" >&2
  exit 1
fi

# :latest moves only when the tag is the highest non-pre-release tag.
tags="ghcr.io/andrius/dev-lease:$REF"
highest="$(git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1 || true)"
latest=false
if [ -n "$highest" ] && [ "$highest" = "$REF" ]; then
  tags="$tags,ghcr.io/andrius/dev-lease:latest"
  latest=true
else
  echo "tag $REF is not the highest non-pre-release tag (${highest:-none}); :latest stays" >&2
fi
printf '%s\n' "$tags" > "$OUT_DIR/tags"
printf '%s\n' "$latest" > "$OUT_DIR/latest"

# key="value" pairs of the Dockerfile's LABEL lines.
static="$(sed -n 's/^.*\(org\.opencontainers\.image\.[a-z]*\)="\([^"]*\)".*$/\1=\2/p' Dockerfile)"
for k in title description url source documentation licenses; do
  if ! printf '%s\n' "$static" | grep -q "^org\.opencontainers\.image\.$k=."; then
    echo "the Dockerfile has no org.opencontainers.image.$k label" >&2
    exit 1
  fi
done
# The commit time in UTC, so a rebuild of the same tag gives the same value.
created="$(TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd "$SHA")"
dynamic="org.opencontainers.image.version=$REF
org.opencontainers.image.revision=$SHA
org.opencontainers.image.created=$created"
# buildx passes annotations to the exporter as comma-separated attributes,
# so a value must not contain a comma (or a quote).
if printf '%s\n%s\n' "$static" "$dynamic" | cut -d= -f2- | grep -q '[,"]'; then
  echo "an image label value contains a comma or a quote" >&2
  exit 1
fi

printf '%s\n' "$dynamic" > "$OUT_DIR/labels"
: > "$OUT_DIR/annotations"
for level in index manifest; do
  printf '%s\n%s\n' "$static" "$dynamic" | sed "s/^/$level:/" >> "$OUT_DIR/annotations"
done
