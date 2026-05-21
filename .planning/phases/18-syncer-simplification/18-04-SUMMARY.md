---
phase: 18-syncer-simplification
plan: 04
subsystem: blockstore-local
tags: [syncer, listunsynced, syncedhashstore, iter-seq2, fsstore, memorystore]
requires:
  - "pkg/metadata.SyncedHashStore (from 18-01)"
  - "pkg/metadata/store/memory.MemoryMetadataStore.IsSynced/MarkSynced/DeleteSynced (from 18-01)"
provides:
  - "local.LocalStore.ListUnsynced(ctx) iter.Seq2[blockstore.ContentHash, error]"
  - "fs.FSStoreOptions.SyncedHashStore injection slot"
  - "fs.FSStore.ListUnsynced Walk-snapshot-then-filter implementation"
  - "memory.MemoryStore.ListUnsynced empty-yield implementation"
affects:
  - "pkg/blockstore/local/local.go (interface delta — additive only)"
  - "pkg/blockstore/local/fs/fs.go (struct + options + constructor)"
  - "pkg/blockstore/local/fs/blockstore_methods.go (new method)"
  - "pkg/blockstore/local/memory/memory.go (new method)"
tech-stack:
  added:
    - "Go 1.23 iter.Seq2 push-iterator stdlib type"
  patterns:
    - "Snapshot-at-start enumeration (Walk-collect, then filter) — bounded iteration under hot-write workloads"
    - "Closure-based iter.Seq2 with ctx.Err() probe per yield"
    - "Nil-injection-slot defensive guard for local-only deployments"
key-files:
  created:
    - pkg/blockstore/local/fs/blockstore_methods_test.go
  modified:
    - pkg/blockstore/local/local.go
    - pkg/blockstore/local/fs/fs.go
    - pkg/blockstore/local/fs/blockstore_methods.go
    - pkg/blockstore/local/memory/memory.go
decisions:
  - "Honored D-04 (Go 1.23 push iterator surface)"
  - "Honored D-05 (snapshot-at-start; no live-tail catch-up)"
  - "Honored D-06 (O(1) per-hash IsSynced — no ListSyncedHashes set-difference variant)"
  - "Honored D-09 (strict-subset invariant: nil SyncedHashStore -> empty iterator)"
  - "Honored D-01 (Tasks 1+2 landed in a single atomic commit so develop's tip stays buildable)"
metrics:
  duration: "~20 minutes"
  tasks_completed: 3
  files_touched: 5
  commits: 2
  completed: 2026-05-21
---

# Phase 18 Plan 04: LocalStore.ListUnsynced + SyncedHashStore Injection Summary

Wave 2 surface: streaming "what needs uploading" enumeration on the local tier, plus the injection slot the engine Syncer (Plan 06) will consume to drive mirror writes.

## One-Liner

Added `LocalStore.ListUnsynced(ctx) iter.Seq2[blockstore.ContentHash, error]` with snapshot-at-start semantics on FSStore (Walk-collect then O(1) IsSynced filter) and an empty-yield satisfaction on MemoryStore, plumbed the `SyncedHashStore` field through `FSStoreOptions` and the FSStore constructor, and covered the contract with six race-clean subtests.

## What Landed

### Interface (additive only)

`pkg/blockstore/local/local.go`

