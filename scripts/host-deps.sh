#!/bin/bash
# Installs the host tools the builder, signer and tester need (Ubuntu 24.04).
# mkosi itself is pinned separately (see deploy/versions.lock).
set -euo pipefail
pkgs=(
  qemu-system-x86 qemu-utils ovmf swtpm swtpm-tools   # tester
  systemd-boot-efi systemd-ukify sbsigntool efitools openssl  # signer
  libtss2-esys-3.0.2-0t64 libtss2-mu-4.0.1-0t64 libtss2-rc0t64  # systemd-measure (PCR policies)
  mtools dosfstools cryptsetup-bin e2fsprogs fdisk zstd xz-utils  # finalize/convert
  postgresql-16 shellcheck python3 git
)
sudo apt-get update
sudo apt-get install -y --no-install-recommends "${pkgs[@]}"
