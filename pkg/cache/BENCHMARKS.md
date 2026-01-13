# Cache Benchmarks

## How to Run

```bash
# Full benchmark suite
go test -bench=. -benchmem ./pkg/cache/

# Sequential writes only
go test -bench=BenchmarkWriteSlice_Sequential -benchmem ./pkg/cache/

# Compare in-memory vs mmap
go test -bench="BenchmarkCache_" -benchmem ./pkg/cache/
```

## Results

**Hardware**: Apple M1 Max
**Date**: 2025-01-13

### Sequential Writes

| Size | Throughput | Allocs |
|------|------------|--------|
| 4KB | ~4300 MB/s | 1 |
| 32KB | ~4900 MB/s | 1 |
| 64KB | ~4500 MB/s | 1 |
| 128KB | ~4900 MB/s | 1 |

### In-Memory vs mmap

| Mode | Sequential 32KB |
|------|-----------------|
| In-memory | ~4933 MB/s |
| mmap | ~4982 MB/s |

### Concurrent Writes

| Benchmark | Throughput |
|-----------|------------|
| 32KB, 100 files | ~2400 MB/s |

## Requirements

- Sequential 32KB writes: > 3000 MB/s
- Concurrent writes: > 2000 MB/s
