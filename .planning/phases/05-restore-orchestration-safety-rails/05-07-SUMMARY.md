---
phase: 05-restore-orchestration-safety-rails
plan: 07
subsystem: backup
tags: [restore, orchestration, storebackups, overlap-guard, orphan-sweep, safety-02, rest-02]

requires:
  - phase: 05-01-shares-enabled-column-disable-enable
    provides: shares.Service.ListEnabledSharesForStore (REST-02 gate)
  - phase: 05-02-store-id-engine-persistence
    provides: engine-persistent GetStoreID surfaced by DefaultResolver
  - phase: 05-03-destination-get-manifest-only
    provides: Destination.GetManifestOnly (pre-flight manifest fetch)
  - phase: 05-04-stores-swap-open-orphan-listing
    provides: stores.Service.SwapMetadataStore / OpenMetadataStoreAtPath / ListPostgresRestoreOrphans / DropPostgresSchema
  - phase: 05-05-nfs-bootverifier-atomic-hoist
    provides: writehandlers.BumpBootVerifier() (D-09 hook)
  - phase: 05-06-restore-orchestration-engine
    provides: pkg/backup/restore.Executor + Params + StoresService interface

provides:
  - Service.RunRestore(ctx, repoID, recordID) — single Phase-6 entrypoint
  - REST-02 share-disabled pre-flight gate wired + enforced
  - D-07 per-repo overlap guard shared with RunBackup (mutex contract)
  - D-14 startup orphan sweep for badger + postgres restore temps
  - D-15 / D-16 record selection (default-latest + explicit --from)
  - SAFETY-02 extension verified: restore-kind jobs recovered uniformly
  - RestoreResolver interface (ResolveWithName + ResolveCfg) on top of StoreResolver

affects: [phase-06-cli-rest-api, phase-07-testing]

tech-stack:
  added: []
  patterns:
    - "Canonical sentinels in pkg/backup/restore/errors.go; storebackups aliases them to break import cycle"
    - "Narrow interfaces (MetadataStoreConfigLister, PostgresOrphanLister, SharesService) satisfied directly by composite Store / sub-services — no adapter wrappers"
    - "Functional options with post-construction setter for cross-import-cycle hooks (SetBumpBootVerifier)"

key-files:
  created:
    - pkg/controlplane/runtime/storebackups/restore.go
    - pkg/controlplane/runtime/storebackups/orphan_sweep.go
    - pkg/controlplane/runtime/storebackups/restore_test.go
  modified:
    - pkg/controlplane/runtime/storebackups/service.go
    - pkg/controlplane/runtime/storebackups/target.go
    - pkg/controlplane/runtime/storebackups/errors.go
    - pkg/backup/restore/errors.go
    - pkg/controlplane/runtime/runtime.go

key-decisions:
  - "Canonical Phase-5 sentinels live in pkg/backup/restore/errors.go (break import cycle); storebackups aliases them"
  - "DefaultResolver gained ResolveWithName + ResolveCfg via new RestoreResolver interface; Resolve delegates to ResolveWithName to avoid duplication"
  - "Constructor signature preserved (New unchanged); Phase-5 deps wired via new functional options WithShares/WithStores/WithMetadataConfigs/WithBumpBootVerifier"
  - "SetBumpBootVerifier post-construction setter added so runtime.Runtime can wire writehandlers.BumpBootVerifier without creating an import cycle"
  - "SweepRestoreOrphans skips (with log warning) when Phase-5 deps are unwired — NO silent no-op fallback, NO adapter wrapper"

patterns-established:
  - "Narrow MetadataStoreConfigLister interface satisfied directly by composite store.Store (ListMetadataStores in pkg/controlplane/store/metadata.go:20)"
  - "Compile-time assertion `var _ PostgresOrphanLister = (*stores.Service)(nil)` guards Plan 04's required-interface contract"
  - "Observability seam via destination call-count: tests observe dst.getManifestCalls to assert delegation without mocking the executor itself"

