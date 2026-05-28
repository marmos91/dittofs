---
phase: 18-syncer-simplification
plan: 05
subsystem: blockstore-local
tags: [rollup, objectid, persister, fsstore, computeobjectid, blockref]
requires:
  - "blockstore.ComputeObjectID(blocks []BlockRef) ObjectID (existing — pkg/blockstore/objectid.go)"
  - "blockstore.BlockRef{Hash, Offset, Size} (existing — pkg/blockstore/types.go)"
  - "fs.FSStoreOptions.SyncedHashStore injection seam (from 18-04, mirrored for the new ObjectIDPersister)"
provides:
  - "fs.ObjectIDPersister callback type (ctx, payloadID, blocks, objectID) -> error"
  - "fs.FSStoreOptions.ObjectIDPersister slot + FSStore.objectIDPersister field + constructor wiring"
  - "rollupFile post-SetRollupOffset ObjectID compute + persister invocation"
  - "BlockRef manifest accumulated in the chunker loop (sorted-by-Offset via monotone pos)"
affects:
  - "pkg/blockstore/local/fs/fs.go (option + struct + constructor)"
  - "pkg/blockstore/local/fs/rollup.go (chunker-loop accumulator + post-commit hook)"
  - "pkg/blockstore/local/fs/rollup_test.go (new test, three subtests)"
tech-stack:
  added: []
  patterns:
    - "Narrow callback injection (no engine import) — mirrors RollupStore / SyncedHashStore wiring shape"
    - "Chunker-loop accumulator for BlockRefs with absolute Offset under monotone-pos invariant"
    - "Nil-callback fallthrough: compute still runs; persist step skipped (local-only / no-engine fixtures)"
key-files:
  created: []
  modified:
    - pkg/blockstore/local/fs/fs.go
    - pkg/blockstore/local/fs/rollup.go
    - pkg/blockstore/local/fs/rollup_test.go
decisions:
  - "Honored D-10 (ComputeObjectID moves to rollup.go post-SetRollupOffset; local-only shares now get ObjectIDs)"
  - "Chose narrow ObjectIDPersister callback (PATTERNS §11 option 2) over a coordinator-interface injection to avoid an engine -> local import cycle"
  - "BlockRef.Offset uses the absolute file offset pos (chunker advances monotonically from minOff); sort.SliceIsSorted invariant verified in Test 1"
  - "Persister error wrapped as 'rollup: ObjectIDPersister: %w' so callers can errors.Is the wrapped sentinel"
  - "Crash-window between SetRollupOffset success and persister failure documented inline (matches the pre-existing Syncer-side persist failure shape, deferred to Plan 06's deletion sweep)"
metrics:
  duration: "~25 minutes"
  tasks_completed: 3
  files_touched: 3
  commits: 3
  completed: 2026-05-21
---

# Phase 18 Plan 05: ComputeObjectID relocation to rollup.go Summary

Wave 3 structural correction: ObjectID becomes a property of the BlockRef manifest computed at rollup time, no longer a side-effect of remote upload.

## One-Liner

Added `fs.ObjectIDPersister` callback slot to `FSStoreOptions`, taught `rollupFile` to accumulate the per-chunk BlockRef manifest in the existing chunker loop and invoke `blockstore.ComputeObjectID(blocks)` + the persister after `SetRollupOffset` returns nil — so local-only shares now materialize non-zero ObjectIDs at rollup time without a remote upload trail. Covered the contract with three race-clean subtests on `TestRollup_CommitChunks_PersistsObjectID`.

## What Landed

### Plumbing

`pkg/blockstore/local/fs/fs.go`

- New exported callback type adjacent to the existing `var _ local.LocalStore` assertion:

  ```go
  type ObjectIDPersister func(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, objectID blockstore.ObjectID) error
  ```

- New field on `*FSStore`: `objectIDPersister ObjectIDPersister`, alongside the Plan-04-landed `syncedHashStore`. Godoc documents nil-fallthrough semantics.
- New `FSStoreOptions.ObjectIDPersister` slot with godoc covering "wire to coordinator.PersistFileBlocks; nil is accepted for local-only / no-engine fixtures".
- Constructor wiring inside `newFSStoreWithOptionsInternal`: `bc.objectIDPersister = opts.ObjectIDPersister` directly after the `SyncedHashStore` plumb.

