# Status — Phase 0 (Foundations)

Date: 2026-10-28. Scope: `docs/plan/phase-0-foundations.md`. (Phase 1 status: `docs/STATUS.md`.)

## Phase 0 tasks

| ID | Task | Status | Where |
|---|---|---|---|
| P0-01 | Customer discovery | Template ready; needs your pilot calls | `docs/pilots.md` |
| P0-02 | Monorepo + tooling | Done: vendored Go module, Makefile, shellcheck, gofmt/vet, pre-commit, pinned versions | `Makefile`, `deploy/versions.lock` |
| P0-03 | CI with KVM | Workflow written; not executed here (no CI service/KVM) | `.github/workflows/ci.yml` |
| P0-04 | Boot spike image | Done, compiler-generated: ESP, erofs `/usr` + dm-verity, deferred verity-sig, empty B slots, UKI, systemd-boot, TPM2-encrypted root created on first boot | `test/golden/out/vm-server/`, `internal/compiler` |
| P0-05 | Secure Boot test keys | Done: per-tenant PK→KEK→db, auto-enrolled in setup mode | `internal/signer`, `internal/finalize` |
| P0-06 | TPM2 + LUKS | Done: PCR 11 policy signed for the `enter-initrd` phase only | `internal/signer/ops.go` |
| P0-07 | A/B + boot counting | Done: sysupdate transfers, `+3` tries, health check, fallback test | `internal/compiler/sections.go`, `internal/tester/acceptance.go` |
| P0-08 | Threat model v0 | Done | `docs/context/security-threat-model.md` |
| P0-09 | Key management design | Done (software keys now, HSM Phase 3) | `docs/runbooks/key-ceremony.md` |
| P0-10 | Spec v1alpha1 | Done | `docs/schemas/`, `internal/osspec` |
| P0-11 | ADRs | Done (0001–0012) | `docs/context/decisions.md` |
| P0-12 | Observability skeleton | Prometheus metrics + JSON logs; dev compose | `internal/metrics`, `deploy/dev/` |

## Acceptance tests (`make phase0`, QEMU TCG, no KVM — all passed on 2026-10-28)

| # | Scenario | Result | Time (TCG) |
|---|---|---|---|
| 1 | `uefi-tpm-reboot` | PASS | 238 s |
| 1 | `sb-tpm-reboot` (Secure Boot enforcing, TPM2, PCR 11 equals prediction, unseal from running OS refused) | PASS | 234 s |
| 2 | `sb-unsigned-uki` (`Access denied`) | PASS | 16 s |
| 3 | `verity-corruption` (`data block 0 is corrupted`) | PASS | 47 s |
| 4 | `tpm-foreign-pcr-key` (cryptsetup fails with a foreign PCR key) | PASS | 160 s |
| 5 | `ab-fallback` (v2 into slot B; 3 failing trial boots; fallback to v1) | PASS | 615 s |

Machine-readable reports: `test/boot/results/`.

## Deviations from the plan
Ubuntu 24.04 boot-tested (Debian 13 / Ubuntu 26.04 exist in the compiler, golden-tested only);
compiler-generated tree instead of a hand-written one; systemd-boot auto-enrollment instead of
`virt-fw-vars`; one PCR key (initrd phase); boots take 2–3 min under TCG (limit 15 min per boot).

## Defects found and fixed by the acceptance runs
UKI size (346→75 MB, `KernelInitrdModules=default`); efivarfs read via pipe; health-check ordering
cycle; enrollment keys under `loader/keys/auto/`; verity-sig partition in slot B; update payload
names carry the root-hash-derived partition UUID; `systemd-container` required by sysupdate;
`.conf` definitions; PCR policy limited to the initrd phase.
