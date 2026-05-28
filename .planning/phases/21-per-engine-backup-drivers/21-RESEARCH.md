# Phase 21: Per-Engine Backup Drivers - Research

**Researched:** 2026-05-27
**Domain:** Go metadata store serialization (gob, BadgerDB KV iteration, PostgreSQL COPY)
**Confidence:** HIGH

## Summary

Phase 21 implements the `Backupable` interface (shipped in Phase 20) for all three metadata store backends: memory (gob), badger (custom KV streaming), and postgres (COPY TO/FROM). Each driver must serialize its full metadata state into the shared envelope format, extract all unique block hashes referenced by file entries, and pass the 5-subtest conformance suite (`RoundTrip`, `ConcurrentWriter`, `Corruption`, `NonEmptyDest`, `HashSetCorrectness`).

The foundation is solid and well-defined. Phase 20 shipped the `Backupable` interface (`pkg/metadata/backupable.go`), the envelope writer/reader (`pkg/metadata/backup/envelope.go`), the `HashSet` type (`pkg/blockstore/hashset.go`), and the conformance suite (`pkg/metadata/storetest/backup_conformance.go`). The CONTEXT.md has locked 9 implementation decisions covering serialization format, hash extraction strategy, empty-store detection, schema versioning, and PR shape.

**Primary recommendation:** Implement three independent `backup.go` files, one per store package, each using the store's native snapshot primitive for consistency (mu.RLock for memory, db.View for badger, REPEATABLE READ txn for postgres). Extract hashes inline during serialization for single-pass efficiency.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- **D-01:** Memory store uses gob to dump full in-memory state under `mu.RLock()`. Single `gob.Encode` call including shares, files, parents, children, linkCounts, symlinkTargets, payloadIDs, deviceNumbers, and transient session state (locks, clients, durable handles).
- **D-03:** Postgres uses `COPY TO STDOUT` / `COPY FROM STDIN` with CSV format per table inside a single `REPEATABLE READ` transaction. Table name + row count header before each section. Restore in dependency order.
- **D-04:** Hash extraction inline during serialization (single pass inside the same snapshot transaction). Memory: scan files map `Blocks` field. Badger: decode `f:` entries for Blocks. Postgres: separate `COPY (SELECT DISTINCT hash FROM file_block_refs)` query inside same txn.
- **D-05:** Postgres hash extraction uses a dedicated `COPY` query, not a JOIN during the files COPY.
- **D-06:** Empty-store detection via shares count > 0. Memory: `len(shares) > 0`. Badger: seek `s:` prefix. Postgres: `SELECT EXISTS(SELECT 1 FROM shares)`.
- **D-07:** Schema version as LE uint32 at payload start (first 4 bytes after envelope header), starting at 1. Restore reads version first, returns `ErrSchemaVersionMismatch` if unknown. Each driver manages version independently.
- **D-08:** Self-contained per-driver code. Each `backup.go` is independent with no shared driver-level helpers. All import `pkg/metadata/backup` (envelope) and `pkg/blockstore` (HashSet).
- **D-09:** Single PR with staged commits: (1) memory backup.go, (2) badger backup.go, (3) postgres backup.go, (4) conformance test wiring.

### Claude's Discretion
- **D-02:** Badger serialization format -- custom KV stream recommended for portability; may use Badger built-in if benchmarks show significant advantage.
- Minor implementation details within each driver (buffer sizes, iteration order, error wrapping style).

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope.
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|------------------|
| DRV-01 | Memory store implements `Backupable` -- gob round-trip under `mu.RLock()` with hash extraction from `files` map | Memory store struct fully mapped (14+ maps). Gob can encode all Go map types natively. `mu.RLock()` provides snapshot isolation for the ConcurrentWriter test. Hash extraction from `fileData.Attr.Blocks` field. |
| DRV-02 | Badger store implements `Backupable` -- custom streaming inside single `db.View()` with hash extraction from file entries | All 21 key prefixes catalogued. `db.View()` provides MVCC snapshot isolation. Custom length-prefixed KV format avoids Badger version coupling. File entries at `f:` prefix are JSON-encoded `metadata.File` with `Blocks` field. |
| DRV-03 | Postgres store implements `Backupable` -- `COPY TO/FROM` inside single `REPEATABLE READ` txn with hash extraction from `file_block_refs` | 15 tables identified. pgx v5 `PgConn().CopyTo(ctx, w, sql)` / `CopyFrom(ctx, r, sql)` provide streaming COPY. REPEATABLE READ isolation via `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`. Hash extraction via `COPY (SELECT DISTINCT hash FROM file_block_refs) TO STDOUT`. |
| DRV-04 | All three drivers pass the shared conformance suite | Existing conformance test patterns documented for all three stores. `RunBackupConformanceSuite` accepts `BackupableStoreFactory`. Memory tests run unconditionally; Badger uses `//go:build integration`; Postgres skips on missing `DITTOFS_TEST_POSTGRES_DSN`. |
</phase_requirements>

