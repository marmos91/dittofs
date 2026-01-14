# pkg/cache/wal

Write-Ahead Log persistence layer for cache crash recovery.

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

This package defines the canonical types used by both WAL and cache:
- `SliceState` - re-exported by cache as `cache.SliceState`
- `BlockRef` - re-exported by cache as `cache.BlockRef`
- `SliceEntry` - WAL persistence format (includes FileHandle, ChunkIdx for context)

### Persister (Interface)

Pluggable interface for WAL implementations.

```go
type Persister interface {
    // Log a slice write
    AppendSlice(entry *SliceEntry) error

    // Log a file removal (clear all slices)
    AppendRemove(fileHandle string) error

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

WAL record for a single slice write.

```go
type SliceEntry struct {
    FileHandle string      // File this slice belongs to
    ChunkIdx   uint32      // Chunk index within file
    SliceID    string      // Unique slice identifier
    Offset     uint32      // Byte offset within chunk
    Length     uint32      // Size of slice data
    Data       []byte      // Actual content
    State      SliceState  // Pending/Flushed/Uploading
    CreatedAt  time.Time   // Creation timestamp
    BlockRefs  []BlockRef  // Block references (for flushed slices)
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
cache, err := cache.NewWithWal(maxSize, persister)
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
    "github.com/marmos91/dittofs/pkg/cache/wal"
)

// Option 1: With WAL persistence (create persister externally)
persister, err := wal.NewMmapPersister("/var/lib/dittofs/wal")
if err != nil {
    return err
}
c, err := cache.NewWithWal(maxSize, persister)

// Option 2: No persistence (in-memory only)
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
