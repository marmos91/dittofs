# pkg/transfer

Cache-to-block-store transfer layer - handles uploads, downloads, prefetch, and content-addressed deduplication.

## Overview

The transfer package manages data movement between the cache and block store:
- **Eager upload**: Upload 4MB blocks as soon as they're ready (don't wait for COMMIT)
- **Content-addressed deduplication**: Skip uploads for blocks that already exist (by SHA-256 hash)
- **Download with prefetch**: Fetch blocks from block store on cache miss, prefetch upcoming blocks
- **Priority scheduling**: Downloads > Uploads > Prefetch
- **Non-blocking flush**: Flush returns immediately - data is safe in WAL mmap cache
- **Small file optimization**: Files below threshold are flushed synchronously to free buffers
- **In-flight deduplication**: Avoid duplicate downloads for the same block

## Architecture

```
PayloadService.WriteAt()
        ↓
  Cache.Write()
        ↓
  TransferManager.OnWriteComplete() ← checks for complete 4MB blocks
        ↓
  queue.EnqueueUpload() ← workers pick it up (non-blocking)

PayloadService.ReadAt() (cache miss)
        ↓
  TransferManager.EnsureAvailable() ← enqueues downloads + prefetch
        ↓                              waits for downloads, prefetch is async
  Cache now has data
        ↓
  PayloadService reads from cache

PayloadService.Flush() (NFS COMMIT / SMB CLOSE)
        ↓
  TransferManager.Flush() ← enqueues remaining blocks for background upload
        ↓                    returns immediately (data safe in WAL cache)
  Returns immediately
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

    // In-flight download tracking (deduplication)
    inFlight   map[string]chan error  // blockKey -> completion channel
    inFlightMu sync.Mutex
}
```

### TransferRequest
Simple data struct for transfer operations. All operations specify block coordinates.

```go
type TransferRequest struct {
    Type      TransferType  // Download, Upload, or Prefetch
    PayloadID string        // Sole identifier for file content
    ChunkIdx  uint32
    BlockIdx  uint32
    Done      chan error    // nil for async operations
}
```

### Request Constructors
```go
// Download: synchronous, caller waits on Done channel
NewDownloadRequest(payloadID string, chunkIdx, blockIdx uint32, done chan error)

// Prefetch: async, best-effort, Done is always nil
NewPrefetchRequest(payloadID string, chunkIdx, blockIdx uint32)

// Upload: async, eager upload of complete blocks
NewBlockUploadRequest(payloadID string, chunkIdx, blockIdx uint32)
```

### TransferQueue
Background worker pool with priority scheduling.

```go
type TransferQueue struct {
    manager *TransferManager

    // Priority channels - workers check in order
    downloads chan TransferRequest  // Highest priority
    uploads   chan TransferRequest  // Medium priority
    prefetch  chan TransferRequest  // Lowest priority

    workers int
}
```

### TransferType
Priority levels for transfer operations.

```go
const (
    TransferDownload TransferType = iota  // Priority 0 (highest)
    TransferUpload                         // Priority 1
    TransferPrefetch                       // Priority 2 (lowest)
)
```

### FlushResult
Result of flush operations.

```go
type FlushResult struct {
    BytesFlushed   uint64
    AlreadyFlushed bool  // true if no pending data
    Finalized      bool  // true if data is durable
}
```

## Key Design Decisions

### Unified Priority Queue
Single worker pool handles ALL transfers with priority scheduling:
- Workers check channels in order: downloads → uploads → prefetch
- Downloads always processed first (user is waiting)
- Prefetch is best-effort, dropped if queue full

### In-Flight Deduplication
Prevents duplicate downloads when multiple requests need the same block:
- `inFlight` map tracks active downloads: blockKey → completion channel
- Multiple waiters can wait on the same channel
- Cleanup happens when download completes

### Prefetch on Cache Miss
When `EnsureAvailable` downloads a block, it also enqueues prefetch for N+1, N+2, etc:
- Downloads and prefetch are enqueued IN PARALLEL (same call)
- Caller waits only on downloads, prefetch runs async
- Improves sequential read performance significantly

