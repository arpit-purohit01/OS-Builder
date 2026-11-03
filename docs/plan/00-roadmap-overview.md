# 00 — Roadmap overview

## Product in one paragraph

An organization describes the OS it wants (look, apps, security level, root access, kernel, ecosystem
services, branding) in a no-code builder. Foundry compiles that description into a signed, immutable,
A/B-updatable disk image on top of a pinned Linux base, boot-tests and security-scans it, lets the
customer download it (USB image, ISO, VM image) or publish it, installs it on bare metal, in VMs or beside
Windows, and then keeps every installed device updated, attested and managed from a fleet console.

## Why B2B first

Organizations with fleets (kiosks, labs, front-office PCs, thin clients) already pay for imaging, MDM and
patching. They choose their hardware (shrinking the driver problem), accept curated options (shrinking the
combinatorial test problem) and pay monthly per device (funding updates and security). Consumer "build and
share your own OS" becomes a community/marketing layer in Phase 6, on top of the same pipeline.

## Phases

| Phase | Name | Duration *(estimate)* | Goal | Exit gate (summary) |
|---|---|---|---|---|
| 0 | Foundations | 4 weeks | Repo, CI with KVM, threat model, key design, boot spike | Hand-written mkosi Debian 13 image boots with Secure Boot + TPM in QEMU CI |
| 1 | MVP image pipeline | 8 weeks | Spec → signed bootable image, one base, one worker, simple form | 3 pilot specs build reproducibly and boot on 2 real devices |
| 2 | Builder & customization | 10 weeks | Full GUI, catalog + SAT, branding, installer, dual boot, live preview | Non-technical pilot user builds & installs an OS unaided in < 30 min |
| 3 | Security & trust | 10 weeks (starts week 8 in parallel) | SBOM, scans, diff, risk score, review console, per-tenant HSM keys, double-build | Red-team test cases blocked; every image signed by tenant key from HSM |
| 4 | Updates & fleet | 12 weeks | A/B updates, TUF, delta chunks, staged rollout, device agent, attestation | 100-device pilot fleet updated 3 times with auto-rollback proven |
| 5 | Ecosystem services | 10 weeks | Device sign-in (OIDC), curated app store, sync, policies, kiosk mgmt | Pilot customer runs a fleet with SSO, app store and sync |
| 6 | Scale & extensibility | 16 weeks | Multi-base, arm64, sysext module SDK, Expert mode, publishing, AI assist | 2 bases × 2 arches in matrix; third-party module published |
| 7 | Enterprise hardening | ongoing | SOC 2, multi-region, on-prem build appliance, CIS profiles, SLAs | Audit passed; DR drill meets RPO/RTO |

Phase 3 overlaps Phase 2 (a dedicated security engineer). Phase 5 can overlap the end of Phase 4.
Total to end of Phase 6: roughly 16–18 months with the team below.

## Phase dependency graph

```
P0 ──► P1 ──► P2 ──► P4 ──► P5 ──► P6 ──► P7
        └───► P3 ───┘ (P3 must pass before any public publishing or fleet GA)
```

## Team plan *(estimate)*

| Role | P0–P1 | P2–P3 | P4–P5 | P6+ |
|---|---|---|---|---|
| Tech lead / Linux image engineer (boot, mkosi, kernel) | 1 | 1 | 1 | 2 |
| Backend/infra engineer (Go, queue, storage, k8s) | 1 | 2 | 2 | 3 |
| Frontend engineer (React, UX) | 0.5 | 1 | 1 | 2 |
| Security engineer | 0.5 | 1 | 1 | 1 |
| QA / hardware-lab engineer | 0 | 0.5 | 1 | 1 |
| Product designer | 0.5 | 1 | 0.5 | 1 |
| Support / review ops | 0 | 0 | 1 | 2 |

## Cross-cutting tracks (run in every phase)

- **Security**: threat model updated per phase (`context/security-threat-model.md`), security review on every PR touching security paths.
- **Quality**: boot matrix in CI grows every phase (`context/sdlc-and-quality.md`).
- **Hardware lab**: starts Phase 1 with 2 devices, 10 by Phase 4, 25+ by Phase 6; remote power + KVM-over-IP.
- **Docs**: customer docs written with each feature; internal runbooks with each service.
- **Customer loop**: weekly call with each pilot customer; every phase ends with a pilot review.

## KPIs per phase

| Phase | KPI | Target |
|---|---|---|
| 1 | Build success rate for valid specs | ≥ 95% |
| 1 | Reproducibility (same spec → same usr hash) | 100% of pilot specs |
| 2 | Time for new user to first bootable OS | < 30 min |
| 2 | p95 build time (warm cache) | < 12 min |
| 3 | Known-bad test corpus blocked | 100% |
| 4 | Update success rate across pilot fleet | ≥ 99.5% |
| 4 | Failed updates auto-rolled back without human help | 100% |
| 5 | Devices managed per support engineer | ≥ 2,000 |
| 6 | Build cost per build *(estimate)* | < $0.10 compute |

## Risk register (top 10)

| # | Risk | Likelihood | Impact | Mitigation | Owner |
|---|---|---|---|---|---|
| R1 | Hardware drivers/firmware break on customer devices | High | High | Customer-chosen hardware list, HCL from agent inventory, hardware lab, kernel/firmware package pinning per profile | Tech lead |
| R2 | Malicious or vulnerable images published | Medium | Critical | Phase 3 pipeline before any publishing; trust tiers; takedown & revocation | Security |
| R3 | Signing key compromise | Low | Critical | HSM, per-tenant keys, offline root, TUF threshold roles, rotation runbooks | Security |
| R4 | Upstream tool changes (mkosi/systemd) break compiler | Medium | Medium | Pin versions, golden tests, scheduled upgrade windows | Tech lead |
| R5 | Secure Boot friction on arbitrary PCs (MOK enrollment) | High | Medium | B2B: enroll customer keys in setup mode; consumer: shim+MOK guided flow | Tech lead |
| R6 | BitLocker recovery triggered during dual-boot install | Medium | High | Detect BitLocker, force recovery-key confirmation, never touch Windows partitions | Tech lead |
| R7 | Bandwidth/storage costs grow with popularity | Medium | Medium | Zero-egress object storage, delta updates, retention policy | Infra |
| R8 | GPL/licensing non-compliance | Medium | High | Source archive per release, license report from SBOM, legal review | Product |
| R9 | Scope creep toward "build anything" | High | Medium | Curated catalog; Expert features gated behind review | Product |
| R10 | Small team burnout from on-call | Medium | Medium | SLO-based alerting only, runbooks, follow-the-sun later | Eng manager |

## Definition of "production grade" used in this plan

- Every published image is reproducible, signed by a key held in an HSM, has an SBOM, passed scans and boot tests.
- Every installed device can be updated atomically, rolls back automatically on failed boot and can prove its state via TPM attestation.
- Platform SLOs are defined, measured and alerted on (see `context/sdlc-and-quality.md`).
- Backups are tested by restore drills; DR targets are RPO 15 min, RTO 4 h.
