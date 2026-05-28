# Phase 21: Per-Engine Backup Drivers - Pattern Map

**Mapped:** 2026-05-27
**Files analyzed:** 7 (3 new, 4 modified)
**Analogs found:** 7 / 7

## File Classification

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|-------------------|------|-----------|----------------|---------------|
| `pkg/metadata/store/memory/backup.go` | service | streaming (serialize) | `pkg/metadata/store/memory/shares.go` | role-match |
| `pkg/metadata/store/badger/backup.go` | service | streaming (serialize) | `pkg/metadata/store/badger/shares.go` | role-match |
| `pkg/metadata/store/postgres/backup.go` | service | streaming (serialize) | `pkg/metadata/store/postgres/file_block_refs.go` | role-match |
| `pkg/metadata/store/memory/memory_conformance_test.go` | test | conformance | `pkg/metadata/store/memory/memory_conformance_test.go` (self) | exact |
| `pkg/metadata/store/badger/badger_conformance_test.go` | test | conformance | `pkg/metadata/store/badger/badger_conformance_test.go` (self) | exact |
| `pkg/metadata/store/postgres/postgres_conformance_test.go` | test | conformance | `pkg/metadata/store/postgres/postgres_conformance_test.go` (self) | exact |
| `pkg/metadata/backup/envelope.go` | utility (read-only ref) | streaming | N/A (foundation, not modified) | N/A |

## Pattern Assignments

### `pkg/metadata/store/memory/backup.go` (service, streaming)

**Analog:** `pkg/metadata/store/memory/shares.go` (locking + map iteration pattern)

**Imports pattern** -- copy from `shares.go` lines 1-7 plus backup/blockstore additions:
```go
package memory

import (
    "context"
    "encoding/binary"
    "encoding/gob"
    "fmt"
    "io"

    "github.com/marmos91/dittofs/pkg/blockstore"
    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/backup"
)
```

**Locking pattern** -- copy from `shares.go` lines 167-174 (ListShares):
```go
// ListShares returns the names of all shares.
func (store *MemoryMetadataStore) ListShares(ctx context.Context) ([]string, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }

    store.mu.RLock()
    defer store.mu.RUnlock()

    names := make([]string, 0, len(store.shares))
    for name := range store.shares {
        names = append(names, name)
    }

    return names, nil
}
```
The new `Backup` must follow the same `ctx.Err()` check then `mu.RLock()/defer mu.RUnlock()` pattern. All map reads under that lock.

**Struct field reference** -- from `store.go` lines 122-275, the `MemoryMetadataStore` fields that Backup must snapshot:
```go
type MemoryMetadataStore struct {
    mu              sync.RWMutex
    shares          map[string]*shareData        // share configs + root handles
    files           map[string]*fileData         // file metadata (has Attr.Blocks for hash extraction)
    parents         map[string]metadata.FileHandle
    children        map[string]map[string]metadata.FileHandle
    linkCounts      map[string]uint32
    deviceNumbers   map[string]*deviceNumber
    pendingWrites   map[string]*metadata.WriteOperation
    serverConfig    metadata.MetadataServerConfig
    capabilities    metadata.FilesystemCapabilities
    // ... lazy sub-stores, rollupOffsets, synced, objectIndex, storeID ...
}
```

**Internal types that gob must handle** -- from `store.go` lines 23-45:
```go
type shareData struct {
    Share      metadata.Share
    RootHandle metadata.FileHandle
}

type fileData struct {
    Attr      *metadata.FileAttr
    ShareName string
    Path      string
}

type deviceNumber struct {
    Major uint32
    Minor uint32
}
```
These are unexported types but `backup.go` lives in `package memory` so it has access. Gob requires exported struct fields -- define an exported snapshot struct with exported field equivalents.

**Empty-store detection pattern** (D-06) -- from `shares.go` line 175:
```go
// Detect empty: len(store.shares) > 0
names := make([]string, 0, len(store.shares))
```

**Hash extraction site** -- `fileData.Attr.Blocks` is `[]blockstore.BlockRef`. Each `BlockRef.Hash` is a `blockstore.ContentHash`. Iterate `store.files` under the same lock and `hs.Add(br.Hash)`.

