---
phase: 05-restore-orchestration-safety-rails
plan: 10
subsystem: infra
tags: [gc, safety-01, backup-hold, blockstore, runtime]

requires:
  - phase: 05-restore-orchestration-safety-rails
    provides: "Plan 08 BackupHold provider + gc.Options.BackupHold field (SAFETY-01 primitives)"
provides:
  - "Runtime.RunBlockGC production entrypoint that attaches storebackups.BackupHold on every gc.CollectGarbage invocation"
  - "shares.Service.DistinctRemoteStores enumeration deduped by configID (not nonClosingRemote wrapper pointer)"
  - "storebackups.Service.BackupStore / DestFactory accessors for backup-hold construction from outside the sub-service"
  - "Runtime.BackupStore / DestFactoryFn accessors that delegate to storebackups.Service"
  - "collectGarbageFn package-level seam for SAFETY-01 invariant testing"
affects: [phase-06, block-gc-cli, block-gc-rest]

tech-stack:
  added: []
  patterns:
    - "Runtime exposes a single production callable path (RunBlockGC); CLI/REST wrappers defer to Phase 6"
    - "Refuse-rather-than-silent-degrade for safety invariants (RunBlockGC returns error when BackupHold unwireable)"
    - "Test seams via package-level function variables (collectGarbageFn) let tests assert invariants without refactoring call sites"

key-files:
  created:
    - "pkg/controlplane/runtime/blockgc.go"
    - "pkg/controlplane/runtime/blockgc_test.go"
  modified:
    - "pkg/controlplane/runtime/runtime.go"
    - "pkg/controlplane/runtime/shares/service.go"
    - "pkg/controlplane/runtime/storebackups/service.go"

key-decisions:
  - "Dedup distinct remote stores by configID (via sharedRemote map), not by nonClosingRemote wrapper pointer — each share wraps its remote individually so pointer dedup would give per-share, not per-underlying-remote"
  - "Test seam via package-level collectGarbageFn variable (not Options-injected) — keeps RunBlockGC signature clean; restore via t.Cleanup"
  - "RunBlockGC returns error (not nil) when BackupHold wiring unavailable — machine-enforces SAFETY-01 rather than silently under-holding"
  - "SetBackupHoldWiringForTest constructs a lightweight storebackups.Service with nil resolver — lets tests exercise RunBlockGC's gate without standing up scheduler/executor"

patterns-established:
  - "Distinct remote-store enumeration: shares.Service owns the dedup (via its sharedRemote ref-count bookkeeping), not the caller. Future GC/migration/backup paths reuse DistinctRemoteStores()."
  - "Runtime-level callable + Phase 6 wrapper: RunBlockGC is the single production entrypoint; any future CLI/REST/cron trigger must go through it and inherits the BackupHold wiring automatically."

requirements-completed: [SAFETY-01]

duration: 7min
completed: 2026-04-17
---

# Phase 5 Plan 10: Runtime.RunBlockGC Closes SAFETY-01 End-to-End Summary

**Runtime.RunBlockGC is the production entrypoint that attaches storebackups.BackupHold on every gc.CollectGarbage call, refusing to run without backup-hold wiring — completing the SAFETY-01 invariant at the runtime level.**

## Performance

- **Duration:** ~7 min
- **Started:** 2026-04-16T23:19:12Z
- **Completed:** 2026-04-16T23:25:40Z
- **Tasks:** 2 (both TDD)
- **Files modified:** 3 (runtime.go, shares/service.go, storebackups/service.go)
- **Files created:** 2 (blockgc.go, blockgc_test.go)

## Accomplishments

- `Runtime.RunBlockGC(ctx, sharePrefix, dryRun) (*gc.Stats, error)` is the single runtime-level callable for block-store GC. Every invocation attaches `storebackups.BackupHold` via `gc.Options.BackupHold` — SAFETY-01 is machine-enforced at the entrypoint.
- Refusal semantics: returns an error containing `"backup-hold wiring unavailable"` when `BackupStore` or `destFactory` is missing. No silent under-holding possible.
- Remote-store dedup by `configID` via `shares.Service.DistinctRemoteStores()`. Shares sharing an S3 bucket trigger exactly one GC invocation, not one-per-share.
- `collectGarbageFn` package-level seam lets tests intercept `gc.CollectGarbage` and assert the SAFETY-01 invariant on captured `*gc.Options`.
- All four plan-mandated tests pass: `AttachesBackupHold`, `MissingBackupStore_ReturnsError`, `DedupesSharedRemoteStores`, `DryRunPropagates`.

