---
phase: 19-write-path-ram-optimizations
plan: 05
subsystem: blockstore-local-fs
tags: [rollup, lru, addref, hot-path, opt1, fastcdc, hit-fallback]

requires:
  - phase: 19-write-path-ram-optimizations
    plan: 03
    provides: "dedupLRU type with Get/Has/Put"
  - phase: 19-write-path-ram-optimizations
    plan: 04
    provides: "FSStore.dedupLRU field instantiated in newFSStoreWithOptionsInternal"
  - phase: 19-write-path-ram-optimizations
    plan: 01
    provides: "FileBlockStore.AddRef surface + ErrUnknownHash sentinel"
  - phase: 19-write-path-ram-optimizations
    plan: 02
    provides: "AddRef implementations on memory/badger/postgres backends"
provides:
  - "rollup.go chunker emit loop with LRU-consulting AddRef fast-path (Opt 1 consumer)"
affects:
  - "Phase 19 SUCCESS-21 (D-21 idempotent-rewrite throughput gate ≤1.00) — first end-to-end consumer of the hash dedup LRU"

tech-stack:
  added: []
  patterns:
    - "Two-branch LRU/Put control flow with unconditional BlockRef append outside the branch (D-02 manifest invariant)"
    - "Sentinel-driven fallback: errors.Is(err, metadata.ErrUnknownHash) gates the StoreChunk fallback; any other error is wrapped and returned (D-04 surfacing contract)"
    - "Package-level slog.Debug for hot-path logging (no per-FSStore logger field — matches existing rollup.go pattern)"

key-files:
  created: []
  modified:
    - "pkg/blockstore/local/fs/rollup.go — chunker emit loop in rollupFile (was lines 319-337; now lines 319-381)"
    - "pkg/blockstore/local/fs/rollup_test.go — 6 new TestRollup_* tests + programmableFBS helper + sync/atomic import"

key-decisions:
  - "FSStore field used to reach FileBlockStore.AddRef is bc.blockStore (typed blockstore.EngineFileBlockStore), not bc.fileBlockStore as the plan suggested. Confirmed by reading fs.go:81 and grep — the constructor parameter is named fileBlockStore but the struct field is blockStore. Mirrors existing call sites: bc.blockStore.Put, bc.blockStore.ListFileBlocks, bc.blockStore.GetFileBlock."
  - "Logger: package-level slog.Debug, not bc.log.Debugw. The FSStore struct has NO log field; rollup.go already uses slog.Warn/Debug at the package level (rollup.go:189, 308, 353, 386). The plan's bc.log.Debugw spelling was speculative and replaced 1:1 with slog.Debug to match the file's established convention."
  - "Control flow uses a skipStoreChunk bool guard with the LRU/AddRef branch first and the !skipStoreChunk StoreChunk branch second — the exact suggested shape from the plan. The unconditional `blocks = append(blocks, blockRef)` sits OUTSIDE both branches, satisfying the D-02 manifest invariant by construction (no path skips the append)."
  - "blockRef construction hoisted to BEFORE the LRU consult (so the AddRef call has the same BlockRef the manifest will record). Plan suggested the same; kept for clarity and to avoid divergence between the AddRef argument and the manifest entry."

requirements-completed: [D-02, D-04, D-27]

duration: ~30min
completed: 2026-05-21
---

# Phase 19 Plan 05: rollup.go LRU consumer + AddRef fast-path Summary

**Wires the Phase 19 Opt 1 in-memory hash dedup LRU into the FastCDC chunker emit loop in `pkg/blockstore/local/fs/rollup.go`. On LRU hit, `FileBlockStore.AddRef` bumps RefCount on the existing block row and the local CAS Put is skipped; `ErrUnknownHash` falls back to the standard StoreChunk path; any other AddRef error is wrapped and surfaced. The BlockRef manifest append is unconditional (D-02), AddRef leaves BlockState unchanged (D-27), and Debug-level logging only.**

## Performance

- **Duration:** ~30 minutes (RED → GREEN single iteration, no deviation work needed)
- **Started:** 2026-05-21
- **Completed:** 2026-05-21
- **Tasks:** 1 (single-task TDD plan)
- **Files modified:** 2 (1 source, 1 test)

## Accomplishments

- `pkg/blockstore/local/fs/rollup.go` chunker emit loop in `rollupFile` now consults `bc.dedupLRU.Get(h)` between `FastCDC.Next()` and `bc.StoreChunk`. On hit:
  - `bc.blockStore.AddRef(ctx, h, payloadID, blockRef)` is invoked.
  - On success: StoreChunk skipped, `slog.Debug("rollup: LRU dedup hit", ...)` logged.
  - On `metadata.ErrUnknownHash`: log + fall through to the existing StoreChunk path (TOCTOU defense against engine.Delete cascade).
  - On any other error: return `fmt.Errorf("rollup: AddRef: %w", err)` — no silent fallback (D-04 contract).
