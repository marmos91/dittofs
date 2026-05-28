---
phase: 16-cache-mmap-removal
verified: 2026-05-20T12:25:22Z
status: passed
score: 6/6 must-haves verified
overrides_applied: 0
---

# Phase 16: Cache RAM-only (remove mmap read path) Verification Report

**Phase Goal:** Replace the `syscall.Mmap` zero-copy read path in `pkg/blockstore/engine/cache.go` with a `[]byte` read from the local block store. Cache becomes pure RAM. Mmap files + Unix/Windows build-tag fork + D-33 perf gate deleted.

**Verified:** 2026-05-20T12:25:22Z
**Status:** PASSED
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths (ROADMAP Success Criteria)

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| SC-1 | `cache_mmap_unix.go`, `cache_mmap_windows.go`, `cache_mmap_test.go` deleted; no `syscall.Mmap`, `readFromCAS`, or `mmap` code references in `pkg/blockstore/engine/` | VERIFIED | All four files absent on disk; repo-wide grep returns zero matches on `syscall\.Mmap\|readFromCAS\|TestPerfGate_Phase12_MmapHotPath\|cache_mmap_\|formatChunkName` |
| SC-2 | `LocalStore.Get(ctx, hash) ([]byte, error)` added to interface; `*FSStore.Get` delegates to `ReadChunk`; fresh allocation per call (D-03) | VERIFIED | `local.go:86` — method on interface with full D-03/D-04/D-05 godoc; `chunkstore.go:147` — one-line `return bc.ReadChunk(ctx, h)` |
| SC-3 | `engine.loadByHash` calls `bs.local.Get(ctx, hash)` with no type assertion; Cache `loadFn` signature unchanged | VERIFIED | `engine.go:205-206` — single-line body `return bs.local.Get(ctx, hash)`; `NewCache` at `engine.go:182` unchanged |
| SC-4 | `TestPerfGate_Phase12_MmapHotPath` removed; `perf_bench_unix_test.go` folded (D-08) | VERIFIED | File absent: `pkg/blockstore/engine/perf_bench_unix_test.go` does not exist; remaining `perf_bench_phase12_test.go` and `perf_bench_test.go` contain no mmap gate |
| SC-5 | `BenchmarkRandReadVerified` ratio ≤1.02 (D-06); cross-OS build passes Linux + Darwin + Windows | VERIFIED | Empirical ratio 0.890 recorded in BENCHMARKS.md; `CGO_ENABLED=0 GOOS=linux/darwin/windows go build ./...` all exit 0 (confirmed in this verification run) |
| SC-6 | Generic byte-correctness asserts ported to `cache_test.go` (D-10); mmap-specific asserts deleted | VERIFIED | `cache_test.go:355-381` — `TestCache_LargeChunkRoundTrip` (256 KiB Put/Get byte-equality); `cache_mmap_test.go` absent |

**Score: 6/6 truths verified**

---

## Decision Coverage (D-01..D-11)

| Decision | Claim | Evidence (file:line or grep) | Verdict |
|----------|-------|------------------------------|---------|
| D-01 | `LocalStore.Get(ctx, hash) ([]byte, error)` ctx-first, forward-compatible with Phase 17 `BlockStore.Get` | `pkg/blockstore/local/local.go:86` | VERIFIED |
| D-02 | `engine.loadByHash` collapses to `return bs.local.Get(ctx, hash)` | `pkg/blockstore/engine/engine.go:205-206` | VERIFIED |
| D-03 | `Get` returns freshly allocated `[]byte`; no aliasing | `local.go:71-86` godoc; `FSStore.Get` delegates to `ReadChunk` which uses `os.ReadFile` (heap-allocated) | VERIFIED |
| D-04 | No `sync.Pool` for read buffers | `grep -n 'sync\.Pool' pkg/blockstore/local/fs/chunkstore.go pkg/blockstore/local/memory/memory.go` returns zero matches | VERIFIED |
| D-05 | No zero-copy aliasing of internal storage | Mutation-based aliasing defense in `RunGetSuite` conformance test; `ReadChunk` returns independent `os.ReadFile` allocation | VERIFIED |
| D-06 | Warm-cache gate ≤1.02 | Ratio 0.890 (post 1,328,307 ns/op / pre 1,492,970 ns/op); BENCHMARKS.md section `v0.16.0 Phase 16 warm-cache baseline (D-06)` | VERIFIED |
| D-07 | No cold-cache benchmark in Phase 16 | No `BenchmarkRandReadVerified_ColdCache` in codebase; explicitly deferred in BENCHMARKS.md | VERIFIED |
| D-08 | `TestPerfGate_Phase12_MmapHotPath` deleted; `perf_bench_unix_test.go` folded | File absent on disk; no `TestPerfGate_Phase12_MmapHotPath` in any `.go` file | VERIFIED |
| D-09 | `cache_mmap_test.go` deleted entirely | File absent: `ls pkg/blockstore/engine/cache_mmap_test.go` fails with ENOENT | VERIFIED |
| D-10 | Generic byte-correctness asserts from `cache_mmap_test.go` ported to `cache_test.go` | `cache_test.go:362` — `TestCache_LargeChunkRoundTrip` (256 KiB); existing `TestCache_GetPut_Basic` uses 11-byte payload, does not subsume | VERIFIED |
| D-11 | No `cache_ram_test.go` file created | `ls pkg/blockstore/engine/cache_ram_test.go` → ENOENT | VERIFIED |

