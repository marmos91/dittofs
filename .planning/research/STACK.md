# Stack Research — Metadata Backup & Restore (v0.13.0 / issue #368)

**Domain:** Go backend service — disaster-recovery subsystem for pluggable metadata stores (memory, BadgerDB, PostgreSQL) with local FS + S3 destinations, scheduling, retention.
**Researched:** 2026-04-15
**Confidence:** HIGH (all recommended additions verified against go.mod and current upstream state)

---

## Executive Summary

This milestone is **mostly additive and low-dependency**. DittoFS already ships every heavy dependency the backup/restore subsystem needs: BadgerDB (`dgraph-io/badger/v4`), PostgreSQL driver (`jackc/pgx/v5` + `gorm.io/driver/postgres`), SQLite (`glebarez/sqlite` + `modernc.org/sqlite` indirect — pure Go, CGO-free), the AWS S3 SDK v2 (`aws-sdk-go-v2/service/s3`), Cobra/Viper CLI, GORM, go-chi HTTP router, and OpenTelemetry.

The **only new direct dependency justified** is a cron-expression scheduler. Everything else (S3 I/O, checksums, compression, JSON manifests, SQLite snapshotting) maps to packages already in the tree or the Go standard library.

**Bottom line — add one dependency (`robfig/cron/v3`), reuse everything else.**

---

## Recommended Stack

### Core Technologies (additions)

| Technology | Version | Purpose | Why recommended for DittoFS |
|------------|---------|---------|-----------------------------|
| `github.com/robfig/cron/v3` | v3.0.1 | In-process cron-expression parser + job runner | De facto Go cron standard (the parser used internally by gocron v2). Tiny, zero transitive deps, supports `CRON_TZ=` prefix and `WithLocation()` for per-schedule timezone, supports optional seconds field. Exactly matches the per-store schedule model described in PROJECT.md. Lower surface area than gocron v2, which adds a Scheduler/Job/Locker abstraction DittoFS does not need (the Runtime already owns lifecycle). |

### Supporting Libraries (all ALREADY in go.mod — reuse only)

| Library | Existing version | Purpose in backup feature | When to use |
|---------|------------------|---------------------------|-------------|
| `github.com/dgraph-io/badger/v4` | v4.5.2 | `DB.Backup(w io.Writer, since uint64)` for consistent online snapshot via Stream framework (SSI snapshot, full + incremental); `DB.Load(r io.Reader, maxPendingWrites)` for restore | Primary path for BadgerDB metadata store backup/restore |
| `github.com/jackc/pgx/v5` | v5.7.6 | `pgx.Conn.CopyTo` / `COPY TO STDOUT (FORMAT binary)` streaming for per-table logical dump; pure-Go alternative to shelling out to `pg_dump` | Primary path for PostgreSQL metadata store backup/restore — keeps DittoFS single-binary, container-image-friendly (no `postgresql-client` required) |
| `gorm.io/gorm` | v1.31.1 | Schema-aware logical export fallback (JSON dump of models) — same approach already used by `dfs backup controlplane --format json` | Portable fallback format; acceptable for memory metadata store (memory backup = GORM-style marshal of in-memory structures) |
| `github.com/glebarez/sqlite` + `modernc.org/sqlite` | v1.11.0 / v1.23.1 (indirect) | `VACUUM INTO 'path'` — already proven in `cmd/dfs/commands/backup/controlplane.go` | Reuse exact pattern if/when a SQLite-backed metadata store appears (currently only used for control-plane DB; the code is a template) |
| `github.com/aws/aws-sdk-go-v2/service/s3` | v1.90.2 | S3 destination driver: `PutObject` / `GetObject` / `ListObjectsV2` / `DeleteObject` / `HeadObject` + multipart upload for large BadgerDB snapshots | Reuse the **same S3 client construction code** as `pkg/blockstore/remote/s3` (endpoint override, path-style, credential chain, TLS config, health-check pattern). DO NOT add a parallel client. |
| `github.com/aws/aws-sdk-go-v2/feature/s3/manager` | (transitively available) | `manager.Uploader` / `manager.Downloader` for streaming multipart I/O of backup archives | Required for >5GiB archives; same package the blockstore already imports |
| `crypto/sha256` (stdlib) | — | Manifest checksums (per-file + whole-archive) | Standard; no dependency added |
| `crypto/aes` + `crypto/cipher` (stdlib) | — | AES-256-GCM for client-side encryption at rest with operator-supplied key | Default for opt-in encryption — zero new deps, FIPS-friendly building blocks |
| `compress/gzip` (stdlib) | — | Compression of JSON/SQL dumps; BadgerDB backup stream is already LSM-compressed so gzip is optional for it | Apply to PostgreSQL + control-plane JSON formats |
| `archive/tar` (stdlib) | — | Bundle multi-file backups (manifest + payload + checksum) into a single portable artifact | Replaces any need for 3rd-party archive libs |
| `gopkg.in/yaml.v3` | v3.0.1 | Manifest format (human-readable, diff-friendly) | Already vendored; use for `manifest.yaml` inside archive |
| `github.com/google/uuid` | v1.6.0 | Backup IDs (UUIDv7 for time-ordered sortability) | Already vendored |
| `go.opentelemetry.io/otel` | v1.37.0 | Trace spans for scheduled backup runs and restore operations | Already wired; add spans in scheduler tick + each driver op |
| `github.com/golang-migrate/migrate/v4` | v4.19.1 | Schema migration for new `backup_repositories` + `backup_runs` tables on the control-plane DB | Already in go.mod; same pattern as existing controlplane migrations |

