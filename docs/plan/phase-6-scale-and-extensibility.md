# Phase 6 — Scale & extensibility (16 weeks)

## Goal
Remove the "restrictive" ceiling: multiple base systems and architectures, a module SDK so anyone can
extend the OS safely, an Expert mode that exposes almost everything, public publishing and remixing,
and an AI assistant that turns a description into a spec — all still flowing through the Phase 3
trust pipeline.

## Tasks

| ID | Task | Detail | Design doc |
|---|---|---|---|
| P6-01 | Multi-base | Add Ubuntu 26.04 LTS and Fedora (current) base profiles. Catalog package mapping per base; compiler `base` switch; separate snapshot pinning (Ubuntu snapshot service, Fedora compose IDs) | build-pipeline.md §Bases |
| P6-02 | arm64 | Builders on arm64 hosts (native, not emulated); UEFI arm64 targets (servers, laptops with UEFI, Raspberry Pi via EDK2 UEFI firmware); boot matrix adds `aarch64` QEMU virt + AAVMF | boot-firmware-and-disk.md §arm64 |
| P6-03 | Module SDK (sysext) | A module = signed systemd-sysext image (erofs + verity + signature) extending `/usr`, plus optional confext for `/etc`. Manifest: id, version, compatible bases/releases (`ID=`, `VERSION_ID=`/`SYSEXT_LEVEL=` in `extension-release`), permissions requested, units shipped. CLI `fctl module build/publish` | build-pipeline.md §Modules |
| P6-04 | Module marketplace | Publish → Phase 3 pipeline → listing with trust tier, permissions, install count; orgs enable modules in spec; paid modules (rev share) later | security-threat-model.md |
| P6-05 | Expert mode | Raw editor for spec (Monaco with JSON Schema), custom kernel cmdline (allowlist + review), custom kernel config fragments (builds kernel package in a separate cached stage; review required), extra repositories (pinned, keyed, reviewed), raw mkosi drop-ins (review required, verified tier impossible) | osspec-format.md §Expert |
| P6-06 | Legacy BIOS compat profile | GRUB i386-pc + BIOS boot partition (1 MiB, type `21686148-6449-6E6F-744E-656564454649`), no Secure Boot/measured boot, A/B via GRUB env boot counter; flagged "reduced security" | boot-firmware-and-disk.md §Legacy |
| P6-07 | Public publishing | Public OS page (screenshots from preview, description, trust tier, "what's inside" from SBOM/diff, data contacts from behavior run), download via CDN, torrent + webseed, version history | update-system.md |
| P6-08 | Remix | Fork a published spec into your org (license metadata carried), diff vs upstream, rebase onto upstream updates (three-way merge of specs using JSON Patch) | algorithms-and-data-structures.md §Spec merge |
| P6-09 | AI assistant | "Describe your OS": LLM produces a JSON Patch against the current spec, constrained by JSON Schema; result validated by SAT and policy; shown as a diff the user accepts; never bypasses review | builder-web-ui skill |
| P6-10 | Collaboration | Multi-user editing via CRDT (Yjs) over the spec document; presence; comments on fields | builder-web-ui skill |
| P6-11 | Build farm scale-out | Autoscaling KVM host pools per region/arch, spot capacity for non-urgent builds, cache warm-up of popular stage outputs by rendezvous hashing | infrastructure-and-cost.md |
| P6-12 | Consumer tier | Free/pro plans, shim+MOK guided Secure Boot flow, Windows USB writer app (signed) | boot-firmware-and-disk.md §MOK |

## Acceptance tests
1. Boot matrix: {debian-13, ubuntu-26.04, fedora} × {x86_64, aarch64} × {uefi, sb, tpm} green nightly.
2. A third-party module (e.g. a printer driver bundle) is built with the SDK, reviewed, published, enabled in a spec and survives a base update (compatibility check rejects mismatched `SYSEXT_LEVEL`).
3. AI assistant: 200 prompt test set → 100% outputs schema-valid; ≥ 85% judged correct by reviewers; 0 outputs bypass validation.
4. Remix rebase: upstream adds an app, fork changed wallpaper → merged without conflict.

## Exit gate
Two bases and two architectures in production; ≥ 1 external module live; public publishing open to verified orgs.
