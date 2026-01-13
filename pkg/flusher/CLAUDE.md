# pkg/flusher

Cache-to-S3 flush layer - handles eager block upload and parallel download.

## Architecture

```
ContentService.WriteAt()
        ↓
  Cache.WriteSlice()
        ↓
  Flusher.OnWriteComplete() ← checks if 4MB block is ready
        ↓
  Async upload to S3 (parallel, bounded)

ContentService.Flush() (NFS COMMIT / SMB FLUSH)
        ↓
  Flusher.WaitForUploads() ← wait for in-flight uploads
        ↓
  Flusher.FlushRemaining() ← upload partial blocks

ContentService.ReadAt() (cache miss)
        ↓
  Flusher.ReadBlocks() ← parallel fetch from S3
        ↓
  Blocks cached for future reads
```

## Key Design Decisions

### Eager Block Upload
- Upload 4MB blocks as soon as they're complete (not waiting for COMMIT)
- Maximizes network bandwidth utilization
- COMMIT just waits for in-flight uploads

### Parallel I/O
- Default: 4 concurrent uploads and 4 concurrent downloads per file
- Bounded by semaphore to prevent resource exhaustion
- Configurable via `flusher.parallel_uploads` and `flusher.parallel_downloads`

### S3 Key Format
```
{keyPrefix}{contentID}/chunk-{chunkIdx}/block-{blockIdx}
```

Where `contentID` already includes the share name (e.g., `export/path/to/file`).

Example: `blocks/export/myfile.bin/chunk-0/block-0`

- Share name embedded in contentID enables multi-tenant support
- Clear data isolation per share
- Easy bucket lifecycle rules per share prefix

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
// Called after each write - checks if 4MB blocks are ready
OnWriteComplete(ctx, shareName, fileHandle, contentID, chunkIdx, offset, length)

// Wait for all in-flight uploads (called by Flush)
WaitForUploads(ctx, contentID) error

// Upload remaining partial blocks (called on COMMIT/CLOSE)
FlushRemaining(ctx, shareName, fileHandle, contentID) error

// Fetch blocks from S3 in parallel (cache miss)
ReadBlocks(ctx, shareName, fileHandle, contentID, chunkIdx, offset, length) ([]byte, error)
```

## Configuration

```yaml
block_store:
  type: s3
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

1. **Duplicating share name in S3 key** - contentID already includes the share name, don't prepend it again
2. **Not handling cache-only mode** - When block store is nil, flusher is nil too
3. **Blocking on upload** - Uploads are async, use WaitForUploads() to wait
4. **Ignoring FlushRemaining** - Partial blocks (< 4MB) need explicit flush on COMMIT
