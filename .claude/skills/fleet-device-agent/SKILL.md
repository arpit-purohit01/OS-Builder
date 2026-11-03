---
name: fleet-device-agent
description: Use when working on cmd/foundry-agent, cmd/fleetsrv, internal/attest, enrollment, heartbeats, device policies, remote commands (lock, wipe, reboot), TPM attestation, inventory/HCL, or the fleet console back end.
---

# Fleet and device agent

Read first: `docs/context/fleet-and-device-agent.md`, `docs/context/memory-management.md` §A4 and §B7, `docs/context/data-model.md`, `docs/context/api.md` (device-facing section).

## Agent budgets (CI-enforced)
- RSS < 30 MiB steady (test: run agent 1 h in QEMU with synthetic load; sample `/proc/<pid>/status` VmRSS).
- CPU < 1% average; no busy loops; timers with jitter.
- Binary: static (`CGO_ENABLED=0`), `-trimpath`, stripped; size < 25 MiB.
- Offline for 30 days: queue reports (bounded, drop oldest with counter), keep last policy, keep working.

## Code rules
- All privileged actions through D-Bus (systemd, logind, NetworkManager, sysupdated) or the fixed-verb `foundry-helper`; never `exec` shell strings.
- Every server message (policy, command) verified: signature, expiry, monotonic version / unique id.
- Persistent state in `/var/lib/foundry/state.db` (bbolt) with schema version; migrations on startup.
- Network: HTTP/2 with mTLS, connection reuse, timeouts on everything (dial 10 s, TLS 10 s, request 30 s, downloads use per-chunk deadlines), retries with full jitter.

## Enrollment/attestation testing
- Use swtpm in QEMU; tests for: EK cert present/absent, token reuse/expired, credential activation failure, cert renewal, quote with wrong nonce, PCR 11 mismatch after booting an unexpected UKI.

## fleetsrv rules
- Heartbeat handler: authenticate cert → lookup device by fingerprint (cached in L0 with 60 s TTL) → compare `state_hash` → batched writes. p99 < 300 ms at 100k devices.
- Never return another org's data; device identity comes from the mTLS cert, never from the request body.
- Commands: insert row, deliver on next heartbeat (or push hint), mark delivered, accept result once.

## Policy backends checklist (when adding a policy key)
schema in `internal/policy/schema.json` → merge semantics (org→group→device) → renderer per backend (dconf lock, Plasma `[$i]`, polkit, nftables, usbguard, browser JSON, flatpak) → idempotency test (apply twice = no change) → compliance reporting field → console UI.