requirements-completed: [REST-01, REST-02, REST-03, REST-04, REST-05, SAFETY-02]

duration: ~45min
completed: 2026-04-16
---

# Phase 05-07: Restore Orchestration Wiring Summary

**storebackups.Service.RunRestore with REST-02 share-disabled pre-flight, D-07 overlap guard, D-14 startup orphan sweep, D-15/D-16 record selection, and SAFETY-02 kind-uniform interrupted-job recovery — single Phase-6 entrypoint.**

## Performance

- **Duration:** ~45 min
- **Started:** 2026-04-16T22:00:00Z (approx)
- **Completed:** 2026-04-16T22:48:30Z
- **Tasks:** 3
- **Files modified:** 8 (5 modified + 3 created)

## Accomplishments

- `Service.RunRestore(ctx, repoID, recordID *string)` wired with every Phase-5 safety rail:
  - D-07 per-repo `OverlapGuard.TryLock` shared with `RunBackup` (same sentinel `ErrBackupAlreadyRunning`)
  - REST-02 pre-flight gate (`ListEnabledSharesForStore` non-empty → `ErrRestorePreconditionFailed`)
  - D-15 default-latest + D-16 explicit `--from` validation (`ErrRecordRepoMismatch`, `ErrRecordNotRestorable`, `ErrNoRestoreCandidate`)
  - Delegates to `restore.Executor.RunRestore` with fully-populated `Params` (TargetStoreKind/ID/Cfg, StoresService, BumpBootVerifier)
- D-14 startup `SweepRestoreOrphans`: per-engine dispatch (badger/postgres/memory), REQUIRED Postgres interface via `stores.Service.ListPostgresRestoreOrphans` (compile-time asserted), grace-window age filter (default 1h), never blocks `Serve`
- New `RestoreResolver` interface (`ResolveWithName` + `ResolveCfg`) extends `StoreResolver` without breaking backward compatibility; `DefaultResolver` satisfies both
- Phase-5 sentinel canonical definitions relocated to `pkg/backup/restore/errors.go` to break the `storebackups → restore` import cycle; `storebackups/errors.go` re-aliases so `errors.Is` still matches across layers
- `runtime.Runtime` composition wires `WithShares(sharesSvc) + WithStores(storesSvc) + WithMetadataConfigs(compositeStore)` — composite Store satisfies `MetadataStoreConfigLister` directly (no adapter wrapper, no noop fallback)
- `Runtime.SetRestoreBumpBootVerifier(fn)` post-construction setter breaks the adapter-package import cycle (`internal/adapter/nfs/v4/handlers` already imports `pkg/controlplane/runtime`)

## Task Commits

1. **Task 1: Service.RunRestore + REST-02/D-07/D-15/D-16 wiring** — `72398880` (feat)
2. **Task 2: Startup orphan sweep for restore temp paths/schemas** — `21a46040` (feat)
3. **Task 3: 12 tests covering pre-flight, selection, orphan sweep, SAFETY-02** — `ea58b366` (test)

## Files Created/Modified

**Created:**
- `pkg/controlplane/runtime/storebackups/restore.go` — `RunRestore` + `selectRestoreRecord` helpers + `SharesService` / `MetadataStoreConfigLister` narrow interfaces
- `pkg/controlplane/runtime/storebackups/orphan_sweep.go` — `SweepRestoreOrphans` + `PostgresOrphanLister` + per-engine helpers
- `pkg/controlplane/runtime/storebackups/restore_test.go` — 12 tests covering all behaviors

