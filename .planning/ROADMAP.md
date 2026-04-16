# Roadmap: DittoFS v0.13.0 — Metadata Backup & Restore

**Milestone:** v0.13.0
**Issue:** #368
**Created:** 2026-04-15
**Granularity:** fine
**Phases:** 7
**Coverage:** 24/24 requirements mapped

## Phases

- [ ] **Phase 1: Foundations — Models, Manifest, Capability Interface** - GORM entities for repos/records/jobs, manifest v1 spec, `Backupable` capability, `BackupRepoStore` sub-interface
- [ ] **Phase 2: Per-Engine Backup Drivers** - Memory, BadgerDB, and PostgreSQL `Backupable` implementations with atomic consistent snapshots
- [x] **Phase 3: Destination Drivers + Encryption** - Local FS + S3 destination drivers with atomic completion, SHA-256 integrity, and AES-256-GCM encryption at rest (completed 2026-04-16)
- [ ] **Phase 4: Scheduler + Retention** - robfig/cron/v3 scheduler with overlap guard, jitter, count/age retention, pin, and separate post-upload retention pass
- [ ] **Phase 5: Restore Orchestration + Safety Rails** - Quiesce-swap-resume restore, share-disable precondition, manifest verification, interrupted-job recovery, block-GC hold integration
- [ ] **Phase 6: CLI & REST API Surface** - `dfsctl store metadata ... backup/restore/repo` subtree and REST API with async job semantics
- [ ] **Phase 7: Testing & Hardening** - Localstack S3 E2E matrix, corruption/partial-restore coverage, cross-version tests, chaos tests

## Phase Details

### Phase 1: Foundations — Models, Manifest, Capability Interface
**Goal**: Operators and downstream phases have a stable schema and contract — GORM entities for backup repos, records, and jobs; a self-describing versioned manifest format; and a capability interface that metadata stores opt into.
**Depends on**: Nothing (foundation)
**Requirements**: REPO-01, REPO-02, REPO-03, REPO-04, REPO-05, ENG-04, SAFETY-03
**Success Criteria** (what must be TRUE):
  1. Operator can create a `backup_repos` row linked to a metadata store via FK, with schedule and retention policy persisted
  2. A single metadata store can have multiple repos attached simultaneously (local + S3)
  3. A backup record can be marked pinned via a persisted flag
  4. Metadata store implementations declare backup capability via the `Backupable` interface; unsupported backends return a clear error
  5. Backup manifests written by any driver carry `manifest_version: 1` and are self-describing (store_id, checksums, block_store_refs)
**Plans:** 3 plans

Plans:
- [ ] 01-01-PLAN.md — Backup GORM entities (BackupRepo, BackupRecord, BackupJob), sentinel errors, AllModels registration, oklog/ulid/v2 dep
- [x] 01-02-PLAN.md — BackupStore sub-interface + GORMStore impl + integration tests (REPO-01..05, SAFETY-02 surface)
- [x] 01-03-PLAN.md — Manifest v1 YAML codec + Backupable capability interface + PayloadIDSet + ErrBackupUnsupported (SAFETY-03, ENG-04)

### Phase 2: Per-Engine Backup Drivers
**Goal**: Each supported metadata store can produce a consistent point-in-time snapshot and load it back, using the engine's native atomic-snapshot API.
**Depends on**: Phase 1
**Requirements**: ENG-01, ENG-02, ENG-03
**Success Criteria** (what must be TRUE):
  1. BadgerDB store produces a backup using native `DB.Backup/Load` that is restorable while the store serves concurrent writes
  2. PostgreSQL store produces a logical binary dump under a single `REPEATABLE READ` transaction without holding locks against vacuum for longer than the configured budget
  3. Memory store produces an RWMutex-guarded dump and can reload it identically (for parity and tests)
  4. Round-trip (backup → restore → byte-compare) passes for all three engines in unit/integration tests
**Plans:** 4 plans