## Architectural Responsibility Map

| Capability | Primary Tier | Secondary Tier | Rationale |
|------------|-------------|----------------|-----------|
| Memory backup/restore | Memory store package | -- | Gob serialization of in-memory maps under RLock |
| Badger backup/restore | Badger store package | -- | Custom KV iteration inside db.View() MVCC snapshot |
| Postgres backup/restore | Postgres store package | -- | COPY TO/FROM via pgx raw connection in REPEATABLE READ txn |
| Hash extraction | Each store package | blockstore.HashSet | Each driver extracts from its own data structures, writes to shared HashSet |
| Envelope wrapping | pkg/metadata/backup | -- | Shared across all drivers (Phase 20 foundation) |
| Conformance testing | pkg/metadata/storetest | Each store's test file | Suite is generic; each store wires its factory |

## Standard Stack

### Core (already in project)
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| encoding/gob | stdlib | Memory store serialization | Native Go binary encoding, handles maps/structs/slices natively [VERIFIED: Go stdlib] |
| github.com/dgraph-io/badger/v4 | v4.5.2 | Badger KV store with MVCC snapshots | Already the project's Badger dependency [VERIFIED: go.mod] |
| github.com/jackc/pgx/v5 | v5.7.6 | PostgreSQL driver with COPY support | Already the project's Postgres dependency [VERIFIED: go.mod] |
| encoding/binary | stdlib | Schema version uint32 LE encoding | Standard binary encoding [VERIFIED: Go stdlib] |
| hash/crc32 | stdlib | CRC32 for envelope (used via backup.Writer) | Already used by envelope.go [VERIFIED: source code] |

### Supporting (already in project)
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/marmos91/dittofs/pkg/metadata/backup | local | Envelope format (NewWriter, ReadHeader, VerifyCRC) | Every Backup/Restore call |
| github.com/marmos91/dittofs/pkg/blockstore | local | HashSet type and ContentHash | Hash collection during backup |
| github.com/marmos91/dittofs/pkg/metadata/storetest | local | RunBackupConformanceSuite | Conformance test wiring |

**No new external dependencies required.** All libraries are already in go.mod.

## Package Legitimacy Audit

> No new packages to install. All dependencies already exist in go.mod.

| Package | Registry | Age | Downloads | Source Repo | slopcheck | Disposition |
|---------|----------|-----|-----------|-------------|-----------|-------------|
| encoding/gob | Go stdlib | 15+ yrs | N/A | golang/go | N/A | Approved (stdlib) |
| encoding/binary | Go stdlib | 15+ yrs | N/A | golang/go | N/A | Approved (stdlib) |

**Packages removed due to slopcheck [SLOP] verdict:** none
**Packages flagged as suspicious [SUS]:** none

## Architecture Patterns

### System Architecture Diagram

```
                    Backup Flow
                    ===========

  Caller (future snapshot orchestrator)
       |
       v
  store.(metadata.Backupable)  <-- type assertion
       |
       +-- Backup(ctx, w) --> (HashSet, error)
       |       |
       |       v
       |   backup.NewWriter(w, engineTag)  <-- envelope header
       |       |
       |       v
       |   [Schema Version uint32 LE]       <-- first 4 payload bytes
       |       |
       |       v
       |   [Engine-specific serialization]  <-- gob / KV stream / COPY
       |       |    |
       |       |    +--> HashSet.Add(hash)  <-- inline extraction
       |       |
       |       v
       |   envWriter.Finish()               <-- trailing CRC32
       |
       +-- Restore(ctx, r) --> error
               |
               v
           backup.ReadHeader(r)             <-- verify magic + version
               |
               v
           backup.VerifyEngine(tag, want)   <-- engine tag match
               |
               v
           [Schema Version uint32 LE]       <-- read + validate
               |
               v
           [Engine-specific deserialization] <-- gob / KV stream / COPY
               |
               v
           backup.VerifyCRC(r, acc)         <-- integrity check
```

### Recommended Project Structure

```
pkg/metadata/store/
  memory/
    backup.go             # NEW: Backup + Restore on MemoryMetadataStore
    memory_conformance_test.go  # MODIFIED: add RunBackupConformanceSuite call
  badger/
    backup.go             # NEW: Backup + Restore on BadgerMetadataStore
    badger_conformance_test.go  # MODIFIED: add RunBackupConformanceSuite call
  postgres/
    backup.go             # NEW: Backup + Restore on PostgresMetadataStore
    postgres_conformance_test.go  # MODIFIED: add RunBackupConformanceSuite call
```

