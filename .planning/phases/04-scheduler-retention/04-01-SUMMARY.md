---
phase: 04-scheduler-retention
plan: 01
subsystem: database
tags: [backup, scheduler, retention, migration, gorm, polymorphic, sentinels]

# Dependency graph
requires:
  - phase: 01-foundations-models-manifest-capability-interface
    provides: "Backupable interface + PayloadIDSet + BackupRepo/Record/Job models + BackupStore sub-interface + RecoverInterruptedJobs SAFETY-02 helper"
  - phase: 02-per-engine-backup-drivers
    provides: "Memory/Badger/Postgres engines implementing Backupable (consumed unchanged via compat shim)"
  - phase: 03-destination-drivers-encryption
    provides: "Destination interface (PutBackup/Delete/List) consumed as-is by Wave 2 executor"
provides:
  - "pkg/backup/backupable.go — canonical Backupable interface + PayloadIDSet + 5 sentinel errors"
  - "pkg/metadata/backup.go — compat shim (type aliases + var re-exports) keeping Phase-2 engine files compiling unchanged"
  - "models.BackupRepo with polymorphic (TargetID, TargetKind) replacing MetadataStoreID FK (D-26)"
  - "BackupStore.ListReposByTarget(kind, id) replacing ListBackupReposByStore(id)"
  - "BackupStore.ListSucceededRecordsForRetention(repoID) — oldest-first succeeded non-pinned candidate set"
  - "GORM pre-AutoMigrate RenameColumn metadata_store_id→target_id + post-AutoMigrate target_kind backfill"
  - "models.ErrScheduleInvalid / ErrRepoNotFound / ErrBackupAlreadyRunning / ErrInvalidTargetKind sentinels"
affects:
  - 04-02 (scheduler primitives — imports pkg/backup.Backupable, uses ListReposByTarget)
  - 04-03 (executor — imports pkg/backup.Backupable, pkg/backup.PayloadIDSet, consumes ListSucceededRecordsForRetention)
  - 04-04 (storebackups sub-service — imports pkg/backup, reads sentinels, uses polymorphic target)
  - 04-05 (retention pass — ListSucceededRecordsForRetention is its sole repo-record query)
  - phase-05 (restore orchestration — sentinels + canonical import path already in place)
  - phase-06 (CLI/REST — ErrScheduleInvalid validator, polymorphic target surfaces in API)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Compat shim pattern — package-level type aliases + `var X = pkg.X` re-assignments preserve sentinel IDENTITY across import-path moves (avoids errors.Is false-negative)"
    - "Pre-AutoMigrate RenameColumn + post-AutoMigrate UPDATE backfill — idempotent via HasColumn guard + scoped WHERE clause"
    - "Polymorphic (id, kind) target instead of direct FK — database stays dumb; service layer enforces kind allow-list via typed sentinel (ErrInvalidTargetKind)"

key-files:
  created:
    - "pkg/backup/backupable.go (moved from pkg/metadata/backup.go per D-27 with verbatim signatures + GoDoc)"
    - "pkg/backup/backupable_test.go (moved from pkg/metadata/backup_test.go)"
    - "pkg/metadata/backup_shim_test.go (new regression test asserting sentinel IDENTITY across shim)"
  modified:
    - "pkg/metadata/backup.go (replaced body with compat shim; aliases + var re-exports)"
    - "pkg/controlplane/models/backup.go (BackupRepo: rename MetadataStoreID→TargetID, add TargetKind, drop FK association field, rename unique index idx_backup_repo_store_name→idx_backup_repo_target_name)"
    - "pkg/controlplane/models/errors.go (added 4 Phase-4 sentinels)"
    - "pkg/controlplane/store/backup.go (rename ListBackupReposByStore→ListReposByTarget, add ListSucceededRecordsForRetention, update GetBackupRepo to query target_id)"
    - "pkg/controlplane/store/interface.go (BackupStore sub-interface: method rename + new retention query + updated GetBackupRepo doc)"
    - "pkg/controlplane/store/gorm.go (pre-AutoMigrate backup_repos.metadata_store_id→target_id rename; post-AutoMigrate target_kind backfill)"
    - "pkg/controlplane/store/backup_test.go (seeds switched to TargetID/TargetKind; TestBackupRepoUniquePerStore renamed to TestBackupRepoUniquePerTarget; added TestListSucceededRecordsForRetention + TestBackupRepoTargetKindBackfill integration tests)"

