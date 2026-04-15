---
phase: 01-foundations-models-manifest-capability-interface
plan: 02
subsystem: controlplane.store
tags: [backup, persistence, gorm, store-interface]
requires:
  - "models.BackupRepo/BackupRecord/BackupJob from Plan 01"
  - "generic GORM helpers from pkg/controlplane/store/helpers.go"
  - "github.com/oklog/ulid/v2 (already in go.mod, now direct)"
provides:
  - "BackupStore sub-interface (18 methods) embedded in composite Store"
  - "GORMStore implementation of BackupStore"
  - "Integration test suite validating CRUD, uniqueness, pin, filter, recovery, in-use"
affects:
  - "pkg/controlplane/store/interface.go"
  - "pkg/controlplane/store/backup.go (new)"
  - "pkg/controlplane/store/backup_test.go (new)"
  - "pkg/controlplane/models/backup.go (diagnostic fix)"
  - "go.mod (ulid v2 promoted from indirect to direct)"
tech-stack:
  added: []
  patterns:
    - "Sub-interface composition in Store"
    - "Generic GORM helpers (getByField, listAll, createWithID, deleteByField)"
    - "UUID for configuration IDs, ULID for time-sorted record/job IDs"
    - "Transactional delete with reference-count check"
key-files:
  created:
    - pkg/controlplane/store/backup.go
    - pkg/controlplane/store/backup_test.go
  modified:
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/models/backup.go
    - go.mod
decisions:
  - "ULID generated client-side before createWithID to avoid forcing helper to know entity ID type"
  - "omitzero (Go 1.25) for nested FK struct fields (MetadataStore, Repo) to properly suppress zero-valued relations in JSON; omitempty is a no-op for non-pointer structs"
  - "ListAllBackupRepos surfaced now to avoid interface churn when Phase 4 scheduler lands"
  - "UpdateBackupRepo maps unique-constraint errors to ErrDuplicateBackupRepo to preserve the (store_id, name) invariant on Save"
metrics:
  completed: "2026-04-15"
---

# Phase 1 Plan 02: BackupStore Sub-Interface & GORM Implementation Summary

BackupStore sub-interface (18 methods) added to the composite control plane Store with a GORM implementation on `*GORMStore`, closing REPO-01/02/04/05 at the persistence layer and pre-staging the SAFETY-02 recovery hook.

## Files

**Created:**
- `pkg/controlplane/store/backup.go` — GORMStore methods for BackupRepo/BackupRecord/BackupJob CRUD, `SetBackupRecordPinned`, `RecoverInterruptedJobs`.
- `pkg/controlplane/store/backup_test.go` — `//go:build integration` suite covering all behaviors locked by the plan.

**Modified:**
- `pkg/controlplane/store/interface.go` — `BackupStore` sub-interface declared after `BlockStoreConfigStore`; embedded in composite `Store` in the corresponding slot.
- `pkg/controlplane/models/backup.go` — `omitempty` → `omitzero` on `BackupRepo.MetadataStore` and `BackupRecord.Repo` (see Deviations).
- `go.mod` — `github.com/oklog/ulid/v2 v2.1.1` moved from `// indirect` to a direct dependency now that BackupStore imports it.

## Full Method Set Added to BackupStore (for Phase 6 API wiring)

```go
// Repo operations
GetBackupRepo(ctx, storeID, name) (*BackupRepo, error)
GetBackupRepoByID(ctx, id) (*BackupRepo, error)
ListBackupReposByStore(ctx, storeID) ([]*BackupRepo, error)
ListAllBackupRepos(ctx) ([]*BackupRepo, error)
CreateBackupRepo(ctx, *BackupRepo) (string, error)
UpdateBackupRepo(ctx, *BackupRepo) error
DeleteBackupRepo(ctx, id) error  // ErrBackupRepoInUse if records exist

// Record operations
GetBackupRecord(ctx, id) (*BackupRecord, error)
ListBackupRecordsByRepo(ctx, repoID) ([]*BackupRecord, error)  // newest-first
CreateBackupRecord(ctx, *BackupRecord) (string, error)
UpdateBackupRecord(ctx, *BackupRecord) error
DeleteBackupRecord(ctx, id) error
SetBackupRecordPinned(ctx, id, pinned) error  // REPO-03

// Job operations (single table, kind discriminator)
GetBackupJob(ctx, id) (*BackupJob, error)
ListBackupJobs(ctx, kind, status) ([]*BackupJob, error)  // "" skips that filter
CreateBackupJob(ctx, *BackupJob) (string, error)
UpdateBackupJob(ctx, *BackupJob) error
RecoverInterruptedJobs(ctx) (int, error)  // SAFETY-02 boot hook
```

## Composite Uniqueness (REPO-04) — Enforced at DB Level

The `BackupRepo` composite unique index `idx_backup_repo_store_name` on `(metadata_store_id, name)` is declared via GORM tags in `models/backup.go` and created by `AutoMigrate`. Verified by `TestBackupRepoUniquePerStore`:

