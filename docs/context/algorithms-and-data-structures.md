# Algorithms and data structures

Each entry: problem → choice → why → complexity → parameters → sketch. Code is Go-like pseudocode.

---------------------------------------------------------------------------------------------------
## 1. Canonical hashing of specs (RFC 8785 JCS + SHA-256)
- Problem: identical specs must have identical IDs regardless of key order/whitespace/YAML vs JSON.
- Choice: JCS canonical JSON, then SHA-256. O(n log n) for key sorting (n = keys per object).
- Pitfall: floats — forbid floats in the schema (use integers/strings) so number formatting can never differ.

## 2. Option validation — SAT over CNF
- Problem: catalog constraints (requires, conflicts, exactly-one groups, provides) + user selections must be consistent; explain conflicts.
- Why SAT: constraints are arbitrary boolean formulas; 2-SAT cannot express "exactly one of N" with requires-chains; catalog size (≤ 5k vars) makes CDCL instant (< 10 ms).
- Encoding:
  - requires a→b: `(¬a ∨ b)`; conflicts: `(¬a ∨ ¬b)`
  - provides p by {o₁..oₖ}: `(¬oᵢ ∨ p)` ∀i and `(¬p ∨ o₁ ∨ … ∨ oₖ)`
  - exactly-one over group G (n items): at-least-one clause + at-most-one via **sequential counter** (Sinz): O(n) clauses and O(n) aux vars instead of O(n²) pairwise.
  - base/arch filter: options not available for the chosen base are unit clauses `¬o`.
  - user selections: unit clauses (soft for explanations, see below).
- Solver: Go `gini` (or `go-sat`); web: MiniSat compiled to WASM (or `logic-solver`). Same canonical CNF JSON input → property test asserts identical SAT/UNSAT on random selections.
- Explanation (MUS over user selections, deletion-based):
  ```
  S = user selections; assert UNSAT(base ∧ S)
  for s in S (ordered by most-recent-last):
      if UNSAT(base ∧ (S \ {s})): S = S \ {s}     // s not needed for conflict
  return S   // minimal: removing any element makes it SAT
  ```
  O(|S|) SAT calls; |S| ≤ ~100 → fine. Use assumptions-based solving to reuse learned clauses.
- Auto-completion: when user picks `session.kiosk`, propagate units (BCP) to show implied selections greyed-in.

## 3. Build DAG scheduling
- Topological order: Kahn's algorithm O(V+E) with a ready-queue.
- Ready-queue ordering: **critical path first** — priority = longest path (sum of historical p50 durations) from node to sink, computed once by reverse topological DP O(V+E).
- Parallelism bounded by VM CPU budget: each stage declares `cpu_weight`; executor admits stages while Σweight ≤ vCPUs.
- Incremental rebuild: a node is "dirty" iff its Merkle key misses all cache layers; dirtiness is implied, no explicit propagation needed because keys include dependency keys.

## 4. Fair multi-tenant scheduling (scheduler)
- **Deficit Round Robin** across orgs: each org has a FIFO/priority queue; quantum Qₒ = base_quantum × plan_weight (free 1, pro 4, business 16); a build's cost = predicted build-minutes. Each round: `deficit += Q`; while head cost ≤ deficit: dispatch, `deficit -= cost`. O(1) amortized per dispatch; starvation-free.
- Within org: binary heap keyed by (priority class, critical path estimate, enqueue time).
- Admission control: **token bucket** per org for build starts (rate r = plan builds/hour, burst b); concurrency cap per org.
- Host selection: **rendezvous (HRW) hashing** on affinity key among hosts with capacity → O(H) per decision (H ≤ few hundred). Weighted variant: score = `-w / ln(U(h,key))` where U ∈ (0,1) derived from hash.
- Bin packing when builds have different resource classes (8/16 vCPU): best-fit decreasing over host free capacity, tie-broken by HRW score.

## 5. Rate limiting — GCRA (generic cell rate algorithm)
- Equivalent to token bucket, single timestamp state per key (TAT) → one Redis key, atomic Lua:
  ```
  T = 1/rate; τ = burst·T
  tat = max(GET key, now)
  if tat - now > τ: reject (retry_after = tat - now - τ)
  else: SET key = tat + T, PX = τ + T; allow
  ```
  O(1) time, O(1) memory per client.

## 6. Content-defined chunking — FastCDC
- Problem: deliver deltas between arbitrary releases without computing per-pair diffs; dedupe storage across releases/tenants' public content.
- Choice: FastCDC with gear rolling hash, normalized chunking (level 2). Parameters: min 16 KiB, avg 64 KiB, max 256 KiB on the uncompressed usr image.
- Sketch:
  ```
  maskS = mask with 18 one-bits (harder, before avg)   // NC level 2 around 2^16 average
  maskL = mask with 14 one-bits (easier, after avg)
  i = min; h = 0
  for i < len && i < avg: h = (h<<1) + GEAR[data[i]]; if h & maskS == 0: return i; i++
  for i < len && i < max: h = (h<<1) + GEAR[data[i]]; if h & maskL == 0: return i; i++
  return min(len, max)
  ```
  O(n), ~1–2 GB/s per core. GEAR = 256 fixed random uint64 (constant in code; changing it changes all boundaries → version the index format).
- Why content-defined: an insertion shifts fixed-size block boundaries, but CDC boundaries resynchronize after the edit, so only nearby chunks change.
- Chunk index `.cidx`: header (format version, total size, image sha256) + array of (offset u64, length u32, sha256 [32]) sorted by offset → binary search by offset; plus a hash map for lookup by digest when seeding.
- Note: erofs compression makes small source changes cascade within compressed clusters; mitigate with fixed per-file layout order (sorted paths) and lz4hc cluster size 64 KiB. Measure delta ratio per release (KPI).

