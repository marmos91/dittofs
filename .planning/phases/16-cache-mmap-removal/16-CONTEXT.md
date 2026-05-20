# Phase 16: Cache RAM-only (remove mmap read path) - Context

**Gathered:** 2026-05-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Replace the `syscall.Mmap` zero-copy read path in `pkg/blockstore/engine/cache.go` with a `[]byte` read from the local block store. The Cache becomes pure RAM — `map[ContentHash]*list.Element` + `list.List` LRU, with bytes copied into LRU slots on miss. All LRU sizing, prefetch workers, sequential tracker, `nullCache{}` fallback, and the public `CacheInterface` (`Get/Put/OnRead/InvalidateFile/Stats/Close`) stay unchanged. The mmap files + Unix/Windows build-tag fork + D-33 perf gate are deleted.

This is the first of four phases in v0.16.0 (CAS Convergence). It has the lowest interface surface impact — Cache contract is unchanged externally; only the source-of-bytes for cache misses changes. It validates the Cache contract before Phase 17 swaps the underlying source (legacy `.blk` reader → unified `BlockStore` interface).

**In scope:** mmap deletion, `local.Get(hash)` introduction, `engine.go:221` rewire, conformance test cleanup, warm-cache perf gate validation.

**Out of scope:** unified `BlockStore` interface (Phase 17), legacy `.blk` writer deletion (Phase 17), Syncer simplification (Phase 18), write-path optimizations (Phase 19), cold-cache benchmarks (deferred to v0.17+ unless production complaints surface).

</domain>

<decisions>
## Implementation Decisions

### Local hash-keyed read surface
- **D-01:** Add `Get(hash ContentHash) ([]byte, error)` directly to the `pkg/blockstore/local.LocalStore` interface in Phase 16. `*FSStore.Get` wraps existing `chunkstore.ReadChunk(h)`. Phase 17's `BlockStore` interface adopts the same `Get` signature — zero rename churn at the call site, only the type narrows from concrete `*FSStore` (or `local.LocalStore`) to `blockstore.BlockStore`.
- **D-02:** `engine.loadByHash` at `pkg/blockstore/engine/engine.go:221` swaps `readFromCAS(fb.LocalPath, 0, buf)` for `local.Get(hash)`. No type assertion needed — the interface method is added directly.

### Buffer ownership on read
- **D-03:** `local.Get(hash)` returns a freshly allocated `[]byte` per call. Caller owns. Matches today's mmap-then-copy semantics from the Cache's perspective — Cache copied bytes out of the mmapped region into its LRU slot anyway, so the alloc moves earlier in the pipeline but the total alloc count is unchanged.
- **D-04:** No `sync.Pool` for read buffers in Phase 16. Cache stores bytes after Put — a pooled read buffer would need a copy into the cache slot, defeating the pool. Revisit in Phase 19 (write-path opts) only if benchmarks justify; Phase 19 Opt 3 (direct-to-Cache on chunk completion) sidesteps the alloc on the freshly-written hot path entirely.
- **D-05:** No zero-copy hand-off (returning `[]byte` that aliases internal chunkstore storage). Unsafe — file lifetime not bounded by caller; eviction race during read.

### Perf gate threshold
- **D-06:** Keep `BenchmarkRandReadVerified` as-is (warm-cache: first read populates LRU, subsequent reads hit `Cache.Get`). Warm path never touched mmap → ratio should be ~1.0. Gate at ≤1.02 vs pre-Phase-16 baseline.
- **D-07:** No dedicated cold-cache benchmark in Phase 16. Production workloads are mostly warm; the warm-gate is sufficient. If cold-read complaints arise post-ship, add `BenchmarkRandReadVerified_ColdCache` (clears LRU before each read) in Phase 19 with gate ≤1.10.
- **D-08:** Delete D-33 perf gate (`TestPerfGate_Phase12_MmapHotPath` in `pkg/blockstore/engine/perf_bench_unix_test.go`) — gate measures mmap vs ReadFile, which has no meaning post-removal.

