# Phase 2 — Builder & customization (10 weeks)

## Goal
A non-technical user at a pilot customer designs an OS in the visual builder (Simple / Custom / Expert
levels), sees a live preview, builds it, and installs it on bare metal or beside Windows using our
installer — unaided, in under 30 minutes of their own time.

## Scope
In: full builder UI, catalog with constraints compiled to CNF and solved client- and server-side, theme
studio, branding (boot splash, login screen, os-release), custom files/apps upload (reviewed path
stubbed until Phase 3), installer (raw/ISO live media), dual boot into free space, live preview streaming,
build cache tiers, fair scheduler, multi-host workers, undo/redo, spec versions & diff.
Out: public publishing (needs Phase 3), updates (Phase 4).

## Tasks

| ID | Task | Detail | Design doc |
|---|---|---|---|
| P2-01 | Catalog format v1 | `catalog/*.yaml`: options with `requires`, `conflicts`, `provides`, `oneOf` groups, per-base package mapping, UI metadata, risk flags. Versioned (`catalog_version` = git tree hash) | osspec-format.md §Catalog |
| P2-02 | CNF compiler + SAT | `internal/catalog`: options → boolean vars; encode requires (¬a ∨ b), conflicts (¬a ∨ ¬b), exactly-one groups (sequential counter encoding), user selections as unit clauses. Output canonical `catalog.cnf.json` consumed by Go (gini) and the web Web Worker (MiniSat/WASM) | algorithms-and-data-structures.md §SAT |
| P2-03 | Explanations | On UNSAT compute a minimal unsatisfiable subset (deletion-based MUS over user selections) → human message "Kiosk session conflicts with Full desktop. Remove one." | algorithms-and-data-structures.md §SAT |
| P2-04 | Builder UI steps | Foundation, Look & feel, Apps, Security & access, Ecosystem (stub), Branding, Review & build. Simple/Custom/Expert switch per step | builder-web-ui skill |
| P2-05 | Theme studio | Accent color, light/dark/auto, fonts (from a licensed font list), icon theme, cursor, corner radius, wallpaper, Plymouth splash (logo + bg color), SDDM/GDM login background, systemd-boot menu hidden by default. Maps to KDE `kdeglobals` / GNOME dconf defaults in `/usr/share` + `/etc/dconf/db/local.d` | osspec-format.md §Look |
| P2-06 | Layout editor (Custom level) | Drag panels/dock/widgets on a canvas; output is a profile-specific layout spec (KDE: Plasma layout JS script in `/usr/share/plasma/look-and-feel/<id>/contents/layouts/`; GNOME: dash-to-dock/extension settings in dconf) | osspec-format.md §Layout |
| P2-07 | Custom files & apps (Expert) | Upload overlay tarball, `.deb` packages, Flatpak refs, shell scripts (postinst). Validation in P2-08. Marked `unverified` until Phase 3 review exists; downloadable only by the org | security-threat-model.md §Upload |
| P2-08 | Upload validator | Streaming tar/zip parser: reject absolute paths, `..`, symlinks escaping root, hardlinks, device nodes, FIFOs, setuid/setgid (Expert + review only), names with control chars, non-UTF-8; normalize NFC; caps: 100 MiB/file, 2 GiB total, 200k entries, ratio 100:1 | algorithms-and-data-structures.md §Path trie |
| P2-09 | Installer | Live mode = same image with `foundry.installer=1` boot entry. Cage + local web UI (same React components). Steps: language, disk select, mode (erase disk / install alongside / custom), encryption PIN/recovery key display, user creation, enrollment token (Phase 4). Backend: `systemd-repart --definitions=/usr/lib/foundry/install.repart.d --empty=allow` + `CopyBlocks=auto` for usr, `bootctl install`, copy UKIs to XBOOTLDR | boot-firmware-and-disk.md §Installer |
| P2-10 | Dual boot | Only unallocated space ≥ 40 GiB; detect Windows ESP; reuse it for systemd-boot if ≥ 20 MiB free else abort with guidance; new XBOOTLDR (1 GiB); detect BitLocker (`-FVE-FS-` signature) and require the user to confirm they hold the recovery key; never resize NTFS | boot-firmware-and-disk.md §Dual boot |
| P2-11 | ISO output | Hybrid ISO via `xorriso`: El Torito EFI image (FAT image containing ESP contents) + GPT for USB; optional BIOS compat via GRUB only for compat profile (Phase 6) | boot-firmware-and-disk.md §Live media |
| P2-12 | Live preview | Preview pool: KVM VMs started from the built qcow2 via a copy-on-write overlay; VNC → websockify → noVNC in browser; 15-minute sessions; 1 GiB RAM kiosk / 4 GiB desktop; pool pre-warmed; per-org quota | infrastructure-and-cost.md §Preview |
| P2-13 | Scheduler v1 | Deficit round robin per org with plan weights, binary heap priority inside org, admission token bucket, rendezvous hashing for cache affinity, multi-host | algorithms-and-data-structures.md §Scheduling |
| P2-14 | Cache tiers | L1 host NVMe cache (size-aware W-TinyLFU), L2 object storage, apt/package proxy cache, stage outputs keyed by Merkle keys | caching.md |
| P2-15 | Spec versions & diff | Immutable spec versions, JSON Patch (RFC 6902) between versions, UI timeline, "build this version", undo/redo (command pattern) | algorithms-and-data-structures.md §Undo |
| P2-16 | Templates | Kiosk, School lab, Office laptop, Thin client, Developer workstation; start-from-template creates a spec version | — |
| P2-17 | Hardware compatibility hints | Catalog of tested devices (from lab + pilots); spec can target device models; build adds model-specific firmware packages and warns on untested combos | fleet-and-device-agent.md §HCL |
| P2-18 | Usability tests | 5 sessions with non-technical pilot staff; fix top issues | — |

## Acceptance tests
1. Constraint tests: 500 random selections; Go and Web Worker solvers agree 100% (property test with shared CNF).
2. UNSAT explanation returns ≤ 3 conflicting user choices in ≥ 95% of generated conflicts.
3. Installer: erase-disk and alongside-Windows flows pass in QEMU with a pre-installed Windows evaluation image fixture (licensed for testing) and on 2 lab laptops; Windows still boots afterwards; BitLocker test VM requires confirmation.
4. Preview session starts in ≤ 20 s p95 from a warm pool.
5. p95 warm build ≤ 12 min across 50 builds/day load test.
6. Upload validator fuzzed (go-fuzz/`testing.F`) 24 h with no crash; all corpus attacks rejected.

## Exit gate
Usability goal met by 4 of 5 participants; installer success on all pilot device models.

## Risks
- Live preview cost → strict quotas, suspend VMs idle > 3 min, smallest RAM profile.
- Theme mapping differs per desktop → every theme field declares which profiles support it; UI hides unsupported fields.
