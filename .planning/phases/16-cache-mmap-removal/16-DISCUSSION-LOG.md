# Phase 16: Cache RAM-only (remove mmap read path) - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-20
**Phase:** 16-cache-mmap-removal
**Areas discussed:** Local hash-keyed read surface, Buffer ownership on read, Cold-read perf gate threshold, Test fixture migration

---

## Local hash-keyed read surface

| Option | Description | Selected |
|--------|-------------|----------|
| Add `Get(hash)` to `LocalStore` now (Recommended) | Add `Get(hash) ([]byte, error)` directly to `pkg/blockstore/local.LocalStore` interface in Phase 16. `*FSStore.Get` wraps existing `chunkstore.ReadChunk`. Phase 17's `BlockStore` interface adopts the same `Get` signature → zero rename churn. Engine.loadByHash calls `local.Get(hash)`. | ✓ |
| Transitional `GetChunk(hash)` | Ship a temporary `(*FSStore).GetChunk(hash)` method that Phase 17 renames to `Get`. Clean separation but forces a rename PR in Phase 17. | |
| Type-assert into chunkstore | Engine type-asserts the `LocalStore` to `*FSStore` and calls `chunkstore.ReadChunk` directly. Leaks internal package boundary. Brittle. Not recommended. | |

**User's choice:** Add `Get(hash)` to `LocalStore` now (recommended)
**Notes:** Forward-compatibility with Phase 17 `BlockStore` interface. Method signature picked to match what Phase 17 will adopt verbatim — at Phase 17, only the receiver type narrows from `LocalStore` to `BlockStore`. No rename PR needed.

---

## Buffer ownership on read

| Option | Description | Selected |
|--------|-------------|----------|
| Fresh allocation per call (Recommended) | Returner allocates fresh `[]byte`, caller owns. Matches today's mmap-then-copy semantics from the cache's perspective. ~one alloc per cache miss. Phase 19 Opt 3 (direct-to-Cache on chunk completion) sidesteps this on the freshly-written hot path. | ✓ |
| sync.Pool for read buffers | Returner allocates from sync.Pool, caller calls `pool.Put(buf)` when done. Less GC pressure. But Cache stores bytes after Put — pooled buffer needs copy into cache slot anyway, defeating the purpose. Premature optimization. | |
| Zero-copy hand-off | Return `[]byte` aliasing internal chunkstore storage. Unsafe — file lifetime not bounded; eviction race. Reject. | |

**User's choice:** Fresh allocation per call (recommended)
**Notes:** Total alloc count from Cache's perspective is unchanged versus current mmap-then-copy. The alloc just moves earlier in the pipeline. Phase 19 Opt 3 will eliminate the alloc on freshly-written content via the direct-to-Cache callback.

---

## Cold-read perf gate threshold

| Option | Description | Selected |
|--------|-------------|----------|
| Strict 1.02 on existing warm bench, skip cold bench (Recommended) | Keep `BenchmarkRandReadVerified` as-is (warm-cache: first read populates LRU, subsequent reads hit `Cache.Get`). Warm path never touched mmap → ratio should be ~1.0. Gate ≤1.02. Skip dedicated cold-cache bench in Phase 16; revisit if production complaints arise. | ✓ |
| Add cold-cache variant, gate ≤1.10 | New `BenchmarkRandReadVerified_ColdCache` that clears LRU before each read, allowing for ~1 disk seek + 1 alloc per read. More honest about cold-path cost; more bench to maintain. | |
| Split warm 1.02 + cold 1.10 | Two gates side by side. Most accurate signal; most complexity. Useful only if cold-read regressions surface. | |

**User's choice:** Strict 1.02 on existing warm bench (recommended)
**Notes:** Warm-cache is the dominant production path. Cold-cache bench deferred to v0.17+ unless real complaints surface post-ship. D-33 (the mmap-vs-ReadFile gate) is deleted with mmap.

---

## Test fixture migration

| Option | Description | Selected |
|--------|-------------|----------|
| Delete entirely, cherry-pick generic asserts (Recommended) | Mmap-specific tests (readFromCAS round-trip, 64 KiB threshold, page-fault) die with mmap. If a test asserts generic behavior (e.g., 8 MiB chunk reads back byte-identical) move it into `cache_test.go` as a generic Cache test. | ✓ |
| Migrate all to RAM-path equivalents | Every scenario rewritten as a non-mmap test. Threshold/page-fault tests become meaningless or trivial. More code, marginal coverage. | |
| Restructure as `cache_ram_test.go` | Dedicated file for RAM-specific Cache behaviors (fresh-alloc correctness, alloc-per-miss accounting). Justified only if RAM path has surprising failure modes — unlikely for pure `[]byte` LRU. | |

**User's choice:** Delete entirely, cherry-pick generic asserts (recommended)
**Notes:** Generic byte-correctness asserts (e.g., 8 MiB chunk round-trip) migrate to `cache_test.go` during the deletion PR. Mmap-specific scenarios (threshold, page-fault) drop entirely. No `cache_ram_test.go` — existing `cache_test.go` covers RAM semantics.

---

## Claude's Discretion

- Exact signature of `local.Get(hash ContentHash)` vs `Get(ctx, hash)` — pick whichever matches existing `LocalStore` method conventions (most methods take `ctx` first).
- Whether to also add `Has(hash)` to `LocalStore` in Phase 16 alongside `Get`. Cheap if added now; otherwise Phase 17 adds it as part of the unified `BlockStore` interface.
- Whether to merge `pkg/blockstore/engine/perf_bench_unix_test.go` into `perf_bench_test.go` post-D-33-deletion if the Unix-specific file becomes effectively empty.

## Deferred Ideas

- **Cold-cache benchmark suite** — deferred to v0.17+ unless cold-read regressions surface post-ship.
- **sync.Pool for read buffers** — revisit in Phase 19 with benchmarks if Phase 19 Opt 3 doesn't fully eliminate the hot-path alloc.
- **`cache_ram_test.go` restructure** — not justified for Phase 16; revisit only if a real failure mode emerges.
- **macOS mmap unlink-race investigations** — moot post-Phase-16. Drop any open issues tracking mmap behavior on Darwin.
