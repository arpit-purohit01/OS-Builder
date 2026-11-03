# Boot, firmware and disk layout

## 1. Firmware landscape and decision
| Firmware | Support | Why |
|---|---|---|
| UEFI x86_64 (2012+ PCs, all Windows 11 PCs) | Primary | Secure Boot, measured boot (TPM), GPT, systemd-boot/UKI |
| UEFI aarch64 (servers, some laptops, RPi via EDK2) | Phase 6 | same chain, AAVMF in tests |
| Legacy BIOS (CSM) | Compat profile only (Phase 6) | no Secure Boot/measured boot; GRUB i386-pc |
| UEFI 32-bit on 64-bit CPU (some cheap tablets) | Not supported v1 | needs 32-bit bootloader |

## 2. Boot chain (UEFI, Secure Boot on)

```
Power on
 └─ UEFI firmware (measures itself → PCR 0..7; SB policy → PCR 7)
     └─ [optional] shim (Microsoft-signed, from base distro)  ── verifies via db or MOK
         └─ systemd-boot  (signed by tenant db key or MOK)      ── measured into PCR 4
             └─ UKI: <osId>_<version>+3-0.efi  (signed)            ── PE measured into PCR 4
                 └─ systemd-stub: measures sections (.linux .osrel .cmdline .initrd .uname .sbat) → PCR 11
                     └─ kernel + initrd
                         ├─ systemd-veritysetup-generator: usrhash=<roothash> from (signed) cmdline
                         ├─ dm-verity opens usr partition whose verity data matches roothash → /usr (ro, erofs)
                         ├─ systemd-cryptsetup: root LUKS2 unlocked by TPM2 (signed PCR 11 policy [+PCR 7] [+PIN])
                         ├─ systemd-pcrphase extends PCR 11 at phase boundaries (enter-initrd, leave-initrd, ...)
                         └─ switch-root → systemd (PID 1) → boot-complete.target → systemd-bless-boot
```

Key property: the root hash of `/usr` is inside the signed UKI cmdline, so a signature check on the UKI
transitively authenticates every block of `/usr`.

## 3. Disk layout (GPT, Discoverable Partitions Specification)

| # | Partition | Type (repart) | Size | FS | Created |
|---|---|---|---|---|---|
| 1 | ESP | `esp` | 1 GiB (or reuse Windows ESP for sd-boot only) | vfat | build image |
| 2 | XBOOTLDR (dual-boot only) | `xbootldr` | 1 GiB | vfat | installer |
| 3 | usr A | `usr` | fixed slot: 3 GiB kiosk / 8 GiB desktop | erofs, lz4hc (repart `Compression=lz4hc`) | image / installer copy |
| 4 | usr A verity | `usr-verity` | ~1/120 of usr (sha256, 4 KiB blocks) → fixed 128 MiB slot | dm-verity hash tree | image |
| 5 | usr A verity sig | `usr-verity-sig` | exactly 16 KiB (fixed by systemd-repart for this type; size settings are rejected) | JSON (rootHash, certificateFingerprint, signature) | finalize |
| 6–8 | usr B (+verity, +sig) | same types, label `_empty` | same as A | — | first boot / installer |
| 9 | root | `root` | rest of disk (min 8 GiB) | ext4 (default) or btrfs; LUKS2 | first boot / installer |
| (10) | no swap partition | — | — | — | Hibernation is not offered: kernel lockdown (automatic under Secure Boot, set at balanced/hardened) disables it. zram provides swap |

Minimum disk: 16 GiB kiosk, 32 GiB desktop. Alignment: repart aligns partitions to a 4 KiB grain (or the sector size if larger).

Verity hash size: for a 4 KiB data block and 32-byte SHA-256 digests, each hash block covers 128 data blocks; total hash ≈ data/127 → 8 GiB usr ≈ 65 MiB hash tree. A 128 MiB slot leaves headroom.

## 4. Unified Kernel Image composition (ukify)

| Section | Content |
|---|---|
| `.linux` | kernel (vmlinuz) |
| `.initrd` | initrd built by mkosi (systemd-based, minimal: storage, crypto, tpm, verity, plymouth, keyboard) |
| `.cmdline` | `usrhash=<hex> rw quiet splash` + profile params |
| `.osrel` | os-release (IMAGE_ID/IMAGE_VERSION) |
| `.uname` | kernel version |
| `.sbat` | SBAT generation entries (needed with shim revocation) |
| `.pcrsig` / `.pcrpkey` | JSON signature over expected PCR 11 values per boot phase + public key (systemd-measure) |
| `.splash` | optional boot splash BMP |

