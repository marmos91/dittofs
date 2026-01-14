# pkg/payload/chunk

Constants and helpers for the Chunk/Slice/Block storage model.

## Overview

This is the single source of truth for chunk/slice/block constants.
All packages should import from here rather than defining their own.

## Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `ChunkSize` | 64MB | File segmentation unit for metadata organization |
| `BlockSize` | 4MB | Storage unit (one S3 object or filesystem file) |
| `MinBlockSize` | 1MB | Minimum allowed block size |
| `MaxBlockSize` | 16MB | Maximum allowed block size |
| `DefaultMaxSlicesPerChunk` | 16 | Compaction trigger threshold |

## Key Functions

### Chunk Calculations
```go
IndexForOffset(offset uint64) uint32           // chunk index for file offset
OffsetInChunk(offset uint64) uint32            // offset within chunk
Range(offset, length uint64) (start, end)      // chunk range for byte range
ChunkBounds(chunkIdx uint32) (start, end)      // file-level bounds
```

### Block Calculations
```go
BlockIndexForOffset(offsetInChunk uint32) uint32  // block index within chunk
OffsetInBlock(offsetInChunk uint32) uint32        // offset within block
BlockRange(offset, length uint32) (start, end)    // block range for chunk range
BlockBounds(blockIdx uint32) (start, end)         // chunk-level bounds
BlocksPerChunk() uint32                           // blocks in a full chunk (16)
```

### Range Helpers
```go
ClipToChunk(chunkIdx, fileOffset, length) (offsetInChunk, clippedLength)
```

## Usage

```go
import "github.com/marmos91/dittofs/pkg/payload/chunk"

// Calculate which chunks a read spans
startChunk, endChunk := chunk.Range(offset, length)

// Get chunk-local offset
offsetInChunk := chunk.OffsetInChunk(fileOffset)

// Calculate block range within chunk
startBlock, endBlock := chunk.BlockRange(offsetInChunk, length)
```

## Why This Package Exists

Previously constants were duplicated across:
- `pkg/cache/types.go`
- `pkg/metadata/chunks.go`
- `pkg/transfer/manager.go`
- `pkg/payload/store/store.go`

This package centralizes them to avoid drift and circular imports.
Other packages re-export these for backward compatibility.
