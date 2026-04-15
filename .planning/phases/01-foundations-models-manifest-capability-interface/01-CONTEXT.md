# Phase 1 Context: Foundations — Models, Manifest, Capability Interface

**Phase:** 1
**Milestone:** v0.13.0 Metadata Backup & Restore (issue #368)
**Captured:** 2026-04-15
**Requirements covered:** REPO-01..05, ENG-04, SAFETY-03

## Phase Goal

Stable schema and contracts that every downstream phase depends on:
- GORM entities for backup repos, backup records, and backup jobs
- Versioned, self-describing manifest format (v1)
- `Backupable` capability interface that metadata stores opt into
- New `BackupStore` sub-interface on the control plane composite `Store`

No drivers, no scheduler, no CLI, no REST API in this phase.

## Locked Decisions

### Models & Schema

- **Backup repo is a first-class GORM entity** in a new `backup_repos` table.
  - FK → `metadata_store_configs.id` (one store → many repos, enables 3-2-1)
  - Columns: `id` (UUID, matches existing convention), `metadata_store_id`, `name`, `kind` (local|s3), `config` (JSON blob for destination-specific fields, mirrors `MetadataStoreConfig.Config` pattern), `schedule` (cron string, nullable), `keep_count` (INT nullable), `keep_age_days` (INT nullable), `encryption_enabled` (bool), `encryption_key_ref` (string: env var name or file path, actual key never stored), timestamps.
  - **Unique constraint:** `(metadata_store_id, name)` — repo names are unique per store, not globally. Operators can reuse `local` or `primary` across stores.
- **Backup records** live in a new `backup_records` table.
  - FK → `backup_repos.id`
  - Columns: `id` (ULID — sortable, meaningful-to-ops timestamp prefix), `repo_id`, `created_at`, `size_bytes`, `status` (pending|running|succeeded|failed|interrupted), `pinned` (bool, retention never prunes pinned records), `manifest_path` (relative key/path in the repo), `sha256`, `store_id` snapshot (guard against restoring into the wrong store), error message.
- **Backup jobs** live in a new `backup_jobs` table — **one table, `kind` enum column** (`backup` | `restore`), not two tables.
  - Unified state machine, one polling endpoint, one interrupted-job recovery path.
  - Columns: `id` (ULID), `kind`, `repo_id`, `backup_record_id` (nullable — set when kind=restore), `status` (pending|running|succeeded|failed|interrupted), `started_at`, `finished_at`, `error`, `progress` (optional %).
- **Retention policy encoding:** structured columns (`keep_count`, `keep_age_days`) on `backup_repos` directly. NOT a JSON blob, NOT a separate table. Keeps retention pass queries simple and validated at bind time.

### Manifest Format (v1)

- **Encoding: YAML** (via `gopkg.in/yaml.v3`, already in go.mod). Human-readable for operator debugging; consistent with other config formats in the project.
- **REST API responses remain JSON** — serialize struct to YAML for storage, JSON for API.
- Required fields in `manifest.yaml`:
  - `manifest_version: 1`
  - `backup_id` (ULID)
  - `created_at` (RFC3339)
  - `store_id` — FK snapshot to prevent cross-store restores
  - `store_kind` (memory|badger|postgres) — driver must match on restore
  - `sha256` — checksum of the payload archive
  - `size_bytes`
  - `encryption` — `{enabled: bool, algorithm: "aes-256-gcm", key_ref: "..."}` (never the key itself)
  - `payload_id_set` — list of PayloadIDs referenced by this backup's metadata contents (SAFETY-01 requirement for block-GC hold)
  - `engine_metadata` — opaque per-engine fields (e.g. Badger version, Postgres schema version)

### Backupable Capability Interface

- **Stream-based signature** — matches Badger's native `DB.Backup` and pgx `CopyTo` directly; destination drivers own buffering/chunking.

```go
// pkg/metadata/backup.go
type Backupable interface {
    // Backup streams a consistent snapshot to w. The returned PayloadIDSet
    // records all block payload refs present at snapshot time (for GC hold).
    Backup(ctx context.Context, w io.Writer) (PayloadIDSet, error)
    // Restore reloads from r. Caller guarantees store is drained (no active shares).
    Restore(ctx context.Context, r io.Reader) error
}
```

- Each metadata store implementation (`memory`, `badger`, `postgres`) implements `Backupable` in Phase 2.
- Stores that cannot implement it return a typed `ErrBackupUnsupported` error on capability check (REQ ENG-04).

### Control Plane Store Topology

- **New `BackupStore` sub-interface** — 10th sub-interface alongside `UserStore`, `GroupStore`, `ShareStore`, `PermissionStore`, `MetadataStoreConfigStore`, `BlockStoreConfigStore`, `AdapterStore`, `SettingsStore`, `GuestStore`.
- Embedded in the composite `Store` interface.
- Groups CRUD for `backup_repos`, `backup_records`, and `backup_jobs`.
- Phase 1 delivers the interface + GORM implementation + migrations; no API handlers yet.

### API & UI Scope

- **All REST API handlers and CLI commands deferred to Phase 6.**
- Phase 1 validates models and interfaces via unit tests only.
- No `dfsctl` commands, no HTTP routes, no API client changes in this phase.

## Dependencies & Constraints

- Adds `github.com/oklog/ulid/v2` (standard Go ULID library) unless already in go.mod. Verify in research.
- Reuses `gopkg.in/yaml.v3` (already present).
- Database migration must be forward-compatible — existing stores get no default repo rows; operators opt in.

## What Downstream Phases Get From Phase 1

- **Phase 2 (engines):** `Backupable` interface to implement; knows signature is stream-based, must compute and return `PayloadIDSet`.
- **Phase 3 (destinations):** manifest schema to write, knows to place `manifest.yaml` last in S3 two-phase commit.
- **Phase 4 (scheduler):** `backup_repos.schedule` + `keep_count` + `keep_age_days` columns available.
- **Phase 5 (restore):** `backup_records.store_id` + manifest `store_id` for cross-store guard; `backup_jobs` recovery on startup.
- **Phase 6 (CLI+API):** `BackupStore` sub-interface to wrap; job polling via `backup_jobs`.

## Deferred Ideas (not this phase, not this milestone)

- Structured retention policy beyond count+age (GFS) — deferred to `GFS-01`
- External KMS integration for encryption keys — deferred
- Cross-engine restore via JSON IR — deferred to `XENG-01`
- Incremental backup manifest fields — deferred to `INCR-01` (manifest v1 already versioned for forward-compat)

## Open Questions for Research Phase

- Confirm `oklog/ulid/v2` vs `segmentio/ksuid` vs custom — pick the one already most aligned with go.mod dependencies.
- Validate GORM migration compatibility for both SQLite and PostgreSQL (project supports both for control plane).
- Confirm `pgx` binary COPY round-trip for enums/jsonb/timestamptz covers all metadata store tables (spike may be needed before Phase 2).
