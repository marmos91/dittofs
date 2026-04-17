---
phase: 06-cli-rest-api-surface
plan: 01
subsystem: backup/runtime/apiclient
tags: [backup, runtime, cancel, progress, apiclient, d-28, d-42, d-43, d-45, d-50]
requires: [phase-5]
provides:
  - BackupStore.ListBackupRecords
  - BackupStore.ListBackupJobsFiltered
  - BackupStore.UpdateBackupRecordPinned
  - BackupStore.UpdateBackupJobProgress
  - storebackups.Service.CancelBackupJob
  - storebackups.Service.RunRestoreDryRun
  - storebackups.DryRunResult
  - storebackups.ErrBackupJobNotFound
  - executor.RunBackupOption + WithOnJobCreated
  - restore.RunRestoreOption + WithOnJobCreated
  - apiclient.Share.Enabled
affects: [phase-6-plans-02-06]
tech-stack:
  added: []
  patterns:
    - per-call option pattern (RunBackupOption / RunRestoreOption) for executor hooks
    - mutex-guarded run-ctx registry keyed by BackupJob.ID
    - best-effort progress updates (WARN-on-error, never fail parent)
key-files:
  created:
    - pkg/apiclient/shares_test.go
    - pkg/controlplane/models/share_test.go
  modified:
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/store/backup.go
    - pkg/controlplane/store/backup_test.go
    - pkg/controlplane/runtime/storebackups/errors.go
    - pkg/controlplane/runtime/storebackups/service.go
    - pkg/controlplane/runtime/storebackups/restore.go
    - pkg/controlplane/runtime/storebackups/service_test.go
    - pkg/controlplane/runtime/storebackups/restore_test.go
    - pkg/controlplane/runtime/storebackups/metrics_test.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/backup/executor/executor.go
    - pkg/backup/executor/executor_test.go
    - pkg/backup/restore/restore.go
    - pkg/backup/restore/restore_test.go
    - pkg/apiclient/shares.go
decisions:
  - "Per-call WithOnJobCreated option on executor.RunBackup / restore.Executor.RunRestore replaces the initially-planned executor-field callback — serializes naturally without a global mutex and supports concurrent per-repo runs"
  - "ErrBackupJobNotFound in storebackups package has distinct identity from models.ErrBackupJobNotFound: the runtime sentinel signals 'no active run-ctx', the model sentinel signals 'DB row missing'"
  - "UpdateBackupRecordPinned implemented as an alias of SetBackupRecordPinned (zero new SQL) so Phase 6 REST verbs can use the PATCH-friendly name without duplicating the WHERE clause"
  - "ListBackupJobsFiltered uses `ORDER BY started_at IS NULL, started_at DESC` — SQLite- and Postgres-safe NULLS LAST without dialect branching"
  - "Runtime.RunBackup wrapper updated to return (*rec, *job, error) — the Phase-6 REST handler bypass layer would otherwise have to reach into storeBackupsSvc directly"
metrics:
  duration: ~35min
  completed: 2026-04-17T11:37:00Z
---

# Phase 6 Plan 1: Foundations (BackupStore + Runtime + Executors + apiclient) Summary

One-liner: BackupStore gains 4 Phase-6 methods (list/filter/pinned/progress),
storebackups.Service gains CancelBackupJob + RunRestoreDryRun + the
(*BackupRecord, *BackupJob, error) return shape on RunBackup/RunRestore,
backup + restore executors emit D-50 progress markers, and apiclient.Share
surfaces the Enabled field for Phase-6 CLI/UI rendering.

## Scope

Foundation layer for Phase 6. Four additive tasks over Phases 1-5 runtime
without any schema migration or new business logic. Plan 02+ (REST handlers,
CLI commands) can now declare endpoints against stable store, runtime, and
apiclient contracts.

## Tasks Completed

| # | Task | Commit |
|---|------|--------|
| 1 | Extend BackupStore with ListBackupRecords, ListBackupJobsFiltered, UpdateBackupRecordPinned, UpdateBackupJobProgress | 1af15531 |
| 2 | Cancel primitive + BackupJob return shape on Service.RunBackup / Service.RunRestore + RunRestoreDryRun | 89551288 |
| 3 | D-50 progress milestones in backup + restore executors | 8f0974d9 |
| 4 | apiclient.Share.Enabled JSON field + regression tests | 0b14ff7e |

