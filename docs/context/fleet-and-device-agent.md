# Fleet management and device agent

## 1. Agent (`cmd/foundry-agent`)
- Static Go binary in `/usr/lib/foundry/foundry-agent`, unit `foundry-agent.service` (Type=notify, WatchdogSec=60, Restart=always).
- Runs as `foundry-agent` user with `AmbientCapabilities=` none; privileged operations go through D-Bus to systemd (sysupdated, logind, NetworkManager) authorized by a polkit rule scoped to that user, plus a tiny root helper `foundry-helper` (socket-activated, fixed verb set: `apply-policy`, `wipe`, `lock`, `collect-logs`) with argument validation.
- Hardening: `ProtectSystem=strict`, `ReadWritePaths=/var/lib/foundry`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, `MemoryMax=64M`, `CPUQuota=20%`, `IPAddressDeny=any` + `IPAddressAllow=` resolved fleet/CDN ranges (optional on networks with proxies).

Internal modules:
```
enroll/     token parse, TPM key creation, CSR, credential activation
transport/  HTTP/2 mTLS client (TPM-backed signer via go-tpm keyfiles), retries w/ full jitter
heartbeat/  15 min ± 20% timer, state hash to skip unchanged payloads
inventory/  DMI, CPU, RAM, disks (SMART summary), PCI/USB IDs + bound drivers, battery, firmware (fwupd), secure boot state, TPM info
policy/     verify signature, diff vs applied, render to backends (dconf, plasma kiosk, polkit, nftables, usbguard, browser policies, flatpak)
update/     TUF client, delta planner, staging, sysupdate D-Bus, health reporting
attest/     AK management, quotes
commands/   signed command queue: reboot, update-now, collect-logs, lock, wipe, set-url (kiosk), screenshot (opt-in)
store/      /var/lib/foundry/state.db (bbolt), ring-buffer logs
```

## 2. Enrollment protocol
```
Admin creates token: {org, group, uses, expires} → string "FDY1-<base32(id)>-<base32(secret)>" (secret 160 bits, stored hashed with argon2id)
Device (installer or agent):
 1. TPM: read EK cert (NV index 0x01c00002 RSA / 0x01c0000a ECC) if present
 2. Create AK (restricted signing key) and device TLS key (non-restricted ECC P-256) under owner hierarchy; persist handles/keyfiles
 3. POST /v1/enroll {token, ek_cert|ek_pub, ak_pub, csr(tls key), inventory_min}
fleetsrv:
 4. Validate token (constant-time compare of hash), uses, expiry
 5. Verify EK cert chain against TPM vendor CA bundle (if cert present; else mark "ek_unverified")
 6. MakeCredential(ek_pub, ak_name, secret) → challenge
Device:
 7. ActivateCredential → secret' ; POST /v1/enroll/activate {secret'}
fleetsrv:
 8. Compare; issue device cert (90 days, SAN = URI spiffe://<tenant>/device/<device_id>) signed by tenant device CA
 9. Return {device_id, cert, ca_bundle, policy, channel}
```
Renewal at 2/3 lifetime with proof of possession of current key + fresh quote.

## 3. Heartbeat
Request (CBOR or JSON+gzip, ≤ 8 KiB typical):
```json
{"device_id":"d_01J...","seq":1832,"ts":"2026-10-28T09:01:00Z","state_hash":"sha256:..",
 "release":{"current":"1.4.0","booted_slot":"A","pending":"1.5.0","stage":"staged"},
 "health":{"boot_ok":true,"failed_units":[],"uptime_s":86400},
 "security":{"sb":"on","tpm":"2.0","luks":"tpm2","verity":true,"mutable_usr":false},
 "mem":{"avail_mb":2210,"psi_full_avg60":0.4,"zram_ratio":3.1,"oom_kills":0},
 "disk":{"root_free_gb":41.2,"smart":"ok"},
 "net":{"type":"wifi","metered":false}}
```
Response: `{policy_version, commands:[...], next_heartbeat_s, update_hint}`. If `state_hash` unchanged server skips the `devices` row update (only touches `last_seen` in a cheap separate table/column).

