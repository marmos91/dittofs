---
phase: 06-cli-rest-api-surface
plan: 02
subsystem: rest/apiclient/handlers/router
tags: [rest, api, apiclient, handlers, router, problem-details, d-13, d-23, d-27, d-28, d-29, d-31, d-42, d-43, d-45, d-46]
requires: [phase-6-plan-01]
provides:
  - handlers.BackupHandler + TriggerBackup + Restore + RestoreDryRun + ListRecords + ShowRecord + PatchRecord + ListJobs + GetJob + CancelJob + CreateRepo + ListRepos + GetRepo + PatchRepo + DeleteRepo
  - handlers.BackupService + BackupHandlerStore + BackupDestinationFactory interfaces
  - handlers.TriggerBackupResponse + RestoreDryRunResponse + BackupRecordResponse + BackupJobResponse + BackupRepoResponse
  - handlers.BackupAlreadyRunningProblem + RestorePreconditionFailedProblem + writer helpers
  - handlers.ShareHandler.Disable + handlers.ShareHandler.Enable + ShareResponse.Enabled
  - runtime.Runtime.DisableShare + EnableShare + StoreBackupsService accessors
  - Router wiring under singular /api/v1/store/metadata/{name}/{backups,backup-jobs,restore,repos} + /api/v1/shares/{name}/{disable,enable}
  - apiclient typed methods for every new endpoint (16 methods)
  - apiclient.BackupAlreadyRunningError + RestorePreconditionError typed errors
  - apiclient.TriggerBackupResponse + DryRunResult + BackupRecord + BackupJob + BackupRepo model types
affects: [phase-6-plans-03-06]
tech-stack:
  added: []
  patterns:
    - RFC 7807 typed problem variants via embedded Problem (running_job_id, enabled_shares flatten at top level)
    - Narrow BackupService interface lets handler tests use fakes without full runtime wiring
    - BackupDestinationDeleter + BackupDestinationFactory kept in handlers package so the router composes the production factory closure once
    - apiclient.doWithTypedProblem parses application/problem+json 409 bodies into *BackupAlreadyRunningError / *RestorePreconditionError via errors.As
    - apiclient storePath(name) helper enforces singular /api/v1/store/metadata/ convention across every new endpoint
key-files:
  created:
    - internal/controlplane/api/handlers/problem_test.go
    - internal/controlplane/api/handlers/backups.go
    - internal/controlplane/api/handlers/backups_test.go
    - internal/controlplane/api/handlers/backup_jobs.go
    - internal/controlplane/api/handlers/backup_jobs_test.go
    - internal/controlplane/api/handlers/backup_repos.go
    - internal/controlplane/api/handlers/backup_repos_test.go
    - internal/controlplane/api/handlers/shares_enabled_test.go
    - pkg/apiclient/backups.go
    - pkg/apiclient/backups_test.go
    - pkg/apiclient/backup_jobs.go
    - pkg/apiclient/backup_repos.go
    - pkg/apiclient/shares_disable_enable_test.go
  modified:
    - internal/controlplane/api/handlers/problem.go
    - internal/controlplane/api/handlers/shares.go
    - internal/controlplane/api/handlers/shares_test.go
    - pkg/controlplane/api/router.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/apiclient/shares.go
    - .planning/phases/06-cli-rest-api-surface/06-CONTEXT.md
    - .planning/REQUIREMENTS.md
decisions:
  - "BackupService interface lives in the handlers package (not in pkg/controlplane/runtime/storebackups) so handler tests can swap a fake without importing runtime internals; *storebackups.Service satisfies it implicitly."
  - "BackupDestinationFactory is a function type returning a narrow BackupDestinationDeleter interface (only Delete + Close). Keeps handler tests free of destination/registry imports while the router composes the production closure once."
  - "Router guards the Phase-6 wiring with `if rt != nil` so pkg/controlplane/api/server_test.TestAPIServer_Lifecycle (which passes a nil runtime) still starts without panicking."
  - "TriggerBackup resolves running_job_id via ListBackupJobsFiltered(status=running,limit=1) rather than a typed carrier — runtime already returns a wrapped sentinel, so a best-effort DB lookup keeps the contract loose."
  - "extractEnabledShares uses errors.As against an `EnabledShares() []string` interface so future typed errors (Plan 01 pipeline may add one later) plug in without changing handler code."
  - "Runtime.StoreBackupsService() exposes the concrete *storebackups.Service instead of widening Runtime with one forwarder per REST verb — the REST handler is tightly coupled to the sub-service API anyway; widening Runtime gets nothing in return."
