---
phase: 04-scheduler-retention
plan: 02
subsystem: backup
tags: [backup, scheduler, cron, jitter, overlap, tdd]

# Dependency graph
requires:
  - phase: 04-scheduler-retention
    plan: 01
    provides: "models.ErrScheduleInvalid + ErrRepoNotFound + ErrBackupAlreadyRunning + ErrInvalidTargetKind sentinels (Phase 4 Plan 01 D-26)"
provides:
  - "pkg/backup/scheduler/jitter.go — PhaseOffset(repoID, max) FNV-1a stable per-repo offset (D-03) + DefaultMaxJitter const (D-04)"
  - "pkg/backup/scheduler/overlap.go — OverlapGuard.TryLock(repoID) per-key sync.Mutex (D-07)"
  - "pkg/backup/scheduler/schedule.go — ValidateSchedule(expr) strict cron parse wrapping ErrScheduleInvalid (D-06) + wrapScheduleError helper"
  - "pkg/backup/scheduler/scheduler.go — Scheduler struct wrapping robfig/cron/v3 with Register/Unregister/Start/Stop + fire() jitter+overlap integration"
  - "pkg/backup/scheduler Target interface (ID, Schedule) — store-agnostic binding contract for Plan 05 BackupRepoTarget adapter (D-24)"
  - "JobFn(ctx, targetID) error callback contract — Plan 03 executor will be invoked from here via Plan 05 wiring"
  - "WithMaxJitter + WithOverlapGuard options — Plan 05 injects a shared OverlapGuard to serialize cron+on-demand paths (D-23)"
  - "robfig/cron/v3 v3.0.1 added to go.mod — the ONLY new external dependency"
affects:
  - 04-03 (executor — consumes JobFn contract shape via Plan 05 wiring, not directly)
  - 04-04 (storebackups sub-service — composes Scheduler, calls Register/Unregister, binds shared OverlapGuard, adapts BackupRepo → Target)
  - 04-05 (retention pass — unaffected, scheduler fires JobFn which triggers executor → retention inline)
  - phase-05 (restore — reuses same OverlapGuard via shared injection to return 409 Conflict while cron run holds it per D-23)
  - phase-06 (REST API — `POST /api/.../repos` handler calls ValidateSchedule synchronously per D-06)
  - future-block-store-backup (reuses Scheduler + Target interface verbatim via BlockStoreTarget adapter — D-24 contract)

# Tech tracking
tech-stack:
  added:
    - "github.com/robfig/cron/v3 v3.0.1 — store-agnostic cron parser with CRON_TZ= prefix support; used as-is with explicit invariants at the orchestrator layer (no wrapper to 'fix' defaults per D-specifics)"
  patterns:
    - "Store-agnostic Target interface — scheduler accepts any type with ID()/Schedule() so future block-store-backup reuses the same primitives without refactor (D-24)"
    - "FNV-1a over repoID for deterministic per-run jitter — survives restart, operator-debuggable (D-03). Non-cryptographic by design; the repo ID is not a secret"
    - "sync.Map + per-key sync.Mutex.TryLock for overlap — mirrors pkg/adapter/nfs/connection.go convention; keys retained for lifetime of scheduler (bounded by repo count)"
    - "Shared OverlapGuard between cron and on-demand paths — D-23 contract realized via WithOverlapGuard option; both code paths contend the same mutex"
    - "Unexported fire() called directly from tests in same package — bypasses wall-clock cron for hermetic sub-second test runs. No production path outside the cron callback"
    - "robfig/cron/v3 EntryID is NOT persistent across restart — scheduler must re-register from DB on Serve; Plan 04-04 owns that orchestration (D-specifics)"
    - "Stop cancels runCtx immediately without waiting for in-flight JobFn — D-18 fast-shutdown contract; partial destination artifacts are Phase 3 orphan-sweep's responsibility"