Plans:
- [x] 02-01-PLAN.md — Phase-2 sentinel errors (ErrRestoreDestinationNotEmpty/Corrupt/SchemaVersionMismatch/BackupAborted) + shared `pkg/metadata/storetest/backup_conformance.go` suite (RoundTrip/ConcurrentWriter/Corruption/NonEmptyDest/PayloadIDSet)
- [x] 02-02-PLAN.md — Memory store Backupable driver: gob-encoded root struct, RWMutex-held same-snapshot PayloadIDSet + encode (D-02, D-05, D-06)
- [x] 02-03-PLAN.md — Badger store Backupable driver: custom streaming inside a single `db.View` txn (D-03 — NOT `DB.Backup`), framed wire format, key_prefix_list defensive check (ENG-01)
- [x] 02-04-PLAN.md — Postgres store Backupable driver: tar-of-COPYs under `REPEATABLE READ` / `READ ONLY` tx with `manifest.yaml` sidecar + schema_migration_version check (ENG-02, D-04)

### Phase 3: Destination Drivers + Encryption
**Goal**: Backups stream to either local filesystem or S3 with atomic completion semantics, SHA-256 integrity, and optional operator-supplied AES-256-GCM encryption.
**Depends on**: Phase 1
**Requirements**: DRV-01, DRV-02, DRV-03, DRV-04
**Success Criteria** (what must be TRUE):
  1. Local FS destination writes to a temp path and atomically renames on success; killing the process mid-write never leaves a published partial archive
  2. S3 destination uses two-phase commit (payload first, manifest last) reusing AWS client plumbing from `pkg/blockstore/remote/s3`
  3. AES-256-GCM encryption can be enabled per-repo with an operator-supplied key (env var or file path); archives are unreadable without the key
  4. Every backup archive records a SHA-256 checksum in the manifest that matches the payload bytes on read-back
**Plans:** 6/6 plans complete

Plans:
- [x] 03-01-PLAN.md — Destination interface + D-07 sentinel errors + Factory/Registry skeleton (DRV-01, DRV-02)
- [x] 03-02-PLAN.md — AES-256-GCM streaming envelope (D-05) + key-ref parser (D-08/D-09) + SHA-256 tee (DRV-03, DRV-04)
- [x] 03-03-PLAN.md — Local FS driver with atomic rename (D-03) + 0600/0700 perms (D-14) + orphan sweep (DRV-01, DRV-03, DRV-04)
- [x] 03-04-PLAN.md — S3 driver with two-phase commit via manager.Uploader (D-02) + orphan+MPU sweep + prefix-collision check (DRV-02, DRV-03, DRV-04)
- [x] 03-05-PLAN.md — Registry wiring: DestinationFactoryFromRepo + explicit RegisterBuiltins (DRV-01, DRV-02)
- [x] 03-06-PLAN.md — Cross-driver conformance suite + docs/BACKUP.md operator guide (DRV-01..04)

### Phase 4: Scheduler + Retention
**Goal**: Scheduled backups run reliably per-repo without overlap, thundering herd, or silent pruner-induced data loss.
**Depends on**: Phases 1, 2, 3
**Requirements**: SCHED-01, SCHED-02, SCHED-03, SCHED-04, SCHED-05, SCHED-06
**Success Criteria** (what must be TRUE):
  1. Operator can set a `CRON_TZ=`-prefixed cron schedule per repo; the in-process scheduler fires at the configured UTC-aware times
  2. Two scheduler ticks on the same repo never produce overlapping runs (per-repo mutex); concurrent cron fires land with randomized jitter
  3. Count-based retention keeps the last N successful backups per repo; age-based retention keeps backups newer than N days
  4. Retention never deletes the only successful backup, and never races with an in-flight upload (runs as a separate pass after upload confirms)
  5. Retention correctly skips pinned records
**Plans:** 5 plans

Plans:
- [x] 04-01-PLAN.md — Framework relocation (pkg/metadata/backup.go → pkg/backup/backupable.go via compat shim) + schema migration (backup_repos.metadata_store_id → target_id + target_kind) + BackupStore method rename + Phase-4 sentinels
- [x] 04-02-PLAN.md — pkg/backup/scheduler: robfig/cron/v3 wrapper + FNV-1a stable per-repo jitter + per-repo OverlapGuard + ValidateSchedule (SCHED-01, SCHED-02)
- [x] 04-03-PLAN.md — pkg/backup/executor: io.Pipe producer/consumer pipeline implementing D-21 sequence (ULID-first, Backupable → Destination.PutBackup, BackupJob + BackupRecord persistence)
- [x] 04-04-PLAN.md — runtime/storebackups/retention.go: D-08..D-17 inline retention (union policy, pinned skip, safety rail, destination-first delete, continue-on-error, 30-day BackupJob pruner) (SCHED-03, SCHED-04, SCHED-05, SCHED-06)
- [x] 04-05-PLAN.md — runtime/storebackups/service.go: 9th sub-service composing scheduler + executor + retention, SAFETY-02 recovery on Serve (D-19), explicit RegisterRepo/UnregisterRepo hot-reload (D-22), unified RunBackup for cron + on-demand (D-23), runtime wiring

