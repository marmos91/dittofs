# Phase 2: Per-Engine Backup Drivers - Context

**Gathered:** 2026-04-16
**Status:** Ready for planning
**Requirements covered:** ENG-01, ENG-02, ENG-03 (ENG-04 interface was delivered in Phase 1)

<domain>
## Phase Boundary

Each of the three supported metadata stores (`memory`, `badger`, `postgres`) implements the `metadata.Backupable` interface locked in Phase 1:

```go
Backup(ctx context.Context, w io.Writer) (PayloadIDSet, error)
Restore(ctx context.Context, r io.Reader) error
```

Delivers consistent point-in-time snapshots of the **entire store** using each engine's native atomic-snapshot primitive, with a same-snapshot PayloadIDSet computed for block-GC hold (SAFETY-01).

**Out of scope for this phase:**
- Destination drivers (local FS, S3) — Phase 3
- Manifest writing at the call site — Phase 3 (Phase 2 produces the payload stream + PayloadIDSet; callers wrap)
- Quiesce / swap / share-disable orchestration — Phase 5
- Scheduler, retention, CLI, REST — Phases 4/6
- Incremental backup — deferred (INCR-01)
- Cross-engine restore — deferred (XENG-01)

</domain>

<decisions>
## Implementation Decisions

### D-01 — Backup scope: full DB / all tables per engine

Native engine APIs snapshot the whole store. No per-prefix/per-table allowlisting.

- **Badger:** iterate every key prefix inside one `db.View` txn (`f:`, `p:`, `c:`, `s:`, `l:`, `d:`, `cfg:`, `cap:`, `fsmeta:`, `lock:`, `lkfile:`, `lkowner:`, `lkclient:`, `srvepoch`, `nsm:client:`, `nsm:monname:`, `dh:*`). No exclusion list.
- **Postgres:** dump every table in the metadata schema (files, directories, server_config, filesystem_capabilities, locks, clients, durable_handles, ACLs, plus whatever the migrations add).
- **Memory:** gob-encode every internal map (`files`, `parents`, `children`, `shares`, `linkCounts`, `deviceNumbers`, `pendingWrites`, `serverConfig`, `capabilities`, `sessions`, plus lazily-initialized sub-stores if present at snapshot time).

**Why full-DB:**
- Uses native snapshot primitives verbatim (REQ ENG-01 mandates for Badger)
- No custom exclusion logic that could silently drop data
- Stale session state after restore is indistinguishable from post-crash restart — clients already handle it via grace period / reconnect (NFSv4 boot verifier, SMB durable handle reconnect)

### D-02 — PayloadIDSet computed inside the SAME snapshot as the payload

This is a safety invariant, not a performance choice. The PayloadIDSet must reference **exactly** the payloads the snapshot references — no more, no less. Post-backup scanning is **unsafe** (race window: a file deleted between snapshot and scan loses its payload ref → GC can free a block the backup manifest references → restore produces IO errors → data loss).

Per-engine mechanics:

- **Memory:** hold `mu.RLock()` across both gob-serialize and `files` map walk.
- **Postgres:** single `REPEATABLE READ` txn wraps both `SELECT DISTINCT payload_id FROM files WHERE payload_id IS NOT NULL` and the per-table `COPY TO STDOUT (FORMAT binary)` streams.
- **Badger:** single `db.View(...)` txn. Inside: iterate `prefixFile` for PayloadIDs AND emit the custom-streamed backup payload. See D-03 for why this replaces `DB.Backup`.

### D-03 — Badger driver: custom streaming inside a single `db.View` (not `DB.Backup`)

Badger's `DB.Backup(w, since)` helper opens its own internal read-ts; a separate `db.View` for PayloadID scanning cannot share that timestamp, producing a race window that violates D-02. We preserve Badger's SSI snapshot primitive by driving our own stream:

```go
err := store.db.View(func(txn *badger.Txn) error {
    // 1. Scan prefixFile, build PayloadIDSet from same txn
    // 2. Iterate all key prefixes, emit framed key/value pairs to w
    //    (or use txn.NewStream() with same read-ts if simpler)
    return nil
})
```

We still use Badger's atomic-snapshot primitive — we just bypass the `DB.Backup` wrapper function. REQ ENG-01's intent ("consistent snapshot, safe under concurrent writes") is honored; the literal `DB.Backup` call is not.

