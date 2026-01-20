# pkg/payload

Payload service layer - handles raw file bytes using the Cache layer.

## Overview

The payload package provides the main entry point for file content operations.
It coordinates between the Cache (fast in-memory storage) and TransferManager
(background persistence to block store with content-addressed deduplication).

## Architecture

```
PayloadService (service.go) - coordinates with cache + transfer manager
         ↓
     ┌───┴───────────┐
     ↓               ↓
cache.Cache    transfer.TransferManager
     ↓                    ↓
(in-memory)        ┌─────┴─────┐
                   ↓           ↓
             block.Store   ObjectStore
             (S3/memory)   (dedup metadata)
```

## Key Design

### Cache + TransferManager Model
- **Cache**: Fast in-memory storage for all reads/writes
- **TransferManager**: Handles cache-to-block-store persistence with deduplication
- Content-addressed deduplication at 4MB block level via ObjectStore

### Single Global Cache
- One cache serves ALL shares
- PayloadID uniqueness ensures data isolation
- Registered once in `registry.NewRegistry()`

### Non-blocking COMMIT
- NFS COMMIT enqueues background upload and returns immediately
- Data is safe in mmap cache (crash-safe via OS page cache)
- Block store uploads happen asynchronously
- Achieves 275+ MB/s write throughput

### Content-Addressed Deduplication
- Blocks are hashed with SHA-256 before upload
- ObjectStore tracks block hashes and reference counts
- Duplicate blocks are detected and skipped (RefCount incremented)
- Storage savings achieved when multiple files share identical blocks

## PayloadService Methods

### Constructor
```go
New(cache *cache.Cache, tm *transfer.TransferManager) (*PayloadService, error)
```

### Read Operations
```go
ReadAt(ctx, payloadID, buf, offset) (int, error)
ReadAtWithCOWSource(ctx, payloadID, cowSource, buf, offset) (int, error)
GetSize(ctx, payloadID) (uint64, error)
Exists(ctx, payloadID) (bool, error)
```

### Write Operations
```go
WriteAt(ctx, payloadID, data, offset) error  // Writes to cache only
Truncate(ctx, payloadID, newSize) error
Delete(ctx, payloadID) error
```

### Flush Operations
```go
Flush(ctx, payloadID) (*FlushResult, error)  // NFS COMMIT - non-blocking
```

## Critical Conventions

### ContentID Is File Handle
- ContentID = `metadata.ContentID` = opaque file identifier
- Used directly as cache key (converted to `[]byte`)
- Unique per file, provides data isolation between files/shares

### Cache Operations Are Chunk-Aware
- Large reads/writes are split across chunk boundaries (64MB chunks)
- `cache.ChunkRange()` calculates which chunks a range spans
- Each chunk is addressed independently

### FlushResult
```go
type FlushResult struct {
    AlreadyFlushed bool  // true if no pending data
    Finalized      bool  // true if data is durable
}
```

## Common Patterns

### Read with Chunk Spanning
```go
// Calculate chunk range
startChunk, endChunk := cache.ChunkRange(offset, uint64(len(buf)))

// Read from each chunk
for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
    found, err := cache.ReadAt(ctx, fileHandle, chunkIdx, offsetInChunk, length, dest)
    // Handle not-found as zeros (sparse file behavior)
}
```

### Write with Chunk Spanning
```go
// Calculate chunk range
startChunk, endChunk := cache.ChunkRange(offset, uint64(len(data)))

// Write to each chunk
for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
    err := cache.WriteAt(ctx, fileHandle, chunkIdx, data[dataStart:dataEnd], offsetInChunk)
}
```

## Migration from pkg/content

| Old (pkg/content) | New (pkg/payload) |
|-------------------|-------------------|
| `ContentService` | `PayloadService` |
| `content.New()` | `payload.New()` |
| `SetFlusher(f)` | Constructor injection |
| `FlushResult` | `FlushResult` |
| `StorageStats` | `StorageStats` |

## Common Mistakes

1. **Blocking on COMMIT** - Use non-blocking FlushRemainingAsync, not sync flush
2. **Per-share cache registration** - Use single global cache via `SetCache()`
3. **Forgetting ErrNoCacheConfigured** - All methods return this if cache not set
4. **Calling cache.Sync() on COMMIT** - Too slow, use mmap's inherent crash safety