- After a successful StoreChunk (the LRU miss branch), `bc.dedupLRU.Put(h, payloadID)` seeds the LRU for subsequent idempotent rewrites.
- `blocks = append(blocks, blockRef)` runs UNCONDITIONALLY for every chunk, hit or miss — the D-02 manifest invariant is preserved at the structural level (no code path can skip it).
- 6 new unit tests in `pkg/blockstore/local/fs/rollup_test.go` PASS under `-race`:
  - `TestRollup_FirstChunk_PopulatesLRU` — empty LRU; post-rollup `bc.dedupLRU.Has(H)` is true.
  - `TestRollup_LRUHit_SkipsStoreChunk` — seeded LRU + pre-seeded FBS row + pre-deleted CAS file; AddRef counter +1, CAS file NOT recreated, manifest still has BlockRef for H.
  - `TestRollup_AddRefReturnsErrUnknownHash_FallsBackToStoreChunk` — forced ErrUnknownHash via programmableFBS override; CAS file IS recreated; LRU repopulated by Put.
  - `TestRollup_AddRefError_OtherThan_ErrUnknownHash_Propagates` — forced `errors.New("metadata: postgres down")` via override; `errors.Is(err, simulated) == true`.
  - `TestRollup_ComputeObjectID_UnaffectedByLRUHit` — two payloads identical content; ObjectIDs and BlockRefs byte-identical across LRU miss vs hit (D-02).
  - `TestRollup_LRUHit_NoBlockStateMutation` — pre-seed FileBlock at State=Remote RefCount=5; post-rollup State==Remote RefCount==6 (D-27 STATE-01..03).
- Full `go test -race ./pkg/blockstore/local/fs/... -count=1` passes (~10.9s) — no regression in the existing rollup, append-log, recovery, or eviction tests.
- `go build ./...` + `go vet ./...` across the repo exit 0.

## Task Commits

TDD cadence — RED first, GREEN second. Both commits signed.

1. **Task 1 RED: rollup LRU consult tests** — `2f46e0ff` (test)
2. **Task 1 GREEN: rollup LRU + AddRef fast-path implementation** — `e8833675` (feat)

## Files Created/Modified

### Modified
- `pkg/blockstore/local/fs/rollup.go`
  - **Chunker emit loop** (was lines 319-337, now 319-381): inserted LRU-consult/AddRef fast-path with skipStoreChunk guard; moved `blocks = append(blocks, blockRef)` outside the branch; added `bc.dedupLRU.Put(h, payloadID)` after successful StoreChunk; switched to package-level `slog.Debug` for the two hit-path log lines.
  - **Imports unchanged** — `errors` and `metadata` were already imported by the existing `metadata.ErrRollupOffsetRegression` handler at line 352.
- `pkg/blockstore/local/fs/rollup_test.go`
  - **Imports:** added `sync/atomic`.
  - **6 new tests** appended after the existing `TestRollup_CommitChunks_PersistsObjectID` block, plus:
    - `programmableFBS` — a test-only FBS wrapper around `blockstore.EngineFileBlockStore` with an `addRefOverride` injection point + per-call atomic counter. Used by tests 2, 3, 4 to force success / `ErrUnknownHash` / arbitrary errors without standing up a separate backend.
    - `newFSStoreForRollupLRUTest` — helper that constructs an FSStore backed by `programmableFBS` wrapping a single memory metadata store (used for both the EngineFileBlockStore and RollupStore surfaces, so seeded FileBlocks are visible to both paths).
    - `hashOfSingleChunk` — wraps `blake3ContentHash` for tests; documents the invariant that a payload ≤ `MinChunkSize` (1 MiB) at offset 0 produces a single chunk equal to the payload bytes.

## Decisions Made

- **FSStore field name** is `bc.blockStore` (typed `blockstore.EngineFileBlockStore`), not `bc.fileBlockStore` as the plan suggested. The constructor *parameter* is named `fileBlockStore` but the struct *field* is `blockStore` (see fs.go:81, fs.go:333). Existing call sites use `bc.blockStore.Put`, `bc.blockStore.ListFileBlocks`, etc. The implementation uses `bc.blockStore.AddRef(ctx, h, payloadID, blockRef)` to mirror that convention.
- **Logger** is the package-level `slog`, not a `bc.log` field. The FSStore struct has no `log` field; rollup.go already calls `slog.Warn`, `slog.Debug` at the package level for its existing hot-path logs (lines 189, 308, 353, 386). The plan's `bc.log.Debugw(...)` spelling was speculative; I replaced it 1:1 with `slog.Debug(...)` to match the file's established convention.
- **Control flow** mirrors the plan's suggested `skipStoreChunk bool` shape exactly. The first attempt at a "cleaner" early-return path was discarded because returning early on the hit branch would skip the unconditional `blocks = append(blocks, blockRef)` and quietly violate D-02. The bool flag keeps the manifest append in a single unconditional site at the bottom of the loop body — structurally impossible to skip.
- **blockRef hoisted** to before the LRU consult so the AddRef call receives the exact `BlockRef` value the manifest will record (and the `blocks` slice can append the same value with no allocation duplication). Plan suggested the same shape.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 — Bug] FileBlock struct field is `DataSize`, not `Size`**
- **Found during:** Task 1 RED (go vet)
- **Issue:** Tests 2 and 6 seed a FileBlock row via `mem.Put(ctx, &blockstore.FileBlock{... Size: uint32(len(payload)) ...})`. `go vet` rejected this with `unknown field Size in struct literal of type blockstore.FileBlock`. The struct field is `DataSize` (types.go:166); `Size` is the field name on `BlockRef`, not `FileBlock`.
- **Fix:** Replaced both `Size:` occurrences with `DataSize:` in the FileBlock literals.
- **Files modified:** `pkg/blockstore/local/fs/rollup_test.go`
- **Committed in:** `2f46e0ff` (RED commit, fixed before tests were run).

