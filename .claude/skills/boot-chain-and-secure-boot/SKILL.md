---
name: boot-chain-and-secure-boot
description: Use for anything touching UEFI/BIOS boot, partitions, systemd-boot, UKIs, Secure Boot keys (PK/KEK/db/MOK), TPM2 measured boot, PCR policies, LUKS/TPM enrollment, boot counting/rollback, the installer, dual boot with Windows, or live/ISO media.
---

# Boot chain, Secure Boot and TPM

Read first: `docs/context/boot-firmware-and-disk.md` (whole file), `docs/context/root-access-and-hardening.md` §2, `docs/context/update-system.md` §3 and §6.

## Mental model (keep this invariant chain intact)
firmware (SB db) → [shim → MOK] → systemd-boot (signed) → UKI (signed; cmdline contains `usrhash=`) → dm-verity `/usr` → TPM unseals root only when PCR 11 matches a value signed by the tenant PCR key.
Any change that lets an unsigned component execute, or lets the cmdline vary, breaks the chain. Treat it as a security bug.

## Local test loop
```
make image SPEC=docs/examples/office-laptop.yaml
test/boot/ovmf/mkvars.sh --keys test/keys/tenant-test --ms-ca  > /tmp/vars.fd   # enroll test PK/KEK/db + MS CAs
make boot-test IMAGE=out/<id>/disk.raw MATRIX=uefi,sb,tpm
# manual:
qemu-system-x86_64 -machine q35,smm=on,accel=kvm -cpu host -m 4G \
  -global driver=cfi.pflash01,property=secure,value=on \
  -drive if=pflash,format=raw,unit=0,file=OVMF_CODE.secboot.fd,readonly=on \
  -drive if=pflash,format=raw,unit=1,file=/tmp/vars.fd \
  -chardev socket,id=chrtpm,path=/tmp/swtpm/sock -tpmdev emulator,id=tpm0,chardev=chrtpm -device tpm-tis,tpmdev=tpm0 \
  -drive if=none,id=d0,file=out/<id>/disk.raw,format=raw -device nvme,drive=d0,serial=t1 -serial mon:stdio
```
(OVMF file names/paths differ per distro package — check `deploy/versions.lock`.)

## Checks to run after any change
- `bootctl status` in guest: Secure Boot enabled, current entry, boot counting state.
- `ukify inspect <uki>`: sections present (.linux .initrd .cmdline .osrel .uname .sbat .pcrsig .pcrpkey).
- `systemd-analyze pcrs` and `/run/log/systemd/tpm2-measure.log` match predictions in manifest.
- `veritysetup status usr` (active, verified), `cryptsetup luksDump` (tpm2 token with signed policy).
- Negative tests: unsigned UKI refused; modified cmdline refused; flipped usr byte → I/O error; different PCR key → no auto-unlock.

## Rules
- Keep Microsoft UEFI CA 2011, Microsoft UEFI CA 2023 and Microsoft Option ROM UEFI CA 2023 in db when enrolling tenant keys (GPU/NIC option ROMs). Keep Microsoft Windows Production PCA 2011 and Windows UEFI CA 2023 when Windows must keep booting.
- Enroll only the **initrd-phase** PCR key (`uki-pcr-initrd`) for disk unlock; never one key that signs all phases.
- Use `auto-entries yes` in dual-boot installs so Windows Boot Manager shows up.
- RSA-2048 for SB keys (firmware compatibility).
- Never allow cmdline editing (`editor no`), never add `rd.shell` (and remember `rd.emergency=reboot` is dracut-only — use reboot drop-ins for emergency/rescue in initrd and host), never use PCRs 0/2/4 in disk policies (they change on firmware/update).
- Installer must never write to NTFS partitions or resize them; must detect BitLocker (`-FVE-FS-` at offset 3) and require explicit recovery-key confirmation.
- XBOOTLDR when the existing ESP is too small; never delete or reformat an existing ESP.
- All changes to `.repart`, `loader.conf`, sysupdate transfers, UKI composition need a security reviewer.

## Debugging guide
| Symptom | Likely cause |
|---|---|
| Firmware "Security Violation" | systemd-boot/UKI signed with key not in db (or MOK not enrolled) |
| Drops to previous version after update | health check failed → read journal of failed boot (`journalctl -b -1 -u foundry-health`) |
| Recovery-key prompt after update | PCR 11 prediction/signature mismatch (UKI rebuilt without new `.pcrsig`) or policy also pinned PCR 7 and SB db changed |
| Disk key can be unsealed from the running OS | enrolled PCR key also signed post-initrd phases — re-enroll with the initrd-only key |
| Black screen on real laptop with tenant keys | MS UEFI CA missing from db → GPU option ROM blocked |
| Windows missing from menu | Windows on a different ESP → add entry, or enable `auto-entries`/`auto-firmware` |
