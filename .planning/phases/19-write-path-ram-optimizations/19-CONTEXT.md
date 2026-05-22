# Phase 19: Write-path RAM optimizations - Context

**Gathered:** 2026-05-21
**Status:** Ready for planning
**GH issue:** [#519](https://github.com/marmos91/dittofs/issues/519)
**Milestone:** v0.16.0 (CAS Convergence) — Phase 4 of 4
**Branch:** `gsd/phase-19-write-path-ram-optimizations` (off develop @ `b31b01f7`)
**Depends on:** Phase 18 (shipped 2026-05-21, PR #537 @ `b31b01f7`)

<domain>
## Phase Boundary

Land four independent write-path RAM/temp-store throughput optimizations on the unified CAS BlockStore. All four target the local rollup → CAS pipeline; none touch the Syncer mirror loop, the remote BlockStore, or the read path.

The four opts (per #519):

1. **In-memory hash dedup LRU** — N-slot hash LRU between FastCDC `Next()` and `Put(hash, data)` in `pkg/blockstore/local/fs/rollup.go`. On LRU hit: skip CAS Put + StoreChunk, bump refcount via new `FileBlockStore.AddRef` surface. Win: VM-disk idempotent overwrites (qcow2 zero-fill, log rotation, config rewrites).
2. **Group commit / batched fsync** — replace per-record fsync at `pkg/blockstore/local/fs/appendwrite.go:259` with a 1ms commit pipeline window. Concurrent AppendWrites to the same file share one fsync. Adaptive bypass: queue depth = 1 → fsync immediately. Win: 5–10× small-write throughput on rotational; 2–3× on NVMe.
3. **Direct-to-Cache on chunk completion** — extend `chunkstore.lruTouch` at `pkg/blockstore/local/fs/chunkstore.go:104` to invoke an engine Cache callback injected via `FSStoreOptions.OnChunkComplete`. Eliminates the "wrote then read" disk hop on NFS COMMIT-then-READ.
4. **Eager small-file dedup** — files ≤ FastCDC `min_chunk` (1 MiB) hash whole content in RAM and short-circuit via `metadata.FindByObjectID` BEFORE rollup runs. Hooks into `engine.Flush`'s existing pre-rollup site (alongside `trySpeculativeFileLevelDedup`, Phase 18 D-12). Skips chunker + log + CAS write entirely on hit.

**In scope:**
- All 4 opts above + their dedicated tests/benches
- New surface: `FileBlockStore.AddRef(hash, payloadID, blockRef)` on every metadata backend
- New surface: `FSStoreOptions.OnChunkComplete func(hash ContentHash, data []byte)` callback
- New config knob: `blockstore.local.dedup_lru_size` (default 4096)
- D-21 aggregate gate tightened to ≤1.00 vs Phase 11 baseline
- Closes the `claimBatch` worker pool deprecation cycle from Phase 18 D-16 (TRANSITIONAL-NEXT-MILESTONE: drop `SyncerConfig.ClaimBatchSize` field + pkg/config schema entry)

**Out of scope (Phase 18 boundaries hold):**
- Syncer mirror loop changes — Phase 18 froze it
- Cold-cache benchmarks (`BenchmarkRandReadVerified_ColdCache`) — deferred to v0.17+ per #519
- Pinned hot-tail RAM, tmpfs spill, O_DIRECT, zstd compression, speculative chunker lookahead — deferred to v0.17+ per #519
- Cross-share dedup LRU — relies on existing metadata `FileBlockStore.GetByHash` (µs cost, no RAM gain)
- Any backward-compat shim or feature flag — project rule (no DittoFS prod users; major releases delete legacy in-line)

</domain>

<decisions>

## Implementation Decisions

### PR shape
- **D-01:** Phase 19 ships as a single mega-PR, matching Phase 17 and Phase 18. All 4 opts + new surfaces (`FileBlockStore.AddRef` across 3 metadata backends, `FSStoreOptions.OnChunkComplete`) + benches + config knob + D-21 tightening land together. Internal commit ordering may stage (e.g., additive interfaces → integration → tests/benches) for `git log -p` reviewability, but no commit may leave develop unbuildable and no flag-gated half-state is permitted. Rejected: 4 atomic per-opt PRs (D-21 aggregate gate spans all 4; would need pseudo-flags to validate intermediates); hybrid 2-PR split (no clear semantic boundary that survives `FileBlockStore.AddRef` shared between opts 1 and 4).

### Opt 1 — In-memory hash dedup LRU
- **D-02:** LRU scope = per-FSStore (= per-share by architecture invariant). Each share owns its own `LocalStore`/`FSStore`/`RemoteStore`; LRU lives on `FSStore` and is automatically per-share. Matches the existing engine `Cache` pattern (Cache is also per-share). Rejected: per-payload (too narrow — misses cross-file idempotent rewrites in same share); explicit global LRU (would require shared infrastructure across FSStore instances, contradicts the per-share invariant). Cross-share dedup is *already* handled by `FileBlockStore.GetByHash` (Postgres index seek / Badger keyed Get / memory map lookup — all µs). The LRU's job is to avoid even that µs hop on hot hashes, not to be a cross-share dedup oracle.
- **D-03:** LRU size = config knob, default 4096 slots. Surfaced via `pkg/config` as `blockstore.local.dedup_lru_size`. Matches existing config-knob patterns (e.g., `SyncerConfig.ClaimBatchSize` precedent). Adaptive-by-RAM rejected as unbench-reproducible at this milestone.
- **D-04:** LRU hit refcount path = new surface `FileBlockStore.AddRef(ctx, hash, payloadID, blockRef) error`. Returns `ErrUnknownHash` if hash is not yet in metadata (caller falls back to full Put). Implemented across all 3 metadata backends (`badger`, `postgres`, `memory`). Hit flow: rollup obtains hash from LRU → `AddRef` → skip `StoreChunk` + skip remote `Put` → block joins the payload's `[]BlockRef` like any other. Rejected: implicit refcount via `FileBlockStore.Put` idempotency (overloads Put semantics; surface-hidden); reusing `FindFileBlockByHash` + `IncrementRefCount` separately (TOCTOU race against concurrent `engine.Delete` cascade).
- **D-05:** LRU implementation = `golang.org/x/sync/singleflight`-friendly stripe-locked LRU (or `groupcache/lru` adapted). Crash semantics: RAM-only; LRU lost on restart. First post-restart write that would have been an LRU hit falls through to the existing `FileBlockStore.GetByHash` µs path — correct, just slightly slower for the first hot-hash. No persistence needed.

### Opt 2 — Group commit / batched fsync
- **D-06:** Commit window = fixed 1ms collect period + adaptive bypass on queue depth = 1. If a single writer is waiting when the window opens, fsync immediately (no latency penalty for single-writer workloads). Under burst, writers landing within the 1ms window share one fsync. No config knob in Phase 19 — defer tunability to post-bench data per the "Claude's discretion" section below.
- **D-07:** Fsync coalesce granularity = per-file log fsync, batched per file. Each `logFile` (per-payload) batches its own concurrent AppendWrites. Different files fsync independently. Matches the existing `lf.f.Sync()` site at `appendwrite.go:259`; preserves the per-file lock ordering invariant (FIX-2, FIX-20) already documented in that file. Rejected: cross-file fsync coalesce via shared journal goroutine — head-of-line blocking risk + more coordination than the win justifies for per-file VM workloads.
- **D-08:** Backpressure shape = caller blocks until its batch fsyncs (synchronous durability contract preserved). NFS COMMIT and SMB Flush callers assume `AppendWrite` durability on return; async ack would break that. The 1ms window is the only added latency for batched callers; depth-1 bypass means single-writer paths see zero added latency. Rejected: async ack via channel (durability contract break is non-starter).
- **D-09:** Implementation site = wrap the `lf.f.Sync()` call at `appendwrite.go:259` in a per-file `groupCommit` coordinator (small struct on `logFile`). Coordinator fields: `pending []chan error`, `timer *time.Timer`, `mu sync.Mutex`. First writer arms the 1ms timer (or fires immediately on depth=1); subsequent writers within the window enqueue and wait on their channel; timer (or last writer hitting a batch cap) calls `f.Sync()` and broadcasts the result. Lock ordering rule (per-file `mu` before `bc.logsMu`) preserved — coordinator never touches `logsMu`.

### Opt 3 — Direct-to-Cache on chunk completion
- **D-10:** Push direction = callback injected via `FSStoreOptions.OnChunkComplete func(hash ContentHash, data []byte, path string)`. Engine constructs `FSStore` with the callback; `chunkstore.lruTouch` invokes it immediately after the on-disk store completes. Matches Phase 18 D-02's `SyncedHashStore` injection pattern (same shape: option-passed surface, no new package coupling, no new goroutine). Rejected: event channel (extra goroutine + buffer surface for marginal decoupling); direct import (breaks layering — chunkstore is a leaf, must not import engine).
- **D-11:** RAM ceiling on Cache push = bounded by engine Cache's existing LRU. No extra cap. Cache already has size-bounded LRU (Phase 16 work); pushed entries evict old ones via the same policy. Rejected: skip-on-large-chunk (FastCDC max 16 MiB chunks are valid cache citizens — they're the most expensive to refetch); skip-under-pressure signal (additional logic not justified pre-bench).
- **D-12:** Callback shape contract: `OnChunkComplete` is invoked exactly once per successful `lruTouch` (post-disk-store, lock-held). If the callback is `nil`, chunkstore behaves identically to today (no-op). Callback MUST be non-blocking on hot paths — implementation is expected to be `Cache.Put` which is bounded by the cache's LRU lock. The callback is invoked with the cached path so the engine can `mmap`-or-copy at its discretion (matches the existing Cache load path).

### Opt 4 — Eager small-file dedup
- **D-13:** Threshold = `FastCDC.MinChunk` (currently 1 MiB). Files at or below this size emit a single chunk anyway when run through FastCDC, so the eager path is pure work elimination. Hardcoded to `MinChunk` to track FastCDC tuning automatically (not a separate knob).
- **D-14:** Hook site = `engine.Flush` pre-rollup hook, alongside the existing `trySpeculativeFileLevelDedup` call site at `engine/engine.go:669`. Eager small-file is the cheap fast-path: compute single-block hash → compute trivial `ObjectID` (= that hash for single-block files) → `metadata.FindByObjectID`. Hit → file-level dedup short-circuit (BSCAS-05); miss → fall through to `trySpeculativeFileLevelDedup` → rollup. No new layering; reuses Phase 18's pre-rollup hook plumbing. Rejected: adapter-side (Phase 18 D-12 already rejected — adapters don't know `ObjectID`); WriteAt-side (complicates hot path; Flush is the natural quiesce point).
- **D-15:** RAM guard = bounded naturally by per-share concurrent `Flush` count. ~1 MiB × N concurrent flushes is bounded by existing Flush concurrency; thousand-file-burst workloads are not Phase 19's target (they're a separate v0.17+ concern). Rejected: explicit semaphore — overengineering for the threshold size.
- **D-16:** Cache interaction: on eager-dedup HIT, populate the engine Cache for the matched hash if the data is in RAM (we just hashed it). On MISS, the data flows into the normal rollup path which now also populates Cache via D-10. Net: every small-file write leaves a warm Cache entry for that file's content.

