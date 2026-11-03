#!/bin/bash
set -uo pipefail
DEV=${DEV:-$(cd "$(dirname "$0")/.." && pwd)/out/dev}
names=("$@"); (( $# )) || names=(worker scheduler signer previewd api)
for n in "${names[@]}"; do
  if [[ -f $DEV/$n.pid ]]; then kill "$(cat "$DEV/$n.pid")" 2>/dev/null && echo "stopped $n"; rm -f "$DEV/$n.pid"; fi
done