metrics:
  duration: ~90min
  completed: 2026-04-17T12:30:00Z
---

# Phase 6 Plan 2: REST handlers + router + apiclient Summary

One-liner: Ships the complete REST surface (14 backup-related routes + 2
share routes) wired under the existing singular `/api/v1/store/metadata/`
admin-gated group, plus 16 typed apiclient methods and two 409 problem
variants (running_job_id + enabled_shares) that round-trip as typed errors
at the client layer.

## Scope

Protocol-surface layer for Phase 6. Zero business logic, zero schema
change. Handlers call Plan 01's storebackups.Service primitives. The CLI
(Plans 03-06) and dittofs-pro UI consume exactly the shapes declared here.

## Tasks Completed

| # | Task | Commit |
|---|------|--------|
| 0 | Docs housekeeping: singular /store/metadata/ convention in CONTEXT.md + REQUIREMENTS.md | d18d61e7 |
| 1 | Typed problem variants (BackupAlreadyRunningProblem, RestorePreconditionFailedProblem) + writer helpers | 0f021511 |
| 2 | BackupHandler + BackupJob/Repo handlers + ShareHandler Disable/Enable + ShareResponse.Enabled | 66fc9189 |
| 3 | Router wiring + apiclient typed methods + typed 409 error surfacing | 482337f9 |

## New HTTP Routes (all admin-gated via inherited RequireAdmin())

### Backup records

| Method | Path | Handler | Success |
|---|---|---|---|
| POST | `/api/v1/store/metadata/{name}/backups` | `BackupHandler.TriggerBackup` | **202 Accepted** → `{record, job}` |
| GET | `/api/v1/store/metadata/{name}/backups?repo={name}&status={status}` | `BackupHandler.ListRecords` | 200 OK |
| GET | `/api/v1/store/metadata/{name}/backups/{id}` | `BackupHandler.ShowRecord` | 200 OK |
| PATCH | `/api/v1/store/metadata/{name}/backups/{id}` | `BackupHandler.PatchRecord` | 200 OK |

### Backup jobs

| Method | Path | Handler | Success |
|---|---|---|---|
| GET | `/api/v1/store/metadata/{name}/backup-jobs?status={s}&kind={k}&repo={r}&limit={n}` | `BackupHandler.ListJobs` | 200 OK |
| GET | `/api/v1/store/metadata/{name}/backup-jobs/{id}` | `BackupHandler.GetJob` | 200 OK |
| POST | `/api/v1/store/metadata/{name}/backup-jobs/{id}/cancel` | `BackupHandler.CancelJob` | **200 OK** on terminal (D-45), **202 Accepted** on running |

### Restore

| Method | Path | Handler | Success |
|---|---|---|---|
| POST | `/api/v1/store/metadata/{name}/restore` | `BackupHandler.Restore` | **202 Accepted** → `BackupJob` |
| POST | `/api/v1/store/metadata/{name}/restore/dry-run` | `BackupHandler.RestoreDryRun` | 200 OK → `{record, manifest_valid, enabled_shares}` — skips shares-enabled gate (D-31) |

### Backup repo CRUD

| Method | Path | Handler | Success |
|---|---|---|---|
| POST | `/api/v1/store/metadata/{name}/repos` | `BackupHandler.CreateRepo` | 201 Created |
| GET | `/api/v1/store/metadata/{name}/repos` | `BackupHandler.ListRepos` | 200 OK |
| GET | `/api/v1/store/metadata/{name}/repos/{repo}` | `BackupHandler.GetRepo` | 200 OK |
| PATCH | `/api/v1/store/metadata/{name}/repos/{repo}` | `BackupHandler.PatchRepo` | 200 OK |
| DELETE | `/api/v1/store/metadata/{name}/repos/{repo}?purge_archives={bool}` | `BackupHandler.DeleteRepo` | **204 No Content** (default), **200 OK + problem body** (partial purge failure) |

