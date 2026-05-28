---
phase: 24-restore-flow
plan: 01
subsystem: metadata
tags: [resetable, restore, conformance]
requires:
  - "Phase 21 Backupable interface + per-backend Restore (pkg/metadata/backupable.go, pkg/metadata/store/*/backup.go)"
  - "Phase 21 backup_conformance.go helpers (populateTestData, asBackupable, createTestShare/File, hashOfSeed)"
provides:
  - "metadata.Resetable optional interface (Reset(ctx) error)"
  - "Memory / Badger / Postgres Reset impls preserving live store handle"
  - "storetest.ResetThenRestoreConformance shared scenario"
  - "TestBackupTablesCoversAllMigrations CI guard (D-24-03)"
affects:
  - "P24-02 (sentinels): ErrMetadataStoreNotResetable now has a backend-side counterpart to assert against"
  - "P24-03 (RestoreSnapshot orch): step 5 type-assertion (store).(metadata.Resetable) is satisfied by all 3 shipped backends"
tech_stack:
  added: []
  patterns:
    - "Optional-capability + type-assertion (mirrors Backupable)"
    - "Compile-time interface assertion at file head"
    - "Reuse of package-level backupTables slice in Postgres reset"
    - "Migrations-vs-backupTables CI audit (regex over migrations/*.up.sql)"
key_files:
  created:
    - "pkg/metadata/resetable.go"
    - "pkg/metadata/store/memory/reset.go"
    - "pkg/metadata/store/memory/reset_test.go"
    - "pkg/metadata/store/badger/reset.go"
    - "pkg/metadata/store/badger/reset_test.go"
    - "pkg/metadata/store/postgres/reset.go"
    - "pkg/metadata/store/postgres/reset_test.go"
    - "pkg/metadata/store/postgres/reset_audit_test.go"
    - "pkg/metadata/storetest/reset_conformance.go"
  modified:
    - "pkg/metadata/store/memory/memory_conformance_test.go"
    - "pkg/metadata/store/badger/badger_conformance_test.go"
    - "pkg/metadata/store/postgres/postgres_conformance_test.go"
decisions:
  - "Migrations audit lives in package postgres (not postgres_test) so it can read backupTables directly without exposing it"
  - "Memory Reset clears serverConfig (operational state) but preserves capabilities/maxStorageBytes/maxFiles/storeID (engine identity)"
  - "Badger Reset uses single db.DropAll() call — no manual key iteration"
  - "Postgres Reset reuses truncateAllTables() helper from backup.go verbatim (single source of truth)"
metrics:
  duration: "single session"
  completed: 2026-05-28
---

# Phase 24 Plan 01: Resetable interface + 3 backends + conformance — Summary

Resetable optional metadata-store interface lands across all three shipped backends (memory / badger / postgres), with a shared `ResetThenRestoreConformance` scenario wired into each backend's existing conformance test file. The Postgres backend reuses the package-level `backupTables` slice (single source of truth); a new no-DSN audit test (`TestBackupTablesCoversAllMigrations`) is the D-24-03 CI guard that fails if a future migration introduces a table without updating `backupTables`.

## What landed

**Resetable interface (`pkg/metadata/resetable.go`):**
- Optional capability, NOT embedded in `MetadataStore` — call sites discover via type assertion (mirrors Phase 21 `Backupable`).
- Doc-comment explicitly warns that Reset bypasses `ErrRestoreDestinationNotEmpty` and is intended only for `Runtime.RestoreSnapshot` after the share-disabled barrier (D-24-01) is in place.
- Implementations MUST preserve the live store handle (no close/reopen).

