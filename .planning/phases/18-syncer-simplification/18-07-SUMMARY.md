---
phase: 18-syncer-simplification
plan: 07
subsystem: blockstore-engine
tags: [refcount-cascade, file-level-dedup, engine-flush, engine-delete, synced-hash-store]
requires:
  - "engine.BlockStore.syncedHashStore field + Syncer.bs back-reference (from 18-06)"
  - "Syncer.snapshotPendingBlockRefs + private trySpeculativeFileLevelDedup (pre-existing)"
  - "MetadataCoordinator.GetFileObjectID + FindByObjectID + DecrementRefCount (pre-existing)"
  - "metadata.SyncedHashStore.DeleteSynced (from 18-01)"
provides:
  - "engine.BlockStore.Flush owns the file-level dedup pre-hook (calls private trySpec same-package); syncer.Flush only handles the mirror loop"
  - "engine.BlockStore.Delete cascades syncedHashStore.DeleteSynced when DecrementRefCount returns newCount == 0; orphan-marker failure is benign-logged"
  - "Public Syncer.TrySpeculativeFileLevelDedup wrapper deleted (private same-package access from engine.Flush replaces it)"
  - "engine_delete_test.go covers happy / no-cascade / nil-safe / benign-failure paths"
affects:
  - "pkg/blockstore/engine/engine.go (Flush pre-hook + Delete cascade)"
  - "pkg/blockstore/engine/syncer.go (public wrapper deletion)"
  - "pkg/blockstore/engine/engine_delete_test.go (new test file)"
tech-stack:
  added: []
  patterns:
    - "Same-package private call from engine.go → Syncer methods (bs.syncer.trySpeculativeFileLevelDedup, bs.syncer.snapshotPendingBlockRefs) — drops the trivial public wrapper in favor of direct access"
    - "Refcount-cascade inside engine.Delete: bind newCount from DecrementRefCount and conditionally fire DeleteSynced in the same loop iteration (T-18-07-01 mitigation: cascade fires after newCount becomes coordinator-visible)"
    - "Benign-orphan logging: DeleteSynced failure logged at Warn, never blocks Delete (orphan-marker contract — re-Put would skip upload but bytes are remote-resident)"
key-files:
  created:
    - pkg/blockstore/engine/engine_delete_test.go
  modified:
    - pkg/blockstore/engine/engine.go
    - pkg/blockstore/engine/syncer.go
decisions:
  - "Honored D-09 (refcount cascade): DeleteSynced fires inside the same loop iteration as DecrementRefCount when newCount == 0; the synced set stays a strict subset of local CAS contents"
  - "Honored D-12 (TrySpec relocation): public Syncer.TrySpeculativeFileLevelDedup wrapper deleted; engine.Flush calls private trySpec directly (no adapter-layer change)"
  - "Honored D-17 (private trySpec preserved in dedup.go): only the public exported seam in syncer.go was deleted; dedup.go's filename + private function are untouched"
  - "Inlined refcount fake in engine_delete_test.go rather than extending the shared fakeCoordinator: the shared fake hardcodes DecrementRefCount returning newCount == 0, which would conflate the cascade-fires and cascade-does-not-fire branches; a dedicated refcountCoordinator with a seedable per-hash counts map exercises both branches deterministically"
  - "Added a 4th subtest (DeleteSyncedFailure_IsBenign) beyond the plan's listed 3 to cover the threat-register T-18-07-03 disposition (Repudiation — logged at Warn, never blocks Delete)"
metrics:
  duration: "~25 minutes"
  tasks_completed: 3
  files_touched: 3
  commits: 3
  completed: 2026-05-21
  engine_flush_loc_delta: "+31 lines (file-level dedup pre-hook + delegation comment)"
  engine_delete_loc_delta: "+18 lines (newCount binding + cascade + benign-Warn)"
  syncer_loc_delta: "-35 lines (public TrySpeculativeFileLevelDedup wrapper + its godoc)"
  test_loc: "292 lines (engine_delete_test.go — 4 subtests with two purpose-built fakes)"
---

# Phase 18 Plan 07: TrySpec relocation + refcount cascade Summary

## One-Liner

Relocated the BSCAS-05 file-level dedup pre-hook from Syncer's public seam into `engine.BlockStore.Flush` (called via same-package access to `bs.syncer.trySpeculativeFileLevelDedup`), added a `syncedHashStore.DeleteSynced` cascade inside `engine.BlockStore.Delete` that fires when `coordinator.DecrementRefCount` returns `newCount == 0`, and deleted the now-dead public `Syncer.TrySpeculativeFileLevelDedup` wrapper — leaving the private function in `dedup.go` untouched.

