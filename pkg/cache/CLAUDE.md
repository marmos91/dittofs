# pkg/cache

Slice-aware cache layer for the Chunk/Slice/Block storage model.

## Architecture

```
Cache Interface (cache.go)
    - Business logic: merging, coalescing, optimization
         ↓
Store Interface (store.go)
    - Persistence: add/get/update slices
         ↓
store/memory/ (implementations)
```

## Package Structure

Following the metadata package pattern:
- `cache.go` - Cache interface AND implementation (business logic layer)
- `store.go` - Store interface (persistence layer)
- `store/memory/store.go` - Memory Store implementation
- `store/memory/benchmark_test.go` - Performance benchmarks

## Key Design Decisions

### Single Global Cache
- One cache serves ALL shares (not per-share)
- ContentID uniqueness guarantees data isolation
- Reduces memory overhead from multiple cache instances

### Slice-Aware API
- `WriteSlice(fileHandle, chunkIdx, data, offset)` - direct slice writes
- `ReadSlice(fileHandle, chunkIdx, offset, length)` - merge reads
- No translation between "bytes" and "slices"

### Business Logic in Cache Layer
The Cache handles:
1. **Sequential write optimization** - extends existing slices instead of creating new ones
2. **Newest-wins read merging** - overlapping slices resolved by creation time
3. **Write coalescing** - merges adjacent pending slices before flush

### Persistence in Store Layer
The Store interface handles:
1. **Slice storage** - add/get/update/replace slices
2. **File/chunk management** - create/remove files and chunks
3. **Size tracking** - total bytes stored

## Slice States

```
SliceStatePending → SliceStateUploading → SliceStateFlushed
```

1. **Pending**: Unflushed writes, cannot evict
2. **Uploading**: Flush in progress, cannot evict
3. **Flushed**: Safe in block storage, can evict

## Sequential Write Optimization

NFS clients write in 16KB-32KB chunks. Without optimization:
- 10MB file = 320 writes = 320 slices (bad)

With `tryExtendSlice()`:
- Sequential writes extend existing pending slice
- 10MB file = 320 writes = 1 slice (good)

## Newest-Wins Algorithm

When reading overlapping slices:
```
Slices (newest first): [Slice2: 2-3MB] [Slice1: 0-4MB] [Slice0: 0-64MB]
Read range: 1-5MB
Result:
  - 1-2MB: from Slice1 (no newer covers it)
  - 2-3MB: from Slice2 (newest)
  - 3-4MB: from Slice1
  - 4-5MB: from Slice0 (oldest still needed)
```

## Implementations

| Component | Location | Use Case |
|-----------|----------|----------|
| Cache | `cache.go` | Business logic: merging, coalescing, locking |
| Memory Store | `store/memory/store.go` | Volatile storage, development, testing |
| (Filesystem Store) | Future | Persistence across restarts |

## Common Mistakes

1. **Per-share caches** - Use single global cache, ContentID isolates data
2. **Evicting dirty entries** - Only flushed slices can be evicted
3. **Parsing slices in store** - Store just persists, cache handles logic
4. **Creating many slices** - Sequential optimization should merge them

## Usage Example

```go
import (
    "github.com/marmos91/dittofs/pkg/cache"
    "github.com/marmos91/dittofs/pkg/cache/store/memory"
)

// Create cache with memory store
c := cache.NewWithStore(memory.New(), 0) // 0 = unlimited

// Write (auto-extends sequential writes)
c.WriteSlice(ctx, fileHandle, chunkIdx, data, offset)

// Read (auto-merges with newest-wins)
data, found, err := c.ReadSlice(ctx, fileHandle, chunkIdx, offset, length)

// Get dirty slices for flush (auto-coalesces)
pending, err := c.GetDirtySlices(ctx, fileHandle)

// Mark flushed after upload
c.MarkSliceFlushed(ctx, fileHandle, sliceID, blockRefs)
```