- Same `name` under the **same** `metadata_store_id` → `ErrDuplicateBackupRepo`.
- Same `name` under a **different** `metadata_store_id` → success.

No manual migration code required; `gorm.go:247 db.AutoMigrate(models.AllModels()...)` picks up the new tables (`backup_repos`, `backup_records`, `backup_jobs`) on next startup for both SQLite and PostgreSQL.

## Tests (all passing under `-tags=integration`)

| Test | What it locks |
|------|---------------|
| `TestBackupRepoOperations` | Create / duplicate-per-store / get-by-id / get-by-(storeID,name) / list / list-all / update / delete / missing-delete |
| `TestBackupRepoUniquePerStore` | REPO-04 composite uniqueness — same name permitted across stores |
| `TestBackupRepoGetConfigRoundTrip` | Opaque JSON config round-trips through SetConfig → create → reload → GetConfig |
| `TestBackupRecordPin` | REPO-03 pin flag persists and toggles; missing record returns ErrBackupRecordNotFound |
| `TestBackupRecordListByRepo` | List scoped to repo with newest-first ordering |
| `TestBackupJobKindFilter` | `kind` and `status` filters compose; empty string skips the filter |
| `TestRecoverInterruptedJobs` | SAFETY-02: 3 running jobs → interrupted w/ error + finished_at; succeeded job untouched |
| `TestDeleteBackupRepoInUse` | `ErrBackupRepoInUse` when records reference repo; succeeds after records removed |
| `TestBackupRecordAutoULID` | Auto-generated ID is 26-char ULID and lexicographically monotonic |
| `TestBackupJobAutoULID` | Same invariant for jobs |

Full integration suite (`go test -tags=integration ./pkg/controlplane/store/... -count=1`) and unit suite (`go test ./pkg/controlplane/store/... -count=1`) both pass with no regressions.

## Deviations from Plan

1. **[Rule 3 — blocking] `omitempty` → `omitzero` on nested FK structs**
   - **Found during:** Task 2 verification (diagnostic pre-reported on `pkg/controlplane/models/backup.go:78,134`).
   - **Issue:** `omitempty` is a no-op for non-pointer struct fields; `json` output includes zero-valued `MetadataStoreConfig{}` / `BackupRepo{}` blobs when the relation is not preloaded.
   - **Fix:** Replaced with Go 1.25 `omitzero`, which suppresses zero-valued structs correctly. Applied only to the two diagnostics-flagged fields.
   - **Files modified:** `pkg/controlplane/models/backup.go` (lines 78, 134).
   - **Commit:** `38ad72ec`.
   - **Trade-off:** Other models in the codebase (e.g. `share.go:40-42`) still use `omitempty` on nested structs and carry the same latent issue. Out of scope for this plan — tracked as deferred (candidate cleanup for a future hygiene pass).

2. **[Refinement] Added `TestBackupJobAutoULID` in addition to `TestBackupRecordAutoULID`.** Plan only specified the record variant; the job variant costs ~10 LOC and locks the same invariant for the second ULID-bearing entity.

3. **[Refinement] `UpdateBackupRepo` maps unique-constraint violations to `ErrDuplicateBackupRepo`.** Plan left the Save error mapping implicit; explicit mapping preserves the composite-uniqueness invariant when a rename would collide.

4. **`go mod tidy` removed the `// indirect` annotation on `oklog/ulid/v2`.** Expected side-effect of BackupStore importing ulid directly; resolves the warning flagged for this plan.

## How Phase 5 Will Consume `RecoverInterruptedJobs`

Phase 5 (Restore & Safety) wires a single boot hook in `pkg/controlplane/runtime/lifecycle/service.go` during `Serve(ctx)`:

```go
// After store init, before any adapter starts accepting requests.
n, err := store.RecoverInterruptedJobs(ctx)
if err != nil {
    return fmt.Errorf("recover interrupted jobs: %w", err)
}
if n > 0 {
    logger.Warn("recovered interrupted backup/restore jobs", "count", n)
}
```

Because `RecoverInterruptedJobs` is a bulk `UPDATE ... WHERE status = 'running'` with no read-modify-write loop, the operation is atomic at the storage layer and safe to call exactly once on startup. No cross-instance coordination is needed (single-instance model per PROJECT.md).

## Self-Check: PASSED

- [x] `pkg/controlplane/store/interface.go` — `BackupStore` declared, embedded in `Store`
- [x] `pkg/controlplane/store/backup.go` — exists, compiles
- [x] `pkg/controlplane/store/backup_test.go` — exists, all tests pass
- [x] `go build ./pkg/controlplane/...` — passes
- [x] `go vet ./pkg/controlplane/store/...` — clean
- [x] `go test -tags=integration ./pkg/controlplane/store/... -count=1` — PASS (0.85s)
- [x] `go test ./pkg/controlplane/store/... -count=1` — PASS (0.33s)
- [x] Commits present: `88718e5f` (interface), `c8f54958` (impl + tests), `38ad72ec` (omitzero fix)