**Three backend impls:**
- **Memory** (`pkg/metadata/store/memory/reset.go`): under `s.mu.Lock`, reassigns every DATA field listed in the constructor to a fresh empty `make(...)`. `rollupMu` and `syncedMu` taken separately; `usedBytes.Store(0)`; lazy sub-stores (`fileBlockData`, `lockStore`, `clientStore`, `durableStore`) nilled to mirror "never initialized". CONFIG fields (`capabilities`, `maxStorageBytes`, `maxFiles`, `storeID`, `attrPool`) explicitly preserved.
- **Badger** (`pkg/metadata/store/badger/reset.go`): single `s.db.DropAll()` call — Badger's documented atomic truncate. The same `*badger.DB` handle stays valid.
- **Postgres** (`pkg/metadata/store/postgres/reset.go`): `BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ` → `truncateAllTables(ctx, pgRaw)` (helper reused verbatim from `backup.go`) → `COMMIT`. Same `*pgx.Pool` handle stays valid. Error prefix `"reset:"` matches `"backup:" / "restore:"` symmetry.

**Conformance scenario (`pkg/metadata/storetest/reset_conformance.go`):**
- `func ResetThenRestoreConformance(t, factory BackupableStoreFactory)` — single scenario per D-24-12 (not a multi-subtest tree).
- `asResetable(t, store)` helper mirrors `asBackupable`.
- Sequence: `populateTestData(t, store, "rst")` → Backup → assert `hs.Len() == 3` → Reset → assert `ListShares` empty → Restore from same dump → assert shares + representative `alpha.bin` + `beta.bin` survived round-trip with correct sizes / modes / block counts.
- Wired into all 3 backend conformance test files (postgres skips cleanly without `DITTOFS_TEST_POSTGRES_DSN`).

**Postgres migrations-vs-backupTables audit (`pkg/metadata/store/postgres/reset_audit_test.go`):**
- Lives in package `postgres` (not `postgres_test`) so it can read the unexported `backupTables` slice directly — avoids exposing a test-only accessor.
- No build tag, no DSN required: pure filesystem regex audit over `migrations/*.up.sql`.
- Audit result: all 15 tables discovered in migrations (`durable_handles`, `file_block_refs`, `file_blocks`, `files`, `filesystem_capabilities`, `link_counts`, `locks`, `nsm_client_registrations`, `parent_child_map`, `pending_writes`, `rollup_offsets`, `server_config`, `server_epoch`, `shares`, `synced_hashes`) are present in `backupTables`. `schema_migrations` is intentionally excluded per the existing doc-comment (owned by `golang-migrate`, recreated by `AutoMigrate`).
- The audit is a CI guard going forward — any future migration adding a `CREATE TABLE` without updating `backupTables` fails the test.

## Conformance scenario behaviors covered

- `ResetThenRestoreConformance(t, factory)` — Backup → Reset → empty assertion → Restore → round-trip equality on shares + two representative files.
- Per-backend unit tests beyond the conformance suite:
  - Memory: `TestReset_EmptyStore`, `TestReset_PopulatedStore`, `TestReset_PreservesConfig` (storeID stable), `TestReset_CtxCancellation` (errors.Is(ctx.Canceled)), `TestReset_ReusableAfterReset`.
  - Badger: `TestReset_Empty`, `TestReset_Populated`, `TestReset_HandleReusable`, `TestReset_CtxCancellation`.
  - Postgres: `TestReset_Postgres_Empty`, `TestReset_Postgres_Populated`, `TestReset_Postgres_HandleReusable`, `TestReset_Postgres_CtxCancellation` (all skip cleanly without DSN).

## Verification

```
go test ./pkg/metadata/store/memory/... -count=1                          # PASS
go test -tags integration ./pkg/metadata/store/badger/... -count=1        # PASS
go test ./pkg/metadata/store/postgres/... -run TestBackupTablesCoversAllMigrations -count=1   # PASS (no DSN required)
go test -tags integration ./pkg/metadata/store/postgres/... -count=1      # skips without DSN (expected)
go test ./pkg/metadata/... -count=1                                       # PASS (all packages green)
go vet -tags integration ./pkg/metadata/...                               # clean
```

Full pkg/metadata regression run:
- `pkg/metadata` ok 0.896s
- `pkg/metadata/acl` ok 0.298s
- `pkg/metadata/backup` ok 0.574s
- `pkg/metadata/lock` ok 2.852s
- `pkg/metadata/store/badger` ok 1.848s
- `pkg/metadata/store/memory` ok 1.589s
- `pkg/metadata/store/postgres` ok 1.765s