### Non-Blocking Flush (FlushAsync/Flush)
Both `FlushAsync` and `Flush` return immediately without waiting for S3:
- Data durability is provided by the **WAL-backed mmap cache** (OS syncs to disk)
- The main performance win comes from **eager upload** of complete 4MB blocks
- Remaining partial blocks are enqueued for background upload
- NFS COMMIT semantics only require data to be on stable storage - mmap provides this
- This achieves maximum throughput by decoupling NFS operations from S3 latency

### PayloadID as Sole Identifier
The `payloadID` is the sole identifier for file content:
- Used directly as cache key
- Used for block store paths
- Removed redundant `fileHandle` and `ShareName` fields
- Simplifies API and eliminates duplicate parameters

## Configuration

```go
type Config struct {
    ParallelUploads    int    // Default: 16 (concurrent block uploads)
    MaxParallelUploads int    // Default: 0 (unlimited, auto-tuned by congestion control)
    ParallelDownloads  int    // Default: 4 (concurrent downloads per file)
    PrefetchBlocks     int    // Default: 4 (16MB prefetch ahead)
    SmallFileThreshold int64  // Default: 0 (disabled). Files smaller than this
                              // are flushed synchronously to free buffers immediately
}

type TransferQueueConfig struct {
    QueueSize int  // Default: 1000 (per channel)
    Workers   int  // Default: 4
}
```

### Small File Optimization
When `SmallFileThreshold > 0`, files smaller than the threshold are flushed synchronously
during `Flush()`. This prevents pendingSize buildup when creating many small files by
immediately freeing the 4MB block buffer after upload completes.

### Content-Addressed Deduplication
TransferManager requires an `ObjectStore` for content-addressed deduplication:
- On block completion, SHA-256 hash is computed
- `ObjectStore.FindBlockByHash()` checks if block already exists
- If exists and uploaded: increment RefCount, skip upload
- If new: register block in ObjectStore, proceed with upload

```go
// Constructor requires ObjectStore
New(cache *cache.Cache, blockStore store.BlockStore, objectStore metadata.ObjectStore, config Config)
```

## Methods

### TransferManager

#### Public API (for PayloadService)
```go
// Download blocks and prefetch (called on cache miss)
EnsureAvailable(ctx, payloadID, chunkIdx, offset, length) error

// Flush - called by NFS COMMIT and SMB CLOSE (non-blocking, enqueues remaining blocks)
Flush(ctx, payloadID) (*FlushResult, error)

// Called after each write - checks if 4MB blocks are ready for eager upload
OnWriteComplete(ctx, payloadID, chunkIdx, offset, length)

// Block store queries
GetFileSize(ctx, payloadID) (uint64, error)
Exists(ctx, payloadID) (bool, error)
Truncate(ctx, payloadID, newSize) error
Delete(ctx, payloadID) error

// Lifecycle
Start(ctx)
Close() error
HealthCheck(ctx) error
```

### TransferQueue
```go
Start(ctx)
Stop(timeout)
Enqueue(req) bool           // Routes to upload channel
EnqueueDownload(req) bool   // Highest priority
EnqueueUpload(req) bool     // Medium priority
EnqueuePrefetch(req) bool   // Lowest priority (best effort)
Pending() int
PendingByType() (download, upload, prefetch int)
Stats() (pending, completed, failed int)
LastError() (time.Time, error)
```

## Common Mistakes

1. **Duplicating share name in block key** - payloadID already includes the share name
2. **Not handling cache-only mode** - When block store is nil, manager is nil too
3. **Not calling Start()** - Background queue won't process entries without Start()
4. **Calling EnsureAvailable for data already in cache** - Check cache first for performance

## Block Key Format
```
{payloadID}/{chunkIdx}/{blockIdx}
```

Where `payloadID` already includes the share name (e.g., `export/path/to/file`).

Example: `export/myfile.bin/2/5`
