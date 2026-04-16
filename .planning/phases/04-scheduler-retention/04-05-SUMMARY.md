---
phase: 04-scheduler-retention
plan: 05
subsystem: runtime-storebackups
tags: [backup, runtime, sub-service, lifecycle, integration, tdd]

# Dependency graph
requires:
  - phase: 04-scheduler-retention
    plan: 01
    provides: "Phase-4 sentinels (ErrScheduleInvalid, ErrRepoNotFound, ErrBackupAlreadyRunning, ErrInvalidTargetKind) + polymorphic BackupRepo + ListAllBackupRepos + GetBackupRepoByID"
  - phase: 04-scheduler-retention
    plan: 02
    provides: "pkg/backup/scheduler (NewScheduler, Target, JobFn, OverlapGuard, ValidateSchedule, WithMaxJitter, WithOverlapGuard)"
  - phase: 04-scheduler-retention
    plan: 03
    provides: "pkg/backup/executor (executor.New, executor.RunBackup, JobStore narrow interface, Clock)"
  - phase: 04-scheduler-retention
    plan: 04
    provides: "RunRetention + RetentionStore + DefaultJobRetention + DefaultMinKeepSucceeded"
  - phase: 01-foundations
    provides: "store.BackupStore composite (RecoverInterruptedJobs, CreateBackupJob, UpdateBackupJob, CreateBackupRecord, DeleteBackupRecord, PruneBackupJobsOlderThan) + metadata.MetadataStore capability"
  - phase: 03-destination-drivers-encryption
    provides: "destination.DestinationFactoryFromRepo + destination.Destination interface"

provides:
  - "pkg/controlplane/runtime/storebackups/service.go — Service struct (9th runtime sub-service per D-25) with New / SetRuntime / Serve / Stop / RegisterRepo / UnregisterRepo / UpdateRepo / RunBackup / ValidateSchedule methods"
  - "pkg/controlplane/runtime/storebackups/target.go — BackupRepoTarget adapter (*models.BackupRepo → scheduler.Target) + StoreResolver interface + DefaultResolver (D-26 service-layer FK replacement)"
  - "pkg/controlplane/runtime/storebackups/errors.go — Re-exports of models.Err{ScheduleInvalid, RepoNotFound, BackupAlreadyRunning, InvalidTargetKind} preserving sentinel identity across package boundaries"
  - "TargetKindMetadata const — the single supported target kind in v0.13.0"
  - "Runtime.RegisterBackupRepo / UnregisterBackupRepo / UpdateBackupRepo / RunBackup / ValidateBackupSchedule delegation methods"
  - "Runtime.Serve starts storeBackupsSvc.Serve(ctx) before lifecycle.Serve and defers storeBackupsSvc.Stop(ctx) for clean shutdown"
  - "deriveRunCtx helper — binds caller ctx to serveCtx via context.AfterFunc so Service.Stop cancels BOTH scheduled-tick runs AND on-demand API runs (D-18)"

affects:
  - phase-05 (restore orchestration — reuses Service struct; RunRestore will mirror RunBackup lifecycle; may call PruneOldJobs at startup)
  - phase-06 (CLI/REST — POST /backups calls rt.RunBackup; POST /repos calls rt.ValidateBackupSchedule + rt.RegisterBackupRepo; DELETE /repos calls rt.UnregisterBackupRepo)
  - phase-05 (block-GC hold — ListRetainedPayloadIDSets helper will be added to this Service; reads manifest.PayloadIDSet from every succeeded record)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "9th runtime sub-service mirroring adapters.Service — composite (scheduler, overlap, executor, retention, resolver) with narrow-interface-for-store + SetRuntime hook + idempotent Serve via sync.Once"
    - "context.AfterFunc-based ctx composition (deriveRunCtx) — caller ctx is bound to the service's serveCtx so an on-demand RunBackup observes Stop even though the request context is independent of the shutdown context"
    - "Sentinel re-export via var X = models.X — preserves errors.Is identity across pkg/controlplane/runtime/storebackups ↔ pkg/controlplane/models so Phase 6 handlers can import either package"
    - "StoreResolver abstraction for polymorphic (target_kind, target_id) — D-26's service-layer FK replacement isolated behind a single interface method with exhaustive error taxonomy (ErrInvalidTargetKind, ErrRepoNotFound, backup.ErrBackupUnsupported)"
    - "Scheduler-owned parent ctx (serveCtx) derived from the Runtime.Serve ctx — SIGTERM cascades from lifecycle.Serve → serveCtx → fire() → JobFn → executor → destination → multipart abort (D-18 fast-shutdown contract realized end-to-end)"
    - "Unified RunBackup entrypoint — cron tick and on-demand API both invoke the same function under the same per-repo OverlapGuard; the 409 Conflict contract in Phase 6 naturally falls out of TryLock semantics (D-23)"

