# v0.16.0 Requirements — Share Snapshots

**Milestone:** v0.16.0 Share Snapshots
**Parent issue:** [#643](https://github.com/marmos91/dittofs/issues/643)
**Source plan:** `~/.claude/plans/we-have-a-new-memoized-meadow.md`
**Defined:** 2026-05-27

**Core Value:** Enable reference-based share snapshots that capture both metadata and block state. O(metadata-size) snapshot time regardless of data volume, leveraging CAS block immutability for zero-copy block "snapshots" via GC hold.

---

## v0.16.0 Requirements

### Backup interface (ENG)

- [x] **ENG-01**: `Backupable` interface in `pkg/metadata/` with `Backup(ctx, w) (HashSet, error)` and `Restore(ctx, r) error` signatures
- [x] **ENG-02**: `HashSet` type captures all unique `ContentHash` values from `FileAttr.Blocks` inside the same atomic snapshot transaction as the metadata dump
- [x] **ENG-03**: Four typed error sentinels: `ErrRestoreDestinationNotEmpty`, `ErrRestoreCorrupt`, `ErrSchemaVersionMismatch`, `ErrBackupAborted`
- [x] **ENG-04**: Shared conformance suite in `pkg/metadata/storetest/` with 5 subtests: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness

### Per-engine backup drivers (DRV)

- [x] **DRV-01**: Memory store implements `Backupable` — gob round-trip under `mu.RLock()` with hash extraction from `files` map
- [x] **DRV-02**: Badger store implements `Backupable` — custom streaming inside single `db.View()` with hash extraction from file entries
- [x] **DRV-03**: Postgres store implements `Backupable` — `COPY TO/FROM` inside single `REPEATABLE READ` txn with hash extraction from `file_block_refs`
- [x] **DRV-04**: All three drivers pass the shared conformance suite

### Snapshot records and GC hold (SNAP)

- [ ] **SNAP-01**: Snapshot GORM model with ID (ULID), ShareName, State (creating/ready/failed), MetadataEngine, ManifestCount, RemoteDurable, timestamps
- [ ] **SNAP-02**: On-disk layout `<share-data-dir>/snapshots/<id>/metadata.dump` + `manifest.hashes` (sorted hex ContentHash, one per line)
- [ ] **SNAP-03**: `SnapshotHoldProvider` interface extends GC mark phase — active snapshot manifests inject hashes into live set, preventing collection
- [ ] **SNAP-04**: GC correctly skips blocks referenced by active snapshots; deleting a snapshot releases the GC hold
- [ ] **SNAP-05**: Control plane store CRUD for snapshot records (Create, List, Get, Delete)

### Sync gate and orchestration (ORCH)

- [x] **ORCH-01**: `VerifyRemoteDurability` — verify all manifest hashes exist on remote via `Head()` with bounded concurrency
- [x] **ORCH-02**: Snapshot create orchestration: metadata dump → hash manifest → sync gate → record "ready"
- [x] **ORCH-03**: Optional `--no-sync-gate` flag skips remote verification (GC hold still applies)

### Restore (REST)

- [ ] **REST-01**: Reference restore: disable share → verify blocks on remote → close metadata store → create fresh → `Restore()` → re-register → enable
- [ ] **REST-02**: Interrupted restore leaves share disabled with original data intact (no partial state)
- [ ] **REST-03**: Restore verification — after metadata restore, confirm all manifest hashes accessible before enabling share

### CLI and REST API (API)

- [ ] **API-01**: REST endpoints: `POST/GET /shares/{name}/snapshots`, `GET/DELETE /shares/{name}/snapshots/{id}`, `POST /shares/{name}/snapshots/{id}/restore`
- [ ] **API-02**: CLI: `dfsctl share snapshot create <share>`
- [ ] **API-03**: CLI: `dfsctl share snapshot list <share>` with table output
- [ ] **API-04**: CLI: `dfsctl share snapshot show <share> <id>` with detail view
- [ ] **API-05**: CLI: `dfsctl share snapshot delete <share> <id>`
- [ ] **API-06**: CLI: `dfsctl share snapshot restore <share> <id>`
- [ ] **API-07**: API client methods for all snapshot operations

### Cleanup (CLN)

- [x] **CLN-01**: Delete orphaned `internal/cli/backupfmt/` package
- [x] **CLN-02**: Archive old backup planning phases (01-07) to milestones directory

### Documentation (DOC)

- [ ] **DOC-01**: Create `docs/SNAPSHOTS.md` — operator guide covering snapshot model, CLI usage, restore procedures, GC hold semantics, limitations
- [ ] **DOC-02**: Update `docs/ARCHITECTURE.md` — GC section (SnapshotHoldProvider replaces old BackupHoldProvider concept)
- [ ] **DOC-03**: Update `docs/CLI.md` — `dfsctl share snapshot` command tree
- [ ] **DOC-04**: Update `README.md` — snapshot feature description replacing old "backup will ship in v0.16.0" note

## v0.17.0 Requirements (Deferred)

### Portable exports

- **EXP-01**: Full archive export — stream metadata dump + all referenced CAS blocks to destination
- **EXP-02**: Destination drivers — local FS (tmp+rename atomicity) and S3 (two-phase commit)
- **EXP-03**: Import from archive — upload blocks + restore metadata on any server
- **EXP-04**: Incremental export — only ship new hashes since last export

### Advanced features

- **ADV-01**: AES-256-GCM encryption for export archives
- **ADV-02**: Scheduled snapshots with cron expressions
- **ADV-03**: Retention policies (count-based, age-based) with automatic pruning

## Out of Scope

| Feature | Reason |
|---------|--------|
| Portable full exports | v0.17.0 — reference snapshots ship first for fast iteration |
| Encryption at rest | v0.17.0 — destination drivers own encryption, not snapshot layer |
| Scheduled snapshots | v0.17.0 — on-demand is sufficient for v0.16.0 |
| Retention policies | v0.17.0 — manual delete sufficient for v0.16.0 |
| Cross-engine restore | Different problem (migration), out of scope for backup |
| Incremental metadata backup | Full-DB backup via native engine primitives is simpler and more reliable |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| ENG-01 | Phase 20 | Complete |
| ENG-02 | Phase 20 | Complete |
| ENG-03 | Phase 20 | Complete |
| ENG-04 | Phase 20 | Complete |
| DRV-01 | Phase 21 | Complete |
| DRV-02 | Phase 21 | Complete |
| DRV-03 | Phase 21 | Complete |
| DRV-04 | Phase 21 | Complete |
| SNAP-01 | Phase 22 | Pending |
| SNAP-02 | Phase 22 | Pending |
| SNAP-03 | Phase 22 | Pending |
| SNAP-04 | Phase 22 | Pending |
| SNAP-05 | Phase 22 | Pending |
| ORCH-01 | Phase 23 | Complete |
| ORCH-02 | Phase 23 | Complete |
| ORCH-03 | Phase 23 | Complete |
| REST-01 | Phase 24 | Pending |
| REST-02 | Phase 24 | Pending |
| REST-03 | Phase 24 | Pending |
| API-01 | Phase 25 | Pending |
| API-02 | Phase 25 | Pending |
| API-03 | Phase 25 | Pending |
| API-04 | Phase 25 | Pending |
| API-05 | Phase 25 | Pending |
| API-06 | Phase 25 | Pending |
| API-07 | Phase 25 | Pending |
| CLN-01 | Phase 20 | Complete |
| CLN-02 | Phase 20 | Complete |
| DOC-01 | Phase 25 | Pending |
| DOC-02 | Phase 25 | Pending |
| DOC-03 | Phase 25 | Pending |
| DOC-04 | Phase 25 | Pending |

**Coverage:**
- v0.16.0 requirements: 31 total
- Mapped to phases: 31
- Unmapped: 0

---
*Requirements defined: 2026-05-27*
*Last updated: 2026-05-27 after milestone definition*
