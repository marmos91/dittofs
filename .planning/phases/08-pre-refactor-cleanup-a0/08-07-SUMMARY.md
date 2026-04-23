---
phase: 08-pre-refactor-cleanup-a0
plan: 07
subsystem: controlplane-api
tags: [cleanup, backup-removal, api-surface, cli]
requirements: [TD-03]
dependency_graph:
  requires:
    - 08-06 (PR-B commit 1: e2e backup tests removed)
  provides:
    - api-surface-clean (no v0.13.0 backup REST routes registered)
    - cli-surface-clean (no backup/restore subcommands on dfs or dfsctl)
    - apiclient-clean (no backup-related client methods)
  affects:
    - 08-08 (runtime storebackups wiring drop — now safe because pkg router no longer calls rt.StoreBackupsService())
tech_stack:
  added: []
  patterns: []
key_files:
  created:
    - pkg/apiclient/stubserver_test.go
  modified:
    - cmd/dfs/commands/root.go
    - cmd/dfsctl/commands/store/metadata/metadata.go
    - cmd/dfsctl/commands/store/metadata/metadata_test.go
    - pkg/controlplane/api/router.go
  deleted:
    - internal/controlplane/api/handlers/backup_jobs.go (+test)
    - internal/controlplane/api/handlers/backup_repos.go (+test)
    - internal/controlplane/api/handlers/backups.go (+test)
    - cmd/dfsctl/commands/store/metadata/backup/ (33 files: backup/format/list/pin/poll/run/show/unpin + job/ + repo/ + restore/ subtrees)
    - cmd/dfs/commands/backup/ (backup.go, controlplane.go)
    - cmd/dfs/commands/restore/ (controlplane.go, restore.go)
    - pkg/apiclient/backups.go (+test)
    - pkg/apiclient/backup_jobs.go
    - pkg/apiclient/backup_repos.go
decisions:
  - "Extracted shared test helper (newStubServer/stubServer/newTestClient) from deleted pkg/apiclient/backups_test.go into a dedicated pkg/apiclient/stubserver_test.go file (Rule 3 blocker resolution — shares_disable_enable_test.go depended on it)."
  - "Rewrote cmd/dfsctl/commands/store/metadata/metadata_test.go to drop two backup-subtree tests and keep TestMetadataCmd_RegistersExistingVerbs (still validates list/add/edit/remove/health verbs)."
  - "Trimmed backup-related examples from metadata.go Cmd.Long docstring."
  - "Files_modified frontmatter listed internal/controlplane/api/router.go but no such file exists — all backup HTTP wiring lives in pkg/controlplane/api/router.go. No action required on the nonexistent file."
metrics:
  commits: 1
  files_changed: 52
  insertions: 63
  deletions: 9305
  duration_minutes: ~15
  completed: 2026-04-23T17:49Z
---

# Phase 08 Plan 07: PR-B commit 2 — Delete v0.13.0 backup REST + CLI + apiclient Summary

One-liner: Deleted the full v0.13.0 backup REST handler layer (`internal/controlplane/api/handlers/backup*.go`), both `dfs` and `dfsctl` backup/restore subcommand trees, all apiclient backup files, and the backup route block + `pkg/backup/destination` import from `pkg/controlplane/api/router.go`.

## Commit

- `c416cceb` — `api: remove v0.13.0 backup handlers, routers, dfs+dfsctl subcommands, apiclient (TD-03)` (signed; 52 files changed, 63 +, 9305 -).

## What Was Deleted

### Internal REST handlers (6 files, `internal/controlplane/api/handlers/`)
- `backup_jobs.go` + `backup_jobs_test.go`
- `backup_repos.go` + `backup_repos_test.go`
- `backups.go` + `backups_test.go`

### `pkg/apiclient/` backup surface (4 files)
- `backups.go` (+ `backups_test.go`)
- `backup_jobs.go`
- `backup_repos.go`

### `cmd/dfsctl/commands/store/metadata/backup/` subtree (33 files)
Entire subtree: `backup.go`, `format.go`, `list.go` (+test), `pin.go` (+test), `poll.go` (+test), `run.go` (+test), `show.go`, `unpin.go` plus three nested subtrees (`job/`, `repo/`, `restore/`).

### `cmd/dfs/` subcommand subtrees (4 files)
- `cmd/dfs/commands/backup/backup.go` + `controlplane.go`
- `cmd/dfs/commands/restore/controlplane.go` + `restore.go`

Deleted together because `restore/controlplane.go:14` imports `cmd/dfs/commands/backup` (threat T-08-07-04 mitigation).

## Router + CLI Wiring Unwired