key-files:
  created:
    - "pkg/backup/scheduler/doc.go — package doc referencing D-03/D-04/D-06/D-07"
    - "pkg/backup/scheduler/jitter.go — PhaseOffset + DefaultMaxJitter"
    - "pkg/backup/scheduler/overlap.go — OverlapGuard + NewOverlapGuard + TryLock"
    - "pkg/backup/scheduler/schedule.go — ValidateSchedule + wrapScheduleError"
    - "pkg/backup/scheduler/scheduler.go — Scheduler + Target + JobFn + Option + NewScheduler + fire()"
    - "pkg/backup/scheduler/jitter_test.go — PhaseOffset range/stability/spread/zero-max/empty-ID"
    - "pkg/backup/scheduler/overlap_test.go — TryLock exclusivity, 100-goroutine winner, independent keys, reacquire cycle"
    - "pkg/backup/scheduler/schedule_test.go — valid/invalid cron table (13 cases) + wrapScheduleError identity"
    - "pkg/backup/scheduler/scheduler_test.go — table-driven T1-T8 + idempotent + replace + WithOverlapGuard + standalone OverlapUnderLoad"
  modified:
    - "go.mod — add github.com/robfig/cron/v3 v3.0.1"
    - "go.sum — cron v3.0.1 hash"

key-decisions:
  - "Defined Target interface locally in pkg/backup/scheduler (not in pkg/backup) — keeps the scheduler module self-describing and avoids polluting the top-level pkg/backup with scheduler-specific contracts. Plan 05's BackupRepoTarget adapter can live anywhere (most naturally next to storebackups.Service)"
  - "Scheduler does NOT own goroutine-per-repo — we use robfig/cron's single internal loop + bound fire() callbacks. Rationale: the cron library already multiplexes Schedule.Next() efficiently; a goroutine-per-repo layer would duplicate work and add lifecycle complexity for the Unregister case. The fire() callback itself is synchronous on cron's goroutine, so our jitter sleep DOES hold cron's goroutine — but cron.Start spawns one goroutine per Run() tick, so this does not block other repos"
  - "Stop does NOT wait on cron's returned context (D-18). We cancel our own runCtx (which unblocks fire()'s offset sleep), then call cron.Stop() and discard its returned ctx. In-flight JobFns see runCtx.Done() via their ctx parameter and return ctx.Err(); destination drivers see ctx.Err() in multipart uploads and abort. Partial destination artifacts are Phase 3's orphan-sweep problem"
  - "Idempotent Register on (ID, schedule) — matching existing pkg/controlplane/runtime/adapters behavior. Re-registering the same pair is a no-op; re-registering with a different schedule removes the old entry first. This lets Plan 04-04 call Register on every boot from DB without worrying about duplicate entries"
  - "wrapScheduleError short-circuits when err already wraps ErrScheduleInvalid — prevents double-wrapping '%w: bar: %w: foo: ...' chains when ValidateSchedule and an adjacent code path both produce wrapped errors. Identity check via errors.Is, not errors.As, because we only care about reachability of the sentinel"
  - "fire() is unexported but package-local tests call it directly to bypass wall-clock cron. This is the 'test-internals hook' documented in the threat model. There is no production path that reaches fire() outside the cron callback wired at Register time, so the hermetic-test hook is a test-only attack surface that does not widen external exposure"
  - "PhaseOffset guards max<=0 AND max<1s — the modulo divides by uint64(max/time.Second) which would panic on zero. Returning 0 for sub-second jitter aligns with cron's second-resolution tick model and keeps arithmetic in unsigned integer space"

patterns-established:
  - "Store-agnostic scheduler — Target interface (ID, Schedule) lets the same primitives serve metadata-store-backup today and block-store-backup tomorrow without refactor (D-24)"
  - "TDD with RED/GREEN commits — failing tests committed first (build-error on undefined symbols), then implementation; commit history documents which behaviors drove which code additions"
  - "Internal unexported helper invoked from same-package tests — fire() bypasses wall-clock cron for sub-second tests; documented as a test-only hook in the threat model to prevent misuse expansion"
  - "Shared guard across cron + on-demand — WithOverlapGuard option propagates a single OverlapGuard instance to both the scheduler's fire() path and the on-demand RunBackup path; the 409 Conflict contract at the REST layer naturally falls out of the same TryLock() semantics (D-23)"