### Development Tools

| Tool | Purpose | Notes |
|------|---------|-------|
| `testcontainers-go` + `testcontainers-go/modules/postgres` | Integration tests for pgx COPY path and restore round-trips | Already in go.mod — used by existing integration tests |
| Localstack (via docker-compose, as in existing S3 E2E) | S3 destination E2E tests | Already used by `test/e2e/run-e2e.sh --s3` |
| `stretchr/testify` v1.11.1 | Assertions | Already in go.mod |

---

## Installation

```bash
# Only one new direct dependency
go get github.com/robfig/cron/v3@v3.0.1

# Everything else is already present — verify with:
go mod tidy
```

---

## Format Decisions (per metadata store backend)

| Store | Backup format | Tool | Consistency model |
|-------|---------------|------|-------------------|
| **BadgerDB** | Raw Badger backup stream (proprietary binary; `DB.Backup` output) wrapped in a tar envelope with manifest + SHA-256 | `badger.DB.Backup(w, sinceTs)` → tar | SSI snapshot — consistent across concurrent writers without quiescing. `sinceTs` enables incremental backups. |
| **PostgreSQL** | Per-table logical dump via `COPY TO STDOUT (FORMAT binary)` inside a single `REPEATABLE READ` read-only transaction, one file per table, plus schema DDL extracted from `information_schema`/`pg_catalog` | `pgx.Conn.CopyTo` — no shell-out required | MVCC snapshot via transaction isolation |
| **Memory** | GORM-model JSON export (portable) | Custom marshaller, same shape as `dfs backup controlplane --format json` | Trivially consistent — single-goroutine snapshot under store mutex |
| **All** | Envelope: `tar` of `{manifest.yaml, payload/*, checksums.sha256, (optional) payload.aes-gcm}` | stdlib | Atomic: upload to destination under `.inflight/` prefix, rename-to-final on success |

**Rationale for avoiding `pg_dump` as the default:**
- DittoFS containers are distroless/minimal — shelling out requires shipping `postgresql-client` (~40MB) and pinning its major version to match the server.
- `pgx` already has `CopyTo`/`CopyFrom` which round-trips `COPY (FORMAT binary)` streams faithfully and gives structured progress/cancellation hooks.
- The existing `controlplane` backup already has an optional `native-cli` path — retain that as an escape hatch but make the pure-Go `COPY` path the default.

**Rationale for Badger's native backup over custom key iteration:**
- `DB.Backup`/`DB.Load` are officially supported, version-compatible across Badger v4.x, consistent via SSI, and roughly an order of magnitude faster than iterating the key space (Stream framework parallelizes across SSTable boundaries).
- The Stream framework chunks into 4MB batches — ideal for streaming directly into a multipart S3 upload.

---

## Encryption at Rest for Backups

| Option | Verdict | Notes |
|--------|---------|-------|
| **AES-256-GCM with operator-supplied key** (stdlib) | **RECOMMENDED default for client-side encryption** | Zero new deps, streaming via `cipher.NewGCM` + chunked framing (16MB frames with per-frame nonce); key delivered via env var / file / KMS reference. Apply *inside* the tar envelope, before upload. |
| **S3 server-side encryption (SSE-S3, SSE-KMS, SSE-C)** | **RECOMMENDED for defence-in-depth** | Pass `ServerSideEncryption` + `SSEKMSKeyID` through the existing S3 client config — free, no key management burden. Complementary to client-side AES-GCM, not a replacement. |
| **`filippo.io/age`** | **NOT RECOMMENDED for v0.13.0** | Excellent library (v1.3.1, actively maintained) and the right answer for multi-recipient human-to-human encrypted files. For machine-to-machine backups with a single operator-supplied key, raw AES-GCM avoids a new direct dependency and keeps the threat model simple. Reconsider if a future milestone wants multi-recipient key wrapping or SSH-key-based recipients. |

