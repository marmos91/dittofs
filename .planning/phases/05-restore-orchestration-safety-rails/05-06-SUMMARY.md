---
phase: 05
plan: 06
subsystem: backup-restore
tags: [restore, executor, side-engine, atomic-swap, d-05, sentinels, backupable]
requires:
  - phase: 01 (foundations)
    provides: manifest v1 (StoreID, StoreKind, SHA256, PayloadIDSet), BackupJobKindRestore, BackupStatus enum, SAFETY-02 RecoverInterruptedJobs primitive
  - phase: 02 (per-engine backup drivers)
    provides: Backupable.Restore destination-must-be-empty invariant (D-06)
  - phase: 03 (destination drivers + encryption)
    provides: Destination.GetManifestOnly, Destination.GetBackup with streaming SHA-256 verify (ErrSHA256Mismatch on Close)
  - phase: 04 (scheduler + retention)
    provides: storebackups.Service skeleton, Phase-4 sentinels, BackupStore composite interface, executor.Executor RunBackup pattern
  - phase: 05 (restore orchestration) plans 01-05
    provides: engine-persistent store_id (Plan 02), GetManifestOnly on destinations (Plan 03), stores.Service.SwapMetadataStore / OpenMetadataStoreAtPath / DropPostgresSchema (Plan 04), atomic BumpBootVerifier (Plan 05)
provides:
  - pkg/backup/restore.Executor with RunRestore(Params) implementing the D-05 13-step sequence
  - pkg/backup/restore.OpenFreshEngineAtTemp / CleanupTempBacking / CommitSwap helpers
  - pkg/backup/restore error sentinels (7 Phase-5 errors + 2 local) with errors.Is compatibility across storebackups → restore layers
  - BackupStore.ListSucceededRecordsByRepo (succeeded INCLUDING pinned, newest-first) for D-15 restore selection and D-11 block-GC hold union
  - 7 Phase-5 storebackups error sentinels (canonical definitions for Phase 6 CLI/REST)
affects:
  - Plan 07 (storebackups.Service.RunRestore wrapper + overlap guard + share-disabled pre-flight)
  - Plan 08 (block-GC hold provider via ListSucceededRecordsByRepo)
  - Phase 6 (CLI/REST surface mapping Phase-5 sentinels to 400/409)

tech-stack:
  added: []
  patterns:
    - Side-engine restore at temp path/schema, atomic registry swap (D-05)
    - Two-layer sentinel wrap with `var X = storebackups.X` aliasing (errors.Is preserved across package boundary, D-26)
    - Narrow JobStore + StoresService interfaces matching real store method names verbatim (no adapters)
    - Named-return err + terminal-state defer for job status classification (D-17)
    - cleanupTemp flag flipped post-swap to skip temp-wipe defer on success

key-files:
  created:
    - pkg/backup/restore/errors.go
    - pkg/backup/restore/fresh_store.go
    - pkg/backup/restore/swap.go
    - pkg/backup/restore/restore.go
    - pkg/backup/restore/restore_test.go
  modified:
    - pkg/controlplane/store/interface.go (ListSucceededRecordsByRepo added to BackupStore)
    - pkg/controlplane/store/backup.go (GORM implementation)
    - pkg/controlplane/store/backup_test.go (integration test)
    - pkg/controlplane/runtime/storebackups/errors.go (7 Phase-5 sentinels appended)

key-decisions:
  - "JobStore interface uses GetBackupRecord (not GetBackupRecordByID) to match pkg/controlplane/store.BackupStore method name verbatim — real *GORMStore satisfies restore.JobStore without adapter shims. Plan's original name renamed."
  - "RenamePostgresSchema treated as extension-point: CommitSwap calls it via interface assertion; if stores.Service does not implement, returns a clear error so Plan 07's orphan sweep (or operator) reclaims the temp schema. Not blocked by missing Plan 04 primitive."
  - "Terminal-state UpdateBackupJob uses context.Background() (not the caller's ctx) so a cancelled parent ctx still persists the terminal row — SAFETY-02 visibility guarantee."
  - "Test strategy: real memory.MemoryMetadataStore embedded in a tolerantMemStore wrapper that overrides Backup/Restore to no-ops. Avoids maintaining a 31-method fake while bypassing the envelope-format dependency for orchestration-focused tests."

patterns-established:
  - "Restore orchestrator pattern: fresh engine + atomic swap + post-swap best-effort cleanup. Plan 07 wraps this behind per-repo mutex + share-disabled pre-flight."
  - "Pre-swap validation gates (manifest version → store_kind → store_id → SHA-256) ordered cheapest-first, all before OpenFreshEngineAtTemp so failures never touch temp backing."

requirements-completed: [REST-01, REST-03, REST-04, REST-05, SAFETY-02]

