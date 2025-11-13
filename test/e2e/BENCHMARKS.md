# DittoFS Benchmark Suite

This directory contains comprehensive end-to-end benchmarks for DittoFS that stress test the complete NFS protocol implementation with different storage backends.

## Overview

The benchmark suite measures:
- **Throughput**: Read/write performance with various file sizes (4KB to 100MB)
- **Latency**: Operation-level timing for metadata and file operations
- **Memory Usage**: Allocation patterns and memory footprint
- **Scalability**: Performance with different directory sizes and workloads
- **Store Comparison**: Side-by-side comparison of memory vs filesystem backends

## Quick Start

### Running Basic Benchmarks

```bash
# Run all benchmarks (default: 10s per benchmark, 3 iterations)
./scripts/benchmark.sh

# Run with custom timing
BENCH_TIME=30s BENCH_COUNT=5 ./scripts/benchmark.sh

# Run with profiling enabled
./scripts/benchmark.sh --profile

# Compare with previous results
./scripts/benchmark.sh --compare
```

### Running Specific Benchmarks

```bash
# Run only for memory store
go test -bench='BenchmarkE2E/memory' -benchtime=10s ./test/e2e/

# Run only throughput tests
go test -bench='Throughput' -benchtime=20s ./test/e2e/

# Run with memory stats
go test -bench=. -benchmem ./test/e2e/

# Run with detailed output
go test -bench=. -benchtime=10s -v ./test/e2e/
```

## Benchmark Categories

### 1. File Operations (`FileOperations`)

Tests basic file system operations:
- **Create**: File creation throughput
- **Stat**: Metadata retrieval (getattr) performance
- **Delete**: File deletion throughput
- **Rename**: File rename/move operations

**Purpose**: Measures the overhead of metadata operations and protocol handling.

**Typical Results**:
```
memory/FileOperations/Create-8       50000    25000 ns/op    512 B/op    10 allocs/op
filesystem/FileOperations/Create-8   30000    40000 ns/op    768 B/op    15 allocs/op
```

### 2. Directory Operations (`DirectoryOperations`)

Tests directory-specific operations:
- **Mkdir**: Directory creation
- **Readdir**: Listing 100 files in a directory
- **ReaddirLarge**: Listing 1000 files in a directory

**Purpose**: Measures directory handling and scalability with large directories.

**Stress Test**: The ReaddirLarge benchmark creates 1000 entries to test handling of large directories, a common performance bottleneck in NFS implementations.

### 3. Read Throughput (`ReadThroughput`)

Measures sequential read performance with different file sizes:
- 4KB (typical small file)
- 64KB (typical NFS read size)
- 1MB (medium file)
- 10MB (large file)
- 100MB (very large file, stress test)

**Purpose**: Identifies throughput limits and overhead at different scales.

**Stress Test**: The 100MB benchmark stresses the I/O path, buffer management, and memory allocation patterns.

**Example Usage**:
```bash
# Focus on read performance
go test -bench='BenchmarkE2E/.*/ReadThroughput' -benchtime=20s ./test/e2e/
```

### 4. Write Throughput (`WriteThroughput`)

Measures sequential write performance with the same file sizes as read tests.

**Purpose**: Identifies write path bottlenecks and tests content store write performance.

**Stress Test**: Large writes (100MB) test:
- Buffer pool efficiency
- Memory allocation patterns
- Disk I/O handling (filesystem store)
- Metadata coordination between stores

**Example Usage**:
```bash
# Compare write performance between stores
go test -bench='WriteThroughput/100MB' -benchtime=10s ./test/e2e/
```

### 5. Mixed Workload (`MixedWorkload`)

Simulates realistic usage patterns with a mix of operations:
- 20% Create and write (4KB files)
- 20% Read existing files
- 20% Stat (metadata queries)
- 20% Readdir (directory listings)
- 20% Rename operations

**Purpose**: Tests performance under realistic, mixed usage scenarios.

