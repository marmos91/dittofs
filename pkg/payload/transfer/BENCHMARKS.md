# Transfer Manager Benchmarks

Performance benchmarks for the transfer manager with different block store backends.

## Test Environment

- Platform: darwin/arm64 (Apple Silicon)
- Date: 2026-01-16
- Go version: go1.23
- CPU: Apple M1 Max

## Results Summary

### Single Block Operations (4MB)

| Benchmark | Memory Store | Filesystem Store | S3 Store |
|-----------|-------------|------------------|----------|
| Upload (write + flush) | 3,962 MB/s | 1,164 MB/s | ~64 MB/s |
| Download (cache miss) | 7,185 MB/s | 2,743 MB/s | ~200 MB/s |
| Flush (partial block) | 11,675 MB/s | 3,059 MB/s | ~100 MB/s |
| Concurrent Upload (4 files) | 2,940 MB/s | 796 MB/s | ~150 MB/s |

### Large File Operations (NFS-style 32KB writes)

| File Size | Memory Store | Filesystem Store | Allocations (Memory) |
|-----------|-------------|------------------|----------------------|
| 16 MB | 1,865 MB/s | 1,221 MB/s | 139 MB |
| 64 MB | 1,736 MB/s | 808 MB/s | 512 MB |

### Sequential Write Performance (Critical for NFS)

| Write Size | Memory Store | Filesystem Store |
|-----------|-------------|------------------|
| 32 KB | 2,239 MB/s | 3,676 MB/s |
| 64 KB | 7,042 MB/s | N/A |

## Profiling Results

CPU profiling shows only **2.46% of time** in application code. The rest is:
- I/O syscalls (27%)
- Goroutine synchronization (30%)
- Memory management (13%)
- Random data generation for UUIDs (17%)

This confirms the code is now **I/O bound**, not CPU bound - exactly what we want for network filesystems where the real bottleneck is network/disk speed.

## Running Benchmarks

```bash
# Run all benchmarks (requires -tags=integration)
go test -tags=integration -bench=. -benchmem ./pkg/payload/transfer/...

# Run specific benchmark with limited iterations (faster)
go test -tags=integration -bench=BenchmarkLargeFile -benchtime=3x -benchmem ./pkg/payload/transfer/...

# Run with CPU profile
go test -tags=integration -bench=BenchmarkLargeFile -benchtime=3x -cpuprofile=cpu.prof ./pkg/payload/transfer/...
go tool pprof -http=:8080 cpu.prof

# Run with memory profile
go test -tags=integration -bench=BenchmarkLargeFile -benchtime=3x -memprofile=mem.prof ./pkg/payload/transfer/...
go tool pprof -http=:8080 mem.prof
```

## S3 Benchmarks

S3 benchmarks support three modes:

### Option 1: Auto-start Localstack (default)
```bash
# Automatically starts Localstack via testcontainers
go test -tags=integration -bench="S3" -benchmem ./pkg/payload/transfer/...
```

### Option 2: Pre-started Localstack (faster iteration)
```bash
# Start Localstack manually for faster repeated runs
docker run -d -p 4566:4566 localstack/localstack:3.0

# Set endpoint to use pre-started container
export LOCALSTACK_ENDPOINT=http://localhost:4566
go test -tags=integration -bench="S3" -benchmem ./pkg/payload/transfer/...
```

### Option 3: Real AWS S3 (production benchmarks)
```bash
# Set your AWS credentials (via profile, environment, or IAM role)
export AWS_PROFILE=my-profile  # or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY

# Set the benchmark bucket and region
export S3_BENCHMARK_BUCKET=my-benchmark-bucket
export S3_BENCHMARK_REGION=us-east-1  # optional, defaults to us-east-1

# Run benchmarks against real S3
go test -tags=integration -bench="S3" -benchmem ./pkg/payload/transfer/...
```

**Note**: When using real S3, all benchmark data is stored under the `blocks/` prefix and automatically cleaned up after each benchmark run.

### Expected S3 Performance

