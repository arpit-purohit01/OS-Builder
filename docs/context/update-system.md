# Update system

## 1. Model
- A **release** = immutable tuple (project, version, arch, usr digest, verity digest, verity-sig digest, UKI digest, roothash, expected PCR 11 set, SBOM, provenance, tier).
- A **channel** (`stable`, `beta`, `canary`, custom) points to a release and a **rollout** (percent + gates).
- Devices belong to groups; group → channel mapping in policy.

## 2. Device update sequence
```
agent timer (every 4 h ± jitter, or push hint via heartbeat response)
 1. GET timestamp.json  → verify (TUF client), snapshot.json, targets.json for the tenant repo
 2. pick release for my channel; check bucket < rollout%; check policy window/deferral
 3. GET <release>.cidx (verify sha256 from targets custom metadata)
 4. plan delta (algorithms §7); download missing chunk ranges → staging file in /var/lib/foundry/staging
 5. verify usr sha256 + verity root hash; fetch verity, verity-sig, UKI (small; full download)
 6. systemd-sysupdate --definitions=/usr/lib/sysupdate.d update <version>   (source: staging dir)
      → writes inactive usr slot partitions (label = aurora_<ver>), writes UKI as aurora_<ver>+3-0.efi (named after the first MatchPattern)
 7. report "staged"; wait for maintenance window / idle
 8. reboot → boot counting → foundry-health → bless or fallback
 9. report "succeeded" | "rolled_back" (+ journal of failed boot)
10. cleanup: sysupdate keeps InstancesMax=2 (current + previous)
```

## 3. systemd-sysupdate transfer definitions (installed in /usr/lib/sysupdate.d/)
```ini
# 50-usr.transfer
[Transfer]
ProtectVersion=%A
[Source]
Type=regular-file
Path=/var/lib/foundry/staging
MatchPattern=aurora_@v_@u.usr.raw
[Target]
Type=partition
Path=auto
MatchPattern=aurora_@v
MatchPartitionType=usr
PartitionFlags=0
ReadOnly=1

# 51-usr-verity.transfer  (MatchPartitionType=usr-verity, MatchPattern=aurora_@v_verity)
# 52-usr-verity-sig.transfer (MatchPartitionType=usr-verity-sig, MatchPattern=aurora_@v_verity_sig)

# 60-uki.transfer
[Source]
Type=regular-file
Path=/var/lib/foundry/staging
MatchPattern=aurora_@v.efi
[Target]
Type=regular-file
Path=/EFI/Linux
PathRelativeTo=boot
MatchPattern=aurora_@v+@l-@d.efi aurora_@v+@l.efi aurora_@v.efi
TriesLeft=3
TriesDone=0
InstancesMax=2
```
(Verify option names/specifiers against the pinned systemd version; `@u` = partition UUID, `@l`/`@d` = tries left/done.)

## 4. TUF repository layout (per tenant, consistent snapshots)
```
metadata/
  1.root.json, 2.root.json …      (offline root, threshold 2-of-3 — includes platform co-signature)
  timestamp.json                  (expiry 1 day, online key)
  <n>.snapshot.json               (expiry 7 days)
  <n>.targets.json                (expiry 90 days, HSM key)
  <n>.<channel>.json              (delegated role per channel, optional; lets channel managers sign w/o full targets)
targets/
  <sha256>.<name>                 (consistent snapshot naming; actually served from sha256/ CAS)
```
Target custom metadata carries: release id, version, arch, roothash, PCR 11 predictions digest, min agent version, rollout id.

Device checks: signature thresholds, version monotonicity (rollback protection), expiry (freeze protection), length + hash before use (endless-data protection: max sizes in targets).

## 5. Delta efficiency
- Chunked usr image (FastCDC, algorithms §6). Packs of chunks served via CDN with Range.
- Expected: small security update touches a few packages → a few % of chunks *(measure per release; KPI ≤ 15%)*.
- Fallback: if planned download > 70% of full image, download the full zstd image (simpler, fewer requests).

## 6. Health checks (foundry-health.service)
```ini
[Unit]
Description=Foundry boot health check
Before=boot-complete.target
After=graphical.target foundry-agent.service
FailureAction=reboot
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/lib/foundry/health --timeout=180s
[Install]
RequiredBy=boot-complete.target
```
Checks: profile session active (display manager or cage unit active), no failed units in critical list, network reachable OR offline policy permits, agent can verify TUF metadata locally. Failure → exit 1 → `boot-complete.target` is not reached (it requires this unit) → boot not blessed → `FailureAction=reboot` → after 3 failed tries systemd-boot falls back to the previous entry. The health binary runs `journalctl --flush` before exiting non-zero so the agent can upload the failed boot's journal later. (Direction matters: the *target* must require the check; the check requiring the target would let failed boots be blessed.)

## 7. Rollout control (fleetsrv)
- States: `draft → canary(1%) → stage(5%) → stage(25%) → full(100%)`, or `paused`, `halted`.
- Gate evaluation every 10 min (algorithms §12). Automatic pause on gate failure; operator can resume/halt.
- Halt = remove release from channel in TUF (new targets version) → devices not yet updated stop; updated devices stay unless "roll back fleet" which targets the previous release with `force_downgrade` metadata flag (agent permits downgrade only when signed metadata says so).

## 8. Revocation
- Release revoked → targets updated (remove), plus `revoked_releases` list in custom metadata; agents on a revoked release show admin banner and update to latest allowed.
- Signing key revoked → new root metadata rotates role keys; for SB db compromise, dbx entry via fwupd + re-sign all current releases with new db key (requires KEK).

## 9. Offline and constrained networks
- `fctl bundle export --release X` → tarball with TUF metadata + full chunk packs; import on device (`foundry-agent import /media/usb/bundle`) or on a site cache.
- Site cache: `foundry-cache` (same binary as updatesrv in pull-through mode) on a local machine; devices discover via DHCP option / DNS-SD `_foundry-cache._tcp`; TUF makes the cache untrusted.
- Bandwidth controls: policy `maxDownloadKbps`, metered network detection (NetworkManager `Metered` property) → defer.

## 10. Agent self-update
The agent ships inside `/usr`, so it updates with the OS. Emergency agent fixes ship as a signed sysext module through the same TUF repo.

## Verified in Phase 0 (systemd 255)
- `systemd-sysupdate` (in `/usr/lib/systemd/`, not on `PATH`) needs `/usr/lib/systemd/systemd-import` from the **systemd-container** package; the compiler adds it to every image.
- With `--definitions=DIR`, systemd 255 reads only `*.conf` files (the default search paths use `*.transfer`).
- Source patterns must include `@u` (`<id>_@v_@u.usr.raw`) so the B-slot partitions get root-hash-derived UUIDs; otherwise `usrhash=` cannot find the new `/usr`.
- An update installs the UKI as `<id>_<ver>+3-0.efi`; systemd-boot boots it as a counted trial; three failed health checks fall back to the previous entry (acceptance test `ab-fallback`).