## 4 New BackupStore Methods

```go
ListBackupRecords(ctx context.Context, repoID string, statusFilter models.BackupStatus) ([]*models.BackupRecord, error)
ListBackupJobsFiltered(ctx context.Context, filter BackupJobFilter) ([]*models.BackupJob, error)
UpdateBackupRecordPinned(ctx context.Context, recordID string, pinned bool) error
UpdateBackupJobProgress(ctx context.Context, jobID string, pct int) error
```

Plus supporting types:

```go
// pkg/controlplane/store/interface.go
type BackupJobFilter struct {
    RepoID string
    Status models.BackupStatus
    Kind   models.BackupJobKind
    Limit  int // 0 = default 50, cap 200 (D-42)
}

var ErrInvalidProgress = errors.New("progress must be between 0 and 100")
```

## Post-Edit Runtime Signatures

```go
// pkg/controlplane/runtime/storebackups/service.go
func (s *Service) RunBackup(ctx context.Context, repoID string) (rec *models.BackupRecord, job *models.BackupJob, err error)
func (s *Service) CancelBackupJob(ctx context.Context, jobID string) error

// pkg/controlplane/runtime/storebackups/restore.go
func (s *Service) RunRestore(ctx context.Context, repoID string, recordID *string) (job *models.BackupJob, err error)
func (s *Service) RunRestoreDryRun(ctx context.Context, repoID string, recordID *string) (*DryRunResult, error)

type DryRunResult struct {
    Record        *models.BackupRecord
    ManifestValid bool
    EnabledShares []string
}

// pkg/controlplane/runtime/storebackups/errors.go
var ErrBackupJobNotFound = errors.New("backup job not found or already terminal")

// pkg/controlplane/runtime/runtime.go — wrapper updated to match
func (r *Runtime) RunBackup(ctx context.Context, repoID string) (*models.BackupRecord, *models.BackupJob, error)
```

## Post-Edit Executor Signatures

```go
// pkg/backup/executor/executor.go
func (e *Executor) RunBackup(
    ctx context.Context,
    source backup.Backupable,
    dst destination.Destination,
    repo *models.BackupRepo,
    storeID string,
    storeKind string,
    opts ...RunBackupOption,
) (*models.BackupRecord, *models.BackupJob, error)

type RunBackupOption func(*runBackupConfig)
func WithOnJobCreated(fn func(*models.BackupJob)) RunBackupOption

// pkg/backup/restore/restore.go
func (e *Executor) RunRestore(
    ctx context.Context,
    p Params,
    opts ...RunRestoreOption,
) (job *models.BackupJob, err error)

type RunRestoreOption func(*runRestoreConfig)
func WithOnJobCreated(fn func(*models.BackupJob)) RunRestoreOption
```

The backup executor JobStore interface (and the restore executor JobStore interface) both
gained `UpdateBackupJobProgress(ctx, jobID, pct) error`.

## D-50 Progress Marker Insertion Points (post-edit)

### pkg/backup/executor/executor.go

| Stage | Inserted at line | Pct |
|---|---|---|
| After successful CreateBackupJob (+ onJobCreated hook) | ~152 | 0 |
| After manifest skeleton built, before io.Pipe | ~185 | 10 |
| Immediately before dst.PutBackup | ~210 | 50 |
| After CreateBackupRecord, before finalize UpdateBackupJob | ~318 | 95 |
| (existing) finalize UpdateBackupJob | — | 100 |

### pkg/backup/restore/restore.go

| Stage | Inserted at line | Pct |
|---|---|---|
| After CreateBackupJob (+ onJobCreated hook) | ~202 | 0 |
| After manifest fetched + validated | ~272 | 10 |
| After OpenFreshEngineAtTemp returned | ~282 | 30 |
| After payload streamed + reader.Close SHA verified | ~311 | 60 |
| After successful SwapMetadataStore | ~321 | 95 |
| (existing) terminal-state defer finalize | — | 100 |

