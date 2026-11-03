---
name: update-and-rollout
description: Use when working on cmd/updatesrv, internal/tuf, internal/chunker, releases, channels, staged rollouts, delta updates, systemd-sysupdate transfer files, health checks, rollback, revocation or offline update bundles.
---

# Updates and rollouts

Read first: `docs/context/update-system.md`, `docs/context/algorithms-and-data-structures.md` §6, §7, §12, §13, `docs/context/boot-firmware-and-disk.md` §7.

## Invariants
- Devices install nothing that isn't referenced by verified TUF targets metadata (signature threshold, expiry, version monotonic, length + hash).
- Downgrades only when signed metadata explicitly allows (`force_downgrade` in target custom metadata).
- The booted slot is never written. sysupdate `ProtectVersion=%A`.
- A release is immutable; fixing a release = new release.

## Chunk index format (`.cidx` v1)
```
magic "FCIDX1\0\0" | u32 version=1 | u32 flags | u64 total_size | [32]byte image_sha256 |
u32 min | u32 avg | u32 max | u64 count | entries[count] { u64 offset; u32 length; [32]byte sha256 } (sorted by offset)
```
Little-endian. Parser must check: count × 44 bytes == remaining length, offsets contiguous and ascending, Σ length == total_size, every length within [min, max] except the last. Fuzz the parser.

## Changing the chunker
GEAR table and parameters are part of the format; any change → new `.cidx` version, server serves both during transition, agent supports N and N−1.

## Rollout logic checks
- Bucket function: `uint64(sha256(device_id ‖ release_id)[0:8]) mod 10000` — keep byte order big-endian, test vector in `internal/sched/rollout_test.go`.
- Gates: Wilson lower bound z = 2.326, `min_devices` ≥ 50 per stage, `min_hours` soak; rollback rate gate separate. Auto-pause writes an audit entry and fires `rollout.paused` webhook.

## Testing
- `make boot-test MATRIX=uefi,sb,tpm SCENARIO=update-ab` (v1→v2), `SCENARIO=rollback` (v2 health fails), `SCENARIO=powercut` (kill QEMU at random byte offsets during sysupdate write; 50 iterations; must always boot a good slot).
- TUF attack tests: expired timestamp (freeze), replayed old snapshot (rollback), targets signed with snapshot key (wrong role), oversized target (endless data), mismatched hash.
- Delta test: v1→v2 with one package changed; assert downloaded bytes ≤ threshold and reconstructed sha256 equals release.

## Pitfalls
- sysupdate matches by partition **label** and type — the label pattern must agree across repart definitions, transfer files and build output (`%M_%A`, `aurora_@v`).
- `InstancesMax=2` keeps current + one previous; never lower it to 1 (no rollback target).
- After writing a new UKI, ensure its `.pcrsig` was produced for the same sections, or TPM unlock breaks on next boot.
