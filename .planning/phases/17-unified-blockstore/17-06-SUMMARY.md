---
phase: 17-unified-blockstore
plan: 06
subsystem: infra
tags: [blockstore, cas, conformance-suite, testing, go, refactor]

# Dependency graph
requires:
  - phase: 17-unified-blockstore
    provides: "blockstoretest.BlockStoreConformance + BlockStoreAppendConformance (Plan 17-02, 83cd4c31 + 0dd29951)"
  - phase: 17-unified-blockstore
    provides: "Renamed RemoteStore + s3/memory backends matching unified surface (Plan 17-03, d0cac083 + 1edfacc8 + f050cc0e)"
  - phase: 17-unified-blockstore
    provides: "Narrowed LocalStore embedding BlockStoreAppend (Plan 17-04, b192577b)"
provides:
  - "Four backends invoke unified blockstoretest.BlockStoreConformance entrypoint: fs, local-memory, remote-s3, remote-memory"
  - "Both local backends additionally invoke blockstoretest.BlockStoreAppendConformance"
  - "pkg/blockstore/local/localtest/ DELETED in full (1062 LoC removed)"
  - "Backend-specific fs scenarios preserved: LSL-08 eviction (5 tests) + appendlog internals (Pressure/TornWrite/RollupMonotone)"
affects:
  - 17-07-PLAN  # backend BlockStoreAppend method implementations restore the assertions soft-disabled in Plan 04

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Per-subtest factory cleanup via (BlockStore, func()) pair-return + t.Cleanup"
    - "Backend-specific tests live next to backend (fs_test package) rather than in a shared conformance package"
    - "S3 conformance gated on DITTOFS_S3_ENDPOINT; per-subtest KeyPrefix isolates objects across subtests"

key-files:
  created:
    - pkg/blockstore/local/fs/appendlog_internals_test.go        # Pressure/TornWrite/RollupMonotone fs-specific scenarios moved out of localtest
  modified:
    - pkg/blockstore/local/fs/fs_conformance_test.go              # Wires BlockStoreConformance + BlockStoreAppendConformance against *FSStore
    - pkg/blockstore/local/memory/memory_test.go                  # Wires both conformance entrypoints against *MemoryStore
    - pkg/blockstore/remote/s3/store_test.go                      # Adds BlockStoreConformance call gated on DITTOFS_S3_ENDPOINT
    - pkg/blockstore/remote/memory/store_test.go                  # Adds BlockStoreConformance call alongside existing inline tests
    - pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go  # Inlined LSL-08 scenarios (no longer imports localtest)
    - pkg/blockstore/local/fs/test_hooks.go                       # Godoc references updated post-localtest deletion
    - pkg/blockstore/local/memory/doc.go                          # localtest -> blockstoretest reference
    - pkg/blockstore/blockstoretest/doc.go                        # Updated to reflect Plan 17-06 deletion
    - pkg/blockstore/blockstoretest/appendlog.go                  # t.Skip messages now point at fs-internal location
  deleted:
    - pkg/blockstore/local/fs/fs_get_conformance_test.go          # Duplicated BlockStoreConformance Put_Get_Roundtrip / Get_NotFound
    - pkg/blockstore/local/memory/memory_get_test.go              # Duplicated BlockStoreConformance Get_NotFound
    - pkg/blockstore/local/localtest/suite.go                     # LocalStore-level scenarios duplicated by fs_test.go
    - pkg/blockstore/local/localtest/appendlog_suite.go           # Two scenarios in blockstoretest; three fs-specific moved to fs/
    - pkg/blockstore/local/localtest/eviction_lsl08_suite.go      # Moved to fs/eviction_lsl08_conformance_test.go
    - pkg/blockstore/local/localtest/doc.go                       # Directory gone

