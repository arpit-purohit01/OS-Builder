# Phase 1 — MVP image pipeline (8 weeks)

## Goal
A pilot customer fills a simple web form, clicks **Build**, and minutes later downloads a signed,
bootable, reproducible image (USB raw image, qcow2, VMDK/OVA) that boots on their real hardware.
One base (Debian 13), one architecture (x86_64), three profiles (desktop-kde, desktop-gnome, kiosk-cage).

## Scope
In: spec v1alpha1, validation, compiler, build DAG with caching, one KVM host with N build VMs, artifact
storage, downloads, auth (email + OIDC), org/project model, build logs streaming, basic boot test.
Out: catalog SAT (simple rules only), installer GUI (raw image + `dd`/Etcher instructions), dual boot,
updates, fleet, scanning beyond SBOM, public publishing.

## Tasks

| ID | Task | Detail | Design doc |
|---|---|---|---|
| P1-01 | `internal/osspec` | Go types generated from JSON Schema (quicktype or hand-written + schema test), `Default()`, `Validate()` layers 1–2, canonical JSON (RFC 8785) + SHA-256 `SpecDigest`, `apiVersion` migration framework | osspec-format.md |
| P1-02 | `internal/compiler` | Pure `Compile(spec, catalog, base) (Tree, error)` producing an in-memory file tree: `mkosi.conf`, `mkosi.conf.d/*.conf`, `mkosi.repart/*.conf`, `mkosi.extra/**`, `mkosi.postinst.chroot`, `mkosi.finalize`. Deterministic ordering, no timestamps | build-pipeline.md §Compilation |
| P1-03 | Golden tests | `test/golden/<example>/` holds the expected tree; `make golden` diffs; CI fails on drift | sdlc-and-quality.md |
| P1-04 | Base profiles | `images/base/debian-13` + `images/profiles/{desktop-kde,desktop-gnome,kiosk-cage}`, pinned `snapshot.debian.org` timestamp, package lists, systemd presets | build-pipeline.md |
| P1-05 | `internal/dag` | Stage graph (resolve → fetch → rootfs → overlay → initrd/UKI → verity → disk → convert → sbom), Merkle cache keys, executor with cache lookups | build-pipeline.md, caching.md |
| P1-06 | `cmd/builder` | Runs inside the VM. Pulls compiled tree + inputs, runs mkosi with `--force`, streams logs over virtio-serial to worker-agent, uploads outputs via presigned URLs, writes `manifest.json` | build-pipeline.md §Sandbox |
| P1-07 | `cmd/worker-agent` | On KVM host: leases job, creates VM from golden builder image (qcow2 backing file + overlay), attaches scratch disk (NVMe-backed raw, 200 GiB), virtio-serial log channel, egress via host proxy; destroys VM after | build-pipeline.md §Sandbox |
| P1-08 | Egress proxy | Squid/Envoy on host, SNI/host allowlist: `snapshot.debian.org`, our package cache, our object store. Everything else denied & logged | security-threat-model.md |
| P1-09 | `internal/queue` + scheduler v0 | River jobs in Postgres; per-org concurrency limit (1 for MVP); FIFO | algorithms-and-data-structures.md §Scheduling |
| P1-10 | Build state machine | `store/buildfsm` with transition table and event log (`build_events`) | data-model.md |
| P1-11 | Artifact store | S3-compatible bucket, content-addressed keys, multipart upload (64 MiB parts, 4 concurrent), SHA-256 computed while streaming, `artifacts` table | caching.md, memory-management.md |
| P1-12 | Signing v0 | `cmd/signer` with one platform test key in software (HSM in Phase 3) behind the same gRPC interface: `SignUKI`, `SignBootloader`, `SignVerityRootHash`, `SignManifest` | security-threat-model.md |
| P1-13 | Output formats | `disk.img.xz` (USB writer tools), `disk.raw.zst` (CLI), `disk.qcow2`, `disk.vmdk` + `.ova` (OVF descriptor), `SHA256SUMS` + signature | build-pipeline.md §Outputs |
| P1-14 | Boot test v0 | `cmd/tester`: QEMU q35 + OVMF SB vars with platform key + swtpm; pass = serial marker `FOUNDRY-BOOT-OK` from `foundry-boot-ok.service` (After=graphical.target or kiosk session) within 180 s | qa skill, sdlc-and-quality.md |
| P1-15 | API v0 | Auth (OIDC + email magic link), orgs, projects, specs (immutable versions), builds (create, get, cancel, logs via SSE), artifacts (presigned GET) | api.md |
| P1-16 | Web v0 | Form-based builder: profile, wallpaper upload, app checklist (from catalog), hostname/locale/timezone, user creation mode, root-access level (3 presets), download page with instructions | builder-web-ui skill |
| P1-17 | SBOM | `syft` on the rootfs tree → SPDX JSON stored as artifact | security-threat-model.md |
| P1-18 | Hardware lab v0 | 2 devices from pilot list; PiKVM or Intel AMT for remote console; manual install checklist | sdlc-and-quality.md §HIL |
| P1-19 | Reproducibility check | Nightly: rebuild 3 pilot specs on a different VM, compare usr partition SHA-256; `diffoscope` report on mismatch | build-pipeline.md §Reproducibility |

## Acceptance tests
1. API: create spec → build → artifacts available; all endpoints covered by integration tests against a real Postgres.
2. Each example spec in `docs/examples/` builds green, boots in QEMU matrix `uefi,sb,tpm`.
3. Cold build ≤ 25 min, warm build (only overlay changed) ≤ 8 min *(targets)*.
4. Same spec built twice on different VMs → identical usr hash.
5. Killing the worker-agent mid-build: job is re-leased after lease timeout (5 min) and completes; no orphan VMs remain (reaper test).
6. A spec with `../../etc/shadow` in an overlay path is rejected at validation with a 422 problem response.

## Exit gate
3 pilot specs built by pilot users themselves; images installed on 2 real devices each; reproducibility nightly green 7 days.

## Risks
- Build time too long → stage cache, local apt cache on host, zstd `-T0`, erofs lz4hc, parallel compressions.
- Large uploads fail → resumable multipart, retry with backoff, verify part ETags/checksums.
