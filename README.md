# Foundry

Foundry is a no-code platform (B2B first) that turns an **OS specification**
(YAML/JSON, edited through a GUI) into a **signed, bootable, updatable Linux
operating system image**, then installs, updates and manages it on fleets.
Images are composed from a pinned upstream base with mkosi and systemd —
we do not write kernels.

This archive is the **Phase 1 (MVP image pipeline)** delivery (it includes everything from Phase 0 and groundwork for Phases 2–3). See
[`docs/STATUS.md`](docs/STATUS.md) for what is complete, the acceptance test
results, and known deviations from the plan.

## What Phase 0 proves

An image generated from a spec boots in an automated harness with the full
production security chain:

```
UEFI firmware ── Secure Boot (tenant PK/KEK/db, auto-enrolled in setup mode)
  └─ systemd-boot (platform copy, signed by tenant db)
       └─ UKI (kernel+initrd+cmdline, signed; cmdline pins usrhash=)
            ├─ /usr  = erofs on dm-verity (root hash in the signed cmdline)
            ├─ root  = ext4 on LUKS2, created on first boot, key sealed to the
            │          TPM2 with a PCR 11 policy signed for the initrd phase only
            └─ A/B slots + boot counting (+3) + health check → automatic fallback
```

Acceptance suite (`make phase0`):

| # | Test | How |
|---|---|---|
| 1 | Image builds and boots (UEFI and Secure Boot, TPM2, reboot) | `uefi-tpm-reboot`, `sb-tpm-reboot` |
| 2 | Secure Boot refuses an unsigned UKI | `sb-unsigned-uki` |
| 3 | A flipped byte in `/usr` is caught by dm-verity | `verity-corruption` |
| 4 | Root unlocks on unchanged boot, not with a foreign PCR key, never from the running OS | `tpm-foreign-pcr-key` + unseal probe in every boot |
| 5 | Update into slot B with systemd-sysupdate; failing trial boots fall back to the old version | `ab-fallback` |

## Quick start (Ubuntu 24.04 host)

```sh
scripts/host-deps.sh            # qemu, ovmf, swtpm, systemd-boot-efi, ukify, sbsigntool, efitools, ...
# mkosi 27.1 (see deploy/versions.lock) on PATH, then build the tools tree once:
make tools-tree                 # /var/lib/foundry/tools-tree (Ubuntu 26.04 tools)
make test                       # unit + golden tests (Postgres optional; tests needing it skip)
make phase0                     # build v1+v2, sign, run the five acceptance tests
```

Individual steps:

```sh
bin/fctl validate test/golden/specs/vm-server.yaml       # schema → semantic → catalog SAT → policy
bin/fctl compile  test/golden/specs/vm-server.yaml -o /tmp/tree   # spec → mkosi project (pure)
make image SPEC=test/golden/specs/vm-server.yaml          # mkosi build → disk.raw, UKI, roothash
make sign-image IMAGE=/var/lib/foundry/builds/<id>/out/ci-server_1.0.0.raw
make boot-test IMAGE=.../signed/ci-server_1.0.0.signed.raw MATRIX=sb-tpm-reboot
```

Mirrors: builds fetch packages from `http://archive.ubuntu.com/ubuntu`
(`FOUNDRY_DIRECT_MIRRORS=1` drops proxy variables for mkosi). Without
`/dev/kvm` QEMU runs in TCG mode: a boot takes 2–3 minutes instead of seconds,
and the full suite about an hour.

## Repository layout

```
cmd/fctl          CLI: validate, compile, build, catalog
cmd/signer        the only program with key access: keys, sign-image (offline), serve (HTTP, DB-authorized)
cmd/tester        QEMU/OVMF/swtpm boot tests; `tester acceptance` runs the Phase 0 suite
cmd/foundry-agent on-device agent (skeleton: sd_notify + watchdog)
internal/osspec   spec types, schema, defaults, semantic validation, RFC 8785 digests, migrations
internal/catalog  option catalog, CNF compiler, DPLL SAT solver with MUS explanations
internal/compiler spec → mkosi tree (pure, golden-tested); boot/initrd/health/reporter scripts in files/
internal/builder  runs mkosi and collects outputs
internal/signer   keys, UKI re-assembly with trusted stub + PCR policy, sbsign, verity sig, enrollment
internal/finalize trusted post-approval stage: signed boot chain into the ESP, verity-sig GPT entry, update payload
internal/gpt      GPT reader/writer (CRC-checked, primary + backup)
internal/uki      UKI section parsing, PCR 11 prediction
internal/tester   VM machine model + acceptance scenarios
catalog/          option/app/theme catalog (YAML)
test/golden/      specs and expected compiler output
docs/             plan, context (design), schemas, runbooks, status
```

Phase 1 groundwork is also present (REST API, Postgres store, job queue,
scheduler, worker pipeline, scanner v0, format converters); its state is
listed separately in `docs/STATUS.md`.

## Conventions

Go 1.24 with vendored modules (`GOFLAGS=-mod=vendor`, `GOTOOLCHAIN=local`),
`make lint` (go vet, gofmt, shellcheck), golden outputs reviewed on change
(`make golden-update`). Non-negotiable invariants are in `CLAUDE.md`.
