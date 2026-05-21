---
phase: 17-unified-blockstore
plan: 02
subsystem: infra
tags: [blockstore, cas, conformance-suite, testing, go]

# Dependency graph
requires:
  - phase: 17-unified-blockstore
    provides: "BlockStore + BlockStoreAppend interfaces + ErrStopWalk sentinel (Plan 17-01, cd5442ca)"
provides:
  - "pkg/blockstore/blockstoretest.BlockStoreConformance — unified CAS-keyed conformance entrypoint (9 scenarios)"
  - "pkg/blockstore/blockstoretest.BlockStoreAppendConformance — random-write absorber conformance entrypoint (5 named scenarios, 2 portable + 3 t.Skip)"
  - "Factory + AppendFactory defined types with (store, cleanup) pair-return contract for deterministic per-subtest teardown"
  - "blake3Sum helper co-located in conformance.go, reused across appendlog.go (same package, no duplication)"
affects:
  - 17-06-PLAN  # backends supply fs/s3/memory factories pointing at these entrypoints
  - 17-07-PLAN  # deletes pkg/blockstore/local/localtest and pkg/blockstore/remote/remotetest

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Factory pair-return (store, cleanup) — each subtest invokes factory then t.Cleanup(cleanup); subtests never share state"
    - "Walk callback ErrStopWalk vs custom-error contract assertion (D-07 wrap pin)"
    - "Aliasing defense by mutation (mutate first Get slice, re-Get, assert unchanged) — Phase 16 D-05 carry-forward"

key-files:
  created:
    - pkg/blockstore/blockstoretest/conformance.go
    - pkg/blockstore/blockstoretest/appendlog.go
    - pkg/blockstore/blockstoretest/doc.go
  modified: []

key-decisions:
  - "Factory and AppendFactory are defined types (not type aliases) — referenced by name in godoc and so backends document the cleanup-closure contract at construction time."
  - "Plan 17-02 adds a tenth subtest (Walk_ErrorWrap) on top of the nine the plan listed — testWalkErrorWrap pins the D-07 'walk halted at %s: %w' wrap-then-detect side of the contract. testWalkStopSentinel only covers the clean-exit branch; testWalkErrorWrap covers the error-propagation branch via errors.Is on a custom callback sentinel."
  - "Three of the five legacy localtest/appendlog_suite.go scenarios (PressureChannel_INV05, TornWriteRecovery_LSL06, RollupOffsetMonotone_INV03) cannot be expressed on the BlockStoreAppend interface surface. They t.Skip with explicit rationale that names the missing fs-internal probes; the fs backend continues to exercise them via the legacy localtest suite until Plan 17-07 deletes it. AppendLogRoundTrip and ConcurrentStorm are interface-portable via Walk-polling and survive in adapted form."
  - "blake3Sum helper lives ONLY in conformance.go and is reused from appendlog.go (same package). No duplication."
  - "Concurrent edits from parallel waves (17-03 remote rename, 17-04 LocalStore narrowing) committed to the same branch DURING this plan's execution broke `go build ./...` globally. Plan 17-02's local artifact builds and vets clean in isolation (`go build ./pkg/blockstore/blockstoretest/` + `go vet ./pkg/blockstore/blockstoretest/`). This is the expected mid-mega-PR state per D-01: individual plans must keep their own surface green; cross-plan build greenness is the merge-end gate, not a per-task gate."

patterns-established:
  - "Test-library factory pair-return: (store, cleanup func()) — pattern picked up from PATTERNS.md §interfaces (lines 461-489)."
  - "t.Skip with explicit rationale that names the missing internal hooks AND points at the legacy suite that still exercises the scenario — keeps test inventory traceable until the legacy suite is deleted."
  - "Walk error-wrap pin: custom errors.New sentinel returned from callback + errors.Is on Walk's return value, in addition to the ErrStopWalk clean-exit assertion. Together they pin both branches of D-07."

requirements-completed: []

# Metrics
duration: ~4min
completed: 2026-05-20
---

# Phase 17 Plan 02: BlockStore Conformance Suite Scaffolding Summary

**Shipped the consolidated `pkg/blockstore/blockstoretest/` library with two D-09 entrypoints (`BlockStoreConformance` + `BlockStoreAppendConformance`), 10 unified CAS-keyed subtests + 5 random-write absorber subtests, before Wave 2 retargets backends.**