---

## Gates

| Gate | Check | Result | Evidence |
|------|-------|--------|----------|
| 1 | D-01..D-11 decision coverage | PASS | All 11 decisions materialized; see table above |
| 2 | Cross-OS build matrix (`linux`/`darwin`/`windows`) | PASS | `CGO_ENABLED=0 GOOS=linux/darwin/windows go build ./...` all exit 0 (verified in this run) |
| 3 | Race tests `go test -race -count=1 ./pkg/blockstore/...` | PASS | All packages pass; engine: 25.200s, local/fs: 8.707s, local/memory: 2.915s |
| 4 | Mmap purge: `! grep -rn 'syscall\.Mmap\|readFromCAS\|TestPerfGate_Phase12_MmapHotPath\|cache_mmap_\|formatChunkName' pkg/ --include='*.go'` | PASS | Zero matches |
| 5 | File deletions in git history | PASS | Commits `59ccdf26` (3 deletes) + `704f2f34` (1 delete) on `gsd/phase-16-cache-mmap-removal`; 16 phase commits total in log |
| 6 | `cache_ram_test.go` does NOT exist (D-11) | PASS | `ls` returns ENOENT |
| 7 | `CacheInterface` unchanged: `Get/Put/OnRead/InvalidateFile/Stats/Close` | PASS | `cache.go:34-41` — all six methods present with original signatures |
| 8 | Warm-cache perf gate D-06 (ratio ≤1.02) | PASS | Ratio 0.890 from empirical pre/post bench; documented in `test/e2e/BENCHMARKS.md` |

---

## Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/blockstore/local/local.go` | `Get(ctx, hash) ([]byte, error)` on interface | VERIFIED | Line 86 — method present with full D-03/D-04/D-05 godoc |
| `pkg/blockstore/local/fs/chunkstore.go` | `(*FSStore).Get` one-line delegate | VERIFIED | Line 147-149 — `return bc.ReadChunk(ctx, h)` |
| `pkg/blockstore/local/memory/memory.go` | `(*MemoryStore).Get` stub returning `ErrChunkNotFound` | VERIFIED | Line 148-156 — RLock + closed-guard + `ErrChunkNotFound` |
| `pkg/blockstore/engine/engine.go` | `loadByHash` → `bs.local.Get(ctx, hash)` | VERIFIED | Line 205-206 — single-line body |
| `pkg/blockstore/engine/cache.go` | No mmap code; RAM-only docstring | VERIFIED | Docstring at line 52-57 says "No mmap/page-cache fast path"; remaining `mmap` mentions in non-engine.go files are historical comments in plan-flow godoc |
| `pkg/blockstore/engine/cache_test.go` | `TestCache_LargeChunkRoundTrip` (D-10) | VERIFIED | Line 362 — 256 KiB Put/Get byte-equality |
| `pkg/blockstore/engine/loadbyhash_test.go` | `TestLoadByHash_DelegatesToLocalGet` + `TestLoadByHash_MissingChunkReturnsErrChunkNotFound` | VERIFIED | Lines 22 + 70 |
| `pkg/blockstore/local/localtest/suite.go` | `RunGetSuite` with three subtests | VERIFIED | Line 374 — `RunGetSuite` present |
| `test/e2e/BENCHMARKS.md` | `v0.16.0 Phase 16 warm-cache baseline (D-06)` section | VERIFIED | Lines 268-348 — full baseline section with reproduction recipe, result table, and rationale |
| `cache_mmap_unix.go` | DELETED | VERIFIED | File absent |
| `cache_mmap_windows.go` | DELETED | VERIFIED | File absent |
| `cache_mmap_test.go` | DELETED | VERIFIED | File absent |
| `perf_bench_unix_test.go` | DELETED | VERIFIED | File absent |

---

## Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `engine.BlockStore.Start` | `engine.loadByHash` | `NewCache(bs.readBufferBytes, bs.prefetchWorkers, bs.loadByHash)` | VERIFIED | `engine.go:182` — loadFn closure wired |
| `engine.loadByHash` | `local.LocalStore.Get` | `bs.local.Get(ctx, hash)` | VERIFIED | `engine.go:206` — direct interface call |
| `*FSStore.Get` | `chunkstore.ReadChunk` | `bc.ReadChunk(ctx, h)` | VERIFIED | `chunkstore.go:148` |
| `RunGetSuite` | `*FSStore` | `TestFSStore_GetConformance` | VERIFIED | `fs_get_conformance_test.go:16` |
| `RunGetSuite` | `*MemoryStore` | `TestMemoryStore_GetConformance` | VERIFIED | `memory_test.go:25` |

---

## Data-Flow Trace (Level 4)

Cache is an in-memory LRU keyed by `ContentHash`. Level 4 applies to the load path:

| Artifact | Data Variable | Source | Produces Real Data | Status |
|----------|---------------|--------|--------------------|--------|
| `Cache.Get(hash)` | `entry.data []byte` | `loadByHash` → `local.Get` → `chunkstore.ReadChunk` → `os.ReadFile(chunkPath)` | Yes — reads from CAS on-disk block | FLOWING |

`readAtInternal` goes through `bs.local.ReadAt` (payload+offset index) not through the Cache's byte-serving path — Cache is hint-based for prefetch. The Cache Get path (`engine.go:readAtInternal`) correctly reads from local store directly. This is the intended architecture: cache is prefetch-only at this layer (the direct-to-Cache byte-serve was Plan 12-10 / mmap scope that is now deleted). No hollow-prop or disconnected data.

---

## Behavioral Spot-Checks

| Behavior | Result | Status |
|----------|--------|--------|
| `go test -race -count=1 ./pkg/blockstore/engine/...` | ok 25.200s | PASS |
| `go test -race -count=1 ./pkg/blockstore/local/fs/...` | ok 8.707s | PASS |
| `go test -race -count=1 ./pkg/blockstore/local/memory/...` | ok 2.915s | PASS |
| `CGO_ENABLED=0 GOOS=linux go build ./...` | exit 0 | PASS |
| `CGO_ENABLED=0 GOOS=darwin go build ./...` | exit 0 | PASS |
| `CGO_ENABLED=0 GOOS=windows go build ./...` | exit 0 | PASS |
| Mmap symbol purge grep | 0 matches | PASS |
| `cache_ram_test.go` absent | ENOENT | PASS |

---

## Requirements Coverage

Phase 16 has no formal REQ-IDs (per ROADMAP.md: "governed by CONTEXT.md decisions D-01..D-11"). All 11 decisions are verified above. No orphaned requirements.

---

## Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| `engine.go` | 240, 543, 756, 803 | Historical `mmap` mentions in godoc comments referencing Plan 12-10 work | INFO | Documentation only — past-tense architectural notes, not functional code. Do not flag as purge violations. The SC-1 purge gate targets `syscall.Mmap` and `readFromCAS` symbols, not plan-history prose. |

No blockers. No stubs. No orphaned artifacts.

---

## Human Verification Required

None. All phase goals are programmatically verifiable:
- File deletions: confirmed by `ls` ENOENT
- Symbol purge: confirmed by grep
- Build matrix: confirmed by `go build`
- Race tests: confirmed by `go test -race`
- Interface shape: confirmed by reading source
- Perf gate: documented empirically in BENCHMARKS.md (ratio 0.890, single-run; not re-run in this verification but the bench result is committed in a signed commit with the worktree recipe)

---

## Gaps Summary

None. All 6 Success Criteria, all 8 gates, and all 11 decisions are VERIFIED. Phase 16 goal achieved.

---

## Final Verdict

## VERIFIED

All phase deliverables are present, substantive, wired, and tested:

1. Four mmap files deleted; symbol purge is total.
2. `LocalStore.Get` interface method + both backend implementations ship.
3. `engine.loadByHash` is a one-liner calling `local.Get`.
4. `CacheInterface` contract (`Get/Put/OnRead/InvalidateFile/Stats/Close`) is unchanged.
5. Cross-OS build matrix passes (Linux, Darwin, Windows).
6. Race test suite is clean across all blockstore packages.
7. Warm-cache D-06 gate passes with ratio 0.890 (limit 1.02).
8. Generic byte-correctness assertion ported (D-10); no `cache_ram_test.go` created (D-11).

Phase 17 (unified `BlockStore` interface + legacy delete) is unblocked.

---

_Verified: 2026-05-20T12:25:22Z_
_Verifier: Claude (gsd-verifier)_
