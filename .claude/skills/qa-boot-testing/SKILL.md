---
name: qa-boot-testing
description: Use when writing or debugging cmd/tester, test/boot scenarios, the QEMU/OVMF/swtpm boot matrix, openQA GUI tests, upgrade/power-cut fault injection, reproducibility checks or hardware-in-the-loop (HIL) lab automation.
---

# QA and boot testing

Read first: `docs/context/sdlc-and-quality.md` §3–4, `docs/context/boot-firmware-and-disk.md`, `docs/context/update-system.md` §6.

## Scenario format (`test/boot/scenarios/<name>.yaml`)
```yaml
name: update-ab
firmware: ovmf-secboot-tenant      # ovmf | ovmf-secboot-tenant | ovmf-secboot-ms-shim | seabios
tpm: swtpm2
arch: x86_64
disk: nvme
memory_mb: 4096
images: [v1: build:<id1>, v2: build:<id2>]
steps:
  - boot: v1
    expect_serial: "FOUNDRY-BOOT-OK"
    timeout_s: 180
  - guest: "foundry-test stage-update --from-file /run/test/v2"   # test-only helper via credential
  - reboot: {}
  - expect_serial: "FOUNDRY-BOOT-OK version=v2"
  - guest_assert: ["bootctl status | grep -q 'aurora_2'", "veritysetup status usr | grep -q verified"]
```

## How tests talk to the guest
- Serial console (`-serial unix:...`) for markers. Test hooks are **not** in production images: tests attach a test sysext (containing `foundry-test` and marker services) signed by the **test** key, which is trusted only in test firmware vars/keyrings. It is enabled by a credential passed with `-smbios type=11,value=io.systemd.credential:foundry.test=1`.
- Use `systemd-run`-style commands over a virtio-serial command channel (`foundry-test` daemon) — never SSH (no sshd in images).

## Fault injection
- Power cut: run sysupdate in guest, kill QEMU after random delay (uniform over measured write duration), restart; 50 iterations per release pair; assert boot of a good slot and successful later retry.
- Disk errors: `blkdebug` QEMU driver to inject EIO on usr B writes.
- Health failure: test overlay masks the session unit → expect rollback after 3 tries.

## GUI tests (openQA, Phase 2+)
Needles for: login screen branding, desktop with dock/panel layout, kiosk URL loaded, installer steps. Keep needles per profile and theme mode; update needles via reviewed PRs only.

## HIL lab
- Each device: PiKVM (HDMI capture + USB keyboard/mass-storage emulation) or Intel AMT/vPro KVM; smart PDU for power.
- Flow: mount image as virtual USB → power cycle → select boot device via firmware hotkey script → run installer automation → assert screens/serial (USB-serial when available) → run suite (Wi-Fi join, suspend/resume ×5, audio playback, external display, update A→B).
- Results feed `hcl_results`.

## Flakiness policy
A test that flakes twice in 7 days is quarantined (still runs, non-blocking) with an owner and a 1-week fix deadline. Boot timeouts must use measured p99 × 2, not guesses.
