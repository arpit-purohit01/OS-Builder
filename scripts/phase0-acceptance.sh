#!/bin/bash
# Phase 0 acceptance (docs/plan/phase-0-foundations.md):
#   1. image builds and boots (UEFI and Secure Boot, TPM2, verity /usr)
#   2. Secure Boot refuses an unsigned UKI
#   3. dm-verity detects a flipped byte in /usr
#   4. TPM2-bound root unlocks on an unchanged boot, not with a foreign PCR key,
#      and never from the running system
#   5. A/B update with boot counting falls back to the previous version
#
# usage: scripts/phase0-acceptance.sh [OUT_DIR] [-- tester flags]
# env:   FOUNDRY_TOOLS_TREE, FOUNDRY_DIRECT_MIRRORS, FOUNDRY_MKOSI_CACHE (see README), KEYS (test keys dir)
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/out/phase0}
shift || true
[[ ${1:-} == -- ]] && shift
mkdir -p "$out"
out=$(cd "$out" && pwd)   # absolute: symlinks below must not be relative
keys=${KEYS:-$out/keys}
export FOUNDRY_TOOLS_TREE=${FOUNDRY_TOOLS_TREE:-/var/lib/foundry/tools-tree}
export FOUNDRY_DIRECT_MIRRORS=${FOUNDRY_DIRECT_MIRRORS:-1}
cache=${FOUNDRY_MKOSI_CACHE:-/var/lib/foundry/builds/cache}   # shared mkosi package/incremental cache
stem=ci-server_1.0.0
v2stem=ci-server_1.0.1
mkdir -p "$out"
cd "$root"
make build components >/dev/null

build() { # spec id
  local res="$out/builds/$2/out/$3.raw"
  if [[ -e $res ]]; then echo "phase0: reusing $2"; return; fi
  echo "phase0: building $2 from $1"
  bin/fctl build "$1" -w "$out/builds" -cache "$cache" -id "$2" > "$out/build-$2.log" 2>&1 || { tail -30 "$out/build-$2.log"; exit 1; }
  # keep only what signing and the tests use (disk, UKI, root hash, manifest)
  rm -f "$out/builds/$2/out/"*.esp.raw "$out/builds/$2/out/"*usr*.raw "$out/builds/$2/out/"*.initrd "$out/builds/$2/out/"*.vmlinuz "$out/builds/$2/out/"initrd*
}
build test/golden/specs/vm-server.yaml v1 "$stem"
build test/boot/specs/vm-server-v2.yaml v2 "$v2stem"

mkdir -p "$out/v1"
ln -sf "$out/builds/v1/out/$stem.raw" "$out/v1/$stem.raw"
ln -sf "$out/builds/v1/out/$stem.efi" "$out/v1/$stem.efi"

sign() { # tenant build-dir stem out-dir
  if [[ -e $4/SHA256SUMS ]]; then echo "phase0: reusing $4"; return; fi
  echo "phase0: signing $3 for tenant $1"
  mkdir -p "$4"
  bin/signer sign-image -keys "$keys" -tenant "$1" -disk "$2/$3.raw" -uki "$2/$3.efi" \
    -roothash "@$2/$3.roothash" -out "$4" > "$4/sign.json" 2> "$4/sign.log" || { cat "$4/sign.log"; exit 1; }
}
sign dev "$out/builds/v1/out" "$stem" "$out/v1-signed"
sign dev "$out/builds/v2/out" "$v2stem" "$out/v2-signed"
rm -f "$out/v2-signed/$v2stem.signed.raw"       # only the update payload is needed
sign foreign "$out/builds/v1/out" "$stem" "$out/foreign"
rm -rf "$out/foreign/$stem.signed.raw" "$out/foreign/update"

echo "phase0: running boot acceptance tests (TCG without /dev/kvm is slow: ~1 h)"
exec bin/tester acceptance -dir "$out" -stem "$stem" -v2-version 1.0.1 "$@"