key-decisions:
  - "Chose the type-alias compat shim over rewriting every Phase-2 engine import (both approaches satisfy D-27 per PATTERNS.md; shim is lower-risk default, touches 0 engine files, and preserves sentinel IDENTITY for errors.Is across the framework boundary)"
  - "Kept pkg/metadata/backup.go permanently in place (not time-boxed for removal) because Phase 6 CLI + Phase 5 restore also use canonical pkg/backup.* names; shim is a permanent backstop not a migration bridge"
  - "Added pkg/metadata/backup_shim_test.go regression test for sentinel identity — if a future contributor ever replaces `var X = backup.X` with `errors.New(...)` the aliased value would decouple across the module boundary and errors.Is would silently return false; the test catches that class of regression"
  - "Dropped the GORM FK association (`MetadataStore MetadataStoreConfig \\`gorm:\\\"foreignKey:MetadataStoreID\\\"\\``) entirely per D-26 step 4; validation of (target_id, target_kind) moves to service layer in Wave 4 (runtime/storebackups.Service) where `stores.Service.Get` resolves the polymorphic target"
  - "Composite unique index rename (idx_backup_repo_store_name → idx_backup_repo_target_name) is picked up by AutoMigrate on next boot since both columns participating in the index were modified; no explicit DropIndex statement required (AutoMigrate drops + recreates when the named uniqueIndex tag moves)"
  - "Post-AutoMigrate backfill scopes UPDATE to `WHERE target_kind = '' OR target_kind IS NULL` so subsequent boots never rewrite operator-set values (T-04-01-01 tampering mitigation)"

patterns-established:
  - "Framework moves use compat shims — when a type/sentinel moves across packages, leave a shim behind that re-exports via type aliases (for types) and `var X = pkg.X` (for sentinels) so errors.Is preserves IDENTITY, not just value-equality"
  - "Polymorphic target refactor — database column rename + new kind discriminator + post-migrate backfill + service-layer allow-list sentinel; repeatable recipe for extending backup_repos to cover block-store targets without schema change"
  - "Migration test pattern for target_kind — test seeds a row via raw SQL with kind='', runs the backfill UPDATE, and asserts the helper query returns the row with kind='metadata' (covers the idempotent-re-run edge case a standard AutoMigrate unit test wouldn't exercise)"

requirements-completed: []

# Metrics
duration: ~35min
completed: 2026-04-16
---

# Phase 4 Plan 01: Framework move + polymorphic backup_repos Summary

**Relocated the Backupable framework to `pkg/backup` with a compat shim that keeps Phase-2 engines compiling unchanged, and migrated `backup_repos` from an FK-bound `metadata_store_id` column to a polymorphic `(target_id, target_kind)` pair so Waves 2–4 can register schedules against any store kind.**

## Performance

- **Duration:** ~35 minutes
- **Tasks:** 2
- **Files created:** 3 (`pkg/backup/backupable.go`, `pkg/backup/backupable_test.go`, `pkg/metadata/backup_shim_test.go`)
- **Files modified:** 7 (`pkg/metadata/backup.go`, `pkg/controlplane/models/backup.go`, `pkg/controlplane/models/errors.go`, `pkg/controlplane/store/backup.go`, `pkg/controlplane/store/interface.go`, `pkg/controlplane/store/gorm.go`, `pkg/controlplane/store/backup_test.go`)
- **Files deleted:** 1 (`pkg/metadata/backup_test.go` — moved verbatim to `pkg/backup/backupable_test.go`)

## Accomplishments