### Rollup hook

`pkg/blockstore/local/fs/rollup.go`

- Inside `rollupFile`'s chunker loop (the `for pos < uint64(len(stream))` body), declared `var blocks []blockstore.BlockRef` before the loop and appended one BlockRef per emitted chunk:

  ```go
  blocks = append(blocks, blockstore.BlockRef{
      Hash:   h,
      Offset: pos,
      Size:   uint32(b),
  })
  ```

  Sorted-by-Offset is automatic because `pos` advances monotonically — that matches the canonical `FileAttr.Blocks` invariant `blockstore.ComputeObjectID` relies on. Invariant explicitly verified in Test 1 via `sort.SliceIsSorted`.

- After `SetRollupOffset` returns nil and BEFORE `advanceRollupOffset`, inserted:

  ```go
  objectID := blockstore.ComputeObjectID(blocks)
  if bc.objectIDPersister != nil {
      if err := bc.objectIDPersister(ctx, payloadID, blocks, objectID); err != nil {
          return fmt.Errorf("rollup: ObjectIDPersister: %w", err)
      }
  }
  ```

  Crash-window note documented inline: persister failure leaves rollup offset advanced + ObjectID unset; operator re-trigger by writing a new chunk reseeds. Matches the pre-existing Syncer-side persist failure shape; the legacy Syncer call site is deleted in Plan 06.

### Tests

`pkg/blockstore/local/fs/rollup_test.go` (existing file extended, plus helper)

Added imports (`errors`, `sort`, `sync`, `pkg/blockstore`), a small `capturedPersist` struct, a `runRollupOnce` helper that mirrors the existing `TestRollup_CommitChunks_MonotoneEnforced` "stabilize then call rollupFile" pattern, and a new test function `TestRollup_CommitChunks_PersistsObjectID` with three subtests:

| Subtest                       | Behavior verified                                                                                                                                                                                                                                                                                                          |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PersistsObjectIDOnCommit`    | 8 MiB AppendWrite -> rollup; persister fires exactly once with: matching payloadID, non-empty BlockRefs, `sort.SliceIsSorted(by Offset)`, captured `objectID == ComputeObjectID(captured blocks)`, and a non-zero (non-sentinel) ObjectID -- proving the local-only path materializes a real identity with no remote.       |
| `NilPersisterIsBenign`        | 8 MiB AppendWrite -> rollup on an FSStore configured with `ObjectIDPersister: nil`; no panic, no error, and `rollup_offset` advances past `logHeaderSize` (rollup quiesces normally).                                                                                                                                       |
| `PersisterErrorPropagates`    | 1 MiB AppendWrite -> rollup with a persister that returns `errors.New("simulated...")`; `rollupFile` returns a non-nil error and `errors.Is(err, simulated)` holds -- proving the `fmt.Errorf("rollup: ObjectIDPersister: %w", ...)` wrap preserves the chain.                                                              |

No mocks on Coordinator / PersistFileBlocks; the callback boundary is the only mock surface, matching PATTERNS §19 (the relocated successor of Phase 13's `TestSyncer_Flush_InvokesPostFlushHook` — neutral domain language only, no Phase / D-NN provenance in the new test name).

## Verification Evidence

```
go build ./pkg/blockstore/local/...                                                       OK
go vet   ./pkg/blockstore/local/...                                                       OK
go build ./...                                                                            OK (engine still has its old persist call site; double-invocation impossible because no caller has wired the new persister yet)
go test  -race -count=1 ./pkg/blockstore/local/fs/... -run TestRollup_CommitChunks_PersistsObjectID -v
  PASS: TestRollup_CommitChunks_PersistsObjectID/PersistsObjectIDOnCommit
  PASS: TestRollup_CommitChunks_PersistsObjectID/NilPersisterIsBenign
  PASS: TestRollup_CommitChunks_PersistsObjectID/PersisterErrorPropagates