### Share lifecycle

| Method | Path | Handler | Success |
|---|---|---|---|
| POST | `/api/v1/shares/{name}/disable` | `ShareHandler.Disable` | 200 OK → updated ShareResponse |
| POST | `/api/v1/shares/{name}/enable` | `ShareHandler.Enable` | 200 OK → updated ShareResponse |

## New apiclient Methods

```go
// pkg/apiclient/backups.go
func (c *Client) TriggerBackup(storeName string, req *TriggerBackupRequest) (*TriggerBackupResponse, error)
func (c *Client) ListBackupRecords(storeName, repo string) ([]BackupRecord, error)
func (c *Client) GetBackupRecord(storeName, recordID string) (*BackupRecord, error)
func (c *Client) SetBackupRecordPinned(storeName, recordID string, pinned bool) (*BackupRecord, error)
func (c *Client) StartRestore(storeName string, req *RestoreRequest) (*BackupJob, error)
func (c *Client) RestoreDryRun(storeName string, req *RestoreRequest) (*DryRunResult, error)

// pkg/apiclient/backup_jobs.go
func (c *Client) ListBackupJobs(storeName string, filter BackupJobFilter) ([]BackupJob, error)
func (c *Client) GetBackupJob(storeName, jobID string) (*BackupJob, error)
func (c *Client) CancelBackupJob(storeName, jobID string) (*BackupJob, error)

// pkg/apiclient/backup_repos.go
func (c *Client) CreateBackupRepo(storeName string, req *BackupRepoRequest) (*BackupRepo, error)
func (c *Client) ListBackupRepos(storeName string) ([]BackupRepo, error)
func (c *Client) GetBackupRepo(storeName, repoName string) (*BackupRepo, error)
func (c *Client) UpdateBackupRepo(storeName, repoName string, req *BackupRepoRequest) (*BackupRepo, error)
func (c *Client) DeleteBackupRepo(storeName, repoName string, purgeArchives bool) error

// pkg/apiclient/shares.go (extended)
func (c *Client) DisableShare(name string) (*Share, error)
func (c *Client) EnableShare(name string) (*Share, error)
```

16 new methods total. Plus typed errors surfaced via `doWithTypedProblem`
→ `errors.As`:

```go
type BackupAlreadyRunningError struct{ RunningJobID string }
type RestorePreconditionError  struct{ EnabledShares []string }
```

## Sentinel → HTTP mapping (as implemented)

| Sentinel | HTTP | Body shape |
|---|---|---|
| `storebackups.ErrBackupAlreadyRunning` | 409 | `BackupAlreadyRunningProblem` with `running_job_id` |
| `storebackups.ErrRestorePreconditionFailed` | 409 | `RestorePreconditionFailedProblem` with `enabled_shares` |
| `storebackups.ErrNoRestoreCandidate` | 409 | generic Conflict |
| `storebackups.ErrRecordNotRestorable` | 409 | generic Conflict |
| `storebackups.ErrStoreIDMismatch` / `ErrStoreKindMismatch` | 400 | BadRequest |
| `storebackups.ErrRecordRepoMismatch` | 400 | BadRequest |
| `storebackups.ErrManifestVersionUnsupported` | 400 | BadRequest |
| `storebackups.ErrInvalidTargetKind` | 400 | BadRequest |
| `storebackups.ErrScheduleInvalid` | 400 | BadRequest |
| `storebackups.ErrRepoNotFound` / `models.ErrBackupRepoNotFound` | 404 | NotFound |
| `models.ErrBackupRecordNotFound` | 404 | NotFound |
| `models.ErrBackupJobNotFound` (from DB miss pre-check in CancelJob) | 404 | NotFound |
| `storebackups.ErrBackupJobNotFound` (runtime registry miss, DB row present) | 200 | idempotent re-read per D-45 |
| `models.ErrShareNotFound` | 404 | NotFound |
| `models.ErrDuplicateBackupRepo` | 409 | generic Conflict |
| `models.ErrBackupRepoInUse` | 409 | generic Conflict |

