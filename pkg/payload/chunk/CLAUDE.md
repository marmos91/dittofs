# pkg/payload/chunk

Constants and helpers for chunk-level file segmentation.

## Overview

Chunks are 64MB segments of a file used for metadata organization and lazy loading.
This package provides chunk-level calculations and the Slices iterator.

For block-level operations (4MB storage units), see `pkg/payload/block`.

## Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `Size` | 64MB | File segmentation unit for metadata organization |
| `DefaultMaxSlicesPerChunk` | 16 | Compaction trigger threshold |

## Key Functions

### Chunk Calculations
```go
IndexForOffset(offset uint64) uint32           // chunk index for file offset
OffsetInChunk(offset uint64) uint32            // offset within chunk
Range(offset, length uint64) (start, end)      // chunk range for byte range
Bounds(chunkIdx uint32) (start, end)           // file-level bounds (exclusive end)
ClipToChunk(chunkIdx, fileOffset, length)      // clip range to chunk boundaries
```

### Slices Iterator
```go
// Iterate over chunks that a byte range spans
// fileOffset is uint64 (supports large files), length is int (buffer size)
for slice := range chunk.Slices(offset, len(buf)) {
    // slice.ChunkIndex - which chunk this slice belongs to
    // slice.Offset     - offset within the chunk
    // slice.Length     - length of data in this chunk
    // slice.BufOffset  - offset into caller's buffer
}
```

## Usage

```go
import "github.com/marmos91/dittofs/pkg/payload/chunk"

// Old pattern (manual iteration)
startChunk, endChunk := chunk.Range(offset, length)
for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
    offsetInChunk, len := chunk.ClipToChunk(chunkIdx, offset+processed, remaining)
    // ...
}

// New pattern (Slices iterator) - zero-copy with dest buffer
for slice := range chunk.Slices(offset, len(buf)) {
    cache.ReadSlice(ctx, handle, slice.ChunkIndex, slice.Offset, slice.Length, buf[slice.BufOffset:])
}
```

## Package Split

The chunk package was split to separate concerns:
- `pkg/payload/chunk/` - Chunk-level constants and helpers (this package)
- `pkg/payload/block/` - Block-level constants and helpers

Import both when you need both:
```go
import (
    "github.com/marmos91/dittofs/pkg/payload/block"
    "github.com/marmos91/dittofs/pkg/payload/chunk"
)

// Chunk-level calculation
chunkIdx := chunk.IndexForOffset(fileOffset)

// Block-level calculation within chunk
blockIdx := block.IndexForOffset(offsetInChunk)
```
