# Security and threat model

## 1. Assets
| Asset | Why it matters |
|---|---|
| Tenant signing keys (SB db, UKI PCR, verity, TUF targets, device CA) | Whoever holds them can make any device boot/run anything |
| Platform root keys (TUF root, provenance) | Trust anchor for all tenants |
| Build integrity (what goes into images) | Supply-chain compromise reaches every device |
| Device fleet control (commands, policies, wipe) | Mass compromise or mass bricking |
| Customer data (specs, assets, device inventory, recovery keys escrow) | Confidentiality, regulations |
| Availability of update service | Unpatched fleets |

## 2. Actors
Malicious creator (publishes trojaned OS) · compromised customer admin account · external attacker on API · malicious package upstream/mirror · compromised build host · rogue insider · network attacker on device path · stolen device.

## 3. STRIDE by component

| Component | Threat | Mitigation |
|---|---|---|
| api | S: account takeover | OIDC/SSO, WebAuthn for admins, session binding, short-lived tokens (15 min access, rotating refresh) |
| api | T: tamper spec of other tenant | row-level tenancy checks in every repo function (`org_id` mandatory parameter), Postgres RLS as defense-in-depth |
| api | I: data leak via IDOR | opaque IDs (ULID with type prefix), authorization middleware tested by generated tests for every route |
| uploads | T/E: path traversal, zip bombs, symlink escape | streaming validator (P2-08), separate bucket, no server-side extraction outside the build VM |
| build VM | E: sandbox escape to host | full VM isolation, minimal QEMU device model, seccomp sandboxing in QEMU (`-sandbox on`), host kernel patched weekly, VM per build, no shared writable mounts |
| build VM | I: exfiltrate other tenants' data | VM sees only its own job; caches mounted read-only and contain only public packages or same-tenant objects |
| build VM | network abuse (crypto-mining, attacks) | egress allowlist, CPU limits, wall-clock timeouts |
| mirrors | T: malicious package | signed repo metadata (apt GPG), pinned snapshot, sha256 in lockfile, CVE/malware scans |
| signer | E: sign arbitrary content | signer accepts only (build_id, artifact digests) for builds in `approved`; double-checks digests against DB; rate limits; separate network zone; HSM non-exportable keys |
| update path | T: malicious update via CDN | TUF (threshold roles, expiry, consistent snapshots), devices verify before sysupdate |
| update path | D: freeze attack | timestamp expiry 1 day; device alerts if metadata stale > 3 days |
| devices | S: fake device enrolls | single-use tokens, TPM EK/AK credential activation |
| devices | T: offline tampering | verity /usr, signed UKI, TPM-bound LUKS, attestation |
| fleet commands | E: malicious wipe via stolen admin | commands signed by fleetsrv with per-command expiry; destructive commands require WebAuthn re-auth + optional second approver |
| insider | any | least privilege, break-glass with audit, two-person rule for key ceremonies and prod DB writes |

## 4. Keys

| Key | Algorithm | Storage | Rotation | Use |
|---|---|---|---|---|
| Platform TUF root | Ed25519, 3 keys, threshold 2 | offline (air-gapped laptop + HSM backup, Shamir 3-of-5) | 1 year (metadata re-sign), keys on compromise | sign TUF root of platform + tenant roots |
| Provenance signing | ECDSA P-256 | HSM | 1 year | SLSA provenance |
| Tenant SB db | RSA-2048 | HSM per tenant | 3–5 years (cert validity), revoke via dbx | systemd-boot, UKI, kernel modules |
| Tenant UKI-PCR (initrd + main) | RSA-2048 ×2 | HSM | with SB db | `.pcrsig`; only the initrd-phase key is enrolled for disk unlock |
| Tenant verity | RSA-2048/ECDSA | HSM | 2 years | usr-verity-sig, sysext signatures |
| Tenant TUF targets | Ed25519 | HSM | 1 year | releases |
| TUF snapshot/timestamp | Ed25519 | online KMS | 90 days | metadata freshness |
| Device CA | ECDSA P-256 | HSM | intermediate 2 years | device mTLS certs (90 days) |
| Job envelope key | Ed25519 | KMS | 90 days | api → worker-agent |

Ceremony (P3-02): scripted, recorded, two witnesses; outputs: public certs into repo, encrypted backups to two safes; checklist signed.

## 5. Trust tiers
| Tier | Conditions | Allowed to |
|---|---|---|
| verified | catalog-only content, no `expert.*`, double-build reproducible, risk < 20, all scans clean | publish publicly with badge, fleet GA |
| reviewed | human approval (2 reviewers if risk > 60), scans clean or accepted findings documented | publish (badge "Reviewed"), fleet GA |
| private-unverified | anything else that passes hard blockers | download inside org only; fleet "test" groups only |
| revoked | takedown or key compromise | nothing; devices notified |

## 6. Risk scoring (v1 weights; tune with data)

Hard blockers (auto-reject): known-malware hash/YARA high-severity match; signer-relevant tampering (modified bootloader/UKI content outside build); kernel cmdline denylist; unapproved repository; egress to non-allowlisted host during build.

| Feature | Weight |
|---|---|
| New/modified file under critical prefix (sudoers, ld.so.preload, root ssh) | 40 |
| New setuid/setgid or file capability | 25 |
| Binary in /usr differs from owning package checksum | 30 |
| New enabled systemd unit not from a catalog package | 15 |
| Script rule hit: download-and-execute | 30 |
| Script rule hit: obfuscation (base64/xxd + exec) | 25 |
| Behavior: DNS to unknown domain | 20 each (max 40) |
| Behavior: new listening socket not from catalog | 15 |
| Disabled security control (firewall/AppArmor/updates) vs. level | 20 |
| Critical CVE (CVSS ≥ 9, fix available) | 10 each (max 30) |
| Uploaded binaries (ELF) count > 0 | 10 |
| New creator account (< 30 days, < 3 approved builds) | 10 |

Thresholds: < 20 auto-approve (only if no `expert.*`), 20–60 single review, > 60 two-person review.

## 7. Supply chain (our own platform)
- SLSA Build L3 target for platform binaries: hermetic CI on ephemeral runners, provenance signed, reproducible Go builds (`-trimpath`, pinned toolchain).
- Dependencies pinned (go.sum, lockfiles), Renovate PRs, `govulncheck`, `npm audit`, Semgrep, gosec in CI.
- Container images minimal (distroless), signed (cosign), admission policy only runs signed images.
- Build VM golden image built by the same pipeline, digest pinned in worker config.

## 8. Compliance
- GPL/LGPL: per release, corresponding source archive (source packages from the same snapshot) retained ≥ 3 years and offered from the public page; written offer text in `/usr/share/doc/foundry/SOURCES`.
- Trademarks: catalog prohibits upstream logos/names in branding (validator checks `osId`/`osName` against a denylist: debian, ubuntu, fedora, windows, macos, …).
- Privacy: device data minimization; screenshot/remote features opt-in; data retention table in data-model.md.

## 9. Incident response
- Severity levels: SEV1 (key compromise, malicious release live, mass bricking) → page immediately; SEV2 (update service down) → 30 min; SEV3.
- Key compromise playbook: revoke in TUF (new targets key via root), push dbx update for tenant db cert if SB key compromised (requires KEK), rotate device CA intermediate, force re-attestation, customer notification within 24 h.
- Malicious release: yank from targets (TUF), mark revoked, devices on it get banner + forced update to last good, preserve evidence (artifacts, logs) under legal hold.