## Task Commits

Each task was committed atomically:

1. **Task 2 (RED): failing tests for Runtime.RunBlockGC** — `ca0b6bc5` (test)
2. **Task 1 + Task 2 (GREEN): Runtime.RunBlockGC implementation** — `0c0e1815` (feat)

Tasks 1 and 2 were implemented together in the GREEN commit because the GREEN for Task 2 is the implementation of Task 1 (symmetric TDD where the tests exercise the new method directly).

## Accessor names used (plan output requirements)

- **Per-share BlockStore accessor:** `share.BlockStore` (field on `shares.Share` struct). Matched the plan's expectation.
- **Distinct remote enumeration:** Added `shares.Service.DistinctRemoteStores() []RemoteStoreEntry`. Pointer-dedup on `nonClosingRemote` wrappers would be incorrect because each share creates its own wrapper; dedup is done on `remoteConfigID` via the existing `sharedRemote` ref-count map.
- **Runtime BackupStore accessor:** Added `Runtime.BackupStore() store.BackupStore`. Delegates to `storebackups.Service.BackupStore()`, which is newly exposed.
- **Runtime DestFactoryFn accessor:** Added `Runtime.DestFactoryFn() storebackups.DestinationFactoryFn`. Delegates to `storebackups.Service.DestFactory()`.
- **Remote store direct accessor on engine.BlockStore:** NOT added — the `shares.Service.DistinctRemoteStores` approach bypasses the need, since it reads from the `sharedRemote` map that tracks the underlying stores (not the per-share wrappers). `engine.BlockStore.RemoteForTesting` remains as-is.

## Files Created/Modified

- `pkg/controlplane/runtime/blockgc.go` — Runtime.RunBlockGC + collectGarbageFn seam + test helpers (SetBackupHoldWiringForTest, setShareRemoteForTest)
- `pkg/controlplane/runtime/blockgc_test.go` — 4 tests asserting SAFETY-01 contract
- `pkg/controlplane/runtime/runtime.go` — BackupStore + DestFactoryFn accessors
- `pkg/controlplane/runtime/shares/service.go` — DistinctRemoteStores + RemoteStoreEntry + SetShareRemoteForTest
- `pkg/controlplane/runtime/storebackups/service.go` — BackupStore + DestFactory accessors

## Decisions Made

- **Dedup by configID, not pointer:** shares.Service already tracks remote stores via a ref-counted `sharedRemote` map keyed by `remoteConfigID`. That bookkeeping is authoritative; `DistinctRemoteStores()` iterates it directly. Per-share dedup by the `nonClosingRemote` wrapper would give incorrect results (one GC per share when they share an S3 bucket).
- **Test seam on CollectGarbage, not Options:** kept `RunBlockGC`'s signature clean by using a package-level `collectGarbageFn` variable rather than plumbing a gc function into `gc.Options`. Restore via `t.Cleanup`.
- **Refuse rather than degrade:** SAFETY-01 is a correctness-critical invariant. `RunBlockGC` returns `fmt.Errorf("RunBlockGC refused: backup-hold wiring unavailable ... — SAFETY-01")` when the runtime lacks `BackupStore` or `destFactory`. Never runs GC without a hold.
- **Lightweight test wiring via `SetBackupHoldWiringForTest`:** constructs a real `storebackups.Service` with nil resolver so tests exercise the production accessor chain (Runtime.BackupStore → storebackups.Service.BackupStore → stored field). No mocks at the accessor boundary.

## Deviations from Plan

None substantive. The plan's suggested structure (remote accessor on engine.BlockStore + dedup by pointer identity) was adjusted to use `shares.Service.DistinctRemoteStores` + dedup by configID, because the per-share `nonClosingRemote` wrapper makes pointer-based dedup incorrect for ref-counted shared remotes. The adjustment is equivalent in contract (one GC invocation per distinct underlying remote) and strictly more accurate.

