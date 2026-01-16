# pkg/cache/wal

Write-Ahead Log persistence layer for cache crash recovery.

## Overview

The WAL package provides crash recovery for the cache layer. It logs block writes to persistent storage, enabling the cache to recover unflushed data after a crash or restart.

## Architecture

```
Cache.Write()
        |
        v
  MmapPersister.AppendBlockWrite() <- logs write to WAL
        |
        v
  (block data in memory)

Server Crash -> Restart
        |
        v
  MmapPersister.Recover() <- replays WAL entries
        |
        v
  Cache populated with unflushed blocks
```

## Key Types

### BlockWriteEntry

WAL record for a single write operation.

```go
type BlockWriteEntry struct {
    PayloadID     string  // Identifies the file this write belongs to
    ChunkIdx      uint32  // Chunk index within file
    BlockIdx      uint32  // Block index within chunk
    OffsetInBlock uint32  // Byte offset within the block (0 to BlockSize-1)
    Data          []byte  // The bytes written
}
```

## MmapPersister

Memory-mapped file persister for high performance.

```go
// Create mmap-backed persister
persister, err := wal.NewMmapPersister("/var/lib/dittofs/wal")
if err != nil {
    return err
}
defer persister.Close()

// Used by cache
cache, err := cache.NewWithWal(maxSize, persister)
```

**Features:**
- Memory-mapped file for fast append
- Automatic file growth
- Binary encoding for efficiency
- OS page cache provides crash safety

**Methods:**
- `AppendBlockWrite(entry *BlockWriteEntry) error` - Log a block write
- `AppendRemove(payloadID string) error` - Log a file removal
- `Sync() error` - Fsync WAL to disk
- `Recover() ([]BlockWriteEntry, error)` - Replay WAL entries on startup
- `Close() error` - Cleanup resources
- `IsEnabled() bool` - Always returns true

## Usage with Cache

```go
import (
    "github.com/marmos91/dittofs/pkg/cache"
    "github.com/marmos91/dittofs/pkg/cache/wal"
)

// Option 1: With WAL persistence
persister, err := wal.NewMmapPersister("/var/lib/dittofs/wal")
if err != nil {
    return err
}
c, err := cache.NewWithWal(maxSize, persister)

// Option 2: No persistence (in-memory only)
c := cache.New(maxSize)
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
- Clean boundaries between cache logic and persistence

## Common Mistakes

1. **Calling Sync() too often** - Only needed for strict durability, expensive
2. **Not closing persister** - Leaks file descriptors and memory mappings
3. **Ignoring Recover() errors** - Can lead to data loss

## File Format

```
[Header: 64 bytes]
  - Magic: 4 bytes ("DTTC")
  - Version: uint16 (2 bytes) - version 2 for block-level format
  - Entry count: uint32 (4 bytes)
  - Next write offset: uint64 (8 bytes)
  - Total data size: uint64 (8 bytes)
  - Reserved: 38 bytes

[Block Write Entry: variable]
  - Type: 1 byte (0=blockWrite)
  - Payload ID length: uint16 (2 bytes)
  - Payload ID: variable
  - Chunk index: uint32 (4 bytes)
  - Block index: uint32 (4 bytes)
  - Offset in block: uint32 (4 bytes)
  - Data length: uint32 (4 bytes)
  - Data: variable

[Remove Entry: variable]
  - Type: 1 byte (3=remove)
  - Payload ID length: uint16 (2 bytes)
  - Payload ID: variable
```

## Package Structure

- `errors.go` - Error definitions (ErrPersisterClosed, ErrCorrupted, ErrVersionMismatch)
- `types.go` - Type definitions (BlockWriteEntry)
- `mmap.go` - MmapPersister implementation
- `mmap_test.go` - Tests and benchmarks
