# Phase 3 — Security & trust (10 weeks, starts in parallel at Phase 2 week 3)

## Goal
No image leaves the platform (download, publish, update) unless it is reproducible, scanned, diffed,
risk-scored, approved (automatically or by a human) and signed with the tenant's own keys held in an HSM.
Make the common malware tactics fail early and make every image's contents visible.

## Tasks

| ID | Task | Detail | Design doc |
|---|---|---|---|
| P3-01 | HSM + signer v1 | PKCS#11 provider (cloud HSM/KMS or YubiHSM2 to start). Per-tenant non-exportable keys generated in HSM: `sb-db` (RSA-2048), `uki-pcr-initrd` + `uki-pcr-main` (RSA-2048, systemd-measure PCR policies; only the initrd key unlocks disks), `verity` (for usr-verity-sig), `tuf-targets` (Ed25519), `device-ca` (ECDSA P-256). Signer authorizes only builds in state `approved` | security-threat-model.md §Keys |
| P3-02 | Key ceremony | Offline platform root (air-gapped laptop, HSM backup with Shamir 3-of-5), signing of intermediate & TUF root metadata, recorded and witnessed | platform-operations skill |
| P3-03 | Scanner service | Stages: SBOM (syft) → CVE (grype, DB mirrored hourly) → malware (ClamAV + YARA rules) → script lint (shellcheck + custom semgrep rules for `curl|sh`, base64-exec, `/dev/tcp`, crontab writes, `authorized_keys`, `LD_PRELOAD`, `chattr +i`, disabling AppArmor/firewall) → tree diff vs. base → behavior run | security-review-pipeline skill |
| P3-04 | Tree diff | Manifest of every file (path, type, mode, uid, gid, size, sha256, xattrs, capabilities) for base and built image; sorted merge-join diff O(n); classify each change by path trie rules (sensitive prefixes) | algorithms-and-data-structures.md §Tree diff |
| P3-05 | Behavior sandbox | Boot image in isolated VM, network to a sinkhole with DNS logging + Suricata; run 10 min idle + scripted session; record DNS names, connections, new listening sockets (`ss -lntup` via serial agent), processes started outside known units | security-review-pipeline skill |
| P3-06 | Risk scoring | Weighted feature model (hard blockers + score), thresholds, explanations; feature table versioned | security-threat-model.md §Risk |
| P3-07 | Review console | Queue sorted by risk & SLA; build detail with diff tree, flagged findings, network log, SBOM, script viewer with highlights; actions approve / reject / request changes; two-person rule for score > 60 | security-review-pipeline skill |
| P3-08 | Trust tiers | `verified` (catalog-only, double-build match, score < 20), `reviewed` (human approved), `private-unverified` (org-only, never publishable). Tier shown on download page and stamped into `/usr/lib/foundry/trust.json` + release manifest | security-threat-model.md §Tiers |
| P3-09 | Double-build | Verified tier requires two independent builds on different hosts → identical usr verity root hash | build-pipeline.md §Reproducibility |
| P3-10 | Build provenance | SLSA v1 provenance (in-toto attestation) for each image: builder ID, spec digest, catalog version, base snapshot, tool versions, VM image digest; signed by platform key | security-threat-model.md §Supply chain |
| P3-11 | GPL source archive | For each release: collect source packages (`apt-get source` equivalent from snapshot) for all binary packages in SBOM, store as release artifact; license report from SBOM | security-threat-model.md §Compliance |
| P3-12 | Platform hardening | Separate signer network segment, mTLS between services (SPIFFE IDs), secrets in a vault, admin actions require WebAuthn, audit log append-only (hash-chained) | security-threat-model.md |
| P3-13 | Abuse & takedown | Report endpoint, takedown workflow (< 1 h SLO), revoke release in TUF targets, mark tier `revoked`, notify installed devices via fleet | update-system.md §Revocation |
| P3-14 | Pentest | External pentest of API, upload paths, build sandbox escape, signer authorization | — |

## Acceptance tests (red-team corpus in `test/redteam/`)
Each case must be blocked or flagged ≥ review threshold:
1. postinst writes SSH key to `/root/.ssh/authorized_keys`.
2. Hidden systemd unit in `/etc/systemd/system/multi-user.target.wants/` calling a reverse shell.
3. Base64-encoded payload decoded and executed at boot.
4. Modified `/usr/bin/sudo` (hash differs from package).
5. Package from a non-allowlisted repository.
6. Overlay with setuid binary.
7. Kernel cmdline adds `init=/bin/sh` or disables `lockdown`.
8. EICAR + YARA test strings inside an app bundle.
9. DNS beacon to an unknown domain during behavior run.
10. Build VM tries to reach the internet (blocked by proxy and logged).
11. Signing request for a non-approved build is refused (signer unit + integration test).

## Exit gate
All red-team cases pass; pentest critical/high findings fixed; every release in the system signed by tenant HSM keys; takedown drill completed in < 1 h.