requirements-completed: [SCHED-01, SCHED-02]

# Metrics
duration: ~45min
completed: 2026-04-16
---

# Phase 4 Plan 02: Scheduler Primitives Summary

**Shipped a store-agnostic `pkg/backup/scheduler` package: FNV-1a jitter, per-repo OverlapGuard, strict ValidateSchedule, and a Scheduler wrapper over robfig/cron/v3 that integrates all three. The Target interface lets Plan 05 adapt `*models.BackupRepo` without this package importing controlplane models — future block-store-backup work reuses the same primitives verbatim (D-24).**

## Performance

- **Duration:** ~45 minutes
- **Tasks:** 2 (both TDD — 4 commits total: RED + GREEN per task)
- **Files created:** 9 (4 source + 4 test + 1 doc)
- **Files modified:** 2 (go.mod, go.sum)
- **New external dep:** 1 (robfig/cron/v3 v3.0.1 — the only permitted new direct dependency per research SUMMARY)

## Accomplishments

- **SCHED-01 (cron primitives)** — `Scheduler` wraps `robfig/cron/v3` with `Register/Unregister/Start/Stop`. Valid cron expressions (including `CRON_TZ=Europe/Rome` prefix) install entries via `cron.AddFunc`; invalid expressions are rejected with a wrapped `models.ErrScheduleInvalid` before any cron state is touched. Missed runs are skipped per D-01 (matches robfig default).

- **SCHED-02 (overlap + jitter)** — `OverlapGuard.TryLock(repoID)` returns `(unlock, false)` when the per-key mutex is held, so a second cron tick for the same repo is skipped while the first is running. `PhaseOffset(repoID, max)` returns the same offset for the same ID on every call — operators can correlate "repo X always fires at 00:03:42" with ops events (D-03). `DefaultMaxJitter = 5min` (D-04) spreads ~20 repos by ~15 seconds each.

- **D-06 strict-at-write-time validation** — `ValidateSchedule(expr)` is exported for Phase 6's `POST /api/.../repos` handler to call synchronously before persisting the DB row, rejecting invalid schedules with 400. The same function is called by `Scheduler.Register` so Plan 05's Serve-time loader can also wrap parse failures as `ErrScheduleInvalid` and WARN-continue rather than fatal-boot.

- **D-07 per-repo overlap mutex** — `sync.Map[repoID → *sync.Mutex]` lazily creates per-key mutexes on first contact via `LoadOrStore`. `TryLock` returns `(nil, false)` on contention; callers log + skip. The pattern mirrors `pkg/adapter/nfs/connection.go` connection tracking.

- **D-22 explicit hot-reload API** — `Register(target)` and `Unregister(id)` are explicit methods; no polling (settings_watcher.go is explicitly rejected as the anti-pattern per PATTERNS.md). Re-registering with the same schedule is a no-op; re-registering with a different schedule removes the old entry first — lets Plan 04-04 call `Register` unconditionally on boot without tracking which schedules changed.

- **D-23 shared guard for 409 Conflict semantics** — `WithOverlapGuard(shared)` option lets Plan 05's `storebackups.Service` inject the same `OverlapGuard` into both the cron path (`Scheduler.fire`) and the on-demand path (`RunBackup`). Any caller of `RunBackup` while a scheduled tick is running sees `TryLock() == false` and returns `models.ErrBackupAlreadyRunning`, which the Phase 6 handler maps to 409.

- **D-18 fast-shutdown semantics** — `Stop(ctx)` cancels an internal `runCtx` that `fire()` uses both for jitter-sleep abort (`select { <-time.After(offset); <-runCtx.Done() }`) and as the parent ctx passed to JobFn. In-flight runs see `ctx.Err()` and abort; destination drivers see the same ctx and abort multipart uploads; partial artifacts are Phase 3 orphan-sweep's responsibility. `cron.Stop()`'s returned context is intentionally discarded — we do NOT wait for in-flight JobFns.