---

## Scheduler Design Guidance

- Use `cron.New(cron.WithLocation(time.UTC), cron.WithParser(cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)))` — supports `@hourly`, `@daily`, `@weekly`, `0 2 * * *`, and `CRON_TZ=Europe/Rome 0 2 * * *` per-schedule.
- Single `*cron.Cron` instance owned by a new `runtime/backups` sub-service, alongside existing `adapters/`, `stores/`, `shares/`, `mounts/`, `lifecycle/`, `identity/` sub-services under `pkg/controlplane/runtime/`.
- Lifecycle: started by `lifecycle.Service.Serve`, stopped by `lifecycle.Service.Shutdown` — drain via `cron.Stop()` which returns a `context.Context` signalling all running jobs finished.
- Each job: look up store by name → resolve its `BackupRepository` → run the driver under a configurable deadline → write a `BackupRun` row → apply retention.
- **No distributed scheduling primitives needed** — DittoFS is explicitly single-instance (per PROJECT.md constraints). DO NOT add `go-co-op/gocron` for Locker/Elector features; they're unused.

---

## Integration Points (existing code to reuse, not reimplement)

| Concern | Existing code to reuse | Location |
|---------|------------------------|----------|
| S3 client + credential chain + endpoint override + health check | `pkg/blockstore/remote/s3` — factor its constructor into a reusable helper | Battle-tested against Localstack + real AWS |
| Tar + manifest pattern | `test/e2e/helpers/backup.go` + `cmd/dfs/commands/backup/controlplane.go` | Shape `manifest.yaml` after existing format for consistency |
| GORM-based JSON dump | `cmd/dfs/commands/backup/controlplane.go` (`--format json`) | Template for memory metadata store |
| VACUUM INTO for SQLite | `cmd/dfs/commands/backup/controlplane.go` (`--format native`) | Template if a SQLite metadata store is added later |
| REST API pattern + JWT | `pkg/controlplane/api/` + `internal/controlplane/api/handlers/` | New handlers: `POST /api/v1/stores/metadata/{name}/backup`, `POST /api/v1/stores/metadata/{name}/restore`, `GET /api/v1/stores/metadata/{name}/backups` |
| CLI pattern (Cobra subcommands) | `cmd/dfsctl/commands/store/metadata/` | Add `backup`, `restore`, `backup list` subcommands alongside existing CRUD |
| GORM migrations | `golang-migrate/migrate/v4` v4.19.1 (already a dep) | Add migration for `backup_repositories` + `backup_runs` tables |
| OpenTelemetry tracing | `go.opentelemetry.io/otel` v1.37.0 | Span names: `backup.run`, `backup.upload`, `backup.prune`, `restore.run`, `restore.download` |
| Prometheus metrics | Existing metrics registry (see PROJECT.md "Production Features") | New counters: `backup_runs_total{store,status}`, `backup_duration_seconds`, `backup_bytes`, `backup_retention_pruned_total` |

---

## What NOT to Use

| Avoid | Why | Use instead |
|-------|-----|-------------|
| `github.com/go-co-op/gocron/v2` | Wraps `robfig/cron/v3` internally and adds Scheduler/Locker/Elector abstractions DittoFS does not need (single-instance). Extra dep, extra surface area. | `github.com/robfig/cron/v3` directly |
| Shelling out to `pg_dump` as default | Requires shipping `postgresql-client` in container image, version coupling to server, no structured error/progress reporting | `pgx.Conn.CopyTo` with `COPY (FORMAT binary)`. Keep CLI path as optional `--format native-cli`, matching the existing controlplane pattern. |
| Shelling out to `pg_basebackup` | File-system-level physical backup; not suitable for per-store logical backup; requires replication role; incompatible with managed PostgreSQL where replication slots are restricted | `pgx` logical dump path |
| `filippo.io/age` | Adds a new direct dep for functionality stdlib AES-GCM covers in ~60 LOC | `crypto/cipher.NewGCM` with streaming chunked framing |
| `github.com/minio/minio-go` as a second S3 client | Parallel S3 client diverges from blockstore behavior (retries, circuit breaker, TLS) | Reuse `pkg/blockstore/remote/s3`'s constructor |
| Custom binary backup format | Fragile across versions, hard to inspect, no tooling | `tar` + `yaml` manifest + raw driver stream (Badger native / pgx COPY binary / GORM JSON) |
| `restic` or `borg` as embedded library | Heavy deps, opinionated repository formats, solve a different problem (multi-source dedup across hosts) | Keep backups store-scoped and store-format-native |
| `go.etcd.io/bbolt` snapshots | Not a metadata backend in DittoFS | N/A |
| Custom in-process ticker instead of cron | Cron expressions are explicitly in the milestone target features | `robfig/cron/v3` |
| `lib/pq` | We already use `jackc/pgx/v5` which is faster and has first-class `COPY` support | `pgx.Conn.CopyTo` |