### Pattern 1: Envelope + Schema Version Protocol

**What:** Every driver follows the same envelope + version protocol for both backup and restore.
**When to use:** Every Backup/Restore implementation.

**Backup flow:**
```go
// Source: pkg/metadata/backup/envelope.go (Phase 20)
func (s *SomeStore) Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error) {
    // 1. Create envelope writer
    envW, err := backup.NewWriter(w, "engine-tag")
    if err != nil {
        return nil, fmt.Errorf("backup: create envelope: %w", err)
    }

    // 2. Write schema version (first 4 bytes of payload)
    var vBuf [4]byte
    binary.LittleEndian.PutUint32(vBuf[:], schemaVersion)
    if _, err := envW.Write(vBuf[:]); err != nil {
        return nil, fmt.Errorf("backup: write schema version: %w", err)
    }

    // 3. Engine-specific serialization + hash extraction
    hs := blockstore.NewHashSet(0)
    // ... serialize data, Add() hashes to hs ...

    // 4. Finalize envelope (writes trailing CRC)
    if err := envW.Finish(); err != nil {
        return nil, fmt.Errorf("backup: finish envelope: %w", err)
    }

    return hs, nil
}
```

**Restore flow:**
```go
func (s *SomeStore) Restore(ctx context.Context, r io.Reader) error {
    // 1. Check destination is empty (D-06)
    if !s.isEmpty(ctx) {
        return metadata.ErrRestoreDestinationNotEmpty
    }

    // 2. Read and validate envelope header
    engineTag, payloadR, acc, err := backup.ReadHeader(r)
    if err != nil {
        return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
    }

    // 3. Verify engine tag
    if err := backup.VerifyEngine(engineTag, "engine-tag"); err != nil {
        return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
    }

    // 4. Read schema version (first 4 bytes of payload)
    var vBuf [4]byte
    if _, err := io.ReadFull(payloadR, vBuf[:]); err != nil {
        return fmt.Errorf("%w: read schema version: %v", metadata.ErrRestoreCorrupt, err)
    }
    version := binary.LittleEndian.Uint32(vBuf[:])
    if version != schemaVersion {
        return fmt.Errorf("%w: got %d, want %d", metadata.ErrSchemaVersionMismatch, version, schemaVersion)
    }

    // 5. Engine-specific deserialization
    // ... restore data from payloadR ...

    // 6. Verify CRC (MUST use original reader, NOT payloadR)
    if err := backup.VerifyCRC(r, acc); err != nil {
        return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
    }

    return nil
}
```

### Pattern 2: Memory Store Gob Serialization (D-01)

**What:** Gob-encode a snapshot struct containing all in-memory maps under `mu.RLock()`.
**When to use:** Memory Backup/Restore.

Key insight from codebase analysis: the `MemoryMetadataStore` has 14+ distinct data maps plus lazy sub-stores. The gob snapshot must capture:

| Map/Field | Type | Purpose |
|-----------|------|---------|
| `shares` | `map[string]*shareData` | Share configs + root handles |
| `files` | `map[string]*fileData` | File metadata (contains `Attr.Blocks` for hash extraction) |
| `parents` | `map[string]metadata.FileHandle` | Parent-child uplinks |
| `children` | `map[string]map[string]metadata.FileHandle` | Directory entries |
| `linkCounts` | `map[string]uint32` | Hard link counts |
| `deviceNumbers` | `map[string]*deviceNumber` | Block/char device info |
| `pendingWrites` | `map[string]*metadata.WriteOperation` | In-flight writes |
| `serverConfig` | `metadata.MetadataServerConfig` | Global config |
| `capabilities` | `metadata.FilesystemCapabilities` | FS capabilities |
| `objectIndex` | `map[blockstore.ContentHash]string` | ObjectID secondary index |
| `rollupOffsets` | `map[string]uint64` | Rollup offset persistence |
| `synced` | `map[blockstore.ContentHash]time.Time` | Synced hash markers |
| `storeID` | `string` | Engine-persistent ID |
| `lockStore` | `*memoryLockStore` | NLM/SMB locks (transient) |
| `clientStore` | `*memoryClientStore` | NSM clients (transient) |
| `durableStore` | `*memoryDurableStore` | SMB durable handles (transient) |

