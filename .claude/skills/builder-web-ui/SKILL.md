---
name: builder-web-ui
description: Use when working on web/ — the no-code OS builder, theme studio, layout editor, review console, fleet console, installer UI, live preview client, solver Web Worker, undo/redo, AI assistant or collaboration features.
---

# Builder web UI

Read first: `docs/context/osspec-format.md`, `docs/context/algorithms-and-data-structures.md` §2, §16, `docs/context/memory-management.md` §B6, `docs/context/api.md`.

## Stack
React 19 + TypeScript (strict) + Vite; state: Zustand store holding the spec document + derived UI state; data fetching: TanStack Query; forms: controlled components bound to JSON pointers; tests: Vitest + Testing Library + Playwright for flows; types generated from the JSON Schema (`make gen-types`).

## Architecture
- **Spec store**: the only source of truth. Every edit is a command `{patch: JsonPatch, inverse: JsonPatch, label}` → apply → push undo stack (cap 200) → schedule validation.
- **Validation worker** (`web/src/workers/validate.ts`): loads JSON Schema (ajv, compiled once) + catalog CNF (transferable ArrayBuffer) + MiniSat WASM; on each change (debounced 150 ms) returns `{errors, implied, conflicts(MUS)}`. Server validation on save is authoritative; show server errors at the same JSON pointers.
- **Levels**: Simple / Custom / Expert switch per step; fields declare their level in catalog UI metadata; Expert fields show a "requires review" badge.
- **Steps**: Foundation → Look & feel → Apps → Security & access → Ecosystem → Branding → Review & build. Each step is a route; deep links to JSON pointers for errors.
- **Live preview**: noVNC client in a panel; session token from API; reconnect with backoff; show remaining session time.
- **Build page**: SSE log stream (virtualized log view, ANSI colors, stage timeline), artifacts with checksums and install instructions per OS (Windows: Rufus/Etcher; macOS: Etcher; Linux: `dd`).

## UX rules (from usability tests — keep them)
- Plain language; never show package names in Simple level.
- Every conflicting choice explains why and offers a one-click fix (from MUS).
- Defaults always produce a valid, bootable OS.
- Destructive or security-reducing choices (Secure Boot off, developer root) require a confirm dialog explaining consequences.

## Accessibility
Real `<button>`, `<a>`, `<label>`; keyboard navigation for the layout editor (arrow keys move items, Enter picks up/drops); WCAG AA contrast; `aria-live` for validation results; focus management on step change.

## AI assistant (Phase 6)
Model output must be a JSON Patch against the current spec, validated by schema + SAT + policy in the worker before showing a diff; the user applies it as one undoable command. Never auto-apply; never send secrets or asset contents to the model.

## Performance budgets
Initial JS ≤ 250 KB gzip for the builder route; validation round-trip ≤ 50 ms p95 for catalogs ≤ 5k options; catalog lists virtualized.
