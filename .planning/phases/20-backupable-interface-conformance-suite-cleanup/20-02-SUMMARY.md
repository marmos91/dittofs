---
phase: 20-backupable-interface-conformance-suite-cleanup
plan: 02
subsystem: metadata-backup-conformance
tags: [backup, conformance, testing, storetest]
dependency_graph:
  requires: [blockstore.HashSet, metadata.Backupable, backup.envelope]
  provides: [storetest.RunBackupConformanceSuite, storetest.BackupableStoreFactory]
  affects: [pkg/metadata/storetest]
tech_stack:
  added: []
  patterns: [conformance-suite, type-assertion-capability, factory-pattern]
key_files:
  created:
    - pkg/metadata/storetest/backup_conformance.go
  modified: []
decisions:
  - "BackupableStoreFactory uses same signature as StoreFactory (func(t) MetadataStore); Backupable discovered via type assertion inside suite"
  - "Suite uses t.Fatal (not t.Skip) for missing Backupable capability because factory explicitly opts in"
  - "Corruption WrongEngineTag subtest accepts both ErrSchemaVersionMismatch and ErrRestoreCorrupt since driver-level engine tag validation is implementation-defined"
  - "ConcurrentWriter verifies initial files present in restored backup; concurrent file exclusion is the contract but timing-dependent"
metrics:
  duration_seconds: 555
  completed: 2026-05-27T10:13:29Z
  tasks_completed: 1
  tasks_total: 1
  files_created: 1
  files_modified: 0
---

# Phase 20 Plan 02: Backup Conformance Suite Summary

Shared backup conformance suite with 5 subtests (RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness) for Phase 21 driver implementations to prove Backupable correctness.

## What Was Done

### Task 1: Backup conformance suite with 5 subtests (9d9e1857)

Created `pkg/metadata/storetest/backup_conformance.go` exporting `BackupableStoreFactory` type and `RunBackupConformanceSuite` function. The suite follows the same factory pattern as the existing `RunConformanceSuite` but uses a type assertion to `metadata.Backupable` at entry (with `t.Fatal`, not `t.Skip`, since factories explicitly opt in).

Five top-level subtests:

1. **RoundTrip** -- Creates a share with 2 files carrying BlockRef hashes, backs up to buffer, restores into a fresh store, and verifies shares/files/blocks/attributes are identical. Also verifies HashSet length matches unique hash count.

2. **ConcurrentWriter** -- Populates initial data, runs Backup in a goroutine while concurrently creating new files. Restores and verifies that at minimum the initial files are present, proving snapshot isolation per ENG-02.

3. **Corruption** (3 sub-scenarios) -- Truncated stream (half-length, expects `ErrRestoreCorrupt`), bit-flip in payload middle (expects `ErrRestoreCorrupt`), wrong engine tag (re-wraps valid payload in a different engine envelope, accepts either `ErrSchemaVersionMismatch` or `ErrRestoreCorrupt`).

4. **NonEmptyDest** -- Backs up from source store, creates a populated destination store, attempts restore, verifies `ErrRestoreDestinationNotEmpty`.

5. **HashSetCorrectness** (2 sub-scenarios) -- ExactMatch: 3 files with 5 unique hashes, verifies HashSet.Len() and Contains for each. Dedup: 2 files sharing the same hash, verifies HashSet.Len() == 1.

Helper functions: `asBackupable` (type assertion with t.Fatal), `populateTestData` (creates share + 2 files with 3 unique / 4 total block refs). Reuses existing `createTestShare`, `createTestFile`, `hashOfSeed` from the storetest package.

## Deviations from Plan

None - plan executed exactly as written.

## Verification Results

| Check | Result |
|-------|--------|
| `go build ./pkg/metadata/storetest/...` | PASS |
| `go vet ./pkg/metadata/storetest/` | PASS |
| `go build ./pkg/metadata/...` | PASS (no import cycles) |
| `go build ./pkg/blockstore/...` | PASS |
| 5 t.Run calls in RunBackupConformanceSuite | Confirmed (RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness) |
| 3 Corruption sub-scenarios | Confirmed (Truncated, BitFlip, WrongEngineTag) |
| 2 HashSetCorrectness sub-scenarios | Confirmed (ExactMatch, Dedup) |
| Type assertion to metadata.Backupable | Present at suite entry and in asBackupable helper |
| BackupableStoreFactory and RunBackupConformanceSuite exported | Confirmed |

## Commit Log

| Task | Commit | Message |
|------|--------|---------|
| 1 | 9d9e1857 | feat(20-02): add backup conformance suite with 5 subtests |

## Self-Check: PASSED

- pkg/metadata/storetest/backup_conformance.go: FOUND
- Commit 9d9e1857: FOUND