- `Backupable` interface + `PayloadIDSet` + 5 sentinel errors live at the canonical import path `github.com/marmos91/dittofs/pkg/backup` (D-27). Signatures preserved byte-for-byte from `pkg/metadata/backup.go`; GoDoc preserved.
- `pkg/metadata/backup.go` is a 2-line-per-symbol compat shim (`type X = backup.X` + `var X = backup.X`) so the three Phase-2 engines (`pkg/metadata/store/{memory,badger,postgres}/backup.go`) and their tests continue to compile with zero edits.
- `BackupRepo.TargetID` + `BackupRepo.TargetKind` (size:10, default `'metadata'`, indexed) replace `BackupRepo.MetadataStoreID` (D-26). The composite unique index migrated from `idx_backup_repo_store_name` to `idx_backup_repo_target_name`. The direct FK association (`MetadataStore MetadataStoreConfig`) was removed entirely — target validation moves to the service layer in Wave 4.
- GORM pre-AutoMigrate `RenameColumn("metadata_store_id", "target_id")` (HasColumn-guarded, idempotent) plus a post-AutoMigrate backfill `UPDATE backup_repos SET target_kind = 'metadata' WHERE target_kind = '' OR target_kind IS NULL`. Mirrors the existing `read_cache_size → read_buffer_size` + `portmapper_port` backfill conventions verbatim.
- `BackupStore.ListReposByTarget(kind, targetID)` replaces `ListBackupReposByStore(storeID)` (D-26 step 5). The method queries `target_kind = ? AND target_id = ?` so Wave 4 can cleanly scope scheduler loads to kind="metadata" today without blocking kind="block" additions later.
- `BackupStore.ListSucceededRecordsForRetention(repoID)` returns succeeded non-pinned records sorted oldest-first, filtered to `status=succeeded AND pinned=false`, ordered `created_at ASC` (D-10, D-12). This is the only per-repo record query the Wave 5 retention pass will need.
- Phase-4 runtime sentinels registered in `models/errors.go`: `ErrScheduleInvalid`, `ErrRepoNotFound`, `ErrBackupAlreadyRunning`, `ErrInvalidTargetKind`.
- Regression test at `pkg/metadata/backup_shim_test.go` asserts `errors.Is(metadata.ErrX, backup.ErrX)` returns `true` in both directions — a partial migration that ever replaces `var X = backup.X` with `errors.New(...)` would decouple the identity and silently break cross-package `errors.Is` matching; this test fails loudly in that scenario.
- Two new integration tests (`//go:build integration`) in `pkg/controlplane/store/backup_test.go`:
  - `TestListSucceededRecordsForRetention` — seeds 3 succeeded + 1 failed + 1 pinned record and asserts the helper returns exactly the 3 succeeded non-pinned records in oldest-first order with no failed or pinned leakage.
  - `TestBackupRepoTargetKindBackfill` — directly forces `target_kind = ''` on a seeded row, runs the backfill `UPDATE`, and asserts the row is stamped `metadata`; proves the boot migration stays idempotent on already-migrated databases.

## Task Commits

1. **Task 1: Relocate Backupable framework to pkg/backup with compat shim (D-27)** — `ba092afd` (refactor)
2. **Task 2: Schema migration — rename backup_repos.metadata_store_id → target_id + add target_kind (D-26)** — `2c678d9b` (feat)

## Files Created/Modified

### Created
- `pkg/backup/backupable.go` — canonical home of `Backupable`, `PayloadIDSet`, and 5 sentinel errors (`ErrBackupUnsupported`, `ErrRestoreDestinationNotEmpty`, `ErrRestoreCorrupt`, `ErrSchemaVersionMismatch`, `ErrBackupAborted`). Moved verbatim from `pkg/metadata/backup.go`.
- `pkg/backup/backupable_test.go` — `NewPayloadIDSet`/`Contains`/`Len`/`Add`/sentinel-distinctness tests. Moved verbatim from `pkg/metadata/backup_test.go` with package declaration switched.
- `pkg/metadata/backup_shim_test.go` — regression test asserting `errors.Is(metadata.ErrX, backup.ErrX)` holds in both directions + type-alias identity for `Backupable` and `PayloadIDSet`.

### Modified
- `pkg/metadata/backup.go` — replaced with a compat shim: `type Backupable = backup.Backupable`, `type PayloadIDSet = backup.PayloadIDSet`, and `var Err… = backup.Err…` re-exports for every sentinel plus `NewPayloadIDSet`.
- `pkg/controlplane/models/backup.go` — `BackupRepo` struct: `MetadataStoreID` → `TargetID`, add `TargetKind` with default `'metadata'` + index, rename unique index to `idx_backup_repo_target_name`, drop the FK association field. Struct GoDoc updated to document the polymorphic target + service-layer allow-list.
- `pkg/controlplane/models/errors.go` — added 4 Phase-4 runtime sentinels inside the shared `var` block.
- `pkg/controlplane/store/backup.go` — `GetBackupRepo` now queries `target_id` (doc-comment clarifies `storeID` is the polymorphic target_id); renamed `ListBackupReposByStore` → `ListReposByTarget(kind, targetID)` with `target_kind = ? AND target_id = ?` filter; added `ListSucceededRecordsForRetention` after `ListBackupRecordsByRepo`.
- `pkg/controlplane/store/interface.go` — `BackupStore` sub-interface: method rename + new retention query + updated `GetBackupRepo` doc + updated interface-level GoDoc to describe polymorphic target.
- `pkg/controlplane/store/gorm.go` — added pre-AutoMigrate `RenameColumn(&models.BackupRepo{}, "metadata_store_id", "target_id")` (HasColumn-guarded) and post-AutoMigrate `UPDATE backup_repos SET target_kind = 'metadata' WHERE target_kind = '' OR target_kind IS NULL`. Comments cite D-26 and the threat IDs (T-04-01-01, T-04-01-02).
- `pkg/controlplane/store/backup_test.go` — integration tests updated: all `MetadataStoreID: X` seeds switched to `TargetID: X, TargetKind: "metadata"`; list-by-target subtest now exercises `ListReposByTarget("metadata", storeID)` and a mismatched-kind subtest asserts `ListReposByTarget("block", storeID)` returns empty; `TestBackupRepoUniquePerStore` renamed `TestBackupRepoUniquePerTarget`; two new tests as documented above.

