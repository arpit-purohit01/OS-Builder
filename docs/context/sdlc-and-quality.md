# SDLC and quality

## 1. Process
- Trunk-based development, short-lived branches (< 3 days), feature flags (OpenFeature + simple Postgres-backed provider) for incomplete work.
- Two-week iterations aligned to phase task IDs; each PR references a task ID (`P2-09`).
- Design docs required for: new service, new spec section, changes to boot chain/signing/TUF, schema changes to hot tables. Template: problem, options, decision, security impact, rollout, test plan.
- Threat model delta reviewed for every design doc.

## 2. Pull requests
- Conventional Commits; PR template: what/why, task ID, test evidence, security impact, rollback plan.
- Required: CI green, 1 approval; 2 approvals + security reviewer for CODEOWNERS security paths (`cmd/signer`, `internal/tuf`, `internal/attest`, `internal/compiler`, `internal/osspec`, `images/`, `deploy/`).
- Golden diffs must be reviewed explicitly (`test/golden/**` changes highlighted by a bot comment).

## 3. CI pipeline (each PR)
| Stage | Content | Time budget |
|---|---|---|
| lint | golangci-lint, eslint, tsc, shellcheck, semgrep, hadolint, sqlfluff, JSON Schema meta-validation | 3 min |
| unit | `go test ./... -race -shuffle=on`, vitest | 5 min |
| golden | compile all `docs/examples/*.yaml` + `test/golden/specs/*` → diff | 1 min |
| integration | testcontainers Postgres/Redis/MinIO, api + queue + scheduler | 6 min |
| image (path-filtered) | build 1 example in KVM runner (warm cache) | 12 min |
| boot (path-filtered) | QEMU matrix subset: uefi+sb+tpm | 6 min |
| security | govulncheck, npm audit, gitleaks, license check | 2 min |
Nightly: full image + boot matrix for all examples and bases, reproducibility double-build, fuzzing (24 h rotating), load tests (fleetsrv, scheduler), HIL lab run.

## 4. Test strategy
| Level | What | Tools |
|---|---|---|
| Unit | pure functions: osspec validation, canonicalization, CNF encoding, compiler pieces, FastCDC boundaries, GCRA, DRR, Wilson bound, diff merge-join, risk scoring | Go testing, table tests, property tests (`rapid`) |
| Golden | compiler output for every example and edge spec | `make golden` |
| Fuzz | upload validator (tar/zip), spec parser (YAML/JSON), chunk index parser, TUF metadata parser, heartbeat decoder | Go native fuzzing |
| Integration | services with real Postgres/Redis/MinIO | testcontainers-go |
| Image | build in KVM VM, assert manifest (partitions, labels, sizes, packages) | builder + assertions |
| Boot | QEMU matrix (below), serial-console assertions, screenshots for GUI (openQA needles in Phase 2+) | tester, openQA |
| Upgrade | v1→v2→v3 A/B, power-cut fault injection (kill QEMU at random points), rollback | tester |
| Security | red-team corpus, sandbox escape attempts, signer authorization tests | test/redteam |
| Load | 100k simulated devices; 500 concurrent builds queued | k6 / custom Go load generator |
| HIL | real devices with PiKVM/AMT: install, dual boot, suspend/resume, Wi-Fi, update | lab scripts |
| Usability | moderated sessions per phase | — |

### Boot matrix
| Axis | Values |
|---|---|
| firmware | OVMF (no SB), OVMF secboot with tenant keys, OVMF with MS keys + shim/MOK, SeaBIOS (compat profile only) |
| TPM | none, swtpm 2.0 |
| arch | x86_64 (q35), aarch64 (virt + AAVMF, Phase 6) |
| disk | virtio, NVMe, SATA (AHCI) |
| scenario | fresh install, live boot, dual boot (Windows fixture), update A→B, rollback, factory reset |
Pass criteria: serial marker within timeout, no failed units (`systemctl --failed` via `foundry-test` credential hook), `bootctl status` shows expected entry, `veritysetup status usr` active, LUKS unlocked via TPM.

## 5. Release process (platform services)
- Semver per service; release train weekly; staging soak 48 h; canary 10% of api pods / 1 build host; automated rollback on SLO burn.
- Database migrations: expand → migrate → contract (never drop/rename in the same release as code using new schema); online index creation (`CONCURRENTLY`).
- Agent changes ship inside OS releases (devices) — agent must stay backward compatible with fleetsrv N-2 and vice versa.

## 6. SLOs
| SLI | SLO |
|---|---|
| API availability (non-5xx, excl. 429) | 99.9% / 30 days |
| Build start latency (queued → building) p95 | < 2 min (paid plans) |
| Platform-caused build failures | < 1% of builds |
| TUF/update metadata availability | 99.95% |
| Heartbeat ingestion lag p99 | < 60 s |
| Takedown time (report confirmed → revoked) | < 1 h |

## 7. On-call and incidents
- Rotation of ≥ 4 engineers once in Phase 4; runbooks linked from every alert.
- Severity definitions in security-threat-model.md §9; blameless postmortems within 5 working days, action items tracked.

## 8. Security SDLC
- Threat model per design doc; SAST/DAST; dependency pinning & updates; secret scanning; signed commits for release tags; SLSA provenance for platform builds; annual external pentest + after major features (P3-14).

## 9. Definition of Done (any task)
- Code + tests (unit, plus integration/golden/boot where relevant) merged with CI green.
- Docs updated (`docs/context/*` for design, customer docs for features).
- Metrics/logs/traces added for new paths; alerts if user-facing.
- Feature flag default decided; migration plan if data changes.
- Security impact noted in PR; threat model updated if boundary changed.
