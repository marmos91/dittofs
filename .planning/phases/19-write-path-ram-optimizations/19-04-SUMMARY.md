---
phase: 19-write-path-ram-optimizations
plan: 04
subsystem: blockstore
tags: [blockstore, fs, lru, callback, options, additive]

requires:
  - phase: 19-write-path-ram-optimizations
    plan: 03
    provides: "newDedupLRU constructor + dedupLRU type (consumed by FSStore field instantiation)"
  - phase: 19-write-path-ram-optimizations
    plan: 01
    provides: "FileBlockStore.AddRef surface — independent prerequisite for Plan 05's rollup LRU consumer"
provides:
  - "pkg/blockstore/local/fs/fs.go — FSStoreOptions.OnChunkComplete + FSStoreOptions.DedupLRUSize slots"
  - "pkg/blockstore/local/fs/fs.go — FSStore.dedupLRU (instantiated) + FSStore.onChunkComplete (nil-allowed) fields"
  - "pkg/blockstore/local/fs/fs.go — SetOnChunkComplete post-hoc setter (mirrors SetObjectIDPersister precedent)"
affects:
  - "Plan 05 (rollup LRU consumer) — can now call bc.dedupLRU.Get/Has/Put"
  - "Plan 07 (chunkstore lruTouch wire-in) — can now invoke bc.onChunkComplete; engine.go will install via SetOnChunkComplete"

tech-stack:
  added: []
  patterns:
    - "Default-on-zero idiom for new numeric option (size <= 0 → 4096) matches existing OrphanLogMinAgeSeconds / MaxLogBytes precedent"
    - "Optional callback with named-type slot in Options + raw-func setter signature — engine.go can install through structural interface assertion without importing the named type"
    - "FSStore field block 'thematic locality' convention: new fields placed adjacent to related fields (dedupLRU + onChunkComplete next to lruIndex/lruList)"

key-files:
  created: []
  modified:
    - "pkg/blockstore/local/fs/fs.go — FSStoreOptions fields (lines 580-592), FSStore fields (lines 257-271), constructor wiring (lines 648-657), SetOnChunkComplete setter (lines 686-701)"
    - "pkg/blockstore/local/fs/fs_test.go — 6 new TestFSStore_* tests + strings import"

key-decisions:
  - "Added SetOnChunkComplete setter (not optional). Rationale: engine.Cache materializes in BlockStore.Start AFTER cfg.Local has been constructed (engine.go:130, 267 — `bs.cache = nullCache{}` placeholder, replaced inside Start by `realCache := NewCache(...)`). The closure that fires Cache.Put has to capture `bs`, which doesn't exist when FSStoreOptions is built. Plan 07 will install via the setter from engine.NewBlockStore — same lifecycle precedent as SetObjectIDPersister."
  - "Setter parameter spelled as raw func value (not the named-type alias on FSStoreOptions.OnChunkComplete). Mirrors SetObjectIDPersister's structural-interface-friendly signature so engine.go can call through an inline interface assertion without importing the package's named-type spelling."
  - "Setter does NOT lock — Plan 07 is documented as 'install once before serving traffic'. SetObjectIDPersister has persisterMu only because the rollup pool launches BEFORE the persister is installed; chunkstore.lruTouch fires AFTER FSStore construction completes, and engine wiring installs the callback before any chunk activity, so no read/write race exists. Documented explicitly in the setter godoc."

requirements-completed: [D-10, D-12, D-22a]

duration: ~15min
completed: 2026-05-21
---

# Phase 19 Plan 04: FSStoreOptions + FSStore surfaces for Opt 1 LRU and Opt 3 cache-push hook

**Additive Wave-1 commit: `OnChunkComplete func(hash, data, path)` and `DedupLRUSize int` land on `FSStoreOptions`; `dedupLRU` (instantiated) and `onChunkComplete` (nil-allowed) land on the `FSStore` struct; `SetOnChunkComplete` setter mirrors `SetObjectIDPersister`. No consumer wires the new surfaces yet — Plans 05 (rollup LRU) and 07 (chunkstore callback) hook them in Wave 2. `chunkstore.go` and `rollup.go` diffs are empty; D-12 nil-safety contract verified by direct unit test.**

## Performance

- **Duration:** ~15 minutes
- **Started:** 2026-05-21
- **Completed:** 2026-05-21
- **Tasks:** 1 (single-task plan)
- **Files modified:** 2 (1 source, 1 test)

