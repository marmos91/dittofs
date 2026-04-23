---
phase: 08-pre-refactor-cleanup-a0
plan: 08
subsystem: controlplane-runtime
tags: [backup-removal, runtime, gc, storebackups, TD-03, D-21, D-30-step-3]
requires:
  - 08-07 (PR-B commit 2 — backup REST + CLI + apiclient removed)
provides:
  - "Runtime.go with zero storebackups references (import, field, builder, SetRestoreBumpBootVerifier, startup block, and 8 delegation methods all gone)"
  - "blockgc.go with SAFETY-01 gate fully removed; gc.CollectGarbage called without BackupHold"
  - "blockgc_test.go pruned of storebackups import and SetBackupHoldWiringForTest call"
  - "storebackups/ package un-imported — ready for deletion in plan 08-09"
affects:
  - pkg/controlplane/runtime/runtime.go
  - pkg/controlplane/runtime/blockgc.go
  - pkg/controlplane/runtime/blockgc_test.go
tech-stack:
  added: []
  patterns:
    - "Runtime composition layer simplified (one sub-service removed; remaining sub-services intact per CLAUDE.md invariant 1)"
key-files:
  created: []
  modified:
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/blockgc.go
    - pkg/controlplane/runtime/blockgc_test.go
decisions:
  - "GC runs without any BackupHold option — the gc.Options.BackupHold field remains in the gc package (out of scope here; addressed in PR-C) but is never set by any runtime-owned caller."
  - "Removed all 3 SAFETY-01 tests (TestRunBlockGC_AttachesBackupHold, TestRunBlockGC_MissingBackupStore_ReturnsError) as their assertions no longer make sense; added TestRunBlockGC_NoRemoteShares to cover the zero-share early-return that SAFETY-01's refusal previously masked."
  - "Retained TestRunBlockGC_DedupesSharedRemoteStores and TestRunBlockGC_DryRunPropagates (neither depends on backup-hold wiring); stripped withBackupHold parameter from newRuntimeForGC helper."
metrics:
  started: "2026-04-23T17:52:22Z"
  completed: "2026-04-23T17:56:00Z"
  duration_minutes: 4
  commits: 1
  files_changed: 3
  lines_added: 35
  lines_deleted: 324
---

# Phase 08 Plan 08: Drop storebackups Runtime wiring + SAFETY-01 gate + delegation methods Summary

Removed the entire storebackups surface from Runtime composition — one import, one struct field, the builder block, `SetRestoreBumpBootVerifier`, the `Serve` startup/shutdown block, and all 8 delegation methods — plus the SAFETY-01 `BackupHold` gate in `blockgc.go` and its test-only helper. Package `pkg/controlplane/runtime/storebackups/` still exists but is now un-imported from runtime (tests inside storebackups still build/run on their own).

## Commit

- `79503576` — `runtime: drop storebackups wiring + SAFETY-01 gate + delegation methods (TD-03)` (signed)

## Deletions by file

### pkg/controlplane/runtime/runtime.go

Per CONTEXT lines 417-498 + 132-145 + 395-400 + 120-125 + 66 + 18.

