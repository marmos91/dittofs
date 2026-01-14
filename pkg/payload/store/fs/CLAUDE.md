# pkg/payload/store/fs

Filesystem-backed block store implementation.

## Overview

Stores blocks as files on the local filesystem with the block key as the path.
Ideal for local development, NAS/SAN storage, or single-server deployments.

## Performance

| Operation | Throughput | Notes |
|-----------|------------|-------|
| Write 4MB | 4,173 MB/s | Atomic write (temp + rename) |
| Read 4MB | 10,367 MB/s | Approaches memory bandwidth |
| Range read | Same as full | Seek is O(1) |

See [../BENCHMARKS.md](../BENCHMARKS.md) for detailed benchmarks.

## Key Features

- **Atomic Writes**: Uses temp file + rename pattern for crash safety
- **Empty Directory Cleanup**: Automatically removes empty parent directories on delete
- **Range Reads**: Efficient partial reads via file seek
- **Configurable Permissions**: Custom directory/file modes

## Usage

```go
// With default config (creates directory, 0755/0644 modes)
store, err := fs.NewWithPath("/data/blocks")

// With custom config
store, err := fs.New(fs.Config{
    BasePath:  "/data/blocks",
    CreateDir: true,
    DirMode:   0700,
    FileMode:  0600,
})

// Operations
err = store.WriteBlock(ctx, "share/content/chunk-0/block-0", data)
data, err := store.ReadBlock(ctx, "share/content/chunk-0/block-0")
data, err := store.ReadBlockRange(ctx, "share/content/chunk-0/block-0", offset, length)
err = store.DeleteBlock(ctx, "share/content/chunk-0/block-0")
err = store.DeleteByPrefix(ctx, "share/content")
keys, err := store.ListByPrefix(ctx, "share")
```

## File Layout

Block keys map directly to filesystem paths:

```
BasePath: /data/blocks
Block key: share1/content123/chunk-0/block-0

File path: /data/blocks/share1/content123/chunk-0/block-0
```

Directory structure:
```
/data/blocks/
  share1/
    content123/
      chunk-0/
        block-0
        block-1
      chunk-1/
        block-0
  share2/
    ...
```

## Thread Safety

All operations are protected by a RWMutex:
- Read operations (ReadBlock, ReadBlockRange, ListByPrefix, HealthCheck): shared lock
- Write operations (WriteBlock, DeleteBlock, DeleteByPrefix): exclusive lock

## Error Handling

| Error | When |
|-------|------|
| `store.ErrBlockNotFound` | Block file doesn't exist |
| `store.ErrStoreClosed` | Operation after Close() |
| `os.PathError` | Filesystem permission/IO errors |

## Write Atomicity

Writes use the temp file + rename pattern:
1. Write data to `{path}.tmp`
2. Rename `{path}.tmp` to `{path}` (atomic on most filesystems)
3. On failure, clean up temp file

This ensures readers never see partial writes.

## Configuration

```yaml
block_store:
  type: filesystem
  filesystem:
    base_path: /data/dittofs/blocks  # Required
    create_dir: true                  # Default: true
    dir_mode: 0755                    # Default: 0755
    file_mode: 0644                   # Default: 0644
```

## Testing

```bash
# Run tests (requires integration tag)
go test -tags=integration ./pkg/payload/store/fs/

# Run benchmarks
go test -tags=integration -bench=. -benchmem ./pkg/payload/store/fs/
```

## Common Mistakes

1. **Empty base_path** - Required, validation will fail
2. **Permission denied** - Ensure process has write access to base_path
3. **NFS-mounted base_path** - Works but atomic rename may not be guaranteed on all NFS implementations
4. **Forgetting -tags=integration** - Tests won't run without it
