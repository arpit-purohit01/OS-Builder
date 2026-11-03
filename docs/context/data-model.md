# Data model

Authoritative DDL: `db/schema.sql` (migrations generated from it with goose). Postgres 17+.

## 1. Entity overview
```
orgs ─┬─ memberships ── users
      ├─ projects ─┬─ spec_versions ── builds ─┬─ build_events (append-only, partitioned)
      │            │                           ├─ artifacts
      │            │                           ├─ scans ── findings
      │            │                           └─ reviews
      │            └─ releases ── channels ── rollouts
      ├─ assets (uploaded files, content-addressed)
      ├─ keys (references to HSM objects, never key material)
      ├─ enrollment_tokens
      ├─ device_groups ── devices ─┬─ device_reports (partitioned daily)
      │                            ├─ attestations
      │                            └─ device_commands
      ├─ policies (versioned, signed)
      └─ audit_log (partitioned monthly, hash-chained)
catalog_versions, hcl_models, hcl_results (global)
```

## 2. Conventions
- IDs: `text` ULIDs with type prefix (`org_`, `prj_`, `spv_`, `bld_`, `rel_`, `dev_`) → sortable by time, opaque, no enumeration.
- Every tenant table has `org_id` and an index starting with `org_id`. Row Level Security policies enabled (`current_setting('app.org_id')`) as a second line of defense; the app also filters explicitly.
- Timestamps `timestamptz`, UTC. Soft delete only where legally required; otherwise hard delete + audit entry.
- Enums as `text` with `CHECK` constraints (easier migrations than PG enums).
- JSON stored as `jsonb` only for documents (spec, policy, inventory); queryable fields are real columns.

## 3. Hot paths and indexes
| Query | Index |
|---|---|
| Builds list for project, newest first (keyset) | `(project_id, created_at DESC, id DESC)` |
| Scheduler: queued builds per org | River's job table; plus `builds (state) WHERE state IN ('queued','leased')` partial index |
| Review queue | `builds (state, risk_score DESC, created_at) WHERE state='awaiting_review'` |
| Devices by group/filter | `(org_id, group_id, last_seen DESC)`, `(org_id, release_version)`, GIN on `tags` |
| Device lookup by cert | unique `(cert_fingerprint)` |
| Reports time range | partition pruning + BRIN on `ts` |

## 4. Partitioning and retention
| Table | Partition | Retention |
|---|---|---|
| build_events | monthly by `ts` | 13 months |
| device_reports | daily by `ts` | 30 days raw; `device_reports_hourly` 13 months |
| audit_log | monthly | 7 years (compliance), cold storage export after 13 months |
| artifacts (objects) | — | non-released builds: 30 days; released: while referenced + 3 years (GPL sources) |

Partitions created ahead by a daily job (`pg_partman` or our own), dropped by retention job.

## 5. Concurrency patterns
- Build state transitions: `UPDATE builds SET state=$new, updated_at=now() WHERE id=$id AND state=$expected RETURNING ...` + insert into `build_events` in the same transaction.
- Idempotency: `idempotency_keys (org_id, key, request_hash, response, created_at)` unique `(org_id, key)`; 24 h TTL.
- Advisory lock for scheduler leadership: `pg_try_advisory_lock(hashtext('scheduler'))`.
- Counters (download counts) via append + periodic aggregation, not hot-row updates.

## 6. Backups
- Continuous WAL archiving (pgBackRest/WAL-G) to object storage in another region; full weekly, incremental daily; PITR window 14 days.
- Monthly restore drill into staging, automated verification queries.