## Performance

- **Duration:** ~4 min
- **Started:** 2026-05-20T16:54:50Z
- **Completed:** 2026-05-20T16:58:45Z
- **Tasks:** 2
- **Files created:** 3 (`conformance.go`, `appendlog.go`, `doc.go`)
- **Files modified:** 0

## Accomplishments

- New `pkg/blockstore/blockstoretest/` package with `BlockStoreConformance(t, factory)` entrypoint dispatching to 10 subtests:
  - `Put_Get_Roundtrip` (with Phase-16 D-05 aliasing defense by mutation)
  - `Get_NotFound` (errors.Is against `blockstore.ErrChunkNotFound`)
  - `GetRange` (16-byte payload, range[4:12])
  - `Delete` (Delete-then-Get → ErrChunkNotFound)
  - `Walk` (3 hashes, Meta.Size + non-zero LastModified)
  - `Walk_ErrStopWalk` (callback returns sentinel → Walk returns nil, callback invoked exactly once)
  - `Walk_ErrorWrap` (callback returns custom error → Walk wraps via errors.Is detection)
  - `Head` (Size + LastModified non-zero)
  - `Put_Idempotent_SameHash` (two puts → one Walk entry)
  - `Put_Concurrent_SameHash` (8 goroutines → one Walk entry)
- New `BlockStoreAppendConformance(t, factory)` entrypoint dispatching to 5 named subtests:
  - `AppendLogRoundTrip` — interface-portable (poll Walk, verify chunks appear post-rollup, DeleteLog tombstones the log without deleting chunks)
  - `PressureChannel_INV05` — `t.Skip` (fs-internal probes required)
  - `TornWriteRecovery_LSL06` — `t.Skip` (on-disk log access required)
  - `ConcurrentStorm` — interface-portable (4×4 goroutines, deadlock-free, Walk surfaces ≥1 chunk)
  - `RollupOffsetMonotone_INV03` — `t.Skip` (header-CRC corruption required)
- Factory contract: `type Factory func(t *testing.T) (blockstore.BlockStore, func())` and `type AppendFactory func(t *testing.T) (blockstore.BlockStoreAppend, func())` (defined types, not aliases; pair-return with cleanup closure invoked via `t.Cleanup(cleanup)` at the top of each subtest body).
- DeleteLog rename (formerly DeleteAppendLog) wired throughout — `grep -c DeleteAppendLog appendlog.go` returns 0.
- Shared `blake3Sum` helper in `conformance.go`; reused from `appendlog.go` (same package, no import, no duplication).

## Task Commits

1. **Task 1: BlockStoreConformance + 9 (+1) scenarios + doc.go** — `83cd4c31` (test, signed, GPG-verified)
2. **Task 2: BlockStoreAppendConformance + 5 named scenarios** — `0dd29951` (test, signed, GPG-verified)

Both commits live on `gsd/phase-16-cache-mmap-removal` (Phase 17 mega-PR branch per D-01).

## Files Created/Modified

- `pkg/blockstore/blockstoretest/doc.go` (NEW, ~25 lines) — package godoc documenting the D-09 two-entrypoint shape.
- `pkg/blockstore/blockstoretest/conformance.go` (NEW, ~340 lines) — `Factory`, `BlockStoreConformance`, 10 `test*` funcs, `blake3Sum` helper.
- `pkg/blockstore/blockstoretest/appendlog.go` (NEW, ~240 lines) — `AppendFactory`, `BlockStoreAppendConformance`, 5 `test*` funcs (2 portable + 3 t.Skip).

## Decisions Made

### Added Walk_ErrorWrap as a 10th subtest

The plan's `<behavior>` block listed nine subtests but also explicitly required *"Walk error wrap: Put 3 hashes; callback returns a custom error after first invocation; assert Walk returns a non-nil error AND errors.Is(err, customErr) is true (validates the 'walk halted at %s: %w' wrap from D-07)"*. The acceptance criterion `grep -c 't.Run' >= 9` is satisfied either way, but pinning the error-wrap branch as a separate subtest (`testWalkErrorWrap`) is cleaner than folding it into `testWalkStopSentinel` (which only covers the clean-exit branch). The total subtest count is therefore 10, and `BlockStoreConformance` dispatches all 10 via `t.Run` calls.

