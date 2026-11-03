# Build pipeline

## 1. Overview
`spec_version` → validate → compile (pure) → schedule → **build VM** (untrusted customer content) → scan →
boot test → review → **finalize VM** (trusted, platform code only, signs via signer) → publish.

Splitting *build* (untrusted) from *finalize/sign* (trusted) means customer code never runs anywhere a
signing key is reachable, and signatures are only produced for approved content.

## 2. Compilation (`internal/compiler`)

`Compile(spec OSSpec, cat Catalog, base BaseProfile) (Tree, error)` — pure, deterministic:
- Inputs are values; no network, no clock, no randomness, no map iteration order leaks (always sort keys).
- Output `Tree` is `map[path]File{Mode, Content}` serialized as a tar with fixed mtime (`SOURCE_DATE_EPOCH`), uid/gid 0, sorted entries → `tree_digest`.

Generated layout:
```
mkosi.conf                    # [Distribution] [Output] [Content] [Validation] — see §2.1
mkosi.conf.d/10-profile.conf  # copied from images/profiles/<profile>
mkosi.conf.d/20-kernel.conf   # kernel package + KernelCommandLine
mkosi.conf.d/30-apps.conf     # Packages= from catalog mapping (sorted)
mkosi.repart/00-esp.conf
mkosi.repart/10-usr.conf
mkosi.repart/11-usr-verity.conf
mkosi.repart/12-usr-verity-sig.conf
mkosi.extra/usr/lib/os-release
mkosi.extra/usr/lib/repart.d/{20-usr-b.conf,21-usr-b-verity.conf,22-usr-b-verity-sig.conf,50-root.conf}
mkosi.extra/usr/lib/sysupdate.d/*.transfer
mkosi.extra/usr/lib/sysctl.d/60-foundry.conf
mkosi.extra/usr/lib/systemd/zram-generator.conf
mkosi.extra/usr/lib/foundry/{trust.json,agent.toml,nftables.conf,policy-default.json}
mkosi.extra/usr/share/factory/etc/...   # /etc defaults materialized by tmpfiles
mkosi.postinst.chroot         # generated, idempotent, set -euo pipefail, no network
mkosi.finalize                # strips caches, writes file manifest
```

### 2.1 mkosi.conf skeleton (verify option names against the pinned mkosi version — e.g. `Verity=defer` and `Seed=`)
```ini
[Distribution]
Distribution=debian
Release=trixie
Architecture=x86-64
Repositories=main,non-free-firmware
Mirror=https://snapshot.debian.org/archive/debian/20261020T000000Z/

[Output]
Format=disk
ImageId=aurora
ImageVersion=1.4.0
SplitArtifacts=yes            # emit usr / usr-verity / usr-verity-sig / UKI separately for updates
Seed=<uuid derived from spec_digest>   # deterministic partition UUIDs

[Content]
Bootable=yes
Bootloader=systemd-boot
UnifiedKernelImages=yes
KernelCommandLine=quiet splash rw
Locale=en_US.UTF-8
Keymap=us
Timezone=Asia/Kolkata
SourceDateEpoch=1792454400
RemoveFiles=/var/cache/apt/** /var/lib/apt/lists/**

[Validation]
SecureBoot=no                 # signing happens in finalize stage, not here
Verity=defer                  # write an unsigned 16 KiB usr-verity-sig placeholder; finalize fills it
```