## What Landed

### File-level dedup pre-hook in engine.Flush

`pkg/blockstore/engine/engine.go`

`BlockStore.Flush` was a trivial proxy:

```go
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
    return bs.syncer.Flush(ctx, payloadID)
}
```

It now owns the file-level dedup pre-hook and delegates to `syncer.Flush` only on miss / nil-coordinator:

```go
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
    if bs.coordinator != nil {
        specBlocks, blockStates, err := bs.syncer.snapshotPendingBlockRefs(ctx, payloadID)
        if err != nil { return nil, fmt.Errorf("snapshot pending blockrefs: %w", err) }
        if len(specBlocks) > 0 {
            fileObjectID, err := bs.coordinator.GetFileObjectID(ctx, payloadID)
            if err != nil { return nil, fmt.Errorf("get file objectID: %w", err) }
            hit, err := bs.syncer.trySpeculativeFileLevelDedup(ctx, payloadID, specBlocks, fileObjectID, blockStates)
            if err != nil { return nil, fmt.Errorf("file-level dedup: %w", err) }
            if hit {
                return &blockstore.FlushResult{Finalized: true}, nil
            }
        }
    }
    return bs.syncer.Flush(ctx, payloadID)
}
```

`snapshotPendingBlockRefs` and `trySpeculativeFileLevelDedup` are both lowercase (private) methods on `*Syncer`. Same-package access from `engine.go` reaches them directly — no new export needed.

### Refcount cascade in engine.Delete

`pkg/blockstore/engine/engine.go`

The pre-existing `DecrementRefCount` loop discarded the returned newCount:

```go
if _, err := bs.coordinator.DecrementRefCount(ctx, b.Hash); err != nil { ... }
```

Rewritten to bind `newCount` and conditionally cascade:

```go
newCount, err := bs.coordinator.DecrementRefCount(ctx, b.Hash)
if err != nil {
    if coordErr == nil {
        coordErr = fmt.Errorf("decrement refcount on delete %s: %w", b.Hash.String(), err)
    }
    continue
}
if newCount == 0 && bs.syncedHashStore != nil {
    if derr := bs.syncedHashStore.DeleteSynced(ctx, b.Hash); derr != nil {
        logger.Warn("delete synced marker (orphan; benign)",
            "hash", b.Hash.String(), "err", derr)
    }
}
```

The benign-orphan logging matches the plan threat-register T-18-07-03 disposition: a stale synced marker only causes one skipped remote upload on a future re-Put of the same hash, and the bytes are already remote-resident from the original `MarkSynced`. Blocking `Delete` on a `DeleteSynced` failure would be strictly worse — the file's local data is already gone, so propagating the error to the caller would leave them holding an unrecoverable partial-state error.

### Public TrySpec wrapper deletion

`pkg/blockstore/engine/syncer.go`

Lines 157–190 (35 lines incl. the multi-paragraph godoc) of the public `(m *Syncer) TrySpeculativeFileLevelDedup` wrapper deleted. The private `(m *Syncer) trySpeculativeFileLevelDedup` in `pkg/blockstore/engine/dedup.go` is untouched — same-package access from `engine.Flush` replaces the public seam.

### Test coverage

`pkg/blockstore/engine/engine_delete_test.go` (new, 292 lines)

Two purpose-built fakes:

- `refcountCoordinator` — `MetadataCoordinator` with a seedable per-hash counts map so `DecrementRefCount` returns realistic post-decrement values. The shared `fakeCoordinator` in `coordinator_test.go` hardcodes `newCount == 0` for every call, which would silently conflate the cascade-fires and cascade-does-not-fire branches. The new fake also supports a single-shot induced-error injection for the no-op-on-error path.
- `recordingSyncedHashStore` — `metadata.SyncedHashStore` over an in-memory map, recording every `DeleteSynced` invocation in order, with optional single-shot `deleteErr` injection.

Four subtests covering every branch of the cascade:

| Subtest                              | Asserts                                                              |
| ------------------------------------ | -------------------------------------------------------------------- |
| `RefcountZero_CascadesDeleteSynced`  | seed refcount=1, Delete → `IsSynced(hash) == false`, exactly 1 call  |
| `RefcountNonZero_DoesNotCascade`     | seed refcount=2, Delete → `IsSynced(hash) == true`, zero calls       |
| `NilSyncedStore_NoOps`               | nil SyncedHashStore + Delete → no panic, coordinator still fires     |
| `DeleteSyncedFailure_IsBenign`       | seeded `deleteErr` → Delete returns nil, cascade was attempted       |