**Modified:**
- `pkg/controlplane/runtime/storebackups/service.go` — new Service fields (`shares`, `stores`, `restoreExec`, `bumpBootVerifier`, `metadataConfigs`); functional options (`WithShares`, `WithStores`, `WithBumpBootVerifier`, `WithMetadataConfigs`); `SetBumpBootVerifier` setter; Serve() invokes `SweepRestoreOrphans` after `RecoverInterruptedJobs`; `WithClock` propagates to `restoreExec`
- `pkg/controlplane/runtime/storebackups/target.go` — new `RestoreResolver` interface; `DefaultResolver.ResolveWithName` + `ResolveCfg`; `Resolve` now delegates
- `pkg/controlplane/runtime/storebackups/errors.go` — Phase-5 sentinels re-aliased from `pkg/backup/restore`
- `pkg/backup/restore/errors.go` — canonical Phase-5 sentinel definitions (moved from storebackups to break import cycle)
- `pkg/controlplane/runtime/runtime.go` — composition wires `WithShares`/`WithStores`/`WithMetadataConfigs`; `SetRestoreBumpBootVerifier` method

## Decisions Made

- **Sentinel canonical location: `pkg/backup/restore/errors.go`.** The plan originally stated storebackups was canonical and restore re-exports. Attempting that wiring caused an import cycle (`storebackups → restore`, and `restore/errors.go → storebackups`). Inverting the direction (restore defines, storebackups re-aliases) preserves `errors.Is` semantics at both layers without introducing a third location. No behavior change from a caller's perspective.
- **Backward-compatible constructor signature.** Phase-5 deps are injected via new functional options (`WithShares`, `WithStores`, `WithMetadataConfigs`, `WithBumpBootVerifier`), not positional params. Existing callers (tests, Phase-4 era wiring) compile without changes; `RunRestore` on an unwired Service returns a clear "restore path not wired" error.
- **`SetBumpBootVerifier` post-construction setter.** The runtime composition site (`pkg/controlplane/runtime/runtime.go`) cannot import `internal/adapter/nfs/v4/handlers` — that package already imports `pkg/controlplane/runtime`, so a reverse import would cycle. The setter pattern lets the adapter layer inject the bump hook after both packages are loaded.
- **Narrow interfaces defined in storebackups, not re-exported.** `SharesService` and `MetadataStoreConfigLister` are declared locally in `storebackups/restore.go`. The composite Store / `*shares.Service` / `*stores.Service` satisfy them structurally without adapter wrappers or explicit `var _ Interface = (*impl)(nil)` assertions at the other package (keeping dependency flow one-way).
- **`SweepRestoreOrphans` fail-safely with log warnings**, not silent no-op, when deps are missing. Three guard branches emit distinct WARN lines depending on which dep is unwired; operator sees exactly what's missing rather than "it just didn't run".
- **Postgres age filter skips zero-CreatedAt orphans.** `PostgresRestoreOrphan.CreatedAt` is parsed from the ULID suffix — a non-ULID suffix means "unknown age" and the sweep errs on the side of caution (skip, log debug) rather than dropping an undatable schema.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Resolved import cycle: storebackups ↔ restore**

