# Phase 7 — Enterprise hardening (ongoing)

## Goal
Meet the procurement and reliability bar of larger customers: audits, compliance profiles, multi-region
availability, data residency, on-prem/air-gapped deployments and contractual SLAs.

## Tasks

| ID | Task | Detail |
|---|---|---|
| P7-01 | SOC 2 Type II | Controls mapped to existing practices (access reviews, change management via PRs, audit logs, backups, incident response); evidence collection automated |
| P7-02 | ISO 27001 (if customers require) | ISMS, risk register (extend `00-roadmap-overview.md`), supplier management |
| P7-03 | Compliance profiles | CIS Debian/Ubuntu benchmark profile as a catalog option; OpenSCAP scan stage reporting per release; profile for education (content filtering hooks), healthcare kiosks (auto-lock, no removable storage) |
| P7-04 | Multi-region | Control plane active-passive across 2 regions (Postgres streaming replica, promoted by runbook), object storage replicated, update metadata served from both via CDN; build pools per region |
| P7-05 | Data residency | Org-level region pinning for artifacts, device data and logs (EU/US/IN) |
| P7-06 | On-prem build appliance | Helm chart + KVM host installer for customers who must build inside their network; licensing via signed license file; updates to the appliance itself through our TUF |
| P7-07 | Air-gapped fleets | Offline update bundles (P4-15) + offline attestation verification tool |
| P7-08 | SLAs | 99.9% API, 99.95% update metadata, P1 response 1 h; status page; credits |
| P7-09 | Customer-managed keys | Tenant keys in customer's HSM/KMS (BYOK/HYOK) via PKCS#11 proxy; signer calls out per request |
| P7-10 | Vendor BIOS management | Read/set firmware settings via Linux `firmware-attributes` sysfs class (Dell, Lenovo, HP supported models): BIOS admin password, boot order lock, Secure Boot enforce, disable USB boot — exposed as fleet policy with per-model capability detection |
| P7-11 | Chaos & DR drills | Quarterly: region failover, signer outage, object storage outage, mass rollback |

## Exit gate (per item)
Audit report issued; DR drill meets RPO 15 min / RTO 4 h; first on-prem customer live.