---

### `pkg/metadata/store/badger/backup.go` (service, streaming)

**Analog:** `pkg/metadata/store/badger/shares.go` (db.View + prefix iteration pattern)

**Imports pattern** -- copy from `shares.go` lines 1-9 plus backup/blockstore additions:
```go
package badger

import (
    "context"
    "encoding/binary"
    "encoding/json"
    "fmt"
    "io"

    badgerdb "github.com/dgraph-io/badger/v4"

    "github.com/marmos91/dittofs/pkg/blockstore"
    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/backup"
)
```

**db.View() MVCC snapshot pattern** -- copy from `shares.go` lines 230-248 (ListShares):
```go
func (s *BadgerMetadataStore) ListShares(ctx context.Context) ([]string, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }

    var names []string

    err := s.db.View(func(txn *badgerdb.Txn) error {
        prefix := []byte(prefixShare)
        opts := badgerdb.DefaultIteratorOptions
        opts.Prefix = prefix
        opts.PrefetchValues = false

        it := txn.NewIterator(opts)
        defer it.Close()

        for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
            key := it.Item().Key()
            name := string(key[len(prefix):])
            names = append(names, name)
        }

        return nil
    })

    return names, err
}
```
For backup: use a single `db.View()` call (MVCC consistent snapshot). Iterate ALL keys with `opts.PrefetchValues = true` (need values) and write length-prefixed key+value pairs.

**Prefix constants** -- from `encoding.go` lines 44-54 and `objects.go` lines 38-43:
```go
// encoding.go
const (
    prefixFile         = "f:"
    prefixParent       = "p:"
    prefixChild        = "c:"
    prefixShare        = "s:"
    prefixLinkCount    = "l:"
    prefixDeviceNumber = "d:"
    prefixConfig       = "cfg:"
    prefixCapabilities = "cap:"
    prefixObjectID     = "obj:"
)

// objects.go
const (
    fileBlockPrefix      = "fb:"
    fileBlockHashPrefix  = "fb-hash:"
    fileBlockLocalPrefix = "fb-local:"
    fileBlockFilePrefix  = "fb-file:"
)
```
Hash extraction: during iteration, when key starts with `prefixFile` (`"f:"`), JSON-decode value as `metadata.File`, iterate `Blocks` field, `hs.Add(br.Hash)`.

**Empty-store detection** (D-06) -- use the ListShares prefix-seek pattern above with `opts.PrefetchValues = false`, check `it.Valid()` after `it.Seek(prefix)`.

---

### `pkg/metadata/store/postgres/backup.go` (service, streaming)

**Analog:** `pkg/metadata/store/postgres/file_block_refs.go` (raw pgx transaction pattern) + `pool_helpers.go` (connection management)

**Imports pattern** -- copy from `file_block_refs.go` lines 1-10 plus backup/io additions:
```go
package postgres

import (
    "context"
    "encoding/binary"
    "fmt"
    "io"

    "github.com/jackc/pgx/v5/pgconn"

    "github.com/marmos91/dittofs/pkg/blockstore"
    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/backup"
)
```

**Connection acquisition pattern** -- copy from `pool_helpers.go` lines 129-153 (beginTx):
```go
func (s *PostgresMetadataStore) beginTx(ctx context.Context) (pgx.Tx, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }

    acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
    defer cancel()

    tx, err := s.pool.Begin(acquireCtx)
    if err != nil {
        if acquireCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
            return nil, fmt.Errorf("connection acquire timeout after %v: pool may be exhausted", poolConnectionAcquireTimeout)
        }
        return nil, mapPgError(err, "beginTx", "")
    }

    return tx, nil
}
```
For backup: acquire raw `PgConn()` for COPY operations. Use `pool.Acquire(ctx)` then `conn.Conn().PgConn()` for raw access. Set isolation via `raw.Exec(ctx, "BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ")`.

