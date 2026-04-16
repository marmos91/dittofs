---
phase: 02-per-engine-backup-drivers
plan: 02-04
subsystem: metadata.store.postgres
tags: [backup, restore, postgres, copy-binary, tar, ENG-02]
requires:
  - "metadata.Backupable interface (Phase 1 Plan 03)"
  - "metadata.PayloadIDSet (Phase 1 Plan 03)"
  - "github.com/jackc/pgx/v5 (already in go.mod)"
  - "gopkg.in/yaml.v3 (already in go.mod)"
  - "archive/tar (stdlib)"
provides:
  - "Postgres-backed metadata.Backupable implementation"
  - "metadata.ErrSchemaVersionMismatch sentinel (restore precondition)"
  - "metadata.ErrRestoreDestinationNotEmpty sentinel (restore precondition)"
  - "Integration test harness: per-test isolated database (createIsolatedDatabase helper)"
affects:
  - "Phase 3 destination drivers (consume the tar stream produced here)"
  - "Phase 5 restore orchestration (wires ErrSchemaVersionMismatch + ErrRestoreDestinationNotEmpty into 409 Conflict responses)"
  - "Phase 5 block-GC hold (consumes the PayloadIDSet returned from Backup)"
tech-stack:
  added: []
  patterns:
    - "Single REPEATABLE READ / READ ONLY transaction scopes every SELECT and every COPY TO STDOUT"
    - "PostgreSQL binary COPY (FORMAT binary) for all per-table streams"
    - "tar archive with deterministic ModTime for reproducible SHA-256"
    - "YAML manifest parsed before opening a tx (avoids holding locks during malformed archive handling)"
    - "TRUNCATE ... RESTART IDENTITY CASCADE before COPY FROM (pg_restore --clean pattern)"
    - "Triggers suppressed via session_replication_role = 'replica' during restore"
    - "Per-test isolated database (UUID-suffixed) with admin-connection DROP DATABASE cleanup"
key-files:
  created:
    - pkg/metadata/store/postgres/backup.go
    - pkg/metadata/store/postgres/backup_test.go
    - .planning/phases/02-per-engine-backup-drivers/deferred-items.md
  modified:
    - pkg/metadata/backup.go
    - pkg/metadata/backup_test.go
decisions:
  - "Schema-version check runs OUTSIDE any transaction so malformed archives never hold destination locks"
  - "Tar entry ModTime is fixed at Unix epoch (deterministicModTime) so identical source data hashes identically — manifest.created_at is the only non-deterministic field and is excluded from byte-identity tests"
  - "Table list is alphabetical (not FK-dependency-ordered): triggers are disabled during restore so order is not constraint-sensitive, and alphabetical order guarantees deterministic output"
  - "Backup uses a dedicated pool connection (via conn.Conn().PgConn()) so CopyTo shares the same session as the REPEATABLE READ transaction"
  - "Restore TRUNCATEs destination tables before COPY FROM: singleton migration rows (server_config, filesystem_capabilities) are treated as bootstrap state that restore replaces wholesale"
  - "The D-06 destination-empty gate observes ONLY the files table: migration-installed singletons are expected in a freshly migrated DB and must not trip the gate"
  - "Manifest format version (backupFormatVersion) is separate from the DB schema_migration_version — the former gates archive layout, the latter gates table shape"
  - "engine_kind recorded in manifest so Phase 5 orchestration can refuse cross-engine restore mechanically (ENG-02 × ENG-01 × ENG-03 boundary)"
metrics:
  duration: ~25m
  tasks: 3
  files_created: 3
  files_modified: 2
  completed: "2026-04-16"
---

# Phase 2 Plan 04: Postgres Store Backup Driver Summary

Postgres metadata store implements `metadata.Backupable` via a tar-bundled, binary-COPY per-table dump taken under a single `REPEATABLE READ / READ ONLY` transaction, with a schema-version gate and destination-empty gate on the restore path (ENG-02).

## Files

**Created**
- `pkg/metadata/store/postgres/backup.go` (534 lines) — `Backup` + `Restore` driver, plus helpers (`readSchemaVersionTx`, `scanPayloadIDsTx`, `countTableRowsTx`, `destinationIsEmpty`, `readBackupArchive`, `writeTarEntry`, `quoteIdent`/`quoteIdents`).
- `pkg/metadata/store/postgres/backup_test.go` (524 lines, `//go:build integration`) — full round-trip suite with per-test isolated database.
- `.planning/phases/02-per-engine-backup-drivers/deferred-items.md` — one out-of-scope finding.

