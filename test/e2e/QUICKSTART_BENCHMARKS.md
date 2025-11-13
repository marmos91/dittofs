# Benchmark Quick Start

Get started with DittoFS benchmarks in 5 minutes.

## TL;DR

```bash
# Run all benchmarks (takes ~10 minutes)
./scripts/benchmark.sh

# View results
cat benchmark_results/*/SUMMARY.md
```

## What Gets Benchmarked?

The benchmark suite tests **end-to-end NFS operations** through a real mounted filesystem:

1. **File Operations**: Create, stat, delete, rename
2. **Directory Operations**: Mkdir, readdir (small and large directories)
3. **Read Throughput**: 4KB to 100MB files
4. **Write Throughput**: 4KB to 100MB files
5. **Mixed Workload**: Realistic combination of operations
6. **Metadata Operations**: Chmod, chtimes

All benchmarks run against **both storage backends**:
- Memory store (fastest)
- Filesystem store (realistic)

## Quick Commands

### Standard Benchmark Run

```bash
./scripts/benchmark.sh
```

**Output**: `benchmark_results/<timestamp>/SUMMARY.md`

**Duration**: ~10 minutes (depends on BENCH_TIME)

### With Profiling (Recommended)

```bash
./scripts/benchmark.sh --profile
```

**Additional Output**:
- `profiles/cpu_*.prof` - CPU profiles
- `profiles/mem_*.prof` - Memory profiles
- `reports/*.txt` - Profile analysis
- `reports/*.svg` - Flame graphs (requires graphviz)

**Duration**: ~20 minutes

### Compare with Previous Run

```bash
# First run (baseline)
./scripts/benchmark.sh

# Make changes to code...

# Second run (comparison)
./scripts/benchmark.sh --compare
```

**Additional Output**: `comparison.txt` with statistical comparison

### Custom Configuration

```bash
# Longer benchmarks for more stable results
BENCH_TIME=30s BENCH_COUNT=5 ./scripts/benchmark.sh

# Quick sanity check
BENCH_TIME=5s BENCH_COUNT=1 ./scripts/benchmark.sh --no-bench --profile
```

## Understanding Results

### Throughput (Higher is Better)

```
| Operation | Store Type | Throughput |
|-----------|------------|------------|
| Read/1MB  | memory     | 450 MB/s   | ← Good
| Read/1MB  | filesystem | 280 MB/s   | ← Slower (disk I/O)
```

**What to look for**:
- Memory store: 200-500 MB/s for large files
- Filesystem store: Depends on your disk (SSD: 200-400 MB/s, HDD: 80-150 MB/s)

### Latency (Lower is Better)

```
| Operation | Store Type | ns/op   | B/op | allocs/op |
|-----------|------------|---------|------|-----------|
| Create    | memory     | 125000  | 512  | 8         | ← Good
| Create    | filesystem | 450000  | 768  | 12        | ← Higher (disk latency)
```

**What to look for**:
- File operations: < 1ms (1,000,000 ns)
- Stat operations: < 100μs (100,000 ns)
- Low allocations: < 20 allocs/op

### Memory Profile

Check `reports/mem_*_alloc.txt` for top allocators:

```
Showing nodes accounting for 512MB, 89% of 576MB total
      256MB  44.4%  github.com/marmos91/dittofs/internal/protocol/nfs.handleWrite
      128MB  22.2%  encoding/binary.Read
       64MB  11.1%  github.com/marmos91/dittofs/pkg/content/fs.WriteAt
```

**What to look for**:
- Buffer allocation patterns
- Memory leaks (growing over time)
- Excessive copying

### CPU Profile

Check `reports/cpu_*_top.txt` for hot functions:

```
      2.50s 15.6% github.com/marmos91/dittofs/internal/protocol/nfs/xdr.DecodeBytes
      1.80s 11.2% github.com/marmos91/dittofs/internal/protocol/nfs/rpc.handleCall
      1.20s  7.5% runtime.mallocgc
```

**What to look for**:
- XDR encoding/decoding overhead
- Lock contention (sync.Mutex functions)
- Unexpected hot spots

## Analyzing Profiles Interactively

### CPU Profile