- **Found during:** Task 1 initial build after adding `import "pkg/backup/restore"` to `service.go`
- **Issue:** `pkg/backup/restore/errors.go` already imported `pkg/controlplane/runtime/storebackups` (Plan 06 re-exported sentinels from storebackups so the restore package's callers could match with errors.Is). Adding `import "pkg/backup/restore"` to storebackups closed the cycle.
- **Fix:** Moved the canonical sentinel definitions (`ErrRestorePreconditionFailed`, `ErrNoRestoreCandidate`, `ErrStoreIDMismatch`, `ErrStoreKindMismatch`, `ErrRecordNotRestorable`, `ErrRecordRepoMismatch`, `ErrManifestVersionUnsupported`) from `pkg/controlplane/runtime/storebackups/errors.go` into `pkg/backup/restore/errors.go`; `storebackups/errors.go` now aliases the restore-package definitions with `var = restore.Err…` identity-preserving assignments. `errors.Is` matching across layers unchanged.
- **Files modified:** `pkg/controlplane/runtime/storebackups/errors.go`, `pkg/backup/restore/errors.go`
- **Verification:** `go build ./...` clean; `go vet ./...` clean; existing tests pass (including Plan 06 tests that rely on `restore.Err*` identities)
- **Committed in:** `72398880` (Task 1 commit)

**2. [Rule 2 - Missing Critical] Runtime wiring for Phase-5 deps required to match plan claims**

- **Found during:** Task 2 (orphan sweep Serve() wiring)
- **Issue:** Plan's must_haves state "Serve() calls SweepRestoreOrphans" and "runtime composition site passes composite Store as MetadataStoreConfigLister" — but `runtime.New` as shipped only called `storebackups.New(s, resolver, DefaultShutdownTimeout)` with no Phase-5 options. Without wiring, RunRestore would fail with "restore path not wired" even in production, and orphan sweep would never run.
- **Fix:** Extended `runtime.New` to compose storebackups with `WithShares(rt.sharesSvc) + WithStores(rt.storesSvc) + WithMetadataConfigs(s)`. Added `Runtime.SetRestoreBumpBootVerifier(fn)` method for adapter-layer bump hook wiring (avoids import cycle).
- **Files modified:** `pkg/controlplane/runtime/runtime.go`
- **Verification:** `go build ./...` clean; full test suite passes.
- **Committed in:** `21a46040` (Task 2 commit)

**3. [Rule 1 - Bug] Method name mismatch: `GetBackupRecordByID` vs `GetBackupRecord`**

- **Found during:** Task 1 (writing `selectRestoreRecord`)
- **Issue:** Plan action text used `GetBackupRecordByID(ctx, *recordID)`; the actual `BackupStore` interface exposes `GetBackupRecord(ctx, id string)` (pkg/controlplane/store/interface.go:393). A literal paste would fail compile.
- **Fix:** Used the real method name `s.store.GetBackupRecord(ctx, *recordID)` in `selectRestoreRecord`.
- **Files modified:** `pkg/controlplane/runtime/storebackups/restore.go`
- **Verification:** Build clean; `TestRunRestore_ByID_RepoMismatch` and `TestRunRestore_ByID_NotRestorable` exercise this exact path.
- **Committed in:** `72398880` (Task 1 commit)

---

**Total deviations:** 3 auto-fixed (1 blocking import cycle, 1 missing critical wiring, 1 bug method-name)
**Impact on plan:** All three essential for the plan's must_haves to hold. No scope creep; no operator-visible behavior change.

## Issues Encountered

None beyond the deviations noted. The Phase-4/Phase-5 earlier plans landed the supporting infrastructure (`stores.SwapMetadataStore`, `shares.ListEnabledSharesForStore`, `restore.Executor`, `BackupJobKindRestore`) in reusable shape — Plan 07's job was assembly, not new primitives.

## DefaultResolver extension rationale

The plan offered three options for surfacing `cfg.Name` + `*MetadataStoreConfig`:
1. Widen the existing `Resolve` signature (breaks all callers)
2. Add a new method (chosen)
3. Add a separate resolver type (scope creep)

Chose option 2: new `RestoreResolver` interface = `StoreResolver` + `ResolveWithName` + `ResolveCfg`. `DefaultResolver.Resolve` now calls `ResolveWithName` internally and drops `storeName` from the return — zero duplication, zero impact on existing callers.

## Postgres orphan-sweep status

Wired via Plan 04's required `stores.Service.ListPostgresRestoreOrphans` + `DropPostgresSchema` interface. The sweep calls them DIRECTLY (no type assertion, no silent skip). Compile-time `var _ PostgresOrphanLister = (*stores.Service)(nil)` guards against future signature drift.

## No `configsForSweep()` helper or noop fallback

Confirmed via grep:

```
grep -c 'noopConfigLister\|configsForSweep' pkg/controlplane/runtime/storebackups/*.go
→ 0 matches
```

If `MetadataStoreConfigLister` is unwired at Service construction, `SweepRestoreOrphans` logs a visible WARN line and skips. The composite Store satisfies the interface directly — no adapter, no wrapper.

## Test outcomes (12 tests in `restore_test.go`)

1. `TestRunRestore_SharesStillEnabled` — ✓ asserts `errors.Is(err, ErrRestorePreconditionFailed)` + share names in message; zero `GetManifestOnly` calls
2. `TestRunRestore_OverlapGuard` — ✓ pre-held overlap lock → `errors.Is(err, ErrBackupAlreadyRunning)`
3. `TestRunRestore_DefaultLatest` — ✓ 3 succeeded records; `GetManifestOnly` called with newest record ID
4. `TestRunRestore_NoSucceededRecords` — ✓ only failed records → `ErrNoRestoreCandidate`
5. `TestRunRestore_ByID_RepoMismatch` — ✓ record from other repo → `ErrRecordRepoMismatch`
6. `TestRunRestore_ByID_NotRestorable` — ✓ failed record → `ErrRecordNotRestorable`
7. `TestRunRestore_HappyPath_DelegatesToExecutor` — ✓ exactly 1 `GetManifestOnly` call, destination Close() on defer; executor aborts at `ErrStoreIDMismatch` (proves delegation)
8. `TestRunRestore_NotWired` — ✓ Service without `WithShares/WithStores` → clear "restore path not wired" error
9. `TestSweepRestoreOrphans_Badger` — ✓ old (3h, grace=1h) removed; young (5m) preserved
10. `TestSweepRestoreOrphans_PostgresCallsRequiredInterface` — ✓ only `public_restore_old` dropped via `DropPostgresSchema("pg-meta", "public_restore_old")`; young schema untouched
11. `TestSweepRestoreOrphans_MemoryNoOp` — ✓ zero drop calls for memory configs
12. `TestSAFETY02_RestoreKindJobsRecovered` — ✓ seeded `BackupJob{Kind: restore, Status: running}`; Serve() → `RecoverInterruptedJobs` → row transitions to Status=interrupted with non-empty Error; zero running restore jobs remain

All tests pass under `-race`: `ok github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups 3.506s`

## Requirements coverage status

| ID | Status | Evidence |
|----|--------|----------|
| REST-01 | ✓ | Service.RunRestore → restore.Executor.RunRestore (full quiesce→side-engine→swap→reopen pipeline) |
| REST-02 | ✓ | Pre-flight `ListEnabledSharesForStore` gate + ErrRestorePreconditionFailed; 3 tests exercise |
| REST-03 | ✓ | Delegated to restore.Executor manifest validation (inherited from Plan 06); TestRunRestore_HappyPath asserts manifest reached executor |
| REST-04 | ✓ | `recordID *string` parameter; default-latest + --from paths both tested |
| REST-05 | ✓ | Every RunRestore creates BackupJob{Kind: restore} via restore.Executor; overlap guard ensures same-repo idempotence |
| SAFETY-02 | ✓ | TestSAFETY02_RestoreKindJobsRecovered directly asserts restore-kind jobs flow through existing RecoverInterruptedJobs path |

## Next Phase Readiness

- Phase-6 CLI/REST can import `storebackups.Service.RunRestore` directly — no further Phase-5 runtime wiring needed
- Phase-6 `dfsctl store metadata restore --repo <id> [--from <backup-id>]` maps cleanly to the Plan 07 signature
- Phase-7 chaos tests can kill the restore mid-stream at well-defined points (manifest fetch, side-engine open, swap) and validate orphan sweep reclaims on next boot
- Observability hooks (D-19) still deferred to Plan 08 — Prometheus/OTel counters on RunRestore are a 3-line addition

## Self-Check: PASSED

**Verified files:**
- FOUND: pkg/controlplane/runtime/storebackups/restore.go
- FOUND: pkg/controlplane/runtime/storebackups/orphan_sweep.go
- FOUND: pkg/controlplane/runtime/storebackups/restore_test.go

**Verified commits:**
- FOUND: 72398880 feat(05-07): wire Service.RunRestore with REST-02 pre-flight
- FOUND: 21a46040 feat(05-07): startup orphan sweep for restore temp paths/schemas
- FOUND: ea58b366 test(05-07): RunRestore pre-flight + orphan sweep + SAFETY-02

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-16*