- **Hermetic tests** — `scheduler_test.go` invokes the unexported `fire()` directly (same-package testing) to bypass wall-clock cron firing. No test blocks on cron's minute-resolution tick; the longest sleep in any test is 1s (TestScheduler T5b), and race-detector passes at `-count=5` without flakes.

## Task Commits

1. **Task 1 RED: failing tests for scheduler primitives** — `0db3298c` (test)
2. **Task 1 GREEN: PhaseOffset + OverlapGuard + ValidateSchedule** — `42bcf745` (feat)
3. **Task 2 RED: failing tests for Scheduler wrapper** — `34b50907` (test)
4. **Task 2 GREEN: Scheduler over robfig/cron/v3** — `4677d5d8` (feat)

## Files Created/Modified

### Created

- **`pkg/backup/scheduler/doc.go`** — Package doc referencing D-03, D-04, D-06, D-07 and citing D-24 store-agnostic rationale.
- **`pkg/backup/scheduler/jitter.go`** — `PhaseOffset(repoID, max) time.Duration` (FNV-1a % seconds) and `DefaultMaxJitter = 5 * time.Minute`. Guards both `max <= 0` and `max < 1s` to keep arithmetic in uint64 space.
- **`pkg/backup/scheduler/overlap.go`** — `OverlapGuard` (unexported `sync.Map` field) + `NewOverlapGuard()` + `TryLock(repoID) (unlock func(), acquired bool)`. Returned `unlock` closure is the bound `mu.Unlock` method.
- **`pkg/backup/scheduler/schedule.go`** — `ValidateSchedule(expr string) error` (uses `cron.ParseStandard`, rejects empty string explicitly) + `wrapScheduleError(expr, err)` internal helper that short-circuits if `err` already wraps `ErrScheduleInvalid` to prevent double-wrapping.
- **`pkg/backup/scheduler/scheduler.go`** — Core wrapper. `Target` interface (ID, Schedule), `JobFn` type, `Scheduler` struct (mu, entries map, cron, overlap guard, jobFn, maxJit, runCtx/runCancel), Options (`WithMaxJitter`, `WithOverlapGuard`), lifecycle methods (`SetJobFn`, `Register`, `Unregister`, `IsRegistered`, `Registered`, `Start`, `Stop`), and the unexported `fire()` callback.
- **`pkg/backup/scheduler/jitter_test.go`** — 7 test functions covering range, stability (1000 iterations), different-IDs, zero-max edge cases, 20-id spread check, empty-ID determinism, `DefaultMaxJitter` constant.
- **`pkg/backup/scheduler/overlap_test.go`** — 5 test functions: basic TryLock, exclusive-per-key (table-driven), 100-goroutine concurrent winner with proper hold-until-all-tried synchronization, 50-distinct-keys concurrent acquire-all, reacquire cycle.
- **`pkg/backup/scheduler/schedule_test.go`** — Table-driven `TestValidateSchedule` (13 cases: hourly, every-minute, daily, every-5-min, 3 CRON_TZ cases, 6 invalid cases) + error message check + `TestWrapScheduleError` covering both short-circuit and fresh-wrap paths.
- **`pkg/backup/scheduler/scheduler_test.go`** — Table-driven `TestScheduler` (T1-T8 as named sub-tests) + 5 standalone tests (idempotent, replace, fire-without-job, nil-target, with-overlap-guard) + `TestScheduler_OverlapUnderLoad` exposed as a standalone test so acceptance criteria `-run TestScheduler_OverlapUnderLoad -count=5 -race` has a target. 10 `t.Run` sub-tests total, exceeding the acceptance floor of 8.

### Modified

- **`go.mod`** — Added `github.com/robfig/cron/v3 v3.0.1` in the direct-deps block. No other module edits.
- **`go.sum`** — Corresponding hash entry.

## Decisions Made