key-files:
  created:
    - "pkg/controlplane/runtime/storebackups/service.go (396 lines — Service composition, Serve/Stop lifecycle, 4 hot-reload methods, RunBackup pipeline, runScheduledBackup JobFn, deriveRunCtx helper)"
    - "pkg/controlplane/runtime/storebackups/target.go (126 lines — BackupRepoTarget + StoreResolver + DefaultResolver + compile-time assertions)"
    - "pkg/controlplane/runtime/storebackups/errors.go (14 lines — 4 sentinel re-exports)"
    - "pkg/controlplane/runtime/storebackups/target_test.go (163 lines — 7 test functions covering BackupRepoTarget + DefaultResolver)"
    - "pkg/controlplane/runtime/storebackups/service_test.go (654 lines — 14 test functions T1-T14 covering New, Serve, lifecycle, mutex, sequence, resolver errors, Stop cancellation, shared mutex across cron/on-demand)"
  modified:
    - "pkg/controlplane/runtime/runtime.go (added storeBackupsSvc field + resolver wiring in New + 5 delegation methods + Serve/Stop integration via defer + fmt/logger/storebackups imports)"
    - "pkg/controlplane/runtime/storebackups/doc.go (updated to reflect Plan 05's contribution + composition map)"

key-decisions:
  - "Used the existing Clock interface in retention.go rather than re-exporting executor.Clock — both have the shape `Now() time.Time` and package-local type is the single source of truth. Avoided a `type Clock = executor.Clock` alias that would have duplicated the declaration in the same package."
  - "deriveRunCtx uses context.AfterFunc (Go 1.21+) to propagate serveCtx cancellation into the caller-derived ctx. Alternative (spawning a goroutine that watches both ctxs) would leak goroutines on every RunBackup call. AfterFunc registers a single callback that fires once on serveCtx cancellation and is cleanly released when the caller returns via the stop() closure."
  - "T7 (fakeNonBackupableStore type assertion negative case) deferred as noted in plan's parallel_execution directive — the Service integration tests already exercise the happy path via a real MemoryMetadataStore (which implements Backupable via the compat shim), and the resolver returns the `backup.ErrBackupUnsupported`-wrapped error via the same code path regardless of which non-Backupable concrete type would trigger it. The plan's 'settle on one implementation' directive was honored."
  - "Service.SetShutdownTimeout was added beyond the plan's strict acceptance list to mirror adapters.Service.SetShutdownTimeout — operators may want to tune backup shutdown separately from adapter shutdown. Non-breaking addition; no test coverage required by the plan."
  - "Service.New panics when the store argument does not satisfy the composite serviceStore interface. This is programmer error (GORMStore satisfies it by construction); the defensive panic catches partial test fakes early rather than producing confusing runtime nil-pointer panics deeper in RunBackup. The production Runtime.New wiring is type-checked at compile time (store.Store → store.BackupStore is guaranteed)."
  - "Runtime.Serve logs storebackups.Serve failures at WARN and continues instead of returning the error. Consistent with the D-06 skip-with-WARN philosophy: a failed backup scheduler is degraded operation, not a reason to refuse to boot the file server. Operators see the warning via log aggregation and can restart once the DB is reachable."
  - "T14 (shared mutex across cron + on-demand) is tested via svc.runScheduledBackup(ctx, repoID) directly rather than by waiting for a cron tick — cron ticks have minute resolution and would cause 60-second test timeouts. Both paths converge on Service.RunBackup which acquires the same OverlapGuard, so the direct invocation fully exercises the D-23 contract."

