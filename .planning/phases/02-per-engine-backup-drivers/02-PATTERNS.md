# Phase 2: Per-Engine Backup Drivers — Pattern Map

**Mapped:** 2026-04-16
**Files analyzed:** 7 new files (3 drivers + 3 driver tests + 1 shared conformance) + 1 extended file (`pkg/metadata/backup.go`)
**Analogs found:** 7 / 7 (100% coverage; no unmapped files)

Each new driver file has a direct structural analog already living in the same package (`locks.go`, `clients.go`, `durable_handles.go` all follow the same "per-feature file" layout). The shared conformance file has a direct analog in `pkg/metadata/storetest/file_block_ops.go`.

---

## File Classification

| New / Modified File | Role | Data Flow | Closest Analog | Match Quality |
|---------------------|------|-----------|----------------|---------------|
| `pkg/metadata/backup.go` (extend) | interface + sentinels | — | existing file itself | self |
| `pkg/metadata/store/memory/backup.go` | driver | streaming (read: map → `w`; write: `r` → maps) | `pkg/metadata/store/memory/locks.go` | role-match + package-peer |
| `pkg/metadata/store/memory/backup_test.go` | test | in-process | `pkg/metadata/store/memory/locks_test.go` | exact |
| `pkg/metadata/store/badger/backup.go` | driver | streaming (read: `txn.NewIterator` → `w`; write: `r` → `txn.Set`) | `pkg/metadata/store/badger/files.go` §`GetFileByPayloadID` (full-prefix scan inside `db.View`) + `pkg/metadata/store/badger/locks.go` (per-feature split) | role-match + stream-pattern |
| `pkg/metadata/store/badger/backup_test.go` | integration test | tmp-dir + real `badger.Open` | `pkg/metadata/store/badger/badger_conformance_test.go` | exact |
| `pkg/metadata/store/postgres/backup.go` | driver | streaming (tar wraps `pgx.Conn.CopyTo` / `CopyFrom` under `REPEATABLE READ` tx) | `pkg/metadata/store/postgres/transaction.go` §`WithTransaction` (tx scaffold) + `pkg/metadata/store/postgres/migrate.go` (migrations / `schema_migrations` access) | role-match; external pgx COPY pattern new to project |
| `pkg/metadata/store/postgres/backup_test.go` | integration test | `//go:build integration` + DSN env var | `pkg/metadata/store/postgres/postgres_conformance_test.go` | exact |
| `pkg/metadata/storetest/backup_conformance.go` | conformance-suite | plug-in via `StoreFactory` | `pkg/metadata/storetest/file_block_ops.go` + `pkg/metadata/storetest/suite.go` | exact |

---

## Pattern Assignments

### `pkg/metadata/backup.go` (extend — add sentinels)

**Analog:** the file itself (existing `ErrBackupUnsupported` at line 63).

**Existing sentinel pattern** (`pkg/metadata/backup.go:61-63`):

```go
// ErrBackupUnsupported is returned by capability checks when a metadata store
// does not implement Backupable (ENG-04).
var ErrBackupUnsupported = errors.New("backup not supported by this metadata store")
```

**What to add (D-07)** — follow the same `var` + sentence-comment pattern, co-located:

```go
// ErrRestoreDestinationNotEmpty is returned by Restore when the destination
// store already contains data (D-06).
var ErrRestoreDestinationNotEmpty = errors.New("restore destination is not empty")

// ErrRestoreCorrupt is returned when the restore stream cannot be decoded
// (truncated, bit-flipped, invalid frame).
var ErrRestoreCorrupt = errors.New("restore stream is corrupt")

// ErrSchemaVersionMismatch is returned by the Postgres driver when the
// archive's schema_migrations version does not match the current binary.
var ErrSchemaVersionMismatch = errors.New("restore archive schema version mismatch")

// ErrBackupAborted is returned when backup is interrupted mid-stream
// (context cancellation or underlying engine error).
var ErrBackupAborted = errors.New("backup aborted")
```

**Existing interface-shape test pattern** (`pkg/metadata/backup_test.go:44-59`): extend with `errors.Is` assertions for the new sentinels (pure `errors.Is(x, x)` round-trip, same spirit as `TestErrBackupUnsupportedIs`).

---

### `pkg/metadata/store/memory/backup.go` (driver, streaming)

**Analog:** `pkg/metadata/store/memory/locks.go` (per-feature file in same package); locking & type-assertion pattern from `pkg/metadata/store/memory/files.go`.

**Imports pattern** (from `pkg/metadata/store/memory/files.go:1-8`):

```go
package memory

import (
    "context"
    "fmt"

    "github.com/marmos91/dittofs/pkg/metadata"
)
```