### Test fixture migration
- **D-09:** Delete `pkg/blockstore/engine/cache_mmap_test.go` entirely. Mmap-specific scenarios (readFromCAS round-trip semantics, 64 KiB threshold, page-fault behavior) have no target post-removal.
- **D-10:** If `cache_mmap_test.go` contains generic byte-correctness asserts (e.g., "8 MiB chunk reads back byte-identical via Cache.Get"), cherry-pick those into `pkg/blockstore/engine/cache_test.go` as generic Cache tests during the deletion PR. Don't lose generic coverage; do lose mmap-specific assertions.
- **D-11:** No `cache_ram_test.go` restructure. The RAM path has no surprising failure modes worth a dedicated file — pure `[]byte` LRU semantics are covered by existing `cache_test.go`.

### Claude's Discretion
- Exact signature of `local.Get(hash ContentHash) ([]byte, error)` vs alternatives like `Get(ctx, hash)` — pick whichever matches existing `LocalStore` method conventions (most methods take `ctx` first).
- Whether to add `Has(hash)` to `LocalStore` in Phase 16 too (cheap, may simplify Cache logic). If yes → also subsumed by Phase 17's `BlockStore` interface. If no → wait for Phase 17.
- Whether to merge `pkg/blockstore/engine/perf_bench_unix_test.go` into `perf_bench_test.go` post-D-33-deletion (Unix-specific bench file may be empty after removing the mmap gate; if so, fold).

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### v0.16.0 design (locked spec)
- `~/.claude/plans/reactive-sprouting-moonbeam.md` — v0.16.0 CAS Convergence design spec (locked 2026-05-20). Phase 16 is "Decision 1" section. Source of truth for what's in/out of scope.
- `.planning/ROADMAP.md` v0.16.0 section (lines after "## v0.16.0 — CAS Convergence") — Phase 16 entry + locked decisions + intended outcome.

### GitHub tracking
- https://github.com/marmos91/dittofs/issues/515 — v0.16.0 parent tracking issue
- https://github.com/marmos91/dittofs/issues/516 — Phase 16 sub-issue

### Code under direct modification
- `pkg/blockstore/engine/cache.go` (lines 84–497) — Cache LRU core; keep.
- `pkg/blockstore/engine/cache_mmap_unix.go` (89 lines) — DELETE.
- `pkg/blockstore/engine/cache_mmap_windows.go` (33 lines) — DELETE.
- `pkg/blockstore/engine/cache_mmap_test.go` — DELETE (per D-09).
- `pkg/blockstore/engine/perf_bench_unix_test.go` `TestPerfGate_Phase12_MmapHotPath` — DELETE (per D-08).
- `pkg/blockstore/engine/engine.go:221` `loadByHash` — call site to rewire (per D-02).
- `pkg/blockstore/local/local.go` — add `Get(hash ContentHash) ([]byte, error)` to `LocalStore` interface (per D-01).
- `pkg/blockstore/local/fs/fs.go` (or wherever `*FSStore` lives) — implement `(*FSStore).Get(ctx, hash)` as a thin wrapper over `chunkstore.ReadChunk`.

### Existing helpers to reuse
- `pkg/blockstore/local/fs/chunkstore.go` `ReadChunk(h ContentHash)` (returns chunk bytes from `blocks/<hh>/<hh>/<hex>`) — already exists; `*FSStore.Get` wraps this.
- `pkg/blockstore/local/fs/chunkstore.go:104` `bc.lruTouch(h, ...)` — internal fs-level chunk LRU; orthogonal to engine Cache, no change.
- `pkg/blockstore/objectid.go` `ComputeObjectID` — untouched in Phase 16.

