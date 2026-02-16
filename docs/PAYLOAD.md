# DittoFS Payload Module

This document describes the payload storage system - the core data management layer that handles file content through the Chunk/Slice/Block model with WAL persistence and S3/filesystem backends.

## Table of Contents

- [Overview](#overview)
- [Module Architecture](#module-architecture)
- [PayloadService](#payloadservice)
- [TransferManager](#transfermanager)
- [TransferQueue](#transferqueue)
- [Block Store Implementations](#block-store-implementations)
- [Crash Recovery](#crash-recovery)
- [Garbage Collection](#garbage-collection)
- [Configuration](#configuration)
- [Performance Tuning](#performance-tuning)

## Overview

The payload module (`pkg/payload/`) is responsible for all file content operations:

- **Read/Write**: Through cache with WAL persistence
- **Transfer**: Background upload/download to block stores (S3, filesystem)
- **Recovery**: Crash recovery from WAL on startup
- **GC**: Garbage collection of orphan blocks

### Package Structure

```
pkg/payload/
├── service.go           # PayloadService - main entry point
├── types.go             # Common types (PayloadID, etc.)
├── errors.go            # Error definitions
├── chunk/
│   └── chunk.go         # 64MB chunk calculations
├── block/
│   └── block.go         # 4MB block calculations
├── store/               # Block store implementations
│   ├── store.go         # BlockStore interface
│   ├── memory/          # In-memory (testing)
│   ├── fs/              # Filesystem
│   └── s3/              # AWS S3
└── transfer/            # Transfer orchestration
    ├── manager.go       # TransferManager
    ├── queue.go         # TransferQueue (priority workers)
    ├── recovery.go      # WAL crash recovery
    ├── gc.go            # Block garbage collection
    └── types.go         # TransferRequest, etc.
```

## Module Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                       NFS/SMB Protocol Handlers                     │
│                    (READ, WRITE, COMMIT, CLOSE)                     │
└────────────────────────────────┬────────────────────────────────────┘
                                 │
                                 ▼
┌─────────────────────────────────────────────────────────────────────┐
│                         PayloadService                              │
│                        pkg/payload/service.go                        │
│                                                                     │
│   ReadAt() → WriteAt() → Flush() → Delete() → GetContentSize()     │
└──────────────┬──────────────────────────────────┬───────────────────┘
               │                                  │
               ▼                                  ▼
┌──────────────────────────┐      ┌───────────────────────────────────┐
│         Cache            │      │        TransferManager            │
│       pkg/cache/         │      │   pkg/payload/transfer/manager.go │
│                          │      │                                   │
│  WriteSlice()            │      │  OnWriteComplete() ← Eager upload │
│  ReadSlice()             │◄────►│  EnsureAvailable() ← Downloads    │
│  GetDirtySlices()        │      │  Flush()          ← Non-blocking  │
│                          │      │  RecoverUnflushedSlices()         │
│  ┌────────────────────┐  │      │                                   │
│  │   WAL Persister    │  │      │  ┌────────────────────────────┐   │
│  │  pkg/cache/wal/    │  │      │  │     TransferQueue          │   │
│  └────────────────────┘  │      │  │   (Priority workers)       │   │
└──────────────────────────┘      │  └─────────────┬──────────────┘   │
                                  └────────────────┼──────────────────┘
                                                   │
                                                   ▼
                                  ┌───────────────────────────────────┐
                                  │           BlockStore              │
                                  │     pkg/payload/store/store.go    │
                                  │                                   │
                                  │  ┌─────────┐ ┌─────────┐ ┌─────┐  │
                                  │  │ Memory  │ │   FS    │ │ S3  │  │
                                  │  └─────────┘ └─────────┘ └─────┘  │
                                  └───────────────────────────────────┘
```

## PayloadService

The main entry point for all content operations.

### Creating PayloadService

```go
// Create with cache (required)
cache := cache.New(maxSize, cache.WithPersister(walPersister))

// Create TransferManager (optional, enables block store persistence)
transferMgr := transfer.New(cache, blockStore, transfer.Config{
    ParallelUploads:   16,
    ParallelDownloads: 8,
})

// Create PayloadService
payloadSvc := payload.New()
payloadSvc.SetCache(cache)
payloadSvc.SetTransferManager(transferMgr)
```

### Key Methods

```go
// Read from cache (downloads from block store on miss)
ReadAt(ctx context.Context, payloadID string, buf []byte, offset int64) (int, error)

// Write to cache (triggers eager upload for complete blocks)
WriteAt(ctx context.Context, payloadID string, data []byte, offset int64) error

// Non-blocking flush (NFS COMMIT)
Flush(ctx context.Context, payloadID string) (*FlushResult, error)

// Blocking flush (SMB CLOSE)
FlushAndFinalize(ctx context.Context, payloadID string) (*FlushResult, error)

// Size and existence
GetContentSize(ctx context.Context, payloadID string) (uint64, error)
ContentExists(ctx context.Context, payloadID string) (bool, error)

// Lifecycle
Truncate(ctx context.Context, payloadID string, newSize int64) error
Delete(ctx context.Context, payloadID string) error
```

### PayloadID

The `payloadID` is the sole identifier for file content:

```
Format: {shareName}/{path/to/file}
Example: export/documents/report.pdf
```

- Used as cache key
- Used for block store paths
- Includes share name for multi-share isolation

## TransferManager

Orchestrates data movement between cache and block store.

### Key Features

1. **Eager Upload**: Uploads complete 4MB blocks immediately (don't wait for COMMIT)
2. **Download with Prefetch**: Fetches blocks on cache miss, prefetches upcoming blocks
3. **Priority Scheduling**: Downloads > Uploads > Prefetch
4. **Non-blocking Flush**: Returns immediately, data safe in WAL
5. **In-flight Deduplication**: Prevents duplicate downloads

### Eager Upload Flow

```
PayloadService.WriteAt()
    │
    ▼
Cache.WriteSlice()  ← Data written to cache
    │
    ▼
TransferManager.OnWriteComplete()
    │
    ├── Calculate which 4MB blocks overlap the write
    │
    ├── For each complete block:
    │   ├── Check if already uploaded (deduplication)
    │   ├── Check if cache covers entire block
    │   └── If covered → Enqueue async block upload
    │
    └── Return immediately (non-blocking)
```

### Download with Prefetch

```go
// Called on cache miss
func (tm *TransferManager) EnsureAvailable(
    ctx context.Context,
    payloadID string,
    chunkIdx uint32,
    offset, length int64,
) error {
    // Calculate required blocks
    startBlock := offset / BlockSize
    endBlock := (offset + length - 1) / BlockSize

    // Download required blocks (wait)
    for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
        tm.downloadBlock(ctx, payloadID, chunkIdx, blockIdx)
    }

    // Enqueue prefetch for next N blocks (async)
    for i := 1; i <= tm.config.PrefetchBlocks; i++ {
        tm.queue.EnqueuePrefetch(NewPrefetchRequest(payloadID, chunkIdx, endBlock+i))
    }
}
```

### Non-Blocking Flush

```go
// NFS COMMIT - returns immediately
func (tm *TransferManager) Flush(ctx context.Context, payloadID string) (*FlushResult, error) {
    // Get remaining dirty data
    dirtySlices := tm.cache.GetDirtySlices(ctx, payloadID)

    // Enqueue for background upload (non-blocking)
    for _, slice := range dirtySlices {
        tm.queue.EnqueueUpload(NewBlockUploadRequest(payloadID, slice.ChunkIdx, slice.BlockIdx))
    }

    // Return immediately - data is safe in WAL-backed cache
    return &FlushResult{
        AlreadyFlushed: len(dirtySlices) == 0,
        Finalized:      false,  // Background upload in progress
    }, nil
}
```

### Configuration

```go
type Config struct {
    ParallelUploads    int  // Concurrent uploads (default: 16)
    ParallelDownloads  int  // Concurrent downloads (default: 8)
    MaxParallelUploads int  // Hard limit on concurrent uploads
    PrefetchBlocks     int  // Blocks to prefetch ahead (default: 4)
}
```

## TransferQueue

Background worker pool with priority scheduling.

### Priority Channels

```go
type TransferQueue struct {
    downloads chan TransferRequest  // Priority 0 (highest)
    uploads   chan TransferRequest  // Priority 1
    prefetch  chan TransferRequest  // Priority 2 (lowest)

    workers int
    manager *TransferManager
}
```

### Worker Loop

```go
func (q *TransferQueue) worker(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case req := <-q.downloads:  // Check downloads first
            q.process(req)
        default:
            select {
            case req := <-q.downloads:
                q.process(req)
            case req := <-q.uploads:  // Then uploads
                q.process(req)
            default:
                select {
                case req := <-q.downloads:
                    q.process(req)
                case req := <-q.uploads:
                    q.process(req)
                case req := <-q.prefetch:  // Finally prefetch
                    q.process(req)
                case <-ctx.Done():
                    return
                }
            }
        }
    }
}
```

### TransferRequest

```go
type TransferRequest struct {
    Type      TransferType  // Download, Upload, or Prefetch
    PayloadID string
    ChunkIdx  uint32
    BlockIdx  uint32
    Done      chan error    // nil for async operations
}

// Constructors
NewDownloadRequest(payloadID string, chunkIdx, blockIdx uint32, done chan error)
NewPrefetchRequest(payloadID string, chunkIdx, blockIdx uint32)  // Done is always nil
NewBlockUploadRequest(payloadID string, chunkIdx, blockIdx uint32)
```

## Block Store Implementations

### BlockStore Interface

```go
type BlockStore interface {
    // Write a block
    PutBlock(ctx context.Context, key string, data []byte) error

    // Read a block
    GetBlock(ctx context.Context, key string) ([]byte, error)

    // Delete a block
    DeleteBlock(ctx context.Context, key string) error

    // Delete all blocks with prefix
    DeleteByPrefix(ctx context.Context, prefix string) error

    // List blocks with prefix
    ListByPrefix(ctx context.Context, prefix string) ([]string, error)

    // Check if block exists
    Exists(ctx context.Context, key string) (bool, error)

    // Get block size
    Size(ctx context.Context, key string) (int64, error)
}
```

### Block Key Format

```
{payloadID}/chunk-{chunkIdx}/block-{blockIdx}

Example: export/documents/report.pdf/chunk-0/block-5
```

### S3 Store

Production-ready S3 implementation with:

- Range reads for partial block fetches
- Multipart uploads for large blocks
- Configurable retry with exponential backoff
- Connection pooling

```bash
./dfsctl store payload add --name s3-production --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'
```

### Filesystem Store

Local filesystem storage:

```bash
./dfsctl store payload add --name local --type filesystem \
  --config '{"path":"/var/lib/dfs/blocks"}'
```

### Memory Store

In-memory for testing:

```bash
./dfsctl store payload add --name test --type memory
```

## Crash Recovery

### Recovery Flow

```
Server Startup
    │
    ▼
WAL Persister Recover()
    │
    ├── Load all slice entries from WAL file
    │
    ▼
Cache.RestoreSlices()
    │
    ├── Restore slices to cache (in-memory)
    │
    ▼
TransferManager.RecoverUnflushedSlices()
    │
    ├── Scan cache for dirty files
    ├── Calculate actual recovered sizes
    ├── Start background uploads for each file
    │
    └── Return RecoveryStats (for metadata reconciliation)
    │
    ▼
ReconcileMetadata()
    │
    ├── Compare recovered sizes with metadata
    ├── Truncate metadata where size > recovered
    │
    └── Log reconciliation stats
```

### RecoveryStats

```go
type RecoveryStats struct {
    FilesScanned       int              // Files found in cache
    SlicesFound        int              // Total slices recovered
    BytesPending       int64            // Bytes needing upload
    RecoveredFileSizes map[string]uint64 // payloadID → actual size
}
```

### Metadata Reconciliation

WAL logs new slices but not slice extensions (for performance). If a crash occurs after:
1. Data written to cache (extending existing slice)
2. Metadata updated with new size (CommitWrite)
3. BUT before WAL persistence of extended data

Then metadata will show larger size than actual recovered data. The `ReconcileMetadata` function fixes this:

```go
func ReconcileMetadata(
    ctx context.Context,
    reconciler MetadataReconciler,
    recoveredSizes map[string]uint64,
) *ReconciliationStats {
    for payloadID, recoveredSize := range recoveredSizes {
        file := getMetadataFile(payloadID)
        if file.Size > recoveredSize {
            file.Size = recoveredSize
            saveFile(file)
        }
    }
}
```

## Garbage Collection

Cleans up orphan blocks in the block store.

### When Orphans Occur

1. File deletion crashes after metadata removed
2. Partial deletion due to server crash
3. Failed upload cleanup

### GC Algorithm

```go
func CollectGarbage(ctx context.Context, blockStore BlockStore, reconciler MetadataReconciler) *GCStats {
    // List all blocks
    blocks := blockStore.ListByPrefix(ctx, "")

    // Extract unique payloadIDs
    payloadIDs := extractPayloadIDs(blocks)

    // Check each payloadID against metadata
    for _, payloadID := range payloadIDs {
        if !metadataExists(payloadID) {
            // Orphan - delete all blocks for this file
            blockStore.DeleteByPrefix(ctx, payloadID)
        }
    }
}
```

### GCStats

```go
type GCStats struct {
    SharesScanned  int    // Shares processed
    BlocksScanned  int    // Total blocks examined
    OrphanFiles    int    // Files with orphan blocks
    OrphanBlocks   int    // Total orphan blocks deleted
    BytesReclaimed int64  // Estimated bytes freed
    Errors         int    // Non-fatal errors
}
```

### GCOptions

```go
type GCOptions struct {
    SharePrefix        string  // Limit to specific share
    DryRun            bool    // Report only, don't delete
    MaxOrphansPerShare int     // Stop after N orphans
    ProgressCallback   func(stats GCStats)
}
```

## Configuration

### Complete Configuration Example

Server config file (cache settings):

```yaml
cache:
  path: /var/lib/dfs/cache
  size: "1Gi"
```

Then create stores and shares via CLI:

```bash
# Create payload store
./dfsctl store payload add --name s3-store --type s3 \
  --config '{"region":"us-east-1","bucket":"dfs-production"}'

# Create metadata store
./dfsctl store metadata add --name badger --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'

# Create share referencing stores
./dfsctl share create --name /export --metadata badger --payload s3-store
```

## Performance Tuning

### Upload Parallelism

| Workload | `parallel_uploads` | Notes |
|----------|-------------------|-------|
| Light (< 10 clients) | 4-8 | Default is sufficient |
| Medium (10-50 clients) | 16 | Good balance |
| Heavy (50+ clients) | 32-64 | May need S3 throttling config |

### Download Parallelism

| Workload | `parallel_downloads` | Notes |
|----------|---------------------|-------|
| Sequential reads | 4-8 | Prefetch handles ahead |
| Random reads | 16-32 | More parallel downloads |
| Large files | 8-16 | Fewer, larger requests |

### Prefetch Depth

| Access Pattern | `prefetch_blocks` | Notes |
|----------------|------------------|-------|
| Sequential read | 4-8 | 16-32MB ahead |
| Mixed | 2-4 | Lower prefetch |
| Random | 0-1 | Disable or minimal |

### Memory Considerations

- **Block buffer pool**: ~4MB × `parallel_uploads` for upload buffers
- **Cache size**: Configure based on working set size
- **WAL size**: Auto-grows, initial 64MB typical

### S3 Optimization

- **Block size (4MB)**: Good balance of parallelism vs API calls
- **Retry backoff**: Start at 100ms, max 2s for transient errors
- **Connection pooling**: SDK handles automatically
