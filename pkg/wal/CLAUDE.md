# pkg/wal

Write-Ahead Log persistence layer for crash recovery.

## Overview

The WAL package provides crash recovery for the cache layer. It logs slice writes to persistent storage, enabling the cache to recover unflushed data after a crash or restart.

## Architecture

```
Cache.WriteSlice()
        │
        ▼
  Persister.AppendSlice() ← logs write to WAL
        │
        ▼
  (slice data in memory)

Server Crash → Restart
        │
        ▼
  Persister.Recover() ← replays WAL entries
        │
        ▼
  Cache populated with unflushed slices
```

## Key Types

### Persister (Interface)

Pluggable interface for WAL implementations.

```go
type Persister interface {
    // Log a slice write
    AppendSlice(entry *SliceEntry) error

    // Log a file removal (clear all slices)
    AppendRemove(fileHandle []byte) error

    // Fsync WAL to disk
    Sync() error

    // Replay WAL entries on startup
    Recover() ([]SliceEntry, error)

    // Cleanup
    Close() error

    // Check if persistence is enabled
    IsEnabled() bool
}
```

### SliceEntry

WAL record for a single slice write.

```go
type SliceEntry struct {
    FileHandle []byte
    ChunkIdx   uint32
    SliceID    uint64
    Offset     uint32
    Data       []byte
    State      uint8       // SliceStatePending, etc.
    CreatedAt  int64       // Unix nano
    BlockRefs  []BlockRef  // For flushed slices
}
```

### BlockRef

Reference to a block in the block store.

```go
type BlockRef struct {
    BlockKey string
    Offset   uint32
    Length   uint32
}
```

## Implementations

### MmapPersister

Memory-mapped file persister for high performance.

```go
// Create mmap-backed persister
persister, err := wal.NewMmapPersister("/var/lib/dittofs/wal")
if err != nil {
    return err
}
defer persister.Close()

// Used by cache
cache, err := cache.NewWithPersister(maxSize, persister)
```

**Features:**
- Memory-mapped file for fast append
- Automatic file growth
- Binary encoding for efficiency
- OS page cache provides crash safety

### NullPersister

No-op persister for in-memory only deployments.

```go
persister := wal.NewNullPersister()
// All operations are no-ops
// IsEnabled() returns false
```

**Use cases:**
- Testing without disk I/O
- Ephemeral deployments
- Development/debugging

## Usage with Cache

```go
import (
    "github.com/marmos91/dittofs/pkg/cache"
    "github.com/marmos91/dittofs/pkg/wal"
)

// Option 1: Convenience constructor
c, err := cache.NewWithMmap("/var/lib/dittofs/wal", maxSize)

// Option 2: Custom persister
persister, err := wal.NewMmapPersister("/var/lib/dittofs/wal")
c, err := cache.NewWithPersister(maxSize, persister)

// Option 3: No persistence
c := cache.New(maxSize)
```

## Recovery Flow

```go
// On startup:
entries, err := persister.Recover()
if err != nil {
    return err
}

// Replay entries into cache
for _, entry := range entries {
    cache.RestoreSlice(entry)
}

// Then flush to block store via TransferManager
recovery.RecoverUnflushedSlices(ctx, persister, cache, transferManager)
```

## Key Design Decisions

### Append-Only Log
- WAL is append-only for simplicity and performance
- No in-place updates or deletions
- Recovery replays full log and deduplicates

### Binary Encoding
- Custom binary format (not JSON/protobuf)
- Minimal overhead for high write rates
- Fixed-size header, variable-size data

### OS Page Cache Reliance
- No explicit fsync on every write
- Relies on OS page cache for crash safety
- Explicit Sync() available for durability guarantees

### Separation from Cache
- WAL is a separate package, not embedded in cache
- Enables different persistence strategies
- Clean interface boundaries

## Common Mistakes

1. **Calling Sync() too often** - Only needed for strict durability, expensive
2. **Not closing persister** - Leaks file descriptors and memory mappings
3. **Ignoring Recover() errors** - Can lead to data loss
4. **Using NullPersister in production** - No crash recovery

## File Format

```
[Header: 8 bytes]
  - Magic: 4 bytes ("DWAL")
  - Version: 2 bytes
  - Flags: 2 bytes

[Entry: variable]
  - Type: 1 byte (slice/remove)
  - Length: 4 bytes
  - Payload: variable bytes

[Entry]...
```