duration: ~10 min
completed: 2026-04-17
---

# Phase 5 Plan 06: Restore Orchestration Engine Summary

**`pkg/backup/restore.Executor.RunRestore` implements the D-05 13-step side-engine restore sequence with pre-swap validation gates, atomic `stores.Service.SwapMetadataStore` commit, post-swap best-effort cleanup, and D-17 ctx-cancel → interrupted job classification.**

## Performance

- **Duration:** ~10 min
- **Started:** 2026-04-17T00:22:33+02:00
- **Completed:** 2026-04-17T00:32:28+02:00
- **Tasks:** 3
- **Files modified:** 5 created, 3 modified

## Accomplishments

- `pkg/backup/restore` package with `Executor.RunRestore` implementing D-05 steps 3-13 verbatim: pre-flight manifest validation (version, store_kind, store_id, SHA-256), OpenFreshEngineAtTemp, Backupable.Restore into fresh engine, streaming SHA-256 verify on Close, SwapMetadataStore commit point, post-swap CommitSwap (close old, rename temp), BumpBootVerifier.
- 7 Phase-5 restore sentinels defined canonically in `pkg/controlplane/runtime/storebackups/errors.go` and re-exported as aliases in `pkg/backup/restore/errors.go` — `errors.Is` matches across both layers.
- `BackupStore.ListSucceededRecordsByRepo` added to interface + GORM implementation: returns succeeded records INCLUDING pinned, newest-first. Phase 5 consumes via two call sites (D-15 default-latest selection and D-11 block-GC hold union).
- Nine unit tests drive the executor through the D-05 branches with fakes: happy path, three validation-gate rejections (store_id, store_kind, manifest_version), empty SHA-256, SHA-256 mismatch, ctx-canceled → interrupted, post-swap cleanup error (restore still succeeds), and boot-verifier call-count.

## Task Commits

1. **Task 1: BackupStore.ListSucceededRecordsByRepo (interface + impl + integration test)** - `a04ee794` (feat)
2. **Task 2: 7 Phase-5 sentinels + pkg/backup/restore package (errors, fresh_store, swap, restore)** - `6675fe59` (feat)
3. **Task 3: Unit tests for Executor.RunRestore** - `5c83e35d` (test)

_Plan metadata commit to follow with SUMMARY.md + STATE.md + ROADMAP.md updates._

## Files Created/Modified

### Created
- `pkg/backup/restore/errors.go` — 7 re-exported Phase-5 sentinels + 2 package-local (`ErrRestoreAborted`, `ErrFreshEngineExists`). Re-exports use `var X = storebackups.X` so `errors.Is(restore.ErrStoreIDMismatch, storebackups.ErrStoreIDMismatch) == true`.
- `pkg/backup/restore/fresh_store.go` — `StoresService` interface, `TempIdentity`, `OpenFreshEngineAtTemp` (memory / badger / postgres dispatch), `CleanupTempBacking`. Badger uses `<origPath>.restore-<ulid>`, Postgres uses `<origSchema>_restore_<lowercase-ulid>`.
- `pkg/backup/restore/swap.go` — `CommitSwap` (close-old + remove-old + rename-temp per kind). Includes `renamePostgresSchema` extension-point shim that falls through to `stores.Service.RenamePostgresSchema` via interface assertion.
- `pkg/backup/restore/restore.go` — `Executor`, `JobStore`, `Params`, `New`, `SetClock`, `RunRestore`. `RunRestore` is 170 lines covering D-05 steps 3-13 with a named-return `err` driving the terminal-state defer.
- `pkg/backup/restore/restore_test.go` — 9 tests exercising every D-05 branch. Uses `memory.MemoryMetadataStore` embedded in a `tolerantMemStore` wrapper so tests focus on orchestration, not envelope format.

### Modified
- `pkg/controlplane/store/interface.go` — `ListSucceededRecordsByRepo` added to `BackupStore` interface with doc contrasting against `ListSucceededRecordsForRetention`.
- `pkg/controlplane/store/backup.go` — GORM implementation (WHERE status=succeeded, ORDER BY created_at DESC, no pinned filter).
- `pkg/controlplane/store/backup_test.go` — `TestListSucceededRecordsByRepo` integration test covering ordering (newest-first), pinned inclusion, and empty-repo edge case.
- `pkg/controlplane/runtime/storebackups/errors.go` — 7 new sentinels appended in a dedicated `var (...)` block with `errors` import added.

## Sentinel Additions Map (storebackups/errors.go)

