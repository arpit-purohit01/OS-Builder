#!/bin/bash
# Starts a complete local Foundry stack (development mode):
#   api :8080, scheduler, worker-agent (build,test,finalize pools), signer :7443
# State lives in $DEV (default out/dev). Stop with scripts/dev-down.sh.
# Usage: scripts/dev-up.sh [service...]   (default: all; e.g. "worker" restarts
# only the worker after scripts/dev-down.sh worker or a crash).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
DEV=${DEV:-$root/out/dev}
mkdir -p "$DEV"/{blobs,keys,work,logs}
DEV=$(cd "$DEV" && pwd)
cd "$root"
make build components >/dev/null
db=${FOUNDRY_DB:-postgres://postgres@127.0.0.1:5432/foundry?sslmode=disable}
psql "${db/\/foundry?/\/postgres?}" -tAc "SELECT 1 FROM pg_database WHERE datname='foundry'" | grep -q 1 ||
  psql "${db/\/foundry?/\/postgres?}" -c "CREATE DATABASE foundry" >/dev/null
[[ -f $DEV/signer.token ]] || head -c 24 /dev/urandom | base64 | tr -d '/+=' > "$DEV/signer.token"
[[ -f $DEV/preview.token ]] || head -c 24 /dev/urandom | base64 | tr -d '/+=' > "$DEV/preview.token"
export FOUNDRY_PREVIEW_TOKEN; FOUNDRY_PREVIEW_TOKEN=$(cat "$DEV/preview.token")
export FOUNDRY_PREVIEW_URL=http://127.0.0.1:7444
export FOUNDRY_DB=$db FOUNDRY_BLOBS=$DEV/blobs FOUNDRY_DEV=1 FOUNDRY_LOG_FORMAT=text
export FOUNDRY_SIGNER_TOKEN; FOUNDRY_SIGNER_TOKEN=$(cat "$DEV/signer.token")
export FOUNDRY_CATALOG=$root/catalog FOUNDRY_BASE_URL=${FOUNDRY_BASE_URL:-http://127.0.0.1:8080}
[[ -d $root/web/dist ]] && export FOUNDRY_WEB=$root/web/dist
want=" ${*:-api previewd signer scheduler worker} "
start() { # name cmd...
  local name=$1; shift
  [[ $want == *" $name "* ]] || return 0
  nohup "$@" > "$DEV/logs/$name.log" 2>&1 &
  echo $! > "$DEV/$name.pid"
  echo "started $name (pid $!)"
}
start api bin/api
[[ $want == *" api "* ]] && sleep 2
FOUNDRY_PREVIEW_DIR=$DEV/preview start previewd bin/previewd
start signer bin/signer serve -keys "$DEV/keys" -blobs "$DEV/blobs" -listen 127.0.0.1:7443
start scheduler bin/scheduler
FOUNDRY_WORK=$DEV/work FOUNDRY_COMPONENTS=$root/bin/components FOUNDRY_SIGNER_URL=http://127.0.0.1:7443 \
FOUNDRY_TOOLS_TREE=${FOUNDRY_TOOLS_TREE:-/var/lib/foundry/tools-tree} FOUNDRY_MKOSI_CACHE=${FOUNDRY_MKOSI_CACHE:-/var/lib/foundry/builds/cache} \
FOUNDRY_ZSTD_LEVEL=${FOUNDRY_ZSTD_LEVEL:-3} FOUNDRY_VERIFY_SECURE_BOOT=${FOUNDRY_VERIFY_SECURE_BOOT:-0} \
  start worker bin/worker-agent
echo "Foundry dev stack: $FOUNDRY_BASE_URL (logs in $DEV/logs)"