- **Target interface lives in `pkg/backup/scheduler` not `pkg/backup`.** The scheduler is the only code that needs this abstraction; exposing it one level up would pollute `pkg/backup` with scheduler-specific contracts. Plan 04-04's `BackupRepoTarget` adapter can live next to `storebackups.Service`.

- **Single cron loop, not goroutine-per-repo.** `robfig/cron/v3` already multiplexes `Schedule.Next()` efficiently; a goroutine-per-repo wrapper would duplicate that work. Each `AddFunc` callback runs on a cron-owned goroutine (cron.Start spawns one goroutine per Run() tick), so our jitter-sleep inside fire() holds that goroutine briefly but does NOT serialize other repos. PATTERNS.md called out goroutine-per-entity as "one option"; we took the simpler alternative because the library already handles concurrency correctly.

- **`Stop` does NOT wait on cron's returned context.** D-18 explicitly requires fast shutdown — the `cron.Stop()` returned ctx closes when in-flight jobs finish, but we discard it and let the cancelled `runCtx` propagate through JobFn → destination → multipart-abort path. Small backups may occasionally race shutdown and get lost to timing, which is acceptable per D-18 ("next cron tick retries").

- **`fire()` unexported but reachable from same-package tests.** The threat model's T-04-02 row acknowledges this as a test-only hook. There is no production path that reaches fire() outside the cron callback wired at Register time, so the hermetic-test escape does not widen external exposure. Alternative (exporting via a `fire_test.go` export_test.go shim) was considered but rejected as unnecessary ceremony — same-package testing is the Go idiomatic solution.

- **Idempotent Register on (ID, schedule).** Matches the existing `pkg/controlplane/runtime/adapters/service.go` convention (CreateAdapter is idempotent on the store side via CreateOrUpdate semantics). Plan 04-04's boot-time load iterates repos and calls Register on each; it doesn't need to track which schedules changed. Re-registering with a different schedule removes the old entry first so cron.Remove + AddFunc are not duplicated.

