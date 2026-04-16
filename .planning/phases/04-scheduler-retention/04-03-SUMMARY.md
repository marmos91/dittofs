---
phase: 04-scheduler-retention
plan: 03
subsystem: backup
tags: [backup, executor, pipeline, tdd, ulid, io.Pipe]

# Dependency graph
requires:
  - phase: 04-01-framework-move
    provides: "pkg/backup/backupable.go (Backupable, PayloadIDSet, ErrBackupAborted), pkg/backup/destination (Destination interface), pkg/backup/manifest (Manifest, CurrentVersion, Encryption)"
  - phase: 01-foundations
    provides: "models.BackupRepo/Record/Job, GORMStore.CreateBackupJob/UpdateBackupJob/CreateBackupRecord"
  - phase: 03-destination-drivers-encryption
    provides: "Destination.PutBackup contract (fills SHA256 + SizeBytes on manifest, manifest-last publish)"
provides:
  - "pkg/backup/executor package with store-agnostic RunBackup pipeline"
  - "Executor.RunBackup(ctx, source, dst, repo, storeID, storeKind) → (*BackupRecord, error)"
  - "Narrow JobStore interface (3 methods: CreateBackupJob, UpdateBackupJob, CreateBackupRecord)"
  - "Injectable Clock interface for deterministic tests"
  - "D-21 sequence enforcement: ULID allocated before any job/manifest/destination call"
  - "D-16 invariant: no BackupRecord row on failure (source, destination, or ctx)"
  - "D-18 invariant: ctx.Canceled / DeadlineExceeded / ErrBackupAborted → BackupJob.Status=interrupted"
  - "D-20 invariant: BackupJob.ID and BackupRecord.ID are distinct ULIDs"
  - "Sorted PayloadIDSet output (deterministic manifests)"
affects: [04-04-retention, 04-05-storebackups-service, 06-api-on-demand-backup]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Narrow-interface-over-composite-store (JobStore vs full store.Store)"
    - "Injectable Clock for testability (mirrors lifecycle/service.go conventions)"
    - "io.Pipe producer/consumer wiring for Backupable → Destination streaming"
    - "ULID-before-side-effects to guarantee single source of truth across manifest/DB/archive"
    - "Sorted set-to-slice helper for deterministic YAML output"

key-files:
  created:
    - "pkg/backup/executor/doc.go (package doc describing D-21 sequence)"
    - "pkg/backup/executor/executor.go (RunBackup pipeline + JobStore + Clock)"
    - "pkg/backup/executor/executor_test.go (11 tests covering T1-T10 + nil guards)"
  modified: []

key-decisions:
  - "Skipped errors.go per plan revision note — ctx wrapping is inline; package surface is doc.go + executor.go only"
  - "Error aggregation priority: source beats destination beats ctx (source errors often cause destination errors, so surface the root cause)"
  - "PayloadIDSet sorted alphabetically via sort.Strings — deterministic manifest YAML for reviewers/diffs"
  - "Manifest.PayloadIDSet is stamped AFTER PutBackup returns (fakeDest captures pointer so it observes post-stamp value; acceptable per plan T8 note)"
  - "Record persist failure after archive publish is treated as 'failed' with explicit error message — Phase 5 orphan sweep will NOT clean up because manifest.yaml is present"

patterns-established:
  - "Package-private test doubles (fakeSource, fakeDest, fakeStore, fixedClock) live in *_test.go, never exposed to consumers"
  - "Test-driven development: RED commit for failing tests, GREEN commit for implementation (plan-level TDD gate)"
  - "1 MiB random-bytes pipe plumbing test to catch buffering bugs that unit-level fixtures miss"

requirements-completed: []

# Metrics
duration: 12min
completed: 2026-04-16
---

# Phase 04 Plan 03: Backup Executor Pipeline Summary

**Store-agnostic Executor.RunBackup implementing D-21 sequence (ULID-first, io.Pipe-driven Backupable→Destination, atomic BackupJob/BackupRecord persistence) with 11 unit tests.**

## Performance

- **Duration:** ~12 min
- **Started:** 2026-04-16 (session resume — worktree parallel wave 2)
- **Completed:** 2026-04-16
- **Tasks:** 1 (TDD: 1 RED + 1 GREEN commit)
- **Files created:** 3

## Accomplishments

- `pkg/backup/executor/` package ready for Plan 05 consumption — `storebackups.Service.RunBackup` will call this from both cron ticks and Phase 6's on-demand API (D-23)
- D-21 sequence enforced: ULID allocated at line 90 BEFORE `CreateBackupJob` (line 102) and BEFORE `PutBackup` (line 157)
- D-16 invariant tested: T4 (destination failure) and T5 (source failure) both assert no BackupRecord was created
- D-18 invariant tested: T5 (ErrBackupAborted) and T6 (ctx.Canceled) both assert final job status is `interrupted`, not `failed`
- D-20 invariant tested: T2 asserts BackupJob.ID and BackupRecord.ID are distinct ULIDs
- 1 MiB random-bytes payload test (T7) catches any future io.Pipe buffering regressions

## Task Commits

1. **Task 1 RED — failing executor tests** — `c6b225c0` (test)
2. **Task 1 GREEN — executor implementation** — `3d240897` (feat)

## Files Created