### 2.2 Partition definitions in the built image
```ini
# 00-esp.conf
[Partition]
Type=esp
Format=vfat
SizeMinBytes=1G
SizeMaxBytes=1G
CopyFiles=/efi:/

# 10-usr.conf  (slot A)
[Partition]
Type=usr
Format=erofs
CopyFiles=/usr:/
Verity=data
VerityMatchKey=usr
SplitName=usr
Compression=lz4hc
SizeMinBytes=8G               # fixed slot size: 3G kiosk / 8G desktop (from profile)
SizeMaxBytes=8G
Label=%M_%A                   # ImageId_ImageVersion — sysupdate matches on label

# 11-usr-verity.conf
[Partition]
Type=usr-verity
Verity=hash
VerityMatchKey=usr
SplitName=usr-verity
SizeMinBytes=128M
SizeMaxBytes=128M
Label=%M_%A_verity

# 12-usr-verity-sig.conf
[Partition]
Type=usr-verity-sig
Verity=signature
VerityMatchKey=usr
SplitName=usr-verity-sig
# size is fixed at 16 KiB by systemd-repart; do not set sizes
Label=%M_%A_verity_sig
```
Slot B and root are created on the target at install/first boot from `/usr/lib/repart.d/` (B slots labelled `_empty`, fixed size: kiosk 3 GiB, desktop 8 GiB; root `Encrypt=tpm2`, grows to fill disk, `FactoryReset=yes`).

## 3. Stage DAG

| # | Stage | Inputs | Output | Cache key components | Typical time *(estimate)* |
|---|---|---|---|---|---|
| S1 | resolve | profile packages + catalog packages, snapshot | `packages.lock` (name, version, arch, sha256) | base, snapshot, sorted package request, resolver version | 30–60 s |
| S2 | fetch | `packages.lock` | .deb files in package CAS | per-package sha256 (already content-addressed) | 0–5 min (cold) |
| S3 | rootfs | lock, profile | mkosi incremental base tree (after package install) | H(lock digest, profile digest, mkosi version, tools tree digest) | 3–8 min cold, 0 warm |
| S4 | flatpak | refs + commits | sideload repo | H(sorted ref@commit list) | 1–4 min cold |
| S5 | customize | tree digest (overlays, postinst, config) | final rootfs | H(S3 key, S4 key, tree_digest) | 1–2 min |
| S6 | assemble | final rootfs | usr erofs, verity, unsigned UKI, ESP, disk.raw | H(S5 key, repart defs digest) | 2–4 min |
| S7 | convert | disk.raw | img.xz, raw.zst, qcow2, vmdk, ova, iso | H(S6 key, format) | 1–3 min (parallel) |
| S8 | metadata | rootfs, disk | SBOM (SPDX), file manifest, PCR11 prediction, manifest.json | H(S6 key) | 30–60 s |
| F1 | finalize (trusted VM, post-approval) | S6/S8 outputs | signed systemd-boot, signed UKI with `.pcrsig`, usr-verity-sig, re-assembled ESP, signed SHA256SUMS | H(S6 key, tenant key ids) | 1–2 min |

Execution: topological order (Kahn), independent stages in parallel (S4 ∥ S3; S7 formats in parallel), critical-path-first when CPU-bound. A stage is skipped when its output key exists in L1/L2 cache (`caching.md`).

## 4. Build sandbox (build VM)

| Property | Value |
|---|---|
| Hypervisor | QEMU/KVM (or cloud-hypervisor) on bare-metal hosts, `-machine q35,accel=kvm`, no device passthrough |
| Image | golden builder qcow2 (Debian + pinned mkosi + tools), read-only backing file + per-build overlay |
| Resources | 8 vCPU, 16 GiB RAM, 200 GiB scratch raw disk on host NVMe (deleted after) |
| Network | tap → host bridge → nftables: only to egress proxy :3128; no inbound; no host metadata; DNS through proxy |
| Egress allowlist | snapshot mirrors, our package proxy, our object storage endpoint (presigned PUT only), Flathub/our OSTree mirror |
| Control channel | vsock: job envelope in, status out; virtio-serial: log stream (line-framed, 64 KiB max line) |
| Limits | host cgroup: cpu.max, memory.max 18 GiB, io.weight; build wall timeout 60 min; per-stage timeouts |
| Customer scripts | run in mkosi chroot inside VM, `timeout 600`, no network namespace access (`unshare -n` wrapper), stdout/stderr captured |
| Teardown | VM destroyed, overlay and scratch `blkdiscard`ed/deleted; reaper kills VMs older than 90 min |