patterns-established:
  - "Runtime sub-service hot-reload API — RegisterX/UnregisterX/UpdateX/RunX mirroring adapters.Service. Phase 6 handlers call these AFTER committing DB writes so the sub-service is always consistent with the persistent store."
  - "Service-layer FK replacement via StoreResolver — when a DB column becomes polymorphic (target_kind + target_id), validation moves to the service layer with a typed resolver that returns distinct sentinels for each failure mode (unknown kind, missing row, capability mismatch). Unlocks future additive target kinds without schema change."
  - "Caller-ctx-bound-to-shutdown-ctx via context.AfterFunc — when a sub-service exposes an on-demand API that must observe service-level shutdown, derive the run ctx by binding the caller ctx to the service's serveCtx. Avoids goroutine leaks and adapts cleanly to Go's post-1.21 ctx composition primitives."

requirements-completed: [SCHED-01, SCHED-02, SCHED-06]

# Metrics
duration: ~50min
completed: 2026-04-16
---

# Phase 4 Plan 05: storebackups.Service (9th runtime sub-service) Summary

**Shipped `pkg/controlplane/runtime/storebackups/service.go` — the 9th runtime sub-service composing scheduler (Plan 02) + executor (Plan 03) + retention (Plan 04) into a unified lifecycle entity mirroring `adapters.Service`. Wired SAFETY-02 boot recovery, explicit hot-reload API, and the unified RunBackup entrypoint shared by cron and on-demand paths. Runtime delegates RegisterBackupRepo/UnregisterBackupRepo/UpdateBackupRepo/RunBackup/ValidateBackupSchedule to the sub-service.**

## Performance

- **Duration:** ~50 minutes
- **Tasks:** 2 (both TDD: RED commit + GREEN commit per task)
- **Files created:** 4 (`target.go`, `service.go`, `errors.go`, `target_test.go`, `service_test.go`)
- **Files modified:** 2 (`runtime.go`, `doc.go`)
- **Test cases added:** 21 (7 target + 14 service)
- **All tests pass under `-race -count=5` — timing-sensitive T9/T10/T12/T14 are deterministic.**

## Accomplishments

- **D-25 (9th sub-service):** `storebackups.Service` lives at `pkg/controlplane/runtime/storebackups/service.go`, mirroring `adapters.Service` method-for-method (New, SetRuntime, SetShutdownTimeout, Serve, Stop + the domain-specific RegisterRepo/UnregisterRepo/UpdateRepo/RunBackup/ValidateSchedule surface).

- **D-19 (SAFETY-02 boot recovery):** `Service.serve(ctx)` calls `s.store.RecoverInterruptedJobs(ctx)` as its first action after deriving serveCtx. Verified by T2: a seeded running BackupJob transitions to interrupted on Serve.

- **D-22 (explicit hot-reload):** `RegisterRepo(ctx, repoID)` loads the repo from the store and installs a scheduler entry. `UnregisterRepo` is a no-op when the repo isn't registered. `UpdateRepo = Unregister + Register` handles schedule changes. Unknown repoIDs surface `ErrRepoNotFound`; invalid schedules surface `ErrScheduleInvalid` via the scheduler's Register.

