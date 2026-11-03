---
name: mkosi-image-compiler
description: Use when working on internal/compiler, images/base or images/profiles — generating mkosi configuration, repart definitions, extra trees, postinst scripts, kernel/package selection, or debugging why an image build produced the wrong content.
---

# mkosi image compiler

Read first: `docs/context/build-pipeline.md` (§2, §3, §6), `docs/context/boot-firmware-and-disk.md` (§3, §4), `deploy/versions.lock` (pinned mkosi/systemd versions).

## Ground rules
- The compiler is a **pure function**: `Compile(spec, catalog, base) (Tree, error)`. No network, filesystem, clock, randomness or environment reads. All maps iterated in sorted order. Output is byte-identical for identical input.
- Before using any mkosi or systemd-repart option, confirm it exists **in the pinned version** (`man mkosi` / `man repart.d` inside the builder golden image, or upstream docs at that tag). Option names and sections have changed between releases.
- Profiles in `images/profiles/*` are static, reviewed config; per-customer content only comes through the compiler.

## Workflow for a change
1. Write or update a golden spec in `test/golden/specs/` that exercises the change.
2. Implement in `internal/compiler/`; keep one file per spec section.
3. `make golden` → inspect diff → `make golden-update`.
4. Build it for real: `make image SPEC=test/golden/specs/<x>.yaml` (KVM required) and then `make boot-test IMAGE=out/<id>/disk.raw MATRIX=uefi,sb,tpm`.
5. Inspect the image without booting: `systemd-dissect out/<id>/disk.raw` (partitions, os-release), `systemd-dissect --mount` read-only to check files, `ukify inspect` on the UKI (sections, cmdline contains `usrhash=`).
6. Check reproducibility locally: build twice with `--force`, compare `sha256sum out/*/usr*.raw`.

## Generated postinst scripts
- Header: `#!/bin/bash` + `set -euo pipefail` + `export LC_ALL=C.UTF-8 TZ=UTC`.
- Idempotent (safe to re-run), no network, no `date`/`$RANDOM`, no interactive tools.
- User-provided values never interpolated into shell source; write them to `/usr/lib/foundry/values.json` and read with `jq -r` inside a fixed script.
- Typical steps: `dconf update`, `plymouth-set-default-theme foundry`, `systemctl preset-all` (presets file generated), `update-mime-database`, `fc-cache -s`, locale generation.

## Common pitfalls
| Symptom | Cause | Fix |
|---|---|---|
| usr hash differs between builds | timestamps, unsorted file lists, `__pycache__`, `ldconfig` cache, random machine-id | SOURCE_DATE_EPOCH, mkosi `Seed=`, remove/regenerate caches deterministically, `/etc/machine-id` = `uninitialized` |
| Boot hangs at "Waiting for device usr" | `usrhash=` missing/mismatch or verity partition label/type wrong | check UKI cmdline vs `veritysetup` root hash in manifest, repart `VerityMatchKey` |
| `/etc` lacks config on first boot | file put in `/etc` but `/etc` isn't in usr image | ship under `/usr/share/factory/etc` + tmpfiles `C` lines, or confext |
| Firmware missing on real hardware | `non-free-firmware` not enabled or initrd lacks firmware | enable component; include only storage/GPU firmware needed early in initrd |
| Image too large | docs/man pages/locales | `RemoveFiles=`, `dpkg` path-exclude for `/usr/share/doc/*` (keep copyright files for GPL), locale purge |

## Never
- Never write a key or certificate private part into the tree.
- Never enable `SecureBoot=yes` signing in the build VM — signing happens in the finalize stage.
- Never fetch from mirrors not in the egress allowlist; add mirrors via catalog + proxy allowlist PR.