| Sentinel | Line | 409/400 |
|----------|------|---------|
| `ErrRestorePreconditionFailed` | 29 | 409 |
| `ErrNoRestoreCandidate` | 33 | 409 |
| `ErrStoreIDMismatch` | 38 | 400 |
| `ErrStoreKindMismatch` | 43 | 400 |
| `ErrRecordNotRestorable` | 48 | 409 |
| `ErrRecordRepoMismatch` | 53 | 400 |
| `ErrManifestVersionUnsupported` | 58 | 400 |

## RenamePostgresSchema Status

**Deferred, not implemented in this plan.** `swap.go:CommitSwap` calls `renamePostgresSchema` which uses an interface assertion (`stores.(interface{ RenamePostgresSchema(...) })`). If `stores.Service` exposes the method, it's called; otherwise `CommitSwap` returns a clear error and the restored data lives under the temp schema name until a follow-up manual rename or Plan 07's orphan sweep reclaims. Badger and memory kinds are fully handled; Postgres rename is scope-flexible per plan guidance.

This is the plan-level "add RenamePostgresSchema to stores.Service if needed" callout (Task 2 step 4 note). We chose to defer because:
1. Plan 04 did not ship a rename primitive.
2. Memory + Badger (the primary test-coverage targets) work fully.
3. The unit tests pass with the current dispatch logic.
4. Any gap Plan 07 surfaces can be fixed inline there without touching the restore engine.

## Test Outcomes

All 9 tests pass with `-race` clean:

| Test | Asserts |
|------|---------|
| `TestRunRestore_HappyPath` | nil err, job=succeeded, swapCount=1, bumpCalls=1, Backupable.Restore called on fresh engine |
| `TestRunRestore_StoreIDMismatch` | `errors.Is(err, ErrStoreIDMismatch)`, job=failed, openCount=0, swapCount=0 |
| `TestRunRestore_StoreKindMismatch` | `errors.Is(err, ErrStoreKindMismatch)`, job=failed |
| `TestRunRestore_ManifestVersionUnsupported` | `errors.Is(err, ErrManifestVersionUnsupported)`, job=failed |
| `TestRunRestore_EmptyManifestSHA256` | err non-nil, job=failed, swapCount=0 |
| `TestRunRestore_SHA256Mismatch` | `errors.Is(err, destination.ErrSHA256Mismatch)`, job=failed |
| `TestRunRestore_CtxCanceled` | `errors.Is(err, context.Canceled)`, job=**interrupted** (D-17) |
| `TestRunRestore_PostSwapCleanupError` | **nil err**, job=succeeded, bumpCalls=1 (post-swap close failure logged, NOT fatal) |
| `TestRunRestore_BumpBootVerifierCalled` | bumpCalls=1 on happy path; nil BumpBootVerifier does not panic |

## Decisions Made

See frontmatter `key-decisions`. Summary:

- **JobStore method name is `GetBackupRecord`, not `GetBackupRecordByID`** — matches the real `*GORMStore` method exactly so Plan 07 can pass the real store as-is. The plan's original name was a drafting inconsistency; the real store (pre-existing from Phase 1) has always used `GetBackupRecord`.
- **`RenamePostgresSchema` left as an interface-assertion extension point** rather than extending `stores.Service` here. Scope-flexible per plan guidance; Plan 07 can add it if its wiring surfaces the need.
- **Terminal-state `UpdateBackupJob` uses `context.Background()`** so SAFETY-02 row lands even after parent ctx cancellation. Matches the spirit of Phase-4 executor but is explicit here rather than implicit.
- **Tests embed a real memory store** (`memory.MemoryMetadataStore`) inside a `tolerantMemStore` wrapper that overrides `Backup`/`Restore`. Avoids writing a 31-method metadata.MetadataStore fake and bypasses the envelope-format contract (memory's Restore validates magic+CRC; tests are orchestration-focused).

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] JobStore method name corrected to `GetBackupRecord`**
- **Found during:** Task 2 (writing restore.go)
- **Issue:** Plan specified `GetBackupRecordByID(ctx, id)` in the JobStore interface. The real `pkg/controlplane/store.BackupStore` exposes `GetBackupRecord(ctx, id)`. Using the plan's name would prevent `*GORMStore` from satisfying `restore.JobStore` without an adapter shim — unnecessary friction for Plan 07.
- **Fix:** Renamed to `GetBackupRecord` in restore.go, added doc comment explaining the name matches the real BackupStore method verbatim.
- **Files modified:** `pkg/backup/restore/restore.go`
- **Verification:** Build clean; the method is declared for Plan 07's satisfaction (RunRestore itself does not invoke it — record selection happens in Plan 07 before RunRestore is called).
- **Committed in:** `6675fe59`