Add `encoding/gob`, `io`, and `errors` for D-05.

**Compile-time interface assertion** — match the pattern at `pkg/metadata/store/memory/objects.go:43`:

```go
// Ensure MemoryMetadataStore implements metadata.Backupable
var _ metadata.Backupable = (*MemoryMetadataStore)(nil)
```

**Locking pattern — same-snapshot PayloadIDSet + payload (D-02)** — copy the RWMutex discipline from `pkg/metadata/store/memory/files.go:19-38`:

```go
func (store *MemoryMetadataStore) Backup(ctx context.Context, w io.Writer) (metadata.PayloadIDSet, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    store.mu.RLock()
    defer store.mu.RUnlock()

    // --- Walk files to build PayloadIDSet under the same RLock
    ids := metadata.NewPayloadIDSet()
    for _, fd := range store.files {
        if fd.Attr.PayloadID != "" {
            ids.Add(string(fd.Attr.PayloadID))
        }
    }

    // --- Build root struct referencing internal maps (still under RLock)
    root := memoryBackupRoot{
        // GobSchemaVersion, GoVersion, FormatVersion
        // Files:            store.files
        // Parents:          store.parents
        // Children:         store.children
        // Shares:           store.shares
        // LinkCounts:       store.linkCounts
        // DeviceNumbers:    store.deviceNumbers
        // PendingWrites:    store.pendingWrites
        // ServerConfig:     store.serverConfig
        // Capabilities:     store.capabilities
        // Sessions:         store.sessions
        // FileBlockData:    store.fileBlockData (nil-safe)
        // LockStore:        store.lockStore    (nil-safe)
        // ClientStore:      store.clientStore  (nil-safe)
        // DurableStore:     store.durableStore (nil-safe)
    }

    enc := gob.NewEncoder(w)
    if err := enc.Encode(&root); err != nil {
        return nil, fmt.Errorf("gob encode: %w", err)
    }
    return ids, nil
}
```

**Restore empty-destination guard (D-06)** — follow the "check-then-modify" shape in the existing Memory `CreateShare`:

```go
func (store *MemoryMetadataStore) Restore(ctx context.Context, r io.Reader) error {
    if err := ctx.Err(); err != nil {
        return err
    }
    store.mu.Lock()
    defer store.mu.Unlock()

    if len(store.files) > 0 || len(store.shares) > 0 {
        return metadata.ErrRestoreDestinationNotEmpty
    }

    var root memoryBackupRoot
    if err := gob.NewDecoder(r).Decode(&root); err != nil {
        return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
    }

    // Replace internals atomically (still under mu.Lock)
    store.files         = root.Files
    store.parents       = root.Parents
    store.children      = root.Children
    store.shares        = root.Shares
    store.linkCounts    = root.LinkCounts
    store.deviceNumbers = root.DeviceNumbers
    store.pendingWrites = root.PendingWrites
    store.serverConfig  = root.ServerConfig
    store.capabilities  = root.Capabilities
    store.sessions      = root.Sessions
    store.fileBlockData = root.FileBlockData
    store.lockStore     = root.LockStore
    store.clientStore   = root.ClientStore
    store.durableStore  = root.DurableStore

    // Recompute usedBytes (match initUsedBytesCounter convention)
    return nil
}
```

**Gob root struct shape** — Claude's discretion (§Claude's Discretion). Define as unexported `memoryBackupRoot` in the same file. Must carry `FormatVersion`, `GoVersion`, `GobSchemaVersion` (D-09). Register concrete types via `gob.Register` for any interface fields (`FileAttr.ACL`, `MetadataServerConfig.CustomSettings any`).

**Risk flags:**
- `unsafe.String`-backed map keys (see `handleToKey` in `store.go:388-398`): fine to gob-encode as-is because gob sees them as `string`, not as unsafe-backed data. Round-trip produces safe owned strings.
- `pendingWrites` holds `*metadata.WriteOperation` — verify gob-encodable (check for non-exported fields).
- `sync.Pool` (`attrPool`) must NOT be in the root struct — transient.
- `map[string]any` in `MetadataServerConfig.CustomSettings` requires `gob.Register` of every concrete type that can appear there. Current tests only put primitives; document this invariant or switch to `map[string]string`.

---

### `pkg/metadata/store/memory/backup_test.go` (test)

**Analog:** `pkg/metadata/store/memory/locks_test.go` (in-process, no build tag, no tmp dir).

**Pattern** — simple `package memory_test` with direct constructor calls:

```go
package memory_test

import (
    "bytes"
    "context"
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/store/memory"
    "github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestBackupConformance(t *testing.T) {
    storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.Backupable {
        return memory.NewMemoryMetadataStoreWithDefaults()
    })
}
```

Memory uses the in-process concurrent-write test from the shared suite (D-08 item 2).

---

### `pkg/metadata/store/badger/backup.go` (driver, streaming)

**Analog:** file organization from `pkg/metadata/store/badger/locks.go` (per-feature); full-DB iteration pattern from `pkg/metadata/store/badger/files.go:58-109` (`GetFileByPayloadID` already demonstrates a full `prefixFile` scan inside `db.View`); prefix catalog from `pkg/metadata/store/badger/encoding.go:44-53` + `locks.go:19-25` + `clients.go:18-24` + `durable_handles.go:16-34`.

**Imports pattern** (from `pkg/metadata/store/badger/files.go:1-9`):

```go
package badger

import (
    "context"
    "encoding/binary"
    "errors"
    "fmt"
    "io"

    badgerdb "github.com/dgraph-io/badger/v4"
    "github.com/marmos91/dittofs/pkg/metadata"
)
```

**Compile-time interface assertion** (match `pkg/metadata/store/badger/objects.go:39`):

```go
var _ metadata.Backupable = (*BadgerMetadataStore)(nil)
```

**Complete prefix list (D-01) — copy-literal from the four encoding files:**

From `encoding.go:44-53`: `prefixFile="f:"`, `prefixParent="p:"`, `prefixChild="c:"`, `prefixShare="s:"`, `prefixLinkCount="l:"`, `prefixDeviceNumber="d:"`, `prefixConfig="cfg:"`, `prefixCapabilities="cap:"`

From `locks.go:19-25`: `prefixLock="lock:"`, `prefixLockByFile="lkfile:"`, `prefixLockByOwner="lkowner:"`, `prefixLockByClient="lkclient:"`, `prefixServerEpoch="srvepoch"` (note: singleton, no separator)

From `clients.go:18-24`: `prefixNSMClient="nsm:client:"`, `prefixNSMByMonName="nsm:monname:"`

From `durable_handles.go:16-34`: `prefixDHID="dh:id:"`, `prefixDHCreateGuid="dh:cguid:"`, `prefixDHAppInstanceId="dh:appid:"`, `prefixDHFileID="dh:fid:"`, `prefixDHFileHandle="dh:fh:"`, `prefixDHShare="dh:share:"`

From `objects.go:31-36` (FileBlock): `fileBlockPrefix="fb:"`, `fileBlockHashPrefix="fb-hash:"`, `fileBlockLocalPrefix="fb-local:"`, `fileBlockFilePrefix="fb-file:"`

Also: `fsmeta:` appears in RESEARCH.md — verify by grepping `pkg/metadata/store/badger` for remaining `prefix*` constants at implementation time and fail-closed (D-09 `key_prefix_list` defensive check).

**Same-snapshot scan pattern (D-02, D-03)** — copy the iterator pattern from `pkg/metadata/store/badger/files.go:58-112` (`GetFileByPayloadID`), scale up to all prefixes:

```go
func (s *BadgerMetadataStore) Backup(ctx context.Context, w io.Writer) (metadata.PayloadIDSet, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }

    ids := metadata.NewPayloadIDSet()

    // Write header (badger_version, format_version, key_prefix_list) BEFORE db.View
    if err := writeBadgerHeader(w); err != nil {
        return nil, err
    }

    err := s.db.View(func(txn *badgerdb.Txn) error {
        // 1) Scan prefixFile for PayloadIDs under THIS txn's read-ts (D-02)
        opts := badgerdb.DefaultIteratorOptions
        opts.PrefetchValues = true
        opts.Prefix = []byte(prefixFile)
        it := txn.NewIterator(opts)
        for it.Rewind(); it.ValidForPrefix([]byte(prefixFile)); it.Next() {
            if err := ctx.Err(); err != nil {
                it.Close()
                return err
            }
            err := it.Item().Value(func(val []byte) error {
                f, err := decodeFile(val)
                if err != nil {
                    return nil // skip corrupt entries (pattern from files.go:73-75)
                }
                if f.PayloadID != "" {
                    ids.Add(string(f.PayloadID))
                }
                return nil
            })
            if err != nil {
                it.Close()
                return err
            }
        }
        it.Close()

        // 2) Iterate ALL prefixes under the same txn, emit framed key/value pairs
        for _, prefix := range allBackupPrefixes {
            if err := streamPrefix(ctx, txn, prefix, w); err != nil {
                return err
            }
        }
        return nil
    })
    if err != nil {
        return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
    }
    return ids, nil
}
```

