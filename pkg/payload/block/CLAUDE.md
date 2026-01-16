# pkg/payload/block

Constants and helpers for block-level storage operations.

## Overview

Blocks are the physical storage units in DittoFS - each block becomes a single
object in the block store (S3, filesystem). The 4MB default size balances:
- S3 PUT efficiency (larger objects = better throughput)
- Memory usage (reasonable buffer size)
- Latency (partial blocks on COMMIT are manageable)

For chunk-level operations (64MB file segments), see `pkg/payload/chunk`.

## Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `Size` | 4MB | Default block size for storage |
| `MinSize` | 1MB | Minimum allowed block size |
| `MaxSize` | 16MB | Maximum allowed block size |

## Key Functions

### Block Calculations
```go
IndexForOffset(offsetInChunk uint32) uint32    // block index within chunk
OffsetInBlock(offsetInChunk uint32) uint32     // offset within block
Range(offset, length uint32) (start, end)      // block range for chunk range
Bounds(blockIdx uint32) (start, end)           // chunk-level bounds (exclusive end)
PerChunk(chunkSize uint32) uint32              // blocks in a full chunk (16)
```

## Usage

```go
import "github.com/marmos91/dittofs/pkg/payload/block"

// Calculate which block an offset falls into
offsetInChunk := chunk.OffsetInChunk(fileOffset)
blockIdx := block.IndexForOffset(offsetInChunk)

// Calculate block range for a chunk-level range
startBlock, endBlock := block.Range(offsetInChunk, length)

// Number of blocks per chunk
numBlocks := block.PerChunk(chunk.Size)  // 16
```

## Package Split

The block package was split from chunk to separate concerns:
- `pkg/payload/chunk/` - Chunk-level constants and helpers (64MB segments)
- `pkg/payload/block/` - Block-level constants and helpers (4MB storage units)

This prevents confusion between file segmentation (chunks) and storage units (blocks).
