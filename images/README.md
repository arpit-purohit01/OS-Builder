# images/

Image definitions are not hand-maintained trees: `internal/compiler` turns an
OSSpec into a complete mkosi project (pure function, golden-tested). The
reference trees for the bundled specs live in `test/golden/out/<spec>/` —
e.g. `test/golden/out/vm-server/` is the Phase 0 boot-spike image (UEFI,
UKI, systemd-boot, erofs `/usr` + dm-verity with `usrhash=`, A/B slots,
TPM2-encrypted root created on first boot, boot counting + health check).

Build one locally: `make image SPEC=test/golden/specs/vm-server.yaml`.
