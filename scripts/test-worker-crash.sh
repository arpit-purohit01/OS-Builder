#!/bin/bash
# Phase 1 acceptance 5: SIGKILL the worker mid-build. The scheduler re-leases
# the build after its lease expires, the restarted worker reaps the orphaned
# mkosi process group, and the build completes. Needs the dev stack
# (scripts/dev-up.sh) and an fctl login (XDG_CONFIG_HOME as used for it).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
DEV=${DEV:-$root/out/dev}
cd "$root"
spec=${1:-test/golden/specs/vm-server.yaml}
project=${2:?usage: test-worker-crash.sh [SPEC] PROJECT_ID}
fail() { echo "FAIL: $*" >&2; exit 1; }

out=$(bin/fctl submit "$spec" -project "$project" -formats qcow2)
build=$(grep -o 'bld_[0-9A-Z]*' <<<"$out" | head -1)
[[ -n $build ]] || fail "no build id in: $out"
echo "build $build submitted"

# Wait until mkosi is really working (package installation started).
for _ in $(seq 1 120); do
  if bin/fctl logs "$build" 2>/dev/null | grep -q 'Installing\|Unpacking\|erofs'; then break; fi
  sleep 5
done
mk=$(pgrep -f "mkosi --directory .*$build" | head -1 || true)
[[ -n $mk ]] || fail "mkosi for $build is not running"
pgid=$(ps -o pgid= -p "$mk" | tr -d ' ')
echo "mkosi running (pid $mk, process group $pgid); killing the worker with SIGKILL"
kill -9 "$(cat "$DEV/worker.pid")"
sleep 3
kill -0 -- "-$pgid" 2>/dev/null && echo "orphaned mkosi group $pgid still alive (expected before the reaper)"

scripts/dev-up.sh worker
sleep 5
if kill -0 -- "-$pgid" 2>/dev/null; then fail "orphaned process group $pgid survived the restarted worker"; fi
grep -q 'reaped orphan' "$DEV/logs/worker.log" || fail "worker did not report reaping"
echo "orphans reaped; waiting for the lease to expire and the build to finish"

deadline=$((SECONDS + 3600))
while (( SECONDS < deadline )); do
  st=$(bin/fctl status "$build" | jq -r .build.state)
  case $st in
    published) break ;;
    failed|cancelled|rejected) bin/fctl status "$build"; fail "build ended in $st" ;;
  esac
  sleep 20
done
[[ $st == published ]] || fail "timed out (state $st)"
psql "${FOUNDRY_DB:-postgres://postgres@127.0.0.1:5432/foundry?sslmode=disable}" -tAc \
  "SELECT count(*) FROM build_events WHERE build_id='$build' AND detail->>'reason'='lease expired'" | grep -q '^[1-9]' \
  || fail "no lease-expired event recorded"
leftover=$(pgrep -f "mkosi --directory" || true)
[[ -z $leftover ]] || fail "mkosi processes left after completion: $leftover"
echo "PASS: $build re-leased after the worker crash, completed, no orphans"
