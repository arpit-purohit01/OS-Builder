# API

REST/JSON over HTTPS, OpenAPI 3.1 spec generated from Go handlers (`api/openapi.yaml`), base path `/v1`.
Internal service-to-service calls use gRPC with mTLS (SPIFFE IDs).

## 1. Authentication
| Caller | Mechanism |
|---|---|
| Browser | OIDC (Authorization Code + PKCE) → session cookie (`HttpOnly; Secure; SameSite=Lax`), CSRF token header for mutations |
| CLI / CI | Personal/Org access tokens (`fdy_pat_…`), hashed (SHA-256) at rest, scoped (`builds:write`, `fleet:read`…), expiry ≤ 1 year |
| Devices | mTLS with device cert (fleetsrv/updatesrv endpoints only) |
| Workers | mTLS + signed job envelopes |

Authorization: role (memberships.role) × resource org check in middleware; per-route required scope; generated tests ensure every route declares auth.

## 2. Conventions
- IDs opaque strings. Timestamps RFC 3339 UTC.
- Pagination: `?limit=50&cursor=<opaque>` (keyset), response `{"items":[…],"next_cursor":"…"}`.
- Errors: RFC 9457 `application/problem+json`: `{"type":"https://docs.foundry.dev/errors/spec-invalid","title":"Spec is invalid","status":422,"detail":"…","errors":[{"pointer":"/security/secureBoot","code":"SEM-004","message":"…"}]}`.
- Idempotency: `Idempotency-Key` on POST; replays return stored response; different body with same key → 409.
- Rate limits: GCRA per token/org; headers `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`; 429 with `Retry-After`.
- ETags on GET of mutable resources; `If-Match` required on PUT/PATCH (optimistic concurrency).

## 3. Endpoints (v1)

### Projects & specs
| Method | Path | Notes |
|---|---|---|
| GET/POST | `/projects` | create: `{name, os_id}` |
| GET | `/projects/{id}` | |
| GET | `/projects/{id}/specs` | spec version list |
| POST | `/projects/{id}/specs` | body: full spec (YAML or JSON) or `{"parent":"spv_…","patch":[…JSON Patch…]}`; returns `spec_version` with digest, validation warnings; 422 with errors |
| GET | `/specs/{id}` | `?format=yaml|json` |
| POST | `/specs/validate` | dry run, returns errors + SAT explanation + implied selections |
| GET | `/specs/{a}/diff/{b}` | JSON Patch |

### Assets
| POST | `/assets/uploads` | `{kind, size, sha256}` → presigned PUT URL + upload id |
| POST | `/assets/uploads/{id}/complete` | triggers validator; returns asset status |

### Catalog
| GET | `/catalog` | `?version=&base=&arch=` options, groups, UI metadata (ETag) |
| GET | `/catalog/{version}/cnf` | canonical CNF for client solver (immutable, long cache) |

### Builds
| POST | `/builds` | `{spec_version_id, arch, priority?}` → 202 `build` |
| GET | `/builds/{id}` | state, stages, tier, risk summary |
| GET | `/builds/{id}/logs` | Server-Sent Events stream (`event: log`, `event: stage`, `event: state`); `Last-Event-ID` resume |
| POST | `/builds/{id}/cancel` | |
| GET | `/builds/{id}/artifacts` | list with presigned GET URLs (15 min) |
| GET | `/builds/{id}/report` | SBOM summary, findings, diff summary, behavior summary |

### Review (reviewer role)
| GET | `/reviews/queue` | sorted by risk, SLA |
| POST | `/builds/{id}/reviews` | `{decision, comment, accepted_findings:[…]}`; second approver enforced server-side when risk > 60 |

### Releases & rollouts
| POST | `/releases` | `{build_id, version}` (build must be `signed`) |
| POST | `/channels/{id}/rollouts` | `{release_id, stages:[{percent, min_devices, min_hours}], gate:{min_success:0.995}}` |
| POST | `/rollouts/{id}/{pause|resume|halt}` | |

### Fleet
| GET | `/devices` | filters: group, release, attestation, tag, q (hostname trigram), last_seen_before |
| GET | `/devices/{id}` | inventory, timeline, compliance |
| POST | `/devices/{id}/commands` | `{type, args}`; wipe requires `X-WebAuthn-Assertion` |
| POST | `/enrollment-tokens` | `{group_id, uses, expires_in}` → token shown once |
| PUT | `/policies/{scope_type}/{scope_id}` | new version, signed server-side |

### Device-facing (fleetsrv, mTLS)
| POST | `/v1/enroll`, `/v1/enroll/activate` | §enrollment in fleet doc |
| POST | `/v1/heartbeat` | returns policy version, commands, nonce |
| POST | `/v1/commands/{id}/result` | |
| POST | `/v1/attest` | quote + event log |
| GET | updatesrv `/tuf/<tenant>/…`, `/chunks/packs/<sha256>` | via CDN, public but unguessable tenant path; TUF-verified |

## 4. Webhooks
Events: `build.state_changed`, `review.required`, `release.created`, `rollout.paused`, `device.attestation_failed`, `device.rolled_back`.
Delivery: POST JSON, header `Foundry-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, t + "." + body)>`; retries with exponential backoff for 24 h; receivers must reject `t` older than 5 min.

## 5. Versioning
URL major version; additive changes only within v1; deprecations announced with `Deprecation` and `Sunset` headers ≥ 6 months ahead.