The plan listed 3 subtests; the 4th (`DeleteSyncedFailure_IsBenign`) was added to cover the threat-register T-18-07-03 (Repudiation) mitigation — the benign-orphan logging contract is part of the plan's must_haves, so a regression test that asserts Delete swallows the cascade error keeps the invariant testable.

## Verification Evidence

```
go build ./...                              OK
go vet ./pkg/blockstore/local/...           OK
go vet ./pkg/blockstore/engine              red (intentional Plan 06 carry-forward;
                                            api_blockref_test.go / syncer_*_test.go /
                                            upload_test.go / perf_bench_test.go /
                                            perf_bench_phase12_test.go reference
                                            deleted private helpers — swept in Plan 08)
```

Test execution evidence (run in isolation by temporarily moving the Plan-06 intentionally-red test files aside, then restoring them):

```
go test -race -count=1 ./pkg/blockstore/engine/ -run TestEngine_Delete_CascadesDeleteSynced -v

=== RUN   TestEngine_Delete_CascadesDeleteSynced
=== RUN   TestEngine_Delete_CascadesDeleteSynced/RefcountZero_CascadesDeleteSynced
=== RUN   TestEngine_Delete_CascadesDeleteSynced/RefcountNonZero_DoesNotCascade
=== RUN   TestEngine_Delete_CascadesDeleteSynced/NilSyncedStore_NoOps
=== RUN   TestEngine_Delete_CascadesDeleteSynced/DeleteSyncedFailure_IsBenign
--- PASS: TestEngine_Delete_CascadesDeleteSynced (0.00s)
    --- PASS: TestEngine_Delete_CascadesDeleteSynced/RefcountZero_CascadesDeleteSynced (0.00s)
    --- PASS: TestEngine_Delete_CascadesDeleteSynced/RefcountNonZero_DoesNotCascade (0.00s)
    --- PASS: TestEngine_Delete_CascadesDeleteSynced/NilSyncedStore_NoOps (0.00s)
    --- PASS: TestEngine_Delete_CascadesDeleteSynced/DeleteSyncedFailure_IsBenign (0.00s)
PASS
```

`TestDedup` (the existing dedup conformance suite) also passes in isolation, confirming the relocation does not regress the private `trySpeculativeFileLevelDedup` semantics:

```
go test -race -count=1 ./pkg/blockstore/engine/ -run TestDedup
ok    github.com/marmos91/dittofs/pkg/blockstore/engine    1.355s
```

Acceptance-criteria greps:

```
grep -c "newCount, err := bs.coordinator.DecrementRefCount" pkg/blockstore/engine/engine.go   -> 1
grep -c "bs.syncedHashStore.DeleteSynced"                   pkg/blockstore/engine/engine.go   -> 1
grep -c "trySpeculativeFileLevelDedup"                      pkg/blockstore/engine/engine.go   -> 1
grep -c "func (m \*Syncer) TrySpeculativeFileLevelDedup"    pkg/blockstore/engine/syncer.go   -> 0
grep -c "func (m \*Syncer) trySpeculativeFileLevelDedup"    pkg/blockstore/engine/dedup.go    -> 1
grep -rln "TrySpeculativeFileLevelDedup" pkg/ | grep -v _test.go                              -> none
```

Provenance scan (no Phase / D-NN refs in new lines):

```
git diff 9e45255c..HEAD | grep '^+' | grep -E 'Phase 18|D-0[0-9]|D-1[0-9]'
  (no matches)
```

## Commits

| Hash       | Message                                                                          |
| ---------- | -------------------------------------------------------------------------------- |
| `e981d446` | refactor(18-07): relocate file-level dedup pre-hook into engine.Flush            |
| `44c40d26` | feat(18-07): cascade DeleteSynced from engine.Delete on refcount=0               |
| `e51036b0` | refactor(18-07): delete public Syncer.TrySpeculativeFileLevelDedup wrapper       |

All three commits GPG-signed (ED25519, key SHA256:n4Yfcg8pGMUtN9fYWsxii3zAz+xCIJhA6o2v3D1tsEY).

Per-task commit boundary:

