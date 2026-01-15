# pkg/cache/wal

Write-Ahead Log persistence layer for cache crash recovery.

## Overview

The WAL package provides crash recovery for the cache layer. It logs slice writes to persistent storage, enabling the cache to recover unflushed data after a crash or restart.

## Architecture

```
Cache.WriteSlice()
        │
        ▼
  MmapPersister.AppendSlice() ← logs write to WAL
        │
        ▼
  (slice data in memory)

Server Crash → Restart
        │
        ▼
  MmapPersister.Recover() ← replays WAL entries
        │
        ▼
  Cache populated with unflushed slices
```

## Key Types

This package defines the canonical types used by both WAL and cache:
- `SliceState` - re-exported by cache as `cache.SliceState`
- `BlockRef` - re-exported by cache as `cache.BlockRef`
- `Slice` - canonical slice type (re-exported by cache)
- `SliceEntry` - WAL persistence format (FileHandle + ChunkIdx + embedded Slice)

### SliceState

```go
type SliceState int

const (
    SliceStatePending   SliceState = iota  // Unflushed data
    SliceStateFlushed                       // Safe in block storage
    SliceStateUploading                     // Flush in progress
)
```

### BlockRef

Reference to a block in the block store.

```go
type BlockRef struct {
    ID   string  // Block's unique identifier
    Size uint32  // Actual size (may be < BlockSize for last block)
}
```

### SliceEntry

WAL record for a single slice write. Embeds Slice for zero-copy efficiency.

```go
type SliceEntry struct {
    FileHandle string  // File this slice belongs to
    ChunkIdx   uint32  // Chunk index within file
    Slice              // Embedded slice (ID, Offset, Length, Data, State, CreatedAt, BlockRefs)
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
- `AppendSlice(entry *SliceEntry) error` - Log a slice write
- `AppendRemove(fileHandle string) error` - Log a file removal
- `Sync() error` - Fsync WAL to disk
- `Recover() ([]SliceEntry, error)` - Replay WAL entries on startup
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
  - Version: uint16 (2 bytes)
  - Entry count: uint32 (4 bytes)
  - Next write offset: uint64 (8 bytes)
  - Total data size: uint64 (8 bytes)
  - Reserved: 38 bytes

[Slice Entry: variable]
  - Type: 1 byte (0=slice)
  - File handle length: uint16 (2 bytes)
  - File handle: variable
  - Chunk index: uint32 (4 bytes)
  - Slice ID: 36 bytes (UUID string)
  - Offset in chunk: uint32 (4 bytes)
  - Data length: uint32 (4 bytes)
  - State: uint8 (1 byte)
  - CreatedAt: int64 (8 bytes)
  - BlockRef count: uint16 (2 bytes)
  - BlockRefs: variable (ID length + ID + size per ref)
  - Data: variable

[Remove Entry: variable]
  - Type: 1 byte (3=remove)
  - File handle length: uint16 (2 bytes)
  - File handle: variable
```

## Package Structure

- `errors.go` - Error definitions (ErrPersisterClosed, ErrCorrupted, ErrVersionMismatch)
- `types.go` - Type definitions (Slice, SliceEntry, SliceState, BlockRef)
- `mmap.go` - MmapPersister implementation
- `mmap_test.go` - Tests and benchmarks