Default: `InternalServerError(...)` with a generic message; raw error strings
not echoed (T-06-02-07 mitigation).

## Typed problem variants

| Type | Emitted by | HTTP | Extra field |
|---|---|---|---|
| `BackupAlreadyRunningProblem` | `writeBackupError` in `backups.go` | 409 | `running_job_id string` |
| `RestorePreconditionFailedProblem` | `writeRestoreError` in `backups.go` | 409 | `enabled_shares []string` |
| `BackupRepoPurgeProblem` | `DeleteRepo` on partial `?purge_archives=true` failure | 200 | `failed_record_ids []string` |

All embed the RFC 7807 base so clients can decode as `map[string]any` and
see `title`/`status`/`detail` flattened at the top level (tested by
`TestEmbeddedProblemBackCompat`).

## Singular path convention enforcement

`grep -rn '/stores/metadata/' pkg/controlplane/api/router.go pkg/apiclient/` returns 0 matches.
Verified by compile-time usage and by the apiclient `TestClient_TriggerBackup_PostsCorrectPath_Singular` test.

Docs aligned: `.planning/phases/06-cli-rest-api-surface/06-CONTEXT.md` and
`.planning/REQUIREMENTS.md` API-05 now use the singular prefix (Task 0).

## Test Outcomes

| Package | Count | Result |
|---|---|---|
| internal/controlplane/api/handlers (unit, no build tag) | 3 problem + 13 backups + 7 backup-jobs + 7 backup-repos + 2 shares_enabled_test | PASS |
| internal/controlplane/api/handlers (integration tag) | 3 new disable/enable/enabled-field + all existing | PASS |
| pkg/controlplane/api | existing lifecycle test | PASS |
| pkg/apiclient | 10 new backup/restore + 2 shares_disable_enable + all existing | PASS |
| go build ./... | — | clean |
| go vet ./... | — | clean |

New test functions:

- problem: `TestWriteBackupAlreadyRunningProblem`, `TestWriteRestorePreconditionFailedProblem`, `TestEmbeddedProblemBackCompat`
- backups: `TestTriggerBackup_SingleRepo_Returns202WithRecordAndJob`, `TestTriggerBackup_MultiRepo_RequiresRepoParam`, `TestTriggerBackup_AlreadyRunning_Returns409WithJobID`, `TestListRecords_FiltersByRepo`, `TestShowRecord_Returns404OnMiss`, `TestPatchRecord_Pinned_Flips`, `TestPatchRecord_BadBody_Returns400`, `TestRestore_PreconditionFailed_Returns409WithEnabledShares`, `TestRestore_InvalidULID_Returns400`, `TestRestore_Succeeds_Returns202AndJob`, `TestRestoreDryRun_ManifestValid_Returns200WithResult`, `TestRestoreDryRun_ManifestInvalid_Returns200WithInvalidFlag`, `TestRestoreDryRun_NoRestoreCandidate_Returns409`
- backup_jobs: `TestListJobs_FilterStatusKindLimit`, `TestListJobs_FilterByRepo`, `TestShowJob_Returns404OnMiss`, `TestCancelJob_Running_Returns202`, `TestCancelJob_Terminal_Idempotent`, `TestCancelJob_RegistryMiss_Returns200Idempotent`, `TestCancelJob_TrulyUnknown_Returns404`
- backup_repos: `TestCreateRepo_ValidPayload_Returns201`, `TestCreateRepo_InvalidSchedule_Returns400`, `TestListRepos_ForStore`, `TestGetRepo_Returns404OnMiss`, `TestPatchRepo_PartialUpdate`, `TestDeleteRepo_Default_RemovesRow`, `TestDeleteRepo_PurgeArchives_CascadesDestination`
- shares_enabled: `TestShareResponse_IncludesEnabled`, `TestShareResponse_EnabledTrue`
- shares_test.go (integration): `TestShareHandler_Disable_NotFound`, `TestShareHandler_Enable_NotFound`, `TestShareHandler_Get_IncludesEnabledField`
- apiclient: `TestClient_TriggerBackup_PostsCorrectPath_Singular`, `TestClient_ListBackupRecords_BuildsRepoQuery`, `TestClient_StartRestore_Returns202AndJob`, `TestClient_RestoreDryRun_Returns200AndResult`, `TestClient_BackupAlreadyRunning_SurfacesRunningJobID`, `TestClient_RestorePreconditionFailed_SurfacesEnabledShares`, `TestClient_GetSetPinnedRecord`, `TestClient_ListBackupJobs_BuildsFilterQuery`, `TestClient_CancelBackupJob_Terminal_Returns200`, `TestClient_RepoCRUD_Paths` (+ 6 sub-tests), `TestClient_DisableShare_ReturnsEnabledFalse`, `TestClient_EnableShare_ReturnsEnabledTrue`