The plan's guidance explicitly allows this: "If the remote-store accessor on *engine.BlockStore is not named `.Remote()`, use the real name". The accessor naming was a stand-in; the actual design problem was wrapper-dedup, solved at the shares.Service layer.

## Auth Gates

None — no external authentication involved.

## Verification

- `go build ./...` — clean
- `go vet ./pkg/controlplane/runtime/...` — clean
- `go test ./pkg/controlplane/runtime/... -count=1 -race -run 'TestRunBlockGC'` — 4/4 pass
- `go test ./pkg/controlplane/runtime/... ./pkg/blockstore/gc/...` — all pass (no regressions)
- Existing Plan 08 tests still pass with nil `BackupHold` option (backward compatible)

## Scope Boundary Honored

- **No files created under `pkg/controlplane/api/`** — confirmed via `ls pkg/controlplane/api/` (only pre-existing files).
- **No CLI command added** — `cmd/dfsctl/commands/` untouched.
- **No scheduled/cron GC** — Phase 5 limit honored; scheduler-GC integration is Phase 7+ material.
- **Operator-facing trigger deferred to Phase 6** — Phase 6 CLI/REST wrapper will call `Runtime.RunBlockGC` and inherit the SAFETY-01 wiring automatically. The runtime callable is the machine-enforcement point for the invariant.

## Issues Encountered

- A flaky pre-existing test (`TestAPIServer_Lifecycle` at `pkg/controlplane/api/server_test.go:77`) fails on the current environment because port 18080 is held by a Docker container. The test uses a hardcoded port and does not free it reliably. This failure is environmental and unrelated to Plan 10 — confirmed via `lsof -i :18080` showing `com.docke` (Docker) holding the port. No code changes made.

## SAFETY-01 Runtime-Level Trace (confirmed complete)

```
Runtime.RunBlockGC
  ├── Runtime.BackupStore → storebackups.Service.BackupStore → store.BackupStore
  ├── Runtime.DestFactoryFn → storebackups.Service.DestFactory → DestinationFactoryFn
  ├── storebackups.NewBackupHold(backupStore, destFactory) → *BackupHold
  ├── shares.Service.DistinctRemoteStores() → []RemoteStoreEntry (deduped)
  └── collectGarbageFn(ctx, entry.Store, r, &gc.Options{BackupHold: hold, ...})
        → gc.CollectGarbage respects BackupHold → held PayloadIDs preserved
```

The invariant is now machine-enforced: any caller wanting to run production block-GC MUST go through `RunBlockGC`; the entrypoint itself refuses to run without BackupHold wiring, so silent under-holding is impossible from the runtime callable path.

## Next Phase Readiness

- **Phase 6 can layer CLI + REST on top of `Runtime.RunBlockGC` with confidence.** Phase 6 owns admin-role middleware, RFC 7807 error emission, HTTP query-param parsing. The runtime callable already enforces SAFETY-01; Phase 6 inherits this for free.
- **Future scheduler for block-GC** (Phase 7+) must call `Runtime.RunBlockGC`, not `gc.CollectGarbage` directly. The refusal semantics guarantee the SAFETY-01 invariant even if the scheduler runs before the backup pipeline is fully wired (e.g., on cold boot).
- **No blockers** — Plan 10 closes Phase 5's SAFETY-01 requirement.

## Self-Check: PASSED

- Created files exist:
  - `pkg/controlplane/runtime/blockgc.go`: FOUND
  - `pkg/controlplane/runtime/blockgc_test.go`: FOUND
- Commits exist:
  - `ca0b6bc5` (test RED): FOUND
  - `0c0e1815` (feat GREEN): FOUND
- All acceptance criteria from PLAN met:
  - `collectGarbageFn` occurrences in blockgc.go: 3 (≥ 2 required) ✓
  - `BackupHold: hold` occurrences in blockgc.go: 1 ✓
  - `storebackups.NewBackupHold` occurrences: 1 ✓
  - `collectGarbageFn` in test file: 6 (≥ 1 required) ✓
  - No files under `pkg/controlplane/api/` touched ✓
  - `go build ./...` clean ✓
  - `go vet ./pkg/controlplane/runtime/...` clean ✓
  - 4/4 TestRunBlockGC_* tests pass ✓

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-16*