### Phase 5: Restore Orchestration + Safety Rails
**Goal**: Operators can safely restore a metadata store in place without corrupting live clients, without losing referenced block data, and without leaving ghost jobs after a crash.
**Depends on**: Phases 1, 2, 3
**Requirements**: REST-01, REST-02, REST-03, REST-04, REST-05, SAFETY-01, SAFETY-02
**Success Criteria** (what must be TRUE):
  1. Restore refuses (409 Conflict) if any share referencing the target store is enabled; operator must disable shares first
  2. Restore verifies the manifest SHA-256 before performing any swap; a corrupted archive aborts before touching live data
  3. Restore drains the store, swaps under a temporary path + atomic rename, reopens, and resumes — leaving the original untouched on failure
  4. `restore` defaults to the latest successful backup; `--from <backup-id>` selects a specific one; retries after interruption are safe
  5. On server startup, any `running` backup/restore jobs with no worker are transitioned to `interrupted` with a recovery message
  6. Block-store GC consults the PayloadID set from retained backup manifests and holds blocks referenced by any retained backup
**Plans**: TBD

### Phase 6: CLI & REST API Surface
**Goal**: Operators drive backup, restore, list, and repo management from `dfsctl` and a REST API that the dittofs-pro UI can consume with async job polling.
**Depends on**: Phases 4, 5
**Requirements**: API-01, API-02, API-03, API-04, API-05, API-06
**Success Criteria** (what must be TRUE):
  1. `dfsctl store metadata <store> backup [--repo <name>]` triggers an on-demand backup and returns a job id immediately
  2. `dfsctl store metadata <store> restore [--from <id>]` prompts for confirmation (unless `--yes`) and refuses if shares are enabled
  3. `dfsctl store metadata <store> backup list` shows id, timestamp, size, status, and repo name across `-o table|json|yaml`
  4. `dfsctl store metadata <store> repo add|list|remove` manages repos (destination config, schedule, retention) on the attached store
  5. REST endpoints `POST /api/stores/metadata/{name}/backups`, `GET /api/stores/metadata/{name}/backups`, `POST /api/stores/metadata/{name}/restore` return 202 + job id; `GET /api/backup-jobs/{id}` polls status
  6. Async job records persist across client disconnect, so dittofs-pro UI can poll the same endpoints as `dfsctl`
**Plans**: TBD
**UI hint**: yes

### Phase 7: Testing & Hardening
**Goal**: Every failure mode that silently corrupts or loses data in production backup systems is covered by an E2E or chaos test before the milestone ships.
**Depends on**: Phases 1–6 (runs in parallel with 5/6)
**Requirements**: (cross-cutting validation — no new REQ-IDs; covers SAFETY-01/02/03, DRV-02, ENG-01/02, REST-02/03)
**Success Criteria** (what must be TRUE):
  1. Localstack-backed E2E matrix exercises happy path × 3 engines × 2 destinations with real multipart uploads
  2. Corruption tests (truncated archive, bit-flip, missing manifest, wrong store_id) all fail cleanly with explicit errors — no panics, no partial restore
  3. Chaos tests (kill server mid-backup, kill mid-restore) leave the system in a recoverable state with no ghost multipart uploads
  4. Restore-while-mounted is rejected in CI; concurrent-write + backup + restore byte-compare passes
  5. Localstack tests use the shared-container helper pattern (not per-test container) to avoid known flakiness
**Plans**: TBD

## Progress

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 1. Foundations — Models, Manifest, Capability Interface | 0/0 | Not started | - |
| 2. Per-Engine Backup Drivers | 0/4 | Not started | - |
| 3. Destination Drivers + Encryption | 6/6 | Complete    | 2026-04-16 |
| 4. Scheduler + Retention | 0/5 | Not started | - |
| 5. Restore Orchestration + Safety Rails | 0/0 | Not started | - |
| 6. CLI & REST API Surface | 0/0 | Not started | - |
| 7. Testing & Hardening | 0/0 | Not started | - |