**Critical detail:** The internal types (`shareData`, `fileData`, `deviceNumber`) and sub-store types (`memoryLockStore`, `memoryClientStore`, `memoryDurableStore`) are unexported. Gob needs exported fields. The snapshot struct should use exported wrapper types or the backup.go file lives in the same package (it does -- `package memory`) so it has access to unexported fields. However, gob requires exported struct fields. **Solution:** define an exported snapshot struct in `backup.go` with exported field names that mirror the internal maps, and populate it from the unexported fields under the lock.

**Gob limitation:** `gob` cannot encode `map[ContentHash]struct{}` directly because `ContentHash` is `[32]byte` and gob handles fixed-size arrays as keys. Actually gob CAN encode maps with array keys -- this works in Go. But gob cannot encode `interface{}` values (used in `ServerConfig.CustomSettings`). Need to handle that via JSON pre-encoding or custom gob registration.

```go
// backup.go in package memory

type memorySnapshot struct {
    Shares        map[string]*shareData
    Files         map[string]*fileData
    Parents       map[string]metadata.FileHandle
    Children      map[string]map[string]metadata.FileHandle
    LinkCounts    map[string]uint32
    DeviceNumbers map[string]*deviceNumber
    ServerConfig  metadata.MetadataServerConfig
    Capabilities  metadata.FilesystemCapabilities
    StoreID       string
    // ... sub-stores, rollup, synced, objectIndex ...
}

func (s *MemoryMetadataStore) Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    // Build snapshot from locked state
    snap := &memorySnapshot{ /* copy all maps */ }

    // Extract hashes while still under lock
    hs := blockstore.NewHashSet(0)
    for _, fd := range s.files {
        for _, br := range fd.Attr.Blocks {
            hs.Add(br.Hash)
        }
    }

    // Write envelope + version + gob
    envW, _ := backup.NewWriter(w, "memory")
    // write version uint32
    gob.NewEncoder(envW).Encode(snap)
    envW.Finish()

    return hs, nil
}
```

### Pattern 3: Badger Custom KV Streaming (D-02 recommendation)

**What:** Iterate all keys by prefix inside `db.View()`, write length-prefixed key+value pairs.
**When to use:** Badger Backup/Restore.

Complete key prefix inventory from codebase (21 prefixes across 7 files):

| Prefix | Source File | Purpose |
|--------|------------|---------|
| `f:` | encoding.go | File data (JSON) |
| `p:` | encoding.go | Parent relationships |
| `c:` | encoding.go | Children map entries |
| `s:` | encoding.go | Share configs |
| `l:` | encoding.go | Link counts |
| `d:` | encoding.go | Device numbers |
| `cfg:` | encoding.go | Server config singleton |
| `cap:` | encoding.go | Filesystem capabilities |
| `obj:` | encoding.go | ObjectID secondary index |
| `fb:` | objects.go | FileBlock primary records |
| `fb-hash:` | objects.go | FileBlock hash index |
| `fb-local:` | objects.go | FileBlock local presence |
| `fb-file:` | objects.go | FileBlock per-file index |
| `lock:` | locks.go | Lock primary records |
| `lkfile:` | locks.go | Lock by-file index |
| `lkowner:` | locks.go | Lock by-owner index |
| `lkclient:` | locks.go | Lock by-client index |
| `srvepoch` | locks.go | Server epoch singleton |
| `synced:` | synced_hash_store.go | Synced hash markers |
| `ro:` | rollup.go | Rollup offsets |
| `nsm:client:` | clients.go | NSM client registrations |
| `nsm:monname:` | clients.go | NSM monitor name index |
| `dh:id:` | durable_handles.go | Durable handle primary |
| `dh:cguid:` | durable_handles.go | Durable handle by CreateGuid |
| `dh:appid:` | durable_handles.go | Durable handle by AppInstanceId |
| `dh:fid:` | durable_handles.go | Durable handle by FileID |
| `dh:fh:` | durable_handles.go | Durable handle by FileHandle |
| `dh:share:` | durable_handles.go | Durable handle by share |

**Recommended approach:** Rather than iterating by each prefix individually, use a single full-DB iteration inside `db.View()` (Badger MVCC snapshot). Write every key-value pair with length-prefixed framing:

```
[key_len: uint32 LE][key bytes][value_len: uint32 LE][value bytes]
```

Terminated by a sentinel `key_len = 0`. This is simpler and captures everything without needing to maintain a prefix whitelist.

For hash extraction: during the iteration, when a key starts with `f:`, decode the JSON value and extract `Blocks[].Hash` into the HashSet.

For restore: iterate the stream and `txn.Set()` each key-value pair in a new Badger DB.

**Badger db.View() provides MVCC snapshot isolation** -- reads see a consistent point-in-time view regardless of concurrent writes, satisfying the ConcurrentWriter conformance test. [VERIFIED: badger source code and documentation]