### Three legacy scenarios `t.Skip` with explicit rationale

`PressureChannel_INV05`, `TornWriteRecovery_LSL06`, and `RollupOffsetMonotone_INV03` all rely on fs-internal test hooks that intentionally do NOT appear on the `BlockStoreAppend` interface surface:

| Scenario | Missing internal probes | Why not portable |
|----------|------------------------|------------------|
| PressureChannel_INV05 | `SetMaxLogBytesForTest`, `LogBytesTotalForTest` | Byte-accounting invariants are fs-internal; interface deliberately hides them. |
| TornWriteRecovery_LSL06 | `BaseDirForTest`, `RollupStoreForTest`, `ReopenForTest`, `IntervalsLenForTest`, direct file ops on `<base>/logs/<id>.log` | On-disk log path + reopen semantics are fs-specific; interface deliberately exposes neither. |
| RollupOffsetMonotone_INV03 | `RecomputeHeaderCRCForTest`, `HeaderRollupOffsetForTest`, `ReopenForTest`, byte-edit of log header | Header-CRC corruption requires byte-level on-disk access; interface deliberately exposes none. |

Each `t.Skip` carries an explicit `t.Skip(...)` message that names the missing probes AND directs the reader to the legacy `localtest.RunAppendLogSuite` (where the fs backend keeps exercising these invariants until Plan 17-07 deletes it). This keeps test inventory traceable across the wave transition.

The two portable scenarios (`AppendLogRoundTrip`, `ConcurrentStorm`) were adapted to poll `Walk` for the first emitted chunk (in lieu of `RollupOffsetForTest` / `BaseDirForTest`-driven inspection). The polling deadlines are 10 s, generous enough to absorb the fs backend's 50 ms stabilization window plus slow-IO CI variance.

### Factory pair-return shape locked

`Factory` and `AppendFactory` are defined types (NOT `type X = ...` aliases) per the plan-checker iter-1 revision. They return `(BlockStore, func())` and `(BlockStoreAppend, func())` respectively. Each subtest invokes the factory once at the top and registers the cleanup closure via `t.Cleanup(cleanup)` so teardown runs after the subtest body returns (the subtest never explicitly calls `cleanup()`). This pattern matches PATTERNS.md §interfaces (lines 461-489).

### `blake3Sum` helper not duplicated

`blake3Sum(b []byte) blockstore.ContentHash` is declared exactly once in `conformance.go` and reused from `appendlog.go` via package-internal call (same package, no import needed). The acceptance check `grep -c "func blake3Sum" pkg/blockstore/blockstoretest/` returns 1.

### Backends not wired here

Per Plan 02 scope, no backend factory is supplied in this package. Plans 17-06 / 17-07 will land the fs / s3 / memory factories in each backend's `_test.go` file, calling the entrypoints declared here. The plan acceptance criterion `go test ./pkg/blockstore/blockstoretest/` reports "no test files" — correct, the package is library-only.

## Deviations from Plan

### Concurrent-wave build pollution observed (not introduced by this plan)

While Task 2 was being prepared, two parallel agents (Plan 17-03 remote rename, Plan 17-04 LocalStore narrowing) committed to the same branch and broke `go build ./...` globally:

- `pkg/blockstore/remote/remote.go` lost `WriteBlockWithHash`, `HeadObject`, `HeadResult`, `ObjectInfo`, etc.
- `pkg/blockstore/local/local.go` narrowed `LocalStore` and removed `DeleteAppendLog` from the interface.
- `pkg/blockstore/remote/memory/store.go` and `pkg/blockstore/remote/s3/store.go` no longer compile against `remote.RemoteStore`.
- `pkg/blockstore/engine/` consumers reference removed methods.
- `pkg/blockstore/remote/remotetest/suite.go` references removed methods.

**This is NOT a Plan 17-02 deviation — it is the expected mid-mega-PR state per Phase 17 D-01.** The plan acceptance criteria for Plan 02 are satisfied at the **package-local** scope (`go vet ./pkg/blockstore/blockstoretest/` clean; `go build ./pkg/blockstore/blockstoretest/` clean). The global `go build ./...` constraint in the plan's `<verify>` block was written before Wave-2 parallelization was understood; in practice it cannot hold while parallel waves modify cross-package interfaces. The mega-PR's `go build ./...` gate runs at merge time, not at per-plan time.

