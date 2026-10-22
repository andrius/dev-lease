#!/usr/bin/env bash
# Checks a multi-arch image exported as an OCI layout tar: the index and
# every platform manifest carry the description, version and source
# annotations, and every image config carries the version label, all equal
# to what image-meta.sh produced. Usage: check-image-meta.sh <oci.tar> <out_dir>
set -euo pipefail
tarball=$1 out=$2
dir="$(mktemp -d)"
trap 'rm -rf -- "${dir:?}"' EXIT
tar -xf "$tarball" -C "$dir"
blob() { cat "$dir/blobs/sha256/${1#sha256:}"; }
want() { sed -n "s/^index:org\.opencontainers\.image\.$1=//p" "$out/annotations"; }

fail=0
check() { # what, got, want
  if [ "$2" != "$3" ]; then echo "FAIL $1: got '$2', want '$3'" >&2; fail=1; else echo "ok   $1"; fi
}
idx="$(jq -r '.manifests[0].digest' "$dir/index.json")"
for k in description version source; do
  check "index $k" "$(blob "$idx" | jq -r --arg k "org.opencontainers.image.$k" '.annotations[$k] // ""')" "$(want "$k")"
done
platforms=0
for m in $(blob "$idx" | jq -r '.manifests[] | select(.platform.os != "unknown") | .digest'); do
  platforms=$((platforms + 1))
  man="$(blob "$m")"
  arch="$(blob "$idx" | jq -r --arg d "$m" '.manifests[] | select(.digest == $d) | .platform.architecture')"
  for k in description version; do
    check "$arch manifest $k" "$(printf '%s' "$man" | jq -r --arg k "org.opencontainers.image.$k" '.annotations[$k] // ""')" "$(want "$k")"
  done
  cfg="$(printf '%s' "$man" | jq -r '.config.digest')"
  check "$arch config label version" "$(blob "$cfg" | jq -r '.config.Labels["org.opencontainers.image.version"] // ""')" "$(want version)"
  check "$arch config label description" "$(blob "$cfg" | jq -r '.config.Labels["org.opencontainers.image.description"] // ""')" "$(want description)"
done
check "platform count" "$platforms" 2
exit "$fail"
