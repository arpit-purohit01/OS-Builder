---
name: osspec-schema-change
description: Use when adding, changing, renaming or removing any field of the Foundry OSSpec (the OS specification), or when a builder UI option needs a new spec field. Covers schema, Go types, defaults, validation, compiler mapping, golden tests, catalog, UI and docs.
---

# OSSpec schema change

Read first: `docs/context/osspec-format.md`, `docs/schemas/osspec.v1alpha1.schema.json`, `internal/osspec/`.

## Procedure (do every step; skipping one breaks invariants)
1. **Decide the shape.** Name in camelCase, place it in the right section (`look`, `apps`, `security`, `access`, `kernel`, `memory`, `ecosystem`, `branding`, `hardware`, `expert`). Prefer enums over free strings, integers over floats (floats are forbidden — canonical hashing). Anything that lets a user inject arbitrary content belongs under `expert`.
2. **Schema.** Edit `docs/schemas/osspec.v1alpha1.schema.json`: type, enum, pattern, `maxLength`, `maxItems`, `default`, `description`. Keep `additionalProperties: false`.
3. **Go types + defaults.** Update `internal/osspec/types.go` and `Default()`. Default must preserve today's behavior for existing specs.
4. **Semantic rules.** Add cross-field checks in `internal/osspec/semantic.go` with a new stable code `SEM-0NN` and a human message. Add table tests for valid and invalid cases.
5. **Catalog impact.** If the field selects catalog options, add/modify `catalog/*.yaml` entries (requires/conflicts/oneOf) and run `go test ./internal/catalog/...` (includes the Go↔WASM solver agreement property test).
6. **Compiler mapping.** Implement in `internal/compiler/<section>.go`. Pure function: no I/O, no time, sorted iteration. Add a row to the mapping table in `osspec-format.md` §5.
7. **Golden tests.** Add/extend a spec in `test/golden/specs/` exercising the field; `make golden-update`; inspect the diff line by line; commit goldens.
8. **Migration.** If renaming/removing/changing meaning: bump `apiVersion` minor (alpha N+1), write `Migrate()` with tests (old fixture → new fixture), keep reading the old version.
9. **UI.** Add the control in the matching builder step (`web/src/steps/<step>/`) at the right level (Simple/Custom/Expert), wired to the spec store via a JSON Patch command; regenerate TS types (`make gen-types`).
10. **Security review.** If the field affects boot, kernel cmdline, access, network exposure, signing, packages or scripts: label PR `security`, update `docs/context/security-threat-model.md` and risk features if needed.
11. **Docs.** Update `osspec-format.md` (example + mapping) and customer docs.

## Rules
- Never let a field value reach a shell command unquoted; compiler writes files, it does not build shell strings from user input. If a script is unavoidable, pass values via a generated JSON/env file read by a fixed script.
- Paths from users are validated as relative, NFC, no `..`, no NUL/control chars, length ≤ 255 per component.
- Changing the meaning of an existing field without a migration is a breaking change → not allowed.

## Done checklist
- [ ] schema, types, defaults, semantic tests
- [ ] catalog + solver tests (if relevant)
- [ ] compiler + goldens reviewed
- [ ] migration (if relevant)
- [ ] UI control + TS types
- [ ] docs + security note
