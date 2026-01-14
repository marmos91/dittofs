# pkg/blocks/store

Block store interface for persistent storage layer (S3, filesystem, memory).

## Overview

Block stores persist cache data to durable storage. The 4MB block size balances:
- S3 PUT efficiency (larger objects = better throughput)
- Filesystem I/O efficiency (reasonable file size)
- Memory usage (reasonable chunk size)
- Latency (partial blocks on COMMIT are small)

## Interface

```go
type BlockStore interface {
    WriteBlock(ctx, blockKey string, data []byte) error
    ReadBlock(ctx, blockKey string) ([]byte, error)
    ReadBlockRange(ctx, blockKey string, offset, length int64) ([]byte, error)
    DeleteBlock(ctx, blockKey string) error
    DeleteByPrefix(ctx, prefix string) error
    ListByPrefix(ctx, prefix string) ([]string, error)
    Close() error
    HealthCheck(ctx) error
}
```

## Implementations

### memory
In-memory implementation for testing. Thread-safe with data isolation (copies on read/write).

### s3
S3-backed implementation with:
- AWS SDK v2
- Configurable endpoint (Localstack/MinIO support)
- Path-style addressing option
- Batch delete via DeleteObjects API

### filesystem (fs)
Local filesystem-backed implementation with:
- Atomic writes via temp file + rename
- Configurable directory/file permissions
- Automatic empty directory cleanup on delete
- Range reads via seek (efficient for partial reads)
- No external dependencies

Ideal for:
- Local development and testing
- NAS/SAN storage backends
- Single-server deployments without S3

## Block Key Format

```
{contentID}/chunk-{chunkIdx}/block-{blockIdx}
```

Where `contentID` already includes the share name (e.g., `export/path/to/file`).

Full storage path examples:
- S3: `{bucket}/{key_prefix}{contentID}/chunk-{n}/block-{n}`
- Filesystem: `{base_path}/{contentID}/chunk-{n}/block-{n}`

Example: `blocks/export/myfile.bin/chunk-0/block-0`

## Configuration

### S3 Block Store
```yaml
block_store:
  type: s3
  s3:
    bucket: dittofs-data
    region: us-east-1
    endpoint: ""  # Optional, for S3-compatible services
    key_prefix: "blocks/"
    max_retries: 3
    force_path_style: false  # true for Localstack/MinIO
```

### Filesystem Block Store
```yaml
block_store:
  type: filesystem
  filesystem:
    base_path: /data/dittofs/blocks  # Required: root directory for blocks
    create_dir: true                  # Default: true - create base_path if missing
    dir_mode: 0755                    # Default: 0755
    file_mode: 0644                   # Default: 0644
```

### Memory Block Store (testing)
```yaml
block_store:
  type: memory  # No additional configuration
```

## BlockRef Types

There are multiple `BlockRef` types in the codebase with different purposes:

| Package | Fields | Purpose |
|---------|--------|---------|
| `pkg/blocks/store` | Key, Size | S3-style path reference for block store |
| `pkg/cache` | ID, Size | Cache's record of flushed block |
| `pkg/wal` | ID, Size | WAL entry tracking flushed blocks |

The store's `BlockRef` uses "Key" to emphasize the S3 key path format, while cache/wal use "ID" for the block identifier.

## Migration from pkg/store/block

| Old (pkg/store/block) | New (pkg/blocks/store) |
|-----------------------|------------------------|
| `block.Store` | `store.BlockStore` |
| `block.ErrBlockNotFound` | `store.ErrBlockNotFound` |
| `block.ErrStoreClosed` | `store.ErrStoreClosed` |
| `block.BlockSize` | `store.BlockSize` |
| `block.BlockRef` | `store.BlockRef` |

## Common Mistakes

1. **Not handling ErrBlockNotFound** - ReadBlock returns this for missing blocks
2. **Missing endpoint for Localstack** - Set endpoint: "http://localhost:4566"
3. **Missing force_path_style** - Localstack/MinIO require path-style addressing
