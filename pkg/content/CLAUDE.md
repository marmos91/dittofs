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
(in-memory)   block.Store (S3/memory)
```

## Key Design

### Cache + Flusher Model
- **Cache**: Fast in-memory storage for all reads/writes
- **Flusher** (optional): Handles cache-to-S3 persistence
- Without flusher: cache-only mode (volatile data)
- With flusher: S3 persistence with eager upload

### Single Global Cache
- One cache serves ALL shares
- ContentID (file handle) uniqueness ensures data isolation
- Registered once in `registry.NewRegistry()`

### Eager Upload (when flusher configured)
- 4MB blocks uploaded as they become ready (not waiting for COMMIT)
- Maximizes bandwidth utilization
- COMMIT/CLOSE just waits for in-flight uploads

## ContentService Methods

### Configuration
```go
SetCache(cache *cache.Cache) error   // Required: set global cache
SetFlusher(f *flusher.Flusher)       // Optional: enable S3 persistence
```

### Read Operations
```go
ReadAt(ctx, shareName, contentID, buf, offset) (int, error)
GetContentSize(ctx, shareName, contentID) (uint64, error)
ContentExists(ctx, shareName, contentID) (bool, error)
```

### Write Operations
```go
WriteAt(ctx, shareName, contentID, data, offset) error  // Notifies flusher for eager upload
Truncate(ctx, shareName, contentID, newSize) error
Delete(ctx, shareName, contentID) error
```

### Flush Operations
```go
Flush(ctx, shareName, contentID) (*FlushResult, error)           // NFS COMMIT, SMB FLUSH
FlushAndFinalize(ctx, shareName, contentID) (*FlushResult, error) // SMB CLOSE
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

### FlushResult for Phase 1
```go
type FlushResult struct {
    AlreadyFlushed bool  // true (Phase 1: cache-only)
    Finalized      bool  // true (Phase 1: cache-only)
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

1. **Checking for ContentStore** - There is no ContentStore in Phase 1, only Cache
2. **Per-share cache registration** - Use single global cache via `SetCache()`
3. **Forgetting ErrNoCacheConfigured** - All methods return this if cache not set
4. **Assuming persistence** - Phase 1 is cache-only (volatile)
