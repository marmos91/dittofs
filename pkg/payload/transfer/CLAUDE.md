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
PayloadService.WriteAt()
        ↓
  Cache.WriteSlice()
        ↓
  TransferManager.OnWriteComplete() ← checks for complete 4MB blocks
        ↓
  startBlockUpload() ← spawns goroutine for each complete block (non-blocking)
        ↓
  (goroutine waits for downloads, then uploads to block store)

PayloadService.FlushAsync() (NFS COMMIT)
        ↓
  TransferManager.FlushRemainingAsync() ← enqueues remaining partial blocks
        ↓
  TransferQueue workers ← upload to block store asynchronously

PayloadService.Flush() (SMB CLOSE)
        ↓
  TransferManager.WaitForUploads() ← wait for in-flight eager uploads
        ↓
  TransferManager.FlushRemaining() ← upload remaining partial blocks (blocking)

PayloadService.ReadAt() (cache miss)
        ↓
  TransferManager.ReadSlice() ← parallel fetch from block store
        ↓                        (pauses upload goroutines via ioCond)
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

    // Per-file upload tracking (for eager upload deduplication)
    uploads   map[string]*fileUploadState
    uploadsMu sync.Mutex

    // Download priority coordination
    ioCond           *sync.Cond  // Condition variable
    downloadsPending int         // Protected by ioCond.L
}
```

### TransferRequest
Simple data struct for transfer operations. The queue calls the manager directly.

```go
type TransferRequest struct {
    ShareName  string
    FileHandle string
    PayloadID  string
    Priority   int
}
```

### TransferQueue
Background worker pool for async uploads.

```go
type TransferQueue struct {
    manager *TransferManager
    queue   chan TransferRequest
    workers int
}
```

## Key Design Decisions

### Non-blocking COMMIT
- NFS COMMIT returns immediately after enqueueing background upload
- Data is safe in mmap cache (crash-safe via OS page cache)
- Block store uploads happen asynchronously
- Achieves 275+ MB/s write throughput (vs ~1 MB/s with blocking)

### Eager Upload
Complete 4MB blocks are uploaded immediately after writes:
- `OnWriteComplete()` is called after each slice write
- Checks if any blocks are fully covered by cached data
- Spawns background goroutine to upload each complete block
- Uses `sync.Pool` for 4MB buffers to reduce GC pressure
- Non-blocking to the write path (goroutine does the actual I/O)

### Download Priority
Downloads (cache misses) have priority over uploads:
- Uses `sync.Cond` for efficient waiting without polling
- When `ReadSlice()` runs, it increments `downloadsPending`
- Upload goroutines call `waitForDownloads()` before doing I/O
- When downloads complete, `Broadcast()` wakes waiting upload goroutines
- Result: reads are never blocked by background uploads

### Simple Request Struct
TransferRequest is a simple data struct - no interface abstraction needed:
- Queue calls `manager.flushRemainingSyncInternal()` directly
- Specialized upload logic (S3 multipart, etc.) lives in the block store implementations

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

// Fetch slice from block store in parallel (cache miss)
// Writes directly into dest buffer (zero-copy)
ReadSlice(ctx, shareName, fileHandle, contentID, chunkIdx, offset, length, dest) error

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
Enqueue(req TransferRequest) bool
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
| `uploadRequest` | `TransferRequest` |
| `flusher.New()` | `transfer.New()` |
| `flusher.Config` | `transfer.Config` |