No Phase 21 backup-conformance regressions.

## Wave 2 readiness (for P24-03)

`Runtime.RestoreSnapshot` step 5 (per D-24-09) will type-assert the resolved metadata store:

```go
resetable, ok := metaStore.(metadata.Resetable)
if !ok {
    return fmt.Errorf("restore snapshot %q: %w", snapID, models.ErrMetadataStoreNotResetable)
}
```

After this plan:

- `*memory.MemoryMetadataStore` — implements `Resetable` (compile-time asserted in `reset.go`).
- `*badger.BadgerMetadataStore` — implements `Resetable` (compile-time asserted).
- `*postgres.PostgresMetadataStore` — implements `Resetable` (compile-time asserted).

All 3 shipped backends pass the type assertion in production. `ErrMetadataStoreNotResetable` (added by P24-02) is unreachable in normal operation but covers the "future backend that opts out" case.

## Postgres backupTables audit results

Migrations directory at this branch: `000001_initial_schema.up.sql` through `000016_durable_handle_granted_access.up.sql` (16 numbered migrations).

Tables discovered via regex: 15 distinct names.
Tables in `backupTables`: 15 (identical set).
Missing from `backupTables`: 0.
Schema-migrations excluded as documented.

No additions to `backupTables` were necessary in this plan.

## Deviations from Plan

**1. [Rule 3 - Blocking issue] Migrations audit test moved from `postgres_test` to `postgres` package**
- **Found during:** Task 2.
- **Issue:** The plan's `<action>` block proposed writing `TestBackupTablesCoversAllMigrations` in `reset_test.go` (package `postgres_test`, `//go:build integration`). That would have either (a) required a new exported accessor like `BackupTablesForTest()` on the `postgres` package — leaking a test-only surface — or (b) been gated behind `//go:build integration`, defeating the "audit runs without DB" intent in the plan's `<behavior>` ("Skip gracefully if `pgx` test deps are missing — the audit itself runs even without DB").
- **Fix:** Moved the audit into a new file `pkg/metadata/store/postgres/reset_audit_test.go` in package `postgres` (not `postgres_test`), with no build tag. This reads the unexported `backupTables` slice directly and runs unconditionally without exposing any new public surface.
- **Files modified:** `pkg/metadata/store/postgres/reset_audit_test.go` (new), `pkg/metadata/store/postgres/reset_test.go` (audit removed).
- **Commit:** `00869cb5`.

**2. [Rule 1 - Behavioral correctness] Memory Reset also clears `serverConfig`**
- **Found during:** Task 1 implementation review.
- **Issue:** The plan's `<interfaces>` block enumerated DATA fields but did not list `serverConfig`. Inspecting `pkg/metadata/store/memory/store.go` and `backup.go` (line 146-156) revealed `serverConfig` carries operational state (`CustomSettings` JSON, etc.) that Restore reassigns from the dump.
- **Fix:** Clear `serverConfig = metadata.MetadataServerConfig{}` in Reset so a follow-up Restore observes a fresh-engine baseline (matches Restore's reassignment shape at backup.go lines 325).
- **Files modified:** `pkg/metadata/store/memory/reset.go`.
- **Commit:** `65ea228f`.

## Self-Check: PASSED

Files created (all verified present):
- `pkg/metadata/resetable.go` FOUND
- `pkg/metadata/store/memory/reset.go` FOUND
- `pkg/metadata/store/memory/reset_test.go` FOUND
- `pkg/metadata/store/badger/reset.go` FOUND
- `pkg/metadata/store/badger/reset_test.go` FOUND
- `pkg/metadata/store/postgres/reset.go` FOUND
- `pkg/metadata/store/postgres/reset_test.go` FOUND
- `pkg/metadata/store/postgres/reset_audit_test.go` FOUND
- `pkg/metadata/storetest/reset_conformance.go` FOUND

Commits verified in git log:
- `65ea228f` FOUND — Task 1 (interface + memory)
- `00869cb5` FOUND — Task 2 (badger + postgres + audit)
- `dffd8dc4` FOUND — Task 3 (conformance scenario + wiring)
