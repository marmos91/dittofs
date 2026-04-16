---
phase: 04-scheduler-retention
verified: 2026-04-16T17:34:19Z
status: passed
score: 11/11 must-haves verified
overrides_applied: 0
requirements_covered:
  SCHED-01: satisfied
  SCHED-02: satisfied
  SCHED-03: satisfied
  SCHED-04: satisfied
  SCHED-05: satisfied
  SCHED-06: satisfied
---

# Phase 4: Scheduler + Retention Verification Report

**Phase Goal:** Scheduled backups run reliably per-repo without overlap, thundering herd, or silent pruner-induced data loss.
**Verified:** 2026-04-16T17:34:19Z
**Status:** passed
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths (from ROADMAP Success Criteria)

| #   | Truth                                                                                                                                                               | Status     | Evidence |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- | -------- |
| 1   | Operator can set a `CRON_TZ=`-prefixed cron schedule per repo; the in-process scheduler fires at the configured UTC-aware times                                      | VERIFIED   | `pkg/backup/scheduler/schedule.go:26` ValidateSchedule wraps robfig/cron/v3 ParseStandard (supports `CRON_TZ=` prefix). Test subcases `cron_tz_rome`, `cron_tz_new_york`, `cron_tz_utc`, `unknown_timezone` all pass in `TestValidateSchedule`. Scheduler registers via `scheduler.Scheduler.Register` (scheduler.go:128), consumes `robfig/cron/v3` internally. Serve wires repos via `Service.Serve` → `sched.Register` (service.go:201). |
| 2   | Two scheduler ticks on the same repo never produce overlapping runs (per-repo mutex); concurrent cron fires land with randomized jitter                              | VERIFIED   | `OverlapGuard.TryLock` at `pkg/backup/scheduler/overlap.go:33` uses `sync.Map[repoID]*sync.Mutex`. Scheduler `fire()` acquires guard at scheduler.go:267; on contention logs WARN and returns. `PhaseOffset` (jitter.go:22) uses FNV-1a over repo ID with `DefaultMaxJitter = 5min`. Tests `TestOverlapGuard_Concurrent100` (100 parallel goroutines → exactly 1 winner), `TestScheduler_OverlapUnderLoad`, `TestService_RunBackup_MutexContention` (second caller gets `ErrBackupAlreadyRunning`), `TestService_ScheduledAndOnDemandShareMutex` all pass. |
| 3   | Count-based retention keeps the last N successful backups per repo; age-based retention keeps backups newer than N days                                              | VERIFIED   | `RunRetention` at `pkg/controlplane/runtime/storebackups/retention.go:92` implements D-09 UNION policy: `keptByCount = (i >= len(candidates)-keepCount)` and `keptByAge = CreatedAt ≥ now-KeepAgeDays`. Tests T2 (count_only_deletes_oldest), T3 (age_only_deletes_old), T4 (union_age_keeps_all), T5 (union_both_active) all pass. |
| 4   | Retention never deletes the only successful backup, and never races with an in-flight upload (runs as a separate pass after upload confirms)                         | VERIFIED   | Safety rail at retention.go:179 `if postPruneSucceeded < DefaultMinKeepSucceeded`; test T7 (safety_rail_keeps_only_old_record) asserts 1-succeeded + KeepAgeDays=7 → `SkippedSafety=1`, `Deleted=[]`. No-race: `Service.RunBackup` acquires `overlap.TryLock` at service.go:295 before executor and holds through retention call at service.go:338; deferred unlock releases only after retention returns. Test T9 (destination_first_delete_order) confirms dst.Delete fires before DB DeleteBackupRecord. Test `TestService_RunBackup_SequencePutBeforeDelete` asserts PutBackup precedes Destination.Delete from retention under the same mutex. |
| 5   | Retention correctly skips pinned records                                                                                                                             | VERIFIED   | `ListSucceededRecordsForRetention` in `pkg/controlplane/store/backup.go:152` filters `pinned = false`. Retention T6 (pinned_skip) asserts 3 pinned + 5 non-pinned-deleted = 8 kept. T12 (pinned_provides_safety_floor) asserts pinned records count toward safety floor so non-pinned records CAN be deleted when a pinned record already provides the floor. |

**Score:** 5/5 ROADMAP success criteria verified

### Additional PLAN Must-Haves (Merged from Frontmatter)

