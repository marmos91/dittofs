# Phase 21: Per-Engine Backup Drivers - Context

**Gathered:** 2026-05-27
**Status:** Ready for planning
**GH issue:** [#643](https://github.com/marmos91/dittofs/issues/643)
**Milestone:** v0.16.0 Share Snapshots — Phase 2 of 6
**Depends on:** Phase 20 (Backupable interface + conformance suite + cleanup)

<domain>
## Phase Boundary

Implement `Backupable` for all three metadata store backends (memory, badger, postgres), each using its engine's native snapshot primitives for consistent serialization and hash extraction. All three must pass the full conformance suite shipped in Phase 20.

**In scope:**
- Memory store `Backup`/`Restore` via gob under `mu.RLock()`
- Badger store `Backup`/`Restore` via custom streaming inside `db.View()`
- Postgres store `Backup`/`Restore` via `COPY TO/FROM` inside `REPEATABLE READ` txn
- Hash extraction from `FileAttr.Blocks` inside each engine's snapshot transaction
- Conformance suite wiring for all three drivers

**Out of scope:**
- Snapshot records, manifest I/O, GC hold (Phase 22)
- Snapshot orchestration and sync gate (Phase 23)
- Restore flow (Phase 24)
- CLI/REST API (Phase 25)

</domain>

<decisions>
## Implementation Decisions

### Serialization format — Memory
- **D-01:** Full state dump via gob. Gob-encode the entire in-memory state under `mu.RLock()`: shares, files, parents, children, linkCounts, symlinkTargets, payloadIDs, deviceNumbers — everything needed to reconstruct a functional store. Single `gob.Encode` call. Transient session state (lock store, client store, durable handles) included for completeness.

### Serialization format — Badger
- **D-02:** Claude's discretion — custom KV stream vs Badger built-in. Recommend custom KV stream (iterate all prefixed keys inside `db.View()`, write length-prefixed key+value pairs) for portability and Badger-version independence. Avoid coupling backup format to Badger's internal wire format.

### Serialization format — Postgres
- **D-03:** COPY TO/FROM with CSV format. `COPY TO STDOUT` for each metadata table inside a single `REPEATABLE READ` transaction. Write table name + row count header before each section. Restore: `COPY FROM STDIN` per table in dependency order inside a transaction.

### Hash extraction
- **D-04:** Inline during serialization. As each file/row is serialized, extract `BlockRef` hashes and `Add()` to HashSet. Single pass over the data, still inside the same atomic snapshot. Memory: scan files map entries for `Blocks`. Badger: decode `f:` entries for Blocks field. Postgres: separate `COPY (SELECT DISTINCT hash FROM file_block_refs)` query inside the same `REPEATABLE READ` txn.
- **D-05:** Postgres uses a dedicated `COPY` query for hash extraction (not a JOIN during the files COPY). Clean, single-purpose query. The txn guarantees consistency with the metadata dump.

### Empty-store detection
- **D-06:** Shares count > 0. All three drivers detect non-empty destination by checking if any shares exist. Memory: `len(shares) > 0`. Badger: seek `s:` prefix. Postgres: `SELECT EXISTS(SELECT 1 FROM shares)`. A store with no shares is empty by definition.

### Schema versioning
- **D-07:** uint32 at payload start. First 4 bytes of each driver's payload (after envelope header) are a LE uint32 schema version, starting at 1. Restore reads version first, returns `ErrSchemaVersionMismatch` if unknown. Each driver manages its own version independently.

### Code structure
- **D-08:** Self-contained per driver. Each `backup.go` is independent. All import `pkg/metadata/backup` for envelope ops and `pkg/blockstore` for HashSet, but share no driver-level helper code. Version uint32 read/write is trivial — not worth abstracting.

### PR shape
- **D-09:** Single PR against develop with staged commits: (1) memory backup.go, (2) badger backup.go, (3) postgres backup.go, (4) conformance test wiring. Each commit builds independently.

### Claude's Discretion
- D-02 (Badger serialization format) — custom KV stream recommended for portability; Claude may use Badger built-in if benchmarks show significant advantage
- Minor implementation details within each driver (buffer sizes, iteration order, error wrapping style)

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements and design
- `.planning/REQUIREMENTS.md` §Per-engine backup drivers (DRV) — DRV-01..04 are Phase 21's requirements
- `.planning/ROADMAP.md` §Phase 21 — success criteria and files-to-touch list

### Phase 20 foundation (direct dependency)
- `pkg/metadata/backupable.go` — `Backupable` interface definition + 4 error sentinels
- `pkg/metadata/backup/envelope.go` — Shared envelope format (Writer, ReadHeader, VerifyCRC, VerifyEngine)
- `pkg/metadata/storetest/backup_conformance.go` — `RunBackupConformanceSuite` with 5 subtests
- `pkg/blockstore/hashset.go` — `HashSet` type (Add, Contains, Len, ForEach, Sorted, Hashes)
- `.planning/phases/20-backupable-interface-conformance-suite-cleanup/20-CONTEXT.md` — Phase 20 decisions (D-01 through D-21)

### Memory store internals
- `pkg/metadata/store/memory/store.go` — `MemoryMetadataStore` struct with `mu sync.RWMutex`, all in-memory maps (shares, files, parents, children, linkCounts, etc.)
- `pkg/metadata/store/memory/memory_conformance_test.go` — existing conformance test wiring pattern

### Badger store internals
- `pkg/metadata/store/badger/store.go` — `BadgerMetadataStore` struct with `db *badger.DB`
- `pkg/metadata/store/badger/encoding.go` — Key namespace prefixes (f:, p:, c:, s:, l:, d:, cfg:, cap:) and encoding helpers
- `pkg/metadata/store/badger/badger_conformance_test.go` — existing conformance test wiring pattern

### Postgres store internals
- `pkg/metadata/store/postgres/store.go` — `PostgresMetadataStore` struct with `pool *pgxpool.Pool`
- `pkg/metadata/store/postgres/file_block_refs.go` — `file_block_refs` table operations
- `pkg/metadata/store/postgres/connection.go` — Connection and transaction management

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/metadata/backup/envelope.go` — `NewWriter`/`ReadHeader`/`VerifyCRC`/`VerifyEngine` for wrapping driver payloads
- `pkg/blockstore/hashset.go` — `HashSet` with `Add`/`Contains` for collecting block hashes
- `pkg/metadata/storetest/backup_conformance.go` — `RunBackupConformanceSuite` + `BackupableStoreFactory`

### Established Patterns
- Envelope wraps opaque driver payload (Phase 20 D-01) — drivers call `backup.NewWriter(w, engineTag)`, write payload, call `Finish()`
- Optional interface with type assertion (`store.(metadata.Backupable)`) — matches `ObjectIDIndexAccessor` pattern
- Conformance suite as exported `Run*Suite` function — mirrors existing `RunConformanceSuite` in storetest
- Badger key prefixes (f:, p:, c:, s:, l:, d:) — iterate by prefix inside `db.View()` for consistent snapshot
- Postgres uses raw pgx (not GORM) — `COPY TO/FROM` uses `pgxpool` connection directly
- Memory store `mu.RLock()` provides snapshot isolation for reads

### Integration Points
- `pkg/metadata/store/memory/backup.go` (new) — implements `Backupable` on `MemoryMetadataStore`
- `pkg/metadata/store/badger/backup.go` (new) — implements `Backupable` on `BadgerMetadataStore`
- `pkg/metadata/store/postgres/backup.go` (new) — implements `Backupable` on `PostgresMetadataStore`
- Each engine's `*_conformance_test.go` — add `RunBackupConformanceSuite` call

</code_context>

<specifics>
## Specific Ideas

No specific requirements — open to standard approaches following established patterns.

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 21-Per-Engine Backup Drivers*
*Context gathered: 2026-05-27*