---

## Alternatives Considered

| Recommended | Alternative | When alternative is better |
|-------------|-------------|----------------------------|
| `robfig/cron/v3` (parser + runner) | `go-co-op/gocron/v2` | If DittoFS ever becomes multi-instance HA and needs distributed lock-based scheduling with electors |
| `pgx` COPY logical dump | `pg_dump` via `exec.Command` | Customer mandates bit-exact `pg_dump` archive format for compliance; keep as `--format native-cli` |
| Badger native `DB.Backup` | Custom key-range iteration + JSON | Never — `DB.Backup` is strictly better; only reach for JSON export if cross-backend portability is explicitly required (use the memory-store GORM-JSON path in that case) |
| Client-side AES-256-GCM | `filippo.io/age` | Multi-recipient encryption, SSH-key-based recipients, or interoperability with `age` CLI users |
| Tar + YAML manifest envelope | Zip, custom binary container | If Windows-native tooling needs to open backups without `tar` — unlikely for a server product |
| Local FS + S3 destinations | Azure Blob, GCS, restic backend | Later milestones if customer demand appears — the destination driver interface should be pluggable from day one so adding a driver is ~200 LOC |

---

## Version Compatibility

| Package A | Compatible with | Notes |
|-----------|-----------------|-------|
| `robfig/cron/v3` v3.0.1 | Go 1.11+ (we're on 1.25.0) | Requires Go modules import path `github.com/robfig/cron/v3`. No transitive deps. Stable since 2019; intentionally feature-frozen. |
| `dgraph-io/badger/v4` v4.5.2 (existing) | `DB.Backup`/`DB.Load` binary format is stable within major version | Cross-major restore (v3→v4) requires a one-shot migration tool, not in scope |
| `jackc/pgx/v5` v5.7.6 (existing) | PostgreSQL 12+ | `COPY (FORMAT binary)` wire format is stable since PG 9.0; matches DittoFS's stated PG support matrix |
| `aws-sdk-go-v2/service/s3` v1.90.2 (existing) | AWS S3, any S3-compatible (Localstack, MinIO, Ceph, Scaleway, Backblaze B2 with `UsePathStyle`) | Same compatibility matrix as existing blockstore S3 driver |
| `gorm.io/gorm` v1.31.1 (existing) | `golang-migrate/migrate/v4` v4.19.1 | Already established pattern in controlplane |

---

## Sources

- [robfig/cron v3 GitHub](https://github.com/robfig/cron) and [pkg.go.dev/github.com/robfig/cron/v3](https://pkg.go.dev/github.com/robfig/cron/v3) — HIGH (verified timezone support via `CRON_TZ=` prefix and `WithLocation()`, zero transitive deps)
- [go-co-op/gocron releases](https://github.com/go-co-op/gocron/releases) — HIGH (confirmed v2 wraps robfig/cron/v3 internally; latest v2.19.1 Jan 2026)
- [BadgerDB v4 docs](https://pkg.go.dev/github.com/dgraph-io/badger/v4) and [Badger backup discussion](https://discuss.dgraph.io/t/how-to-backup-badgerdb/16265) — HIGH (`DB.Backup` uses Stream framework over SSI snapshot, 4MB batching, full + incremental via `sinceTs`)
- [PostgreSQL 18 pg_dump docs](https://www.postgresql.org/docs/current/app-pgdump.html) and [pg_basebackup docs](https://www.postgresql.org/docs/current/app-pgbasebackup.html) — HIGH (pg_dump logical + MVCC-consistent; pg_basebackup physical + replication role required)
- [FiloSottile/age](https://github.com/FiloSottile/age) and [pkg.go.dev/filippo.io/age](https://pkg.go.dev/filippo.io/age) — HIGH (v1.3.1 current; considered and rejected for reasons above)
- Internal verification: `/Users/marmos91/Projects/dittofs-368/go.mod` — all reused dependencies confirmed present at current versions
- Internal verification: `/Users/marmos91/Projects/dittofs-368/cmd/dfs/commands/backup/controlplane.go` — existing VACUUM INTO + GORM-JSON + pg_dump-fallback pattern to mirror

---

*Stack research for: DittoFS metadata backup/restore (v0.13.0, issue #368)*
*Researched: 2026-04-15*
