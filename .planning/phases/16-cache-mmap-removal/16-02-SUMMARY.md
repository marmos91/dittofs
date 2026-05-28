---
phase: 16-cache-mmap-removal
plan: 02
subsystem: blockstore
tags: [blockstore, engine, cache, rewire]

requires:
  - phase: 16-cache-mmap-removal
    plan: 01
    provides: LocalStore.Get(ctx, hash) ([]byte, error)
provides:
  - engine.loadByHash reduced to a single content-addressed local.Get call
  - cache.go docstring purged of mmap / readFromCAS / cache_mmap_{unix,windows}.go references
  - cache_test.go large-chunk byte-correctness assertion preserved ahead of Plan 16-03 deletion
affects: [16-03 mmap file + perf-gate deletion]

tech-stack:
  added: []
  patterns:
    - "Engine-internal read primitive is now the unified LocalStore.Get seam (forward-compat with Phase 17 BlockStore.Get)"
    - "Test-fixture migration ahead of file deletion: ports generic asserts before the source test file is removed (D-10)"

key-files:
  created:
    - pkg/blockstore/engine/loadbyhash_test.go
  modified:
    - pkg/blockstore/engine/engine.go
    - pkg/blockstore/engine/cache.go
    - pkg/blockstore/engine/cache_test.go

key-decisions:
  - "loadByHash collapses to a 1-line delegate `return bs.local.Get(ctx, hash)` per D-02; FileBlock.GetByHash, LocalPath/DataSize plumbing, readFromCAS fast-path, and GetBlockData legacy fallback all removed from the closure body"
  - "Cache loadFn signature (LoadByHashFn) is unchanged — NewCache wiring at engine.go untouched"
  - "ErrChunkNotFound now surfaces verbatim from local.Get on miss (previously masked by the bespoke `loadByHash: block not local` errors.New)"
  - "Large-chunk (256 KiB) round-trip ported from cache_mmap_test.go as TestCache_LargeChunkRoundTrip — existing TestCache_GetPut_Basic uses an 11-byte string and does not subsume large-chunk equality"
  - "Mmap-specific assertions (PartialOffset, DestSmallerThanFile, BelowMmapThreshold_UsesReadFile, OffsetAtEOF, EmptyDest, MissingFile, Windows_FallbackPath) deliberately not ported per D-10 — they target a primitive that disappears in Plan 16-03"
  - "errors import retained in engine.go — still used by lines 100, 103, 229, 470 (errors.New + errors.Join)"
  - "cache_mmap_test.go untouched in this plan (Plan 16-03 owns the deletion); the test file still passes against the still-live readFromCAS primitive"

patterns-established:
  - "Behavior-pin RED test for a refactor: stage state that only the post-rewire path can serve (chunk in CAS without a FileBlock row) → the test fails today, passes after the rewire"

requirements-completed: []

duration: ~20min
completed: 2026-05-20
---

# Phase 16 Plan 02: Rewire loadByHash to local.Get Summary

**Collapses `engine.loadByHash` from a 35-line mmap fast-path + legacy-fallback to a single `bs.local.Get(ctx, hash)` delegate; purges mmap references from `cache.go`'s docstring; ports the one generic byte-correctness assertion worth saving from `cache_mmap_test.go` into `cache_test.go` ahead of Plan 16-03's deletion.**

## Performance

- **Duration:** ~20 min
- **Started:** 2026-05-20T13:25:00Z (approx)
- **Completed:** 2026-05-20T13:45:00Z (approx)
- **Tasks:** 2 (Task 1 TDD RED/GREEN; Task 2 single test-only commit per plan judgement clause)
- **Files modified:** 4 (1 created, 3 modified)

## Accomplishments

- `(*BlockStore).loadByHash` reduced to a one-line delegate calling `bs.local.Get(ctx, hash)`. The mmap fast-path branch (`fb.DataSize > 0` → `readFromCAS(fb.LocalPath, 0, buf)`), the FileBlock lookup (`fileBlockStore.GetByHash`), the LocalPath gate (`fb.LocalPath == ""`), and the GetBlockData legacy fallback are all removed from the closure body.
- Cache `loadFn` (LoadByHashFn) signature is unchanged → `NewCache(bs.readBufferBytes, bs.prefetchWorkers, bs.loadByHash)` at `engine.go:182` is untouched. Cache contract preserved externally.
- `cache.go` docstring rewritten: the Plan 12-10 / CACHE-06 multi-paragraph block (~20 lines describing the mmap-vs-ReadFile platform-aware single-copy primitive) replaced with a one-paragraph Phase 16 note describing the RAM-only flow and D-03 buffer ownership.
- New test `TestCache_LargeChunkRoundTrip` in `cache_test.go` ports the 256 KiB byte-equality assertion from `cache_mmap_test.go::TestReadFromCAS_RoundTrip`, reshaped onto the `Cache.Put` / `Cache.Get` surface. Existing `TestCache_GetPut_Basic` was confirmed to use only an 11-byte payload — the large-chunk port is not subsumed.
- New RED-then-GREEN test pair `TestLoadByHash_DelegatesToLocalGet` + `TestLoadByHash_MissingChunkReturnsErrChunkNotFound` pins the D-02 contract (content-addressed read via local.Get; ErrChunkNotFound surfaces verbatim).
- `cache_mmap_test.go` is byte-untouched and still passes against the still-live `readFromCAS` primitive — Plan 16-03 owns its deletion.

## Task Commits

1. **Task 1 RED** — `f744608b` `test(16-02): add failing tests for loadByHash → local.Get rewire` (both tests fail against current impl with `loadByHash: block not local`)
2. **Task 1 GREEN** — `5cb1bd40` `feat(16-02): rewire loadByHash to local.Get; purge mmap from cache.go docstring`
3. **Task 2** — `b0d65d56` `test(16-02): port large-chunk round-trip assertion from cache_mmap_test.go`

