---
name: security-review-pipeline
description: Use when working on cmd/scanner, cmd/signer, internal/risk, the review console, trust tiers, upload validation, SBOM/CVE/malware/YARA/script rules, tree diffs, behavior sandbox, provenance, takedowns or anything about stopping malicious or vulnerable OS images.
---

# Security review pipeline

Read first: `docs/context/security-threat-model.md` (all), `docs/context/algorithms-and-data-structures.md` §8–11, §17, `docs/plan/phase-3-security-and-trust.md`.

## Pipeline order (scanner)
1. SBOM (syft, SPDX JSON) on the mounted rootfs (read-only, via `systemd-dissect --mount`).
2. CVE match (grype against mirrored DB; record DB version on the scan row).
3. Malware: ClamAV + YARA (rules in `scanner/rules/yara/`, versioned).
4. Script analysis: collect all executable scripts, systemd units, cron/timer files, udev rules, postinst content → shellcheck + semgrep rules (`scanner/rules/semgrep/`) + Aho–Corasick token scan.
5. Tree manifest + diff vs base image of the same base/snapshot/profile → classify with path trie; verify files against package checksums.
6. Behavior run (isolated VM, sinkhole network, 10 min) → DNS/connection log, listening sockets, process list.
7. Risk score + tier decision → `findings` rows with weights and explanations.

## Adding a detection rule
1. Add a red-team fixture in `test/redteam/<case>/` (spec + overlay) that the rule must catch, and a benign fixture that must **not** trigger it.
2. Implement the rule (YARA, semgrep, trie path rule, or Go feature in `internal/risk/features/`). Give it a stable `rule_id`, severity, weight, and a one-line explanation shown to reviewers.
3. Run `make redteam` — all cases blocked/flagged, benign corpus false-positive rate unchanged.
4. Bump the risk model version; document the weight in `security-threat-model.md` §6.

## Signer authorization (never weaken)
- Signer accepts `SignRequest{build_id, purpose, digest}` only if: build state `finalizing`; digest equals the DB-recorded artifact digest for that purpose; tenant key for purpose exists and is not revoked; caller SPIFFE ID is `finalize`.
- Every signature is logged to the audit log with request hash and HSM operation id.
- Tests: wrong state, wrong digest, wrong tenant, revoked key, wrong caller → all refused.

## Review console rules
- Show: risk breakdown (top features), diff tree filtered to flagged paths, script viewer with highlights, network log, SBOM, CVEs, provenance.
- Score > 60 requires two different reviewers; reviewers cannot review builds from their own org's members unless platform reviewers are disabled for that org (business plans with self-review).
- Accepting a finding requires a reason (stored on the finding).

## Upload validator rules (P2-08)
Reject: absolute paths, `..` segments, symlinks/hardlinks resolving outside root, device/FIFO/socket nodes, setuid/setgid (unless expert+review), control chars, invalid UTF-8, > 255-byte components, > 4096-byte paths; caps 100 MiB/file, 2 GiB total, 200k entries, compression ratio 100:1 (stop streaming when exceeded). Fuzz it.
