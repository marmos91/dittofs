# Cache & WAL Benchmarks

Performance benchmarks for the cache and WAL modules.

**Test Environment:**
- CPU: Apple M1 Max
- OS: macOS (darwin/arm64)
- Go: 1.23+
- Date: 2025-01-16

## Summary

| Operation | Throughput | Notes |
|-----------|------------|-------|
| Sequential Write (128KB) | **15.8 GB/s** | Zero allocations |
| File Copy (100MB) | **12.4 GB/s** | End-to-end block buffer model |
| Concurrent File Copies | **32.3 GB/s** | Multi-threaded |
| Read (128KB) | **29.9 GB/s** | Zero allocations |
| WAL Append (1MB) | **2.9 GB/s** | mmap-backed, zero allocs |

## Architecture

The cache uses a **Block Buffer Model** where data is written directly to 4MB block buffers:
- Each block has a coverage bitmap tracking which bytes are written
- Writes go directly to the target position (no slice coalescing needed)
- Memory tracking uses BlockSize (4MB) per buffer for accurate OOM prevention
- Immediate eviction possible after block upload

## Write Benchmarks

### Sequential Writes

Sequential writes directly to block buffers achieve excellent throughput:

| Size | Throughput | Allocs/op |
|------|------------|-----------|
| 4KB | 11.0 GB/s | 0 |
| 16KB | 14.1 GB/s | 0 |
| 32KB | 14.3 GB/s | 0 |
| 64KB | 15.3 GB/s | 0 |
| 128KB | 15.8 GB/s | 0 |

### Multi-File Writes

Writing to multiple files concurrently:

| Files | Throughput | Allocs/op |
|-------|------------|-----------|
| 10 | 8.2 GB/s | 1 |
| 100 | 8.2 GB/s | 1 |
| 1000 | 8.2 GB/s | 2 |

Excellent scalability - throughput remains constant as file count increases.

### Concurrent Writes

Multi-threaded write performance (GOMAXPROCS=10):

| Benchmark | Throughput | Notes |
|-----------|------------|-------|
| Concurrent 32KB | 67.9 GB/s | Minimal lock contention |

## Read Benchmarks

### Sequential Reads

| Size | Throughput | Allocs/op |
|------|------------|-----------|
| 4KB | 22.9 GB/s | 0 |
| 32KB | 29.4 GB/s | 0 |
| 64KB | 30.3 GB/s | 0 |
| 128KB | 29.9 GB/s | 0 |

Zero-allocation reads with excellent throughput.

### Overlapping Writes

Performance when reading ranges with overlapping writes (newest-wins):

| Overlapping Writes | Throughput | Allocs/op |
|--------------------|------------|-----------|
| 1 | 57.8 GB/s | 0 |
| 5 | 58.2 GB/s | 0 |
| 10 | 61.7 GB/s | 0 |
| 25 | 58.6 GB/s | 0 |
| 50 | 59.1 GB/s | 0 |

**Optimizations:**
- Direct block buffer access (no merge needed)
- Coverage bitmap for sparse file support
- Zero allocations in hot path

### IsRangeCovered

Checking if a byte range is covered in cache:

| Benchmark | Latency |
|-----------|---------|
| IsRangeCovered | 369 ns |

## Flush Benchmarks

### GetDirtyBlocks

Retrieving dirty blocks for upload:

| Chunks | Latency | Allocs |
|--------|---------|--------|
| 1 | 126 ns | 1 |
| 10 | 1.1 us | 5 |
| 100 | 12.5 us | 8 |

### MarkBlockUploaded

Marking blocks as uploaded after S3 transfer:

| Benchmark | Latency | Allocs |
|-----------|---------|--------|
| MarkBlockUploaded | 78 ns | 1 |

## Eviction Benchmarks

LRU eviction with dirty data protection:

| Operation | Latency | Notes |
|-----------|---------|-------|
| EvictLRU | 146 us | Evicts uploaded blocks only |

## End-to-End Benchmarks

### File Copy Simulation

Simulates complete NFS file copy (sequential writes + flush):

| File Size | Throughput | Total Time |
|-----------|------------|------------|
| 1MB | 6.0 GB/s | 176 us |
| 10MB | 8.7 GB/s | 1.2 ms |
| 100MB | 12.4 GB/s | 8.4 ms |

### Concurrent File Copies

Multiple files being copied simultaneously:

| Benchmark | Throughput |
|-----------|------------|
| Concurrent 1MB files | 32.3 GB/s |

Excellent parallel scaling with multiple goroutines.

### Mixed Read/Write

Write-Read-Write pattern:

| Operation | Throughput |
|-----------|------------|
| Write+Read+Write | 22.6 GB/s |

## WAL Benchmarks

### Append Performance

| Data Size | Throughput | Allocs/op |
|-----------|------------|-----------|
| 512B | 2.5 GB/s | 0 |
| 32KB | 2.8 GB/s | 0 |
| 1MB | 2.9 GB/s | 0 |

Zero-allocation writes via mmap.

### Recovery Performance

| Entries | Latency | Allocs |
|---------|---------|--------|
| 100 | 54 us | 319 |
| 1000 | 276 us | 3022 |
| 500 (with removes) | 161 us | 1550 |

Recovery is fast even with thousands of entries.

### WAL Operations

| Operation | Latency |
|-----------|---------|
| AppendRemove | 80 ns |
| Sync (MS_ASYNC) | 14 ns |

### Sustained Throughput

| Benchmark | Throughput |
|-----------|------------|
| WAL Throughput | 8.5 GB/s |

## WAL vs In-Memory Comparison

Direct comparison of cache performance with and without WAL persistence:

| Benchmark | In-Memory | With WAL | Overhead |
|-----------|-----------|----------|----------|
| Sequential 32KB | 14.7 GB/s | 2.2 GB/s | WAL I/O bound |
| File Copy 10MB | 11.0 GB/s | 1.9 GB/s | WAL I/O bound |

**Note:** WAL overhead is dominated by disk I/O. For in-memory workloads without durability requirements, use `cache.New()` instead of `cache.NewWithWal()`.

## Memory Allocation

| Operation | Allocs/op | Notes |
|-----------|-----------|-------|
| Write (new block) | 10 | Creates file/chunk/block entries |
| Write (existing block) | 0 | Direct buffer write |
| Read | 0 | Zero-allocation read |
| WAL Append | 0 | Direct mmap write |

### Memory Tracking

The cache tracks actual memory allocation (BlockSize = 4MB per block buffer), not bytes written:
- Prevents OOM by providing accurate backpressure
- Returns `ErrCacheFull` when pending blocks exceed `maxSize`
- Only uploaded blocks can be evicted

## Performance Requirements

The cache must achieve **>3 GB/s** sequential write throughput for acceptable NFS file copy performance. Current benchmarks show:

- Sequential writes: **15.8 GB/s** (427% above requirement)
- File copy E2E: **12.4 GB/s** (313% above requirement)
- Concurrent copies: **32.3 GB/s** (977% above requirement)

All performance requirements are met with significant headroom.

## Running Benchmarks

```bash
# Run all cache benchmarks
go test -bench=. -benchmem ./pkg/cache

# Run all WAL benchmarks
go test -bench=. -benchmem ./pkg/cache/wal

# Run specific benchmark
go test -bench=BenchmarkWrite_Sequential -benchmem ./pkg/cache

# Run with longer duration for stability
go test -bench=. -benchmem -benchtime=1s ./pkg/cache
```