**Framing wire format** — length-prefixed key/value records (Claude's discretion §Claude's Discretion). Use `binary.BigEndian.PutUint32` consistent with `encoding.go:181-185`:

```go
// frame layout: [prefix_idx u8][key_len u32][key][value_len u32][value]
// EOF marker:   [0xFF]
func writeFrame(w io.Writer, prefixIdx uint8, k, v []byte) error {
    hdr := make([]byte, 1+4+4)
    hdr[0] = prefixIdx
    binary.BigEndian.PutUint32(hdr[1:5], uint32(len(k)))
    binary.BigEndian.PutUint32(hdr[5:9], uint32(len(v)))
    if _, err := w.Write(hdr); err != nil { return err }
    if _, err := w.Write(k); err != nil { return err }
    _, err := w.Write(v)
    return err
}
```

**Restore empty-check pattern (D-06)** — match iterator usage style:

```go
func (s *BadgerMetadataStore) Restore(ctx context.Context, r io.Reader) error {
    if err := ctx.Err(); err != nil { return err }

    // Empty-dest check: any key with prefix "f:" → reject
    nonEmpty := false
    err := s.db.View(func(txn *badgerdb.Txn) error {
        opts := badgerdb.DefaultIteratorOptions
        opts.Prefix = []byte(prefixFile)
        it := txn.NewIterator(opts)
        defer it.Close()
        it.Rewind()
        if it.ValidForPrefix([]byte(prefixFile)) {
            nonEmpty = true
        }
        return nil
    })
    if err != nil {
        return err
    }
    if nonEmpty {
        return metadata.ErrRestoreDestinationNotEmpty
    }

    // Verify header (format_version, key_prefix_list)
    // Stream frames into txn.Set, batching via WriteBatch for perf
    wb := s.db.NewWriteBatch()
    defer wb.Cancel()

    // ... decode frames; wb.Set(k, v); on error: wrap as ErrRestoreCorrupt
    if err := wb.Flush(); err != nil {
        return err
    }
    return nil
}
```

**Error wrapping pattern (D-07)** — match the existing `%w` convention (`store.go:210`, `files.go:124`): `fmt.Errorf("failed to X: %w", err)`. Restore-stream decode errors MUST wrap `metadata.ErrRestoreCorrupt` so callers can `errors.Is(err, metadata.ErrRestoreCorrupt)`.

**Risk flags:**
- **CRITICAL (D-03):** Do NOT call `badger.DB.Backup(w, since)` — it opens its own internal read-timestamp. Driver must use `s.db.View(func(txn *badger.Txn)` and drive both the PayloadID scan AND the stream inside the same `txn`. `txn.NewStream()` shares the txn's read-ts and is acceptable if simpler (D-03 explicitly allows it).
- **Prefix completeness risk (D-09):** New prefix families may be added by future milestones (e.g., SMB persistent handles, NLM v4). Runtime defense: emit `key_prefix_list` into the backup header; on restore, fail-closed if the archive lists unknown prefixes. Implementation-time defense: grep `pkg/metadata/store/badger/*.go` for all `prefix*` constants at plan time, cross-reference with D-01's list.
- **Concurrent writer test (D-08 item 2):** Badger's SSI gives snapshot isolation per `db.View`, but the concurrent writer goroutine uses `db.Update` which commits with a later read-ts. The test asserts that backup produces a consistent view as-of the `db.View` txn's read-ts, not as-of backup-completion time.
- **WriteBatch vs Update in restore:** `NewWriteBatch` is faster but skips `ManagedTxns` conflict detection. Acceptable for restore into an empty store.
- **Lazy sub-stores (risk of missed data):** `s.lockStore`, `s.clientStore`, `s.durableStore` are Go-level caches (see `locks.go:470-476`). The actual K-V data lives on `s.db` under the `lock:*`, `nsm:*`, `dh:*` prefixes regardless of whether the cache was initialized. Driver MUST iterate `s.db` directly by prefix (D-01), never go through the sub-store wrappers.

---

### `pkg/metadata/store/badger/backup_test.go` (integration test)

**Analog:** `pkg/metadata/store/badger/badger_conformance_test.go` (exact shape).

**Pattern:**

```go
//go:build integration

package badger_test

import (
    "context"
    "path/filepath"
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/store/badger"
    "github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestBackupConformance(t *testing.T) {
    storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.Backupable {
        dbPath := filepath.Join(t.TempDir(), "metadata.db")
        store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
        if err != nil {
            t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
        }
        t.Cleanup(func() { _ = store.Close() })
        return store
    })
}
```