- **`wrapScheduleError` short-circuits.** If `err` already wraps `ErrScheduleInvalid`, returning a double-wrapped `fmt.Errorf("%w: ...: %w: ...", ...)` chain produces confusing error messages. The helper is internal; the only current call site is the belt-and-suspenders path inside `Register` after `ValidateSchedule` already returned nil (cron.AddFunc fails despite ParseStandard succeeding — shouldn't happen in practice, but defensive).

- **`PhaseOffset` guards `max < 1s`.** The modulo divides by `uint64(max/time.Second)` which would be zero for sub-second `max` and cause a panic. Returning 0 aligns with cron's second-resolution tick model.

## Deviations from Plan

None — plan executed exactly as written.

The plan's acceptance criteria required `grep -c "t.Run" pkg/backup/scheduler/scheduler_test.go >= 8`. The first draft used distinct test functions per behavior, which count as tests but not as `t.Run` matches. I restructured the test file so T1-T8 are sub-tests inside a single `TestScheduler` driver — produces 10 `t.Run` occurrences and keeps the failure report precise per-behavior. This is an internal test-style adjustment, not a deviation from spec.

## Authentication Gates

None — this is pure library code with no external service interaction.

## Issues Encountered

**Test-1 race flake in `TestOverlapGuard_Concurrent100`.** The first draft had each successful goroutine `defer unlock()` and exit immediately. Under high concurrency, the winner could release BEFORE later goroutines even attempted TryLock — so multiple goroutines could each "win" sequentially, producing 72/100 successes instead of 1/100. Fixed by holding the winner on `<-holdUntilAllTried` until the test driver confirmed all N goroutines had attempted acquisition (`attempts.Load() == n`), then closing the release channel. This now passes deterministically at `-count=10 -race`.

## Callers Swept

None — this is greenfield package code; no existing imports.

`grep -rn "pkg/backup/scheduler" pkg/ internal/ cmd/` returns only internal references within the new package. Plan 04-04 (Wave 4 storebackups.Service) will be the first external consumer.

## Pointers for Plan 04-04 (Wave 4 storebackups.Service)

- **Binding contract.** Plan 04-04 defines a `BackupRepoTarget` adapter (private type) that implements `scheduler.Target`:
  ```go
  type BackupRepoTarget struct{ repo *models.BackupRepo }
  func (b BackupRepoTarget) ID() string       { return b.repo.ID }
  func (b BackupRepoTarget) Schedule() string { if b.repo.Schedule == nil { return "" } ; return *b.repo.Schedule }
  ```
  Pass these to `scheduler.Register(target)` on boot and on every DB write commit via `RegisterRepo`.

- **Shared guard.** `storebackups.Service` constructs `overlap := scheduler.NewOverlapGuard()` once, passes it to `scheduler.NewScheduler(scheduler.WithOverlapGuard(overlap))`, AND acquires it in `RunBackup(ctx, repoID)` via `overlap.TryLock(repoID)` before invoking the executor. On-demand callers that hit a held mutex should return `models.ErrBackupAlreadyRunning` (D-23).

- **JobFn wiring.** `storebackups.Service.jobFn = func(ctx, targetID) error { ... }` calls into the Plan 03 executor (wave 3). The executor returns structured errors; the scheduler logs them but does NOT remove the entry (D-01). Any error disposition (interrupted vs failed) is the executor's responsibility — the scheduler just propagates.

- **Boot sequence.** On `Serve(ctx)`:
  1. `s.store.RecoverInterruptedJobs(ctx)` (D-19 SAFETY-02 helper)
  2. `repos := s.store.ListAllBackupRepos(ctx)`
  3. For each repo with non-empty Schedule: `s.scheduler.Register(BackupRepoTarget{repo})` — WARN+continue on `errors.Is(err, ErrScheduleInvalid)` per D-06
  4. `s.scheduler.Start()` — non-blocking

- **Shutdown.** On `Stop(ctx)`: `s.scheduler.Stop(ctx)`. Cancels all in-flight; cron stops. Do NOT wait for JobFns to drain (D-18).

- **Schedule field on BackupRepo.** Is `*string` (nullable). Empty/nil means "unscheduled" — skip `Register` entirely (do NOT pass an empty string to `scheduler.Register`; it will return `ErrScheduleInvalid`).

## Test-Only Internal Hooks (flag for reviewers)

The package exposes `fire()` as unexported but package-local tests call it directly to bypass wall-clock cron firing. This is the only "test escape hatch" in the package and is documented in the threat model (T-04-02 reviewed: there is no production path that reaches fire() outside the cron.AddFunc callback wired at Register time).

If a future reviewer sees `s.fire(...)` in non-test code, it is a bug — prefer `s.Register(target)` + `s.Start()` and let cron invoke it.

## Threat Model Verification

All 5 STRIDE threats in the plan's `<threat_model>` are mitigated as planned:

| Threat ID | Mitigation | Status |
|-----------|------------|--------|
| T-04-02-01 DoS via malicious cron | `ValidateSchedule` uses `cron.ParseStandard` (finite-state parser, no regex backtracking); rejects at write time | IMPLEMENTED |
| T-04-02-02 Tampering via concurrent ticks | `OverlapGuard.TryLock` serializes per-repo; shared guard with on-demand path | IMPLEMENTED |
| T-04-02-03 EoP via invalid CRON_TZ | `cron.ParseStandard` validates TZ at parse time; unknown TZ → `ErrScheduleInvalid`. Test `cron_tz Not/Real` asserts rejection | IMPLEMENTED |
| T-04-02-04 Repudiation on mid-JobFn crash | JobFn receives `runCtx`; on Stop ctx cancels; Plan 03 executor marks BackupJob `interrupted` | INTERFACE READY (wiring in Plan 04-04) |
| T-04-02-05 Info disclosure via jitter | Accepted — FNV-1a is non-cryptographic; offsets are operator-observable and repo IDs are already in DB | ACCEPTED |

## Next Plan Readiness

- `go build ./...` exits 0 — no regressions in other packages.
- `go test -race -count=3 ./pkg/backup/scheduler/...` exits 0 — all tests pass race detector.
- `go test -run TestScheduler_OverlapUnderLoad -count=5 -race ./pkg/backup/scheduler/` exits 0 — concurrent overlap is deterministic.
- No file in the test suite sleeps for a full minute or blocks on wall-clock cron firing (`grep "time.Sleep(time.Minute)" pkg/backup/scheduler/*_test.go` returns no matches).
- `pkg/backup/scheduler/` is fully self-contained — imports `github.com/robfig/cron/v3`, `hash/fnv`, `sync`, `time`, `context`, `errors`, `fmt`, `github.com/marmos91/dittofs/internal/logger`, `github.com/marmos91/dittofs/pkg/controlplane/models`. No import cycles, no other intra-project dependencies beyond the sentinels layer.
- Plan 04-03 (executor) runs in parallel and touches `pkg/backup/executor/*` only — zero file overlap with this plan (confirmed via `git status` during execution).
- Plan 04-04 (storebackups sub-service) has all the primitives + contracts it needs:
  - Target interface binding contract
  - Scheduler lifecycle (Register/Unregister/Start/Stop)
  - Shared OverlapGuard option
  - ValidateSchedule for Phase-6 boot WARN path
  - PhaseOffset exposed if a future refactor needs it independently

## Self-Check: PASSED

Verified against acceptance criteria:

| Criterion | Status |
|-----------|--------|
| `pkg/backup/scheduler/doc.go` exists with `package scheduler` | PASS — line 11 |
| `pkg/backup/scheduler/jitter.go` contains `func PhaseOffset(repoID string, max time.Duration) time.Duration` | PASS |
| `pkg/backup/scheduler/jitter.go` contains `const DefaultMaxJitter = 5 * time.Minute` | PASS |
| `pkg/backup/scheduler/overlap.go` contains `type OverlapGuard struct` | PASS |
| `pkg/backup/scheduler/overlap.go` contains `func (g *OverlapGuard) TryLock(repoID string) (unlock func(), acquired bool)` | PASS |
| `pkg/backup/scheduler/schedule.go` contains `func ValidateSchedule(expr string) error` and imports `cron "github.com/robfig/cron/v3"` | PASS |
| `grep -n "robfig/cron/v3" go.mod` returns a matching line | PASS — line 29 |
| `go test -race -count=3 ./pkg/backup/scheduler/...` exits 0 | PASS |
| `go test -run TestPhaseOffset_Stable -count=100 ./pkg/backup/scheduler/` exits 0 | PASS |
| `go test -run TestOverlapGuard_Concurrent100 ./pkg/backup/scheduler/` exits 0 | PASS |
| `go test -run TestValidateSchedule ./pkg/backup/scheduler/` exits 0 with >= 5 sub-tests | PASS — 13 sub-tests |
| `pkg/backup/scheduler/scheduler.go` contains `type Scheduler struct`, `type Target interface`, `type JobFn func` | PASS |
| `pkg/backup/scheduler/scheduler.go` contains `func NewScheduler(opts ...Option) *Scheduler` | PASS |
| `Register` calls `ValidateSchedule` | PASS |
| `Unregister(id string)` exists | PASS |
| `Start()` and `Stop(ctx context.Context) error` exist | PASS |
| `s.overlap.TryLock(targetID)` inside fire path | PASS — scheduler.go line 232 |
| `PhaseOffset(id, s.maxJit)` call | PASS — Register() line 139 |
| `go test -race -timeout 60s ./pkg/backup/scheduler/...` exits 0 | PASS |
| `grep -c "t.Run" pkg/backup/scheduler/scheduler_test.go` >= 8 | PASS — 10 |
| No `time.Sleep(time.Minute)` in any `_test.go` file | PASS — 0 matches |
| `go test -run TestScheduler_OverlapUnderLoad -count=5 -race ./pkg/backup/scheduler/` exits 0 | PASS |
| `go build ./...` exits 0 | PASS |

---
*Phase: 04-scheduler-retention*
*Completed: 2026-04-16*