| Sub-step | What | Original line range |
|---|---|---|
| (a) | Import `"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"` | 18 |
| (a') | Import `"fmt"` — removed because no longer referenced after delegation-method deletion | 5 |
| (a') | Import `"github.com/marmos91/dittofs/internal/logger"` — removed because no longer referenced after startup-block deletion | 9 |
| (b) | Struct field `storeBackupsSvc *storebackups.Service` | 66 |
| (c) | Builder block: `resolver := storebackups.NewDefaultResolver(...)` + 5-option `storebackups.New(...)` assignment (WithShares/WithStores/WithMetadataConfigs) | 120-126 |
| (c') | Doc comments for storebackups wiring inside `New(...)` `if s != nil` block | 106-119 |
| (d) | `SetRestoreBumpBootVerifier(fn func())` method + doc comment | 132-145 |
| (e) | Serve/Start startup+defer block `if r.storeBackupsSvc != nil { Serve(ctx); defer Stop(...) }` + doc comments | 394-411 |
| (g) | `RegisterBackupRepo`, `UnregisterBackupRepo`, `UpdateBackupRepo`, `RunBackup`, `ValidateBackupSchedule`, `BackupStore()`, `DestFactoryFn()`, `StoreBackupsService()` — all 8 delegation methods + doc comments + "--- Store Backup Management ---" banner | 417-498 |

### pkg/controlplane/runtime/blockgc.go

Per CONTEXT lines 42-66 + 121-139.

| Sub-step | What | Original line range |
|---|---|---|
| (h) | SAFETY-01 gate: `backupStore := r.BackupStore()` / `destFactory := r.DestFactoryFn()` + nil-check refusal error | 42-51 |
| (h) | `storebackups.NewBackupHold(...)` construction | 52 |
| (h) | `hold.HeldPayloadIDs(ctx)` eager resolution + error refusal | 54-64 |
| (h) | `resolvedHold := gc.StaticBackupHold(held)` | 65 |
| (h) | `BackupHold: resolvedHold,` field inside `gc.Options{...}` literal at the `gc.CollectGarbage` call site | 82 |
| (i) | `SetBackupHoldWiringForTest(...)` test-only helper + doc comment | 121-139 |
| (j) | Import `"fmt"` — removed (no longer needed after refusal-error deletion) | 5 |
| (j) | Import `"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"` | 10 |
| (j) | Import `"github.com/marmos91/dittofs/pkg/controlplane/store"` — removed (only used by `SetBackupHoldWiringForTest` signature) | 11 |
| (j) | Updated doc comment on `RunBlockGC` to drop the SAFETY-01 paragraph (now describes plain GC enumeration) | 14-40 |

### pkg/controlplane/runtime/blockgc_test.go

Per CONTEXT lines 15 + 129.

| Sub-step | What | Original line range |
|---|---|---|
| — | `storebackups` import | 15 |
| — | `SetBackupHoldWiringForTest(...)` call inside `newRuntimeForGC` | 128-130 |
| — | `withBackupHold bool` parameter on `newRuntimeForGC` — dropped (no call sites need it anymore) | 108 |
| — | `gcBlockBackupStore` fake `store.BackupStore` (used only by `SetBackupHoldWiringForTest`) | 47-56 |
| — | `destFactoryNoop` helper + `fakeGCDestination` fake (used only by `SetBackupHoldWiringForTest`) | 58-83 |
| — | `TestRunBlockGC_AttachesBackupHold` test | 139-160 |
| — | `TestRunBlockGC_MissingBackupStore_ReturnsError` test | 162-185 |
| — | Imports no longer needed: `errors`, `io`, `strings`, `pkg/backup/destination`, `pkg/backup/manifest`, `pkg/controlplane/models`, `pkg/controlplane/runtime/storebackups`, `pkg/controlplane/store` | 3-19 |

**Test coverage retained:**
- `TestRunBlockGC_DedupesSharedRemoteStores` — pointer-identity dedup of shared remotes (unchanged behavior)
- `TestRunBlockGC_DryRunPropagates` — dryRun + sharePrefix field pass-through into `gc.Options` (stripped BackupHold assertion)

**Test coverage added:**
- `TestRunBlockGC_NoRemoteShares` — asserts empty stats and zero CollectGarbage invocations when no remote-backed shares are registered (this path was previously unreachable because SAFETY-01 refusal would catch the test fixture before share enumeration).

## Caller audit

Ran `grep -rn "SetRestoreBumpBootVerifier\|SetBumpBootVerifier" internal/ pkg/ cmd/ --include='*.go' | grep -v 'runtime/storebackups/'` → 0 external callers. `Runtime.SetBumpBootVerifier` was never wired from adapter code despite its doc comment claiming otherwise — plan 08-07's REST+CLI removal already pruned the would-have-been caller (`internal/adapter/nfs/v4/handlers` never imported it in this branch).

Ran `grep -rn "\.RegisterBackupRepo\b\|\.UnregisterBackupRepo\b\|\.UpdateBackupRepo\b\|\.RunBackup\b\|\.ValidateBackupSchedule\b\|\.BackupStore()\|\.DestFactoryFn()\|\.StoreBackupsService()" . --include='*.go' | grep -v 'runtime/storebackups/' | grep -v 'pkg/backup/'` → only matches are within the deleted runtime.go/blockgc.go code itself (before this commit) and a `GORMStore.UpdateBackupRepo` in `pkg/controlplane/store/backup_test.go` (different method — on GORMStore, not Runtime; scheduled for deletion in plan 08-08a).

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Removed `fmt` and `internal/logger` imports from runtime.go**

- **Found during:** Step 1 (runtime.go edits)
- **Issue:** After deleting the 8 delegation methods and the Serve startup block, `fmt.Errorf(...)` and `logger.Warn(...)` had zero remaining usages in runtime.go — Go treats unused imports as a build error.
- **Fix:** Dropped both imports as part of the same edit.
- **Commit:** `79503576`

**2. [Rule 3 - Blocking] Removed `fmt` and `store` imports from blockgc.go**

- **Found during:** Step 2 (blockgc.go edits)
- **Issue:** After deleting the SAFETY-01 refusal error and the `SetBackupHoldWiringForTest` helper, `fmt.Errorf(...)` and `store.BackupStore` had zero remaining usages in blockgc.go.
- **Fix:** Dropped both imports as part of the file rewrite.
- **Commit:** `79503576`

**3. [Rule 3 - Blocking] Trimmed blockgc_test.go imports**

- **Found during:** Step 3 (test cleanup)
- **Issue:** After deleting the two SAFETY-01 tests and the `gcBlockBackupStore`/`destFactoryNoop`/`fakeGCDestination` fakes, many imports became unused: `errors`, `io`, `strings`, `pkg/backup/destination`, `pkg/backup/manifest`, `pkg/controlplane/models`, `pkg/controlplane/runtime/storebackups`, `pkg/controlplane/store`.
- **Fix:** Dropped all of them; rewrote the file with only the imports actually referenced (`context`, `testing`, `pkg/blockstore/gc`, `pkg/blockstore/remote`, `pkg/health`, `pkg/metadata/store/memory`).
- **Commit:** `79503576`

**4. [Scope-preserving] Added `TestRunBlockGC_NoRemoteShares`**

- **Found during:** Step 3 (test review)
- **Issue:** With SAFETY-01 removed, the "no-remote-shares early-return" branch at `blockgc.go:35-40` became reachable from tests for the first time (previously masked by SAFETY-01's refusal on the no-backup-hold path). Zero coverage on that branch.
- **Fix:** Added one 10-line test (`TestRunBlockGC_NoRemoteShares`) asserting `stats != nil`, `err == nil`, `len(captured) == 0` when the runtime has no remote-backed shares.
- **Commit:** `79503576`

No architectural changes (Rule 4) required. No authentication gates encountered.

## Verification

### Acceptance criteria — all green

```
grep -c "storebackups" pkg/controlplane/runtime/runtime.go            → 0
grep -c "storeBackupsSvc|SetRestoreBumpBootVerifier|SetBumpBootVerifier" pkg/controlplane/runtime/runtime.go → 0
grep -cE "func \(.*\) (RegisterBackupRepo|UnregisterBackupRepo|UpdateBackupRepo|RunBackup|ValidateBackupSchedule|BackupStore|DestFactoryFn|StoreBackupsService)\b" pkg/controlplane/runtime/runtime.go → 0
grep -c "NewBackupHold|StaticBackupHold|SetBackupHoldWiringForTest|BackupHold" pkg/controlplane/runtime/blockgc.go → 0
grep -c "storebackups|SetBackupHoldWiringForTest" pkg/controlplane/runtime/blockgc_test.go → 0
grep -rn "SetRestoreBumpBootVerifier|SetBumpBootVerifier" internal/ pkg/ cmd/ --include='*.go' | grep -v 'runtime/storebackups/' → 0 matches
```

### Build + tests

- `go build ./...` → exit 0, no output.
- `go vet ./...` → exit 0, no output.
- `go test -count=1 -race ./pkg/controlplane/runtime/...` → all packages OK (3.742s for the runtime package itself; storebackups internal tests still pass independently).
- `go test -count=1 -race ./pkg/blockstore/...` → all packages OK.
- `go test -count=1 -race ./internal/controlplane/...` → all packages OK.

### Commit signature + hygiene

- `git log -1 --show-signature` → `Good "git" signature for m.marmos@gmail.com with RSA key ...`
- `git log -1 --format='%B' | grep -iE 'claude code|co-authored-by'` → no matches.
- `git log -1 --format=%s` → `runtime: drop storebackups wiring + SAFETY-01 gate + delegation methods (TD-03)`.

## Self-Check

- commit `79503576` present in git log: **FOUND**
- SUMMARY.md at `.planning/phases/08-pre-refactor-cleanup-a0/08-08-SUMMARY.md`: **FOUND** (this file)
- pkg/controlplane/runtime/runtime.go: **FOUND** (modified)
- pkg/controlplane/runtime/blockgc.go: **FOUND** (modified)
- pkg/controlplane/runtime/blockgc_test.go: **FOUND** (modified)

## Self-Check: PASSED