### Related v0.15.0 context
- `pkg/blockstore/engine/cache.go:84` Cache struct — `entries map[ContentHash]*list.Element` + `lru *list.List` (already RAM-only at the LRU layer; mmap is only the loader, not the storage).
- `pkg/blockstore/engine/cache.go:186` `NewCache(maxBytes, workers, loadFn)` — `loadFn` signature is what `loadByHash` satisfies; signature unchanged in Phase 16.
- `pkg/blockstore/engine/engine.go:181–185` Cache construction — `if readBufferBytes > 0 { NewCache(...) } else { nullCache{} }` — unchanged.

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `chunkstore.ReadChunk(h)` — already returns chunk bytes by ContentHash from the CAS layout. `*FSStore.Get` is a one-line wrapper. No new disk-read code needed.
- `Cache` LRU + budget + prefetch machinery — already RAM-based at the LRU layer. Only the load source changes.
- `nullCache{}` Null Object — preserves the `ReadBufferSize: 0` disable path.

### Established Patterns
- `LocalStore` interface methods uniformly take `ctx context.Context` as first parameter — `Get` should follow.
- Cache `loadFn` signature `LoadByHashFn func(ContentHash) ([]byte, error)` (per `cache.go:186` `NewCache`) — engine wires it via closure capturing `local` reference. Phase 16 doesn't change this signature; only changes what the closure body does.
- v0.15.0 perf-gate naming: `TestPerfGate_<Phase>_<Subject>` — Phase 16 deletes the only Phase 12 mmap perf gate; no new perf-gate test introduced.

### Integration Points
- `pkg/blockstore/engine/engine.go:181–185` Cache construction in `BlockStore.Start()` — receives `bs.local` (LocalStore impl) and builds the `loadByHash` closure. This is the single integration point.
- `pkg/blockstore/engine/engine.go:283,364,430,467` — `cache.OnRead` / `cache.InvalidateFile` call sites. Unchanged in Phase 16.
- Build-tag matrix — `cache_mmap_unix.go` has `//go:build unix` (or similar); `cache_mmap_windows.go` has `//go:build windows`. After deletion, no per-OS cache file exists; build matrix Windows + Linux + Darwin should all compile cleanly.

</code_context>

<specifics>
## Specific Ideas

- The `local.Get(hash)` method introduced in Phase 16 becomes one of the `BlockStore` interface methods in Phase 17 verbatim. Naming chosen for forward compatibility (D-01).
- Phase 19 Opt 3 ("direct-to-Cache on chunk completion") makes the read-time allocation moot for freshly-written content — rollup's already-allocated buffer is handed straight into `engineCache.Put`. Phase 16's "fresh alloc per call" is intentionally simple because Phase 19 will optimize the hot case.
- Phase 16 ships a one-PR change. No flag-gated rollout, no dual-path window. Cache mmap path was an internal optimization (no external API surface) so removal is a pure simplification.

</specifics>

<deferred>
## Deferred Ideas

- **Cold-cache benchmark suite** (`BenchmarkRandReadVerified_ColdCache` clearing LRU before each read, gate ≤1.10). Deferred to v0.17+ unless cold-read regressions surface post-ship. Phase 16 ships with warm-cache gate only.
- **sync.Pool for read buffers** — deferred. Premature for Phase 16; revisit in Phase 19 with benchmarks if Opt 3 (direct-to-Cache) doesn't fully eliminate the hot-path alloc.
- **`cache_ram_test.go` restructure** — not justified for Phase 16; existing `cache_test.go` covers RAM semantics. Revisit only if a real failure mode emerges.
- **`Has(hash)` on `LocalStore`** — possibly cheap to add in Phase 16 alongside `Get`, but explicitly under Claude's discretion. If skipped, Phase 17 adds it as part of the unified `BlockStore` interface.
- **macOS-specific mmap unlink-race investigations** — moot post-Phase-16. Drop any open issues tracking mmap behavior on Darwin.

</deferred>

---

*Phase: 16-cache-mmap-removal*
*Context gathered: 2026-05-20*