**2. [Rule 2 - Missing Critical] Terminal-state UpdateBackupJob uses context.Background()**
- **Found during:** Task 2 (reviewing executor.go pattern for RunRestore defer)
- **Issue:** If the caller's ctx is cancelled (shutdown, operator abort), the defer's `UpdateBackupJob(ctx, ...)` would itself fail — the SAFETY-02 terminal row would never land, leaving the job stuck in `running` until next boot's `RecoverInterruptedJobs` sweep. D-17's "interrupted" classification only reaches the DB if the update itself succeeds.
- **Fix:** The defer uses `context.Background()` for `UpdateBackupJob` calls so the terminal row always lands regardless of parent ctx state. Parent ctx is still used for manifest fetch, stream, restore, and swap (those should abort on cancel).
- **Files modified:** `pkg/backup/restore/restore.go`
- **Verification:** `TestRunRestore_CtxCanceled` asserts final job status = `interrupted` (not the initial `running`), proving the UpdateBackupJob landed despite the ctx cancellation.
- **Committed in:** `6675fe59`

**3. [Rule 4 — deferred] RenamePostgresSchema treated as extension point**
- **Found during:** Task 2 (writing swap.go)
- **Issue:** The plan's note acknowledged Plan 04 might not have shipped `RenamePostgresSchema`. It hasn't (verified at `pkg/controlplane/runtime/stores/service.go`).
- **Choice:** Rather than extend `stores.Service` here (architectural change outside plan scope), implemented `renamePostgresSchema` as a shim that uses an interface assertion to call the method if present, or returns a clear error otherwise. This keeps the restore engine usable for memory + badger (the primary test targets) without blocking on Plan 04.
- **Files modified:** `pkg/backup/restore/swap.go`
- **Verification:** Build clean; Postgres path documented as deferred; memory + badger fully exercised in tests.
- **Committed in:** `6675fe59`

---

**Total deviations:** 3 auto-fixes (1 bug — wrong method name; 1 missing critical — ctx.Background for terminal update; 1 deferred — RenamePostgresSchema extension point)
**Impact on plan:** All three auto-fixes improve correctness without scope creep. Rename corrects a drafting inconsistency. The ctx.Background change prevents a subtle SAFETY-02 violation. The RenamePostgresSchema extension point keeps Phase 5 moving while flagging a clean hand-off to Plan 07.

## Issues Encountered

None — every task went through test-first and passed on the first compile. Initial draft of `restore_test.go` attempted to build a from-scratch metadata.MetadataStore fake; discovered the interface has 31 methods and pivoted to embedding `memory.MemoryMetadataStore` inside a `tolerantMemStore` wrapper that overrides just `Backup`/`Restore`. This is documented in the test file's comments.

## Self-Check: PASSED

- `pkg/backup/restore/errors.go` exists with 7 re-exports + 2 local sentinels — FOUND
- `pkg/backup/restore/fresh_store.go` exports `StoresService`, `TempIdentity`, `OpenFreshEngineAtTemp`, `CleanupTempBacking` — FOUND
- `pkg/backup/restore/swap.go` exports `CommitSwap` — FOUND
- `pkg/backup/restore/restore.go` exports `Executor`, `JobStore`, `Params`, `New`, `SetClock`, `RunRestore` — FOUND
- `pkg/controlplane/runtime/storebackups/errors.go` contains `ErrRestorePreconditionFailed`, `ErrNoRestoreCandidate`, `ErrStoreIDMismatch`, `ErrStoreKindMismatch`, `ErrRecordNotRestorable`, `ErrRecordRepoMismatch`, `ErrManifestVersionUnsupported` — FOUND (7/7)
- Commits `a04ee794`, `6675fe59`, `5c83e35d` — FOUND in `git log`
- `go build ./... && go vet ./...` — PASSED
- `go test ./pkg/backup/restore/... -race -count=1` — 9 tests PASSED
- `go test -tags=integration ./pkg/controlplane/store/... -run TestListSucceededRecordsByRepo -count=1` — PASSED

## Next Plan Readiness

Plan 07 (storebackups.Service.RunRestore wrapper) is unblocked:
- Narrow `restore.JobStore` and `restore.StoresService` interfaces match the real `*GORMStore` and `stores.Service` method signatures — direct composition with no adapter layer needed.
- 7 Phase-5 sentinels are canonical in storebackups; Phase-6 CLI can import them with a single package reference.
- `ListSucceededRecordsByRepo` is wired for D-15 (restore selection) and D-11 (block-GC hold — Plan 08).

The only open hand-off is the `RenamePostgresSchema` extension point. Plan 07 should decide whether to:
(a) extend `stores.Service` with the method (clean but requires Plan 04 revisit), or
(b) document the orphan-schema path so operators know temp schemas survive until the sweep reclaims them.

Recommendation: (a) — add ALTER SCHEMA RENAME alongside DropPostgresSchema; trivial to implement, avoids the orphan-tolerance hand-wave.

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-17*