**Stress Test**: This benchmark runs operations in quick succession without delays, stressing:
- Concurrent lock handling
- Cache coherency
- Mixed read/write performance
- Protocol overhead under varied operations

### 6. Metadata Operations (`MetadataOperations`)

Tests metadata-heavy operations:
- **Chmod**: Permission changes
- **Chtimes**: Timestamp modifications

**Purpose**: Measures metadata update performance independent of data I/O.

## Benchmark Metrics

Each benchmark reports:

1. **ns/op**: Nanoseconds per operation (latency)
2. **MB/s**: Throughput (for read/write benchmarks)
3. **B/op**: Bytes allocated per operation
4. **allocs/op**: Number of allocations per operation

## Profiling

### CPU Profiling

Identifies CPU-intensive code paths:

```bash
# Generate CPU profile
go test -bench=BenchmarkE2E/memory/WriteThroughput/100MB \
    -cpuprofile=cpu.prof \
    -benchtime=30s \
    ./test/e2e/

# Analyze with pprof
go tool pprof cpu.prof

# View in browser
go tool pprof -http=:8080 cpu.prof
```

**Common hotspots to investigate**:
- XDR encoding/decoding
- Buffer allocation/copying
- Lock contention
- Protocol message parsing

### Memory Profiling

Identifies memory allocation patterns:

```bash
# Generate memory profile
go test -bench=BenchmarkE2E/memory/WriteThroughput/100MB \
    -memprofile=mem.prof \
    -benchtime=30s \
    ./test/e2e/

# Analyze allocations
go tool pprof -alloc_space mem.prof

# Analyze in-use memory
go tool pprof -inuse_space mem.prof
```

**What to look for**:
- Excessive allocations in hot paths
- Memory leaks (growing in-use memory)
- Large temporary allocations
- Buffer pool efficiency

### Using the Benchmark Script

The `scripts/benchmark.sh` automates profiling and report generation:

```bash
# Full benchmark with all profiling
./scripts/benchmark.sh --profile --compare

# Results are saved to benchmark_results/<timestamp>/
# - profiles/: Raw .prof files
# - reports/: Text and SVG reports
# - raw/: Raw benchmark output
# - SUMMARY.md: Summary report
# - comparison.txt: Comparison with previous run (if --compare used)
```

## Comparing Stores

### Memory vs Filesystem

The benchmark suite runs all tests against both stores:

```bash
# Run both stores
go test -bench=. -benchtime=10s ./test/e2e/

# Results show side-by-side comparison:
# BenchmarkE2E/memory/ReadThroughput/1MB-8         5000   250000 ns/op  4.00 MB/s
# BenchmarkE2E/filesystem/ReadThroughput/1MB-8     3000   400000 ns/op  2.50 MB/s
```

**Expected differences**:
- **Memory store**: Faster, no disk I/O overhead
- **Filesystem store**: More realistic, tests actual file system integration

### Using benchstat for Comparison

```bash
# Install benchstat
go install golang.org/x/perf/cmd/benchstat@latest

# Save baseline
go test -bench=. -count=5 ./test/e2e/ > old.txt

# Make changes...

# Run new benchmarks
go test -bench=. -count=5 ./test/e2e/ > new.txt

# Compare
benchstat old.txt new.txt
```

## Stress Testing

### High Concurrency

Test under high concurrent load:

```bash
# Run with 16 parallel goroutines
go test -bench=. -cpu=16 -benchtime=30s ./test/e2e/
```

### Long Duration

Test for memory leaks and stability:

```bash
# Run for extended period
go test -bench=BenchmarkE2E/memory/MixedWorkload \
    -benchtime=5m \
    -memprofile=mem_long.prof \
    ./test/e2e/

# Check for memory growth
go tool pprof -inuse_space mem_long.prof
```

### Memory Pressure

Test behavior under memory constraints:

