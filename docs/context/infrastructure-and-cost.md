# Infrastructure and cost

## 1. Environments
| Env | Purpose | Data | Keys |
|---|---|---|---|
| dev | laptops + docker compose (Postgres, Redis, MinIO, Grafana stack) | fake | test keys (in repo, clearly marked TEST) |
| ci | ephemeral KVM runners | fixtures | test keys |
| staging | full replica of prod topology, smaller | synthetic + anonymized | staging HSM partition |
| prod | customers | real | prod HSM |

## 2. Topology
- **Control plane**: managed Kubernetes (3 nodes min, spread over 3 zones): api, scheduler (leader election), scanner workers, fleetsrv, updatesrv, web static via CDN.
- **Data**: managed Postgres (primary + synchronous standby in another zone + async replica in DR region), Redis (two instances), object storage bucket per region with versioning off (objects are immutable), lifecycle rules.
- **Build/test/preview hosts**: bare-metal servers with KVM (AMD EPYC/Ryzen or Intel Xeon, 16–32 cores, 128–256 GiB RAM, 2× NVMe 3.84 TB RAID1 for cache + scratch), provisioned by Ansible, OS = our own image (dogfooding) with host hardening, worker-agent as systemd service. Pools: `build`, `test`, `preview`, `finalize` (finalize hosts isolated network, no customer content except built images).
- **Signer zone**: separate network segment/VPC; signer pods (2 replicas) + HSM (cloud HSM/KMS PKCS#11 or network HSM); only api/finalize can reach signer via mTLS.
- **Edge**: CDN in front of object storage and web app; WAF on api.

## 3. Capacity model *(estimates — measure in Phase 1 and replace)*
| Resource | Unit demand | Notes |
|---|---|---|
| Build VM | 8 vCPU, 16 GiB, ~15 min warm / 30 min cold | 4 concurrent per 32-thread/128 GiB host |
| Builds/host/day | ~4 × (24 × 60 / 20) ≈ 288 at full utilization, plan for 50% → ~140 | |
| Test VM | 2–4 vCPU, 4 GiB, ~5 min per matrix cell | matrix 3 cells in P1 → ~15 VM-min per build |
| Preview VM | 2 vCPU, 4 GiB, 15 min session | |
| Storage per build | 5–15 GiB (all formats) before retention | dedupe via CAS for chunks/packages |
| Chunk store per release | new chunks only, typically a small fraction after first release | |

Formulas:
- `hosts_build = ceil(peak_builds_per_hour × avg_build_minutes / 60 / slots_per_host / target_utilization)`
- `storage_GB_month = Σ_active_builds(size) + Σ_releases(unique_chunk_bytes + sources) `
- `egress_GB = downloads × image_GB + Σ_devices(updates × delta_GB)`

## 4. Cost model (illustrative; *verify all prices before use*)
Assumed prices (replace with real quotes): bare-metal 32-thread/128 GiB host ≈ $150–250/month; object storage ≈ $0.015–0.023/GB-month; zero-egress storage + CDN for downloads (or ~$0.02–0.09/GB egress on hyperscalers); managed Postgres small HA ≈ $300–600/month; Kubernetes control plane + 3 nodes ≈ $300–500/month; HSM: cloud KMS per-key/per-op pricing or a dedicated HSM ≈ hundreds to thousands/month.

Example: pilot stage (≈ 300 builds/month, 500 devices)
| Item | Qty | Monthly *(illustrative)* |
|---|---|---|
| Build/test host | 2 | $300–500 |
| Control plane k8s + Postgres + Redis | — | $700–1,200 |
| Object storage | 3 TB | $45–70 |
| CDN/egress (zero-egress storage) | 5 TB | ~$0 egress + request fees |
| HSM/KMS | — | $100–1,500 depending on option |
| Observability | — | $0–300 (self-hosted vs SaaS) |
| **Total** | | **≈ $1.2k–3.5k** |

Per-build compute ≈ host cost / builds per host ≈ $200 / 4,000 ≈ $0.05 at moderate utilization.

## 5. Preview service
- Pool of pre-booted QEMU VMs per popular profile; per session: qcow2 overlay on the built image (copy-on-write), VNC on unix socket → websockify → noVNC over WSS through the api gateway with a signed short-lived session token.
- Quotas: free 2 sessions/day, pilot 20/day; idle suspend 3 min, hard stop 15 min.
- KSM (kernel samepage merging) enabled on preview hosts to share identical guest pages across sessions.

## 6. Observability
- OpenTelemetry SDK in all services → collector → metrics (Prometheus/Mimir), traces (Tempo), logs (Loki). Trace context propagated into build VM via job envelope so a build is one trace.
- Key dashboards: build funnel (queued→published) with p50/p95 per stage; cache hit ratios; scheduler queue depth per org; scanner backlog; device heartbeat rate & lag; update success/rollback by release; attestation status; API RED metrics.
- Alerts only on SLO burn rates (multi-window: 2% budget in 1 h, 5% in 6 h) + a few hard alerts (signer down, HSM errors, TUF timestamp expiring < 6 h, backup failure).

## 7. Backups and DR
| Data | Method | RPO | RTO |
|---|---|---|---|
| Postgres | WAL archiving + PITR, cross-region replica | 15 min (5 min target) | 1–4 h |
| Object storage | cross-region replication for releases/TUF/sources; builds not replicated | 1 h | 4 h |
| HSM keys | HSM backup/cloning per vendor, offline encrypted backups for root | n/a | per ceremony |
| Config/IaC | git | 0 | redeploy |

Runbooks: region failover, restore drill, key restore, CDN purge (rarely needed — immutable objects).

## 8. Infrastructure as code
Terraform (cloud resources, DNS, CDN, buckets), Helm charts (services), Ansible (bare-metal KVM hosts), `deploy/versions.lock` (tool & image digests). All changes via PR + plan output in CI; prod applies require two approvals.
