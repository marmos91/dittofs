# pkg/store/block

Block store interface for S3 persistence layer.

## Overview

Block stores persist cache data to durable storage (S3). The 4MB block size balances:
- S3 PUT efficiency (larger objects = better throughput)
- Memory usage (reasonable chunk size)
- Latency (partial blocks on COMMIT are small)

## Interface

```go
type Store interface {
    WriteBlock(ctx, blockKey string, data []byte) error
    ReadBlock(ctx, blockKey string) ([]byte, error)
    ReadBlockRange(ctx, blockKey string, offset, length int64) ([]byte, error)
    DeleteBlock(ctx, blockKey string) error
    DeleteByPrefix(ctx, prefix string) error
    ListByPrefix(ctx, prefix string) ([]string, error)
    Close() error
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

## Block Key Format

```
{contentID}/chunk-{chunkIdx}/block-{blockIdx}
```

Where `contentID` already includes the share name (e.g., `export/path/to/file`).

Full S3 path: `{key_prefix}{contentID}/chunk-{n}/block-{n}`
Example: `blocks/export/myfile.bin/chunk-0/block-0`

## Configuration

```yaml
block_store:
  type: s3  # or "memory" for testing
  s3:
    bucket: dittofs-data
    region: us-east-1
    endpoint: ""  # Optional, for S3-compatible services
    key_prefix: "blocks/"
    max_retries: 3
    force_path_style: false  # true for Localstack/MinIO
```

## Common Mistakes

1. **Not handling ErrBlockNotFound** - ReadBlock returns this for missing blocks
2. **Missing endpoint for Localstack** - Set endpoint: "http://localhost:4566"
3. **Missing force_path_style** - Localstack/MinIO require path-style addressing