### Bench / CI strategy
- **D-17:** Bench gating policy = correctness tests gate; perf benches yellow-flag. Hard-gate (must PASS to merge): `TestCache_PopulatedOnRollupComplete` (Opt 3 correctness), `TestSmallFileEagerDedup_BSCAS06` (Opt 4 e2e). Yellow-flag (report ratio, don't block): `BenchmarkRandWriteCAS_IdempotentBytes` (Opt 1 — Put count ≤ N for K identical writes), `BenchmarkAppendWrite_GroupCommit` (Opt 2 — fsync count ≤ writes/groupSize). The hard quantitative gate is **D-21 aggregate** (see D-19).
- **D-18:** Cold-cache deferred bench (`BenchmarkRandReadVerified_ColdCache`) stays deferred to v0.17+ per #519's "Deferred" section. Phase 19 is write-path; cold-cache is read-path. Scope hygiene over completeness.
- **D-19:** D-21 aggregate gate tightened from ≤1.02 to **≤1.00 vs Phase 11 baseline** (no regression allowed, win required). Phase 19's entire purpose is write-path throughput improvement; if the four opts can't even keep parity, the LoC isn't justified. D-41 (cross-VM dedup ≥40%) and D-43 (RandRead warm-cache ≤1.02) gates unchanged.

### Test infrastructure
- **D-20:** New benches live in `pkg/blockstore/local/fs/` (`appendwrite_group_commit_bench_test.go`, `rollup_idempotent_dedup_bench_test.go`) and `pkg/blockstore/engine/` (`cache_populated_on_rollup_test.go`, `small_file_eager_dedup_test.go`). All four also feed into a new `internal/bench/phase19_test.go` aggregate runner that emits the D-21 ratio for CI.
- **D-21:** `FileBlockStore.AddRef` conformance lives in `pkg/metadata/storetest/` per the metadata store contract invariant (CLAUDE.md). All 3 backends (badger/postgres/memory) must pass the new conformance scenarios before merge. Scenarios cover: AddRef on existing hash (RefCount +1, state preserved); AddRef on missing hash (returns sentinel error, no row created); concurrent AddRef vs DecrementRefCount cascade (no negative RefCount, no orphan).

### Code structure
- **D-22a:** New files for Opt 1 + Opt 2:
  - `pkg/blockstore/local/fs/dedup_lru.go` — stripe-locked hash LRU type + `Get/Put/Has` ops. Unit-testable in isolation; clean git blame.
  - `pkg/blockstore/local/fs/groupcommit.go` — per-file group-commit coordinator struct + 1ms timer arming. Lives in its own file so the locking-order documentation block in `appendwrite.go` stays focused on append semantics.
  - Rejected: inline in `rollup.go` / `appendwrite.go` (files grow ~100 LoC each, dilute call-site readability); combined `ramopts.go` (mixes LRU + group-commit + future opts in one bag — anti-pattern).

- **D-22b:** `FileBlockStore.AddRef(ctx, hash, payloadID, blockRef) error` joins the existing `FileBlockStore` interface in `pkg/metadata/file_block_store.go`. Expands META-03 from 6 to 7 methods. All 3 backends (`badger`, `postgres`, `memory`) implement directly; conformance scenarios in `pkg/metadata/storetest/` (D-21). Rejected: optional sub-interface `FileBlockStoreAddRef` (ugly type-assert at hot call site, no real backend opts out); free function composing `Get + IncrementRefCount` (D-04 already rejected — TOCTOU race against concurrent `engine.Delete` cascade).

- **D-22c:** Config knob naming:
  - **`blockstore.local.dedup_lru_size`** (Opt 1 size, default 4096) — nested under existing `blockstore.local.*` shape (matches `blockstore.local.path`, `blockstore.local.retention`, etc.).
  - Group commit window: **NO knob in Phase 19.** Hardcode `const groupCommitWindow = 1 * time.Millisecond` in `groupcommit.go`. Per D-06 — defer knob until bench data justifies tuning. No TRANSITIONAL marker either; the const is self-explanatory.

### Cleanup folded into Phase 19
- **D-23:** Close the `claimBatch` worker pool deprecation from Phase 18 D-16: delete `SyncerConfig.ClaimBatchSize` field, drop the `pkg/config` schema entry, remove the no-op test that asserts it parses. Single cleanup commit at the head of the mega-PR. TRANSITIONAL-NEXT-MILESTONE marker in `pkg/blockstore/doc.go` updated accordingly.

- **D-24:** Dead `LocalStore` admin-method sweep post-Opt 1+2. Audit `pkg/blockstore/local/fs/*.go` for methods whose only callers were Phase 18 transitional code paths now deleted (e.g., callers that lived in the legacy Syncer orchestrator). Anything with **zero in-tree callers** post-Opt 1+2 wiring gets removed in the same mega-PR. Phase 18 D-18 preserved the admin-superset (Truncate, EvictMemory, SetRetentionPolicy, etc.) intentionally — those STAY. This sweep targets only newly-dead surface introduced or left over by Phase 18 wave deletions. If the audit finds nothing, the sweep is a no-op commit (acceptable; the audit itself is the deliverable).

- **D-25:** Grep + resolve every `TRANSITIONAL-NEXT-MILESTONE:` marker across `pkg/blockstore/`. For each marker:
  - If addressed by Phase 19 work — delete the marker comment alongside the change.
  - If still deferred — leave the marker AND update the comment with the actual target milestone (e.g., `TRANSITIONAL-V0.17:` if v0.17 will address it).
  Convention (Phase 18 D-19) preserved: `TRANSITIONAL-NEXT-MILESTONE:` is the generic forward-pointer; this sweep clarifies which are still generic vs which have a known target. Documented update in `pkg/blockstore/doc.go`.

- **D-26:** Add `TRANSITIONAL-NEXT-MILESTONE:` comments at the v0.17+ hook sites listed in #519's "Deferred" section:
  - `pkg/blockstore/local/fs/chunkstore.go` near the on-disk store path: pinned hot-tail RAM hook anchor.
  - `pkg/blockstore/local/fs/appendlog.go` (or the log overflow site): tmpfs spill anchor.
  - `pkg/blockstore/local/fs/appendwrite.go` near the `f.Sync()` site: O_DIRECT anchor.
  - `pkg/blockstore/local/fs/chunkstore.go::StoreChunk`: zstd compression anchor.
  - `pkg/blockstore/engine/cache.go`: cold-cache prefetch anchor.
  Markers reference #519's "Deferred to v0.17+" so future grep finds the source rationale.

### State-machine invariant preservation
- **D-27:** Opt 1 LRU hit path MUST honor STATE-01..03 (three states `Pending → Syncing → Remote` only). On LRU hit:
  - The matched block is already in the metadata store (LRU populated only after successful `Put`); its `BlockState` is whatever the previous insertion left it as (typically `Remote`).
  - `AddRef` increments `RefCount` ONLY. State unchanged.
  - No new block row created; no new state transition fired.
  Rejected: skip-Pending optimization (STATE-01 violation — every block must visit Pending at creation; AddRef does NOT create a block, it references an existing one, so this isn't even an exception); new `DedupReference` state (contradicts STATE-01 "three states only").

### Claude's discretion (planner's call)
- Exact internal commit wave ordering within the mega-PR (additive surfaces → consumers wired → benches/tests → cleanup sweeps D-23/24/25/26), as long as `go build ./... + go vet ./... + go test ./...` stays green at every wave boundary.
- LRU library choice (groupcache/lru vs hand-rolled stripe-locked) — bench against each other in plan; choose the lower-overhead one. Lives in `dedup_lru.go` either way (D-22a).
- Whether `OnChunkComplete` callback fires before or after `bc.lruTouch`'s internal map update — the contract says "once per successful touch", planner picks the order that doesn't widen the lock window.
- Whether `BenchmarkAppendWrite_GroupCommit` runs in `-race` mode or skips like the existing `D-20 perf gate` does — match the existing pattern from Phase 11's `raceEnabled` constant.
- Exact `FileBlockStore.AddRef` error sentinel name (`ErrUnknownHash` vs `ErrHashNotFound`) and whether it lives in `pkg/metadata/errors.go` or a backend-specific file — naming detail.
- D-24 dead-method sweep result is a no-op if the audit finds nothing — acceptable; the audit *is* the deliverable.
- D-25 grep result: which TRANSITIONAL-NEXT-MILESTONE markers get a concrete target version vs stay generic — planner judgment per marker.

</decisions>

<canonical_refs>

## Canonical References

These docs/specs MUST be consulted by downstream agents (researcher, planner, executor):

- **`.planning/ROADMAP.md`** — Phase 19 entry (line 48) + Phase 17 detail (post-Phase-17 contract) + Phase 18 detail (line 418, mirror loop + dedup hook relocation).
- **`.planning/REQUIREMENTS.md`** — v0.15.0 BSCAS-01..06, LSL-01..08, CACHE-01..06, META-01..04, DEDUP-01..03, STATE-01..03 (the contracts Phase 19 must preserve).
- **`.planning/PROJECT.md`** — milestone framing.
- **`.planning/phases/18-syncer-simplification/18-CONTEXT.md`** — Phase 18 D-02 (`SyncedHashStore` injection pattern, model for `FSStoreOptions.OnChunkComplete`), D-12 (engine.Flush pre-rollup hook home), D-16 (`claimBatch` deprecation cycle Phase 19 closes), D-18 (LocalStore admin-superset scope).
- **`.planning/phases/16-cache-mmap-removal/16-CONTEXT.md`** — engine Cache RAM-only architecture (Opt 3 push target).
- **`.planning/phases/17-unified-blockstore/17-CONTEXT.md`** — unified `BlockStore` interface contract Opt 1 must preserve when calling `Put`/`AddRef` paths.
- **GitHub issue #519** — Phase 19 tracking issue with the 4-opt scope, per-opt file locations, per-opt benches.
- **GitHub issue #543** — Phase 19 sub-tracking (linked from STATE.md).
- **GitHub issue #515** — v0.16.0 parent tracking issue.
- **Design spec** `~/.claude/plans/reactive-sprouting-moonbeam.md` (locked 2026-05-20) — v0.16.0 4-phase plan; Phase 19 is the final phase.
- **`CLAUDE.md` (repo root)** — architecture invariants (block stores per-share, file handles opaque, WRITE coordinates metadata + block store in fixed order, metadata-store contract in `pkg/metadata/storetest/`).
- **`pkg/blockstore/local/fs/rollup.go`** — Opt 1 mount point (between FastCDC `Next()` and `Put(hash, data)`).
- **`pkg/blockstore/local/fs/appendwrite.go:259`** — Opt 2 fsync coalesce site.
- **`pkg/blockstore/local/fs/chunkstore.go:104`** (`lruTouch`) — Opt 3 push site.
- **`pkg/blockstore/engine/engine.go:669`** (`engine.Flush` pre-rollup hook) — Opt 4 hook site.
- **`pkg/blockstore/engine/dedup.go`** — existing `trySpeculativeFileLevelDedup` private function Opt 4 lives alongside.
- **`pkg/metadata/rollup_store.go` + `pkg/metadata/storetest/`** — `RollupStore` precedent for `FileBlockStore.AddRef` surface shape + conformance suite.

</canonical_refs>

<code_context>

## Codebase Touchpoints

### Reusable patterns
- **`FSStoreOptions.SyncedHashStore` injection (Phase 18 D-02)** — direct precedent for `FSStoreOptions.OnChunkComplete` (Opt 3 D-10). Same shape: option field, nil-safe, no goroutine.
- **`engine.Cache` (Phase 16 work)** — already RAM-only, size-bounded LRU. Opt 3 push target; no new infrastructure needed.
- **`engine.Flush` pre-rollup hook (Phase 18 D-12)** — already calls `trySpeculativeFileLevelDedup` at `engine.go:669`. Opt 4 hooks alongside it.
- **`SyncerConfig` config-knob pattern in `pkg/config`** — model for `blockstore.local.dedup_lru_size` knob.
- **`metadata.RollupStore` + `storetest/` conformance** — exact precedent for `FileBlockStore.AddRef` interface design + cross-backend conformance scenarios.

### Files to touch (planner-confirmed)
- **Create:** `pkg/blockstore/local/fs/dedup_lru.go`, `pkg/blockstore/local/fs/groupcommit.go` (or inline on `logFile`), `pkg/metadata/file_block_store.go` (extend `AddRef`), bench/test files in `pkg/blockstore/local/fs/` and `pkg/blockstore/engine/`.
- **Modify:** `pkg/blockstore/local/fs/rollup.go` (Opt 1 hook), `pkg/blockstore/local/fs/appendwrite.go` (Opt 2 group commit at line 259), `pkg/blockstore/local/fs/chunkstore.go` (Opt 3 callback at line 104, `lruTouch`), `pkg/blockstore/engine/engine.go` (Opt 4 hook at line 669, `OnChunkComplete` wiring), `pkg/blockstore/engine/dedup.go` (Opt 4 eager-path fast-track), `pkg/metadata/{badger,postgres,memory}/...` (`AddRef` implementations), `pkg/metadata/storetest/` (conformance scenarios for `AddRef`), `pkg/config/blockstore.go` (`dedup_lru_size` knob), `pkg/blockstore/engine/syncer.go` + `pkg/config/syncer.go` (D-22 — delete `ClaimBatchSize`), `pkg/blockstore/doc.go` (TRANSITIONAL marker update), `internal/bench/` (D-21 aggregate runner update for ≤1.00).
- **Delete:** `SyncerConfig.ClaimBatchSize` field + its pkg/config schema entry + any no-op assertions (D-22).

### Existing invariants Phase 19 must preserve
- Block stores are per-share (D-02 leans on this).
- File handles are opaque (no Phase 19 changes here).
- WRITE coordinates metadata + block store in fixed order (`metadataStore.WriteFile` → `blockStore.WriteAt`) — Opt 4 short-circuits BEFORE this chain at `engine.Flush`, not during WriteAt; Opts 1/2/3 are all below the WriteAt → rollup boundary so the contract holds.
- Every operation carries `*metadata.AuthContext` — Phase 19 surfaces (`AddRef`, `OnChunkComplete`) all run inside operations that already have AuthContext on the goroutine.
- Lock ordering in `appendwrite.go` (per-file `mu` before `bc.logsMu`) — Opt 2 groupcommit coordinator MUST honor this (D-09 calls it out).

</code_context>

<perf_gates>

## Performance Gates Preserved / Tightened

| Gate | Source | Phase 19 status |
|---|---|---|
| D-21 (RandWrite warm) | Phase 11 baseline, ≤1.02 | **TIGHTENED to ≤1.00** (D-19) |
| D-41 (cross-VM dedup ≥40%) | Phase 13 | unchanged — Opts 1+4 should improve hit rate |
| D-43 (RandRead warm-cache ≤1.02) | Phase 15 | unchanged |
| D-06 (RandReadVerified warm ≤1.02) | Phase 16 baseline | unchanged |
| D-33 (mmap hot path) | deleted Phase 16 | n/a |
| `BenchmarkRandReadVerified_ColdCache` | deferred to v0.17+ | stays deferred (D-18) |

</perf_gates>

<deferred>

## Deferred / Out of Scope

Per #519 "Deferred to v0.17+":
- Pinned hot-tail RAM
- tmpfs spill (memory-backed temp tier for log overflow)
- O_DIRECT for log writes
- zstd compression of chunks
- Speculative chunker lookahead

Per Phase 19 scoping discussion:
- Cross-share dedup LRU — metadata `GetByHash` already µs; LRU per-share is sufficient
- Cold-cache read-path bench (`BenchmarkRandReadVerified_ColdCache`) — read-path scope, stays deferred
- Adaptive LRU sizing by available RAM — bench-reproducibility concern; defer until we have phase 19 numbers
- Group commit config knob — bench data first, knob later if needed
- Cache push pressure signal — current bounded LRU sufficient; add if Phase 19 bench shows cache thrash
- Eager small-file semaphore — concurrent Flush count already bounds RAM; revisit if thousand-file-burst surfaces

</deferred>
