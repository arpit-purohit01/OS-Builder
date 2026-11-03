# Foundry — No-Code OS Builder Platform: Engineering Pack

> "Foundry" and "Aurora OS" are placeholder names. Check trademarks before using any name publicly.

This pack is the engineering blueprint for a B2B-first platform that lets organizations build,
sign, install, update and manage their own Linux-based operating system with no code.
It is written to be handed to engineers **and** to AI coding agents (Claude Code skills + context).

## What is inside

```
README.md                         ← you are here
CLAUDE.md                         ← root context for coding agents (copy to repo root)
plan/
  00-roadmap-overview.md          ← phases, timeline, team, KPIs, risk register
  phase-0-foundations.md
  phase-1-mvp-image-pipeline.md
  phase-2-builder-and-customization.md
  phase-3-security-and-trust.md
  phase-4-updates-and-fleet.md
  phase-5-ecosystem-services.md
  phase-6-scale-and-extensibility.md
  phase-7-enterprise-hardening.md
context/                          ← low-level design (read by humans and agents)
  architecture.md
  decisions.md                    ← architecture decision records (ADRs)
  osspec-format.md                ← the OS specification language
  build-pipeline.md               ← stage DAG, sandbox, mkosi compilation
  boot-firmware-and-disk.md       ← UEFI/BIOS, Secure Boot, TPM, partitions, installer, dual boot
  root-access-and-hardening.md    ← root/admin models, kernel variants, sysctls, MAC
  memory-management.md            ← on-device memory + platform memory budgets
  caching.md                      ← cache layers, keys, eviction, CDN
  algorithms-and-data-structures.md
  security-threat-model.md
  update-system.md                ← A/B updates, TUF, deltas, staged rollouts
  fleet-and-device-agent.md
  data-model.md
  api.md
  infrastructure-and-cost.md
  sdlc-and-quality.md
  glossary.md
skills/                           ← Claude Code skills (copy to .claude/skills/)
  osspec-schema-change/SKILL.md
  mkosi-image-compiler/SKILL.md
  boot-chain-and-secure-boot/SKILL.md
  build-sandbox-and-pipeline/SKILL.md
  security-review-pipeline/SKILL.md
  update-and-rollout/SKILL.md
  fleet-device-agent/SKILL.md
  builder-web-ui/SKILL.md
  qa-boot-testing/SKILL.md
  platform-operations/SKILL.md
schemas/osspec.v1alpha1.schema.json
db/schema.sql
examples/kiosk.yaml
examples/school-lab.yaml
examples/office-laptop.yaml
```

## How to use it

1. Create the monorepo (layout in `CLAUDE.md`).
2. Copy `CLAUDE.md` to the repo root, `context/`, `plan/`, `schemas/`, `db/`, `examples/` into `docs/` (keep the relative paths; skills reference `docs/context/...`).
3. Copy every folder in `skills/` into `.claude/skills/` so Claude Code loads them automatically.
4. Work phase by phase. Each phase file lists task IDs (e.g. `P1-07`), the design docs they depend on, acceptance tests and an exit gate. Do not start a phase until the previous gate passes (exceptions are noted).

## Reading order

- Founders / PMs: `plan/00-roadmap-overview.md` → each phase's "Goal" and "Exit gate".
- Engineers: `context/architecture.md` → `context/decisions.md` → the docs your phase references.
- Agents: `CLAUDE.md` → the matching skill → the docs the skill names.

## Assumptions this pack makes (change them deliberately, not silently)

| # | Assumption | Where it matters |
|---|---|---|
| A1 | B2B first: kiosks, school/computer labs, office laptops, thin clients | Phase order, defaults |
| A2 | x86_64 UEFI machines first; arm64 in Phase 6; legacy BIOS only as an optional compat profile | Boot design |
| A3 | First base OS: Debian 13 "trixie" (kernel 6.12 LTS, systemd 257). Ubuntu/Fedora later | Compiler, catalog |
| A4 | Image-based, immutable `/usr` with A/B slots, dm-verity, Unified Kernel Images | Boot, updates, security |
| A5 | Build engine: `mkosi` (pinned release), wrapped by our compiler. We never write OS internals | Build pipeline |
| A6 | Backend Go, frontend TypeScript/React, Postgres, S3-compatible storage | All code |
| A7 | Team of 4–6 engineers in Phases 0–3, growing to ~10 by Phase 6 | Estimates |
| A8 | Every number marked *(estimate)* or *(verify)* must be checked against current vendor docs/prices | Costs, sizes |

## Open verification items (check in Phase 0 spikes)

An independent technical review was run on the boot, build, update, hardening and memory documents, and its
corrections are already applied. These points were left as "verify on the pinned versions":
- whether `CopyBlocks=auto` in the installer also locates the `usr-verity-sig` source partition;
- the exact mkosi spelling of deferred verity signing (`Verity=defer`) and `Seed=` in the pinned mkosi release;
- where sysext signature trust anchors live (`/usr/lib/verity.d/` userspace check vs. kernel keyring) in the pinned systemd;
- `rp_filter` mode per security level on customer networks (VPNs, multi-homed devices).

Versions of third-party tools move. Pin every tool version in `deploy/versions.lock` and re-read upstream
docs (mkosi, systemd-repart, systemd-sysupdate, ukify) for the pinned versions before implementing.