**Modified**
- `pkg/metadata/backup.go` — added `ErrSchemaVersionMismatch` and `ErrRestoreDestinationNotEmpty` sentinels.
- `pkg/metadata/backup_test.go` — `TestRestoreSentinelsDistinct` locks the sentinels as distinct, non-nil, self-`errors.Is`-equal.

## Commits

| Task | Commit    | Message |
|------|-----------|---------|
| 1    | `f5ca8bfa` | feat(02-04): add restore precondition sentinels |
| 2    | `c0d3c1ba` | feat(02-04): postgres metadata backup driver (ENG-02) |
| 3    | `dd8f24e2` | test(02-04): postgres backup driver integration suite |

## How the Driver Works

### Backup (D-01, D-02, D-04, D-09)

1. Acquire a dedicated pool connection (so COPY and SELECT share one session).
2. Begin `pgx.TxOptions{ IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly }`.
3. Inside the tx, read:
   - `SHOW server_version` → `pg_server_version` (diagnostic).
   - `schema_migrations.version` → `schema_migration_version`.
   - `SELECT DISTINCT content_id FROM files WHERE content_id IS NOT NULL` → `PayloadIDSet` (SAFETY-01 hold set).
   - `COUNT(*)` on each table → `table_list[].row_count` (D-09).
4. Write tar archive to the writer:
   - `manifest.yaml` — first entry, so a truncated archive is still self-describing.
   - `tables/<name>.bin` — one entry per table in alphabetical order, using `COPY <tbl> TO STDOUT (FORMAT binary)` via the raw `*PgConn`.
5. Tar headers use a fixed `ModTime = Unix(0,0)` and mode `0o644` so identical payloads hash identically.
6. Rollback the tx (it is read-only; no commit needed).

Returned `PayloadIDSet` contains every distinct non-NULL `content_id` at snapshot time.

### Restore (D-04, D-06)

1. Parse the tar into memory, pulling `manifest.yaml` and a `tables/<name>.bin` map.
2. Validate `format_version == 1` and `engine_kind == "postgres"`.
3. Read the binary's current `schema_migrations.version` using a short-lived pool query (NO transaction held). If it does not match `manifest.schema_migration_version`, return `fmt.Errorf("%w: archive=%d, binary=%d", metadata.ErrSchemaVersionMismatch, ...)`.
4. Run `SELECT EXISTS (SELECT 1 FROM files LIMIT 1)` (still NO tx). If non-empty, return `metadata.ErrRestoreDestinationNotEmpty`.
5. Acquire a pool connection; `BeginTx` (default isolation).
6. `SET LOCAL session_replication_role = 'replica'` → suppresses user triggers for this session only.
7. `TRUNCATE TABLE ... RESTART IDENTITY CASCADE` over every backup table in one statement. This wipes migration-bootstrap singletons (`server_config`, `filesystem_capabilities`) that the emptiness gate intentionally ignores.
8. For each table, `COPY <tbl> FROM STDIN (FORMAT binary)` via the raw `*PgConn`.
9. Commit.
10. Re-run `initUsedBytesCounter` so the atomic byte counter reflects the restored file set.

## Manifest v1 Shape (Driver-Specific)

```yaml
format_version: 1
engine_kind: postgres
pg_server_version: "16.2"
schema_migration_version: 7
table_list:
  - name: durable_handles
    row_count: 0
  - name: files
    row_count: 42
  # ...alphabetical...
created_at: 2026-04-16T10:27:13Z
```

This manifest is **distinct from** Phase 1 Plan 03's `pkg/backup/manifest.Manifest`. The Phase 1 manifest is the **destination-level** descriptor (recorded by the destination driver in Phase 3, carrying `backup_id`, SHA-256 of the payload, encryption metadata). This driver emits an **engine-level** manifest embedded inside the payload tar so the engine-specific restore path can verify its own preconditions before the outer manifest is consulted. Phase 3 wraps this tar as an opaque byte stream.

## Tables Covered

Every table created by migrations `000001`..`000007`:

`durable_handles`, `filesystem_capabilities`, `files`, `link_counts`, `locks`, `nsm_client_registrations`, `parent_child_map`, `pending_writes`, `server_config`, `server_epoch`, `shares`

Listed in the `backupTables` slice (keep in sync with migrations).

## Test Coverage