File name `aurora_1.4.0+3-0.efi` in `ESP/EFI/Linux/` (or XBOOTLDR) → systemd-boot boot counting starts at 3 tries. sysupdate names new UKIs after the first `MatchPattern` (`aurora_@v+@l-@d.efi`), so never hard-code file names.

Emergency shells: the systemd initrd does not understand dracut's `rd.emergency=reboot`. Instead ship drop-ins for `emergency.service` and `rescue.service` in **both** the initrd and the host (`ExecStart=` reset, then `ExecStart=/usr/bin/systemctl --no-block reboot`), so failures reboot (boot counting then handles fallback) rather than offering a shell.
Initrd size target < 40 MiB (only firmware for storage/display needed at boot; Wi-Fi firmware stays in /usr).

## 5. Secure Boot

### 5.1 Key hierarchy (UEFI variables)
| Variable | Holds | Who controls |
|---|---|---|
| PK | Platform Key (1 cert) | tenant (B2B fleets) or OEM |
| KEK | keys allowed to update db/dbx | tenant + Microsoft KEK CA (keep for dbx updates) |
| db | allowed signers | tenant db cert + **Microsoft UEFI CA 2011** and **Microsoft UEFI CA 2023** (third-party loaders such as shim) + **Microsoft Option ROM UEFI CA 2023** (GPU/NIC option ROMs; older ROMs are signed via the 2011 UEFI CA — removing these can leave a machine without display or boot) + **Microsoft Windows Production PCA 2011** and **Windows UEFI CA 2023** when Windows must keep booting |
| dbx | revoked hashes/certs | updated via fwupd/LVFS dbx updates |

### 5.2 Mode A — tenant keys (recommended for fleets)
1. Device firmware put into Setup Mode (vendor menu "Reset to Setup Mode"/"Clear keys"; fleet docs per model).
2. Installer (or systemd-boot `secure-boot-enroll force|manual` with `loader/keys/<tenant>/{PK,KEK,db}.auth` on ESP) writes signed `.auth` variables.
3. Firmware returns to User Mode; only tenant-signed bootloaders + Microsoft-signed components run.
Keys: RSA-2048 (maximum firmware compatibility), `.auth` files generated by signer with time-based authenticated variable headers.

### 5.3 Mode B — shim + MOK (distributable images, arbitrary PCs)
1. Ship base distro's Microsoft-signed shim as `EFI/BOOT/BOOTX64.EFI`, our systemd-boot as `grubx64.efi` (the name shim chain-loads by default) signed with tenant MOK key.
2. **Live media (first boot):** shim cannot verify our systemd-boot yet and launches MokManager. The user chooses "Enroll key from disk" and selects `\EFI\foundry\tenant.der` on the ESP (the download page and a printed card show these steps). Nothing can be pre-staged, because no installer has run yet.
   **Installed systems that need a new key** (e.g. rotation): the running OS stages `mokutil --import tenant.der` with a one-time password shown to the user, who confirms it in MokManager on the next reboot.
3. User confirms in MokManager blue screen once; afterwards boots silently.
Caveats: SBAT revocations; MOK is trusted for shim-verified loads; systemd-boot uses shim's verification protocol for UKIs when shim is present.

### 5.4 Mode C — Secure Boot off
Allowed only for `security.level: relaxed` and private tier. Device attestation reports `sb=off`.

## 6. Measured boot and TPM2 disk encryption

| PCR | Measured by | Used in policy? |
|---|---|---|
| 0 | firmware code | no (changes with firmware updates) |
| 2 | option ROMs | no |
| 4 | boot loader + UKI PE images | no (changes every update) |
| 7 | Secure Boot state, PK/KEK/db/dbx, certs used | yes (optional, binds to SB being on with our keys) |
| 11 | UKI sections + boot phases | yes, via **signed policy** |
| 12 | cmdline/credentials overrides | no (UKI ignores overrides under SB) |
| 15 | machine-id, root fs identity (systemd-pcrmachine/pcrfs) | optional hardening |

- Two PCR-signing keys per tenant (the pattern in the systemd-measure man page):
  - `uki-pcr-initrd`: signs **only** the `enter-initrd` phase (`systemd-measure sign --phase=enter-initrd`). This is the only key enrolled for disk unlock.
  - `uki-pcr-main` (optional): signs later phases (`enter-initrd:leave-initrd`, `…:sysinit`, `…:ready`) for secrets that may be unsealed after the initrd (none by default).
  Both signatures go into the UKI `.pcrsig` section (ukify accepts several key/phase pairs).