**Factory-to-typed-interface cast** — since `Backupable` is narrower than `MetadataStore`, add a dedicated `BackupStoreFactory` in `storetest/backup_conformance.go`. If conformance needs both (populate store via MetadataStore ops then back it up via Backupable), factory returns `*BadgerMetadataStore` directly which satisfies both.

---

### `pkg/metadata/store/postgres/backup.go` (driver, streaming)

**Analog:** `pkg/metadata/store/postgres/transaction.go:51-115` (WithTransaction scaffold + retry + timeout pattern); `pkg/metadata/store/postgres/migrate.go:99-111` (migration-version lookup pattern).

**Imports pattern** (from `pkg/metadata/store/postgres/transaction.go:1-14` + external):

```go
package postgres

import (
    "archive/tar"
    "context"
    "errors"
    "fmt"
    "io"

    "github.com/jackc/pgx/v5"
    "github.com/marmos91/dittofs/pkg/metadata"
)
```

**Compile-time interface assertion:**

```go
var _ metadata.Backupable = (*PostgresMetadataStore)(nil)
```

**Deterministic table list (D-04)** — hard-code alphabetically:

```go
// Table order: alphabetical, deterministic for reproducible SHA-256.
// Must be updated whenever a new migration adds a table.
var backupTables = []string{
    "acls",                     // migration 000004
    "clients",                  // migration 000003
    "durable_handles",          // migration 000005
    "files",                    // migration 000001
    "filesystem_capabilities",  // migration 000001
    "link_counts",              // migration 000001
    "locks",                    // migration 000002
    "parent_child_map",         // migration 000001
    "pending_writes",           // migration 000001
    "schema_migrations",        // managed by golang-migrate (include for restore sanity)
    "server_config",            // migration 000001
    "shares",                   // migration 000001
    // Note: file_blocks / fb tables — verify existence per objects.go before planning
}
```

**REPEATABLE READ txn + COPY TO pattern (D-02, D-04)** — adapt `WithTransaction` (transaction.go:51-115) to expose a typed tx with explicit isolation:

```go
func (s *PostgresMetadataStore) Backup(ctx context.Context, w io.Writer) (metadata.PayloadIDSet, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }

    // Acquire connection with timeout (pattern from transaction.go:61-78)
    acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
    conn, err := s.pool.Acquire(acquireCtx)
    cancel()
    if err != nil {
        return nil, fmt.Errorf("acquire conn: %w", err)
    }
    defer conn.Release()

    tx, err := conn.BeginTx(ctx, pgx.TxOptions{
        IsoLevel:   pgx.RepeatableRead, // D-02: snapshot stability
        AccessMode: pgx.ReadOnly,
    })
    if err != nil {
        return nil, fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback(ctx) //nolint: errcheck — commit below on success

    // 1) PayloadIDSet under the SAME tx (D-02)
    ids := metadata.NewPayloadIDSet()
    rows, err := tx.Query(ctx,
        `SELECT DISTINCT content_id FROM files WHERE content_id IS NOT NULL`)
    if err != nil {
        return nil, fmt.Errorf("scan payload ids: %w", err)
    }
    for rows.Next() {
        var pid string
        if err := rows.Scan(&pid); err != nil {
            rows.Close()
            return nil, err
        }
        ids.Add(pid)
    }
    rows.Close()
    if err := rows.Err(); err != nil {
        return nil, err
    }

    // 2) Lookup schema migration version (D-04) — pattern from migrate.go:77-79
    var schemaVer int
    var dirty bool
    if err := tx.QueryRow(ctx,
        `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&schemaVer, &dirty); err != nil {
        return nil, fmt.Errorf("read schema_migrations: %w", err)
    }
    if dirty {
        return nil, fmt.Errorf("refusing to back up dirty schema (version=%d)", schemaVer)
    }

    // 3) Write tar: manifest.yaml first, then tables/NN-<name>.copy
    tw := tar.NewWriter(w)
    defer tw.Close()

    // Write manifest.yaml with:
    //   pg_server_version, schema_migration_version, format_version, table_list w/ counts
    if err := writePGManifestTar(ctx, tw, tx, schemaVer, backupTables); err != nil {
        return nil, err
    }

    // Each table: COPY TO STDOUT (FORMAT binary) → tar entry
    for i, table := range backupTables {
        if err := copyTableToTar(ctx, tw, tx, i, table); err != nil {
            return nil, fmt.Errorf("%w: copy %s: %v", metadata.ErrBackupAborted, table, err)
        }
    }

    if err := tx.Commit(ctx); err != nil {
        return nil, fmt.Errorf("commit backup tx: %w", err)
    }
    return ids, nil
}
```

**`copyTableToTar` — pgx binary COPY stream into tar** (pgx v5 pattern):

```go
func copyTableToTar(ctx context.Context, tw *tar.Writer, tx pgx.Tx, idx int, table string) error {
    // Buffer the COPY output in-memory so we can set tar header Size correctly.
    // For very large tables (>1GB) consider two-pass: get row count via SELECT
    // COUNT(*) then stream into tar with exact size computed up-front.
    var buf bytes.Buffer
    // pgx.Conn.PgConn() exposes the low-level CopyTo for binary-format streams.
    conn := tx.Conn().PgConn()
    sql := fmt.Sprintf(`COPY %s TO STDOUT (FORMAT binary)`, pgx.Identifier{table}.Sanitize())
    if _, err := conn.CopyTo(ctx, &buf, sql); err != nil {
        return err
    }

    hdr := &tar.Header{
        Name:    fmt.Sprintf("tables/%02d-%s.copy", idx, table),
        Mode:    0o600,
        Size:    int64(buf.Len()),
        ModTime: time.Now(),
    }
    if err := tw.WriteHeader(hdr); err != nil { return err }
    _, err := tw.Write(buf.Bytes())
    return err
}
```

**Restore empty-check (D-06)** — use `EXISTS`:

```go
func (s *PostgresMetadataStore) Restore(ctx context.Context, r io.Reader) error {
    // Empty-dest check BEFORE opening tx to fail fast without DDL side effects
    var exists bool
    if err := s.pool.QueryRow(ctx,
        `SELECT EXISTS(SELECT 1 FROM files LIMIT 1)`).Scan(&exists); err != nil {
        return fmt.Errorf("check files empty: %w", err)
    }
    if exists {
        return metadata.ErrRestoreDestinationNotEmpty
    }

    // Read manifest.yaml from tar, verify schema_migration_version matches current.
    // If mismatch: return ErrSchemaVersionMismatch (no DDL executed, no tx opened).

    // Single tx wraps all COPY FROM for atomic rollback on any failure.
    return s.pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted},
        func(tx pgx.Tx) error {
            for each table entry in tar order {
                sql := fmt.Sprintf(`COPY %s FROM STDIN (FORMAT binary)`,
                    pgx.Identifier{table}.Sanitize())
                if _, err := tx.Conn().PgConn().CopyFrom(ctx, entryReader, sql); err != nil {
                    return fmt.Errorf("%w: restore %s: %v",
                        metadata.ErrRestoreCorrupt, table, err)
                }
            }
            return nil
        })
}
```

**Risk flags:**
- **pgx binary COPY round-trip caveat (RESEARCH.md §GAP #1):** `pgx.Conn.CopyTo` / `CopyFrom` must round-trip identically for: `uuid`, `timestamptz`, `jsonb` (share.options, acls), `smallint`, arrays, null-constrained columns. Plan Phase 2 includes a spike integration test that round-trips every column type. Known problem types: `jsonb` with large payloads may differ in whitespace post-restore (acceptable); `timestamptz` with fractional seconds must preserve precision to `time.Time` nanos.
- **Advisory-lock during backup:** golang-migrate holds `schema_migrations` advisory locks during migrations. Backup tx (REPEATABLE READ READ ONLY) will not deadlock with it but may observe a partial migration mid-run; the `dirty` check above rejects that case.
- **`pgx.Identifier{}.Sanitize()` for table names** — prevents SQL injection even though `backupTables` is a hard-coded list. Keep this defensive idiom; matches general pgx best practice.
- **Deterministic tar ordering (D-04):** Go's `archive/tar` is order-preserving, so writing entries in alphabetical `backupTables` order is sufficient for reproducible SHA-256.
- **Triggers during restore:** `files_path_hash_trigger` (migration 000001 line 191) and `files_content_id_hash_trigger` re-compute md5 on INSERT. `COPY FROM STDIN (FORMAT binary)` triggers fire the same as INSERT, so hashed columns are written twice (once from archive, once recomputed). This is equal-valued — idempotent — but consider `ALTER TABLE ... DISABLE TRIGGER` inside the restore tx to avoid wasted CPU on large restores. Document this as a perf tuning opportunity, not a correctness issue.
- **Context propagation:** `conn.PgConn().CopyTo` honors context; cancellation aborts the stream. Wrap abort as `ErrBackupAborted` (D-07).
- **PG server version** header (D-09): `SELECT current_setting('server_version_num')`; cast to int. Cheap, single-round-trip.

---

### `pkg/metadata/store/postgres/backup_test.go` (integration test)

**Analog:** `pkg/metadata/store/postgres/postgres_conformance_test.go` (exact shape — build tag, DSN env var, skip-if-unset).

**Pattern:**

```go
//go:build integration