| #   | Truth                                                                                                                                                     | Status   | Evidence |
| --- | --------------------------------------------------------------------------------------------------------------------------------------------------------- | -------- | -------- |
| 6   | Tree compiles cleanly after Backupable moved to pkg/backup with compat shim in pkg/metadata (D-27)                                                         | VERIFIED | `go build ./...` exits 0. `pkg/backup/backupable.go` line 26 `type Backupable interface`. `pkg/metadata/backup.go` lines 18,20 `type Backupable = backup.Backupable`, `type PayloadIDSet = backup.PayloadIDSet`. Sentinel identity preserved via `var X = backup.X` (lines 31–39). |
| 7   | D-26 polymorphic target schema — BackupRepo has TargetID + TargetKind, no MetadataStoreID                                                                 | VERIFIED | `pkg/controlplane/models/backup.go:69-70` has `TargetID string` + `TargetKind string` with `default:'metadata';index`. Zero matches for `MetadataStoreID` in model. `pkg/controlplane/store/gorm.go:258-260` pre-AutoMigrate RenameColumn guarded by HasColumn; line 275 post-AutoMigrate UPDATE backfill. `go test -tags=integration ./pkg/controlplane/store/...` passes. |
| 8   | BackupStore exposes ListReposByTarget + ListSucceededRecordsForRetention + PruneBackupJobsOlderThan                                                       | VERIFIED | `pkg/controlplane/store/backup.go:41` ListReposByTarget, `:152` ListSucceededRecordsForRetention, `:291` PruneBackupJobsOlderThan. Interface declarations in `pkg/controlplane/store/interface.go:368,401,447`. Zero references to old `ListBackupReposByStore` in production code (only historical docs). |
| 9   | Executor follows D-21 sequence: ulid.Make → CreateBackupJob → PutBackup → CreateBackupRecord → finalize                                                    | VERIFIED | `pkg/backup/executor/executor.go:90` recordID = ulid.Make().String(); `:94` jobID = ulid.Make().String(); `:102` CreateBackupJob; `:157` dst.PutBackup; `:225` CreateBackupRecord. Test `TestRunBackup_ULIDIdentity` asserts recordID == manifest.BackupID == *updatedJob.BackupRecordID (single ULID across 3 places). Test `TestRunBackup_ContextCancelled` asserts status=interrupted on ctx cancel (D-18). Test `TestRunBackup_DestinationFailure` asserts NO BackupRecord on failure (D-16). |
| 10  | storebackups.Service is the 9th sub-service with SAFETY-02 boot recovery, explicit hot-reload API, and unified RunBackup entry                             | VERIFIED | `pkg/controlplane/runtime/storebackups/service.go` contains `type Service struct`, `New`, `SetRuntime`, `Serve`, `Stop`, `RegisterRepo`, `UnregisterRepo`, `UpdateRepo`, `RunBackup`, `ValidateSchedule`. Line 183 `s.store.RecoverInterruptedJobs(ctx)` during serve (D-19 / SAFETY-02). `runtime.go:66` storeBackupsSvc field, `:113-115` compose via NewDefaultResolver + storebackups.New + SetRuntime. `runtime.go:361,367` Serve + Stop integration. 5 delegation methods on Runtime (lines 380,390,399,409,419). Tests `TestService_ServeRecoversInterruptedJobs`, `TestService_Serve*`, `TestService_RegisterRepo`, `TestService_UnregisterRepo`, `TestService_UpdateRepo`, `TestService_RunBackup_*`, `TestService_Stop_CancelsInFlight` all pass. |
| 11  | Target resolution via StoreResolver rejects invalid (target_kind, target_id) pairs                                                                         | VERIFIED | `target.go:99-116` DefaultResolver.Resolve: kind≠"metadata" → wrap ErrInvalidTargetKind; missing config → wrap ErrRepoNotFound; store not registered → wrap ErrRepoNotFound; non-Backupable → wrap backup.ErrBackupUnsupported. Tests `TestDefaultResolver_UnknownKind`, `TestDefaultResolver_ConfigMissing`, `TestDefaultResolver_StoreNotRegistered` all pass. |

**Score:** 11/11 total must-haves verified

### Required Artifacts

