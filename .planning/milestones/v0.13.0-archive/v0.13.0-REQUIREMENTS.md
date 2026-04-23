# Requirements: DittoFS v0.13.0 — Metadata Backup & Restore

**Defined:** 2026-04-15
**Source:** Issue #368 + `.planning/research/SUMMARY.md`
**Core Value:** Give operators first-class disaster-recovery for metadata store contents — on-demand + scheduled backups to local FS or S3, with restore and listing from CLI and REST.

## v0.13.0 Requirements

### Repository Model (REPO)

- [x] **REPO-01**: Backup repository is a first-class GORM entity (`backup_repos`) with FK to metadata store (enables multiple repos per store for 3-2-1 strategy)
- [x] **REPO-02**: Each repo stores its own schedule (cron expression, optional) and retention policy
- [x] **REPO-03**: A backup record can be pinned (non-prunable) so retention never deletes it
- [x] **REPO-04**: A metadata store can have multiple backup repos active simultaneously (e.g. local + S3)
- [x] **REPO-05**: Repo config, schedule, and retention are persisted — scheduler and triggers are stateless consumers (per issue #368)

### Backup Drivers & Destinations (DRV)

- [ ] **DRV-01**: Local FS destination driver with atomic tmp+rename semantics on completion
- [ ] **DRV-02**: S3 destination driver with two-phase commit (manifest written last) reusing existing AWS SDK plumbing from `pkg/blockstore/remote/s3`
- [ ] **DRV-03**: Client-side AES-256-GCM encryption at rest for backup payloads, operator-supplied key (env var or file path)
- [ ] **DRV-04**: SHA-256 integrity checksum written into manifest at backup time

### Per-Engine Backup Semantics (ENG)

- [ ] **ENG-01**: BadgerDB store implements native `DB.Backup/Load` with SSI consistent snapshot (online backup, no quiesce required)
- [ ] **ENG-02**: PostgreSQL store implements logical dump via `pgx.CopyTo (FORMAT binary)` under a REPEATABLE READ transaction
- [ ] **ENG-03**: Memory store implements RWMutex-guarded binary dump (for parity and testing)
- [ ] **ENG-04**: Each metadata store implements an optional `Backupable` capability interface; unsupported backends return a clear error

### Scheduler & Retention (SCHED)

- [ ] **SCHED-01**: In-process cron scheduler based on `robfig/cron/v3` with per-repo schedules supporting `CRON_TZ=` timezone prefix
- [ ] **SCHED-02**: Scheduler prevents overlapping runs for the same repo (mutex); adds startup jitter to avoid thundering herd
- [ ] **SCHED-03**: Count-based retention — keep last N successful backups per repo
- [ ] **SCHED-04**: Age-based retention — keep backups ≤ N days per repo
- [ ] **SCHED-05**: Retention never deletes the only successful backup (safety rail)
- [ ] **SCHED-06**: Retention runs as a separate pass after each successful backup; retention does not race with an in-flight backup

### Restore (REST)

- [x] **REST-01**: Restore in-place via drain → close store → restore files → reopen → resume
- [x] **REST-02**: Restore pre-flight REQUIRES all shares referencing the target store to be in disabled state (a disabled share disconnects all clients and refuses new connections until re-enabled) — restore returns 409 Conflict otherwise
- [x] **REST-03**: Restore verifies SHA-256 integrity of the backup manifest before performing any swap
- [x] **REST-04**: Restore latest backup by default; `--from <backup-id>` selects a specific backup
- [x] **REST-05**: Restore is idempotent and safe to retry if interrupted mid-run

### CLI & REST API Surface (API)

- [ ] **API-01**: `dfsctl store metadata <store-name> backup` triggers an on-demand backup using the repo attached to the store (if multiple repos, `--repo <name>`)
- [ ] **API-02**: `dfsctl store metadata <store-name> restore [--from <backup-id>]` restores from the attached repo (requires shares disabled; interactive confirmation unless `--yes`)
- [ ] **API-03**: `dfsctl store metadata <store-name> backup list` shows backup id, timestamp, size, status, repo name
- [ ] **API-04**: `dfsctl store metadata <store-name> repo add/list/remove` manages backup repos on the store (destination config, cron schedule, retention policy)
- [ ] **API-05**: REST API exposes `POST /api/v1/store/metadata/{name}/backups`, `GET /api/v1/store/metadata/{name}/backups`, `POST /api/v1/store/metadata/{name}/restore` with async-job semantics (202 Accepted + `GET /api/v1/store/metadata/{name}/backup-jobs/{id}` for status)
- [ ] **API-06**: Async job records are persisted so clients can poll after disconnect; dittofs-pro UI drives the same endpoints as `dfsctl`

### Safety & GC Integration (SAFETY)

- [x] **SAFETY-01**: Block-store GC consults retained backup manifests before deleting block payloads — a block referenced by any retained backup manifest is held
- [x] **SAFETY-02**: On server startup, orphaned backup jobs in `running` state are transitioned to `interrupted` with a recovery message in the job log
- [ ] **SAFETY-03**: Backup manifest is self-describing and versioned (`manifest_version: 1`) so future versions can forward-compat

## Future Requirements (deferred from v0.13.0)

- **INCR-01**: Incremental backups (BadgerDB since-cursor, Postgres LSN)
- **XENG-01**: Cross-engine restore (JSON IR — Badger → Postgres, etc.)
- **OBS-01**: Prometheus metrics (`backup_last_success_timestamp_seconds`, counters, failure gauges)
- **OBS-02**: OpenTelemetry spans for backup/restore operations
- **KMS-01**: SSE-KMS pass-through for S3 destination
- **REST2NEW-01**: Restore to a new (different) metadata store for staging/forensic use
- **GFS-01**: GFS retention (hourly/daily/weekly/monthly/yearly)
- **AUTO-01**: Automatic test-restore (backup verification job)
- **SCHED-CATCHUP**: Missed-run catch-up policy after server downtime

## Out of Scope

| Feature | Reason |
|---------|--------|
| Continuous PITR / streaming replication | Massive surface, not required for DR; single-node project constraint |
| Multi-node backup coordination (HA) | Project is single-instance per CLAUDE.md |
| Block-store data backup | Block data is in object storage / local cache — out of scope for this milestone (metadata only) |
| External KMS integration (HashiCorp Vault, AWS KMS, etc.) | Operator-supplied key covers v0.13.0; revisit in BlockStore Security milestone |
| Backup to own NFS/SMB mount | Reentrancy trap; not supported |
| Auto-restore on corruption detection | Never silently mutate user state |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| REPO-01 | Phase 1 | Complete |
| REPO-02 | Phase 1 | Complete |
| REPO-03 | Phase 1 | Complete |
| REPO-04 | Phase 1 | Complete |
| REPO-05 | Phase 1 | Complete |
| ENG-04 | Phase 1 | Pending |
| SAFETY-03 | Phase 1 | Pending |
| ENG-01 | Phase 2 | Pending |
| ENG-02 | Phase 2 | Pending |
| ENG-03 | Phase 2 | Pending |
| DRV-01 | Phase 3 | Pending |
| DRV-02 | Phase 3 | Pending |
| DRV-03 | Phase 3 | Pending |
| DRV-04 | Phase 3 | Pending |
| SCHED-01 | Phase 4 | Pending |
| SCHED-02 | Phase 4 | Pending |
| SCHED-03 | Phase 4 | Pending |
| SCHED-04 | Phase 4 | Pending |
| SCHED-05 | Phase 4 | Pending |
| SCHED-06 | Phase 4 | Pending |
| REST-01 | Phase 5 | Complete |
| REST-02 | Phase 5 | Complete |
| REST-03 | Phase 5 | Complete |
| REST-04 | Phase 5 | Complete |
| REST-05 | Phase 5 | Complete |
| SAFETY-01 | Phase 5 | Complete |
| SAFETY-02 | Phase 5 | Complete |
| API-01 | Phase 6 | Pending |
| API-02 | Phase 6 | Pending |
| API-03 | Phase 6 | Pending |
| API-04 | Phase 6 | Pending |
| API-05 | Phase 6 | Pending |
| API-06 | Phase 6 | Pending |

**Coverage:** 33/33 REQ-IDs mapped (7 REPO/ENG-04/SAFETY-03 + 3 ENG + 4 DRV + 6 SCHED + 5 REST + 2 SAFETY + 6 API) ✓
