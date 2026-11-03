# Architecture

## 1. Component map

```
                         ┌──────────────────────── Control plane (Kubernetes) ───────────────────────┐
 Browser (builder UI) ──►│ api (REST, auth, BFF) ──► Postgres (primary + replica)                    │
 fctl CLI ─────────────► │   │                          ▲   ▲                                         │
                         │   ├─► queue (River jobs) ────┘   │                                         │
                         │   ├─► scheduler (DRR + rendezvous) ──► lease ──┐                           │
                         │   ├─► scanner (SBOM/CVE/YARA/diff/risk)        │                           │
                         │   ├─► tester (QEMU boot matrix) [KVM nodes]    │                           │
                         │   ├─► preview pool (QEMU + noVNC) [KVM nodes]  │                           │
                         │   ├─► fleetsrv (enroll, heartbeat, policy, attest)                         │
                         │   └─► updatesrv (TUF metadata, chunk index)                                │
                         │ Redis (rate limits, short caches)     Object storage (S3/R2, CAS)          │
                         └────────────────────────────────────────────────────────────────────────────┘
                                                              │ lease
                         ┌──── Build hosts (bare-metal KVM) ───▼────┐      ┌── Signer zone (isolated) ──┐
                         │ worker-agent ──► ephemeral build VM      │      │ signer ──► HSM (PKCS#11)   │
                         │   (builder + mkosi inside)               │      │ only reachable via mTLS    │
                         │ egress proxy (allowlist) ──► mirrors     │      │ from api/scheduler          │
                         │ L1 NVMe cache                            │      └────────────────────────────┘
                         └──────────────────────────────────────────┘
 Devices ── mTLS (TPM key) ──► fleetsrv            Devices ── HTTPS ──► CDN ──► object storage (chunks, TUF)
```

## 2. Core flows

### 2.1 Build flow
1. UI edits spec → `PUT /projects/{id}/spec` stores an immutable `spec_version` (canonical JSON, digest).
2. `POST /builds {spec_version_id}` (Idempotency-Key) → validation layers 1–4 → `builds` row `queued`, job enqueued.
3. Scheduler picks next org by DRR, next build by priority, chooses host by rendezvous hash on the build's base-stage cache key among hosts with free slots → lease (5 min, renewed every 60 s).
4. worker-agent starts build VM, passes signed job envelope (spec digest, compiled tree URL, presigned upload URLs).
5. builder executes the stage DAG, reusing cached stage outputs, streams logs, uploads artifacts.
6. State `built` → scanner job → `scanned` → tester job → `tested` → risk decision → `approved` | `awaiting_review`.
7. On approval: signer signs UKI/bootloader/verity sig/manifest with tenant keys (UKI signing happens *inside* the build via a signing callback in Phase 3: builder sends digest, signer returns signature; keys never enter the VM).
8. `published` → artifacts downloadable; release can be created for update channels.

### 2.2 Install flow
User writes USB image → boots installer entry → repart onto disk (copy verified usr blocks) → bootctl → TPM enrollment → optional fleet enrollment → reboot into installed system.

### 2.3 Update flow
Agent polls TUF timestamp → snapshot → targets → release for its channel and rollout bucket → chunk index → fetch missing chunks via CDN → reconstruct + verify → sysupdate writes inactive slot → reboot in window → boot counting → health → `bless` or fallback → report.

### 2.4 Enrollment flow
Installer/agent reads enrollment token → creates TPM key → CSR + EK cert + AK → fleetsrv validates token, runs credential activation → issues device cert → device appears in console.

## 3. Trust boundaries

| Boundary | Between | Control |
|---|---|---|
| B1 | Internet ↔ api | OIDC/PAT auth, WAF, rate limits, input validation |
| B2 | api ↔ build VM | Signed job envelope, VM isolation, no inbound network, egress allowlist |
| B3 | build VM ↔ signer | VM never talks to signer; builder submits digests via worker-agent → api → signer with build authorization |
| B4 | platform ↔ HSM | PKCS#11 over private network, HSM audit log |
| B5 | devices ↔ fleetsrv | mTLS with TPM-bound keys, signed commands with expiry |
| B6 | devices ↔ CDN | TUF metadata verification makes CDN untrusted |

## 4. Scaling characteristics

| Component | Scales by | Bottleneck | Notes |
|---|---|---|---|
| api | horizontal pods | Postgres connections | pgbouncer transaction pooling |
| scheduler | single active leader (Postgres advisory lock) + standby | none at < 50k builds/day | stateless decisions from DB |
| build hosts | add hosts | NVMe IOPS, RAM | ~4 concurrent builds per 16-core/128 GiB host *(estimate)* |
| scanner | horizontal workers | CPU (YARA, grype) | per-build parallel stages |
| fleetsrv | horizontal pods | DB write rate | batched COPY, partitioned tables |
| updatesrv/CDN | CDN | origin hit rate | immutable content-addressed objects |

## 5. Failure modes and behavior

| Failure | Effect | Behavior |
|---|---|---|
| worker host dies mid-build | lease expires | job re-leased elsewhere; partial stage outputs not committed (atomic rename on upload completion) |
| object storage outage | uploads/downloads fail | builds pause at upload with retries (exponential backoff, cap 5 min); devices keep current release |
| signer/HSM outage | approvals queue | builds wait in `approved_pending_sign`; alert; no unsigned fallback ever |
| Postgres primary loss | control plane down | promote replica (runbook, RTO target 30 min); devices unaffected (keep running, retry heartbeats) |
| bad release | devices fail health | boot counting rollback; rollout gate pauses; operator yanks release (TUF targets update) |
| CDN serving stale/malicious data | — | TUF rejects; agent falls back to origin |