- LUKS2 enrollment: `systemd-cryptenroll --tpm2-device=auto --tpm2-public-key=/usr/lib/foundry/uki-pcr-initrd.pem --tpm2-public-key-pcrs=11 [--tpm2-pcrs=7] [--tpm2-with-pin=yes]`. Because the policy trusts any PCR 11 value signed by the initrd key, A/B updates do not break unlock, and booting an unsigned kernel does.
- After `leave-initrd`, PCR 11 has moved past the only phase the initrd key signed → the TPM refuses to unseal the disk key once the OS is running (prevents runtime extraction). Signing every phase with one enrolled key would silently remove this protection.
- Recovery: `systemd-cryptenroll --recovery-key` generated at install, shown once, optionally escrowed to fleet (encrypted with tenant escrow key, admin-only access, audited).

## 7. Boot counting and rollback
- New UKI installed as `name_ver+3-0.efi`. Each attempt systemd-boot renames to `+2-1`, `+1-2`, …
- `boot-complete.target` requires `foundry-health.service` (session up, agent heartbeat OK, no failed critical units). On success `systemd-bless-boot` renames to `name_ver.efi` (good). On failure the health unit reboots the machine (`FailureAction=reboot`), so the next try — or the fallback — happens without waiting for a user.
- When tries reach 0 the entry is "bad"; systemd-boot sorts it last → previous good entry boots. Previous usr slot is still intact, so the old UKI's `usrhash=` finds it.
- Agent reports `rolled_back` with journal excerpt from the failed boot (persistent journal on root).

## 8. Installer
Live boot = same image, boot entry `installer` with `foundry.installer=1` (separate signed UKI with the same initrd).
Steps (backend in Go, UI = React in cage+Chromium kiosk):
1. Enumerate disks (`lsblk -J -b -o NAME,SIZE,TYPE,TRAN,MODEL,SERIAL,PTTYPE,PTUUID,RM`); exclude boot medium (match by PARTUUID of running ESP).
2. Choose mode: **erase** (new GPT) / **alongside** (free space only) / **advanced** (pick existing free region).
3. Generate repart definitions for the target into `/run/foundry/install.repart.d/`: usr A with `CopyBlocks=auto` when booted from USB/disk (copies the verified running usr partition), or `CopyBlocks=<path of the usr image inside the ISO>` when booted from ISO (there `/usr` is loop-mounted, so `auto` finds no usr partition), verity + sig likewise, empty B slots, root with `Encrypt=tpm2`, ESP/XBOOTLDR.
4. `systemd-repart --dry-run=no --empty=<force|allow> --definitions=... /dev/<disk>` → wait for udev settle.
5. `bootctl install --esp-path=... [--boot-path=<xbootldr>]`, copy signed UKI(s), write `loader.conf` (`timeout 0`, `editor no`, `auto-entries no` for erase-disk / `auto-entries yes` for dual boot (otherwise the Windows Boot Manager entry is hidden), `auto-firmware yes`, `default @saved` or `aurora_*`).
6. Secure Boot: if setup mode and Mode A → enroll; if Mode B → stage MOK request.
7. TPM: create LUKS key in TPM, show recovery key, optional PIN.
8. Users: firstboot (systemd-firstboot/our OOBE) or predefined; enrollment token for fleet.
9. Write `/var/lib/foundry/install.json` (installer version, mode, disk serial hash) and reboot.

## 9. Dual boot with Windows
- Never resize or write NTFS. Require ≥ 40 GiB *unallocated* (guide user to shrink in Windows Disk Management first).
- Detect Windows ESP (`esp` type containing `EFI/Microsoft/Boot/bootmgfw.efi`). Windows ESPs are often 100 MiB, too small for UKIs → put only systemd-boot (~200 KiB) + `loader/` there and create a 1 GiB **XBOOTLDR** for UKIs (Boot Loader Specification). If < 20 MiB free on ESP → abort with guidance.
- systemd-boot auto-detects Windows Boot Manager on the ESP and lists it; `timeout 3` in dual-boot mode.
- BitLocker: detect volumes with the `-FVE-FS-` signature at byte offset 3 of the partition; adding a boot entry / changing boot order / changing SB keys can trigger BitLocker recovery → require the user to confirm they have the recovery key, and recommend suspending BitLocker once before install. Mode A key enrollment is disabled in dual-boot mode unless db keeps Microsoft Windows Production CA.
- Set NVRAM order so our loader is first only if user chose "make default".

