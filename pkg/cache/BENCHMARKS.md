# Cache & WAL Benchmarks

Performance benchmarks for the cache and WAL modules.

**Test Environment:**
- CPU: Apple M1 Max
- OS: macOS (darwin/arm64)
- Go: 1.23+
- Date: 2026-01-15

## Summary

| Operation | Throughput | Notes |
|-----------|------------|-------|
| Sequential Write (32KB) | **5.0 GB/s** | Zero allocations |
| File Copy (100MB) | **4.8 GB/s** | End-to-end with coalesce |
| Concurrent File Copies | **8.6 GB/s** | Multi-threaded |
| WAL Append (32KB) | **1.9 GB/s** | mmap-backed, zero allocs |
| WAL vs In-Memory | ~3% overhead | Negligible WAL cost |

## Write Benchmarks

### Sequential Writes

Sequential writes benefit from the slice extension optimization, achieving near-memory-copy speeds.

| Size | Throughput | Allocs/op |
|------|------------|-----------|
| 4KB | 4.1 GB/s | 0 |
| 16KB | 5.1 GB/s | 0 |
| 32KB | 5.0 GB/s | 0 |
| 64KB | 4.6 GB/s | 0 |
| 128KB | 5.2 GB/s | 0 |

The sequential extension optimization merges 32KB NFS writes into single growing slices:
- **96K iterations** produced only **47 slices** (not 96K slices)
- This is critical for NFS file copy performance

### Random Writes

Random writes create more slices due to non-adjacent offsets:

| Pattern | Throughput | Allocs/op |
|---------|------------|-----------|
| Random 4KB | 1.1 GB/s | 5 |

### Multi-File Writes

Writing to multiple files concurrently:

| Files | Throughput | Allocs/op |
|-------|------------|-----------|
| 10 | 4.9 GB/s | 1 |
| 100 | 4.9 GB/s | 1 |
| 1000 | 4.9 GB/s | 2 |

Excellent scalability - throughput remains constant as file count increases.

### Concurrent Writes

Multi-threaded write performance (GOMAXPROCS=10):

| Benchmark | Throughput | Notes |
|-----------|------------|-------|
| Concurrent 32KB | 4.6 GB/s | Minimal lock contention |

## Read Benchmarks

### Sequential Reads

| Size | Throughput | Allocs/op |
|------|------------|-----------|
| 4KB | 365 MB/s | 1 |
| 32KB | 370 MB/s | 1 |
| 64KB | 369 MB/s | 1 |
| 128KB | 372 MB/s | 1 |

Read performance is consistent across sizes.

### Slice Merging (Newest-Wins Algorithm)

Performance when reading ranges covered by overlapping slices:

| Overlapping Slices | Throughput |
|--------------------|------------|
| 1 | 2.3 GB/s |
| 5 | 812 MB/s |
| 10 | 459 MB/s |
| 25 | 312 MB/s |
| 50 | 313 MB/s |

The merge algorithm scales sub-linearly with slice count.

## Flush Benchmarks

### GetDirtySlices

Retrieving dirty slices for upload:

| Chunks | Latency | Allocs |
|--------|---------|--------|
| 1 | 175 ns | 2 |
| 10 | 2.1 us | 15 |
| 100 | 23.5 us | 108 |

### CoalesceWrites

Merging adjacent slices before flush:

| Slices | Latency | Allocs |
|--------|---------|--------|
| 10 | 11.7 us | 40 |
| 50 | 37.8 us | 164 |
| 100 | 72.8 us | 316 |

## Eviction Benchmarks

LRU eviction with dirty data protection:

| Operation | Latency | Notes |
|-----------|---------|-------|
| EvictLRU (1MB) | 122 us | From 100MB cache |

## End-to-End Benchmarks

### File Copy Simulation

Simulates complete NFS file copy (sequential 32KB writes + coalesce):

| File Size | Throughput | Total Time |
|-----------|------------|------------|
| 1MB | 4.5 GB/s | 235 us |
| 10MB | 4.8 GB/s | 2.2 ms |
| 100MB | 4.8 GB/s | 21.7 ms |

### Concurrent File Copies

Multiple files being copied simultaneously:

| Benchmark | Throughput |
|-----------|------------|
| Concurrent 1MB files | 8.6 GB/s |

Excellent parallel scaling with multiple goroutines.

### Mixed Read/Write

Write-Read-Write pattern:

| Operation | Throughput |
|-----------|------------|
| Write+Read+Write | 973 MB/s |

## WAL Benchmarks

### Append Performance

| Data Size | Throughput | Allocs/op |
|-----------|------------|-----------|
| 512B | 1.0 GB/s | 0 |
| 32KB | 1.9 GB/s | 0 |
| 1MB | 2.7 GB/s | 0 |

Zero-allocation writes via mmap.

### Recovery Performance

| Entries | Latency | Allocs |
|---------|---------|--------|
| 100 | 66 us | 419 |
| 1000 | 345 us | 4022 |
| 500 (with removes) | 181 us | 2050 |

Recovery is fast even with thousands of entries.

### WAL Operations

| Operation | Latency |
|-----------|---------|
| AppendRemove | 80 ns |
| Sync (MS_ASYNC) | 14 ns |

## WAL vs In-Memory Comparison

Direct comparison of cache performance with and without WAL persistence:

| Benchmark | In-Memory | With WAL | Overhead |
|-----------|-----------|----------|----------|
| Sequential 32KB | 5.9 GB/s | 5.7 GB/s | ~3% |
| File Copy 10MB | 4.7 GB/s | 4.8 GB/s | ~0% |

**Key Finding:** WAL persistence adds negligible overhead (~3%) to write operations.

## Memory Allocation

| Operation | Allocs/op | Notes |
|-----------|-----------|-------|
| WriteSlice (new file) | 10 | Creates file/chunk entries |
| WriteSlice (extend) | 0 | Zero-alloc slice extension |
| ReadSlice | 1 | Destination buffer |
| WAL Append | 0 | Direct mmap write |

## Performance Requirements

The cache must achieve **>3 GB/s** sequential write throughput for acceptable NFS file copy performance. Current benchmarks show:

- Sequential writes: **5.0 GB/s** (67% above requirement)
- File copy E2E: **4.8 GB/s** (60% above requirement)
- Concurrent copies: **8.6 GB/s** (187% above requirement)

All performance requirements are met with significant headroom.

## Running Benchmarks

```bash
# Run all cache benchmarks
go test -bench=. -benchmem ./pkg/cache

# Run all WAL benchmarks
go test -bench=. -benchmem ./pkg/cache/wal

# Run specific benchmark
go test -bench=BenchmarkWriteSlice_Sequential -benchmem ./pkg/cache

# Run with longer duration for stability
go test -bench=. -benchmem -benchtime=1s ./pkg/cache
```