| Test | What it locks |
|------|---------------|
| `TestBackupRoundTrip_EmptyStore` | Backup produces a valid archive on a fresh store; restore into a second fresh store succeeds; PayloadIDSet is empty. |
| `TestBackupRoundTrip_WithFiles` | Payload-id set size and membership round-trip exactly; file count round-trips across backup + restore. |
| `TestBackupDeterministic` | Two backups of the same data produce byte-identical `tables/*.bin` entries (manifest excluded, since `created_at` is wall-clock). |
| `TestRestore_RejectsSchemaMismatch` | Manifest with a bogus schema version → Restore returns an `errors.Is`-equal `ErrSchemaVersionMismatch` AND the destination remains empty (D-06 gate safety). |
| `TestRestore_RejectsNonEmptyDestination` | Restore into a pre-populated target → `ErrRestoreDestinationNotEmpty`; destination file count unchanged. |
| `TestBackupable_CompileTimeAssertion` | Named test that surfaces the `var _ metadata.Backupable = (*postgres.PostgresMetadataStore)(nil)` assertion in `go test` output. |

All six pass against a live Postgres 16 container. Tests skip cleanly when `DITTOFS_TEST_POSTGRES_DSN` is unset.

## Test Isolation

Each test creates a database named `dittofs_backup_test_<uuid>` via the admin `postgres` DB, runs `AutoMigrate`, and in `t.Cleanup` terminates lingering backends and `DROP DATABASE IF EXISTS`. This keeps the existing `TestConformance` suite (which uses the single `dittofs_test` DB) unaffected and allows `go test -count=N -parallel M` without collisions.

Env-var overrides for non-standard Postgres deployments: `DITTOFS_TEST_POSTGRES_{HOST,PORT,USER,PASSWORD,SSLMODE}`. The primary `DITTOFS_TEST_POSTGRES_DSN` remains the on/off signal (matching `postgres_conformance_test.go`).

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing critical functionality] Restore must TRUNCATE singleton migration rows**
- **Found during:** Task 3 — `TestBackupRoundTrip_EmptyStore` first run.
- **Issue:** Migrations seed `server_config` and `filesystem_capabilities` with singleton rows at startup. A COPY FROM STDIN into a freshly migrated destination therefore hit `ERROR: duplicate key value violates unique constraint "filesystem_capabilities_pkey"` and the archive could never be restored.
- **Fix:** `TRUNCATE TABLE <all-tables> RESTART IDENTITY CASCADE` inside the restore transaction, AFTER the D-06 empty-check but BEFORE the COPY loop. This mirrors `pg_restore --clean`. The D-06 gate still observes user-visible state (`files` rows), so the semantic "refuse to overwrite live data" holds.
- **File:** `pkg/metadata/store/postgres/backup.go` (Restore function).
- **Commit:** `dd8f24e2`.