## Deviations from Plan

### Rule 3 - Blocking issue

**1. [Rule 3] Router guarded with `if rt != nil` around Phase-6 wiring**

- **Found during:** Task 3 (router wiring added to existing test harness)
- **Issue:** `pkg/controlplane/api/server_test.TestAPIServer_Lifecycle` passes a nil `*runtime.Runtime` into `NewRouter`. Unconditional call to `rt.StoreBackupsService()` segfaults the test.
- **Fix:** Wrapped the new `/store/metadata/{name}/backups|backup-jobs|restore|repos` sub-routes in `if rt != nil { ... }`. The existing `/store/metadata` CRUD routes already tolerated nil-rt via their own guards.
- **Files modified:** pkg/controlplane/api/router.go
- **Commit:** 482337f9

### Rule 2 - Missing critical functionality

**2. [Rule 2] Runtime.DisableShare / EnableShare wrappers**

- **Found during:** Task 2 handler implementation
- **Issue:** Plan prescribed calling `h.runtime.ShareService().DisableShare(...)`. No `ShareService()` accessor existed on `*runtime.Runtime`; callers would otherwise have to reach into private `sharesSvc`.
- **Fix:** Added `(*Runtime).DisableShare(ctx, name)` and `(*Runtime).EnableShare(ctx, name)` wrappers that internally call `r.sharesSvc.DisableShare(ctx, r.store, name)`. Keeps the sub-service private + matches existing Runtime→sub-service delegation pattern.
- **Files modified:** pkg/controlplane/runtime/runtime.go
- **Commit:** 66fc9189

**3. [Rule 2] Runtime.StoreBackupsService() accessor**

- **Found during:** Task 3 router wiring
- **Issue:** The new handler receives a `BackupService` argument at construction; the router needs a concrete `*storebackups.Service` to pass in. Plan said "inspect pkg/controlplane/runtime/runtime.go first" — no such accessor existed.
- **Fix:** Added `(*Runtime).StoreBackupsService() *storebackups.Service`. Returns the private sub-service (nil-safe when Runtime was constructed with a nil store, matching other Phase-6 accessors like `BackupStore()` and `DestFactoryFn()`).
- **Files modified:** pkg/controlplane/runtime/runtime.go
- **Commit:** 482337f9

**4. [Rule 2] BackupDestinationDeleter interface for purge-archives path**

- **Found during:** Task 2 DeleteRepo `?purge_archives=true` implementation
- **Issue:** The handler package importing `pkg/backup/destination` solely for two methods (`Delete`, `Close`) would create a dependency fanout and make tests need the destination registry.
- **Fix:** Declared the narrow `BackupDestinationDeleter` interface (`Delete`, `Close`) + `BackupDestinationFactory` function type in the handlers package. Router composes the production closure over `destination.DestinationFactoryFromRepo` once. Tests inject a stub without importing the destination package.
- **Files modified:** internal/controlplane/api/handlers/backups.go, internal/controlplane/api/handlers/backup_repos.go, pkg/controlplane/api/router.go
- **Commit:** 66fc9189 + 482337f9

