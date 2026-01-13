# pkg/transfer

Cache-to-block-store transfer layer - handles eager block upload and parallel download.

## Overview

The transfer package manages data movement between the cache and block store:
- **Eager upload**: Upload 4MB blocks as soon as they're ready (don't wait for COMMIT)
- **Async flush**: Non-blocking COMMIT via background queue
- **Parallel download**: Fetch blocks from block store on cache miss
- **Recovery**: Scan cache on startup for unflushed slices

## Architecture

```
BlockService.WriteAt()
        ↓
  Cache.WriteSlice()
        ↓
  (writes accumulate in cache)

BlockService.Flush() (NFS COMMIT / SMB FLUSH)
        ↓
  TransferManager.FlushRemainingAsync() ← enqueues background upload (non-blocking)
        ↓
  TransferQueue workers ← upload to block store asynchronously

BlockService.FlushAndFinalize() (SMB CLOSE)
        ↓
  TransferManager.WaitForUploads() ← wait for in-flight uploads
        ↓
  TransferManager.FlushRemaining() ← upload partial blocks (blocking)

BlockService.ReadAt() (cache miss)
        ↓
  TransferManager.ReadBlocks() ← parallel fetch from block store
        ↓
  Blocks cached for future reads
```

## Key Types

### TransferManager
Main orchestrator for cache-to-block-store transfers.

```go
type TransferManager struct {
    cache      *cache.Cache
    blockStore block.Store
    config     Config
    queue      *TransferQueue
}
```

### TransferQueueEntry (Interface)
Generic interface for transfer entries - enables specialized implementations.

```go
type TransferQueueEntry interface {
    ShareName() string
    FileHandle() []byte
    ContentID() string
    Execute(ctx context.Context, manager *TransferManager) error
    Priority() int
}
```

### TransferQueue
Background worker pool for async uploads.

```go
type TransferQueue struct {
    manager *TransferManager
    queue   chan TransferQueueEntry
    workers int
}
```

## Key Design Decisions

### Non-blocking COMMIT
- NFS COMMIT returns immediately after enqueueing background upload
- Data is safe in mmap cache (crash-safe via OS page cache)
- Block store uploads happen asynchronously
- Achieves 275+ MB/s write throughput (vs ~1 MB/s with blocking)

### TransferQueueEntry Interface
Enables specialized implementations:
- `DefaultEntry`: Standard cache flush
- Future: `S3MultipartEntry` for optimized multipart uploads
- Future: `FSEntry` for filesystem-specific sync modes

### Parallel I/O
- Default: 4 concurrent uploads and 4 concurrent downloads per file
- Bounded by semaphore to prevent resource exhaustion
- Configurable via Config

### Block Key Format
```
{contentID}/chunk-{chunkIdx}/block-{blockIdx}
```

Where `contentID` already includes the share name (e.g., `export/path/to/file`).

Example: `blocks/export/myfile.bin/chunk-0/block-0`

## Configuration

```go
type Config struct {
    ParallelUploads   int  // Default: 4
    ParallelDownloads int  // Default: 4
}

type TransferQueueConfig struct {
    QueueSize int  // Default: 1000
    Workers   int  // Default: 4
}
```

## Methods

### TransferManager
```go
// Enqueue background upload (non-blocking) - called by Flush()
FlushRemainingAsync(ctx, shareName, fileHandle, contentID) error

// Wait for all in-flight uploads (called by FlushAndFinalize)
WaitForUploads(ctx, contentID) error

// Upload remaining partial blocks (blocking - called on CLOSE)
FlushRemaining(ctx, shareName, fileHandle, contentID) error

// Fetch blocks from block store in parallel (cache miss)
ReadBlocks(ctx, shareName, fileHandle, contentID, chunkIdx, offset, length) ([]byte, error)

// Optional: Called after each write - checks if 4MB blocks are ready
OnWriteComplete(ctx, shareName, fileHandle, contentID, chunkIdx, offset, length)

// Recovery on startup
RecoverUnflushedSlices(ctx) (*RecoveryStats, error)

// Lifecycle
Start(ctx)
Close() error
HealthCheck(ctx) error
```

### TransferQueue
```go
Start(ctx)
Stop(timeout)
Enqueue(entry TransferQueueEntry) bool
Pending() int
Stats() (pending, completed, failed int)
```

## Common Mistakes

1. **Duplicating share name in block key** - contentID already includes the share name
2. **Not handling cache-only mode** - When block store is nil, manager is nil too
3. **Blocking on upload** - Use FlushRemainingAsync for COMMIT, only FlushRemaining for CLOSE
4. **Ignoring FlushRemaining** - Partial blocks (< 4MB) need explicit flush on CLOSE
5. **Not calling Start()** - Background queue won't process entries without Start()

## Migration from pkg/flusher

| Old (pkg/flusher) | New (pkg/transfer) |
|-------------------|-------------------|
| `Flusher` | `TransferManager` |
| `BackgroundUploader` | `TransferQueue` |
| `BackgroundUploaderConfig` | `TransferQueueConfig` |
| `uploadRequest` | `TransferQueueEntry` (interface) |
| `flusher.New()` | `transfer.New()` |
| `flusher.Config` | `transfer.Config` |