**2. [Rule 3 - Blocking] Test env-var overrides for non-standard Postgres port**
- **Found during:** Task 3 verification run.
- **Issue:** The test helper initially matched `postgres_conformance_test.go` by hardcoding port 5432. Local development used port 54321 (a container mapped away from the host's default postgres).
- **Fix:** Optional `DITTOFS_TEST_POSTGRES_{HOST,PORT,USER,PASSWORD,SSLMODE}` env vars, layered on top of the DSN on/off signal. Zero impact on existing CI behavior (defaults unchanged when the overrides are unset).
- **File:** `pkg/metadata/store/postgres/backup_test.go` (`loadPostgresEnv`).
- **Commit:** `dd8f24e2`.

**3. [Plan deviation - seeding helper] Skip top-level CreateShare in the test seeder**
- **Found during:** Task 3 verification run.
- **Issue:** `PostgresMetadataStore.CreateShare` (shares.go:76) does a standalone `INSERT INTO shares (share_name, options)` without populating `root_file_id`, which is `NOT NULL`. Calling `CreateShare` before `CreateRootDirectory` fails. `CreateRootDirectory` already performs `INSERT INTO shares ... ON CONFLICT DO UPDATE` with the correct `root_file_id`.
- **Fix:** `seedShareWithFiles` now calls only `CreateRootDirectory`, mirroring the production ShareService boot sequence.
- **Note:** This is a pre-existing inconsistency in the postgres store (`TestConformance/FileOps` also fails for the same reason — see deferred-items.md in this phase for the tracking note). Out of scope for ENG-02.
- **File:** `pkg/metadata/store/postgres/backup_test.go` (`seedShareWithFiles`).
- **Commit:** `dd8f24e2`.

### Additions over the Plan

- **`TestRestoreSentinelsDistinct`** in `pkg/metadata/backup_test.go` — a ~10-LOC meta-test locking the new sentinels as distinct, non-nil, and self-`errors.Is`-equal. Added for symmetry with the existing `TestErrBackupUnsupportedIs`.
- **`quoteIdents` vector helper** — tiny utility used by the `TRUNCATE` path; keeps the call site declarative.
- **`extractTableBlobs` and `rewriteManifestSchemaVersion`** test helpers — both are necessary for the determinism and schema-mismatch tests respectively; both live in the test file only.

## Success Criteria Checklist

- [x] 3 tasks executed and committed individually (`f5ca8bfa`, `c0d3c1ba`, `dd8f24e2`)
- [x] SUMMARY.md committed at `.planning/phases/02-per-engine-backup-drivers/02-04-SUMMARY.md`
- [x] `pkg/metadata/store/postgres/backup.go` exists and implements `metadata.Backupable`
- [x] `pgx.RepeatableRead` + `pgx.ReadOnly` present in backup.go (lines 153–154)
- [x] `COPY ... TO STDOUT (FORMAT binary)` and `COPY ... FROM STDIN (FORMAT binary)` both present
- [x] `schema_migration_version` field in manifest struct (line 102); `metadata.ErrSchemaVersionMismatch` returned from restore path (line 285)
- [x] `metadata.ErrRestoreDestinationNotEmpty` returned from restore path (line 295)
- [x] Compile-time assertion: `var _ metadata.Backupable = (*PostgresMetadataStore)(nil)` (line 39)
- [x] `go test -tags=integration ./pkg/metadata/store/postgres/... -run TestBackup -count=1` — ALL PASS (verified against Postgres 16 container)
- [x] `go build ./...` and `go vet ./...` clean

## Threat Surface Scan

No new network endpoints, auth paths, file access patterns, or schema changes at trust boundaries. The driver consumes existing `pgxpool.Pool` credentials; no new credential material is introduced.

The one file-system-adjacent concern — `readBackupArchive` does `io.ReadAll(tr)` per tar entry — is bounded only by caller-supplied reader bytes. For Phase 5 restore orchestration, this is fine because the outer Phase 3 destination driver will wrap the reader with a SHA-256 verification pass BEFORE this driver touches it. If direct consumers appear outside Phase 5, they should wrap with `io.LimitReader` (same note as Phase 1 Plan 03 manifest `ReadFrom`).

## Known Stubs

None. The driver is complete; no placeholder returns or TODO wiring.

## Deferred Issues

See `.planning/phases/02-per-engine-backup-drivers/deferred-items.md` — one pre-existing port-collision in `TestAPIServer_Lifecycle` unrelated to this plan.

## How Phase 3 Consumes This Driver

```go
// In the Phase 3 destination driver (pseudo-code):
if b, ok := store.(metadata.Backupable); ok {
    payloadIDs, err := b.Backup(ctx, pipeWriter)
    // destination writer computes SHA-256 while forwarding to local/S3
    // returns manifest with payload_id_set populated from payloadIDs
} else {
    return metadata.ErrBackupUnsupported
}
```

## How Phase 5 Consumes This Driver

```go
// In the Phase 5 restore orchestrator (pseudo-code):
err := targetStore.Restore(ctx, verifiedTarStream)
switch {
case errors.Is(err, metadata.ErrSchemaVersionMismatch):
    return http.StatusConflict, "backup schema version does not match this binary"
case errors.Is(err, metadata.ErrRestoreDestinationNotEmpty):
    return http.StatusConflict, "destination not empty — disable shares and retry"
case err != nil:
    return http.StatusInternalServerError, err.Error()
}
```

## Self-Check: PASSED

- [x] `pkg/metadata/store/postgres/backup.go` — FOUND
- [x] `pkg/metadata/store/postgres/backup_test.go` — FOUND
- [x] `pkg/metadata/backup.go` — sentinels FOUND
- [x] Commit `f5ca8bfa` — FOUND
- [x] Commit `c0d3c1ba` — FOUND
- [x] Commit `dd8f24e2` — FOUND
- [x] `go build ./...` — PASS
- [x] `go vet -tags=integration ./pkg/metadata/store/postgres/...` — clean
- [x] `go test -tags=integration -run 'TestBackup|TestRestore' ./pkg/metadata/store/postgres/...` — 6/6 PASS against Postgres 16 container
- [x] `go test ./pkg/metadata/...` — PASS (no regressions on unit suite)