### Deleted
- `pkg/metadata/backup_test.go` — moved verbatim to `pkg/backup/backupable_test.go`. Intentional deletion: the test file's package-internal assertions become more correct in the canonical package home.

## Decisions Made

- **Compat shim over direct import rewrite.** PATTERNS.md explicitly says both approaches satisfy D-27 and identifies the shim as "the lower-risk default." We took it: zero Phase-2 engine edits, easier rollback, and sentinel IDENTITY preserved via `var X = backup.X` re-exports. The shim is permanent (not a migration bridge) because Phase 6 CLI + Phase 5 restore use canonical `pkg/backup.*` names while existing test files still use `metadata.Err*`; both keep working.
- **Sentinel IDENTITY regression test.** Shipped `pkg/metadata/backup_shim_test.go` to catch the class of regression where a contributor "cleans up" the shim by replacing `var X = backup.X` with `errors.New(...)`. That substitution is silently wrong — `errors.Is(metadata.ErrX, backup.ErrX)` would start returning `false` and callers that straddle the boundary would miss matches. The test uses `require.Truef(errors.Is(...))` in both directions so the failure message is specific.
- **Dropped FK association entirely, not just renamed.** D-26 step 4 explicitly drops the direct FK to `metadata_store_configs`. Removing the GORM association field (`MetadataStore MetadataStoreConfig \`gorm:"foreignKey:MetadataStoreID"\``) forces any Wave 4 code that needs to dereference the target through the service layer's `stores.Service.Get(target_id)` path, which validates both existence AND allowed kind. If we'd kept the association with a foreignKey tag pointing at `TargetID`, GORM would still enforce the old contract and sneak an unintended FK constraint back into AutoMigrate output.
- **No explicit DropIndex for `idx_backup_repo_store_name`.** AutoMigrate handles index migration when both columns participating in the uniqueIndex tag are modified. The integration test suite (`TestBackupRepoUniquePerTarget`) proves the new composite constraint works; if the old index had persisted, the dual-target duplicate check would fail.

## Deviations from Plan

None — plan executed exactly as written. The compat-shim path was one of two options PATTERNS.md identified ("either approach satisfies the decision; the shim is the lower-risk default") and plan Task 1 explicitly instructed it. No auto-fixes (Rule 1/2/3) and no architectural stops (Rule 4) were triggered.

## Issues Encountered

None. All unit tests (`go test ./...`) pass with no changes to any file under `pkg/metadata/store/{memory,badger,postgres}/`, confirming the compat shim preserves the Phase-2 import surface. Integration tests (`go test -tags=integration ./pkg/controlplane/store/...`) pass including the two new tests.

## Callers Swept

Plan Step 6 of Task 2 asked for a sweep of any caller of the renamed `ListBackupReposByStore` outside the plan's files. Expected: none.

**Result:** `grep -rn "ListBackupReposByStore" pkg/ internal/ cmd/` returns zero matches. Phase 6 CLI and the REST API handlers are not yet implemented; there were no existing callers to update.

## Pointers for Waves 2–4