- `pkg/backup/executor/doc.go` — package doc narrating the D-21 sequence
- `pkg/backup/executor/executor.go` — `Executor.RunBackup` pipeline (~240 lines, heavily commented), `JobStore` narrow interface (3 methods), `Clock` injection point
- `pkg/backup/executor/executor_test.go` — 11 tests (T1 happy path, T2 ULID identity, T3 job lifecycle, T4 destination failure, T5 source failure, T6 ctx cancellation, T7 pipe plumbing 1 MiB, T8 manifest fields, T9 encryption enabled, T10 encryption disabled, + nil guards)

## Signatures (for Plan 05 consumers)

```go
// Narrow interface — Plan 05 passes store.BackupStore, which satisfies it transitively.
type JobStore interface {
    CreateBackupJob(ctx context.Context, job *models.BackupJob) (string, error)
    UpdateBackupJob(ctx context.Context, job *models.BackupJob) error
    CreateBackupRecord(ctx context.Context, rec *models.BackupRecord) (string, error)
}

// Clock is injectable; Plan 05 wires realClock{}, tests inject fixedClock.
type Clock interface { Now() time.Time }

func New(store JobStore, clock Clock) *Executor
func (e *Executor) RunBackup(
    ctx context.Context,
    source backup.Backupable,
    dst destination.Destination,
    repo *models.BackupRepo,
    storeID string,
    storeKind string,
) (*models.BackupRecord, error)
```

Plan 05 resolves `source` from `runtime/stores.Service.Get(repo.TargetID)`, resolves `dst` via `destination.Lookup(repo.Kind)`, and passes `repo.TargetID` + `repo.TargetKind` as `storeID` + `storeKind`.

## Decisions Made

- **No errors.go:** Plan revision note explicitly deferred `errors.go` because ctx normalization is inline via `errors.Is(runErr, context.Canceled || context.DeadlineExceeded || backup.ErrBackupAborted)`. The package surface is just `doc.go` + `executor.go` + `executor_test.go`.
- **Error priority source > destination > ctx:** when source errors (e.g., engine panic), the destination reader observes a closed pipe with that error — we surface the source root cause instead of the downstream destination read error.
- **PayloadIDSet stamped after PutBackup returns:** the destination writes manifest.yaml LAST (per D-21), so the PayloadIDSet is available by then. In tests, `fakeDest` captures the manifest by pointer and observes the post-stamp value — T8 asserts `{p1, p2}` sorted.
- **Deterministic PayloadIDSet ordering:** `sort.Strings(out)` makes the manifest's `payload_id_set` reproducible across runs with identical sources. Not strictly required for correctness but cleaner for diffs and Phase 5 block-GC consumers.

## Deviations from Plan

None — plan executed exactly as written, honoring the revision note to skip `errors.go`.

## Issues Encountered

None.

## Threat Flags

Scanned `pkg/backup/executor/executor.go` for security-relevant surface not in the plan's threat model (T-04-03-01 through T-04-03-06). No new endpoints, auth paths, file access patterns, or schema changes introduced. All threats enumerated in the plan remain applicable and mitigated as planned:

- T-04-03-01 (partial BackupRecord after destination failure) — enforced by T4
- T-04-03-02 (orphan running jobs) — deferred to Plan 05 (SAFETY-02 recovery hook)
- T-04-03-03 (KeyRef never stored as key material) — passthrough from repo row, test T9 asserts `"env:TESTKEY"` flows through
- T-04-03-04 (goroutine leak on source hang) — `<-srcDone` after PutBackup returns; ctx cancellation unblocks both sides
- T-04-03-05 (wrong StoreID cross-store restore) — storeID argument snapshotted into both manifest and BackupRecord
- T-04-03-06 (archive-published-but-record-persist-failed) — mitigated with explicit error message in UpdateBackupJob

## Next Phase Readiness

Plan 04 (retention) and Plan 05 (storebackups.Service) can now import `github.com/marmos91/dittofs/pkg/backup/executor` and call `New(store, nil).RunBackup(...)` without further dependencies. The Clock injection point makes deterministic scheduler tests possible.

No blockers. Plan 05 will:

1. Construct the Executor once during `storebackups.Service.New`.
2. Resolve `source` from `runtime/stores.Service` and `dst` from `destination.Lookup`.
3. Acquire per-repo overlap mutex, then call `executor.RunBackup`, then run retention inline (D-08).

## Self-Check: PASSED

- `pkg/backup/executor/doc.go` — exists
- `pkg/backup/executor/executor.go` — exists
- `pkg/backup/executor/executor_test.go` — exists
- Commit `c6b225c0` — FOUND (test: failing tests)
- Commit `3d240897` — FOUND (feat: implementation)
- `go build ./pkg/backup/executor/...` — exit 0
- `go test -race -timeout 60s ./pkg/backup/executor/...` — exit 0, 11/11 PASS
- `go vet ./pkg/backup/executor/...` — exit 0
- `go build ./...` — exit 0 (whole repo)

## TDD Gate Compliance

- RED commit: `c6b225c0 test(04-03): add failing tests for backup executor pipeline`
- GREEN commit: `3d240897 feat(04-03): implement store-agnostic backup executor pipeline`
- Both gates present; fail-fast rule honored (initial test run reported `undefined: New` build failures, confirming tests would fail without implementation).

---
*Phase: 04-scheduler-retention*
*Completed: 2026-04-16*