### Pattern 4: Postgres COPY Streaming (D-03)

**What:** Use `COPY TO STDOUT` / `COPY FROM STDIN` per table inside a `REPEATABLE READ` transaction.
**When to use:** Postgres Backup/Restore.

Tables to back up (dependency order for restore):

1. `server_config` (no FK deps)
2. `filesystem_capabilities` (no FK deps)
3. `files` (no FK deps)
4. `shares` (FK: files.id)
5. `parent_child_map` (FK: files.id x2)
6. `link_counts` (FK: files.id)
7. `pending_writes` (FK: files.id)
8. `file_block_refs` (FK: files.id)
9. `file_blocks` (FK: references file data)
10. `locks` (no FK to files -- uses TEXT file_id)
11. `server_epoch` (no FK deps)
12. `nsm_client_registrations` (no FK deps)
13. `durable_handles` (no FK deps)
14. `rollup_offsets` (no FK deps)
15. `synced_hashes` (no FK deps)

**pgx v5 COPY API** (verified from `go doc`):
```go
// Acquire a raw connection for COPY operations
conn, err := s.pool.Acquire(ctx)
raw := conn.Conn().PgConn()

// Set isolation level
_, err = raw.Exec(ctx, "BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ").ReadAll()

// COPY TO for each table
tag, err := raw.CopyTo(ctx, writer, "COPY files TO STDOUT WITH (FORMAT csv, HEADER true)")

// Hash extraction
tag, err := raw.CopyTo(ctx, hashWriter, "COPY (SELECT DISTINCT hash FROM file_block_refs) TO STDOUT WITH (FORMAT binary)")

// Commit
_, err = raw.Exec(ctx, "COMMIT").ReadAll()
```

**Restore** uses `COPY FROM STDIN` in dependency order, preceded by table truncation.

**REPEATABLE READ isolation** in Postgres provides snapshot isolation -- the transaction sees a frozen view at start time. This satisfies the ConcurrentWriter conformance test. [VERIFIED: PostgreSQL documentation]

### Anti-Patterns to Avoid

- **Using Badger's built-in `db.Backup()`:** Couples the backup format to Badger's internal wire protocol. Format changes across Badger versions would break restore. Custom KV streaming is portable and version-independent.
- **Reading the entire payload with `io.ReadAll` on the tee reader:** The envelope's `ReadHeader` returns a tee reader that accumulates CRC. Using `io.ReadAll` on it will consume the trailing CRC bytes into the CRC accumulator, corrupting the verification. Drivers must track payload size and read exactly that many bytes.
- **Writing schema version inside the envelope header:** Version goes INSIDE the payload (first 4 bytes after envelope header), not as part of the envelope. The envelope has its own version field for the wire format.
- **Forgetting to handle `encoding/gob` limitations with `interface{}` values:** `metadata.MetadataServerConfig.CustomSettings` is `map[string]any`. Gob cannot encode `interface{}` directly. Pre-encode to JSON bytes or register concrete types.
- **Postgres COPY with TEXT format for binary data:** `file_block_refs.hash` is BYTEA. CSV format hex-encodes it automatically, but the restore path must handle the hex decoding. Using `FORMAT binary` for the hash-only extraction query avoids this.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Envelope framing + CRC | Custom wire format | `backup.NewWriter` / `ReadHeader` / `VerifyCRC` | Already shipped in Phase 20; handles magic, version, engine tag, CRC32 |
| Hash deduplication | Custom set logic | `blockstore.NewHashSet` + `Add` | O(1) dedup, sorted output for manifest, already tested |
| Conformance testing | Per-driver test suites | `storetest.RunBackupConformanceSuite` | 5 subtests already written; factory pattern isolates drivers |
| Badger snapshot isolation | Custom locking | `db.View()` MVCC | Built-in to Badger; read-only txn sees consistent snapshot |
| Postgres snapshot isolation | Custom locking | `REPEATABLE READ` txn | Built-in to PostgreSQL; standard SQL isolation level |
| Gob encoding | Custom binary serialization | `encoding/gob` | Handles Go maps, structs, slices natively; zero schema definition needed |

**Key insight:** Phase 20 did the hard architectural work (envelope, conformance suite, error sentinels). Phase 21 is pure implementation within well-defined contracts.

## Common Pitfalls

### Pitfall 1: Gob Cannot Encode `map[string]any` (ServerConfig.CustomSettings)
**What goes wrong:** `gob.Encode` panics or returns error on `interface{}` values unless concrete types are registered.
**Why it happens:** `metadata.MetadataServerConfig.CustomSettings` is `map[string]any`. Gob needs concrete type info for interface values.
**How to avoid:** Pre-encode `CustomSettings` to `[]byte` (JSON) in the snapshot struct. On restore, decode back to `map[string]any`.
**Warning signs:** `gob: type not registered for interface` error.

