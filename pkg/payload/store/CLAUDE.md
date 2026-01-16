# pkg/payload/store

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

| Store | Throughput | Use Case |
|-------|------------|----------|
| memory | ~15-38 GB/s | Unit tests, ephemeral data |
| filesystem | ~2-10 GB/s | Local dev, NAS/SAN, single-server |
| s3 | ~100-300 MB/s | Production, durability required |

See [BENCHMARKS.md](BENCHMARKS.md) for detailed performance comparison.

### memory
In-memory implementation for testing. Thread-safe with data isolation (copies on read/write).
- Single allocation per operation
- No external dependencies
- Data lost on restart

### s3
S3-backed implementation with:
- AWS SDK v2
- Configurable endpoint (Localstack/MinIO support)
- Path-style addressing option
- Batch delete via DeleteObjects API
- HTTP connection pooling

### filesystem (fs)
Local filesystem-backed implementation with:
- Atomic writes via temp file + rename
- Configurable directory/file permissions
- Automatic empty directory cleanup on delete
- Range reads via seek (efficient for partial reads)
- No external dependencies

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

## Testing

```bash
# Memory store (unit tests, no special flags)
go test ./pkg/payload/store/memory/

# Filesystem store (integration tests)
go test -tags=integration ./pkg/payload/store/fs/

# S3 store (integration tests, requires Docker)
go test -tags=integration ./pkg/payload/store/s3/

# Run benchmarks
go test -tags=integration -bench=. -benchmem ./pkg/payload/store/...
```

## Common Mistakes

1. **Not handling ErrBlockNotFound** - ReadBlock returns this for missing blocks
2. **Missing endpoint for Localstack** - Set endpoint: "http://localhost:4566"
3. **Missing force_path_style** - Localstack/MinIO require path-style addressing
4. **Forgetting -tags=integration** - fs and s3 tests won't run without it