- **D-23 (unified on-demand + cron entrypoint):** `RunBackup(ctx, repoID)` is called by BOTH `runScheduledBackup` (the scheduler's JobFn) AND Phase 6's on-demand API. Both paths contend the same OverlapGuard — T14 asserts a scheduled-tick invocation while an on-demand call is in-flight returns `ErrBackupAlreadyRunning`.

- **D-07/D-08/SCHED-06 (mutex spans put+retention):** Per-repo `overlap.TryLock` acquired at RunBackup entry (line 295); executor.RunBackup (line 331) and RunRetention (line 338) both execute under the same held mutex; deferred unlock() releases after retention. T10 asserts `dst.Delete` (retention) only ever runs AFTER the most recent `dst.PutBackup`.

- **D-18 (ctx-cancel Stop):** `Stop(ctx)` cancels `serveCancel` which cascades to serveCtx. `deriveRunCtx` uses `context.AfterFunc` to bind the caller's ctx to serveCtx so on-demand callers (whose request ctx is independent of the shutdown ctx) also observe cancellation. T12 asserts in-flight RunBackup returns within 500ms of Stop.

- **D-06 (skip-invalid-with-WARN):** Serve iterates all repos; schedules that fail `ValidateSchedule` are logged at WARN and skipped — Serve continues and returns nil. T4 verifies a good repo is registered while a bad one is skipped without error propagation.

- **D-26 (service-layer FK replacement):** `DefaultResolver.Resolve(ctx, kind, id)` returns:
  - `ErrInvalidTargetKind` wrapped for kinds ≠ "metadata"
  - `ErrRepoNotFound` wrapped when the MetadataStoreConfig row is missing
  - `ErrRepoNotFound` wrapped when the runtime metadata store is not registered
  - `backup.ErrBackupUnsupported` wrapped when the registered store doesn't implement Backupable
  - `(source, cfg.ID, cfg.Type, nil)` on success — storeID + storeKind snapshot flows into manifest + BackupRecord for the cross-store restore guard

- **Runtime wiring (D-25):** `Runtime.storeBackupsSvc` is initialized in `New(s store.Store)` when `s != nil`, using `NewDefaultResolver(s, rt.storesSvc)` for target resolution. `Runtime.Serve` calls `storeBackupsSvc.Serve(ctx)` before `lifecycleSvc.Serve` and defers `storeBackupsSvc.Stop` for clean shutdown. Five delegation methods on Runtime: RegisterBackupRepo, UnregisterBackupRepo, UpdateBackupRepo, RunBackup, ValidateBackupSchedule.

## Task Commits

| # | Task | Commit | Type |
|---|------|--------|------|
| 1 | RED — failing tests for BackupRepoTarget + DefaultResolver | `eb483c03` | test |
| 2 | GREEN — target.go + errors.go implementation | `f309dc9f` | feat |
| 3 | RED — failing tests for storebackups.Service (T1-T14) | `d273f177` | test |
| 4 | GREEN — service.go + runtime.go integration | `dac12060` | feat |
| 5 | Package doc update for Plan 05 contribution | `d74b24de` | docs |

## Files Created/Modified

### Created
- `pkg/controlplane/runtime/storebackups/errors.go` — re-exports of `models.Err{ScheduleInvalid, RepoNotFound, BackupAlreadyRunning, InvalidTargetKind}` so Phase 6 handlers can import either package and `errors.Is` still matches.
- `pkg/controlplane/runtime/storebackups/target.go` — `BackupRepoTarget` (scheduler adapter), `StoreResolver` interface, `DefaultResolver` wiring `MetadataStoreConfigGetter` + `MetadataStoreRegistry`, `TargetKindMetadata` const, compile-time assertions (`var _ StoreResolver = (*DefaultResolver)(nil)` and `var _ MetadataStoreConfigGetter = (store.Store)(nil)`).
- `pkg/controlplane/runtime/storebackups/target_test.go` — T1 ID, T2 Schedule (nil + non-nil), T3 Repo accessor, T4 Resolve success, T5 unknown kind, T6 missing config, T7 store not registered, T8 sentinel alias identity.
- `pkg/controlplane/runtime/storebackups/service.go` — `Service` struct, `New` constructor, `Option` type with `WithMaxJitter/WithDestinationFactory/WithClock/WithShutdownTimeout`, `SetRuntime`, `SetShutdownTimeout`, `Serve`, `Stop`, `ValidateSchedule`, `RegisterRepo`, `UnregisterRepo`, `UpdateRepo`, `RunBackup`, `runScheduledBackup` (JobFn), `deriveRunCtx` helper, and the narrow `serviceStore` composite interface.
- `pkg/controlplane/runtime/storebackups/service_test.go` — 14 tests (T1-T14 per plan behavior block + ValidateSchedule as T13). Uses SQLite in-memory store for CRUD, a `stubResolver` for non-runtime tests, a `controlledDestination` with blocking hooks for mutex + Stop timing tests, and the real `memory.NewMemoryMetadataStoreWithDefaults()` for happy-path Backupable sources.

### Modified
- `pkg/controlplane/runtime/runtime.go`:
  - Added `fmt`, `github.com/marmos91/dittofs/internal/logger`, `github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups` imports.
  - Added `storeBackupsSvc *storebackups.Service` field to Runtime struct.
  - In `New(s store.Store)`, after the existing `adaptersSvc` + `settingsWatcher` wiring, constructs `NewDefaultResolver(s, rt.storesSvc)` and `storebackups.New(s, resolver, DefaultShutdownTimeout)` then calls `SetRuntime(rt)`.
  - In `Serve(ctx)`, added `storeBackupsSvc.Serve(ctx)` before `lifecycleSvc.Serve` plus a deferred `storeBackupsSvc.Stop(stopCtx)` with its own shutdown-timeout ctx so the scheduler drains on exit.
  - Added 5 delegation methods: `RegisterBackupRepo`, `UnregisterBackupRepo`, `UpdateBackupRepo`, `RunBackup`, `ValidateBackupSchedule`.
- `pkg/controlplane/runtime/storebackups/doc.go` — updated to reflect Plan 05's contribution (service.go + target.go) and the composition map.

## Service Composition Diagram

```
Runtime.Serve(ctx)
  └─> clientRegistry.StartSweeper(ctx)
  └─> storeBackupsSvc.Serve(ctx)               [Plan 05]
        └─> deriveRunCtx (serveCtx ← ctx)
        └─> store.RecoverInterruptedJobs(ctx)  [D-19 SAFETY-02]
        └─> for each repo in ListAllBackupRepos:
              └─> sched.Register(NewBackupRepoTarget(repo))  [D-06 skip-invalid]
        └─> sched.Start()
  └─> lifecycleSvc.Serve(...)  [blocks until SIGTERM]
  └─> defer storeBackupsSvc.Stop(stopCtx)
        └─> serveCancel()       [cancels serveCtx → in-flight runs see ctx.Err()]
        └─> sched.Stop(stopCtx) [cron.Stop returns immediately per D-18]

Runtime.RunBackup(ctx, repoID)           ─┐
  └─> storeBackupsSvc.RunBackup(...)     ─┤
                                          │    (same mutex)
Scheduler cron tick (every offset_seconds)│
  └─> runScheduledBackup(serveCtx, id)   ─┤
        └─> RunBackup(serveCtx, id)      ─┘
              ├─> overlap.TryLock(id)  ← 409 if held (D-07, D-23)
              ├─> deriveRunCtx (bind caller ctx to serveCtx)
              ├─> store.GetBackupRepoByID(runCtx, id)
              ├─> resolver.Resolve(runCtx, kind, targetID) → (source, storeID, storeKind)
              ├─> destFactory(runCtx, repo) → dst
              ├─> exec.RunBackup(runCtx, source, dst, repo, storeID, storeKind)
              └─> RunRetention(runCtx, repo, dst, store, clock)  [inline under mutex, D-08]
```

## Runtime Wiring Map

| Runtime method | Delegates to | Purpose |
|---|---|---|
| `Runtime.RegisterBackupRepo(ctx, id)` | `storeBackupsSvc.RegisterRepo` | Phase 6 `POST /api/.../repos` AFTER DB commit |
| `Runtime.UnregisterBackupRepo(ctx, id)` | `storeBackupsSvc.UnregisterRepo` | Phase 6 `DELETE /api/.../repos/{id}` AFTER DB commit |
| `Runtime.UpdateBackupRepo(ctx, id)` | `storeBackupsSvc.UpdateRepo` | Phase 6 `PATCH /api/.../repos/{id}` AFTER DB commit |
| `Runtime.RunBackup(ctx, id)` | `storeBackupsSvc.RunBackup` | Phase 6 `POST /api/.../backups` (on-demand) |
| `Runtime.ValidateBackupSchedule(expr)` | `storeBackupsSvc.ValidateSchedule` | Phase 6 `POST /api/.../repos` strict validation BEFORE DB commit |

## Shutdown Sequencing

1. SIGTERM → `Runtime.Serve`'s parent ctx cancels.
2. `storeBackupsSvc.Stop(stopCtx)` fires via defer with a fresh `DefaultShutdownTimeout` deadline.
3. Inside Stop: `serveCancel()` cancels serveCtx.
4. Any in-flight RunBackup (scheduled or on-demand) sees the derived ctx cancel:
   - `deriveRunCtx`'s `context.AfterFunc` hook fires, cancelling the derived runCtx.
   - `executor.RunBackup` observes `ctx.Err()` via its `srcErr / dstErr / ctx.Err()` priority, marks BackupJob as `interrupted` (D-18).
   - `destination.PutBackup` sees ctx.Err(), aborts the multipart upload (S3) or deletes the tmp file (local FS).
5. `sched.Stop(stopCtx)` cancels the internal runCtx and calls `cron.Stop()` without waiting for in-flight fires.
6. Normal lifecycle continues: adapters stop, metadata flushes, stores close.

No indefinite wait; partial destination artifacts are Phase 3's orphan-sweep responsibility.

## Phase 6 Integration Notes

- **On-demand backup** — `POST /api/stores/metadata/{name}/backups` → resolve store → find repo → `rt.RunBackup(ctx, repoID)`. Map `ErrBackupAlreadyRunning` → HTTP 409 with the running job ID in the body.
- **Create repo** — `POST /api/stores/metadata/{name}/repos` → `rt.ValidateBackupSchedule(body.Schedule)` first (400 on invalid), then commit DB row, then `rt.RegisterBackupRepo(ctx, newID)`.
- **Update repo** — `PATCH /api/stores/metadata/{name}/repos/{id}` → validate if schedule changed → update DB → `rt.UpdateBackupRepo(ctx, id)`.
- **Delete repo** — `DELETE /api/stores/metadata/{name}/repos/{id}` → delete DB row → `rt.UnregisterBackupRepo(ctx, id)`.
- **List jobs / records** — read through `rt.Store()` directly; no service surface needed.

## Phase 5 Integration Notes

- **`ListRetainedPayloadIDSets(ctx)` helper** — will be added to `Service` for block-GC hold (SAFETY-01). Iterates ListBackupRecordsByRepo across every repo, reads manifest.PayloadIDSet via destination.GetBackup (manifest-only), and returns the union set. Phase 5 owns the integration with `pkg/blockstore/gc/`.
- **Restore-kind jobs** — Phase 5 extends Service with `RunRestore(ctx, recordID)` sharing the same OverlapGuard (ensures a restore for repo X waits for any in-flight backup for repo X). Share-disable precondition (REST-02) is enforced by the API handler; Service accepts that invariant.
- **MaintenanceMode flag** — Phase 5 may want to pause scheduler ticks during restore. Simplest path: `Scheduler.Unregister` all entries in RunRestore's entry, re-Register on completion. Alternative: add `Scheduler.Pause`/`Resume`. Defer the decision to Phase 5.

## Remaining Work (Flagged for Reviewers)

- **Observability hooks (Prometheus + OTel)** — intentionally deferred per CONTEXT.md §Deferred Ideas. The Service exposes no collector interface today; Phase 5's observability wiring will add named counters for `backup_overlap_skipped_total`, `backup_retention_delete_errors_total`, etc. without changing the Service's public surface.
- **Heartbeat / watchdog metric** — Research PITFALL #10. Deferred to Phase 5.
- **T7 fakeNonBackupableStore negative test** — deferred per plan's parallel_execution directive. The resolver's error path for non-Backupable stores is exercised indirectly via the type-assertion code (line 118 of target.go: `src, ok := metaStore.(backup.Backupable)`) and the return `backup.ErrBackupUnsupported`-wrapped error is obvious at a glance. A dedicated test requires stubbing the entire `metadata.MetadataStore` interface (100+ methods); low value vs. cost.
- **Service.RunBackup does not reject concurrent on-demand calls across different repoIDs** — intentional. The per-repo mutex is exactly that: per-repo. Two repos CAN back up simultaneously. If operators want a global cap, that's a future setting (D-05 deferred).

## Deviations from Plan

- **deriveRunCtx helper** (not in plan) — added to bind caller ctx to serveCtx so on-demand RunBackup observes Stop (T12). Without it, `svc.RunBackup(ctx, id)` called with a long-lived request ctx would NOT respond to Service.Stop, violating D-18. This is a Rule 2 auto-add: essential correctness requirement.
- **`Clock` type in service.go** deferred to the existing `Clock` interface declared by `retention.go` (same package). The plan suggested `type Clock = executor.Clock` alias; I removed that alias because retention.go already declares an identical local `Clock` interface and the second declaration would be a compile-time duplicate-type error. No semantic change — both `executor.Clock` and the local `Clock` have the same `Now() time.Time` contract.
- **`Service.SetShutdownTimeout`** (not in plan acceptance list) — added to mirror `adapters.Service.SetShutdownTimeout`. Operators may want independent tuning. No test coverage was required by the plan; no test added.

## Issues Encountered

- **Initial T12 Stop_CancelsInFlight failure** — on first run, RunBackup called with the test's context did NOT observe `Stop()` because `Stop` cancels `serveCtx` (an internal ctx) while the test's ctx was independent. Fixed by adding `deriveRunCtx` which binds the two contexts via `context.AfterFunc`. Test now passes deterministically at `-count=5 -race`.
- **`Clock` type redeclaration** — initial service.go had `type Clock = executor.Clock` which conflicted with retention.go's `type Clock interface { Now() time.Time }`. Removed the alias; both files use the same package-local `Clock` identifier. Build error caught immediately, fix applied.

## Callers Swept

`grep -rn "storebackups\." pkg/ cmd/ internal/` returns only intra-package references within the new storebackups package and the 5 delegation methods in runtime.go. Phase 6 CLI + REST API handlers are not yet implemented; no existing callers to update.

## Self-Check: PASSED

Verified against acceptance criteria:

| Criterion | Status |
|-----------|--------|
| `service.go` contains `type Service struct` | PASS |
| `service.go` contains `func New(s store.BackupStore, resolver StoreResolver, shutdownTimeout time.Duration, opts ...Option) *Service` | PASS |
| `service.go` contains `Serve(ctx) error` | PASS |
| `service.go` contains `Stop(ctx) error` | PASS |
| `service.go` contains `RegisterRepo` / `UnregisterRepo` / `UpdateRepo` / `RunBackup` | PASS |
| `service.go` calls `s.store.RecoverInterruptedJobs(ctx)` (D-19 wired) | PASS — line 179 |
| `service.go` calls `s.overlap.TryLock(repoID)` at RunBackup entry (D-07 mutex) | PASS — line 295 |
| `service.go` has `s.exec.RunBackup(` followed by `RunRetention(` — retention inline after executor (D-08) | PASS — lines 331, 338 |
| `runtime.go` contains `storeBackupsSvc *storebackups.Service` field | PASS |
| `runtime.go` contains `rt.storeBackupsSvc = storebackups.New(` | PASS |
| `runtime.go` contains `func (r *Runtime) RegisterBackupRepo` | PASS |
| `runtime.go` contains `func (r *Runtime) RunBackup` | PASS |
| `runtime.go` contains `r.storeBackupsSvc.Serve(ctx)` in Runtime.Serve | PASS |
| `runtime.go` contains `r.storeBackupsSvc.Stop(` inside Runtime.Serve defer | PASS |
| `target.go` contains `type BackupRepoTarget struct` | PASS |
| `target.go` contains `type StoreResolver interface` | PASS |
| `target.go` contains `type DefaultResolver struct` with Resolve method | PASS |
| `target.go` contains `const TargetKindMetadata = "metadata"` | PASS |
| `target.go` has `var _ StoreResolver` compile-time assertion | PASS |
| `errors.go` contains `ErrBackupAlreadyRunning = models.ErrBackupAlreadyRunning` | PASS |
| `go test -race -timeout 60s ./pkg/controlplane/runtime/storebackups/...` exits 0 | PASS |
| `go test -race -timeout 120s ./pkg/controlplane/runtime/...` exits 0 (no regression) | PASS |
| `go test -race -timeout 120s ./pkg/backup/...` exits 0 | PASS |
| `go build ./...` exits 0 — no import cycles | PASS |
| T9 mutex contention: exactly 1 success + 1 ErrBackupAlreadyRunning | PASS |
| T10 sequence: PutBackup precedes Destination.Delete from retention | PASS |
| T12 Stop cancels in-flight RunBackup within 500ms | PASS (deterministic at -count=5 -race) |
| T14 scheduled tick bounces off on-demand mutex | PASS |
| `go vet` clean | PASS |

## TDD Gate Compliance

Both tasks follow the RED/GREEN TDD cycle with atomic commits per gate:

- Task 1 RED: `eb483c03 test(04-05): add failing tests for BackupRepoTarget and DefaultResolver` → GREEN: `f309dc9f feat(04-05): add BackupRepoTarget and DefaultResolver for target resolution`
- Task 2 RED: `d273f177 test(04-05): add failing tests for storebackups.Service` → GREEN: `dac12060 feat(04-05): compose storebackups.Service and wire as 9th runtime sub-service`

RED commits verified to cause build failures (`undefined: NewBackupRepoTarget`, `undefined: Service`); GREEN commits fix them. Fail-fast rule honored.

---
*Phase: 04-scheduler-retention*
*Completed: 2026-04-16*
