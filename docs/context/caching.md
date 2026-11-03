# Caching

## 1. Layers

| Layer | Where | Contents | Size | Eviction | Consistency |
|---|---|---|---|---|---|
| L0 | in-process (api, scanner) | catalog CNF, compiled JSON Schemas, plan limits, TUF metadata | ≤ 128 MiB | size-bounded LRU (ristretto/otter) | key includes version → never stale |
| L1 | build host NVMe | stage outputs (S3 base trees, S4 flatpak repos, S6 disks), package files | 2–4 TiB per host | size-aware W-TinyLFU | content-addressed → immutable |
| L1p | build host | package proxy (apt) cache, read-only virtiofs into VMs | 500 GiB | LRU by last access | content-addressed by Packages sha256 |
| L2 | object storage | all stage outputs, artifacts, chunks | unbounded (retention policy) | lifecycle rules | immutable |
| L3 | CDN | artifacts, chunks, TUF metadata | provider | TTL | immutable objects + short-TTL metadata |
| R | Redis `cache` | API responses (catalog pages, org settings), presigned URL memo | 2–8 GiB | allkeys-lfu | version keys + TTL |
| D | device | current usr slot (chunk seed), downloaded chunks, Flatpak sideload | disk | removed after install | verified by hash |

## 2. Keys

Stage key (Merkle, like Bazel action keys):
```
key(stage) = SHA256( "foundry-stage-v1" ‖ stage.name ‖ stage.impl_version ‖
                     canonical(stage.config_slice) ‖ key(dep₁) ‖ … ‖ key(depₙ) )   (deps sorted by name)
```
- Changing only the wallpaper changes S5's config slice → S5, S6, S7, S8 rebuild; S1–S4 hit cache.
- `impl_version` is bumped manually whenever stage code/tool versions change (CI check: modifying `internal/dag/stages/<name>/` without bumping fails lint).
- Packages: the archive's `Packages` index gives each `.deb` a SHA-256 → stored at `sha256/<hex>` → naturally deduped across tenants (public packages only; tenant-private artifacts are stored under tenant prefixes and never shared).

## 3. Lookup algorithm (builder)
```
for stage in topo_order(dag):
    k = key(stage)
    if L1.has(k):            mount/use L1 copy; L1.touch(k)
    elif L2.exists(k):       stream L2 → L1 (verify sha256 while streaming); use
    else:                    run stage; write output to L1 atomically (tmp + rename);
                             upload to L2 asynchronously (but before build is marked built)
```
- L2 existence checks are batched (one LIST/HEAD round per DAG level) and fronted by a per-host Bloom filter of known keys refreshed every 5 min (see algorithms §Bloom) to skip HEADs for definite misses.
- Concurrent builds needing the same missing stage on one host: **singleflight** per key (one executes, others wait on its result).

## 4. Eviction on L1 (size-aware W-TinyLFU)
- Objects vary from KiB (lock files) to 10 GiB (disks) → plain count-based LRU is wrong.
- Admission: TinyLFU frequency estimate from a 4-bit Count-Min Sketch (depth 4) with periodic halving (aging every `10 × capacity_in_items` increments). A new object is admitted only if its estimated frequency ≥ the frequencies of the victims it would displace (sum of bytes freed ≥ object size).
- Main region: segmented LRU (probation 20% / protected 80% of bytes); window LRU 1% of bytes for new arrivals.
- Pinning: objects used by running builds are ref-counted and never evicted.
- High/low watermarks: start eviction at 90% disk, evict to 80%.

## 5. Routing for cache hits (rendezvous hashing)
Each build has an affinity key = key(S3) (the expensive base tree). Scheduler picks host `argmax_h H(h.id ‖ affinity)·weight_h` among hosts with free slots. Same base → same host → L1 hit. Host removal only remaps keys owned by that host.

## 6. HTTP caching
| Object | Cache-Control | Notes |
|---|---|---|
| `sha256/<hex>` artifacts, chunks | `public, max-age=31536000, immutable` | names never reused |
| TUF `timestamp.json` | `public, max-age=60, stale-while-revalidate=30` | small, polled |
| TUF `snapshot.json`, `targets.json` | versioned file names (`<v>.targets.json`) → immutable | consistent snapshots |
| API GET (catalog) | `private, max-age=0` + `ETag` | 304 on match |
| Presigned downloads | `private, max-age=300` | URL expires in 15 min |

## 7. Stampede protection
- Singleflight in api for expensive reads (e.g., catalog render per version).
- Probabilistic early refresh (XFetch): refresh when `now - Δ·β·ln(rand()) ≥ expiry` (β = 1).
- CDN origin shield enabled to collapse misses for popular new releases.

## 8. Negative caching
- "Package not found in snapshot" cached 10 min per (snapshot, package) to avoid hammering mirrors on retries.
- Never negative-cache auth decisions.

## 9. Invalidation rules
- Content-addressed objects: never invalidated; deleted only by retention GC (mark & sweep from live releases/builds; grace period 7 days).
- Versioned keys (catalog version, policy version) change → old entries age out.
- Mutable small objects (org settings): write-through + delete key in Redis on update, publish invalidation on a Postgres `LISTEN/NOTIFY` channel for L0 caches.

## 10. Metrics
Per layer: hit ratio (requests and bytes), eviction bytes/s, admission rejections, singleflight waits, L2 HEAD calls saved by Bloom filter. Target: warm-build S3 hit ratio ≥ 90%.
