---
name: build-sandbox-and-pipeline
description: Use when working on cmd/scheduler, cmd/worker-agent, cmd/builder, internal/dag, internal/cache, internal/queue or internal/sched — build VMs, egress proxy, stage DAG, cache keys, L1/L2 caches, fair scheduling, leases, retries and the build state machine.
---

# Build sandbox and pipeline

Read first: `docs/context/build-pipeline.md`, `docs/context/caching.md`, `docs/context/algorithms-and-data-structures.md` §3–5, §11, `docs/context/memory-management.md` §B.

## Invariants
1. Customer content (spec-derived trees, uploads, scripts, packages) is only ever executed inside an ephemeral build VM.
2. The build VM has no route except to the egress proxy; the proxy allowlist is code-reviewed config (`deploy/proxy/allowlist.yaml`).
3. The VM never sees signing keys, other tenants' private objects, or long-lived credentials. It receives presigned URLs scoped to its own build prefix, valid ≤ wall timeout.
4. Stage outputs are committed atomically (write tmp → fsync → rename for L1; multipart complete for L2) and only after their SHA-256 is verified.
5. Every state transition goes through `store/buildfsm.Transition(ctx, id, from, to, detail)`.

## Adding a stage
1. Define in `internal/dag/stages/<name>/`: inputs, outputs, `ImplVersion` constant, `ConfigSlice(spec) any`, `CPUWeight`, `Timeout`.
2. Cache key: automatically `H("foundry-stage-v1" ‖ name ‖ ImplVersion ‖ JCS(ConfigSlice) ‖ dep keys sorted)`. Put **only** what affects the output into the config slice (too much → cache misses; too little → wrong cache hits = correctness bug).
3. Bump `ImplVersion` whenever stage code or tool versions change (lint enforces).
4. Add unit test for key stability (golden key for a fixed input) and an integration test that a second run hits cache.

## Scheduler changes
- DRR quantum per plan weight; cost = predicted minutes from `stage_runs` history (p50 per stage × miss probability).
- Rendezvous host choice on affinity key = S3 key. Test: removing a host remaps only its keys (property test).
- Leases: 5 min, renewed every 60 s by worker-agent; expiry → `leased|building → queued` with `attempt+1` if retryable; max 3 attempts.
- Never hold a DB transaction while talking to a host.

## Worker-agent / VM lifecycle
```
lease → verify envelope sig/expiry → create overlay qcow2 on golden image → create scratch raw (sparse)
→ start QEMU (-sandbox on, -nodefaults, virtio-blk/net/serial/vsock only, tap on isolated bridge)
→ stream virtio-serial logs to api (batched, 64 KiB max line, redacted) → wait exit or timeout
→ collect status → destroy VM → blkdiscard + delete overlay/scratch → release lease
```
Reaper (every minute): kill QEMU processes without a live lease or older than wall timeout + 30 min; delete orphan overlays.

## Memory/IO rules
- Stream everything image-sized; `io.CopyBuffer` with pooled 1 MiB buffers; hash while copying.
- Multipart: 64 MiB parts × 4 concurrency; global semaphore per process.
- L1 eviction: size-aware W-TinyLFU, watermarks 90%/80%, pinned refs for in-use objects.

## Tests to keep green
- `go test ./internal/dag/... ./internal/sched/... ./internal/cache/... -race`
- Integration: kill worker-agent mid-build → re-lease and completion; orphan VM reaper test.
- Egress test: build VM `curl https://example.com` fails and is logged.