### Pitfall 2: Envelope Tee Reader Consumes CRC Bytes
**What goes wrong:** `io.ReadAll(payloadReader)` reads past the payload and consumes the trailing 4-byte CRC into the CRC accumulator. Then `VerifyCRC` fails because there are no bytes left to read.
**Why it happens:** The envelope has no payload-length field. The tee reader from `ReadHeader` passes ALL subsequent bytes through the CRC accumulator.
**How to avoid:** Each driver must know its payload size. For gob/badger: write a payload-length prefix before the payload. For postgres: write table count + per-table row counts as headers. Read exactly the payload bytes from payloadReader, then call `VerifyCRC(r, acc)` on the ORIGINAL reader `r`.
**Warning signs:** `ErrCRCMismatch` or `ErrTruncated` during restore of a valid backup.

### Pitfall 3: Memory Snapshot Gob Field Visibility
**What goes wrong:** Gob silently skips unexported struct fields, producing an incomplete backup.
**Why it happens:** `fileData`, `shareData`, `deviceNumber` have a mix of exported and unexported fields. Gob only encodes exported fields.
**How to avoid:** Define a dedicated snapshot struct in `backup.go` with all-exported fields. Populate it by copying from the internal types under the lock. Restore: copy back from snapshot to internal types.
**Warning signs:** Restored store has empty maps or missing data despite passing the Corruption test.

### Pitfall 4: Postgres COPY Binary vs CSV Format Mismatch
**What goes wrong:** Backup uses CSV format but restore attempts binary import, or vice versa.
**Why it happens:** Different `FORMAT` options in `COPY TO` vs `COPY FROM` SQL.
**How to avoid:** Use `FORMAT csv, HEADER true` consistently for both directions. The header row aids debugging and format validation.
**Warning signs:** `ERROR: invalid input syntax` or garbled data on restore.

### Pitfall 5: Badger Transaction Size Limits
**What goes wrong:** Restore fails with `badger: txn too big` when inserting all keys in a single `db.Update()`.
**Why it happens:** Badger limits transaction size. Large databases may have millions of key-value pairs.
**How to avoid:** Use `db.NewWriteBatch()` for bulk writes, or split into batched `db.Update()` calls with a configurable batch size (e.g., 10000 entries).
**Warning signs:** `ErrTxnTooBig` error during restore of a large backup.

### Pitfall 6: Postgres FK Constraint Violation on Restore
**What goes wrong:** `COPY FROM` into `shares` fails because referenced `files.id` rows don't exist yet.
**Why it happens:** Restore order doesn't respect foreign key dependencies.
**How to avoid:** Restore tables in dependency order: `files` before `shares`, `shares` before nothing, etc. Or temporarily defer constraints: `SET CONSTRAINTS ALL DEFERRED` at transaction start.
**Warning signs:** `ERROR: insert or update on table "shares" violates foreign key constraint`.

## Code Examples

### Example 1: Memory Backup (D-01)

```go
// Source: Derived from codebase analysis of store.go + backupable.go

const memoryEngineTag = "memory"
const memorySchemaVersion = uint32(1)

func (s *MemoryMetadataStore) Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error) {
    if err := ctx.Err(); err != nil {
        return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
    }

    s.mu.RLock()
    defer s.mu.RUnlock()

    // Build snapshot + extract hashes under lock
    snap := s.buildSnapshot()
    hs := blockstore.NewHashSet(0)
    for _, fd := range s.files {
        for _, br := range fd.Attr.Blocks {
            hs.Add(br.Hash)
        }
    }

    // Write envelope
    envW, err := backup.NewWriter(w, memoryEngineTag)
    if err != nil {
        return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
    }

    // Write schema version
    var vBuf [4]byte
    binary.LittleEndian.PutUint32(vBuf[:], memorySchemaVersion)
    if _, err := envW.Write(vBuf[:]); err != nil {
        return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
    }

    // Gob-encode snapshot
    if err := gob.NewEncoder(envW).Encode(snap); err != nil {
        return nil, fmt.Errorf("%w: gob encode: %v", metadata.ErrBackupAborted, err)
    }

    if err := envW.Finish(); err != nil {
        return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
    }

    return hs, nil
}
```

### Example 2: Badger Empty-Store Detection (D-06)

```go
// Source: Derived from encoding.go prefix constants

func (s *BadgerMetadataStore) hasShares() bool {
    var found bool
    _ = s.db.View(func(txn *badger.Txn) error {
        opts := badger.DefaultIteratorOptions
        opts.Prefix = []byte(prefixShare)
        opts.PrefetchValues = false
        it := txn.NewIterator(opts)
        defer it.Close()
        it.Rewind()
        found = it.Valid()
        return nil
    })
    return found
}
```