## 10. Live media
- USB: `disk.img.xz` for Etcher/Rufus (DD mode), or `disk.raw.zst` for `zstdcat … | dd`; it is a full GPT disk with ESP + usr A → boots directly as live/installer.
- ISO: `xorriso -as mkisofs -iso-level 3 -full-iso9660-filenames -volid AURORA -e efiboot.img -no-emul-boot -isohybrid-gpt-basdat -append_partition 2 0xef efiboot.img ...` with the usr erofs inside the ISO filesystem, mounted via a loop device in the initrd (live mode reads `usrhash` the same way; verity over the loop-mounted image).
- VM: qcow2 (KVM/Proxmox), VMDK streamOptimized + OVF/OVA (VMware/VirtualBox); VM templates specify UEFI firmware + TPM 2.0 (vTPM) + Secure Boot off by default (VM vendor key provisioning differs).

## 11. First boot sequence
1. `systemd-repart` (initrd) creates B slots and root, encrypts root with TPM2 (repart `Encrypt=tpm2`).
2. `systemd-firstboot`/OOBE: locale, keymap, timezone, hostname (or from spec/fleet).
3. `machine-id` committed; tmpfiles populate `/etc` from `/usr/share/factory/etc`.
4. Flatpak sideload install; agent enrollment if token present.
5. `foundry-boot-ok.service` writes `FOUNDRY-BOOT-OK` to `/dev/ttyS0` when `FOUNDRY_TEST=1` credential is set (tests only).

## 12. Firmware updates and BIOS settings
- `fwupd` with LVFS metadata (mirrored, signature-checked) for UEFI capsule updates, dbx updates, device firmware; policy: `auto | manual | never`.
- Vendor firmware settings via `/sys/class/firmware-attributes/*/attributes/` on supported Dell/Lenovo/HP models (Phase 7): read current values, set with admin password (`authentication/Admin/current_password`), changes pending until reboot.
- What the OS cannot do: change settings on unsupported firmware, clear a BIOS password, or bypass vendor Secure Boot UI requirements.

## 13. arm64 notes (Phase 6)
UKI/systemd-boot work on aarch64 UEFI (`BOOTAA64.EFI`). Device trees: either firmware-provided (ACPI/DT from EDK2) or `.dtb` UKI section. Raspberry Pi 4/5: EDK2 RPi UEFI firmware on the first FAT partition before ESP content.

## 14. Legacy BIOS compat profile (Phase 6)
GPT + 1 MiB BIOS boot partition (`21686148-6449-6E6F-744E-656564454649`) for GRUB core.img; GRUB loads kernel+initrd from ESP-like `/boot` vfat; A/B selection via `grubenv` boot counter variables; no verity root-hash signature chain (verity still used for integrity against corruption, not tampering); clearly labelled "reduced security".

## Verified in Phase 0 (Ubuntu 24.04, systemd 255, QEMU/OVMF/swtpm)
- **Enrollment**: systemd-boot enrolls `loader/keys/auto/{PK,KEK,db}.auth` automatically with `secure-boot-enroll force` (VM targets); hardware images use `manual` (menu entry). Other directory names are not auto-enrolled.
- **Trusted boot chain**: the signer signs the *platform's* systemd-boot and re-assembles the UKI with the platform's systemd-stub; only `.linux/.initrd/.cmdline/.osrel/.uname/.splash` come from the build. The UKI must carry `usrhash=` matching the verified root hash, or signing is refused.
- **TPM policy**: `.pcrsig` is signed for the `enter-initrd` phase only (`ukify --phases=enter-initrd`). The root unlocks in the initrd; `cryptsetup --test-passphrase --token-only` from the running system is refused. A UKI whose policy is signed by another key cannot unlock the root (`systemd-cryptsetup@root` fails).
- **Unsigned dev images** (no `.pcrpkey`) make repart bind the root to PCR 7 only, which is unsealable from the running system. Customer images are always signed by finalize.
- **PCR 11 prediction**: `systemd-measure calculate` over the signed UKI's sections predicts the value the TPM holds after boot (phase `…:ready`); the boot test asserts equality (basis for remote attestation, Phase 4).
- **Deferred verity-sig**: mkosi `Verity=defer` leaves a 16 KiB gap after each usr-verity partition; finalize adds the GPT entries (A: signed JSON, B: `_empty`, zeroed).
- **Partition UUIDs**: usr = first half of the root hash, usr-verity = second half — `usrhash=` finds them this way, so update payload files carry the UUID (`<id>_<ver>_<uuid>.usr.raw`, sysupdate `@u`).
- **Health check** is part of the boot only on counted (`+N`) entries (it is `RequiredBy=boot-complete.target`). Ordering it into `multi-user.target` creates a cycle (it is ordered *after* multi-user).
- **efivarfs** files are not seekable: read them through a pipe (`cat … | tail -c +5`).
- **Boot time** without KVM (TCG): ~2 minutes per boot to multi-user with a 75 MB UKI (`KernelInitrdModules=default`; the full module set made a 346 MB UKI).
