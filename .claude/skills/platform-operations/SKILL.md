---
name: platform-operations
description: Use for deploy/ (Terraform, Helm, Ansible), environment setup, KVM host provisioning, observability, SLO alerts, backups and restore drills, incident response, key ceremonies and rotations, takedowns, and capacity/cost planning.
---

# Platform operations

Read first: `docs/context/infrastructure-and-cost.md`, `docs/context/security-threat-model.md` §4 and §9, `docs/context/sdlc-and-quality.md` §5–7.

## Change management
- All infra via PR: Terraform plan posted by CI; prod apply needs two approvals; Helm values per env in `deploy/helm/values/<env>.yaml`; image digests (not tags) only.
- `deploy/versions.lock` is the single place for tool versions (mkosi, systemd in builder image, QEMU, OVMF, swtpm, syft, grype, ClamAV, YARA, Go, Node). Upgrades: one tool per PR, with golden + boot matrix run.

## KVM host provisioning (Ansible role `kvm_host`)
1. Install our own host OS image; verify `/dev/kvm`, IOMMU on, nested virt off.
2. NVMe RAID1 for `/var/lib/foundry` (cache + scratch), XFS with `discard` via periodic fstrim.
3. Isolated bridge `fbr0` without uplink; nftables: VM → proxy:3128 only; host → object storage/api only.
4. Egress proxy (Squid/Envoy) with allowlist from `deploy/proxy/allowlist.yaml`; logs to Loki.
5. worker-agent systemd unit with host credentials (mTLS cert from internal CA, 30-day rotation).
6. Register host in scheduler (labels: arch, pool, slots, NVMe free).

## Runbooks (keep in `deploy/runbooks/`, link from alerts)
| Runbook | Key steps |
|---|---|
| Signer/HSM down | confirm HSM health; builds queue in `finalizing` (expected); never enable software keys in prod; failover to second HSM partition |
| TUF timestamp expiring | check timestamp signer job; re-sign manually with online key; alert at < 6 h |
| Mass rollback detected | halt rollout; compare failing devices' HCL/model; collect journals; yank release |
| Postgres failover | promote replica; update DNS/service; verify scheduler leader; replay River jobs stuck in `running` past lease |
| Takedown | confirm report; revoke release (TUF targets); mark tier revoked; notify org; preserve evidence; postmortem |
| Key compromise | SEV1; follow security-threat-model.md §9; customer comms template |
| Restore drill (monthly) | restore PITR to staging; run verification queries; record duration vs RTO |

## Key ceremony checklist (P3-02)
Air-gapped laptop booted from verified live image → HSM initialized → generate root keys (TUF root ×3) → sign root metadata → export public parts → back up wrapped keys to two encrypted media → Shamir 3-of-5 split of backup passphrase to custodians → video + signed log → media to two safes. Repeat signature tests from a clean machine with only public material.

## Capacity review (monthly)
Builds/day, p95 queue time, cache hit ratio, host utilization, storage growth, egress, cost per build/device. Scale build hosts when p95 start latency > 2 min for 3 days or utilization > 70%.
