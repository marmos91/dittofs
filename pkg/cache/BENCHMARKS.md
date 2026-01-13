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
**Date**: 2026-01-13

### Direct Cache Performance

#### Sequential Writes

| Size | Throughput | Allocs |
|------|------------|--------|
| 4KB | ~2953 MB/s | 1 |
| 32KB | ~3936 MB/s | 1 |
| 64KB | ~4252 MB/s | 1 |
| 128KB | ~3926 MB/s | 1 |

#### In-Memory vs mmap

| Mode | Sequential 32KB |
|------|-----------------|
| In-memory | ~3479 MB/s |
| mmap | ~4789 MB/s |

#### E2E Sequential Write

| Size | Throughput |
|------|------------|
| 1MB | ~2204 MB/s |
| 10MB | ~3221 MB/s |
| 100MB | ~4532 MB/s |

#### Concurrent Writes

| Benchmark | Throughput |
|-----------|------------|
| 32KB, 100 files | ~2264 MB/s |

### NFS Performance (via localhost)

Measured with mmap cache enabled, memory metadata store, and ERROR logging level.

#### Throughput (After Optimizations)

| Operation | Cold | Warm (Cached) |
|-----------|------|---------------|
| Sequential Write (100MB, 1M blocks) | ~150 MB/s | **~850 MB/s** |
| Sequential Read (100MB) | ~720 MB/s | ~720 MB/s |

**Note**: "Cold" is first write to a file (cache miss). "Warm" is subsequent writes to the same file (all caches hot).

#### Optimization Impact

| Benchmark | Baseline | Cold | Warm | Improvement |
|-----------|----------|------|------|-------------|
| Sequential Write | 89 MB/s | 150 MB/s | 850 MB/s | +68% / +855% |
| Sequential Read | 626 MB/s | 720 MB/s | 720 MB/s | +15% |

#### Logging Level Impact

| Benchmark | DEBUG | ERROR | Improvement |
|-----------|-------|-------|-------------|
| Sequential Write | 70 MB/s | 89 MB/s | +27% |
| Sequential Read | 350 MB/s | 626 MB/s | +79% |
| Concurrent Write | 213 MB/s | 275 MB/s | +29% |

## NFS Performance Analysis

### Why NFS is slower than direct cache

The ~30x gap between direct cache (4000 MB/s) and NFS (125 MB/s) is due to:

1. **Metadata operations per WRITE** - Each NFS WRITE triggers ~3 metadata operations:
   - PrepareWrite/GetFile (1 read + validation)
   - CommitWrite (1 transaction + 1 read + 1 write)

2. **Protocol overhead** - XDR encoding, RPC framing, TCP/IP stack

3. **Userspace vs kernel** - Context switches, syscall overhead

### Optimizations Applied

1. **Eliminated buffer copy** - Pooled buffer passed directly to worker goroutine
2. **Removed double RPC parsing** - RPC header parsed once in readRequest, reused in processRequest
3. **Removed GetFilesystemCapabilities call** - Use fixed max write size (1MB)
4. **Removed redundant getFileOrError** - PrepareWrite does same validation
5. **Deferred metadata commits** - CommitWrite batches updates until NFS COMMIT
6. **File metadata caching** - PrepareWrite caches file metadata for sequential writes
7. **Auth context caching** - Cache per (share, UID, GID) to avoid registry lookups

### Future Optimization Opportunities

1. **Warm cache on file open** - Pre-populate caches on CREATE/LOOKUP
2. **Use sync.Map for pending writes** - Reduce mutex contention for concurrent writes
3. **Batch multiple files in COMMIT** - Single transaction for multi-file commits

## Requirements

- Direct cache sequential 32KB writes: > 3000 MB/s
- Direct cache concurrent writes: > 2000 MB/s
- NFS sequential writes: > 80 MB/s
- NFS sequential reads: > 500 MB/s