| Mode | Upload | Download | Large file (16MB) |
|------|--------|----------|-------------------|
| Localstack | ~64-100 MB/s | ~200-300 MB/s | ~100 MB/s |
| Real S3 (same region) | ~100-300 MB/s | ~300-500 MB/s | ~150-300 MB/s |
| Real S3 (cross region) | ~50-100 MB/s | ~100-200 MB/s | ~80-150 MB/s |

## Garbage Collection Performance

The GC scans the block store and removes orphan blocks (blocks without metadata).

### GC Benchmarks (Memory Store)

| Files | Time/op | Memory/op | Allocs/op |
|-------|---------|-----------|-----------|
| 10 files | ~15 μs | ~8 KB | ~100 |
| 100 files | ~120 μs | ~55 KB | ~1,000 |
| 1000 files | ~1.2 ms | ~571 KB | ~10,000 |

### Key Metrics

| Metric | Value |
|--------|-------|
| Parse block key | 34.6 ns (zero alloc) |
| Per-file overhead | ~1.2 μs |
| Memory scaling | O(n) linear |

### S3 GC Characteristics

For S3 stores, GC performance depends on:
- ListObjectsV2 pagination (1000 objects/request)
- DeleteObjects batching (1000 objects/request)
- Network latency to S3 endpoint

Expected S3 GC times:
- 1000 files: ~2-5 seconds (dominated by API calls)
- 10000 files: ~20-50 seconds

### Running GC Benchmarks

```bash
# Unit benchmarks (memory store)
go test ./pkg/payload/transfer/... -bench='GC' -benchmem -run='^$'

# Integration benchmarks (filesystem, S3)
go test -tags=integration ./pkg/payload/transfer/... -bench='GC' -benchmem -run='^$'
```

## Performance Targets

| Target | Status |
|--------|--------|
| 300+ MB/s sequential write | **Achieved** (2,239 MB/s) |
| 300+ MB/s large file write | **Achieved** (1,865 MB/s) |
| < 200 MB allocations/16MB file | **Achieved** (139 MB) |
| I/O bound (< 5% CPU in app code) | **Achieved** (2.46%) |

## Raw Benchmark Output

```
BenchmarkUpload_Memory-10                      	3961.87 MB/s	16786272 B/op	  68 allocs/op
BenchmarkUpload_Filesystem-10                  	1164.46 MB/s	 8406232 B/op	 143 allocs/op
BenchmarkDownload_Memory-10                    	7185.10 MB/s	 8395280 B/op	 100 allocs/op
BenchmarkDownload_Filesystem-10                	2743.32 MB/s	 8411648 B/op	 170 allocs/op
BenchmarkFlush_Memory-10                       	11675.17 MB/s	 4201152 B/op	  59 allocs/op
BenchmarkFlush_Filesystem-10                   	3058.56 MB/s	 2110016 B/op	 120 allocs/op
BenchmarkConcurrentUpload_Memory-10            	2940.34 MB/s	67130632 B/op	 220 allocs/op
BenchmarkConcurrentUpload_Filesystem-10        	 796.18 MB/s	33597848 B/op	 416 allocs/op
BenchmarkLargeFile_16MB_Memory-10              	1865.15 MB/s	139245032 B/op	 122 allocs/op
BenchmarkLargeFile_64MB_Memory-10              	1735.57 MB/s	511853816 B/op	 263 allocs/op
BenchmarkLargeFile_16MB_Filesystem-10          	1221.41 MB/s	105713040 B/op	 287 allocs/op
BenchmarkLargeFile_64MB_Filesystem-10          	 807.55 MB/s	386075872 B/op	 700 allocs/op
BenchmarkSequentialWrite_32KB_Memory-10        	2238.86 MB/s	  262808 B/op	   9 allocs/op
BenchmarkSequentialWrite_64KB_Memory-10        	7042.25 MB/s	  262808 B/op	   9 allocs/op
BenchmarkSequentialWrite_32KB_Filesystem-10    	3676.47 MB/s	  262808 B/op	   9 allocs/op
```