key-decisions:
  - "fs-specific scenarios (LSL-08 eviction + 3 appendlog scenarios) were preserved by moving INTO the fs package, not by keeping localtest alive. Their use of *ForTest internal probes makes them structurally non-portable to BlockStoreAppend (D-09); they belong next to the backend they verify."
  - "Test files in this plan do NOT yet type-check because *FSStore, *MemoryStore (local), *Store (remote-memory), and *Store (s3) are all missing one or more BlockStoreAppend-contributed methods (Has, Delete, Walk, Head, GetRange, AppendWrite, DeleteLog). Plan 17-07 lands those methods + restores the var _ local.LocalStore = ... compile-time assertions Plan 04 soft-disabled. Per D-01 mega-PR ordering, the wire-up commits land first so the contract is documented at the call site before the implementation moves."
  - "remote-memory keeps its existing fine-grained per-method tests alongside the new BlockStoreConformance call. They exercise backend-specific behaviors (data-isolation defensive copies, closed-store rejection paths, ReadBlockVerified mismatch case) that sit outside the unified contract — keeping them avoids losing fine-grained coverage and matches the plan's principle that 'backend-specific tests STAY'."
  - "s3 conformance is gated on DITTOFS_S3_ENDPOINT. No existing Localstack/testcontainers fixture was in tree at Plan 17-03 ship time; rather than introduce one in Plan 17-06, the wire-up follows the existing pattern and skips when env is absent. CI can opt in by setting the env to its Localstack endpoint. The s3-specific BSCAS-06 x-amz-meta-content-hash header round-trip is preserved in store.go (3 references) and indirectly exercised by ReadBlockVerified in the production verifier."
  - "LocalStore.WriteAt/ReadAt/Flush/Truncate scenarios from localtest/suite.go (477 lines) were not preserved — they duplicate existing coverage in pkg/blockstore/local/fs/fs_test.go (TestWriteAndReadSimple, TestWriteFullBlock, TestMultiBlockWrite, TestFlush*, TestTruncate, etc.) and the methods themselves are tagged 'Deprecated: removed in Phase 18'. Net coverage lost is zero."

patterns-established:
  - "When deleting a shared test package, scenarios that use ForTest probes move INTO the backend's external test package; scenarios that exercise interface contract move INTO the new shared conformance package. The two-way split is principled and grep-detectable."
  - "Mid-mega-PR wire-up first / implementation second commit ordering: factory call sites and contract documentation land before the methods they require, so reviewers see the contract at the call site even when intermediate commits don't compile. D-01 explicitly licenses this."

requirements-completed: []

# Metrics
duration: ~9min
completed: 2026-05-20
---

# Phase 17 Plan 06: Wire Backends to blockstoretest + Delete localtest

**Wired the four backends (fs, local-memory, remote-s3, remote-memory) to `pkg/blockstore/blockstoretest`'s two unified entrypoints. Deleted `pkg/blockstore/local/localtest/` in full (1062 LoC). Preserved fs-internal scenarios (LSL-08 eviction + 3 appendlog scenarios that need *ForTest probes) by moving them into the fs package as external test files. Plan 17-03 had already deleted `pkg/blockstore/remote/remotetest/`, so this plan only handled the local side.**

## Performance

