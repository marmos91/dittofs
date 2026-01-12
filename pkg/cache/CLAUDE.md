# pkg/cache

Slice-aware cache layer for the Chunk/Slice/Block storage model.

## Architecture

```
Cache (cache.go)
    - Business logic: merging, coalescing, optimization
    - In-memory storage (integrated)
    - Optional mmap backing (future)
```

## Package Structure

- `cache.go` - Cache implementation (business logic + storage)
- `types.go` - Slice, SliceState, SliceUpdate, BlockRef types
- `benchmark_test.go` - Performance benchmarks

## Key Design Decisions

### Single Global Cache
- One cache serves ALL shares (not per-share)
- ContentID uniqueness guarantees data isolation
- Reduces memory overhead from multiple cache instances

### Simplified Architecture
- No Store interface - storage integrated directly into Cache
- Prepares for optional mmap backing without abstraction overhead
- S3 is the real persistence layer (blocks flushed there)

### Slice-Aware API
- `WriteSlice(fileHandle, chunkIdx, data, offset)` - direct slice writes
- `ReadSlice(fileHandle, chunkIdx, offset, length)` - merge reads
- No translation between "bytes" and "slices"

### Business Logic
The Cache handles:
1. **Sequential write optimization** - extends existing slices instead of creating new ones
2. **Newest-wins read merging** - overlapping slices resolved by creation time
3. **Write coalescing** - merges adjacent pending slices before flush

## Slice States

```
SliceStatePending → SliceStateUploading → SliceStateFlushed
```

1. **Pending**: Unflushed writes, cannot evict
2. **Uploading**: Flush in progress, cannot evict
3. **Flushed**: Safe in block storage (S3), can evict

## Sequential Write Optimization

NFS clients write in 16KB-32KB chunks. Without optimization:
- 10MB file = 320 writes = 320 slices (bad)

With `tryExtendAdjacentSlice()`:
- Sequential writes extend existing pending slice
- 10MB file = 320 writes = 1 slice (good)

Uses Go's `append()` for amortized O(1) growth.

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

## Common Mistakes

1. **Per-share caches** - Use single global cache, ContentID isolates data
2. **Evicting dirty entries** - Only flushed slices can be evicted
3. **Creating many slices** - Sequential optimization should merge them

## Usage Example

```go
import "github.com/marmos91/dittofs/pkg/cache"

// Create cache (in-memory)
c := cache.New(0) // 0 = unlimited size

// Write (auto-extends sequential writes)
c.WriteSlice(ctx, fileHandle, chunkIdx, data, offset)

// Read (auto-merges with newest-wins)
data, found, err := c.ReadSlice(ctx, fileHandle, chunkIdx, offset, length)

// Get dirty slices for flush (auto-coalesces)
pending, err := c.GetDirtySlices(ctx, fileHandle)

// Mark flushed after upload to S3
c.MarkSliceFlushed(ctx, fileHandle, sliceID, blockRefs)
```

## Performance Requirements

Sequential 32KB writes MUST achieve > 3000 MB/s.
This is critical for NFS file copy performance.

See BENCHMARKS.md for current baseline measurements.
