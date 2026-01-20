# pkg/payload/store/s3

S3-compatible block store implementation for durable cloud storage.

## Overview

Stores blocks as S3 objects using AWS SDK v2. Supports AWS S3, MinIO, Localstack,
and other S3-compatible services. Production-ready with connection pooling, retries,
and batch operations.

## Performance

| Operation | Throughput | Notes |
|-----------|------------|-------|
| Write 4MB | 165 MB/s | HTTP PUT overhead |
| Read 4MB | 291 MB/s | HTTP GET with streaming |
| Range read | ~20 MB/s | Uses HTTP Range header |
| Parallel write | 30 MB/s | Connection reuse helps |

Performance is network-bound. Use the cache layer for high-throughput workloads.
See [../BENCHMARKS.md](../BENCHMARKS.md) for detailed benchmarks.

## Key Features

- **AWS SDK v2**: Modern, efficient SDK with connection pooling
- **S3-Compatible**: Works with AWS S3, MinIO, Localstack, Ceph, etc.
- **Batch Delete**: Uses DeleteObjects API for efficient prefix deletion
- **Range Reads**: HTTP Range header for efficient partial reads
- **Server-Side Copy**: CopyBlock uses S3 CopyObject API (no data transfer through client)
- **Path-Style Support**: Required for Localstack/MinIO

## HTTP Client Optimizations

The S3 client uses a custom HTTP transport optimized for parallel uploads:

| Optimization | Value | Reason |
|--------------|-------|--------|
| Force HTTP/1.1 | `ForceAttemptHTTP2: false` | HTTP/2 multiplexing adds overhead for large parallel uploads |
| Max connections | 200 per host | High parallelism for concurrent block uploads |
| Write buffer | 256KB | Reduce syscall overhead for large uploads |
| Read buffer | 256KB | Reduce syscall overhead for large downloads |
| ExpectContinue | Disabled | Skip 100-Continue round trip for faster uploads |
| Keep-alive | 30s timeout | Efficient connection reuse |
| Idle connections | 200 max | Pool connections for burst traffic |

These optimizations improve parallel upload throughput by 2-3x compared to default settings.

## Usage

```go
// Create S3 client
cfg, _ := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
client := s3.NewFromConfig(cfg)

// Create store
store := s3store.New(client, s3store.Config{
    Bucket:    "my-bucket",
    KeyPrefix: "blocks/",
})
defer store.Close()

// Operations
err := store.WriteBlock(ctx, "share/content/chunk-0/block-0", data)
data, err := store.ReadBlock(ctx, "share/content/chunk-0/block-0")
data, err := store.ReadBlockRange(ctx, "share/content/chunk-0/block-0", offset, length)
err = store.CopyBlock(ctx, "src/chunk-0/block-0", "dst/chunk-0/block-0")  // Server-side copy
err = store.DeleteBlock(ctx, "share/content/chunk-0/block-0")
err = store.DeleteByPrefix(ctx, "share/content")
keys, err := store.ListByPrefix(ctx, "share")
err = store.HealthCheck(ctx)  // Verifies bucket access
```

## S3 Key Format

Block keys are prefixed with `KeyPrefix`:

```
Bucket: my-bucket
KeyPrefix: blocks/
Block key: share1/content123/chunk-0/block-0

S3 Key: blocks/share1/content123/chunk-0/block-0
```

## Configuration

```yaml
block_store:
  type: s3
  s3:
    bucket: dittofs-data           # Required
    region: us-east-1              # Required for AWS
    endpoint: ""                   # Optional: for S3-compatible services
    key_prefix: "blocks/"          # Optional: prefix for all keys
    force_path_style: false        # Required for Localstack/MinIO
```

### Localstack/MinIO Example

```yaml
block_store:
  type: s3
  s3:
    bucket: test-bucket
    region: us-east-1
    endpoint: "http://localhost:4566"  # Localstack
    force_path_style: true             # Required for path-style URLs
```

## Error Handling

| Error | When |
|-------|------|
| `store.ErrBlockNotFound` | Object doesn't exist (404) |
| `store.ErrStoreClosed` | Operation after Close() |
| AWS SDK errors | Network, auth, permissions |

## Testing

Tests use Localstack via testcontainers (requires Docker):

```bash
# Run tests (requires integration tag + Docker)
go test -tags=integration ./pkg/payload/store/s3/

# Run benchmarks
go test -tags=integration -bench=. -benchmem ./pkg/payload/store/s3/

# Use existing Localstack (faster for repeated runs)
LOCALSTACK_ENDPOINT=http://localhost:4566 go test -tags=integration ./pkg/payload/store/s3/
```

## Common Mistakes

1. **Missing endpoint for Localstack** - Set `endpoint: "http://localhost:4566"`
2. **Missing force_path_style** - Localstack/MinIO require `force_path_style: true`
3. **Forgetting -tags=integration** - Tests won't run without it
4. **No Docker running** - Tests start Localstack container automatically
5. **Expecting low latency** - S3 has ~3ms minimum per request; use cache layer for performance