**Batch SQL pattern** -- copy from `file_block_refs.go` lines 32-53 (putFileBlockRefs):
```go
func putFileBlockRefs(ctx context.Context, tx pgx.Tx, fileID uuid.UUID, blocks []blockstore.BlockRef) error {
    if _, err := tx.Exec(ctx, `DELETE FROM file_block_refs WHERE file_id = $1`, fileID); err != nil {
        return fmt.Errorf("delete file_block_refs for %s: %w", fileID, err)
    }
    if len(blocks) == 0 {
        return nil
    }
    batch := &pgx.Batch{}
    for _, b := range blocks {
        batch.Queue(
            `INSERT INTO file_block_refs (file_id, "offset", size, hash) VALUES ($1, $2, $3, $4)`,
            fileID, int64(b.Offset), int32(b.Size), b.Hash[:],
        )
    }
    br := tx.SendBatch(ctx, batch)
    defer func() { _ = br.Close() }()
    for range blocks {
        if _, err := br.Exec(); err != nil {
            return fmt.Errorf("insert file_block_ref: %w", err)
        }
    }
    return nil
}
```
For COPY operations: use `raw.CopyTo(ctx, writer, sql)` and `raw.CopyFrom(ctx, reader, sql)`. The pgconn-level API is lower than pgx.Tx -- the backup path needs raw connection access.

**Empty-store detection** (D-06) -- use `SELECT EXISTS(SELECT 1 FROM shares)` via the existing `queryRow` helper pattern from `pool_helpers.go` line 37.

---

### `pkg/metadata/store/memory/memory_conformance_test.go` (test, MODIFIED)

**Analog:** Self (exact match -- add a second conformance suite call)

**Existing pattern** -- lines 1-16:
```go
package memory_test

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/store/memory"
    "github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
    storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        return memory.NewMemoryMetadataStoreWithDefaults()
    })
}
```

**Addition pattern** -- add a new test function with the same factory:
```go
func TestBackupConformance(t *testing.T) {
    storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        return memory.NewMemoryMetadataStoreWithDefaults()
    })
}
```

---

### `pkg/metadata/store/badger/badger_conformance_test.go` (test, MODIFIED)

**Analog:** Self (exact match)

**Existing pattern** -- lines 1-32 (note `//go:build integration` tag and cleanup):
```go
//go:build integration

package badger_test

import (
    "context"
    "path/filepath"
    "testing"

    // ...
    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/store/badger"
    "github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
    storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        dbPath := filepath.Join(t.TempDir(), "metadata.db")
        store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
        if err != nil {
            t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
        }
        t.Cleanup(func() {
            store.Close()
        })
        return store
    })
}
```

**Addition pattern** -- add a new test function with identical factory:
```go
func TestBackupConformance(t *testing.T) {
    storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        dbPath := filepath.Join(t.TempDir(), "metadata.db")
        store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
        if err != nil {
            t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
        }
        t.Cleanup(func() {
            store.Close()
        })
        return store
    })
}
```

---

### `pkg/metadata/store/postgres/postgres_conformance_test.go` (test, MODIFIED)

**Analog:** Self (exact match)

**Existing pattern** -- lines 1-62 (note `//go:build integration`, DSN skip, full config):
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