## apiclient.Share.Enabled

```go
// pkg/apiclient/shares.go (lines 22-27)
type Share struct {
    // ...
    ReadOnly bool `json:"read_only,omitempty"`
    Enabled  bool `json:"enabled"` // D-28: no omitempty (false is semantically meaningful)
    // ...
}
```

`pkg/controlplane/models/share.go` Share.Enabled already had the matching
`json:"enabled"` tag (landed in Phase 5 Plan 01). This plan adds regression
tests in both packages locking the tag and the no-omitempty invariant.

## Phase 4/5 Call Sites Updated for New Return Shapes

| Site | Changes |
|---|---|
| pkg/backup/executor/executor_test.go | 13 RunBackup call sites → 3-return shape |
| pkg/backup/restore/restore_test.go | 10 RunRestore call sites → (job, err) shape |
| pkg/controlplane/runtime/storebackups/service_test.go | 8 RunBackup call sites → 3-return shape |
| pkg/controlplane/runtime/storebackups/restore_test.go | 8 RunRestore call sites → (job, err) shape |
| pkg/controlplane/runtime/storebackups/metrics_test.go | 3 call sites (2 RunRestore, 1 RunBackup) |
| pkg/controlplane/runtime/runtime.go | 1 Runtime.RunBackup wrapper — propagates the 3-return shape |
| pkg/controlplane/runtime/storebackups/service.go | runScheduledBackup wrapper — discards the extra return |

## Test Outcomes

| Package | Count | Result |
|---|---|---|
| pkg/controlplane/store (-tags=integration) | 5 new + all existing | PASS |
| pkg/backup/executor | 2 new D-50 + all existing | PASS |
| pkg/backup/restore | 2 new D-50 + all existing | PASS |
| pkg/controlplane/runtime/storebackups | 8 new (3 RunBackup/RunRestore shape + 3 Cancel + 4 DryRun -1 overlap) + all existing | PASS |
| pkg/apiclient | 1 new (regression) | PASS |
| pkg/controlplane/models | 1 new (regression) | PASS |
| full suite | all packages | PASS |

`go vet ./...` clean. `go build ./...` clean.

New test functions added:

- store: `TestListBackupRecords_FilterByStatus`, `TestListBackupRecords_EmptyRepo`, `TestListBackupJobsFiltered_Filter`, `TestUpdateBackupRecordPinned`, `TestUpdateBackupJobProgress`
- storebackups (service): `TestRunBackup_ReturnsBackupJob`, `TestRunBackup_FailurePath_StillReturnsJob`, `TestCancelBackupJob_InFlight_CancelsRunCtx`, `TestCancelBackupJob_UnknownID_ReturnsErrBackupJobNotFound`, `TestCancelBackupJob_Terminal_IdempotentNoOp`
- storebackups (restore): `TestRunRestore_ReturnsBackupJob`, `TestRunRestoreDryRun_ValidatesManifest_SkipsSharesGate`, `TestRunRestoreDryRun_InvalidManifest_Fails`, `TestRunRestoreDryRun_NoRestoreCandidate_Fails`, `TestRunRestoreDryRun_ManifestInvalid_NonFatal`
- executor: `TestRunBackup_RecordsProgress_0_10_50_95_100`, `TestRunBackup_ProgressUpdateError_DoesNotFailBackup`
- restore: `TestRunRestore_RecordsProgress_0_10_30_60_95_100`, `TestRunRestore_ProgressUpdateError_DoesNotFailRestore`
- apiclient: `TestShare_JSON_IncludesEnabled`
- models: `TestShare_JSON_IncludesEnabled`

## Deviations from Plan

### Rule 3 - Blocking issue

**1. [Rule 3] Refactored onJobCreated from executor struct field to per-call option**

