# pkg/flusher

Cache-to-block-store flush layer - handles eager block upload and parallel download.

## Architecture

```
ContentService.WriteAt()
        ↓
  Cache.WriteSlice()
        ↓
  (writes accumulate in cache)

ContentService.Flush() (NFS COMMIT / SMB FLUSH)
        ↓
  Flusher.FlushRemainingAsync() ← enqueues background upload (non-blocking)
        ↓
  BackgroundUploader workers ← upload to block store asynchronously

ContentService.FlushAndFinalize() (SMB CLOSE)
        ↓
  Flusher.WaitForUploads() ← wait for in-flight uploads
        ↓
  Flusher.FlushRemaining() ← upload partial blocks (blocking)

ContentService.ReadAt() (cache miss)
        ↓
  Flusher.ReadBlocks() ← parallel fetch from block store
        ↓
  Blocks cached for future reads
```

## Key Design Decisions

### Non-blocking COMMIT
- NFS COMMIT returns immediately after enqueueing background upload
- Data is safe in mmap cache (crash-safe via OS page cache)
- Block store uploads happen asynchronously
- Achieves 275+ MB/s write throughput (vs ~1 MB/s with blocking)

### Background Uploader
- Worker pool (default: 4 workers) processes upload queue
- Bounded queue (1000 items) prevents memory exhaustion
- Falls back to sync upload if queue is full
- Graceful shutdown drains queue before stopping

### Eager Block Upload (Optional)
- `OnWriteComplete()` can upload 4MB blocks as they become ready
- Currently disabled during writes to avoid lock contention
- Can be re-enabled for specific workloads

### Parallel I/O
- Default: 4 concurrent uploads and 4 concurrent downloads per file
- Bounded by semaphore to prevent resource exhaustion
- Configurable via `flusher.parallel_uploads` and `flusher.parallel_downloads`

### Block Key Format
```
{keyPrefix}{contentID}/chunk-{chunkIdx}/block-{blockIdx}
```

Where `contentID` already includes the share name (e.g., `export/path/to/file`).

Example: `blocks/export/myfile.bin/chunk-0/block-0`

- Share name embedded in contentID enables multi-tenant support
- Clear data isolation per share
- Easy lifecycle rules per share prefix

## Key Types

### Config
```go
type Config struct {
    ParallelUploads   int  // Default: 4
    ParallelDownloads int  // Default: 4
}
```

### Methods
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
```

## Configuration

```yaml
block_store:
  type: s3  # or: memory, fs
  s3:
    bucket: dittofs-data
    region: us-east-1
    key_prefix: "blocks/"
    force_path_style: false  # true for Localstack/MinIO

flusher:
  parallel_uploads: 4
  parallel_downloads: 4
```

## Common Mistakes

1. **Duplicating share name in block key** - contentID already includes the share name, don't prepend it again
2. **Not handling cache-only mode** - When block store is nil, flusher is nil too
3. **Blocking on upload** - Use FlushRemainingAsync for COMMIT, only FlushRemaining for CLOSE
4. **Ignoring FlushRemaining** - Partial blocks (< 4MB) need explicit flush on CLOSE
5. **Calling cache.Sync() on COMMIT** - Too slow, use mmap's inherent crash safety instead