- **Canonical import path.** Waves 2–4 MUST import the framework from `github.com/marmos91/dittofs/pkg/backup`, not `pkg/metadata`. The shim at `pkg/metadata/backup.go` is a permanent backstop for legacy callers, not a preferred path for new code.
- **Scheduler repo load.** `storebackups.Service.Serve(ctx)` should call `s.store.ListAllBackupRepos(ctx)` for the full boot-time load (already in interface). Wave 4's service-layer validation of `(target_kind, target_id)` against `stores.Service.Get` uses the polymorphic pair from `BackupRepo.TargetKind` + `BackupRepo.TargetID`.
- **Retention query.** Wave 5's retention pass should call `s.store.ListSucceededRecordsForRetention(repoID)`. It already filters to the candidate set (`status=succeeded AND pinned=false`) in oldest-first order; the retention union math (D-09: keep-count ∪ keep-age-days) operates on the returned slice.
- **Sentinels.** `models.ErrScheduleInvalid` is ready for Wave 4's `ValidateSchedule` function (D-06 strict-at-write-time validation path). `models.ErrBackupAlreadyRunning` is the sentinel returned by `RunBackup` when the overlap mutex is held (D-07, D-23). `models.ErrRepoNotFound` covers the Wave 4 registry miss (D-22 UnregisterRepo on unknown ID). `models.ErrInvalidTargetKind` is the service-layer allow-list guard (T-04-01-03).
- **AutoMigrate idempotency.** The pre-AutoMigrate `HasColumn` guard makes the rename a no-op on freshly initialized databases and on databases already migrated by a prior boot. Same for the backfill (scoped to empty/NULL rows only). Safe to boot, crash, and re-boot in any order.

## Next Phase Readiness

- Tree compiles cleanly; `go build ./...` exits 0.
- All unit tests pass (`go test ./pkg/backup/... ./pkg/metadata/ ./pkg/metadata/store/... ./pkg/controlplane/store/... ./pkg/controlplane/models/...`).
- Integration tests pass including new `TestListSucceededRecordsForRetention` and `TestBackupRepoTargetKindBackfill`.
- Zero edits to files under `pkg/metadata/store/{memory,badger,postgres}/` — the compat shim removes the need.
- Wave 2 (scheduler primitives) can import `pkg/backup` without an import cycle.
- Wave 2 (scheduler) can call `ListReposByTarget("metadata", storeID)` for kind-scoped loads.
- Wave 3 (executor) can call `ListSucceededRecordsForRetention(repoID)` and consume `backup.Backupable` / `backup.PayloadIDSet` types directly.
- Wave 4 (storebackups sub-service) has all Phase-4 runtime sentinels registered.

## Self-Check: PASSED

Verified against acceptance criteria:

| Criterion | Status |
|-----------|--------|
| `pkg/backup/backupable.go` exists and contains `type Backupable interface` | PASS — line 26 |
| `pkg/backup/backupable.go` contains `var ErrBackupAborted = errors.New("backup aborted")` | PASS — line 92 |
| `pkg/metadata/backup.go` contains `type Backupable = backup.Backupable` | PASS — line 18 |
| `pkg/metadata/backup.go` contains `ErrBackupAborted = backup.ErrBackupAborted` | PASS — line 39 |
| `pkg/backup/backupable_test.go` exists and `go test ./pkg/backup/...` exits 0 | PASS |
| `go build ./...` exits 0 | PASS |
| `go test ./pkg/metadata/store/memory/...` exits 0 | PASS |
| `go test ./pkg/metadata/store/badger/...` exits 0 | PASS (no test files, build-only) |
| `grep "package backup" pkg/backup/backupable.go` returns line 1 | PASS |
| No modifications to `pkg/metadata/store/{memory,badger,postgres}/` | PASS |
| `TargetID` / `TargetKind` on BackupRepo struct | PASS |
| `MetadataStoreID` / `MetadataStore ` removed from backup model | PASS — 0 matches |
| `RenameColumn` for backup_repos in gorm.go | PASS — line 259 |
| `UPDATE backup_repos SET target_kind` backfill in gorm.go | PASS — line 275 |
| `ListReposByTarget` + `ListSucceededRecordsForRetention` in backup.go + interface.go | PASS |
| 0 matches for `ListBackupReposByStore` across `pkg/ internal/ cmd/` | PASS |
| 4 Phase-4 sentinels in models/errors.go | PASS — lines 51–54 |
| `go test ./pkg/controlplane/store/... ./pkg/controlplane/models/...` exits 0 | PASS |
| Integration tests `go test -tags=integration ./pkg/controlplane/store/...` pass | PASS |

---
*Phase: 04-scheduler-retention*
*Completed: 2026-04-16*
