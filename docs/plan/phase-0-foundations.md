# Phase 0 — Foundations (4 weeks)

## Goal
Remove the biggest technical unknowns before writing product code: prove that a hand-written mkosi
configuration produces a Debian 13 image that boots under UEFI with Secure Boot, verity-protected `/usr`,
TPM2-bound disk encryption and A/B layout, entirely in automated CI. Set up the repo, CI, and security
foundations every later phase depends on.

## Inputs
- Pilot customer list (A1). For each: device models, count, use case, network constraints, current imaging process, pain points.

## Tasks

| ID | Task | Detail | Design doc |
|---|---|---|---|
| P0-01 | Customer discovery | 2 calls per pilot customer. Capture: device models (DMI vendor/product), CPU/GPU/Wi-Fi chips (lspci/lsusb output if possible), RAM/disk, Secure Boot state, Windows co-existence needs, kiosk vs desktop, offline constraints, app list, identity provider. Output: `pilots.md` with a hardware matrix | — |
| P0-02 | Monorepo + tooling | Layout from `CLAUDE.md`. Go workspace, Vite app skeleton, golangci-lint, eslint, shellcheck, semgrep, pre-commit, Makefile targets, `deploy/versions.lock` | sdlc-and-quality.md |
| P0-03 | CI with KVM | Self-hosted runners on one bare-metal KVM host (`/dev/kvm`, nested virtualization not required). Jobs: lint, unit, golden, image-build, boot-test. Runners are ephemeral VMs (one job per VM) | sdlc-and-quality.md §CI |
| P0-04 | Boot spike: hand-written mkosi tree | `images/base/debian-13/` with mkosi.conf, repart definitions (ESP, usr-A verity data/hash/sig, root), UKI via ukify, systemd-boot, `usrhash=` in cmdline. Pin mkosi release | boot-firmware-and-disk.md |
| P0-05 | Secure Boot test keys | Generate test PK/KEK/db (RSA-2048) with `openssl`, build OVMF vars with enrolled keys (OVMF secboot build + `virt-fw-vars` to enroll), sign systemd-boot and UKI. Include Microsoft UEFI CAs in the test db to mirror real firmware | boot-firmware-and-disk.md §Secure Boot |
| P0-06 | TPM2 + LUKS spike | swtpm in QEMU; first boot: `systemd-repart` creates encrypted root; enroll with `systemd-cryptenroll --tpm2-device=auto --tpm2-public-key=<pcr-signing-pub>` using a PCR 11 policy signed by an **initrd-phase-only** key (`systemd-measure sign --phase=enter-initrd`) | boot-firmware-and-disk.md §TPM |
| P0-07 | A/B + boot counting spike | Build v1 and v2; install v2 into slot B via sysupdate from a local directory; boot v2 with tries `+3`; inject failure (unit that fails boot-complete.target) and prove automatic fallback to v1 | update-system.md |
| P0-08 | Threat model v0 | STRIDE over the architecture in `context/architecture.md`; list assets, trust boundaries; record mitigations owned by later phases | security-threat-model.md |
| P0-09 | Key management design | Choose HSM path (cloud KMS/HSM with PKCS#11 vs. YubiHSM2 for start). Write the key ceremony runbook (offline root, Shamir backup 3-of-5) | security-threat-model.md §Keys |
| P0-10 | Spec v1alpha1 draft | Write `schemas/osspec.v1alpha1.schema.json` and 3 example specs from pilot needs | osspec-format.md |
| P0-11 | ADRs | Record decisions 0001–0012 in `context/decisions.md` (review with whole team) | decisions.md |
| P0-12 | Observability skeleton | OpenTelemetry SDK wired into a hello-world service; Prometheus/Grafana/Loki in dev compose | infrastructure-and-cost.md |

## Acceptance tests
1. `make image SPEC=<hand-written>` in CI produces `disk.raw`; `make boot-test MATRIX=uefi,sb,tpm` reaches `multi-user.target` (or `graphical.target`) and prints the marker `FOUNDRY-BOOT-OK` on the serial console within 120 s.
2. With Secure Boot enforcing, an unsigned UKI placed on the ESP is refused (test asserts firmware/sd-boot error on serial).
3. Flipping one byte in the usr data partition causes I/O errors on read (dm-verity), test asserts the kernel log `verity` corruption message.
4. TPM-bound root unlocks automatically on unchanged boot; after enrolling a different PCR-signing key it does **not** unlock (falls back to recovery key prompt); and a TPM unseal attempt from the running OS (after `leave-initrd`) fails.
5. A/B fallback test passes 10/10 runs.

## Exit gate
All five acceptance tests green in CI for 5 consecutive days; ADRs approved; pilot hardware matrix complete.

## Risks
- OVMF Secure Boot variable setup is fiddly → keep a script `test/boot/ovmf/mkvars.sh` and check the generated vars file into the test fixture store (not git LFS of secrets; test keys only).
- mkosi option names differ between releases → pin and read the man page of the pinned version.
