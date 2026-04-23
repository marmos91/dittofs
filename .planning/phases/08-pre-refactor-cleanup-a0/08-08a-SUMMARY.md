---
phase: 08-pre-refactor-cleanup-a0
plan: 08a
subsystem: controlplane/store+models
tags: [backup-removal, td-03, d-30]
status: complete
requires: [08-08]
provides: []
affects: [pkg/controlplane/store, pkg/controlplane/models]
tech-stack:
  added: []
  removed: [gorm-backed BackupStore interface, BackupRepo/BackupRecord/BackupJob GORM models]
  patterns: []
key-files:
  created: []
  modified:
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/models/models.go
    - pkg/controlplane/store/gorm.go
  deleted:
    - pkg/controlplane/store/backup.go
    - pkg/controlplane/store/backup_test.go
    - pkg/controlplane/models/backup.go
    - pkg/controlplane/models/backup_test.go
decisions:
  - "Plans 08-08a, 08-08b, 08-09, 08-10 collapsed into ONE atomic commit to preserve the compile-green invariant (D-11 granularity traded)."
metrics:
  completed: 2026-04-23
---

# Phase 08 Plan 08a: BackupStore Persistence + GORM Models Removal Summary

Removed the GORM-backed `BackupStore` persistence layer and the `BackupRepo` / `BackupRecord` / `BackupJob` GORM model registrations from the control-plane store and models packages. Part of the v0.13.0 backup system deletion (D-30 step 4).

## One-liner

Store-layer backup surface (interface + persistence + models) deleted as part of atomic PR-B collapse.

## Commit

- **SHA:** `7308eb92f4c63446d9de28acb5e669a188066d87`
- **Subject:** `refactor: remove v0.13.0 backup system (D-30 steps 4-7 combined, TD-03)`
- **Signed:** yes (Good signature, RSA SHA256:ADuGa4QCr9JgRW9b88cSh1vU3+heaIMjMPmznghPWT8)

This plan was collapsed into the atomic commit alongside 08-08b, 08-09, and 08-10 because the four plans form a compile-time dependency cycle:

- `pkg/backup/*` imports `pkg/controlplane/models.Backup*` (this plan's targets)
- `pkg/metadata/backup.go` shim aliases `pkg/backup` types (08-08b target)
- `pkg/metadata/store/{badger,memory,postgres}/backup.go` implement the shim
- `pkg/controlplane/runtime/storebackups/*` imports `store.BackupStore` (this plan's target) + `models.Backup*` (this plan's target) + `pkg/backup` (08-10 target)

No ordering of 08-08a/08-08b/08-09/08-10 keeps `go build ./...` green between commits. They were collapsed into one deletion with the user's acceptance of the granularity tradeoff.

## Pre-audit greps (confirmed empty before deletion)

Each of these returned zero non-backup-tree matches:

- `grep -rn "store\.BackupStore\|store\.ErrInvalidProgress\|store\.BackupJobFilter\|UpdateBackupRepo\|..." . --include='*.go'` (excluding files being deleted)
- `grep -rn "models\.BackupRepo\|models\.BackupRecord\|models\.BackupJob" . --include='*.go'` (excluding files being deleted)

All external references existed only inside directories also being deleted in the same commit (`pkg/backup/`, `pkg/controlplane/runtime/storebackups/`).

## Deletions attributed to this plan

### Files deleted

- `pkg/controlplane/store/backup.go`
- `pkg/controlplane/store/backup_test.go`
- `pkg/controlplane/models/backup.go`
- `pkg/controlplane/models/backup_test.go`

### Surgical edits

**`pkg/controlplane/store/interface.go`:**

- Removed `ErrInvalidProgress` sentinel
- Removed `BackupJobFilter` struct
- Removed entire `BackupStore interface` block (~140 lines, ~28 methods)
- Removed `BackupStore` embedding from the `Store` composite interface
- Removed now-unused `errors` import (kept `time` because `UpdateLastLogin` still uses it)

**`pkg/controlplane/models/models.go`:**

- Removed `&BackupRepo{}`, `&BackupRecord{}`, `&BackupJob{}` from `AllModels()` (3 lines)

**`pkg/controlplane/store/gorm.go` (deviation — Rule 3, blocking):**

- Removed the pre-migration block that renames `backup_repos.metadata_store_id -> target_id` (lines ~253-262 of original, no longer meaningful without the table)
- Removed the post-migration `UPDATE backup_repos SET target_kind` backfill (lines ~263-268 of original)
- Updated comment on the sibling `shares.enabled` backfill (no longer references `backup_repos.target_kind` as its pattern exemplar)

**`pkg/controlplane/models/errors.go` (deviation — Rule 2, dead critical surface):**

- Removed backup sentinels: `ErrBackupRepoNotFound`, `ErrDuplicateBackupRepo`, `ErrBackupRepoInUse`, `ErrBackupRecordNotFound`, `ErrBackupRecordPinned`, `ErrDuplicateBackupRecord`, `ErrBackupJobNotFound`, `ErrDuplicateBackupJob`
- Removed scheduler sentinels: `ErrScheduleInvalid`, `ErrRepoNotFound`, `ErrBackupAlreadyRunning`, `ErrInvalidTargetKind`
- All sentinels were dead (no references anywhere in the codebase after deletions).

## Verification

- `go build ./...` exits 0
- `go vet ./...` exits 0
- `go test -count=1 -short -race ./pkg/controlplane/... ./pkg/metadata/... ./pkg/blockstore/...` exits 0
- `grep -rn "BackupStore\b" pkg/controlplane/store/ pkg/controlplane/runtime/` -> 0 matches
- `grep -rn "BackupRepo\b\|BackupRecord\b\|BackupJob\b" pkg/controlplane/` -> 0 matches
- `grep -rn "store\.BackupStore\|store\.ErrInvalidProgress\|store\.BackupJobFilter" .` -> 0 matches
- `grep -rn "models\.BackupRepo\|models\.BackupRecord\|models\.BackupJob" .` -> 0 matches

## Deviations from Plan

### Deviations (Rule 2/3 — critical cleanup to complete the deletion)

1. **[Rule 3 - Blocking] Removed `pkg/controlplane/store/gorm.go` backup pre-migration + post-migration backfill**
   - Found during: `go build ./...` before commit (referenced `models.BackupRepo{}` which no longer exists).
   - Fix: removed the two blocks.

2. **[Rule 2 - Dead critical surface] Removed backup error sentinels in `pkg/controlplane/models/errors.go`**
   - Found during: final grep sweep (`BackupRepo|BackupRecord|BackupJob` in `pkg/controlplane/`).
   - All were unreferenced after the deletion chain; leaving them would have left a misleading API surface.

## Self-Check: PASSED

- Commit `7308eb92` exists in git log
- `pkg/controlplane/store/backup.go` absent: FOUND (absent)
- `pkg/controlplane/models/backup.go` absent: FOUND (absent)
- All builds/tests/greps as listed above
