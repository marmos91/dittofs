# pkg/content

Content service layer - handles raw file bytes using the Cache layer.

## Architecture

```
ContentServiceInterface (interface.go)
         ↓
ContentService (service.go) - coordinates with cache + flusher
         ↓
     ┌───┴───┐
     ↓       ↓
cache.Cache  flusher.Flusher (optional)
     ↓              ↓
(in-memory)   block.Store (S3/memory/fs)
```

## Key Design

### Cache + Flusher Model
- **Cache**: Fast in-memory storage for all reads/writes
- **Flusher** (optional): Handles cache-to-block-store persistence
- Without flusher: cache-only mode (volatile data)
- With flusher: persistent storage with background upload

### Single Global Cache
- One cache serves ALL shares
- ContentID (file handle) uniqueness ensures data isolation
- Registered once in `registry.NewRegistry()`

### Non-blocking COMMIT (when flusher configured)
- NFS COMMIT enqueues background upload and returns immediately
- Data is safe in mmap cache (crash-safe via OS page cache)
- Block store uploads happen asynchronously
- Achieves 275+ MB/s write throughput

## ContentService Methods

### Configuration
```go
SetCache(cache *cache.Cache) error   // Required: set global cache
SetFlusher(f *flusher.Flusher)       // Optional: enable block store persistence
```

### Read Operations
```go
ReadAt(ctx, shareName, contentID, buf, offset) (int, error)
GetContentSize(ctx, shareName, contentID) (uint64, error)
ContentExists(ctx, shareName, contentID) (bool, error)
```

### Write Operations
```go
WriteAt(ctx, shareName, contentID, data, offset) error  // Writes to cache only
Truncate(ctx, shareName, contentID, newSize) error
Delete(ctx, shareName, contentID) error
```

### Flush Operations
```go
Flush(ctx, shareName, contentID) (*FlushResult, error)           // NFS COMMIT - non-blocking
FlushAndFinalize(ctx, shareName, contentID) (*FlushResult, error) // SMB CLOSE - blocking
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
    data, found, err := cache.ReadSlice(ctx, fileHandle, chunkIdx, offsetInChunk, length)
    // Handle not-found as zeros (sparse file behavior)
}
```

### Write with Chunk Spanning
```go
// Calculate chunk range
startChunk, endChunk := cache.ChunkRange(offset, uint64(len(data)))

// Write to each chunk
for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
    err := cache.WriteSlice(ctx, fileHandle, chunkIdx, data[dataStart:dataEnd], offsetInChunk)
}
```

## Common Mistakes

1. **Blocking on COMMIT** - Use non-blocking FlushRemainingAsync, not sync flush
2. **Per-share cache registration** - Use single global cache via `SetCache()`
3. **Forgetting ErrNoCacheConfigured** - All methods return this if cache not set
4. **Calling cache.Sync() on COMMIT** - Too slow, use mmap's inherent crash safety
