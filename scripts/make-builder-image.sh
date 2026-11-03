#!/bin/bash
# Builds the golden build-VM image: out/builder/foundry-builder.qcow2
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/out/builder}
mkdir -p "$out" "$root/images/builder/mkosi.extra/usr/bin"
cd "$root"
CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=vendor go build -trimpath -ldflags "-s -w" -o images/builder/mkosi.extra/usr/bin/foundry-builder ./cmd/builder
env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
  mkosi --directory images/builder --tools-tree="${FOUNDRY_TOOLS_TREE:-/var/lib/foundry/tools-tree}" \
  --output-directory "$out" --cache-directory "${FOUNDRY_MKOSI_CACHE:-/var/lib/foundry/builds/cache}" --force build
qemu-img convert -q -f raw -O qcow2 "$out/foundry-builder.raw" "$out/foundry-builder.qcow2"
rm -f "$out/foundry-builder.raw"
echo "builder image: $out/foundry-builder.qcow2"
