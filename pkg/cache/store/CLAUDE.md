# pkg/cache/store

Persistence layer for the slice cache. Handles low-level storage operations.

## Architecture

```
cache.Store Interface (pkg/cache/store.go)
    - Defines persistence operations
         ↓
Implementations:
    store/memory/ - In-memory storage (volatile)
    store/fs/     - Filesystem storage (future)
```

## Design Principle

**Separation of Concerns:**
- **Store**: Only persistence (add/get/update slices, file management, size tracking)
- **Cache**: Only business logic (merging, coalescing, optimization, statistics)

The store does NOT know about:
- Sequential write optimization
- Newest-wins merging
- Coalescing adjacent slices
- Cache hit/miss statistics

## Key Types

### SliceState
```go
SliceStatePending   // Unflushed, dirty
SliceStateFlushed   // Safe in block storage
SliceStateUploading // Flush in progress
```

### Slice
```go
type Slice struct {
    ID        string
    Offset    uint32      // Within chunk (0 to ChunkSize-1)
    Length    uint32
    Data      []byte
    State     SliceState
    CreatedAt time.Time
    BlockRefs []BlockRef  // After flush
}
```

### SliceUpdate
Used for partial updates:
```go
type SliceUpdate struct {
    State     *SliceState  // Optional
    BlockRefs []BlockRef   // Optional
    Data      []byte       // Optional (for extending)
    Length    *uint32      // Optional
    Offset    *uint32      // Optional
}
```

## Store Interface

```go
type Store interface {
    // File operations
    CreateFile(ctx, fileHandle) error
    FileExists(ctx, fileHandle) bool
    RemoveFile(ctx, fileHandle) error
    ListFiles(ctx) [][]byte

    // Chunk operations
    GetChunkIndices(ctx, fileHandle) []uint32
    RemoveChunk(ctx, fileHandle, chunkIdx) error

    // Slice operations
    GetSlices(ctx, fileHandle, chunkIdx) []Slice
    AddSlice(ctx, fileHandle, chunkIdx, slice) error
    UpdateSlice(ctx, fileHandle, chunkIdx, sliceID, update) error
    SetSlices(ctx, fileHandle, chunkIdx, slices) error
    FindSlice(ctx, fileHandle, sliceID) (chunkIdx, slice, error)

    // Size tracking
    GetTotalSize() uint64
    AddSize(delta int64)

    // Lifecycle
    IsClosed() bool
    Close() error
}
```

## Memory Implementation

Located in `memory/store.go`:

- **Thread-safe**: Uses `sync.RWMutex` for map-level thread safety
- **Deep copies**: All returned slices are copies (prevents external mutation)
- **Size tracking**: Atomic uint64 for total size
- **Idempotent**: RemoveFile/RemoveChunk succeed even if not found

## Adding a New Implementation

1. Create `pkg/cache/store/<name>/store.go`
2. Implement `cache.Store` interface
3. Add compile-time check: `var _ cache.Store = (*Store)(nil)`
4. Use the store with: `cache.NewWithStore(yourStore, maxSize)`

## Common Mistakes

1. **Mutating returned slices** - Always deep copy before returning
2. **Forgetting size tracking** - AddSize/subtract on add/remove/update
3. **Non-idempotent removes** - Should return nil if not found
4. **Business logic in store** - That belongs in the cache layer