Backup wire format: framed key/value records (length-prefixed). `Restore()` iterates the stream and writes into a fresh `badger.DB` via `txn.Set`. No `DB.Load` (which is the counterpart to `DB.Backup`'s envelope).

### D-04 — Postgres serialization: tar-of-COPYs + sidecar manifest

Backup payload is `archive/tar` with:
- `manifest.yaml` — table order, per-table row counts, Postgres server version, schema migration version from `schema_migrations` table
- `tables/<n>-<table_name>.copy` — one entry per table, containing `COPY TO STDOUT (FORMAT binary)` output verbatim

All COPY streams are emitted inside one `REPEATABLE READ` txn (same txn that computed the PayloadIDSet). Table order is deterministic (alphabetical) for reproducible SHA-256 checksums downstream.

Restore: parse tar, verify schema migration version matches current binary's migration set (reject mismatch with `ErrSchemaVersionMismatch`), run `COPY FROM STDIN (FORMAT binary)` per table inside a single transaction, commit atomically.

### D-05 — Memory serialization: `encoding/gob`

Single gob stream of a top-level struct containing every internal map. Deterministic, Go-native, stable across Go versions, trivial round-trip. No frame overhead, no schema evolution concerns (memory store is non-production by definition — for parity + tests).

Restore builds fresh maps from the gob stream and replaces internal fields under write-lock on an **empty** store (see D-06).

### D-06 — Restore contract: require empty destination

`Restore(ctx, r)` errors with `ErrRestoreDestinationNotEmpty` if the store contains any data:

- **Memory:** `len(s.files) > 0` OR `len(s.shares) > 0` → reject.
- **Badger:** any key with prefix `f:` exists → reject.
- **Postgres:** `SELECT EXISTS(SELECT 1 FROM files LIMIT 1)` → reject.

**Why:** defense in depth. Phase 5's orchestrator owns all destruction explicitly (close store → swap under temp path / DROP/CREATE schema / fresh gob target). Phase 2's driver is pure load. A bug in scheduler/test/caller that accidentally invokes `Restore` against a live store errors loudly instead of silently destroying production data. Matches `pg_restore` / `etcdctl snapshot restore --data-dir=new` / `restic` precedent.

### D-07 — Error taxonomy: fail-fast + typed sentinels

Any error aborts the operation. Writer is left in a partial state — Phase 3 (destination drivers) and Phase 5 (restore orchestration) handle atomicity at their layers via tmp+rename / swap-under-temp-path.

Typed errors added to `pkg/metadata/backup.go` (co-located with `ErrBackupUnsupported`):

- `ErrRestoreDestinationNotEmpty` — destination has data; see D-06
- `ErrRestoreCorrupt` — stream decode failed (truncated, bit-flipped, invalid frame)
- `ErrSchemaVersionMismatch` — restore archive was produced by a binary with different schema migrations (PG)
- `ErrBackupAborted` — backup was interrupted by ctx cancellation or engine error mid-stream

Partial restores are **always** fatal — no "best-effort recovery" path. A partial restore mid-transaction is rolled back (PG) or the destination store is expected to be discarded (Badger/Memory).

### D-08 — Conformance tests live in shared suite

New `pkg/metadata/storetest/backup_conformance.go` exercised by each engine's test package. Tests:

1. **Round-trip byte-compare** — populate → Backup → drain to new store → Restore → enumerate all files/attrs/hierarchy → assert identical to source
2. **Concurrent writer during backup** (Badger + Postgres only) — spawn goroutine issuing writes while Backup runs; assert snapshot is consistent (all files referenced by backup's PayloadIDSet match what's enumerable post-restore)
3. **Corruption detection** — truncate archive at various offsets, flip a byte in header/body → Restore must return `ErrRestoreCorrupt`, leaves destination untouched (PG via txn rollback, Badger via tmp dir discard, Memory via no-op on empty)
4. **Non-empty destination rejection** — populate destination, call Restore → must return `ErrRestoreDestinationNotEmpty` without modifying any data
5. **PayloadIDSet correctness** — after Restore, enumerate payload refs in restored store → set must equal the PayloadIDSet returned by Backup

Memory store uses in-process concurrent test. Badger uses tmp-dir + real `badger.Open`. Postgres uses the existing Localstack/docker-compose integration test harness (`-tags=integration`) with shared-container pattern per MEMORY.md.

### D-09 — Engine metadata field in archive

Per-engine metadata travels in the per-engine archive header (Badger framing, PG sidecar `manifest.yaml`, Memory gob root struct field) — **not** in the Phase-1 `manifest.yaml` at the destination layer (that's bolted on in Phase 3).

Required fields:

- **Badger:** `badger_version` (from `badger.DefaultOptions().Compression` + build-time import), `format_version` (Phase-2-internal, starts at 1), `key_prefix_list` (defensive — detects unknown prefixes introduced by future binaries)
- **Postgres:** `pg_server_version`, `schema_migration_version` (from `schema_migrations` table), `table_list` with row counts, `format_version`
- **Memory:** `go_version`, `gob_schema_version` (bumped when the root struct type changes), `format_version`

The Phase-1 top-level `manifest.yaml` will carry this as `engine_metadata: map[string]string` (already defined in `pkg/backup/manifest/manifest.go`) — Phase 3 flattens per-engine headers into that field.

### D-10 — Code layout: one backup.go per store package

- `pkg/metadata/store/memory/backup.go` + `backup_test.go`
- `pkg/metadata/store/badger/backup.go` + `backup_test.go`
- `pkg/metadata/store/postgres/backup.go` + `backup_test.go`
- `pkg/metadata/storetest/backup_conformance.go` — shared conformance suite entry points

**Not** a separate `pkg/backup/engine/` subtree — keeping engine-specific code in each store package preserves colocation with the existing store internals (prefix constants, txn helpers, connection pools), and matches how `lock` / `clients` / `durable_handles` are already split in each store package.

### Claude's Discretion

- Exact gob root-struct shape for Memory store (as long as it round-trips)
- Badger stream framing (length-prefixed vs tar; length-prefixed is simpler)
- Order of tables within the PG tar (as long as deterministic)
- Whether to add progress callback signature (`io.Writer` wrapping) or skip — Phase 6 job polling doesn't need fine-grained progress

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner) MUST read these before planning.**

### Phase 1 lock-ins (binding contracts)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md` — full Phase 1 context
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-02-SUMMARY.md` — BackupStore sub-interface (CRUD for Phase 4/5/6 wiring)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-03-SUMMARY.md` — manifest v1 + Backupable interface + PayloadIDSet + ErrBackupUnsupported
- `pkg/metadata/backup.go` — `Backupable` interface signature, `PayloadIDSet` type, existing `ErrBackupUnsupported` (add new sentinels alongside)
- `pkg/backup/manifest/manifest.go` — manifest v1 struct, `engine_metadata` field target

### Project-level
- `.planning/REQUIREMENTS.md` §ENG — requirements ENG-01, ENG-02, ENG-03 covered by this phase
- `.planning/research/SUMMARY.md` §Phase 02 — pitfall analysis, pgx binary COPY caveat (GAP #1)
- `.planning/PROJECT.md` "Key Decisions" — conventions on single-instance / no clustering

### Engine-specific
- `pkg/metadata/store/badger/store.go` — `BadgerMetadataStore` struct + `db *badger.DB` field
- `pkg/metadata/store/badger/encoding.go` §prefix constants — complete prefix list for full-DB scan (D-01)
- `pkg/metadata/store/badger/locks.go`, `clients.go`, `durable_handles.go` — additional prefix families
- `pkg/metadata/store/postgres/store.go` — `PostgresMetadataStore` struct + `pool *pgxpool.Pool`
- `pkg/metadata/store/postgres/migrations/` — schema_migrations table source for D-04 version check
- `pkg/metadata/store/memory/store.go` — `MemoryMetadataStore` struct + all internal maps for D-05

### Test harnesses
- `pkg/metadata/storetest/suite.go` — existing conformance suite pattern (add `backup_conformance.go` alongside)
- `pkg/controlplane/store/backup_test.go` — reference integration-test layout (build tag, shared helper)

### External (read at plan/execute time)
- BadgerDB v4 `db.View` + Stream framework docs — https://pkg.go.dev/github.com/dgraph-io/badger/v4
- pgx v5 `CopyTo` + `CopyFrom` docs — https://pkg.go.dev/github.com/jackc/pgx/v5
- PostgreSQL binary COPY format — https://www.postgresql.org/docs/current/sql-copy.html (Binary Format section)

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets

- **Per-store prefix/table inventories:** already well-factored in encoding.go (Badger), migrations (PG), field list on struct (Memory). Full-DB iteration needs no new discovery.
- **`pkg/backup/manifest/manifest.go`:** `EngineMetadata map[string]string` field already exists on Manifest — D-09 drops engine-specific info there (string-valued only; complex structures go in per-engine archive headers).
- **`pkg/metadata/storetest/`:** shared conformance suite pattern exists (`suite.go`, `dir_ops.go`, etc.). New `backup_conformance.go` plugs in identically.
- **Existing `dfs backup controlplane` command (`cmd/dfs/commands/backup/controlplane.go`):** precedent for stream-based dumps with `VACUUM INTO` (SQLite) / `pg_dump` — **we do NOT reuse this** (wrong layer, shells out). But read it to understand the operator UX baseline.
- **Localstack shared-container helper** (MEMORY.md, `TestCollectGarbage_S3` fix) — pattern already available for Postgres integration tests; use it for D-08.

### Established Patterns

- **Interface-check via type assertion (ENG-04):** callers do `if b, ok := store.(Backupable); ok { ... }` — this is how Phase 3/5 will invoke drivers. Each store must be assignable to `metadata.Backupable`.
- **Sub-store lazy-init in Badger/PG:** `lockStore`, `clientStore`, `durableStore` are created on first access. Backup scope (D-01) must force-init-or-detect these to avoid missing prefixes; simplest is to walk `db.*` prefixes directly rather than through sub-store getters.
- **Error wrapping with `%w`:** existing store code wraps low-level errors with `fmt.Errorf("failed to X: %w", err)`. New sentinels in D-07 must work with `errors.Is` / `errors.As`.
- **Build tag `//go:build integration` for PG tests** — applies to backup_test.go for postgres too.

### Integration Points

- **Phase 3 (destination drivers) reads:** `Backupable.Backup(w)` as a pure stream producer. Destinations wrap with tar-or-stream envelope around it.
- **Phase 5 (restore orchestration) calls:** `Backupable.Restore(r)` on a fresh store instance that Phase 5 just constructed in a temp directory / temp schema.
- **Phase 5 GC-hold consumes:** the `PayloadIDSet` returned by `Backup` — requires D-02's same-snapshot invariant.
- **Phase 7 (testing) extends:** the conformance suite from D-08 with chaos scenarios (kill mid-backup, kill mid-restore) and cross-version tests.

</code_context>

<specifics>
## Specific Ideas

- User emphasized "reliable and safe" for enterprise/edge NAS DR context. Every gray-area choice defaulted to the conservative option (full-DB over partial, same-snapshot over best-effort, empty-destination requirement over wipe, fail-fast over best-effort).
- PayloadIDSet correctness tied to zero-downtime concern: the feature must not introduce data-loss windows under concurrent writes. D-02 and D-03 jointly close that window for Badger (the trickiest engine).
- Postgres schema-evolution concern drove D-04 (sidecar manifest with schema version) — so a backup taken on binary N+1 fails cleanly on binary N instead of silently restoring into a mismatched schema.

</specifics>

<deferred>
## Deferred Ideas

- **Incremental backups** — Badger's `DB.Backup(w, since)` `since` parameter is ignored (passed as 0). Manifest v1 engine_metadata could carry a `last_ts` for future forward-compat, but no incremental path this phase. (INCR-01 in future milestone.)
- **Cross-engine restore** — JSON IR dumps from one engine restorable into another. Rejected for v0.13.0; manifest's `store_kind` field already enforces engine match. (XENG-01 in future milestone.)
- **Progress reporting per backup** — fine-grained byte/row counters streamed to a progress callback. Deferred to Phase 6 if job-polling UX demands it.
- **Encryption at backup-producer layer** — intentionally deferred to Phase 3 (destination driver). Phase 2 produces cleartext streams; Phase 3 wraps with AES-256-GCM. Keeps driver logic focused, makes it easy to encrypt-at-rest for S3 while keeping local-FS plaintext if operator chooses.
- **External KMS for encryption keys** — deferred outside v0.13.0 entirely.
- **Backup of block payload data itself** — scope-boundary of the milestone (metadata-only).

</deferred>

---

*Phase: 02-per-engine-backup-drivers*
*Context gathered: 2026-04-16*
