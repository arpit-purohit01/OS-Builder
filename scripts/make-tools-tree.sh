#!/bin/bash
# Build the pinned mkosi tools tree once (Ubuntu 26.04 userspace: systemd 259,
# repart/ukify/sbsign...). Builds then use ToolsTree=<dest> instead of
# rebuilding it per build. Usage: scripts/make-tools-tree.sh [/var/lib/foundry/tools-tree]
set -euo pipefail
dest=${1:-/var/lib/foundry/tools-tree}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat > "$work/mkosi.conf" <<'CONF'
[Distribution]
Distribution=ubuntu
Release=noble
[Output]
Format=none
[Build]
ToolsTree=default
ToolsTreeDistribution=ubuntu
ToolsTreeRelease=resolute
ToolsTreeMirror=http://archive.ubuntu.com/ubuntu
CONF
(cd "$work" && env -u https_proxy -u HTTPS_PROXY mkosi -f build)
rm -rf "$dest"
cp -a "$work/mkosi.tools" "$dest"
echo "tools tree at $dest"
