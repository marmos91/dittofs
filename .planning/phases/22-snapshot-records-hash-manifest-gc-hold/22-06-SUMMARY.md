---
phase: 22-snapshot-records-hash-manifest-gc-hold
plan: 06
subsystem: controlplane/runtime
tags: [snapshot, gc, integration-test, hold-provider]
requires:
  - 22-01 (Snapshot model)
  - 22-02 (Manifest reader/writer)
  - 22-03 (SnapshotStore CRUD)
  - 22-04 (Engine HoldProvider injection)
  - 22-05 (Runtime HoldProvider wire-up + RemoveShare snapshot cleanup hook)
provides:
  - End-to-end regression guard for the snapshot-vs-GC contract
affects:
  - pkg/controlplane/runtime (test-only)
tech-stack:
  added: []
  patterns:
    - sub-tests via t.Run sharing a single fixture for ordered lifecycle scenarios
    - bypass of Backup's metadata extraction in favor of an explicit manifest to keep
      live-set / held-set semantics distinguishable
key-files:
  created:
    - pkg/controlplane/runtime/snapshot_lifecycle_test.go
  modified: []
decisions:
  - "Manifest constructed explicitly (hLive1 + hSnap) rather than fed by Backup output:
    the memory backend's Backup pulls hashes from fileData.Attr.Blocks, not from the
    FileBlockStore. Live FileBlocks are seeded via st.Put so EnumerateFileBlocks streams
    them into the GC live set, while the manifest carries hSnap to model the canonical
    'snapshot taken at T0 then file deleted at T1' divergence."
  - "Per-snapshot delete in test mimics both halves explicitly (DB row via
    DeleteSnapshot + os.RemoveAll of snapshot dir) because the per-snapshot cleanup
    pairing is a future orchestration concern; today only RemoveShare wires filesystem
    cleanup."
  - "MetadataEngine='memory' on Snapshot rows: CONTEXT D-21 scopes the integration
    test to the memory backend; the field is informational on the row and not validated."
  - "setShareRemoteForTest used over a full engine.BlockStore build: matches the
    existing blockgc_test.go fixture pattern and keeps the test focused on
    HoldProvider + RunBlockGC semantics, not block-store composition."
metrics:
  duration_minutes: 25
  completed_date: 2026-05-28
---

# Phase 22 Plan 06: End-to-End Snapshot Lifecycle Integration Test Summary

One-liner: integration test that drives the full Phase 22 wire (memory metadata
backend Backupable → HashSet → WriteManifestAtomic → SnapshotStore CRUD →
Runtime.RunBlockGC with HoldProvider) and locks in held-block survival,
held-block release after deletion, and RemoveShare snapshots-tree cleanup.

## What was built

A single test file, `pkg/controlplane/runtime/snapshot_lifecycle_test.go`,
containing one top-level test `TestSnapshotLifecycleVsGC` with three sequential
sub-tests sharing a `lifecycleFixture`:

1. **`snapshot ready preserves held block`** — creates a snapshot row, writes
   a manifest covering `hLive1` and `hSnap` to disk, transitions state to
   ready, runs `RunBlockGC`, and asserts that `hLive1`, `hLive2`, and `hSnap`
   all survive while `hOrphan` is collected. `ObjectsSwept == 1`.
2. **`snapshot deletion releases held block`** — calls
   `SnapshotStore.DeleteSnapshot` + `os.RemoveAll` on the snapshot directory,
   re-runs `RunBlockGC`, and asserts `hSnap` is now collected (no longer held)
   while `hLive1` / `hLive2` still survive. `ObjectsSwept == 1`.
3. **`RemoveShare cleans snapshots tree`** — creates a fresh ready snapshot on
   disk, calls `Runtime.RemoveShare`, and asserts the entire
   `<localStoreDir>/snapshots/` tree is gone — even with a ready DB row still
   present at call time. This guards the RemoveShare cleanup hook from
   plan 22-05.

## Fixture composition

- `cpstore.New` with in-memory SQLite for snapshot CRUD.
- `metadatamemory.NewMemoryMetadataStoreWithDefaults()` registered under name
  `"memory"`; seeded with two FileBlocks (`hLive1`, `hLive2`) that
  `EnumerateFileBlocks` streams to the GC mark phase.
- A share `"data"` added via `Runtime.AddShare` with a per-share
  `localStoreDir = t.TempDir()` set through
  `sharesSvc.SetLocalStoreDirForTesting` (the memory backend's AddShare path
  does not derive one).
- A `remotememory.Store` bound through `setShareRemoteForTest` so
  `DistinctRemoteStores` surfaces it to `RunBlockGC`. Seeded with the four
  CAS objects; `SetNowFnForTest` pushes `LastModified` 2h into the past so the
  engine's default 1h grace TTL does not preserve any seeded object.

## Deliberate divergence from CONTEXT D-21 path

The plan offered two manifest-construction paths: (a) bypass Backup with a
synthetic HashSet, or (b) round-trip through Backup then delete the snapshot
hash from live metadata. I took option (a) — the memory store's Backup
extracts hashes from `fileData.Attr.Blocks`, which requires creating actual
files (CreateFile + WriteFile). Seeding only the FileBlock store (which is
what the GC mark phase actually consumes) and then constructing the manifest
explicitly keeps the test focused on GC-hold semantics without coupling it to
the metadata store's file-creation API. Backup is still called once and its
success asserted — proving the Backupable wiring is reachable from the
runtime — but its returned HashSet is discarded in favor of the explicit
manifest. This is the path the plan explicitly sanctioned with the comment
"either path satisfies the integration intent".

## Verification

- `go test ./pkg/controlplane/runtime/... -run 'TestSnapshotLifecycleVsGC' -count=1 -race -v`: PASS (3/3 sub-tests).
- `go test ./pkg/controlplane/... -count=1 -race`: PASS (no regression in the broader controlplane suite).
- `gofmt -l pkg/controlplane/runtime/snapshot_lifecycle_test.go`: empty (clean).
- `go vet ./pkg/controlplane/runtime/...`: clean.
- No GSD IDs in test source: confirmed via `grep -E 'Phase 22|D-0[0-9]|SNAP-'` returns empty.

## Deviations from Plan

None — the plan's "alternative path" clause for manifest construction was the
explicit second option the plan listed, and the executor selected it for the
documented reason (avoids coupling the test to the memory metadata store's
file-creation API).

## Self-Check: PASSED