## Authentication Gates

None.

## Known Stubs

None. All data flows into responses are wired; the `BackupRepoResponse.Config`
map echoes the DB value as-is (T-06-02-08 notes an explicit accept trade-off
under admin-only gating — no redaction logic is added).

## Threat Flags

None new beyond the plan's `<threat_model>`:

- T-06-02-01 Admin gate — all new routes inherit `RequireAdmin()` via the existing `/store` (line 215) and `/shares` (line 187) groups.
- T-06-02-02 ULID validation — `validateOptionalULID` enforces 26-char length before the svc call.
- T-06-02-04 Limit validation — `parseLimit` rejects non-numeric/negative; store layer caps at 200 (D-42).
- T-06-02-05 Enum validation — `parseBackupStatus` / `parseBackupJobKind` reject unknown values with 400.
- T-06-02-09 Partial purge safety — DeleteRepo preserves the repo row on partial destination failures; tests assert `storeFake.deleteRepoCalledID == ""`.
- T-06-02-11 Idempotency — CancelJob returns 200 on terminal or registry-miss (tested).
- T-06-02-12 / T-06-02-13 — Dry-run shape is distinct from real restore (200 vs 202; no job row created; handler invokes `RunRestoreDryRun` not `RunRestore`).

## Self-Check: PASSED

Verification commands:

- `grep -n 'type BackupAlreadyRunningProblem struct' internal/controlplane/api/handlers/problem.go` → match
- `grep -n 'type RestorePreconditionFailedProblem struct' internal/controlplane/api/handlers/problem.go` → match
- `grep -n 'func NewBackupHandler' internal/controlplane/api/handlers/backups.go` → match
- `grep -c 'func (h \*BackupHandler) \(TriggerBackup\|Restore\|RestoreDryRun\)' internal/controlplane/api/handlers/backups.go` → 3
- `grep -c 'func (h \*BackupHandler) \(ListJobs\|GetJob\|CancelJob\)' internal/controlplane/api/handlers/backup_jobs.go` → 3
- `grep -c 'func (h \*BackupHandler) \(CreateRepo\|ListRepos\|GetRepo\|PatchRepo\|DeleteRepo\)' internal/controlplane/api/handlers/backup_repos.go` → 5
- `grep -c 'func (h \*ShareHandler) \(Disable\|Enable\)' internal/controlplane/api/handlers/shares.go` → 2
- `grep -c 'backupHandler\.' pkg/controlplane/api/router.go` → 14
- `grep -n 'shareHandler.Disable\|shareHandler.Enable' pkg/controlplane/api/router.go` → 2 matches
- `grep -c '/stores/metadata/' pkg/controlplane/api/router.go pkg/apiclient/` → 0 (singular regression guard)
- `grep -c 'func (c \*Client) \(TriggerBackup\|ListBackupRecords\|GetBackupRecord\|SetBackupRecordPinned\|StartRestore\|RestoreDryRun\)' pkg/apiclient/backups.go` → 6
- `grep -c 'func (c \*Client) \(ListBackupJobs\|GetBackupJob\|CancelBackupJob\)' pkg/apiclient/backup_jobs.go` → 3
- `grep -c 'func (c \*Client) \(CreateBackupRepo\|ListBackupRepos\|GetBackupRepo\|UpdateBackupRepo\|DeleteBackupRepo\)' pkg/apiclient/backup_repos.go` → 5
- `grep -c 'func (c \*Client) \(DisableShare\|EnableShare\)' pkg/apiclient/shares.go` → 2
- `grep -n 'type BackupAlreadyRunningError struct\|type RestorePreconditionError struct' pkg/apiclient/backups.go` → 2 matches
- `go build ./...` → clean
- `go vet ./...` → clean
- `go test ./internal/controlplane/api/handlers/... ./pkg/controlplane/api/... ./pkg/apiclient/... -count=1` → all PASS
- `go test -tags=integration ./internal/controlplane/api/handlers/... -count=1` → all PASS
- All 4 commits (d18d61e7, 0f021511, 66fc9189, 482337f9) present in `git log --oneline`.