### `pkg/controlplane/api/router.go` — 2 edits
- **Import removed:** `"github.com/marmos91/dittofs/pkg/backup/destination"` (previously line 17).
- **Backup route block removed** (previously lines 248-288): `backupHandler` construction (`var svc handlers.BackupService` + `rt.StoreBackupsService()` gate + `destFactory` + `NewBackupHandler` call) plus the four `r.Route("/{name}/...")` subgroups registering all 14 `backupHandler.*` calls (`TriggerBackup`, `ListRecords`, `ShowRecord`, `PatchRecord`, `ListJobs`, `GetJob`, `CancelJob`, `Restore`, `RestoreDryRun`, `CreateRepo`, `ListRepos`, `GetRepo`, `PatchRepo`, `DeleteRepo`).
- Also dropped the now-unused `"context"` and `"pkg/controlplane/models"` imports (they were only referenced inside the deleted `destFactory` closure).

### `cmd/dfsctl/commands/store/metadata/metadata.go`
- Removed imports: `cmd/dfsctl/commands/store/metadata/backup` and `.../backup/restore`.
- Removed `Cmd.AddCommand(backup.Cmd)` and `backup.Cmd.AddCommand(restore.Cmd)` from `init()`.
- Trimmed the four backup example blocks from the `Cmd.Long` docstring.

### `cmd/dfsctl/commands/store/metadata/metadata_test.go`
- Dropped `backup` package import.
- Removed `TestMetadataCmd_RegistersBackupSubtree` and `TestBackupSubtree_ExposesPhase6Verbs`.
- Kept `TestMetadataCmd_RegistersExistingVerbs` (retains list/add/edit/remove/health verb coverage).

### `cmd/dfs/commands/root.go`
- Removed imports: `cmd/dfs/commands/backup`, `cmd/dfs/commands/restore`.
- Removed `rootCmd.AddCommand(backup.Cmd)` and `rootCmd.AddCommand(restore.Cmd)` from `init()`.

## `internal/controlplane/api/router.go` — Nonexistent

The PLAN frontmatter `files_modified` lists this path, but no such file exists in the repo (confirmed via `find internal/controlplane/api -maxdepth 2 -name router.go`). All backup HTTP wiring lives in `pkg/controlplane/api/router.go` only; only that router needed editing. The stray frontmatter entry is a no-op — no phantom work created.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 — Blocking Issue] Extracted shared test helper after file deletion**
- **Found during:** Step 4 (verify) — `go vet ./...` reported `pkg/apiclient/shares_disable_enable_test.go:12:7: undefined: newStubServer`.
- **Root cause:** The `newStubServer`, `stubServer`, `stubCall`, `newTestClient` test helpers were defined in the top of the (legitimately deleted) `pkg/apiclient/backups_test.go` but also consumed by the unrelated `shares_disable_enable_test.go`. Co-location of shared helpers with subject-specific tests created a hidden dependency.
- **Fix:** Created a new dedicated test helper file `pkg/apiclient/stubserver_test.go` containing only the helper types/functions (no backup-related code). `shares_disable_enable_test.go` continues to compile and pass unmodified.
- **Commit:** `c416cceb` (same commit as the deletions — preserves the atomicity property).

No other deviations.

## Verification Results

- `go build ./...` — exit 0.
- `go vet ./...` — exit 0.
- `go test -count=1 -short -race ./internal/controlplane/api/... ./pkg/apiclient/... ./pkg/controlplane/api/... ./cmd/dfsctl/... ./cmd/dfs/...` — all pass.
- All 7 grep acceptance checks return 0 matches.
- All file/dir absence checks pass.
- `git log -1 --show-signature` — Good signature.
- Commit message contains no "claude code" or "co-authored-by" strings.

## Known Stubs

None.

## Threat Model Mitigations Applied

| Threat ID | Mitigation |
|-----------|-----------|
| T-08-07-01 (I) | Source files deleted from repo — cannot be linked into binary. |
| T-08-07-02 (S) | Grep across both `internal/controlplane/api/` and `pkg/controlplane/api/` returns 0 matches for `handlers.Backup*` / `backupHandler.` / `pkg/backup/destination`. |
| T-08-07-04 (T) | Deleted `cmd/dfs/commands/backup/` and `cmd/dfs/commands/restore/` together in a single `git rm -r` — no intermediate compile state produced. |

## Self-Check: PASSED

- Commit `c416cceb` exists in git log (verified via `git rev-parse --short HEAD`).
- SUMMARY.md file written at `.planning/phases/08-pre-refactor-cleanup-a0/08-07-SUMMARY.md`.
- No STATE.md / ROADMAP.md modifications made (per parallel-execution constraint).
