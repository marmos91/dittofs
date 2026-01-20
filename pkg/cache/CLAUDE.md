# pkg/cache

Block-aware cache layer for the Chunk/Block storage model.

## Architecture

```
Cache (cache.go)
    - Business logic: block buffer management, coverage tracking
    - In-memory storage (integrated)
    - Optional WAL persistence via wal.MmapPersister
```

## Package Structure

- `cache.go` - Core types (Cache, fileEntry, chunkEntry, blockBuffer), constructors, helpers
- `write.go` - Write, block buffer allocation
- `read.go` - Read, IsRangeCovered, IsBlockFullyCovered
- `flush.go` - GetDirtyBlocks, MarkBlockUploaded, MarkBlockUploading, GetBlockData
- `eviction.go` - LRU eviction (EvictLRU, Evict, EvictAll)
- `state.go` - Remove, Truncate, HasDirtyData, GetFileSize, Stats, Close, Sync
- `types.go` - BlockState, PendingBlock, Stats types; coverage bitmap helpers
- `wal/` - WAL persistence layer (BlockWriteEntry)
- `benchmark_test.go` - Performance benchmarks

## WAL Persistence

The cache uses `*wal.MmapPersister` for crash recovery:
- `NewWithWal(maxSize, persister)` - Create cache with WAL persistence
- `New(maxSize)` - Create in-memory cache (no persistence)

Create the WAL persister externally for better separation of concerns.

## Key Design Decisions

### Single Global Cache
- One cache serves ALL shares (not per-share)
- ContentID uniqueness guarantees data isolation
- Reduces memory overhead from multiple cache instances

### Simplified Architecture
- No Store interface - storage integrated directly into Cache
- Prepares for optional mmap backing without abstraction overhead
- S3 is the real persistence layer (blocks flushed there)

### Block Buffer Model
- `Write(payloadID, chunkIdx, data, offset)` - writes directly to 4MB block buffers
- `Read(payloadID, chunkIdx, offset, length, dest)` - reads from block buffers
- Coverage bitmap tracks which bytes have been written (64-byte granularity)
- No slice coalescing needed - data goes directly to target position

### Business Logic
The Cache handles:
1. **Block buffer allocation** - 4MB buffers created on-demand
2. **Coverage tracking** - bitmap tracks written bytes for sparse file support
3. **OOM prevention** - backpressure via ErrCacheFull when cache is full of dirty data

## Block States

```
BlockStatePending → BlockStateUploading → BlockStateUploaded
```

1. **Pending**: Unflushed writes, cannot evict
2. **Uploading**: Flush in progress, cannot evict
3. **Uploaded**: Safe in block storage (S3), can evict

## LRU Eviction

Cache enforces `maxSize` using LRU eviction with dirty data protection:

1. **Automatic eviction** - On `Write`, if cache would exceed maxSize, evicts uploaded blocks from LRU files
2. **LRU tracking** - Each file tracks `lastAccess` time (updated on writes)
3. **Dirty protection** - Only `BlockStateUploaded` blocks can be evicted; pending/uploading are protected
4. **Manual eviction** - `EvictLRU(ctx, targetFreeBytes)` for explicit eviction

```go
// Create cache with 1GB limit
c := cache.New(1 << 30)

// Automatic eviction happens on Write when full
c.Write(ctx, payloadID, chunkIdx, data, offset)

// Manual eviction to free 100MB
evicted, err := c.EvictLRU(ctx, 100*1024*1024)

// Get cache stats
stats := c.Stats()
// stats.DirtyBytes - protected, cannot evict
// stats.UploadedBytes - can be evicted
```

## Memory Tracking

The cache tracks actual memory allocation (BlockSize per block buffer), not bytes written:
- Each block buffer allocates 4MB regardless of content
- `GetTotalSize()` returns total allocated memory
- `Stats()` returns breakdown of actual data written (DirtyBytes + UploadedBytes)

## Common Mistakes

1. **Per-share caches** - Use single global cache, ContentID isolates data
2. **Evicting dirty entries** - Only uploaded blocks can be evicted
3. **Ignoring ErrCacheFull** - Indicates backpressure; caller should flush before retrying

## Usage Example

```go
import (
    "github.com/marmos91/dittofs/pkg/cache"
    "github.com/marmos91/dittofs/pkg/cache/wal"
)

// Create cache (in-memory only, no persistence)
c := cache.New(0) // 0 = unlimited size

// Create cache with WAL persistence (crash recovery)
persister, err := wal.NewMmapPersister("/var/lib/dittofs/wal")
if err != nil {
    return err
}
c, err := cache.NewWithWal(1<<30, persister) // 1GB max

// Write data directly to block buffers
c.Write(ctx, payloadID, chunkIdx, data, offset)

// Read data from block buffers
dest := make([]byte, length)
found, err := c.Read(ctx, payloadID, chunkIdx, offset, length, dest)

// Get dirty blocks for flush
pending, err := c.GetDirtyBlocks(ctx, payloadID)

// Mark block as uploaded after upload to block store
c.MarkBlockUploaded(ctx, payloadID, chunkIdx, blockIdx)

// Sync WAL to disk (if persistence enabled)
c.Sync()
```

## Performance Requirements

Sequential 32KB writes MUST achieve > 3000 MB/s.
This is critical for NFS file copy performance.

See BENCHMARKS.md for current baseline measurements.