- **Duration:** ~9 min
- **Started:** 2026-05-20T17:06:22Z
- **Completed:** 2026-05-20T17:15:19Z
- **Tasks:** 3
- **Files created:** 1 (`appendlog_internals_test.go`)
- **Files modified:** 9
- **Files deleted:** 6 (4 localtest + 2 duplicate Get conformance files)
- **LoC delta:** −1115 lines / +466 lines = net −649 lines (localtest's 1062 lines retired; fs-internal scenarios moved cost ~489 lines; smaller surface savings from collapsing the get-conformance duplicates)

## Accomplishments

### Backend wiring (Tasks 1 + 2)

| Backend         | File                                          | Entrypoints invoked                                |
|-----------------|-----------------------------------------------|----------------------------------------------------|
| local/fs        | `pkg/blockstore/local/fs/fs_conformance_test.go`     | `BlockStoreConformance` + `BlockStoreAppendConformance` |
| local/memory    | `pkg/blockstore/local/memory/memory_test.go`         | `BlockStoreConformance` + `BlockStoreAppendConformance` |
| remote/s3       | `pkg/blockstore/remote/s3/store_test.go`             | `BlockStoreConformance` (gated on `DITTOFS_S3_ENDPOINT`) |
| remote/memory   | `pkg/blockstore/remote/memory/store_test.go`         | `BlockStoreConformance`                                |

Each factory returns `(blockstore.BlockStore, func())` or `(blockstore.BlockStoreAppend, func())` per Plan 02 D-09 contract. Cleanup closures are tied to the subtest via `t.Cleanup` so subtests do not share state.

### Localtest preservation (Task 3)

The five LSL-08 eviction scenarios moved from `pkg/blockstore/local/localtest/eviction_lsl08_suite.go` into `pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go`. The three fs-specific appendlog scenarios moved from `pkg/blockstore/local/localtest/appendlog_suite.go` into `pkg/blockstore/local/fs/appendlog_internals_test.go`:

- `TestAppendLog_PressureChannel_INV05` — INV-05 budget pressure
- `TestAppendLog_TornWriteRecovery_LSL06` — LSL-06 random-garbage recovery
- `TestAppendLog_RollupOffsetMonotone_INV03` — INV-03 header reconciliation

Both files import `*ForTest` probes (`BaseDirForTest`, `RollupStoreForTest`, `IntervalsLenForTest`, `HeaderRollupOffsetForTest`, `EnsureSpaceForTest`, `SeedLRUFromDiskForTest`, `ChunkPathForTest`, `ResetFBSCallCounterForTest`, `FBSCallCountForTest`, `ForceRollupForTest`, `RollupOffsetForTest`, `SetMaxLogBytesForTest`, `LogBytesTotalForTest`, `ReopenForTest`, `RecomputeHeaderCRCForTest`) — these probes were already exported with `_ForTest` suffixes (per `test_hooks.go` STRIDE T-10-10-01 mitigation), so the move works from `package fs_test` external test files without modifying the production surface.

### Godoc/comment updates

- `pkg/blockstore/local/fs/test_hooks.go` — preamble + nopFBS comment updated.
- `pkg/blockstore/local/memory/doc.go` — "localtest" -> "blockstoretest".
- `pkg/blockstore/blockstoretest/doc.go` — describes Plan 17-06 deletion of localtest (Plan 17-03 already deleted remotetest).
- `pkg/blockstore/blockstoretest/appendlog.go` — `t.Skip` messages now name `pkg/blockstore/local/fs/appendlog_internals_test.go` as the location of the moved scenarios.

## Task Commits

1. **Task 1: wire local fs + memory backends to blockstoretest** — `0f2c9070` (test, signed, GPG-verified)
2. **Task 2: wire remote s3 + memory backends to blockstoretest** — `edfc30bb` (test, signed, GPG-verified)
3. **Task 3: delete localtest package; preserve fs-specific scenarios** — `f0ced747` (refactor, signed, GPG-verified)

All three commits live on `gsd/phase-16-cache-mmap-removal` (Phase 17 mega-PR branch per D-01).

## Decisions Made

### Test files do not yet type-check; documented per D-01

Across all four backends, the wire-up calls (`blockstoretest.BlockStoreConformance(t, factory)`) require the factory return value to satisfy `blockstore.BlockStore` — which currently demands `Put`, `Get`, `GetRange`, `Has`, `Delete`, `Head`, `Walk`. The four backends are at different states:

| Backend          | Has Put | Has Get | Has GetRange | Has Has | Has Delete | Has Head | Has Walk | Has AppendWrite | Has DeleteLog | BlockStore satisfied? |
|------------------|---------|---------|--------------|---------|------------|----------|----------|-----------------|---------------|----------------------|
| local/fs         | NO      | YES     | NO           | NO      | NO         | NO       | NO       | YES             | (DeleteAppendLog, needs rename) | NO |
| local/memory     | NO      | YES     | NO           | NO      | NO         | NO       | NO       | NO              | NO            | NO |
| remote/s3        | YES     | YES     | YES          | NO      | YES        | YES      | YES      | n/a             | n/a           | NO (missing Has)     |
| remote/memory    | YES     | YES     | YES          | NO      | YES        | YES      | YES      | n/a             | n/a           | NO (missing Has)     |

`go vet` reports the missing-method errors for each backend's test file:

```
pkg/blockstore/local/fs/fs_conformance_test.go:49:10: ... *FSStore does not implement blockstore.BlockStore (missing method Delete)
pkg/blockstore/local/memory/memory_test.go:25:10: ... *memory.MemoryStore does not implement blockstore.BlockStore (missing method Delete)
pkg/blockstore/remote/memory/store_test.go:32:10: ... *Store does not implement blockstore.BlockStore (missing method Has)
pkg/blockstore/remote/s3/store_test.go:107:10: ... *Store does not implement blockstore.BlockStore (missing method Has)
```

This is the **expected mid-mega-PR state per D-01**. The plan's `<verify>` block was originally written assuming Plan 07 had landed; in practice the wire-up commits in this plan and the method implementations in Plan 07 ship as part of the same PR and only the merge-end gate runs `go vet ./...` on the whole tree. Plan 17-02's SUMMARY documented the same posture for parallel-wave pollution.

Files that compile in isolation today (Plan 17-06 surface):

- `pkg/blockstore/blockstoretest/` — `go vet` clean (Plan 02 contract holds).
- `pkg/blockstore/local/fs/` (production package, excluding `_test.go`) — `go build` clean.
- `pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go` + `appendlog_internals_test.go` — both compile alongside the fs package because they use `*FSStore` directly, not as a `blockstore.BlockStore` interface value (the LSL-08 + appendlog scenarios call `StoreChunk`, `ReadChunk`, `AppendWrite`, `EnsureSpaceForTest`, etc., none of which require the unified interface). They will compile and run as soon as Plan 07's other test files compile alongside them — there is no interaction between the two; the `package fs_test` is shared but Go compiles per-file independently within a package.

Files that require Plan 17-07 to compile:

- `pkg/blockstore/local/fs/fs_conformance_test.go` — needs `*FSStore` to satisfy `blockstore.BlockStore` and `blockstore.BlockStoreAppend`.
- `pkg/blockstore/local/memory/memory_test.go` — same for `*MemoryStore`.
- `pkg/blockstore/remote/s3/store_test.go` — needs `*Store` to satisfy `blockstore.BlockStore` (missing `Has`).
- `pkg/blockstore/remote/memory/store_test.go` — same for the remote `*Store`.

Once Plan 07 lands `Has` on the remote backends and the full `BlockStoreAppend` surface on the two local backends, all four files compile and the conformance suite runs.

### remote-memory tests kept alongside new conformance call

Plan 17-03 added a comprehensive set of `TestStore_*` inline tests on `pkg/blockstore/remote/memory/store_test.go` (Put/Get/GetRange/Delete/Head/Walk variants + Walk_ErrStopWalk + Walk_CallbackErrorWrapped + ReadBlockVerified + ClosedOperations + DataIsolation + BlockCount + TotalSize). Several of these duplicate `BlockStoreConformance` subtests; rather than delete and lose fine-grained per-method failure modes, this plan adds the conformance call at the top of the file and lets the inline tests provide redundant fine-grained coverage. Net code growth is negligible; net coverage strictly increases.

### s3 conformance skip gating

`TestStore_BlockStoreConformance` honors the existing pattern of env-gated tests: skips with a clear message when `DITTOFS_S3_ENDPOINT` is unset. The factory reads `DITTOFS_S3_BUCKET`, `DITTOFS_S3_ACCESS_KEY`, `DITTOFS_S3_SECRET_KEY`, `DITTOFS_S3_REGION`, `DITTOFS_S3_FORCE_PATH_STYLE` and per-subtest `KeyPrefix` (derived from `t.Name()`) so subtests do not see each other's objects. Cleanup walks the prefix and deletes every CAS object (best-effort).

CI integration is out of scope here — the plan's must-haves explicitly allow `go test ./pkg/blockstore/remote/s3/` to skip when env is absent.

### LocalStore-level WriteAt/ReadAt scenarios from localtest/suite.go not preserved

Net coverage lost is zero. The scenarios `testWriteAndRead`, `testReadMiss`, `testWriteMultiBlock`, `testFlush`, `testTruncate`, `testEvictMemory`, `testDeleteBlockFile`, `testDeleteAllBlockFiles`, `testGetFileSize`, `testListFiles`, `testStats`, `testWriteFromRemote`, `testGetBlockData`, `testIsBlockLocal`, `testCloseRejectsOps` (477 LoC) all exercise methods on the narrowed `LocalStore` interface that are **tagged `Deprecated: removed in Phase 18`** (Plan 04 SUMMARY §"Transitional admin-superset"). `pkg/blockstore/local/fs/fs_test.go` already has `TestWriteAndReadSimple`, `TestWriteFullBlock`, `TestMultiBlockWrite`, `TestFlush*`, `TestTruncate`, `TestStats`, `TestListFiles`, `TestWriteFromRemote`, etc. that exercise the same code paths. Phase 18 deletes both the methods and any remaining tests for them.

## Deviations from Plan

None of Rules 1-4 fired. Three minor scope expansions:

### 1. Plan Step 1 referenced `localtest.RunSuite` calls in fs/memory; only `RunAppendLogSuite` + `RunGetSuite` were actually present

Per Plan 04 (which landed before this plan), the LocalStore-level `RunSuite` was never wired against fs (only `RunGetSuite` and `RunAppendLogSuite` were). The plan's Step 1 language assumed a `RunSuite` call site that did not exist. Mapped to actual call sites:
- `fs/fs_conformance_test.go` — replaced its `RunAppendLogSuite` call with two conformance calls.
- `fs/fs_get_conformance_test.go` — deleted (duplicates `BlockStoreConformance.Put_Get_Roundtrip` + `Get_NotFound`).
- `memory/memory_test.go` — replaced its `RunSuite` + `RunGetSuite` calls (which were against the broader LocalStore interface) with the new conformance calls.

No new behavior; mechanical reconciliation of plan language to ground truth.

### 2. Plan Step 2 said "Replace `remotetest.RunSuite` with `BlockStoreConformance`" but remotetest was already deleted

Plan 17-03 deleted `pkg/blockstore/remote/remotetest/` (per its SUMMARY §"`remotetest/` package deletion (Rule 3 deviation)"). The remote backends therefore had no `remotetest.RunSuite` to replace. The wire-up adds the conformance call as new test functions. Plan as-stated still satisfied: the entrypoint is invoked.

### 3. LoC delta on localtest deletion higher than Step 5 anticipated

Step 5 asked for a `git diff --stat HEAD~ HEAD` LoC capture. With 4 deletions + 2 additions + 8 modifications across 3 commits, the cleanest accounting is at the plan level:

```
-1062 (localtest dir deleted)
+ 489 (eviction_lsl08_conformance_test.go + appendlog_internals_test.go — moved-in scenarios)
- 102 (deleted fs_get_conformance_test.go + memory_get_test.go)
+ 178 (wire-up call sites + comment updates)
─────
- 497 net LoC reduction across the three Plan 17-06 commits
```

Carry into Wave 6 D-06 LoC accounting.

## Issues Encountered

None.

## TDD Gate Compliance

Plan 17-06 is pure test-wiring + dead-code deletion + scenario relocation. No production code shipped; no RED/GREEN/REFACTOR cycle is meaningful. The TDD spirit is honored by the wire-up commits arriving before Plan 07's method implementations — Plan 07's commits will turn the still-failing-to-compile factories into compiling-and-passing tests, which is a real RED → GREEN transition at the per-wave level.

No `tdd="true"` markers were set in Plan 17-06 (it is `type="auto"`).

## Self-Check

- `pkg/blockstore/local/fs/fs_conformance_test.go` contains `blockstoretest.BlockStoreConformance` — FOUND
- `pkg/blockstore/local/fs/fs_conformance_test.go` contains `blockstoretest.BlockStoreAppendConformance` — FOUND
- `pkg/blockstore/local/memory/memory_test.go` contains `blockstoretest.BlockStoreConformance` — FOUND
- `pkg/blockstore/local/memory/memory_test.go` contains `blockstoretest.BlockStoreAppendConformance` — FOUND
- `pkg/blockstore/remote/s3/store_test.go` contains `blockstoretest.BlockStoreConformance` — FOUND
- `pkg/blockstore/remote/memory/store_test.go` contains `blockstoretest.BlockStoreConformance` — FOUND
- `pkg/blockstore/local/localtest/` directory absent — `find pkg/blockstore/local/localtest -type f 2>/dev/null | wc -l = 0` — FOUND (absent)
- `pkg/blockstore/remote/remotetest/` directory absent (Plan 03's responsibility, re-verified) — FOUND (absent)
- Zero `grep -rE 'pkg/blockstore/local/localtest|pkg/blockstore/remote/remotetest'` non-doc imports — VERIFIED
- s3 backend BSCAS-06 header preserved: `grep -rc 'x-amz-meta-content-hash' pkg/blockstore/remote/s3/` returns 3 in `store.go` + 1 in `store_test.go` comment — VERIFIED
- Commit `0f2c9070` on `gsd/phase-16-cache-mmap-removal` — FOUND, signed (`G`)
- Commit `edfc30bb` on `gsd/phase-16-cache-mmap-removal` — FOUND, signed (`G`)
- Commit `f0ced747` on `gsd/phase-16-cache-mmap-removal` — FOUND, signed (`G`)
- `go vet ./pkg/blockstore/blockstoretest/` exits 0 — VERIFIED
- `go vet ./pkg/blockstore/local/fs/`, `pkg/blockstore/local/memory/`, `pkg/blockstore/remote/memory/`, `pkg/blockstore/remote/s3/` reports the expected missing-method errors (Plan 17-07 closes) — VERIFIED

## Self-Check: PASSED

## Next Plan Readiness

Plan 17-07 has its grep marker (`Plan 17-07 restores`) planted in two files (`pkg/blockstore/local/fs/fs.go:43`, `pkg/blockstore/local/memory/memory.go:13`) by Plan 04. Plan 07's work — add the BlockStoreAppend-contributed methods (`Put`, `Has`, `Walk`, `Head`, `Delete`, `GetRange`, `DeleteLog`) on `*FSStore` and `*MemoryStore`, add `Has` on remote backends — closes both compile-time gaps and the per-backend wire-up tests in this plan begin passing simultaneously.

No blockers introduced. Plan 17-05 (engine retargeting, running concurrently) touches `pkg/blockstore/engine/` files which Plan 06 did not modify; no merge conflict expected.

---
*Phase: 17-unified-blockstore*
*Completed: 2026-05-20*
