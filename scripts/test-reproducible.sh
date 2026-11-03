#!/bin/bash
# Phase 1 acceptance 4 / P1-19: two builds of the same spec produce the same
# /usr partition (identical dm-verity root hash). On a mismatch the two file
# manifests are diffed (and diffoscope is run on the usr images if installed).
# Needs the dev stack (scripts/dev-up.sh) and an fctl login.
# Usage: test-reproducible.sh PROJECT_ID [SPEC...]   (default: vm-server, vm-kiosk)
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
project=${1:?usage: test-reproducible.sh PROJECT_ID [SPEC...]}
shift
specs=("$@")
[[ ${#specs[@]} -gt 0 ]] || specs=(test/golden/specs/vm-server.yaml test/golden/specs/vm-kiosk.yaml)
db=${FOUNDRY_DB:-postgres://postgres@127.0.0.1:5432/foundry?sslmode=disable}
q() { psql "$db" -tAc "$1"; }
fail=0

wait_built() { # build -> waits until the usr root hash is recorded
  local b=$1 deadline=$((SECONDS + 5400))
  while ((SECONDS < deadline)); do
    local h st
    h=$(q "SELECT encode(usr_roothash,'hex') FROM builds WHERE id='$b'")
    [[ -n $h ]] && { echo "$h"; return 0; }
    st=$(bin/fctl status "$b" | jq -r .build.state)
    case $st in failed | cancelled | rejected) echo "build $b ended in $st" >&2; return 1 ;; esac
    sleep 15
  done
  echo "timed out waiting for $b" >&2
  return 1
}

for spec in "${specs[@]}"; do
  echo "== $spec"
  ids=()
  for i in 1 2; do
    out=$(bin/fctl submit "$spec" -project "$project")
    id=$(grep -o 'bld_[0-9A-Z]*' <<<"$out" | head -1)
    [[ -n $id ]] || { echo "no build id in: $out" >&2; exit 1; }
    ids+=("$id")
    echo "  build $i: $id"
  done
  h1=$(wait_built "${ids[0]}") || { fail=1; continue; }
  h2=$(wait_built "${ids[1]}") || { fail=1; continue; }
  echo "  usr root hash 1: $h1"
  echo "  usr root hash 2: $h2"
  if [[ $h1 == "$h2" ]]; then
    echo "  REPRODUCIBLE"
    continue
  fi
  fail=1
  echo "  MISMATCH — file manifest differences:"
  tmp=$(mktemp -d)
  for i in 0 1; do
    bin/fctl download "${ids[$i]}" -kind file-manifest -o "$tmp/$i" >/dev/null 2>&1 || true
    f=$(find "$tmp/$i" -type f 2>/dev/null | head -1)
    if [[ -n $f ]]; then
      zcat -f "$f" | jq -r '[.path,.type,.sha256,.link,.mode]|@tsv' | sort >"$tmp/m$i"
    else
      : >"$tmp/m$i"
    fi
  done
  diff "$tmp/m0" "$tmp/m1" | head -40 || true
  if command -v diffoscope >/dev/null; then
    echo "  (run diffoscope on the usr images for the full report)"
  fi
  rm -rf "$tmp"
done
if ((fail != 0)); then
  echo "FAIL" >&2
  exit 1
fi
echo "PASS: all specs reproduce"