## 7. Delta planning on device
```
have = map[digest] -> (slot offset, len)   // by chunking current usr slot once (cached index for current release)
need = [c in new_index if c.digest not in have]
ranges = coalesce(need, gap_limit=256KiB, max_range=8MiB)   // merge adjacent chunks into fewer HTTP Range requests
for r in ranges (parallel 4): GET chunks pack with Range; verify each chunk sha256; pwrite to staging at c.offset
for c in new_index where c in have: copy from current slot (pread/pwrite or copy_file_range)
verify sha256(staging) == release.usr_sha256  and verity root hash recomputed == release.roothash
```
Chunks are stored in *packs* (e.g., 64 MiB objects, zstd per chunk) so CDN objects are few and Range-friendly; pack index maps digest → (pack, offset, clen).

## 8. Merkle trees
- dm-verity: 4 KiB blocks, SHA-256, salt per image; root hash in signed UKI cmdline; verification O(log n) hashes per block read, cached.
- Release manifest Merkle root over artifact digests → one signature covers all artifacts; inclusion proofs let the console show "this file belongs to release X".
- Audit log: hash chain `hᵢ = SHA256(hᵢ₋₁ ‖ entryᵢ)`; daily anchor of the head hash signed by platform key (tamper evidence).

## 9. Filesystem tree diff (scanner)
- Manifest per image: entries (path, type, mode, uid, gid, size, sha256, xattrs, fcaps) **sorted by path bytes**.
- Diff = merge-join of two sorted lists O(n + m), emitting added/removed/modified (field-wise).
- Sensitive-path classification with a **prefix trie** (radix tree) of rules:
  `/etc/sudoers.d/` → critical, `/etc/systemd/system/` → high, `/usr/lib/systemd/system/` → medium, `/root/.ssh/` → critical, `/etc/ld.so.preload` → critical, `/etc/cron*` → high, `/usr/lib/udev/rules.d/` → medium, `/etc/polkit-1/` → high.
  Lookup O(path length). Also flag: new setuid/setgid, new file capabilities, world-writable files, files differing from their owning package's checksum (dpkg md5sums / package sha256).

## 10. Multi-pattern script scanning — Aho–Corasick
- Build an automaton over suspicious tokens (`/dev/tcp/`, `base64 -d`, `curl `…`| sh`, `nc -e`, `LD_PRELOAD`, `chattr +i`, `iptables -F`, `setenforce 0`, `aa-disable`, crypto-miner pool strings).
- Scan all scripts/units/configs in one pass O(text + matches); matches feed risk features. YARA handles binaries; semgrep handles structured shell rules.

## 11. Bloom / cuckoo filters
- Known-bad file hashes (malware feeds): Bloom filter in scanner memory. For n = 50M, p = 1e-6: m = −n·ln p / (ln 2)² ≈ 1.44e9 bits ≈ 180 MiB, k = (m/n)·ln 2 ≈ 20. Positive → exact lookup in DB.
- L2 cache key existence on build hosts: **cuckoo filter** (supports deletion when GC removes objects), fingerprint 16 bits, ~2–3 bytes/item, false-positive ≈ 2b/2^f.

## 12. Staged rollout bucketing and gates
- Bucket: `b = uint64(SHA256(device_id ‖ release_id)[:8]) mod 10000`; eligible if `b < percent × 100`. Deterministic, stable when percent grows (monotone inclusion), uncorrelated across releases (release_id in hash → same devices aren't always canaries).
- Gate: success proportion p̂ over n devices that attempted; use **Wilson score lower bound**:
  `L = (p̂ + z²/2n − z·sqrt(p̂(1−p̂)/n + z²/4n²)) / (1 + z²/n)`, z = 2.33 (99%). Advance stage if n ≥ 50 and L ≥ 0.995 − tolerance; pause if L < 0.98.
- Rollback rate and crash signals use the same bound.

## 13. Retry with backoff + full jitter
`sleep = rand(0, min(cap, base·2^attempt))`, base 2 s, cap 15 min for devices; avoids thundering herds after outages. Heartbeat interval itself jittered ±20%.

## 14. Keyset pagination
`WHERE (created_at, id) < ($1, $2) ORDER BY created_at DESC, id DESC LIMIT 50` with composite index → O(log n + page) independent of page depth (vs OFFSET O(offset)).

## 15. Device report ingestion (batching)
- In-memory ring buffer per fleetsrv pod; flush on 5k rows or 1 s via `COPY` into the daily partition. Upsert latest state into `devices` with `INSERT … ON CONFLICT (id) DO UPDATE` only when fields changed (compare hash of state blob to skip writes; reduces WAL).

## 16. Undo/redo and spec merge
- Command pattern: each edit = JSON Patch (RFC 6902) with its inverse patch computed at apply time; two stacks; O(1) undo.
- Remix rebase (three-way merge): base B, upstream U, fork F → `patchU = diff(B,U)`, `patchF = diff(B,F)`; apply both; conflict when both modify the same JSON pointer or one modifies inside a path the other removes; arrays of ids treated as sets (merge = union minus removals) to avoid index conflicts.
- Collaboration (Phase 6): Yjs CRDT document mirroring the spec; server materializes canonical JSON on save.

## 17. Risk score
Linear model with hard blockers (see security-threat-model.md §Risk): `score = min(100, Σ wᵢ·fᵢ)`; features computed from diff, scans and behavior. Later: logistic regression calibrated on reviewer decisions; keep explanations (top contributing features) mandatory.

## 18. Search in catalog/apps
Postgres full-text (`tsvector` with weights: name A, summary B, keywords C) + `pg_trgm` for typo tolerance; in-UI fuzzy filter over the already-loaded catalog (≤ 5k items) with a precomputed lowercase index.
