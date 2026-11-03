# Status — Phase 1 (MVP image pipeline)

Date: 2026-10-29. Scope: `docs/plan/phase-1-mvp-image-pipeline.md`. Phase 0 (boot chain) results
are unchanged and kept in `docs/phase0-status.md`.

## What works, verified in this environment (QEMU TCG, no KVM, Ubuntu 24.04 images)

**End to end (`make e2e`, last run PASS in 481 s):** API login → project → spec → build (mkosi) →
SBOM + image scan (risk score 0, no findings on `vm-server`) → boot test (UEFI + TPM2, reboot) →
auto-approval → finalize (signed UKI/bootloader/verity, `SHA256SUMS` signed with the artifact key,
uki + qcow2 covered by it) → publish → signed download → signature verification.

**Go test suite (`make test`, real Postgres, all 30+ packages green, `go vet` clean):**
- Acceptance 1: every registered API route is hit by an integration test, enforced in
  `internal/api/main_test.go` (the package fails if an endpoint is never exercised).
- Acceptance 6: overlay containing `../../etc/shadow` → 422 problem+json at upload and at build
  creation (`TestOverlayTraversalRejected`).
- Auth incl. OIDC code flow against a fake IdP (PKCE, nonce, single-use state, open-redirect guard),
  org/members/tokens/scopes/revocation, reviews (two-person rule), finding acceptance, cancel,
  layouts, signed blob GET/HEAD/Range, live-preview bridge (WebSocket origin check, quota).
- Store, FSM, queue (leases, dead-job hook), scheduler, signer authorization, finalize, scanner
  (baseline snapshot, stock-unit links, YARA-style rules), egress proxy, VM sandbox, process registry.
- Web: 14 unit tests, `tsc`, Playwright UI smoke (`test/ui/smoke.mjs`); Go↔TS solver parity on 800 fixtures.

**Reproducibility (acceptance 4):** four builds of `vm-server` made in this session on separate
work dirs all produced the identical `/usr` dm-verity root hash. The worker now compares every
build with the previous build of the same tree in the same organization and records
`builds.reproduced` (feeds the risk tier); `scripts/test-reproducible.sh` automates the nightly check.

**Artifact store (P1-11):** `internal/blob/s3.go` — SigV4, multipart (64 MiB × 4), streaming SHA-256,
upload-to-temp then copy to `sha256/<hex>`, retries, ranged reads, presigned GET, `FOUNDRY_S3_*`
config (`docs/runbooks/artifact-store.md`). Tested against an in-memory S3 that verifies SigV4 on
every request, and against the official AWS `get-vanilla` signature vector.

## Not verified here (be aware before relying on it)

| Item | State |
|---|---|
| Acceptance 5 — worker SIGKILL mid-build | `scripts/test-worker-crash.sh` + process-group reaper (`internal/procreg`) written and unit-tested; the script itself was **not run** |
| Acceptance 2 — `docs/examples/*.yaml` boot matrix (uefi/sb/tpm) | `vm-server` boots in the matrix (Phase 0 + e2e); kiosk / school-lab / office-laptop were **not built or booted** (desktop images need hours under TCG) |
| Acceptance 3 — build time targets | Cold `vm-server` build 2.5–4.5 min here; desktop profiles not measured |
| Output formats img.xz / vmdk / ova / iso | Converters unit-tested; qcow2 verified on a real build. A build requesting all five was still finalizing when this archive was cut |
| S3 against a real server (MinIO/AWS) | Not run — the MinIO download host is blocked from this sandbox |
| Full build inside the QEMU builder VM (`FOUNDRY_SANDBOX=vm`) | VM isolation test passes with seccomp; a full mkosi build inside it was not run (process sandbox used for e2e) |
| Installer QEMU scenarios, preview with a real VM (Phase 2 items) | Written, **not run** |
| Exit gate (pilot users, real devices, 7 nights green) | Needs people and hardware |
| Debian 13 base | Compiler support + goldens; only Ubuntu 24.04 is boot-tested |

## Real defects found by this round of testing (fixed)
- Image scan treated everything package maintainer scripts generate as customer content (risk 100, two
  reviewers on a plain build). Now: baseline snapshot of `/usr` and `/etc` taken before customer
  scripts; the "generated" path list had never matched (leading slash); stock-unit enable links are
  recognized, sensitive units (ssh, vnc, smb, …) never are.
- API logging wrapper hid `http.Hijacker` → every live-preview WebSocket would have answered 200.
- WebSocket layer emitted empty frames for empty writes.
- Job envelope signature broke under jsonb normalization; SHA256SUMS signing refused (artifact not yet
  recorded); orphan mkosi processes after a worker crash; builds stuck `finalizing` when a job
  exhausted its retries.

## How to run

```sh
scripts/host-deps.sh && make tools-tree        # once
scripts/pg-dev.sh                              # local Postgres
make test                                      # unit + integration (FOUNDRY_REQUIRE_PG=1 to forbid skips)
make e2e                                       # starts the dev stack, runs the full pipeline test
bin/fctl login -url http://127.0.0.1:8080 -email you@example.com -create-org "Dev:dev"
make test-crash PROJECT=prj_…   |   make test-repro PROJECT=prj_…
```
Web app: `make web` then open the API URL (it serves `web/dist`).