- **Found during:** Task 2 initial implementation
- **Issue:** The plan proposed an executor `onJobCreated` struct field set via `SetOnJobCreated` before each RunBackup call. However, the backup + restore executors are shared across concurrent per-repo runs; a mutex around "set hook → invoke RunBackup → clear hook" would serialize all backups globally and defeat the per-repo overlap guard.
- **Fix:** Replaced the struct field with a variadic `RunBackupOption` / `RunRestoreOption` pattern. The callback lives in a per-call `runBackupConfig` / `runRestoreConfig` struct populated from opts, so concurrent calls cannot race on shared state.
- **Files modified:** pkg/backup/executor/executor.go, pkg/backup/restore/restore.go, pkg/controlplane/runtime/storebackups/service.go, pkg/controlplane/runtime/storebackups/restore.go
- **Commit:** 89551288

**2. [Rule 3] Runtime.RunBackup wrapper updated to match new 3-return shape**

- **Found during:** Task 2 build failure
- **Issue:** `pkg/controlplane/runtime/runtime.go:435 Runtime.RunBackup` wrapper was returning `(*BackupRecord, error)` and calling the new `storeBackupsSvc.RunBackup` which now returns `(*BackupRecord, *BackupJob, error)`. Without the update, the build would fail for downstream callers.
- **Fix:** Updated the wrapper to also return `(*BackupRecord, *BackupJob, error)` so Phase 6 handlers can surface job.ID without reaching into private composition state.
- **Files modified:** pkg/controlplane/runtime/runtime.go
- **Commit:** 89551288

### Rule 2 - Missing critical functionality

**3. [Rule 2] metrics_test.go call sites updated for new return shapes**

- **Found during:** Task 2 build verification
- **Issue:** The plan enumerated service_test.go and restore_test.go call sites but did not list `metrics_test.go:86, 137, 160` which also call `RunRestore` / `RunBackup`. Without updates the package would fail to compile.
- **Fix:** Updated all 3 call sites in metrics_test.go to match the new signatures.
- **Files modified:** pkg/controlplane/runtime/storebackups/metrics_test.go
- **Commit:** 89551288

## Authentication Gates

None.

## Known Stubs

None.

## Threat Flags

None — plan introduces no new network surface, no new auth paths, no new
trust boundaries. Every new method is covered by the Phase 5 admin-only
REST gate (Phase 6 Plan 02 wires routes).

## Self-Check: PASSED

Verification (all via Bash):

- `grep -n 'ListBackupRecords(ctx context.Context, repoID string' pkg/controlplane/store/interface.go` → one match in BackupStore interface
- `grep -n 'UpdateBackupJobProgress(ctx context.Context, jobID string, pct int)' pkg/controlplane/store/interface.go` → one match
- `grep -n 'func (s \*GORMStore) ListBackupRecords' pkg/controlplane/store/backup.go` → one match
- `grep -n 'func (s \*GORMStore) UpdateBackupJobProgress' pkg/controlplane/store/backup.go` → one match
- `grep -n 'ErrInvalidProgress' pkg/controlplane/store/interface.go pkg/controlplane/store/backup.go` → definition + usage
- `grep -n 'func (s \*Service) CancelBackupJob' pkg/controlplane/runtime/storebackups/service.go` → one match
- `grep -n 'func (s \*Service) RunRestoreDryRun' pkg/controlplane/runtime/storebackups/restore.go` → one match
- `grep -n 'ErrBackupJobNotFound' pkg/controlplane/runtime/storebackups/errors.go` → one match (definition)
- `grep -n 'runCtxRegistry' pkg/controlplane/runtime/storebackups/service.go` → 5 matches (struct field + 2 helpers + CancelBackupJob lookup)
- `grep -c 'UpdateBackupJobProgress(' pkg/backup/executor/executor.go` → 5 (4 marker calls via updateProgress closure + 1 interface decl)
- `grep -c 'UpdateBackupJobProgress(' pkg/backup/restore/restore.go` → 6 (5 marker calls + 1 interface decl)
- `grep -n 'Enabled\s*bool\s*.json:\"enabled\"' pkg/apiclient/shares.go` → one match
- `grep -n 'Enabled\s*bool' pkg/controlplane/models/share.go` → pre-existing (verified in read_first)
- All 4 commits (1af15531, 89551288, 8f0974d9, 0b14ff7e) present in `git log --oneline`
- `go build ./...` clean
- `go vet ./...` clean
- `go test ./...` — all packages PASS
- `go test -tags=integration ./pkg/controlplane/store/...` — all packages PASS