```bash
# Limit memory (Linux)
GOMEMLIMIT=512MiB go test -bench=. -benchtime=30s ./test/e2e/

# Monitor with profiling
go test -bench=BenchmarkE2E/memory/WriteThroughput/100MB \
    -benchtime=1m \
    -memprofile=mem_pressure.prof \
    ./test/e2e/
```

## Interpreting Results

### Good Performance Indicators

- **Low allocs/op**: Efficient memory usage (< 50 for most operations)
- **Consistent timing**: Low variance across iterations
- **High throughput**: > 100 MB/s for large sequential I/O
- **Fast metadata ops**: < 50μs for stat/chmod operations

### Performance Issues to Investigate

- **High allocation rate**: > 1000 allocs/op suggests excessive copying
- **High latency variance**: Suggests lock contention or GC pressure
- **Poor scaling**: Performance doesn't improve with -cpu=N
- **Memory growth**: inuse_space grows over time (memory leak)

### Comparing with Other NFS Implementations

To compare DittoFS with kernel NFS or other implementations:

1. **Mount kernel NFS**:
```bash
# Linux
sudo mount -t nfs -o vers=3 server:/export /mnt/kernel_nfs

# Run same operations manually
cd /mnt/kernel_nfs
time dd if=/dev/zero of=testfile bs=1M count=100
time dd if=testfile of=/dev/null bs=1M
```

2. **Use NFS benchmarking tools**:
```bash
# Install bonnie++ or iozone
apt-get install bonnie++

# Run on both mounts
bonnie++ -d /mnt/dittofs -u root
bonnie++ -d /mnt/kernel_nfs -u root
```

3. **Create a comparison matrix**:
Create a table comparing:
- Sequential read/write throughput
- Random I/O performance
- Metadata operation rates
- CPU usage
- Memory footprint

## Continuous Performance Tracking

### Saving Results

```bash
# Create baseline
./scripts/benchmark.sh
mv benchmark_results/latest benchmark_results/baseline

# After changes
./scripts/benchmark.sh --compare
```

### Automated Tracking

Set up periodic benchmarking:

```bash
# Add to cron (example: daily at 2 AM)
0 2 * * * cd /path/to/dittofs && ./scripts/benchmark.sh --profile --compare
```

### Performance Regression Detection

Use `benchstat` to detect regressions:

```bash
# Compare current with baseline
benchstat benchmark_results/baseline/raw/benchmark.txt \
         benchmark_results/latest/raw/benchmark.txt | \
    grep -E '\+[0-9]+\.[0-9]+%' # Flag significant increases
```

## Troubleshooting

### Benchmarks Failing to Mount

```bash
# Check NFS mount permissions
sudo mount -t nfs -o vers=3,tcp,port=2049 localhost:/export /mnt/test

# macOS may require resvport
sudo mount -t nfs -o vers=3,tcp,port=2049,resvport localhost:/export /mnt/test
```

### Inconsistent Results

- Ensure no other processes are using significant CPU/disk
- Run with `-count=5` or higher for statistical significance
- Increase `-benchtime` to reduce variance
- Close other applications

### Out of Memory

- Reduce benchmark count: `BENCH_COUNT=1`
- Reduce benchmark time: `BENCH_TIME=5s`
- Skip large file benchmarks: `-bench='!/100MB'`

## Contributing

When adding new benchmarks:

1. Follow the existing patterns in `benchmark_test.go`
2. Use descriptive names that indicate what is being measured
3. Include both memory and filesystem store variants
4. Add documentation to this file
5. Ensure benchmarks are idempotent (can run multiple times)
6. Clean up resources (unmount, stop server) in defer statements

## References

- [Go Benchmark Best Practices](https://dave.cheney.net/2013/06/30/how-to-write-benchmarks-in-go)
- [Profiling Go Programs](https://go.dev/blog/pprof)
- [NFS Performance Tuning](https://access.redhat.com/documentation/en-us/red_hat_enterprise_linux/7/html/performance_tuning_guide/sect-red_hat_enterprise_linux-performance_tuning_guide-performance-nfs)