## Accomplishments

- `FSStoreOptions` extended with two slots:
  - `OnChunkComplete func(hash blockstore.ContentHash, data []byte, path string)` (Opt 3 D-10 hook).
  - `DedupLRUSize int` (Opt 1 slot count; default 4096 when zero).
- `FSStore` struct extended with `dedupLRU *dedupLRU` and `onChunkComplete func(...)` fields placed adjacent to the `lruIndex`/`lruList` block.
- Constructor `newFSStoreWithOptionsInternal` wires both fields: `dedupLRU` is instantiated with the default-on-zero idiom (`size := opts.DedupLRUSize; if size <= 0 { size = 4096 }; bc.dedupLRU = newDedupLRU(size)`); `onChunkComplete = opts.OnChunkComplete` is assignment-only (nil-allowed).
- `SetOnChunkComplete(fn ...)` setter added (mirrors `SetObjectIDPersister` at fs.go:639 — same lifecycle, same raw-func signature for structural-interface install from engine.go).
- 6 unit tests in `fs_test.go` PASS:
  - `TestFSStore_DefaultDedupLRUSize_AppliedWhenZero` — default-on-zero idiom verified (maxSize == 4096).
  - `TestFSStore_ExplicitDedupLRUSize_Honored` — explicit override (8192) flows through.
  - `TestFSStore_NilOnChunkComplete_LruTouchUnchanged` — **D-12 nil-safety regression gate**: StoreChunk succeeds with no callback configured; chunk lands on disk; no panic.
  - `TestFSStore_OnChunkComplete_StoredOnConstruction` — non-nil callback passed via Options reaches `bc.onChunkComplete` and fires identically.
  - `TestFSStore_DedupLRU_FieldExists` — regression guard against accidental deletion in future refactors.
  - `TestFSStore_SetOnChunkComplete_PostHocInstall` — post-hoc setter install fires the same callback.

## Task Commits

TDD cadence — RED first, GREEN second. Both commits signed.

1. **Task 1 RED: failing tests for OnChunkComplete + DedupLRUSize surfaces** — `a4aa62fe` (test)
2. **Task 1 GREEN: implement FSStoreOptions + FSStore fields + setter** — `3dbcbd27` (feat)

## Files Created/Modified

### Modified
- `pkg/blockstore/local/fs/fs.go`
  - **FSStoreOptions block** (lines 580-592): two new fields appended after `OrphanLogMinAgeSeconds`. Godoc cites D-10 nil-safety contract and the pkg/config dotted path.
  - **FSStore struct block** (lines 257-271): two new fields inserted immediately after `lruList` (thematic locality with the CAS-chunk LRU). Section header comment names the plan/phase.
  - **`newFSStoreWithOptionsInternal` body** (lines 648-657): default-on-zero LRU instantiation + assignment of the optional callback. Placed at the end of the option-assignment block.
  - **`SetOnChunkComplete` setter** (lines 686-701): new method immediately after `SetObjectIDPersister`. Same raw-func signature shape; godoc documents the no-locking choice (install-once-before-traffic precondition).
- `pkg/blockstore/local/fs/fs_test.go`
  - **Imports:** added `"strings"` (needed by `strings.Repeat` in the StoreChunk fixture).
  - **6 new tests** appended at end of file with a section-header comment block.

## Decisions Made