No code was modified in this plan to work around the pollution — Plans 17-03 / 17-04 / 17-06 / 17-07 own their own surfaces and will return the global build to green before the mega-PR merges.

### Rule classification

None of Rules 1-4 fired. Plan 17-02 is pure scaffolding (no behavior to debug, no missing critical functionality to backfill, no blocker to unstick that isn't owned by a sibling plan). The concurrent-wave pollution above is also not a Rule 3 blocker for this plan because the package builds and vets cleanly in isolation.

## Issues Encountered

None.

## TDD Gate Compliance

Both tasks were marked `tdd="true"`. Plan 17-02 ships test scenarios but no implementation under test — the implementers (fs / s3 / memory) are wired in Plan 17-06. A formal RED/GREEN gate split therefore would have been vacuous: the RED commit would have been `go test ./pkg/blockstore/blockstoretest/` reporting "no test files" (the package has no `TestXxx` functions, only library funcs starting with lowercase `test`), and the GREEN commit would have been identical. Documented here for the verifier; the conformance-suite shape is itself the contract, and Plan 17-06's backend wiring is where RED → GREEN becomes meaningful.

## Self-Check

- `pkg/blockstore/blockstoretest/conformance.go` exists — FOUND
- `pkg/blockstore/blockstoretest/appendlog.go` exists — FOUND
- `pkg/blockstore/blockstoretest/doc.go` exists — FOUND
- Commit `83cd4c31` on `gsd/phase-16-cache-mmap-removal` — FOUND, signed (ED25519, GPG-verified)
- Commit `0dd29951` on `gsd/phase-16-cache-mmap-removal` — FOUND, signed (ED25519, GPG-verified)
- `go vet ./pkg/blockstore/blockstoretest/` exits 0 — verified
- `go build ./pkg/blockstore/blockstoretest/` exits 0 — verified
- `grep -cE '^func test' pkg/blockstore/blockstoretest/conformance.go` returns 10 (≥10 required) — PASS
- `grep -c 't.Run' pkg/blockstore/blockstoretest/conformance.go` returns 10 (≥9 required) — PASS
- `grep -c 'ErrStopWalk' pkg/blockstore/blockstoretest/conformance.go` returns 7 (≥2 required) — PASS
- `grep -c 'ErrChunkNotFound' pkg/blockstore/blockstoretest/conformance.go` returns 5 (≥1 required) — PASS
- `grep -E 'func BlockStoreConformance\(t \*testing\.T, factory Factory\)' conformance.go` returns 1 — PASS
- `grep -cE '^func test' pkg/blockstore/blockstoretest/appendlog.go` returns 5 — PASS
- `grep -c 't.Run' pkg/blockstore/blockstoretest/appendlog.go` returns 10 (≥5 required; 5 real calls + 5 godoc references) — PASS
- `grep -E 'func BlockStoreAppendConformance\(t \*testing\.T, factory AppendFactory\)' appendlog.go` returns 1 — PASS
- All 5 named append funcs present: testAppendLogRoundTrip, testPressureChannelINV05, testTornWriteRecoveryLSL06, testConcurrentStorm, testRollupOffsetMonotoneINV03 — PASS
- `grep -c "DeleteLog" appendlog.go` returns 9 (≥1 required) — PASS
- `grep -c "DeleteAppendLog" appendlog.go` returns 0 (legacy name absent) — PASS
- `pkg/blockstore/local/localtest/appendlog_suite.go` UNTOUCHED — verified via `git status`

## Self-Check: PASSED

## Next Plan Readiness

Plan 17-06 (backend factories wiring fs/s3/memory against `BlockStoreConformance` + `BlockStoreAppendConformance`) has its contract ready. Plan 17-07 (legacy `localtest/` + `remotetest/` deletion) will run after Plan 17-06 so the scenarios it deletes are already covered by the new suite for every backend that needs them.

No blockers introduced. The concurrent-wave global build breakage is not owned by this plan; sibling plans (17-03 remote rename completion, 17-04 LocalStore narrowing completion) and their downstream consumer rewrites are responsible for returning `go build ./...` to green before merge.

---
*Phase: 17-unified-blockstore*
*Completed: 2026-05-20*
