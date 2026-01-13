# Cache Benchmarks

Baseline benchmark results for the cache layer. Use these to compare performance after changes.

## How to Run

```bash
# Full benchmark suite
go test ./pkg/cache/store/memory/... -bench=. -benchmem -count=3

# Quick comparison (key metrics)
go test ./pkg/cache/store/memory/... -bench="BenchmarkWriteSlice_Sequential/size=32KB|BenchmarkWriteSlice_Concurrent" -benchmem -count=3
```

## Baseline Results

**Date**: 2024-01-13
**Commit**: feat/chunking branch (after Store interface refactor)
**Hardware**: Apple M1 Max
**Go Version**: 1.21+

### Sequential Writes (Critical Path)

Sequential writes are the hot path for NFS file copies. The `TryExtendAdjacentSlice` optimization uses `append()` for amortized O(1) growth.

| Size | Throughput | Allocs | Notes |
|------|------------|--------|-------|
| 4KB | ~1.3 MB/s | 1 | Small writes, overhead dominates |
| 32KB | **5000-5500 MB/s** | 1 | Typical NFS write size |
| 64KB | ~5200 MB/s | 1 | |
| 128KB | ~5300 MB/s | 1 | |

### Concurrent Writes

Multiple goroutines writing to different files simultaneously.

| Benchmark | Throughput | Allocs | Notes |
|-----------|------------|--------|-------|
| Concurrent (32KB) | **2400-2500 MB/s** | 10 | 100 files, parallel |

### Raw Store Operations

| Operation | Throughput | Allocs | Notes |
|-----------|------------|--------|-------|
| AddSlice | ~320 MB/s | 6 | New slice creation |
| GetSlices | - | - | Returns deep copies |

### End-to-End

| File Size | Throughput | Notes |
|-----------|------------|-------|
| 1MB | ~385 MB/s | Write + coalesce |
| 10MB | ~33 MB/s | |
| 100MB | ~7 MB/s | Memory pressure |

## Key Performance Invariants

1. **Sequential 32KB writes MUST be > 3000 MB/s** - This is the NFS hot path
2. **Concurrent writes MUST be > 2000 MB/s** - Multi-client scenario
3. **Sequential writes should have ≤ 1 alloc/op** - `append()` optimization working

## Architecture Notes

The performance depends on:

1. **`TryExtendAdjacentSlice`** in Store - Uses `append()` for O(1) amortized growth
2. **Per-file locking** in Cache - Allows concurrent access to different files
3. **Map-level RWMutex** in Store - Minimal contention for file lookups

## Historical Comparisons

### 2024-01-13: Store Interface Refactor

Before optimization (naive GetSlices + UpdateSlice):
- Sequential 32KB: ~8 MB/s ❌ (375x slower)
- Concurrent: ~220 MB/s ❌ (10x slower)

After adding `TryExtendAdjacentSlice`:
- Sequential 32KB: ~5500 MB/s ✅
- Concurrent: ~2400 MB/s ✅

**Lesson**: Never do `GetSlices()` (deep copy) + `UpdateSlice()` (another copy) for sequential write optimization. Use atomic in-place extension.