### Other deviations

- **FSStore field name** (`bc.blockStore` vs the plan's `bc.fileBlockStore`) — documented above under Decisions Made. Not a code change vs. the plan's intent; the plan acknowledged "If not on FSStore directly, locate the closest accessor" and instructed reading fs.go for the exact name.
- **Logger spelling** (`slog.Debug` vs the plan's `bc.log.Debugw`) — documented above under Decisions Made. Same situation: the plan explicitly noted "if bc.log exists or if the Debug log line used a package-level logger" as a SUMMARY output item.

None of the above changed plan semantics — D-02, D-04, D-27 invariants are all enforced.

## Issues Encountered

None. RED→GREEN ran in one iteration after the `DataSize` field fix. The plan's read_first list captured every relevant context (existing emit loop, ComputeObjectID call, FileBlockStore.AddRef shipped by Plan 01, ErrUnknownHash sentinel re-export, dedupLRU type from Plan 03, FSStore.dedupLRU instantiation from Plan 04).

## Verification Results

| Gate                                                                                       | Result |
|--------------------------------------------------------------------------------------------|--------|
| `go vet ./pkg/blockstore/local/fs/...`                                                     | PASS   |
| `go build ./...`                                                                           | PASS   |
| `go vet ./...`                                                                             | PASS   |
| `go test -race ./pkg/blockstore/local/fs/... -count=1` (full package, ~10.9s)              | PASS   |
| `go test ./pkg/blockstore/local/fs/... -run "Rollup_LRUHit|Rollup_FirstChunk_PopulatesLRU\|Rollup_AddRefReturnsErr\|Rollup_ComputeObjectID\|Rollup_LRUHit_NoBlockStateMutation\|Rollup_AddRefError" -v -count=1` | 6/6 PASS |
| `grep -c "bc.dedupLRU.Get" pkg/blockstore/local/fs/rollup.go`                              | 1      |
| `grep -c "ErrUnknownHash" pkg/blockstore/local/fs/rollup.go`                               | 2      |
| `grep -c "bc.dedupLRU.Put(h, payloadID)" pkg/blockstore/local/fs/rollup.go`                | 1      |
| `grep -c "bc.blockStore.AddRef" pkg/blockstore/local/fs/rollup.go`                         | 1      |

## Authentication gates

None — fully autonomous execution.

## Deferred Issues

None.

## Known Stubs

None.

## Next Plan Readiness

- The Phase 19 Opt 1 path (per-share hash dedup LRU → AddRef fast-path) is now end-to-end wired. The next idempotent-rewrite hash that lands in `rollupFile` will skip the local CAS Put and the implicit remote upload (StoreChunk is the only call StoreChunk makes; engine-level remote Put happens via the syncer claiming Pending rows, which AddRef does NOT create).
- D-21 throughput gate (≤1.00 idempotent-rewrite vs. P18 baseline) is unblocked for benchmark.
- No follow-up plan dependencies remain on rollup.go for Opt 1.

## Self-Check: PASSED

- `pkg/blockstore/local/fs/rollup.go` — modifications FOUND:
  - `bc.dedupLRU.Get(h)` → 1 occurrence
  - `bc.blockStore.AddRef(ctx, h, payloadID, blockRef)` → 1 occurrence
  - `errors.Is(addRefErr, metadata.ErrUnknownHash)` → 1 occurrence
  - `bc.dedupLRU.Put(h, payloadID)` → 1 occurrence
  - `blocks = append(blocks, blockRef)` (unconditional, outside if/else) → 1 occurrence
- `pkg/blockstore/local/fs/rollup_test.go` — 6 new test functions + `programmableFBS` + `newFSStoreForRollupLRUTest` + `hashOfSingleChunk` helpers FOUND.
- Commits `2f46e0ff` (RED test), `e8833675` (GREEN feat) — both in `git log --oneline`, both signed.
- Final verification suite: all gates PASS as tabulated above.

---
*Phase: 19-write-path-ram-optimizations*
*Plan: 05*
*Completed: 2026-05-21*
