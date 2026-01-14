# pkg/payload/store/memory

In-memory block store implementation for testing and ephemeral data.

## Overview

Stores blocks in a thread-safe `sync.Map`. Data is copied on read/write to ensure
isolation. Ideal for unit tests and scenarios where persistence is not required.

## Performance

| Operation | Throughput | Notes |
|-----------|------------|-------|
| Write 4MB | 38,401 MB/s | Single allocation (data copy) |
| Read 4MB | 36,813 MB/s | Single allocation (data copy) |
| Parallel write | 18,464 MB/s | Scales well with goroutines |

See [../BENCHMARKS.md](../BENCHMARKS.md) for detailed benchmarks.

## Key Features

- **Thread-Safe**: Uses `sync.Map` for concurrent access
- **Data Isolation**: Copies on read/write prevent aliasing bugs
- **Zero Configuration**: No setup required
- **Instant Operations**: No I/O latency

## Usage

```go
store := memory.New()
defer store.Close()

// Operations
err := store.WriteBlock(ctx, "share/content/chunk-0/block-0", data)
data, err := store.ReadBlock(ctx, "share/content/chunk-0/block-0")
data, err := store.ReadBlockRange(ctx, "share/content/chunk-0/block-0", offset, length)
err = store.DeleteBlock(ctx, "share/content/chunk-0/block-0")
err = store.DeleteByPrefix(ctx, "share/content")
keys, err := store.ListByPrefix(ctx, "share")

// Stats
count := store.BlockCount()
size := store.TotalSize()
```

## Data Isolation

The store copies data on both read and write:

```go
// Write copies input data
store.WriteBlock(ctx, key, data)
data[0] = 'X'  // Does NOT affect stored data

// Read returns a copy
read, _ := store.ReadBlock(ctx, key)
read[0] = 'Y'  // Does NOT affect stored data
```

This prevents subtle bugs where callers accidentally modify stored data.

## Error Handling

| Error | When |
|-------|------|
| `store.ErrBlockNotFound` | Block doesn't exist |
| `store.ErrStoreClosed` | Operation after Close() |

## Testing

```bash
# Run tests (no special flags needed)
go test ./pkg/payload/store/memory/

# Run benchmarks
go test -bench=. -benchmem ./pkg/payload/store/memory/
```

## Common Mistakes

1. **Expecting persistence** - Data is lost when store is closed or process exits
2. **Memory exhaustion** - No eviction policy; store grows unbounded
3. **Sharing data references** - While the store copies, callers might still share references between themselves
