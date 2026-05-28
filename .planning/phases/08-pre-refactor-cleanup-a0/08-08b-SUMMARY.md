---
phase: 08-pre-refactor-cleanup-a0
plan: 08b
subsystem: metadata
tags: [backup-removal, td-03, d-30]
status: complete
requires: [08-08a]
provides: []
affects: [pkg/metadata, pkg/metadata/storetest, pkg/metadata/store/badger, pkg/metadata/store/memory, pkg/metadata/store/postgres]
tech-stack:
  added: []
  removed: [Backupable shim, PayloadIDSet alias, per-backend Backup/Restore impls, RunBackupConformanceSuite]
  patterns: []
key-files:
  created: []
  modified:
    - pkg/metadata/store/badger/store.go
    - pkg/metadata/store/memory/store.go
    - pkg/metadata/store/postgres/store.go
  deleted:
    - pkg/metadata/backup.go
    - pkg/metadata/backup_shim_test.go
    - pkg/metadata/storetest/backup_conformance.go
    - pkg/metadata/store/badger/backup.go
    - pkg/metadata/store/badger/backup_test.go
    - pkg/metadata/store/memory/backup.go
    - pkg/metadata/store/memory/backup_test.go
    - pkg/metadata/store/postgres/backup.go
    - pkg/metadata/store/postgres/backup_test.go
decisions:
  - "Collapsed into single atomic commit with 08-08a/08-09/08-10 due to import cycle."
  - "storetest/suite.go was NOT edited (confirmed 2026-04-23: it has zero backup references)."
metrics:
  completed: 2026-04-23
---

# Phase 08 Plan 08b: Metadata Backup Shim + Per-backend + Conformance Removal Summary

Deleted the `pkg/metadata` backup shim (type aliases for `Backupable` / `PayloadIDSet` / sentinels), its per-backend implementations in `pkg/metadata/store/{badger,memory,postgres}/backup.go`, and the conformance suite at `pkg/metadata/storetest/backup_conformance.go`. Part of the v0.13.0 backup deletion (D-30 step 5).

## One-liner

Metadata-layer backup surface (shim + 3 per-backend impls + conformance suite) deleted as part of atomic PR-B collapse.

## Commit

- **SHA:** `7308eb92f4c63446d9de28acb5e669a188066d87`
- **Subject:** `refactor: remove v0.13.0 backup system (D-30 steps 4-7 combined, TD-03)`
- **Signed:** yes

This plan was collapsed into the atomic commit alongside 08-08a, 08-09, and 08-10 — see 08-08a-SUMMARY.md for the cycle explanation.

## Pre-audit greps (confirmed empty)

- `grep -rn "metadata\.Backupable\|metadata\.PayloadIDSet\|metadata\.ErrBackupUnsupported" . --include='*.go'` (excluding files being deleted) -> 0
- `grep -rn "RunBackupConformanceSuite\|BackupTestStore\|BackupStoreFactory\|BackupSuiteOptions" . --include='*.go'` (excluding files being deleted) -> 0

## Deletions attributed to this plan

### Files deleted (9)

- `pkg/metadata/backup.go` (shim: `Backupable`, `PayloadIDSet`, `ErrBackupUnsupported` aliases)
- `pkg/metadata/backup_shim_test.go`
- `pkg/metadata/storetest/backup_conformance.go` (contained `RunBackupConformanceSuite`, `RunBackupConformanceSuiteWithOptions`, `BackupTestStore`, `BackupStoreFactory`, `BackupSuiteOptions`, `TestStoreID_PreservedAcrossRestore`)
- `pkg/metadata/store/badger/backup.go`
- `pkg/metadata/store/badger/backup_test.go`
- `pkg/metadata/store/memory/backup.go`
- `pkg/metadata/store/memory/backup_test.go`
- `pkg/metadata/store/postgres/backup.go`
- `pkg/metadata/store/postgres/backup_test.go`

### storetest/suite.go: NOT modified

Confirmed via `grep -n "Backup" pkg/metadata/storetest/suite.go` -> zero matches. `RunConformanceSuite` never called `RunBackupConformanceSuite`; only the per-backend `backup_test.go` files did. No edits needed.

### Deviation edits (comment-only, to zero out residual `storebackups` mentions)

The three `store.go` files in `pkg/metadata/store/{badger,memory,postgres}/` had comments referencing the now-deleted `pkg/controlplane/runtime/storebackups/target.go` as a caller of `GetStoreID()`. These comments were updated to remove the dangling references:

- `pkg/metadata/store/badger/store.go`: simplified `ensureStoreID` doc comment (removed `allBackupPrefixes` reference) and the `var _ interface{ GetStoreID() string }` comment
- `pkg/metadata/store/memory/store.go`: updated ULID-construction comment + `GetStoreID` assertion comment
- `pkg/metadata/store/postgres/store.go`: updated `GetStoreID` assertion comment

## Verification

- `go build ./...` exits 0
- `go vet ./...` exits 0
- `go test -count=1 -short -race ./pkg/metadata/...` exits 0 (memory backend conformance suite still runs — backup subtests removed)
- `grep -rn "metadata\.Backupable\|metadata\.PayloadIDSet\|metadata\.ErrBackupUnsupported" . --include='*.go'` -> 0
- `grep -rn "RunBackupConformanceSuite" . --include='*.go'` -> 0
- `grep -rn "pkg/metadata/backup\b" pkg/ --include='*.go'` -> 0

## Deviations from Plan

### [Rule 3 - Blocking] Removed comment-only references to deleted storebackups package

- Plan did not list these comment lines, but the final grep check `grep -rn "storebackups" pkg/ --include='*.go'` would have matched them as dangling references to deleted code.
- Cleaned up three comments in `pkg/metadata/store/{badger,memory,postgres}/store.go` to zero out the match count.

## Self-Check: PASSED

- Commit `7308eb92` exists
- All 9 target files absent from working tree: confirmed
- Build/tests/greps green as above