- **`SetOnChunkComplete` setter added** (not optional). The plan said "If construction-time wiring is sufficient, skip the setter — leave the field assignment-only." Reading engine.go:130 + 267 shows that the engine's `Cache` materializes in `BlockStore.Start`, NOT in `NewBlockStore`. The closure that fires `bs.cache.Put` must capture `bs`, which does not exist when FSStoreOptions is built for the FSStore that is being passed to `NewBlockStore` as `cfg.Local`. Conclusion: **construction-time wiring is NOT sufficient; the setter IS needed**. This mirrors the SetObjectIDPersister precedent (engine.go:156-188) exactly — same lifecycle problem, same structural-interface install pattern.
- **Setter is NOT locked.** SetObjectIDPersister uses `persisterMu` because the rollup pool may have already launched before the engine wires the persister. `onChunkComplete` is read from `chunkstore.lruTouch` (Plan 07's wire-in target), which only fires from `StoreChunk` and `ReadChunk` — both of which the engine quiesces before installing the callback. The "install once before serving traffic" precondition is documented in the setter godoc.
- **Setter parameter spelled as raw func value** (not `FSStoreOptions.OnChunkComplete` alias). Mirrors SetObjectIDPersister so engine.go can install via an inline structural interface assertion without importing the named-type spelling: `if setter, ok := cfg.Local.(interface { SetOnChunkComplete(func(blockstore.ContentHash, []byte, string)) }); ok { ... }`.
- **Test 4 strategy:** Go does not permit `==` comparison of `func` values. The test invokes `bc.onChunkComplete(...)` and asserts a counter incremented — function-identity check via observable side-effect.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Missing `strings` import in fs_test.go**
- **Found during:** Task 1 RED (test compile)
- **Issue:** TestFSStore_NilOnChunkComplete_LruTouchUnchanged uses `strings.Repeat` for hex hash construction (matching chunkstore_test.go precedent), but `fs_test.go` did not import `"strings"`.
- **Fix:** Added `"strings"` to the import block.
- **Files modified:** `pkg/blockstore/local/fs/fs_test.go`
- **Committed in:** `a4aa62fe` (RED commit included the fix).

### Other deviations

None. Plan executed exactly as written, with the setter resolved per the plan's "informed by engine.go FSStore-construction inspection" guidance.

## Issues Encountered

None. The plan was a clean, single-task, single-file additive surface plant. The only judgment call (setter vs construction-only) was resolvable by reading engine.go construction order.

## Wave-1 Additive Constraint — Verified

- `git diff pkg/blockstore/local/fs/chunkstore.go` → **0 lines** (Plan 07 territory; D-22a).
- `git diff pkg/blockstore/local/fs/rollup.go` → **0 lines** (Plan 05 territory).
- Plan 04 touched only `fs.go` (the canonical declaration site) and `fs_test.go` (the canonical fs-package test site).
- `TestFSStore_NilOnChunkComplete_LruTouchUnchanged` proves StoreChunk's lruTouch path behaves identically to pre-Phase-19 with no callback installed — the D-12 contract holds at the chunkstore.go layer untouched.

## User Setup Required

None — pure code change, no external service configuration.

## Next Plan Readiness

- **Plan 05 unblocked** — `bc.dedupLRU.Get(hash)` / `Has(hash)` / `Put(hash, payloadID)` callable from `rollup.go` between `FastCDC.Next()` and the CAS Put path.
- **Plan 07 unblocked** — `chunkstore.lruTouch` can guard-and-invoke `bc.onChunkComplete` once Plan 07 lands the wire-in. Engine wiring (engine.go: `if setter, ok := cfg.Local.(interface { SetOnChunkComplete(...) }); ok { setter.SetOnChunkComplete(func(h, d, p) { bs.cache.Put(h, d) }) }`) goes into the same Plan 07 wave.

No blockers. Build green, all tests pass under default test invocation; Wave-1 additive invariant intact.

## Self-Check: PASSED

- `pkg/blockstore/local/fs/fs.go` — modifications FOUND at expected ranges:
  - `OnChunkComplete func(hash blockstore.ContentHash` → 1 occurrence
  - `DedupLRUSize int` → 1 occurrence
  - `dedupLRU *dedupLRU` → 1 occurrence
  - `newDedupLRU(size)` → 1 occurrence
  - `func (bc *FSStore) SetOnChunkComplete(` → 1 occurrence
- Commits `a4aa62fe` (RED) + `3dbcbd27` (GREEN) — both in `git log --oneline`.
- Final verification suite:
  - `go test ./pkg/blockstore/local/fs/... -run "FSStore_DefaultDedupLRUSize|FSStore_ExplicitDedupLRUSize|FSStore_NilOnChunkComplete|FSStore_OnChunkComplete|FSStore_DedupLRU_FieldExists|FSStore_SetOnChunkComplete" -v -count=1` → 6/6 PASS.
  - `go test ./pkg/blockstore/local/fs/... -count=1` → PASS (no regression).
  - `go build ./...` → exit 0.
  - `go vet ./...` → exit 0.
- Wave-1 additive gates: `git diff pkg/blockstore/local/fs/chunkstore.go | wc -l` = 0; `git diff pkg/blockstore/local/fs/rollup.go | wc -l` = 0.

---
*Phase: 19-write-path-ram-optimizations*
*Plan: 04*
*Completed: 2026-05-21*
