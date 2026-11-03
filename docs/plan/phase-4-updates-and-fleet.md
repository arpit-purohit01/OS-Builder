# Phase 4 — Updates & fleet (12 weeks)

## Goal
Every installed device receives signed, atomic A/B updates with automatic rollback, downloads only the
changed chunks, reports health, and can prove via TPM attestation that it runs an approved release.
Customers manage their fleet (groups, channels, rollouts, policies) from a console.

## Tasks

| ID | Task | Detail | Design doc |
|---|---|---|---|
| P4-01 | Release model | Release = (project, version semver, usr image digest, UKI digest, verity root hash, expected PCR 11 values, SBOM, provenance, tier). Channels: `stable`, `beta`, `canary`, custom | update-system.md |
| P4-02 | TUF repository | Per-tenant TUF repo: root (offline, threshold 2-of-3), targets (HSM), snapshot + timestamp (online, short expiry: timestamp 1 day, snapshot 7 days, targets 90 days, root 1 year) | update-system.md §TUF |
| P4-03 | Chunk store | FastCDC (min 16 KiB / avg 64 KiB / max 256 KiB) over the uncompressed usr partition image; chunks zstd-compressed, content-addressed; chunk index file per release (`.cidx`) | algorithms-and-data-structures.md §FastCDC |
| P4-04 | Delta planner | Device sends current release; server returns index; agent computes missing chunks = new index − local chunks (seeded from current usr partition), fetches with range-coalescing, reconstructs image, verifies SHA-256 and verity root hash | update-system.md §Delta |
| P4-05 | sysupdate integration | Transfer definitions for usr partition, usr-verity, usr-verity-sig, UKI; source is local staging dir (`regular-file`) populated by agent; `systemd-sysupdate update` via D-Bus (`systemd-sysupdated`) or exec | update-system.md §sysupdate |
| P4-06 | Health & rollback | Boot counting (`+3`), `boot-complete.target` requires `foundry-health.service` (checks: graphical/kiosk session up, network, agent heartbeat). Failure → systemd-boot falls back; agent reports `rolled_back` | update-system.md §Health |
| P4-07 | Staged rollout | Deterministic bucket `H(device_id‖release_id) mod 10000`; stages 1→5→25→100%; gates use Wilson lower bound on success rate; auto-pause on gate failure | algorithms-and-data-structures.md §Rollout |
| P4-08 | Maintenance windows | Policy: download any time, install in window (e.g. 02:00–05:00 local), defer up to N days, force after deadline; kiosks reboot only when idle (logind idle hint) | fleet-and-device-agent.md §Policy |
| P4-09 | Device agent | `cmd/foundry-agent`: enrollment, mTLS with TPM-resident key, heartbeat (15 min ± 20% jitter), policy apply, update orchestration, inventory, logs on demand; MemoryMax=64M | fleet-and-device-agent.md |
| P4-10 | Enrollment | Token (org + group, single/multi-use, expiry) entered in installer or via QR/USB file; CSR from TPM key; server issues device cert (90 days, auto-renew at 2/3 life) | fleet-and-device-agent.md §Enrollment |
| P4-11 | Attestation | EK cert check, AK creation + MakeCredential/ActivateCredential, quotes over PCR 7,11 with server nonce, compare to release expected values; status `attested`/`mismatch`/`unknown` | fleet-and-device-agent.md §Attestation |
| P4-12 | Fleet console | Devices table (keyset pagination, filters), groups, channels, rollout control, device detail (hardware, release, health timeline), actions (reboot, update now, collect logs, lock, wipe) | builder-web-ui skill |
| P4-13 | fleetsrv scaling | Heartbeats UPSERT into `devices`, append to `device_reports` (daily partitions, 30-day retention, hourly rollups); write path batched (COPY every 1 s or 5k rows) | data-model.md |
| P4-14 | Firmware updates | `fwupd` with LVFS metadata mirrored; policy controls whether firmware updates apply automatically | boot-firmware-and-disk.md §Firmware |
| P4-15 | Offline/air-gapped | Export bundle (TUF metadata + chunks) to USB/LAN cache; `foundry-cache` server mode for sites with poor internet | update-system.md §Offline |
| P4-16 | Remote wipe/lock | Wipe = rotate LUKS keys (destroy keyslots, TPM token) then reboot into installer-reset; lock = switch to lock-screen-only session; both signed commands with expiry | fleet-and-device-agent.md §Commands |

## Acceptance tests
1. 100 QEMU devices + 20 lab devices: update v1→v2→v3; ≥ 99.5% success; all failures rolled back automatically.
2. Fault injection: power cut during download, during write to slot B, during first boot of B; device always boots a good slot.
3. Delta efficiency: security-only update downloads ≤ 15% of full image *(target, measure)*.
4. Compromise drill: stolen online snapshot/timestamp key cannot install a malicious target (targets signature fails); freeze attack detected by timestamp expiry.
5. Attestation flags a device booted with a self-signed UKI.
6. fleetsrv load test: 100k simulated devices heartbeating, p99 < 300 ms, DB CPU < 60%.

## Exit gate
Pilot fleet (≥ 100 real devices across customers) updated 3 times with zero bricked devices.