| Artifact                                                     | Expected                                                                | Status    | Details |
| ------------------------------------------------------------ | ----------------------------------------------------------------------- | --------- | ------- |
| `pkg/backup/backupable.go`                                   | Canonical Backupable interface + PayloadIDSet + 5 sentinels (D-27)      | VERIFIED  | 93 lines, package `backup`, all 5 sentinel vars present |
| `pkg/metadata/backup.go`                                     | Compat shim with type aliases + var re-exports                          | VERIFIED  | 41 lines, type aliases (18,20) + var re-exports (28–39) |
| `pkg/controlplane/models/backup.go`                          | BackupRepo with TargetID+TargetKind, no MetadataStoreID                 | VERIFIED  | Lines 69–70 new fields; 0 matches for MetadataStoreID |
| `pkg/controlplane/store/backup.go`                           | ListReposByTarget + ListSucceededRecordsForRetention + PruneBackupJobs  | VERIFIED  | Lines 41, 152, 291 — all 3 methods present |
| `pkg/backup/scheduler/` package                              | Scheduler + OverlapGuard + PhaseOffset + ValidateSchedule               | VERIFIED  | 4 source files + 4 test files; 22 test cases pass |
| `pkg/backup/executor/executor.go`                            | Executor.RunBackup with D-21 sequence                                   | VERIFIED  | ULID at line 90 precedes CreateBackupJob (102), PutBackup (157), CreateBackupRecord (225); 11 tests pass |
| `pkg/controlplane/runtime/storebackups/retention.go`         | RunRetention + PruneOldJobs implementing D-08..D-17                     | VERIFIED  | 263 lines; destination-first order (line 206 before 216); 13 tests pass |
| `pkg/controlplane/runtime/storebackups/service.go`           | Service with Serve/Stop/Register/Unregister/Update/RunBackup/Validate   | VERIFIED  | 399 lines; all 9 methods present |
| `pkg/controlplane/runtime/runtime.go`                        | storeBackupsSvc composition + Serve/Stop + 5 delegation methods         | VERIFIED  | Line 66 field; lines 113–115 compose; 361/367 serve/stop; 380/390/399/409/419 delegations |

### Key Link Verification

| From                                              | To                                                        | Via                                                                 | Status  | Details |
| ------------------------------------------------- | --------------------------------------------------------- | ------------------------------------------------------------------- | ------- | ------- |
| Phase 2 engine stores (memory/badger/postgres)    | `pkg/backup/backupable.go` via metadata shim              | Compat shim preserves metadata.Backupable identity                  | WIRED   | metadata/backup.go:18 `type Backupable = backup.Backupable` |
| `pkg/controlplane/store/backup.go`                | `models.BackupRepo.TargetID/TargetKind`                   | GORM `Where("target_kind = ? AND target_id = ?")`                   | WIRED   | backup.go:45 `Where("target_kind = ? AND target_id = ?", kind, targetID)` |
| `pkg/controlplane/store/gorm.go`                  | Schema migration                                          | Migrator().RenameColumn + Exec UPDATE backfill                      | WIRED   | gorm.go:258-260, 273-277 |
| `pkg/backup/scheduler/scheduler.go`               | `github.com/robfig/cron/v3`                               | `cron.New` + AddFunc                                                | WIRED   | Import line 8, `cron.New()` at scheduler.go:107 |
| `pkg/backup/scheduler/overlap.go`                 | sync.Map + sync.Mutex.TryLock                             | LoadOrStore + TryLock                                               | WIRED   | overlap.go:34-37 |
| `pkg/backup/scheduler/jitter.go`                  | hash/fnv stdlib                                           | fnv.New64a over repoID                                              | WIRED   | jitter.go:25-26 |
| `pkg/backup/executor/executor.go`                 | Backupable.Backup → io.Pipe → Destination.PutBackup       | source.Backup(ctx, pw) and dst.PutBackup(ctx, m, pr)                 | WIRED   | Lines 143 (source.Backup) and 157 (dst.PutBackup) |
| `pkg/controlplane/runtime/storebackups/retention.go` | Destination.Delete before store.DeleteBackupRecord    | D-14 destination-first ordering                                     | WIRED   | retention.go:206 dst.Delete before :216 DeleteBackupRecord |
| `pkg/controlplane/runtime/storebackups/service.go` | scheduler + executor + retention pipeline                | TryLock → Resolve → destFactory → exec.RunBackup → RunRetention     | WIRED   | service.go:295 TryLock, 316 Resolve, 321 destFactory, 331 exec.RunBackup, 338 RunRetention — all under held mutex (defer unlock at 299) |
| `pkg/controlplane/runtime/runtime.go`             | storebackups.Service                                      | Runtime.New constructs + 5 delegation methods + Serve/Stop          | WIRED   | runtime.go:113-115 construction, 361-371 Serve+Stop, 380-423 delegations |

### Requirements Coverage

