# Root access models and system hardening

## 1. Concepts
- **Root model** = what the device's users can do to the system (spec `access.rootModel`).
- **Security level** = how hardened the system is by default (spec `security.level`).
- They are independent axes with some forbidden combinations (validation `SEM-0xx`).

| | relaxed | balanced | hardened |
|---|---|---|---|
| locked | allowed | allowed (kiosk default) | allowed |
| admin-password | allowed | allowed (desktop default) | allowed |
| developer | allowed | allowed with warning | **forbidden** (SEM-012) |

## 2. Root models — exact configuration

### 2.1 `locked` (kiosks, labs, shared devices)
- Root account locked: `/etc/shadow` root hash `!*` (written via `usr/share/factory/etc/shadow` + sysusers).
- No `sudo`/`doas` package; `pkexec` removed or polkit rule denies everything not explicitly allowed:
  ```js
  // /usr/share/polkit-1/rules.d/10-foundry-locked.rules
  polkit.addRule(function(action, subject) {
    if (action.id.startsWith("org.freedesktop.NetworkManager.") && subject.active && subject.local) return polkit.Result.YES; // Wi-Fi join
    if (action.id == "org.freedesktop.login1.power-off" || action.id == "org.freedesktop.login1.reboot") return polkit.Result.YES;
    return polkit.Result.NO;
  });
  ```
- No text consoles: mask `getty@tty2..6`, `NAutoVTs=0`, `ReserveVT=0` in logind.conf.d; `kernel.sysrq=0`.
- Initrd/emergency: `systemd.show_status=false`; drop-ins for `emergency.service` and `rescue.service` in **both** the initrd and the host (`ExecStart=` reset, then `ExecStart=/usr/bin/systemctl --no-block reboot`), so failures reboot instead of offering a shell (with root locked, sulogin would refuse anyway). `rd.emergency=reboot` is dracut syntax and is ignored by the systemd initrd.
- Boot editing impossible: UKI cmdline is signed; `loader.conf` `editor no`; menu hidden (`timeout 0`).
- Recommend firmware admin password + USB boot disabled (Phase 7 automation).
- Session: autologin to a non-privileged user (`foundry-kiosk`, no shell login), cage + app.

### 2.2 `admin-password` (office desktops)
- First user created at OOBE is in `sudo` (Debian) / `wheel` group; `sudo` requires password, `Defaults timestamp_timeout=5`, `Defaults use_pty`, `Defaults logfile=/var/log/sudo.log`.
- polkit: `auth_admin_keep` for admin actions (default upstream behavior).
- Still immutable `/usr`: admin can manage users, network, printers, Flatpaks, sysext modules signed by tenant, but cannot alter `/usr` (verity) — that is a feature, not a limitation.

### 2.3 `developer`
- Sudo for first user (password or `NOPASSWD` if spec says so).
- `/usr` remains verity-protected; customization through:
  - `/opt`, `/usr/local` → symlinked to `/var/opt`, `/var/usrlocal` (writable).
  - Developer sysext: `systemd-sysext` with `--mutable=yes` (systemd ≥ 256) to layer a writable overlay on `/usr` stored in `/var/lib/extensions.mutable/` — removable with one command.
  - Container tooling (podman, distrobox) in catalog for "apt install" style workflows inside containers.
- Attestation reports `mutable_usr=true` when mutable sysext is active; such devices are excluded from "verified" compliance.

### 2.4 "Unlocked image" (Expert, private tier only)
Writable `/usr` without verity (btrfs root containing `/usr`), no A/B. Updates become full reinstall. Not eligible for fleet GA or publishing.

## 3. Kernel flavors

| Flavor | Debian package | Extra cmdline | Use |
|---|---|---|---|
| standard | `linux-image-amd64` | — | default |
| hardened | same kernel + params | see §4 | regulated kiosks, high-risk |
| realtime | `linux-image-rt-amd64` (PREEMPT_RT) | `threadirqs` | audio, industrial |
| gaming | standard or backports kernel | `split_lock_detect=off` (optional) + sysctl `vm.max_map_count=1048576` | game stations |