- Added `"iter"` to imports.
- New method on `LocalStore`, placed adjacent to the `blockstore.BlockStoreAppend` embedding (Walk's natural neighbor):

  ```go
  ListUnsynced(ctx context.Context) iter.Seq2[blockstore.ContentHash, error]
  ```

- Godoc documents:
  - snapshot-at-start semantics (no live-tail catch-up),
  - first-non-nil-error iteration halt,
  - nil-SyncedHashStore collapse to empty iterator.

### Injection plumbing

`pkg/blockstore/local/fs/fs.go`

- New struct field on `*FSStore`: `syncedHashStore metadata.SyncedHashStore` (alongside `rollupStore`).
- New slot on `FSStoreOptions`: `SyncedHashStore metadata.SyncedHashStore` with godoc covering "Required when remote configured; nil accepted for local-only".
- Constructor wiring inside `newFSStoreWithOptionsInternal`: `bc.syncedHashStore = opts.SyncedHashStore` directly after the `RollupStore` plumb.

### FSStore implementation

`pkg/blockstore/local/fs/blockstore_methods.go`

- Added `"iter"` to imports.
- `(*FSStore).ListUnsynced` is a closure-based `iter.Seq2` that:
  1. Early-returns (empty iterator) when `bc.syncedHashStore == nil` — strict-subset invariant.
  2. Calls `bc.Walk(ctx, …)` to collect every CAS hash into a local `snapshot []blockstore.ContentHash`. Walk's directory file handles release before the filter loop runs.
  3. On Walk error, yields `(zero, wrapped err)` and returns.
  4. Iterates the snapshot: probes `ctx.Err()` per item; calls `bc.syncedHashStore.IsSynced(ctx, h)` (O(1) per D-06); skips synced hashes; yields `(h, nil)` for unsynced. Per-hash lookup errors yield `(h, wrapped err)` and respect a `false` return from `yield` for clean consumer cancel.

### MemoryStore implementation

`pkg/blockstore/local/memory/memory.go`

- Added `"iter"` to imports.
- `(*MemoryStore).ListUnsynced` is an empty-yield closure. Rationale: per package godoc, MemoryStore is used only for tests and ephemeral configurations; there is no production Syncer wired against it, so a hash-walk-plus-IsSynced loop would have no consumer. Interface satisfaction only.

### Tests

`pkg/blockstore/local/fs/blockstore_methods_test.go` (new file, 6 subtests):

| Subtest             | Behavior verified                                                                                                                                                                                                                                                  |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `EmptyStore`        | Fresh FSStore + wired SyncedHashStore — iterator yields zero items, no errors.                                                                                                                                                                                     |
| `NilSyncedHashStore`| FSStore with `SyncedHashStore: nil` + 5 seeded chunks — iterator yields zero items (strict-subset invariant collapse).                                                                                                                                              |
| `AllSynced`         | 4 chunks all `MarkSynced` — iterator yields zero items.                                                                                                                                                                                                            |
| `AllUnsynced`       | 4 chunks, none marked — iterator yields exactly the seeded set (set equality).                                                                                                                                                                                     |
| `Partial`           | 5 chunks, 2 marked synced — iterator yields the other 3 (set equality).                                                                                                                                                                                            |
| `CtxCancelMidIter`  | 10 chunks; cancel ctx after the 1st successful yield; iterator surfaces `context.Canceled` in the next yield slot and stops.                                                                                                                                       |

Test fixture uses `memory.NewMemoryMetadataStoreWithDefaults()` for the `SyncedHashStore`. Hash addressing uses BLAKE3-256 to match `StoreChunk`'s scheme. Helper `newFSStoreForTest` (existing) wires the test-only `nopFBS`.

## Verification Evidence

```
go build ./...                                           OK
go vet ./pkg/blockstore/local/...                        OK
go test -race -count=1 ./pkg/blockstore/local/fs/... -run TestFSStore_ListUnsynced -v
  PASS: TestFSStore_ListUnsynced/EmptyStore
  PASS: TestFSStore_ListUnsynced/NilSyncedHashStore
  PASS: TestFSStore_ListUnsynced/AllSynced
  PASS: TestFSStore_ListUnsynced/AllUnsynced
  PASS: TestFSStore_ListUnsynced/Partial
  PASS: TestFSStore_ListUnsynced/CtxCancelMidIter
go test -race -count=1 ./pkg/blockstore/local/...        ok (full package)
```

Provenance scan (no Phase/D-NN/.planning refs in newly-added lines):

```
git diff … | grep '^+' | grep -E 'Phase 18|D-0[0-9]|\.planning'
  (no matches)
```

`LocalStore` implementer enumeration unchanged at 2:

```
grep -rn "var _ local.LocalStore" pkg/
  pkg/blockstore/local/fs/fs.go:45    — (*FSStore)(nil)
  pkg/blockstore/local/memory/memory.go:23 — (*MemoryStore)(nil)
```

Both satisfy the interface post-change.

## Commits

| Hash       | Message                                                                  |
| ---------- | ------------------------------------------------------------------------ |
| `80e3cb5c` | feat(18-04): add LocalStore.ListUnsynced + SyncedHashStore injection     |
| `fc3e0d12` | test(18-04): cover FSStore.ListUnsynced contract                         |

Tasks 1 and 2 share commit `80e3cb5c` per D-01 (atomic-merge invariant — interface declaration and both implementations land together so develop's tip stays buildable). Task 3 ships as its own `test(...)` commit.

Both commits are GPG-signed (ED25519, key SHA256:n4Yfcg8pGMUtN9fYWsxii3zAz+xCIJhA6o2v3D1tsEY).

## Deviations from Plan

None — plan executed exactly as written.

## Known Stubs

None. The MemoryStore empty-yield is documented in-godoc as the intended terminal implementation (no production Syncer is wired against MemoryStore) — not a stub.

## Downstream Hooks

- Plan 06 (Syncer mirror loop): consumes `LocalStore.ListUnsynced` + the wired `SyncedHashStore.MarkSynced` to drive uploads.
- Plan 08 (transitional method deletion): unaffected — `ListUnsynced` is additive; the 7 `TRANSITIONAL-PHASE-18` methods on `LocalStore` are left untouched here.
- Engine constructor wiring (Plan 06 or 07 per CONTEXT): must source `opts.SyncedHashStore` from the per-share metadata-store handle, mirroring how `opts.RollupStore` is supplied today.

## Self-Check: PASSED

Verified:

- `pkg/blockstore/local/fs/blockstore_methods_test.go` — FOUND
- `pkg/blockstore/local/local.go` — FOUND (modified)
- `pkg/blockstore/local/fs/fs.go` — FOUND (modified)
- `pkg/blockstore/local/fs/blockstore_methods.go` — FOUND (modified)
- `pkg/blockstore/local/memory/memory.go` — FOUND (modified)
- Commit `80e3cb5c` — FOUND in git log
- Commit `fc3e0d12` — FOUND in git log