- Commit 1 (Task 1) lifts the pre-rollup hook into `engine.Flush`. The public `Syncer.TrySpeculativeFileLevelDedup` wrapper is still present at this point — keeping the build clean before deletion.
- Commit 2 (Task 2) adds the refcount cascade + the new test file. Builds clean; new test passes (under -race) in isolation.
- Commit 3 (Task 3) deletes the now-dead public wrapper. Build still clean; only one in-tree reference to the uppercase symbol remains (a comment in the intentionally-RED `syncer_flush_test.go`, swept in Plan 08).

## Deviations from Plan

None of the deviations affect correctness or change the must_haves contract.

**1. [Plan re-targeting] Custom refcount fake instead of extending shared fakeCoordinator**

- **Found during:** Task 2 fixture construction.
- **Issue:** The shared `fakeCoordinator` in `coordinator_test.go` hardcodes `DecrementRefCount` returning `(0, nil)` on every call. This means a test using it could not distinguish the "cascade fires (newCount==0)" path from the "cascade does not fire (newCount > 0)" path — both subtests would show the same return value, defeating the cascade assertion.
- **Fix:** Added a purpose-built `refcountCoordinator` in `engine_delete_test.go` with a seedable per-hash counts map. Same `MetadataCoordinator` interface; richer state.
- **Files modified:** `pkg/blockstore/engine/engine_delete_test.go`
- **Commit:** `44c40d26`

**2. [Test scope expansion] Added 4th subtest for benign-orphan failure path**

- **Found during:** Task 2 implementation.
- **Issue:** Plan listed 3 subtests (RefcountZero, RefcountNonZero, NilSyncedStore). The plan also requires "DeleteSynced failure path uses logger.Warn, NOT return (benign-orphan logging contract)" in the acceptance criteria and threat-register T-18-07-03. Without an explicit subtest, this invariant is asserted only in code-review, not regression-testable.
- **Fix:** Added `DeleteSyncedFailure_IsBenign` subtest using the recordingSyncedHashStore's `deleteErr` single-shot injection. Asserts `bs.Delete(...)` returns nil even when `DeleteSynced` returns an error, and that the cascade was actually attempted.
- **Files modified:** `pkg/blockstore/engine/engine_delete_test.go`
- **Commit:** `44c40d26`

## Known Stubs

None. The relocation is structural — no new placeholder data or unconfigured wiring. The intentionally-RED test files (api_blockref_test.go, syncer_flush_test.go, syncer_unit_test.go, syncer_crash_test.go, syncer_put_error_test.go, upload_test.go, perf_bench_test.go, perf_bench_phase12_test.go) carry over from Plan 06 and are owned by Plan 08's sweep, not this plan.

## Downstream Hooks

- **Plan 08** sweeps the intentionally-RED engine test files. The reference to the deleted `TrySpeculativeFileLevelDedup` in `syncer_flush_test.go:368` (one of the comments) will be cleaned up there alongside the symbol-level references in api_blockref_test.go / perf_bench_test.go / syncer_*_test.go / upload_test.go.
- **Plan 09** (integration) re-creates `pkg/blockstore/engine/syncer_test.go` with `//go:build integration` and adds an integration-level scenario for `TestEngine_Delete_CascadesDeleteSynced` against s3 + memory remote fixtures. The unit-level coverage added here exercises the engine boundary; the integration layer exercises the full round-trip including the actual remote store's idempotent re-Put on crash replay.

## Self-Check: PASSED

Verified:

- `pkg/blockstore/engine/engine.go` — FOUND (modified, +49 / -1 lines)
- `pkg/blockstore/engine/syncer.go` — FOUND (modified, -35 lines)
- `pkg/blockstore/engine/engine_delete_test.go` — FOUND (created, 292 lines, package `engine`)
- Commit `e981d446` — FOUND in git log (signed, ED25519)
- Commit `44c40d26` — FOUND in git log (signed, ED25519)
- Commit `e51036b0` — FOUND in git log (signed, ED25519)
- `go build ./...` — clean
- Public `TrySpeculativeFileLevelDedup` wrapper deletion confirmed via grep (0 matches in non-test code)
- Private `trySpeculativeFileLevelDedup` preserved in dedup.go (1 match — the original definition, untouched)
- 4 new subtests pass under `-race` (test execution in isolation, with intentionally-RED Plan-06 files quarantined and restored)
- Existing `TestDedup` suite passes in isolation (relocation does not regress private function semantics)
- No "Phase 18" / "D-NN" / .planning refs introduced in new code or new comments
