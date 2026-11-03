# CLAUDE.md — Foundry monorepo context

Foundry is a no-code platform that turns an **OS specification** (YAML/JSON, edited through a GUI) into a
**signed, bootable, updatable Linux operating system image**, then installs, updates and manages it on fleets
of devices. Customers are organizations (B2B). We do not write kernels; we compose a pinned upstream base
(Debian 13 first) with `mkosi`, `systemd-repart`, `ukify`, `systemd-sysupdate`.

## Repository layout

```
cmd/
  api/            public REST API, auth, web BFF
  scheduler/      fair-share build scheduler, worker assignment (rendezvous hashing)
  worker-agent/   runs on KVM hosts; boots one ephemeral build VM per build
  builder/        runs INSIDE the build VM: osspec -> mkosi tree -> artifacts
  scanner/        SBOM, CVE, malware, YARA, script lint, tree diff, risk score
  tester/         QEMU/OVMF/swtpm boot-test harness
  signer/         isolated signing service (PKCS#11/HSM). Only binary allowed to sign.
  updatesrv/      TUF metadata + chunk store front end for device updates
  fleetsrv/       enrollment, heartbeats, policies, attestation, rollouts
  foundry-agent/  on-device agent (static Go binary)
  fctl/           CLI for customers and operators
internal/
  osspec/         spec types, defaults, canonicalization (RFC 8785), hashing, migrations
  catalog/        option catalog, CNF compiler, SAT validation
  compiler/       osspec -> mkosi configuration tree (pure function, golden-tested)
  dag/            build stage graph, Merkle cache keys
  cache/          content-addressed store client, local size-aware W-TinyLFU cache
  chunker/        FastCDC content-defined chunking + chunk index format
  store/          Postgres repositories; store/buildfsm = build state machine
  queue/          Postgres-backed job queue (River)
  sched/          deficit round robin + rendezvous hashing
  risk/           risk scoring features and rules
  tuf/            TUF metadata generation/verification
  attest/         TPM quote verification, PCR prediction comparison
web/              React + TypeScript builder (Vite). Solver runs in a Web Worker.
images/
  base/debian-13/ mkosi base profile (pinned snapshot)
  profiles/       desktop-kde, desktop-gnome, kiosk-cage, thin-client
  modules/        systemd-sysext modules
catalog/          YAML option/app/theme catalog (versioned)
deploy/           terraform, helm charts, ansible for KVM hosts, versions.lock
test/golden/      osspec -> mkosi tree golden outputs
test/boot/        QEMU boot scenarios (UEFI, SecureBoot, TPM, A/B, rollback)
test/e2e/         end-to-end API -> image -> boot -> enroll -> update
docs/             plan/, context/, schemas/, db/, examples/
```

## Commands

```
make test                 # unit + integration tests (Postgres at 127.0.0.1:5432; FOUNDRY_REQUIRE_PG=1 to fail instead of skip)
make lint                 # go vet, gofmt, shellcheck
make golden               # compile test/golden/specs/*.yaml and diff against test/golden/out
make golden-update        # regenerate goldens (review the diff in the PR!)
make image SPEC=test/golden/specs/vm-server.yaml          # mkosi build (process sandbox on hosts without KVM)
make sign-image IMAGE=<build>/out/<id>_<ver>.raw          # offline finalize with dev keys (out/keys)
make boot-test IMAGE=<disk.raw> MATRIX=uefi-tpm-reboot,sb-tpm-reboot
make phase0               # Phase 0 acceptance: build v1+v2, sign, 6 boot scenarios
```

## Non-negotiable invariants

1. **Never trust spec content.** All spec input passes `osspec.Validate` (schema → semantic → catalog SAT → policy) before the compiler sees it. The compiler is a pure function: `Compile(spec, catalogVersion, baseSnapshot) -> Tree`, no I/O.
2. **Builds run only inside an ephemeral build VM** with egress restricted to allowlisted mirrors. Never run mkosi or customer scripts on a host, in CI runners without VMs, or in the API process.
3. **Keys never leave `cmd/signer`.** No other package imports PKCS#11 or reads private keys. Signing requests are authorized by build ID + approved review state.
4. **Artifacts are content-addressed and immutable.** Object keys are `sha256/<hex>`. Never overwrite, never mutate. Releases point at digests.
5. **Build state changes go through `store/buildfsm`.** No raw `UPDATE builds SET state=...` anywhere else.
6. **A new spec field needs:** schema + Go type + defaults + validation + compiler mapping + golden test + catalog/UI + docs (`docs/context/osspec-format.md`). Use the `osspec-schema-change` skill.
7. **Reproducibility:** `SOURCE_DATE_EPOCH` from the base snapshot, pinned mirrors (snapshot URLs), pinned tool versions. Two builds of the same spec digest must produce identical `usr` partition hashes.
8. **Device agent budgets:** < 30 MiB RSS, < 1% CPU average, no CGO, works offline for 30 days.
9. **No secrets in logs.** Logs go through `internal/log` which redacts tokens, keys and enrollment secrets.

## Conventions

- Go 1.25+, `context.Context` first arg, errors wrapped with `%w`, no panics outside `main`.
- Streaming I/O for anything image-sized: never `io.ReadAll` an artifact. Hash while streaming.
- Database: keyset pagination, `SELECT ... FOR UPDATE SKIP LOCKED` only inside `queue/`.
- API errors: RFC 9457 `application/problem+json`. All POSTs that create things accept `Idempotency-Key`.
- Commits: Conventional Commits. PRs need one reviewer; security-labelled paths (`signer/`, `tuf/`, `attest/`, `compiler/`, `osspec/`, boot profiles) need a security reviewer.

## Where to read before changing things

| Area | Read |
|---|---|
| Spec fields | docs/context/osspec-format.md |
| Build stages, cache | docs/context/build-pipeline.md, docs/context/caching.md |
| Boot, partitions, Secure Boot, TPM | docs/context/boot-firmware-and-disk.md |
| Root access, kernel, sysctls | docs/context/root-access-and-hardening.md |
| Memory budgets | docs/context/memory-management.md |
| Algorithms | docs/context/algorithms-and-data-structures.md |
| Security | docs/context/security-threat-model.md |
| Updates | docs/context/update-system.md |
| Fleet/agent | docs/context/fleet-and-device-agent.md |
| DB / API | docs/context/data-model.md, docs/db/schema.sql, docs/context/api.md |
| Process | docs/context/sdlc-and-quality.md |