Job envelope (signed by api, Ed25519, verified by worker-agent):
```json
{"build_id":"b_01J...","org_id":"o_...","spec_digest":"sha256:..","tree_url":"https://.../presigned",
 "stage_cache":{"get":"https://.../presigned-prefix","put":"..."},"uploads":{"disk.raw.zst":"https://..."},
 "limits":{"wall_sec":3600},"expires_at":"2026-10-28T10:00:00Z","nonce":"..."}
```

## 5. Outputs and naming
- Every artifact stored at `sha256/<hex>`; `artifacts` table maps (build_id, kind) → digest, size, media type.
- Download names: `<osId>_<version>_<arch>.img.xz` (USB writer tools), `.raw.zst` (CLI), `.qcow2`, `.vmdk`, `.ova`, `.iso`.
- Compression: `xz -T0 -6` for `.img.xz`; `zstd -T0 -19 --long=27` for `.raw.zst` (decompress memory ~128 MiB); update payload chunks zstd level 9.
- `manifest.json`: spec digest, catalog version, base snapshot, tool versions, partition table (type, label, uuid, size, sha256), verity root hash, UKI section hashes, PCR 11 predictions, SBOM digest, provenance digest.

## 6. Reproducibility
- `SOURCE_DATE_EPOCH` = snapshot timestamp; mkosi `Seed=` from spec digest; erofs built with fixed timestamps/uuid (mkosi passes these; verify in pinned version); sorted file lists; `LC_ALL=C.UTF-8`; `TZ=UTC`.
- Postinst scripts generated by compiler must not use `date`, `$RANDOM`, network, or `/dev/urandom` for content.
- Strip non-deterministic files: `/var/cache/**`, `/var/lib/apt/lists/**`, `/etc/machine-id` emptied (`uninitialized`), ldconfig aux-cache, `__pycache__` regenerated deterministically or removed.
- Nightly + verified tier: rebuild on another host; compare usr partition sha256; mismatch → `diffoscope` on the two erofs images → bug ticket.

## 7. Build state machine (`store/buildfsm`)

```
queued → leased → building → built → scanning → scanned → testing → tested
   ↘ cancelled          ↘ failed(stage, reason, retryable)
tested → approved (auto)            → finalizing → signed → published
tested → awaiting_review → approved → finalizing → ...
                         → rejected | changes_requested
published → revoked
```
- Transitions are rows in `build_events` (append-only) + `builds.state` updated in the same transaction with optimistic check `WHERE state = $expected`.
- Retry policy: infra failures (`retryable=true`: VM crash, upload 5xx, lease lost) retried up to 3 times with backoff; spec failures (package missing, script exit≠0) not retried.

## 8. Bases (Phase 6)

| Base | Mirror pinning | Kernel | Notes |
|---|---|---|---|
| debian-13 | snapshot.debian.org timestamp | linux-image-amd64 6.12 LTS (+ backports option) | `non-free-firmware` for Wi-Fi/GPU firmware |
| ubuntu-26.04 | snapshot.ubuntu.com timestamp *(verify service)* | linux-generic (HWE option) | restricted/multiverse per catalog |
| fedora | compose ID / fixed repo snapshot | kernel | SELinux instead of AppArmor; different shim |

## 9. Modules (Phase 6)
Module = sysext image built in the same sandbox: erofs + verity + signature, `usr/lib/extension-release.d/extension-release.<name>` with `ID=` and `SYSEXT_LEVEL=` (or `VERSION_ID=`) matching the target. Installed to `/var/lib/extensions/` and merged by `systemd-sysext`; signature verified against the tenant certificate shipped in `/usr/lib/verity.d/` (systemd userspace verification; confirm the mechanism in the pinned systemd version).