No REFACTOR commits — Task 1's GREEN body is already a one-liner; nothing to clean up.

## Files Created/Modified

### Created

- `pkg/blockstore/engine/loadbyhash_test.go` — focused unit tests pinning the Phase 16 D-02 contract: `loadByHash` delegates to `local.Get` and surfaces `ErrChunkNotFound` verbatim. Uses an on-disk `fs.FSStore` with chunks staged via `StoreChunk` (no FileBlock row) so the assertion only passes post-rewire.

### Modified

- `pkg/blockstore/engine/engine.go` — `loadByHash` body collapsed from ~35 lines to a single `return bs.local.Get(ctx, hash)`. Godoc rewritten to call out D-02 (single content-addressed read), D-03 (buffer ownership / Cache copies into LRU slot), and the unchanged "prefetch is best-effort, no remote round-trip here" semantics. `errors` import retained (still used at lines 100, 103, 229, 470).
- `pkg/blockstore/engine/cache.go` — replaced the Plan 12-10 / CACHE-06 docstring block describing the mmap fast-path with a one-paragraph Phase 16 note. No code changes.
- `pkg/blockstore/engine/cache_test.go` — added `TestCache_LargeChunkRoundTrip` (256 KiB Put/Get byte-equality) and the `bytes` + `crypto/rand` imports it requires.

## Decisions Made

All Must-Haves from `<must_haves>` were honored:

- `loadByHash` calls `bs.local.Get(ctx, hash)` — D-02 satisfied at `engine.go`.
- No type assertion on `bs.local`; the interface method (added in Plan 16-01) is called directly.
- Cache `loadFn` signature unchanged.
- Generic large-chunk byte-correctness ported (D-10) — `TestCache_LargeChunkRoundTrip` lands in `cache_test.go`; confirmed not subsumed by existing 11-byte `TestCache_GetPut_Basic`.
- `cache.go` docstring no longer references mmap / syscall.Mmap / readFromCAS / cache_mmap_unix.go / cache_mmap_windows.go.
- Cache copies the returned `[]byte` into its LRU slot per D-03; behavior unchanged from pre-Phase-16 semantics.

## Deviations from Plan

None — plan executed exactly as written.

Task 2 ran as a single `test(...)` commit (no RED gate) because the code under test (`Cache.Put` / `Cache.Get`) already exists and works; the only artifact is the new assertion. The plan's `<behavior>` block explicitly allows planner judgement here ("If review of cache_test.go's existing TestCache_GetPut_Basic reveals it already covers large-chunk equality, do NOT add a duplicate test"). The judgement was: 11 bytes ≠ 256 KiB → port.

## Issues Encountered

None.

## TDD Gate Compliance

- Task 1 RED gate: `f744608b` (test commit). Both tests proven to fail via `go test ./pkg/blockstore/engine/ -run TestLoadByHash` → `loadByHash: block not local`.
- Task 1 GREEN gate: `5cb1bd40` (feat commit). All acceptance gates pass:
  - `grep -q 'return bs.local.Get(ctx, hash)' pkg/blockstore/engine/engine.go` — OK
  - `! grep -n 'readFromCAS' pkg/blockstore/engine/engine.go` — NONE
  - `! grep -n -E 'syscall\.Mmap|readFromCAS|cache_mmap_unix|cache_mmap_windows' pkg/blockstore/engine/cache.go` — NONE
  - `awk loadByHash body | grep fb.LocalPath` — NONE
  - `git diff --stat pkg/blockstore/engine/cache_mmap_test.go` — empty (untouched)
  - `go build ./... && go vet ./pkg/blockstore/engine/...` — exit 0
  - `go test ./pkg/blockstore/engine/... -count=1` — PASS (52.246s)
- Task 2: single `test(...)` commit per the plan's "subsumed by existing coverage" judgement clause — RED gate not required because no production code changes.
- Plan-wide final: `go test ./pkg/blockstore/engine/... -count=1 -race` — PASS (28.205s).

## User Setup Required

None.

## Next Phase Readiness

- **Plan 16-03** (delete cache_mmap_unix.go + cache_mmap_windows.go + cache_mmap_test.go + TestPerfGate_Phase12_MmapHotPath) is unblocked. The engine no longer depends on `readFromCAS`; the test file's deletion is the next action.
- **Plan 16-04** (any residual cleanup / perf re-baseline) inherits a green engine suite under -race.
- **Phase 17** (unified `BlockStore.Get`) adopts the same call shape verbatim — zero rename churn at the loadByHash site.

## Self-Check: PASSED

- Created files present:
  - `pkg/blockstore/engine/loadbyhash_test.go` — FOUND
- Modified files contain expected content:
  - `return bs.local.Get(ctx, hash)` in `engine.go` — FOUND
  - `Phase 16: bytes loaded on miss via local.Get(ctx, hash)` in `cache.go` — FOUND
  - `func TestCache_LargeChunkRoundTrip` in `cache_test.go` — FOUND
- Commits exist on `gsd/phase-16-cache-mmap-removal`:
  - `f744608b` (test RED) — FOUND
  - `5cb1bd40` (feat GREEN) — FOUND
  - `b0d65d56` (test port) — FOUND
- Verification gates green:
  - `go build ./...` exit 0
  - `go vet ./pkg/blockstore/engine/...` exit 0
  - `go test ./pkg/blockstore/engine/... -count=1 -race` exit 0

---
*Phase: 16-cache-mmap-removal*
*Completed: 2026-05-20*