## 4. Policy model
Signed JSON document (tenant policy key), versioned, assigned per group with inheritance (org → group → device overrides), merged by JSON Merge Patch in order.
```json
{"version":12,
 "updates":{"channel":"stable","window":{"start":"02:00","end":"05:00","tz":"device"},"deferDays":3,"deadlineDays":7,"firmware":"manual"},
 "apps":{"required":["org.mozilla.firefox"],"blocked":["com.valvesoftware.Steam"]},
 "usb":{"storage":"read-only"},
 "network":{"wifi":[{"ssid":"Corp","security":"wpa3-eap","eapRef":"cert:corp-wifi"}],"firewall":{"allowInbound":[]}},
 "desktop":{"lockWallpaper":true,"screenLockMinutes":10,"disableTerminal":true},
 "kiosk":{"url":"https://kiosk.example.com","idleResetMinutes":5,"rebootAt":"03:30"},
 "browser":{"homepage":"https://intranet","extensionsAllowlist":[]}}
```
Rendering is idempotent; each backend reports `applied|failed(reason)`; console shows per-device policy compliance.

## 5. Attestation
- Server sends nonce (32 bytes) in heartbeat response every 24 h or on demand.
- Agent `TPM2_Quote(AK, PCR selection sha256:{7,11}, nonce)` + PCR values + event log (`/sys/kernel/security/tpm0/binary_bios_measurements`).
- Server: verify quote signature with AK, nonce, recompute PCR 7 from event log and compare with expected SB state (tenant db cert present, SB enabled); compare PCR 11 with release's predicted values for the booted release. Status: `attested`, `mismatch(pcr)`, `unknown(no tpm)`.
- Mismatch → alert, optional policy: quarantine (network restricted via policy), require reinstall.

## 6. Commands
Each command: `{id, type, args, issued_at, expires_at, issuer, signature}`. Agent verifies signature with fleet command key, expiry, and idempotency (`id` recorded). Destructive commands (`wipe`) also require a fresh admin WebAuthn assertion recorded server-side.
- `wipe`: helper runs `systemd-cryptenroll --wipe-slot=all` style key destruction (or `cryptsetup luksErase`), then reboots into factory reset (`systemd-repart` `FactoryReset=yes` partitions recreated). Data unrecoverable once keyslots are destroyed.
- `lock`: switch to lock session showing org message; unlock with admin-issued code.

## 7. Hardware compatibility list (HCL)
- Inventory produces a hardware fingerprint: DMI `sys_vendor`, `product_name`, `product_version`, `board_name`, BIOS version; PCI IDs (vendor:device, subsystem) and bound driver; USB IDs; missing-firmware messages from kernel log (`Direct firmware load for ... failed`).
- HCL table aggregates per (model, release): boot success, suspend/resume success (logind events), Wi-Fi up, GPU accel, audio devices, missing firmware → status `certified | works | issues | unsupported`.
- Builder shows HCL status for `hardware.targets` and suggests firmware packages.

## 8. Identity on devices (Phase 5)
- AD/LDAP: SSSD + realmd join at enrollment (credentials via one-time join token from fleet).
- OIDC: PAM module performing device-code or password grant against IdP with offline cache of verified credentials (salted, slow hash) for N days; greeter integration; Kerberos optional.
- Local accounts: systemd-homed (LUKS home, portable) optional.

## 9. Scale numbers *(estimate)*
100k devices × 4 heartbeats/h ≈ 111 req/s average; with jitter peaks ~3×. Each heartbeat ≈ 1 small UPSERT (often skipped) + 1 appended row (batched COPY). Single Postgres primary handles this comfortably; partition `device_reports` daily, retain 30 days raw, hourly rollups 13 months.