```bash
# Text-based UI
go tool pprof benchmark_results/latest/profiles/cpu_write_memory.prof

# Commands:
# - top: Show top functions by CPU time
# - list <func>: Show source code with timing
# - web: Open in browser (requires graphviz)

# Web UI (best experience)
go tool pprof -http=:8080 benchmark_results/latest/profiles/cpu_write_memory.prof
```

### Memory Profile

```bash
# Show allocation hotspots
go tool pprof -alloc_space benchmark_results/latest/profiles/mem_write_100mb.prof

# Show in-use memory (for leak detection)
go tool pprof -inuse_space benchmark_results/latest/profiles/mem_mixed.prof

# Web UI
go tool pprof -http=:8080 -alloc_space benchmark_results/latest/profiles/mem_write_100mb.prof
```

## Running Specific Benchmarks

### By Store Type

```bash
# Only memory store
go test -bench='BenchmarkE2E/memory' ./test/e2e/

# Only filesystem store
go test -bench='BenchmarkE2E/filesystem' ./test/e2e/
```

### By Operation Type

```bash
# Only throughput tests
go test -bench='Throughput' -benchtime=20s ./test/e2e/

# Only file operations
go test -bench='FileOperations' ./test/e2e/

# Only large files (stress test)
go test -bench='100MB' -benchtime=5s ./test/e2e/
```

### Single Benchmark

```bash
# Very specific
go test -bench='BenchmarkE2E/memory/WriteThroughput/1MB' \
    -benchtime=30s -benchmem -cpuprofile=cpu.prof ./test/e2e/
```

## Troubleshooting

### "NFS mounting not available"

You need NFS client tools installed:

```bash
# macOS: Already included
# Linux:
sudo apt-get install nfs-common

# Test mounting manually:
sudo mount -t nfs -o vers=3,tcp,port=2049 localhost:/export /mnt/test
```

### Benchmarks are slow

```bash
# Reduce benchmark time
BENCH_TIME=5s ./scripts/benchmark.sh

# Skip large file tests
go test -bench='!/100MB' ./test/e2e/
```

### High variance in results

```bash
# Run more iterations
BENCH_COUNT=10 ./scripts/benchmark.sh

# Increase benchmark time
BENCH_TIME=30s ./scripts/benchmark.sh

# Close other applications
# Disable CPU throttling (laptop)
```

### Profile visualization fails

Install graphviz:

```bash
# macOS
brew install graphviz

# Linux
sudo apt-get install graphviz

# Test
dot -V
```

## Next Steps

1. **Read full documentation**: [BENCHMARKS.md](BENCHMARKS.md)
2. **Compare with other implementations**: [COMPARISON_GUIDE.md](COMPARISON_GUIDE.md)
3. **Set up automated benchmarking**: Run weekly and track regressions
4. **Share results**: Help us understand real-world performance

## Common Questions

**Q: Should I run benchmarks in CI?**
A: No. Benchmarks are stress tests that take 10-30 minutes and require NFS mounting. Run them manually or on a schedule.

**Q: Why are filesystem benchmarks slower?**
A: That's expected. Filesystem store has real disk I/O overhead. It's useful for testing realistic scenarios.

**Q: How do I compare with kernel NFS?**
A: See [COMPARISON_GUIDE.md](COMPARISON_GUIDE.md) for detailed methodology and tools.

**Q: What throughput should I expect?**
A: Depends on your hardware:
- Memory store: 200-500 MB/s (limited by Go runtime)
- Filesystem store (SSD): 150-350 MB/s
- Filesystem store (HDD): 80-150 MB/s
- Kernel NFS (SSD): 400-800 MB/s

**Q: Can I benchmark my own storage backend?**
A: Yes! Implement the ContentStore interface and add it to the benchmark matrix in `framework/server.go`.

## Tips for Best Results

1. **Close other applications** during benchmarking
2. **Use a fast disk** (SSD) for filesystem backend tests
3. **Run multiple times** and average the results
4. **Disable CPU throttling** on laptops
5. **Compare apples to apples**: Same machine, same conditions
6. **Profile first, optimize second**: Don't guess where the bottlenecks are