go test  -race -count=1 ./pkg/blockstore/local/fs/...                                     ok (full package, no neighbour regressions)
```

Acceptance-criteria greps:

```
grep -c "ComputeObjectID"          pkg/blockstore/local/fs/rollup.go        -> 1
grep -c "bc.objectIDPersister"     pkg/blockstore/local/fs/rollup.go        -> 2
grep -c "blockstore.BlockRef{"     pkg/blockstore/local/fs/rollup.go        -> 1
grep -q "type ObjectIDPersister func"           pkg/blockstore/local/fs/fs.go  -> OK
grep -q "objectIDPersister ObjectIDPersister"   pkg/blockstore/local/fs/fs.go  -> OK
grep -q "ObjectIDPersister ObjectIDPersister"   pkg/blockstore/local/fs/fs.go  -> OK
grep -q "bc.objectIDPersister = opts.ObjectIDPersister"  pkg/blockstore/local/fs/fs.go  -> OK
```

Provenance scan (no "Phase 18" / "D-NN" / ".planning" refs in any added line):

```
git diff effb8090^..HEAD -- pkg/blockstore/local/fs/fs.go pkg/blockstore/local/fs/rollup.go pkg/blockstore/local/fs/rollup_test.go \
  | grep '^+' | grep -E 'Phase 18|D-0[0-9]|D-1[0-9]|\.planning'
  (no matches)
```

## Commits

| Hash       | Message                                                                  |
| ---------- | ------------------------------------------------------------------------ |
| `effb8090` | feat(18-05): add ObjectIDPersister slot to FSStoreOptions                |
| `ea65b5a8` | feat(18-05): compute ObjectID in rollup CommitChunks hook                |
| `715b32fb` | test(18-05): cover rollup ObjectID persist hook                          |

All three commits GPG-signed (ED25519, key SHA256:n4Yfcg8pGMUtN9fYWsxii3zAz+xCIJhA6o2v3D1tsEY).

Tasks ship as three separate commits (one per task) because the plumbing-only commit (Task 1) leaves no observable behavior change and is a clean review boundary, the rollup hook (Task 2) introduces the runtime change in isolation, and the test commit (Task 3) is conventional `test(...)` provenance for the new coverage.

## Deviations from Plan

None — plan executed exactly as written.

The plan's verify step for Task 2 used a portable `grep -c` pipeline; the project gates were ultimately checked via direct `grep -c` returns (1, 2, 1), all in the required positive range.

## Known Stubs

None. The nil-persister fallthrough is documented behavior, not a stub — the engine wiring lands in Plan 06 along with deletion of the legacy `Syncer.persistFileBlocksAfterFlush` site.

## Downstream Hooks

- **Plan 06 (Syncer mirror loop)** — wires `engine.NewBlockStore` to set `FSStoreOptions.ObjectIDPersister = func(ctx, pid, blocks, oid) error { return bs.coordinator.PersistFileBlocks(ctx, pid, blocks, oid) }` and deletes the now-redundant `Syncer.persistFileBlocksAfterFlush` plus its call site. Until Plan 06 lands, the new persister slot is unused in production — no double-invocation risk.
- **Phase 13 test sweep** — `TestSyncer_Flush_InvokesPostFlushHook` (and the seven `TestSyncer_Flush_*` siblings in `syncer_flush_test.go`) become deletion targets when Plan 06 removes `persistFileBlocksAfterFlush`. The new `TestRollup_CommitChunks_PersistsObjectID` already covers the happy / nil / error paths at the rollup boundary, so Plan 06 can delete the engine-side tests rather than retarget them.
- **Crash-window posture** — documented in `rollup.go` comments adjacent to the new call. If the persister proves brittle in practice (Plan 06 review), a follow-up could either (a) defer ObjectID compute to a post-rollup batch sweep, or (b) extend the rollup commit to a 3-phase write (offset -> objectid -> header) — both deferred outside this plan.

## Self-Check: PASSED

Verified:

- `pkg/blockstore/local/fs/fs.go` -- FOUND (modified)
- `pkg/blockstore/local/fs/rollup.go` -- FOUND (modified)
- `pkg/blockstore/local/fs/rollup_test.go` -- FOUND (modified)
- Commit `effb8090` -- FOUND in git log (signed)
- Commit `ea65b5a8` -- FOUND in git log (signed)
- Commit `715b32fb` -- FOUND in git log (signed)