### Example 3: Postgres COPY + REPEATABLE READ (D-03)

```go
// Source: Derived from pgx v5 go doc + pool_helpers.go patterns

func (s *PostgresMetadataStore) Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error) {
    conn, err := s.pool.Acquire(ctx)
    if err != nil {
        return nil, fmt.Errorf("%w: acquire conn: %v", metadata.ErrBackupAborted, err)
    }
    defer conn.Release()

    raw := conn.Conn().PgConn()

    // Start REPEATABLE READ transaction
    _, err = raw.Exec(ctx, "BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ").ReadAll()
    if err != nil {
        return nil, fmt.Errorf("%w: begin txn: %v", metadata.ErrBackupAborted, err)
    }
    defer func() { _, _ = raw.Exec(ctx, "ROLLBACK").ReadAll() }()

    // Write envelope + version...
    // COPY each table to the envelope writer...
    // Extract hashes via COPY (SELECT DISTINCT hash FROM file_block_refs)...

    _, _ = raw.Exec(ctx, "COMMIT").ReadAll()
    return hs, nil
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| v0.13.0 backup (deleted in Phase 20) | Phase 20 Backupable interface + envelope | 2026-05-27 | Clean foundation with conformance suite |
| Badger `db.Backup()` built-in | Custom KV stream (D-02 recommendation) | Phase 21 | Portability across Badger versions |
| Postgres `pg_dump` | COPY TO/FROM inside app (D-03) | Phase 21 | No external tool dependency; streaming; txn-scoped |

**Deprecated/outdated:**
- `internal/cli/backupfmt/`: Deleted in Phase 20 (CLN-01)
- v0.13.0 backup phases 01-07: Archived to `.planning/milestones/v0.13.0-archive/`

## Assumptions Log

| # | Claim | Section | Risk if Wrong |
|---|-------|---------|---------------|
| A1 | Gob can encode `map[[32]byte]struct{}` (ContentHash as map key) | Memory Pattern | If wrong, need custom encoding for objectIndex and synced maps |
| A2 | Badger `db.View()` full iteration performance is acceptable for databases with millions of keys | Badger Pattern | If slow, may need prefix-batched iteration with progress reporting |
| A3 | pgx v5 `PgConn().CopyTo/CopyFrom` works correctly within an existing transaction started via raw `Exec("BEGIN...")` | Postgres Pattern | If wrong, may need to use pgx's `Begin()` API + raw `COPY` SQL instead |

## Open Questions

1. **Payload size tracking for CRC verification**
   - What we know: The envelope has no payload-length field. `ReadHeader` returns a tee reader. `VerifyCRC` must read from the original reader after the payload is exhausted.
   - What's unclear: How does each driver know when the payload ends and the 4-byte CRC begins?
   - Recommendation: Each driver should include its own payload-length prefix (e.g., uint64 LE after the schema version). On restore, read exactly that many bytes from the tee reader. Alternatively, buffer the entire payload and use `bytes.Buffer` length. The gob decoder naturally stops at the end of the encoded data; the badger KV stream uses the key_len=0 sentinel; postgres can use table-count + row-count headers.

2. **Memory store sub-store lazy initialization**
   - What we know: `lockStore`, `clientStore`, `durableStore` are initialized lazily (nil until first use). `fileBlockData` is also lazy.
   - What's unclear: Should backup include nil sub-stores? Should restore initialize them?
   - Recommendation: Backup should check for nil and skip. Restore should leave them nil (lazy init will create them on demand). Only non-nil sub-stores need serialization.

3. **Postgres table cleanup before COPY FROM**
   - What we know: Restore must target an empty store (D-06 check). But the store may have been opened with migrations that seed `server_config` and `filesystem_capabilities`.
   - What's unclear: Should restore TRUNCATE these seeded tables before COPY FROM?
   - Recommendation: After the non-empty check, TRUNCATE all metadata tables before COPY FROM. The seeded singleton rows will be replaced by the backup's versions.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing + testify (assert/require) |
| Config file | none (Go convention) |
| Quick run command | `go test ./pkg/metadata/store/memory/ -run TestBackupConformance -v` |
| Full suite command | `go test ./pkg/metadata/store/memory/ ./pkg/metadata/store/badger/ ./pkg/metadata/store/postgres/ -run TestBackupConformance -v -tags integration` |

### Phase Requirements to Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| DRV-01 | Memory backup/restore round-trip with hashes | unit (conformance suite) | `go test ./pkg/metadata/store/memory/ -run TestBackupConformance -v` | Wave 0 (add to existing test file) |
| DRV-02 | Badger backup/restore round-trip with hashes | integration (conformance suite) | `go test ./pkg/metadata/store/badger/ -run TestBackupConformance -v -tags integration` | Wave 0 (add to existing test file) |
| DRV-03 | Postgres backup/restore round-trip with hashes | integration (conformance suite) | `go test ./pkg/metadata/store/postgres/ -run TestBackupConformance -v -tags integration` | Wave 0 (add to existing test file) |
| DRV-04 | All three pass full conformance suite | unit+integration | All three commands above | Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./pkg/metadata/store/memory/ -run TestBackupConformance -v` (memory runs fast, no infra needed)
- **Per wave merge:** `go test -race ./pkg/metadata/store/memory/ -run TestBackupConformance -v` + badger integration
- **Phase gate:** Full suite green including postgres (if DSN available) before verify-work