| Requirement | Source Plan              | Description                                                                | Status    | Evidence |
| ----------- | ------------------------ | -------------------------------------------------------------------------- | --------- | -------- |
| SCHED-01    | 04-02, 04-05             | In-process cron scheduler based on robfig/cron/v3 with `CRON_TZ=` support  | SATISFIED | `pkg/backup/scheduler/` package + `storebackups.Service.Serve` registers via `scheduler.Register`. `TestValidateSchedule` subcases `cron_tz_*` pass. `pkg/backup/scheduler/schedule.go:27` calls `cron.ParseStandard`. |
| SCHED-02    | 04-02, 04-05             | Scheduler prevents overlapping runs (per-repo mutex); adds startup jitter   | SATISFIED | `OverlapGuard.TryLock` (overlap.go:33) + `PhaseOffset` FNV-1a (jitter.go:22). `TestOverlapGuard_Concurrent100`, `TestPhaseOffset_*`, `TestScheduler_OverlapUnderLoad` all pass. `TestService_RunBackup_MutexContention` confirms 2 concurrent callers → 1 winner + 1 ErrBackupAlreadyRunning. |
| SCHED-03    | 04-04                    | Count-based retention — keep last N successful backups per repo             | SATISFIED | `RunRetention` at retention.go:163 implements keep-last-N logic. Test T2 (count_only_deletes_oldest) passes. |
| SCHED-04    | 04-04                    | Age-based retention — keep backups ≤ N days per repo                        | SATISFIED | `RunRetention` at retention.go:166 implements age-cutoff logic. Tests T3 (age_only_deletes_old), T5 (union_both_active) pass. |
| SCHED-05    | 04-04                    | Retention never deletes the only successful backup (safety rail)            | SATISFIED | retention.go:179 `if postPruneSucceeded < DefaultMinKeepSucceeded`. Test T7 (safety_rail_keeps_only_old_record) asserts `SkippedSafety=1` and `Deleted=[]` for a single-old-record repo. |
| SCHED-06    | 04-04, 04-05             | Retention runs as separate pass after upload; no race with in-flight upload | SATISFIED | `Service.RunBackup` (service.go:294-356) acquires overlap.TryLock at 295, defers unlock at 299, calls exec.RunBackup at 331, then RunRetention at 338 — both under the same held mutex. Test `TestService_RunBackup_SequencePutBeforeDelete` asserts PutBackup ordering precedes retention's Destination.Delete. |

All 6 SCHED requirements are satisfied and covered by at least one plan's `requirements` frontmatter.

### Anti-Patterns Found

No blocker anti-patterns detected. Plans include deliberate placeholders for deferred work (observability hooks, SAFETY-01 block-GC, restore orchestration) explicitly scoped to Phase 5.

### Behavioral Spot-Checks

| Behavior                                                       | Command                                                                                                       | Result        | Status |
| -------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- | ------------- | ------ |
| Full build compiles                                            | `go build ./...`                                                                                              | EXIT=0        | PASS   |
| Race-free scheduler/executor/retention/service tests           | `go test -race -timeout 120s ./pkg/backup/... ./pkg/controlplane/runtime/storebackups/...`                    | all 10 packages pass | PASS   |
| D-26 migration integration tests                               | `go test -tags=integration -timeout 120s ./pkg/controlplane/store/...`                                        | EXIT=0        | PASS   |
| Models + metadata (shim identity) tests                        | `go test -race ./pkg/controlplane/store/... ./pkg/controlplane/models/... ./pkg/metadata/...`                 | EXIT=0        | PASS   |
| CRON_TZ support                                                | `go test -v -run TestValidateSchedule ./pkg/backup/scheduler/`                                                | 13 subcases pass | PASS   |
| Retention invariants (T1-T14)                                  | `go test -v -run TestRunRetention ./pkg/controlplane/runtime/storebackups/`                                   | T1-T12 pass + TestRunRetention_PrunesJobs pass | PASS   |
| Executor D-21 sequence, D-18 ctx cancel, D-16 no-record-on-fail| `go test -v ./pkg/backup/executor/`                                                                           | 11 test functions pass | PASS   |

### Gaps Summary

No gaps found. All 11 must-haves (5 roadmap truths + 6 additional plan-frontmatter truths) are VERIFIED. The tree compiles cleanly, all Phase 4 test suites pass under `-race`, integration tests for the D-26 column rename succeed, and every SCHED-0x requirement maps to working code with test evidence.

The phase goal — "Scheduled backups run reliably per-repo without overlap, thundering herd, or silent pruner-induced data loss" — is achieved:
- **Reliably per-repo:** per-repo mutex via OverlapGuard + stable FNV-1a jitter
- **Without overlap:** TryLock returns false for contending callers (scheduler path logs+skips, on-demand path returns ErrBackupAlreadyRunning)
- **Without thundering herd:** PhaseOffset spreads repos over DefaultMaxJitter = 5min
- **Without silent pruner-induced data loss:** safety rail keeps the only succeeded backup, pinned records are excluded from candidates, destination-first delete ensures no orphaned archives, retention runs under the same per-repo mutex as the upload so there is no race

---

_Verified: 2026-04-16T17:34:19Z_
_Verifier: Claude (gsd-verifier)_