package postgres_test

import (
    "context"
    "os"
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/store/postgres"
    "github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestBackupConformance(t *testing.T) {
    if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
        t.Skip("DITTOFS_TEST_POSTGRES_DSN not set")
    }
    storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.Backupable {
        cfg := &postgres.PostgresMetadataStoreConfig{
            Host:        "localhost",
            Port:        5432,
            Database:    "dittofs_test",
            User:        "postgres",
            Password:    "postgres",
            SSLMode:     "disable",
            AutoMigrate: true,
        }
        caps := metadata.FilesystemCapabilities{ /* defaults per postgres_conformance_test.go:36-50 */ }
        store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
        if err != nil {
            t.Fatalf("NewPostgresMetadataStore() failed: %v", err)
        }
        t.Cleanup(func() { store.Close() })
        return store
    })
}
```

**Shared-container note (MEMORY.md §CI/Build Notes):** `TestCollectGarbage_S3` flakiness precedent. Use a shared-container helper if Phase 2 needs multiple Localstack/Postgres runs — but for this conformance file a single container per CI job is sufficient. Each test's factory gets a fresh schema via AutoMigrate against a uniquely-named database (e.g., `dittofs_test_backup_<ulid>`) to avoid cross-test contamination.

---

### `pkg/metadata/storetest/backup_conformance.go` (conformance-suite)

**Analog:** `pkg/metadata/storetest/file_block_ops.go` + `pkg/metadata/storetest/suite.go:21-43` (factory + `RunXxxSuite` entry point).

**Factory signature** — new, since `Backupable` is narrower than `MetadataStore`:

```go
// BackupStoreFactory creates a fresh backup-capable store for each test.
// The returned value MUST satisfy BOTH metadata.MetadataStore (for population)
// AND metadata.Backupable (for Backup/Restore exercise).
type BackupStoreFactory func(t *testing.T) BackupTestStore

// BackupTestStore is the union required by the conformance tests.
type BackupTestStore interface {
    metadata.MetadataStore
    metadata.Backupable
    io.Closer // for t.Cleanup() — all three engines expose Close()
}
```

**Entry point — match `suite.go:21-43` exactly:**

```go
// RunBackupConformanceSuite runs the Phase 2 backup/restore conformance suite.
// Each sub-test gets a fresh store instance.
//
// The suite covers five scenarios (D-08):
//   1. RoundTrip:           populate → Backup → Restore → enumerate, assert equal
//   2. ConcurrentWriter:    writes during Backup; assert snapshot consistent
//   3. Corruption:          truncate/flip bytes → Restore returns ErrRestoreCorrupt
//   4. NonEmptyDest:        populate dest → Restore returns ErrRestoreDestinationNotEmpty
//   5. PayloadIDSet:        enumerate restored payload refs, assert == returned set
func RunBackupConformanceSuite(t *testing.T, factory BackupStoreFactory) {
    t.Helper()

    t.Run("RoundTrip",        func(t *testing.T) { testBackupRoundTrip(t, factory) })
    t.Run("ConcurrentWriter", func(t *testing.T) { testBackupConcurrentWriter(t, factory) })
    t.Run("Corruption",       func(t *testing.T) { testBackupCorruption(t, factory) })
    t.Run("NonEmptyDest",     func(t *testing.T) { testBackupNonEmptyDest(t, factory) })
    t.Run("PayloadIDSet",     func(t *testing.T) { testBackupPayloadIDSet(t, factory) })
}
```

**Test helpers** — reuse `createTestShare`, `createTestFile`, `createTestDir` from `suite.go:47-183`. These are package-scoped (lowercase) and directly callable from this file since it lives in `package storetest`.

**Two-store fixture** — conformance tests need a source (populate → Backup) AND a destination (Restore). Factory is called twice per test. For memory it's trivial; for badger use two distinct `t.TempDir()`; for postgres use two distinct schemas (drop + recreate inside the factory).

**Risk flags:**
- **ConcurrentWriter test (D-08 item 2)** — memory runs it in-process (goroutines racing on `mu`); badger runs real concurrent `db.Update` during `db.View`; postgres launches a parallel goroutine doing INSERTs on a separate connection during the REPEATABLE READ backup tx. Test assertion: after restore, for every PayloadID in the backup's returned set, there EXISTS a file in the restored store with that PayloadID (i.e., no dangling refs), AND for every file in the restored store, its PayloadID IS in the returned set (i.e., no uncounted files → unsafe GC).
- **Corruption test** — three variants: (a) truncate at header midpoint, (b) truncate at body midpoint, (c) flip a byte in an existing frame. All must yield `errors.Is(err, metadata.ErrRestoreCorrupt)` AND leave destination unchanged (check via `Backup()` returning an empty-equivalent dump).
- **Parallel testing (`t.Parallel()`)** — postgres conformance uses shared schema/database; DO NOT mark these sub-tests `Parallel()` on postgres. Badger tmp-dir isolation is safe for parallel. Memory is safe. Keep suite single-threaded at the top level; engines can override by not using `t.Parallel()` in their factory.

---

## Shared Patterns

### Error Wrapping (applies to all three drivers)

**Source:** existing store code (`pkg/metadata/store/badger/store.go:210`, `pkg/metadata/store/postgres/store.go:84`, `pkg/metadata/errors.go`).

**Rule:** wrap all low-level errors with `fmt.Errorf("...: %w", err)` so callers can `errors.Is` / `errors.As`. For the new Phase-2 sentinels (`ErrRestoreCorrupt`, `ErrSchemaVersionMismatch`, `ErrRestoreDestinationNotEmpty`, `ErrBackupAborted`), use the `%w: %v` pattern so Phase 5 orchestrator code can pattern-match on the typed error while preserving the concrete cause for the operator log:

```go
return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, decodeErr)
```

### Context Cancellation (applies to all three drivers)

**Source:** every method in the existing stores (`pkg/metadata/store/badger/files.go:20-23`, `pkg/metadata/store/memory/files.go:20-22`, `pkg/metadata/store/postgres/transaction.go:52-54`).

**Rule:** first line of `Backup` and `Restore` MUST be:

```go
if err := ctx.Err(); err != nil {
    return nil, err
}
```

Inside long-running loops (prefix iteration for Badger, table iteration for PG, large map walk for Memory — only relevant at extreme scale), check `ctx.Err()` periodically. pgx `CopyTo` / `CopyFrom` honor ctx natively.

### Compile-Time Interface Assertion (applies to all three drivers)

**Source:** `pkg/metadata/store/badger/objects.go:39`, `pkg/metadata/store/memory/objects.go:43`, `pkg/metadata/store/postgres/` (implicit via `objects.go`).

**Rule:** each `backup.go` ends with:

```go
var _ metadata.Backupable = (*XxxMetadataStore)(nil)
```

This is how Phase 3/5 capability-check at compile-time (ENG-04 Phase-1 doc). Missing assertion = latent runtime bug.

### Build Tags (applies to badger + postgres test files, NOT memory)

**Source:** `pkg/metadata/store/badger/badger_conformance_test.go:1` (`//go:build integration`), same for postgres.