### Wave 0 Gaps
- None -- test infrastructure exists. Only need to add `RunBackupConformanceSuite` calls to the three existing `*_conformance_test.go` files. The conformance suite itself was shipped in Phase 20.

## Security Domain

### Applicable ASVS Categories

| ASVS Category | Applies | Standard Control |
|---------------|---------|-----------------|
| V2 Authentication | no | N/A -- backup is an internal store operation, not user-facing |
| V3 Session Management | no | N/A |
| V4 Access Control | no | N/A -- access control is at the orchestrator level (Phase 23+) |
| V5 Input Validation | yes | Schema version validation on restore; envelope CRC verification; engine tag matching |
| V6 Cryptography | no | CRC32 is integrity-only (not cryptographic); encryption deferred to v0.17.0 (ADV-01) |

### Known Threat Patterns for This Phase

| Pattern | STRIDE | Standard Mitigation |
|---------|--------|---------------------|
| Corrupted backup stream | Tampering | CRC32 Castagnoli envelope verification (Phase 20) |
| Schema version mismatch | Tampering | `ErrSchemaVersionMismatch` sentinel on unknown version |
| Restore into populated store | Information Disclosure | `ErrRestoreDestinationNotEmpty` check before any writes |
| Engine tag confusion | Tampering | `backup.VerifyEngine()` rejects wrong engine type |

## Sources

### Primary (HIGH confidence)
- `pkg/metadata/backupable.go` -- Backupable interface and error sentinels [VERIFIED: source code]
- `pkg/metadata/backup/envelope.go` -- Envelope wire format and Writer/Reader [VERIFIED: source code]
- `pkg/metadata/storetest/backup_conformance.go` -- Conformance suite (5 subtests) [VERIFIED: source code]
- `pkg/blockstore/hashset.go` -- HashSet type [VERIFIED: source code]
- `pkg/metadata/store/memory/store.go` -- MemoryMetadataStore struct (14+ maps) [VERIFIED: source code]
- `pkg/metadata/store/badger/encoding.go` -- Key namespace prefixes [VERIFIED: source code]
- `pkg/metadata/store/badger/store.go` -- BadgerMetadataStore struct [VERIFIED: source code]
- `pkg/metadata/store/postgres/store.go` -- PostgresMetadataStore struct [VERIFIED: source code]
- `pkg/metadata/store/postgres/pool_helpers.go` -- Connection pool helpers and beginTx [VERIFIED: source code]
- `pkg/metadata/store/postgres/file_block_refs.go` -- file_block_refs CRUD [VERIFIED: source code]
- `pkg/metadata/store/postgres/migrations/*.up.sql` -- Full Postgres schema (15 tables) [VERIFIED: source code]
- `go doc github.com/jackc/pgx/v5/pgconn.PgConn.CopyTo` -- pgx COPY API [VERIFIED: go doc]
- `go doc github.com/jackc/pgx/v5/pgconn.PgConn.CopyFrom` -- pgx COPY API [VERIFIED: go doc]
- Badger prefix constants from `objects.go`, `locks.go`, `clients.go`, `durable_handles.go`, `synced_hash_store.go`, `rollup.go` [VERIFIED: source code grep]

### Secondary (MEDIUM confidence)
- Go `encoding/gob` behavior with array keys and interface{} values [ASSUMED -- needs validation in implementation]

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all dependencies already in project, verified from go.mod and source code
- Architecture: HIGH -- Phase 20 foundation fully analyzed, all store internals mapped, conformance suite understood
- Pitfalls: HIGH -- identified from direct code analysis (gob limitations, envelope tee reader, FK ordering, Badger txn size)

**Research date:** 2026-05-27
**Valid until:** 2026-06-27 (stable -- internal project code, no external API drift)