func TestConformance(t *testing.T) {
    connStr := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
    if connStr == "" {
        t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL conformance tests")
    }

    storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        cfg := &postgres.PostgresMetadataStoreConfig{
            Host:        "localhost",
            Port:        5432,
            Database:    "dittofs_test",
            User:        "postgres",
            Password:    "postgres",
            SSLMode:     "disable",
            AutoMigrate: true,
        }

        caps := metadata.FilesystemCapabilities{
            MaxReadSize:         1048576,
            PreferredReadSize:   1048576,
            MaxWriteSize:        1048576,
            PreferredWriteSize:  1048576,
            MaxFileSize:         9223372036854775807,
            MaxFilenameLen:      255,
            MaxPathLen:          4096,
            MaxHardLinkCount:    32767,
            SupportsHardLinks:   true,
            SupportsSymlinks:    true,
            CaseSensitive:       true,
            CasePreserving:      true,
            TimestampResolution: 1,
        }

        store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
        if err != nil {
            t.Fatalf("NewPostgresMetadataStore() failed: %v", err)
        }
        t.Cleanup(func() {
            store.Close()
        })
        return store
    })
}
```

**Addition pattern** -- add a second function with same DSN skip + factory. Extract the factory to a helper to avoid duplication (or duplicate -- D-08 says self-contained).

---

## Shared Patterns

### Envelope Protocol (all 3 drivers)
**Source:** `pkg/metadata/backup/envelope.go` lines 76-129 (Writer) and lines 149-215 (Reader)
**Apply to:** All three `backup.go` files

Backup flow:
```go
envW, err := backup.NewWriter(w, engineTag)  // writes header, returns CRC-accumulating writer
// ... write schema version uint32 LE ...
// ... write engine-specific payload through envW ...
envW.Finish()  // writes trailing CRC32
```

Restore flow:
```go
engineTag, payloadR, acc, err := backup.ReadHeader(r)  // validates magic+version, returns tee reader
backup.VerifyEngine(engineTag, expectedTag)             // reject wrong engine
// ... read schema version from payloadR ...
// ... read engine-specific payload from payloadR ...
backup.VerifyCRC(r, acc)  // r is ORIGINAL reader, NOT payloadR
```

### Error Sentinels (all 3 drivers)
**Source:** `pkg/metadata/backupable.go` lines 43-63
**Apply to:** All three `backup.go` files

```go
var (
    ErrRestoreDestinationNotEmpty = errors.New("metadata: restore destination is not empty")
    ErrRestoreCorrupt             = errors.New("metadata: restore data is corrupt")
    ErrSchemaVersionMismatch      = errors.New("metadata: schema version mismatch")
    ErrBackupAborted              = errors.New("metadata: backup aborted")
)
```

Error wrapping style: `fmt.Errorf("%w: detail: %v", metadata.ErrBackupAborted, err)` -- sentinel first, detail second.

### HashSet Collection (all 3 drivers)
**Source:** `pkg/blockstore/hashset.go` lines 21-28
**Apply to:** All three `backup.go` Backup methods

```go
hs := blockstore.NewHashSet(0)
// Inside serialization loop:
hs.Add(blockRef.Hash)
// Return hs at the end
```

### Schema Version Protocol (all 3 drivers)
**Source:** RESEARCH.md Pattern 1 (no existing codebase analog)
**Apply to:** All three `backup.go` files

```go
const schemaVersion = uint32(1)

// Write (backup):
var vBuf [4]byte
binary.LittleEndian.PutUint32(vBuf[:], schemaVersion)
envW.Write(vBuf[:])

// Read (restore):
var vBuf [4]byte
io.ReadFull(payloadR, vBuf[:])
version := binary.LittleEndian.Uint32(vBuf[:])
if version != schemaVersion {
    return fmt.Errorf("%w: got %d, want %d", metadata.ErrSchemaVersionMismatch, version, schemaVersion)
}
```

### Conformance Test Wiring (all 3 test files)
**Source:** `pkg/metadata/storetest/backup_conformance.go` lines 19-63
**Apply to:** All three `*_conformance_test.go` files

```go
// BackupableStoreFactory creates a fresh MetadataStore instance for each test.
type BackupableStoreFactory func(t *testing.T) metadata.MetadataStore

func RunBackupConformanceSuite(t *testing.T, factory BackupableStoreFactory) {
    // 5 subtests: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness
}
```

Each test file adds a `TestBackupConformance` function using the SAME factory pattern as its existing `TestConformance`, calling `storetest.RunBackupConformanceSuite` instead of `storetest.RunConformanceSuite`.

## No Analog Found

| File | Role | Data Flow | Reason |
|------|------|-----------|--------|
| (none) | -- | -- | All files have adequate analogs in the existing codebase |

The envelope + schema version protocol for backup/restore is new to the codebase, but the wire format is fully defined by Phase 20's `envelope.go` and the RESEARCH.md patterns. No external analog search is needed.

## Metadata

**Analog search scope:** `pkg/metadata/store/memory/`, `pkg/metadata/store/badger/`, `pkg/metadata/store/postgres/`, `pkg/metadata/backup/`, `pkg/metadata/storetest/`, `pkg/blockstore/`
**Files scanned:** 12 analog files read directly
**Pattern extraction date:** 2026-05-27