Module signature enforcement: Debian kernels are built with module signing; under Secure Boot the kernel enters lockdown (integrity) and refuses unsigned modules. Out-of-tree drivers (e.g. NVIDIA) must be signed by the tenant key at build time (DKMS in build VM, sign in finalize via signer) — mark such images as needing review.

## 4. Hardening parameters

### 4.1 Kernel cmdline by level
| Param | relaxed | balanced | hardened |
|---|---|---|---|
| `lockdown=integrity` | – | ✓ | `lockdown=confidentiality` |
| `init_on_alloc=1` | – | ✓ | ✓ |
| `init_on_free=1` | – | – | ✓ (≈1–7% perf cost) |
| `slab_nomerge` | – | ✓ | ✓ |
| `page_alloc.shuffle=1` | – | ✓ | ✓ |
| `randomize_kstack_offset=on` | ✓ | ✓ | ✓ |
| `vsyscall=none` | ✓ | ✓ | ✓ |
| `debugfs=off` | – | ✓ | ✓ |
| `iommu=force` / `intel_iommu=on` / `amd_iommu=force_isolation` | – | – | ✓ (DMA attack protection; test per model) |
| `module.sig_enforce=1` | – | ✓ | ✓ |

### 4.2 sysctl (`/usr/lib/sysctl.d/60-foundry.conf`)
```
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
kernel.unprivileged_bpf_disabled = 1
net.core.bpf_jit_harden = 2
kernel.yama.ptrace_scope = 1          # hardened: 2
kernel.kexec_load_disabled = 1
kernel.sysrq = 0                       # developer: 176
kernel.perf_event_paranoid = 3         # developer: 2
dev.tty.ldisc_autoload = 0
fs.protected_symlinks = 1
fs.protected_hardlinks = 1
fs.protected_fifos = 2
fs.protected_regular = 2
fs.suid_dumpable = 0
vm.unprivileged_userfaultfd = 0
net.ipv4.conf.all.rp_filter = 2      # loose; strict (1) breaks VPN/multi-homed setups — hardened may use 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.tcp_syncookies = 1
```
Note: `kernel.unprivileged_userns_clone` controls (Debian/Ubuntu-specific) interact with Flatpak/bubblewrap and browsers' sandboxes — keep user namespaces enabled; on Ubuntu use the AppArmor userns restriction model instead.

## 5. Mandatory access control and sandboxing
- AppArmor enabled (Debian/Ubuntu default), profiles enforced for exposed services; SELinux on Fedora base.
- Apps as Flatpaks: bubblewrap sandbox + portals; catalog sets default permission overrides (`flatpak override --system --nofilesystem=home` for kiosk browsers).
- systemd service hardening for our units: `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`, `SystemCallFilter=@system-service`, `CapabilityBoundingSet=` minimal, `MemoryMax=` (agent: 64M).

## 6. Network and device controls
- nftables default: drop inbound except established/related, ICMP rate-limited, mDNS optional, DHCP client.
- `usbguard` for kiosk/hardened: allow keyboard/mouse/touch classes + devices present at enrollment; block new mass storage (or `security.usbStorage: read-only` via udev + `ro` mount options in udisks2 config).
- Bluetooth off by default on kiosks (`systemctl mask bluetooth`).

## 7. Logging and audit
- Persistent journald on root, `SystemMaxUse=500M`, forward selected units to fleet on demand.
- auditd rules (hardened): execve of setuid binaries, changes to `/etc/sudoers*`, `/etc/passwd`, polkit rules, kernel module loads.

## 8. What each level guarantees (customer-facing)
| Guarantee | relaxed | balanced | hardened |
|---|---|---|---|
| Disk encrypted | optional | ✓ TPM2 | ✓ TPM2+PIN option |
| Secure Boot | optional | ✓ | ✓ (required) |
| Verified /usr | ✓ | ✓ | ✓ |
| Automatic security updates | optional | ✓ | ✓ (forced deadline ≤ 7 days) |
| App sandboxing | ✓ | ✓ | ✓ + stricter overrides |
| USB storage | allow | allow | read-only/block |
