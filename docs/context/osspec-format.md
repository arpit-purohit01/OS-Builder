# OS specification format (OSSpec)

The OSSpec is the single source of truth for an OS. The GUI is an editor for this document; the
compiler turns it into a mkosi tree. JSON Schema: `schemas/osspec.v1alpha1.schema.json`.

## 1. Full example (annotated)

```yaml
apiVersion: foundry.dev/v1alpha1
kind: OSSpec
metadata:
  name: aurora-office          # [a-z0-9-]{3,40}
  displayName: Aurora OS
  version: 1.4.0               # semver of THIS OS, becomes IMAGE_VERSION
foundation:
  base: debian-13              # catalog id: debian-13 | ubuntu-26.04 | fedora-<n>
  arch: [x86_64]               # x86_64 | aarch64
  snapshot: "2026-10-20T00:00:00Z"   # pinned mirror snapshot; default = latest approved
  profile: desktop-kde         # desktop-kde | desktop-gnome | kiosk-cage | thin-client | server
  bootMode: uefi               # uefi | uefi+bios-compat (Phase 6)
look:
  theme: { mode: auto, accent: "#4F46E5", radius: medium, font: inter, iconTheme: breeze }
  wallpaper: { asset: "sha256:9f2c000000000000000000000000000000000000000000000000000000000000" }      # uploaded asset digest
  layout: { preset: dock-bottom }             # or { custom: "sha256:..." } from layout editor
  bootSplash: { logo: "sha256:a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", background: "#0B1020" }
  loginScreen: { background: "sha256:b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2" }
apps:
  preinstalled: [flatpak:org.mozilla.firefox, flatpak:org.libreoffice.LibreOffice]
  system: [cups, printer-driver-gutenprint]    # catalog ids → base packages
  custom: []                                   # Expert: uploaded .deb/flatpak bundles (reviewed)
security:
  level: balanced              # relaxed | balanced | hardened
  diskEncryption: tpm2         # none | tpm2 | tpm2+pin | passphrase
  secureBoot: tenant-keys      # tenant-keys | shim-mok | off(only relaxed, private tier)
  firewall: { default: deny-inbound, allow: [mdns] }
  usbStorage: allow            # allow | read-only | block
  autoUpdates: true
access:
  rootModel: admin-password    # locked | admin-password | developer
  users:
    mode: firstboot            # firstboot | predefined | directory(Phase 5) | autologin-kiosk
    predefined: []
  ttyConsole: false
kernel:
  flavor: standard             # standard | hardened | realtime | gaming
  extraCmdline: []             # Expert, allowlisted params only
memory:
  profile: auto                # auto | low-ram | desktop | workstation
ecosystem:
  identity: { provider: oidc, issuer: "https://login.example.com", clientId: "..." }
  appStore: { enabled: true }
  sync: { provider: hosted }   # none | hosted | webdav(url)
  handoff: { enabled: false }
  fleet: { enroll: true, group: front-office }
  kiosk: {}                    # kiosk-cage profile only: { url, idleResetMinutes, allowedDomains }
branding:
  osName: Aurora OS
  osId: aurora                 # → /usr/lib/os-release ID=
  homeUrl: "https://aurora.example.com"
  supportUrl: "https://aurora.example.com/help"
localization: { locale: en_US.UTF-8, keymap: us, timezone: Asia/Kolkata }
hardware:
  targets: [lenovo-thinkpad-t14-gen4, generic-uefi-x86_64]
  extraFirmware: [firmware-iwlwifi]
expert:                        # each item forces review; forbids verified tier
  packages: [htop, jq]         # extra packages from the base archive (Debian package-name syntax)
  overlays: []                 # sha256 of validated tarballs (extracted at / via mkosi ExtraTrees)
  postinstScripts: []          # sha256 of shell scripts; run in the image chroot, no network, 10 min limit
  extraRepos:                  # third-party APT repositories
    - url: "https://repo.example.com/apt"
      suite: noble             # optional; default = base codename
      components: [main]       # optional; default [main]
      keyAsset: "sha256:..."   # uploaded public key (gpg-key asset; secret keys rejected)
      keyFingerprint: "0123…"  # 40 hex, must equal the uploaded key's fingerprint (AST-KEY)
      pin: "origin repo.example.com"  # apt pin; packages from the repo never replace base
                                       # packages unless pinned (priority 100 by origin, 500 for this pin)
  mkosiDropins: []             # sha256 of mkosi .conf drop-ins; allowlisted keys only
```

Custom `.deb` packages (`apps.custom`) are uploads of kind `deb`; the package name comes
from the control file at upload, and maintainer scripts are flagged for review.

## 2. Validation layers (all server-side; layers 1–3 also client-side)

| Layer | What | Implementation | Failure |
|---|---|---|---|
| 1 Schema | types, enums, patterns, sizes | JSON Schema 2020-12 (santhosh-tekuri/jsonschema in Go, ajv in web) | 422 with JSON pointer per error |
| 2 Semantic | cross-field rules: `secureBoot: off` requires `level: relaxed`; `rootModel: locked` forbids `ttyConsole: true`; `diskEncryption: tpm2+pin` requires a profile with a greeter or plymouth PIN entry; asset digests exist and belong to org | Go rule functions, each with id `SEM-xxx` | 422 |
| 3 Catalog SAT | selected options satisfiable under catalog constraints for chosen base/arch/profile | `internal/catalog` CNF + SAT | 422 + MUS explanation |
| 4 Policy | org/plan limits (expert allowed?, max image size, allowed bases), trust-tier implications | Go policy funcs | 403/422 |

