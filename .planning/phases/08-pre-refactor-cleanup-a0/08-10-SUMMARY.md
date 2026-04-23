---
phase: 08-pre-refactor-cleanup-a0
plan: 10
subsystem: backup
tags: [backup-removal, td-03, d-30]
status: complete
requires: [08-09]
provides: []
affects: [pkg/backup]
tech-stack:
  added: []
  removed: [pkg/backup package tree, Manifest/Scheduler/Destination/Executor/Restore subsystems, backup error taxonomy]
  patterns: []
key-files:
  created: []
  modified:
    - cmd/dfs/commands/start.go
    - internal/controlplane/api/handlers/problem.go
  deleted:
    - pkg/backup/backupable.go
    - pkg/backup/backupable_test.go
    - pkg/backup/clock.go
    - pkg/backup/concurrent_write_backup_restore_test.go
    - pkg/backup/destination/** (tree, 30+ files)
    - pkg/backup/errors/** (tree)
    - pkg/backup/executor/** (tree)
    - pkg/backup/manifest/** (tree)
    - pkg/backup/restore/** (tree)
    - pkg/backup/scheduler/** (tree)
    - internal/controlplane/api/handlers/problem_test.go
decisions:
  - "Collapsed into single atomic commit with 08-08a/08-08b/08-09 due to import cycle."
  - "internal/controlplane/api/handlers/problem.go lost its backup-specific Problem variants (classifier, BackupAlreadyRunningProblem, RestorePreconditionFailedProblem) + the test file — cleanup of residual 08-07 surface."
metrics:
  completed: 2026-04-23
---

# Phase 08 Plan 10: pkg/backup Full Tree Removal Summary

Deleted the entire `pkg/backup/` directory tree (manifest / scheduler / destination / executor / errors / restore subsystems plus top-level `backupable.go`, `clock.go`, and two integration tests). Part of the v0.13.0 backup deletion (D-30 step 7).

## One-liner

`pkg/backup/` package tree (~39 files across 6 subtrees) deleted as part of atomic PR-B collapse.

## Commit

- **SHA:** `7308eb92f4c63446d9de28acb5e669a188066d87`
- **Subject:** `refactor: remove v0.13.0 backup system (D-30 steps 4-7 combined, TD-03)`
- **Signed:** yes

This plan was collapsed into the atomic commit alongside 08-08a, 08-08b, and 08-09 — see 08-08a-SUMMARY.md for the cycle explanation.

## Deletions attributed to this plan

### `pkg/backup/` tree

- Top-level: `backupable.go`, `backupable_test.go`, `clock.go`, `concurrent_write_backup_restore_test.go`
- `destination/`: full subtree including `builtins/`, `destinationtest/`, `fs/`, `s3/` + envelope/hash/keyref/registry/errors + top-level `destination.go`
- `errors/`: full subtree (backup error taxonomy: CodeBackupAlreadyRunning, CodeRestorePreconditionFailed, CodeDestinationPermissionDenied, etc.)
- `executor/`: full subtree
- `manifest/`: full subtree
- `restore/`: full subtree
- `scheduler/`: full subtree

### Orphan-importer fixes (deviation — Rule 3, blocking)

Two call sites outside `pkg/backup/` still imported it after previous plans:

**`cmd/dfs/commands/start.go`:**

- Removed `"github.com/marmos91/dittofs/pkg/backup/destination/builtins"` import
- Removed the `builtins.RegisterBuiltins()` call in `runStart` (the boot hook that registered the fs and s3 backup destination drivers).

**`internal/controlplane/api/handlers/problem.go`:**

- Removed `bkperrors "github.com/marmos91/dittofs/pkg/backup/errors"` import
- Removed the backup-specific Problem variants:
  - `BackupAlreadyRunningProblem` struct
  - `RestorePreconditionFailedProblem` struct
  - `WriteBackupAlreadyRunningProblem` function
  - `WriteRestorePreconditionFailedProblem` function
  - `WriteBackupProblem` function
  - `statusForBackupCode` function
  - `WriteClassifiedBackupError` function
  - `writeProblemJSON` helper (was only used by the backup variants)
- Kept the generic RFC 7807 Problem surface (`Problem`, `WriteProblem`, `BadRequest`, `Unauthorized`, `Forbidden`, `NotFound`, `Conflict`, `UnprocessableEntity`, `InternalServerError`, `ServiceUnavailable`, `WriteJSON`, `WriteJSONOK`, `WriteJSONCreated`, `WriteNoContent`).

**`internal/controlplane/api/handlers/problem_test.go`:** deleted (only tested the backup-specific variants).

## Pre-check

Initial build failed on two orphan importers (listed above). These were residuals from plan 08-07 that had not been cleaned. Rule 3 (blocking-issue auto-fix) applied.

After fixes:

- `grep -rnE '"github\.com/marmos91/dittofs/pkg/backup"|"github\.com/marmos91/dittofs/pkg/backup/' . --include='*.go'` -> 0 matches

## Verification

- `go build ./...` exits 0
- `go vet ./...` exits 0
- `go test -count=1 -short -race ./pkg/... ./internal/... ./cmd/...` exits 0
- `test ! -d pkg/backup` passes
- `grep -rn "marmos91/dittofs/pkg/backup" . --include='*.go'` -> 0 matches

## Deviations from Plan

### [Rule 3 - Blocking] Orphan importers in cmd/dfs/commands/start.go and internal/controlplane/api/handlers/problem.go

These call sites still imported `pkg/backup` after plan 08-07's REST/CLI cleanup. `go build ./...` failed until they were unwired. Cleaned up as part of this atomic commit.

## Self-Check: PASSED

- Commit `7308eb92` exists in git log
- `pkg/backup/` absent: confirmed
- Build/tests/greps green