**Rule:**
- Memory test file — no build tag (runs in default `go test ./...`)
- Badger test file — `//go:build integration` (needs tmp-dir I/O; keeps default `go test ./...` fast)
- Postgres test file — `//go:build integration` + `DITTOFS_TEST_POSTGRES_DSN` env-var skip

This matches MEMORY.md convention and the existing conformance test files verbatim.

---

## No Analog Found

None. Every new Phase-2 file has a structural analog in the existing codebase.

Two patterns have NO in-project precedent and must be introduced cleanly:

1. **`encoding/gob` usage** — no existing callers in the codebase. The Memory driver introduces it. Ensure the root struct is defined with deterministic field ordering; register any `interface{}`-typed payload concrete types via `gob.Register` (`MetadataServerConfig.CustomSettings` is the main offender).
2. **`archive/tar` usage** — no existing callers in Go code (mentioned only in planning docs). The Postgres driver introduces it. Standard library; no new dependency. Be explicit about header `ModTime` (use a fixed value derived from the backup's `BackupID` ULID time-prefix for byte-identical reproducibility; DO NOT use `time.Now()`).

---

## Metadata

**Analog search scope:**
- `pkg/metadata/backup.go` (interface + existing sentinel)
- `pkg/metadata/backup_test.go` (interface-shape test pattern)
- `pkg/metadata/store/memory/` (all files)
- `pkg/metadata/store/badger/` (all files — especially `encoding.go`, `locks.go`, `clients.go`, `durable_handles.go`, `objects.go`, `transaction.go`, `files.go`)
- `pkg/metadata/store/postgres/` (all files — especially `transaction.go`, `migrate.go`, `connection.go`, `migrations/`)
- `pkg/metadata/storetest/` (all files)
- `pkg/controlplane/store/backup_test.go` (layout reference)
- `pkg/backup/manifest/manifest.go` (Phase-1 contract, engine_metadata field)

**Files scanned:** ~30 source files across the four relevant packages.

**Pattern extraction date:** 2026-04-16.

## PATTERN MAPPING COMPLETE