## 3. Canonicalization and identity
- Defaults are applied (`Default()`), then the document is serialized with RFC 8785 (JSON Canonicalization Scheme): sorted keys, no insignificant whitespace, ECMAScript number formatting.
- `spec_digest = sha256(canonical_json)`. Spec versions are immutable rows keyed by `(project_id, spec_digest)` — saving an identical spec is idempotent.
- YAML is an input format only; never hash YAML.

## 4. Versioning and migrations
- `apiVersion` follows `foundry.dev/v1alphaN → v1betaN → v1`. Each bump ships a pure migration `func Migrate(prev) (next, []Warning)`; chain migrations; store original + migrated.
- Removed fields: deprecate for one minor version with warning, then migrate away.

## 5. Field → implementation mapping (debian-13)

| Field | Compiler output |
|---|---|
| `foundation.base/snapshot` | `[Distribution] Distribution=debian Release=trixie Mirror=https://snapshot.debian.org/archive/debian/<ts>/` (+ security, non-free-firmware components), `SOURCE_DATE_EPOCH=<ts>` |
| `foundation.profile` | include `images/profiles/<profile>/mkosi.conf.d/*` |
| `look.theme` (KDE) | `/usr/share/foundry/kde/kdeglobals` copied as `/etc/xdg/kdeglobals` defaults; accent `AccentColor=r,g,b` |
| `look.theme` (GNOME) | dconf keyfile `/etc/dconf/db/local.d/10-foundry-theme` (`accent-color`, `color-scheme`), `dconf update` in postinst |
| `look.wallpaper` | asset to `/usr/share/wallpapers/foundry/` + profile default config |
| `look.bootSplash` | Plymouth theme `/usr/share/plymouth/themes/foundry/` + `plymouth-set-default-theme` in postinst; `splash quiet` in cmdline |
| `apps.preinstalled` | sideload repo baked into `/usr/share/foundry/flatpak-sideload`, unit `foundry-flatpak-firstboot.service` |
| `apps.system` | `Packages=` lines (sorted, deduped) |
| `security.diskEncryption` | repart root `Encrypt=tpm2` / `key-file` and firstboot enrollment unit |
| `security.firewall` | `/usr/lib/foundry/nftables.conf` + `nftables.service` enabled |
| `security.usbStorage` | usbguard policy or udev rule `ENV{UDISKS_IGNORE}` / modprobe blacklist `usb_storage` (block) |
| `access.rootModel` | see root-access-and-hardening.md §2 |
| `kernel.flavor` | package (`linux-image-amd64` / `linux-image-rt-amd64`) + cmdline set + sysctl drop-in |
| `memory.profile` | zram-generator.conf, sysctl, oomd drop-ins (memory-management.md §1) |
| `branding` | `/usr/lib/os-release` (`NAME`, `ID`, `ID_LIKE=debian`, `VERSION_ID`, `IMAGE_ID`, `IMAGE_VERSION`, `HOME_URL`, `SUPPORT_URL`), UKI `.osrel` section, systemd-boot title |
| `localization` | `Locale=`, `Keymap=`, `Timezone=` (mkosi) |
| `ecosystem.fleet` | agent package + `/usr/lib/foundry/agent.toml` (server URL, tenant CA bundle) |

## 6. Catalog format

```yaml
id: session.kiosk
kind: session            # groups: desktop/session/kernel/... used by oneOf
label: Single-app kiosk
bases: [debian-13, ubuntu-26.04]
arch: [x86_64, aarch64]
requires: [display.wayland, app.browser-kiosk]
conflicts: [desktop.kde, desktop.gnome]
provides: [session.graphical]
oneOf: session           # exactly one option with oneOf=session must be selected
packages:
  debian-13: [cage, chromium]
risk: { flags: [] }
ui: { icon: kiosk, levels: [simple, custom, expert], help: "Runs one app full screen." }
```

`provides` creates virtual vars: `provides: [x]` adds clause `(¬opt ∨ x)` and `x → (opt₁ ∨ opt₂ ∨ …)` over all providers.

## 6a. Custom layout document (`look.layout.custom`)
The layout editor uploads a JSON document (asset kind `layout`, max 64 KiB, validated
with unknown fields rejected) and stores its digest in `look.layout.custom`:

```json
{"version": 1,
 "panels": [{"edge": "bottom", "style": "dock", "size": 48, "autohide": false,
             "items": ["launcher", "favorites", "tasks", "tray", "clock"]}],
 "favorites": ["org.mozilla.firefox.desktop"]}
```

`edge` ∈ top/bottom/left/right (unique), `style` ∈ dock/bar, `size` 24–96, 1–12 items from
launcher, favorites, tasks, tray, clock, search, spacer, desktops, trash; ≤ 24 favorites
(`*.desktop` ids). KDE: panels are recreated by a Plasma first-start script; GNOME: the
first non-top panel becomes the dash-to-dock dock and favorites become `favorite-apps`
(GNOME's top bar is fixed). The API passes the parsed document to the compiler (pure).

## 6b. Hardware compatibility (catalog `hcl`)
Device options may carry `hcl: {status: certified|tested|community, tested: [bases], notes}`.
Choosing a device with a base it was not tested on adds warning `HCL-W01` (the build is
allowed). The builder shows the status next to each device.

## 7. Expert section rules
- Any non-empty `expert.*` → tier cannot be `verified`, review mandatory.
- `extraCmdline` allowlist: `quiet`, `splash`, `loglevel=`, `video=`, `i915.*`, `amdgpu.*`, `nvme_core.default_ps_max_latency_us=`, `pcie_aspm=`, `mem_sleep_default=`. Denylisted examples: `init=`, `rd.break`, `systemd.unit=emergency.target`, `lockdown=none`, `module.sig_enforce=0`, `selinux=0`, `apparmor=0`.
- `extraRepos`: HTTPS only, pinned signing key fingerprint, snapshot/compose pin required.
