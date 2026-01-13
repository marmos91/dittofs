# pkg/content

Content service layer - handles raw file bytes using the Cache layer.

## Architecture

```
ContentServiceInterface (interface.go)
         ↓
ContentService (service.go) - coordinates with cache
         ↓
cache.Cache (pkg/cache/cache.go) - Chunk/Slice/Block model
         ↓
cache.Store (pkg/cache/store.go) - persistence layer
         ↓
store/memory/ - implementation
```

## Key Design Changes (Phase 1.5)

### Cache-Only Storage Model
- **No ContentStore layer** - Cache IS the storage for Phase 1
- All reads/writes go through `cache.Cache` interface
- Data is volatile (lives only in memory cache)
- Future phases will add BlockStore for persistence

### Single Global Cache
- One cache serves ALL shares
- ContentID (file handle) uniqueness ensures data isolation
- Registered once in `registry.NewRegistry()`

## ContentService Methods

### Read Operations
```go
ReadAt(ctx, shareName, contentID, buf, offset) (int, error)
GetContentSize(ctx, shareName, contentID) (uint64, error)
ContentExists(ctx, shareName, contentID) (bool, error)
```

### Write Operations
```go
WriteAt(ctx, shareName, contentID, data, offset) error
Truncate(ctx, shareName, contentID, newSize) error
Delete(ctx, shareName, contentID) error
```

### Flush Operations
```go
Flush(ctx, shareName, contentID) (*FlushResult, error)
FlushAndFinalize(ctx, shareName, contentID) (*FlushResult, error)
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
