# Block Store Benchmarks

Performance comparison of the three BlockStore implementations.

**Test Environment:**
- OS: macOS (Darwin)
- Architecture: arm64
- CPU: Apple M1 Max
- Go version: 1.25.5

## Summary

| Operation | Memory | Filesystem | S3 (Localstack) |
|-----------|--------|------------|-----------------|
| Write 1KB | 7,779 MB/s | 5.77 MB/s | 0.30 MB/s |
| Write 64KB | 14,019 MB/s | 362.75 MB/s | 17.73 MB/s |
| Write 1MB | 18,291 MB/s | 2,165 MB/s | 106.66 MB/s |
| Write 4MB | 38,401 MB/s | 4,173 MB/s | 165.77 MB/s |
| Read 1KB | 8,243 MB/s | 71.30 MB/s | 0.31 MB/s |
| Read 64KB | 14,052 MB/s | 2,961 MB/s | 16.21 MB/s |
| Read 1MB | 16,213 MB/s | 10,219 MB/s | 154.56 MB/s |
| Read 4MB | 36,813 MB/s | 10,367 MB/s | 291.75 MB/s |

## Detailed Results

### Memory Store

Pure in-memory storage using `sync.Map`. Fastest option, ideal for testing and ephemeral data.

```
BenchmarkWriteBlock/1KB-10           8959516       131.6 ns/op   7779.04 MB/s    1024 B/op    1 allocs/op
BenchmarkWriteBlock/64KB-10           285841      4675 ns/op    14019.40 MB/s   65536 B/op    1 allocs/op
BenchmarkWriteBlock/1MB-10             21045     57325 ns/op    18291.64 MB/s 1048579 B/op    1 allocs/op
BenchmarkWriteBlock/4MB-10             10982    109221 ns/op    38401.85 MB/s 4194311 B/op    1 allocs/op

BenchmarkReadBlock/1KB-10            8886727       124.2 ns/op   8243.39 MB/s    1024 B/op    1 allocs/op
BenchmarkReadBlock/64KB-10            262910      4664 ns/op    14052.07 MB/s   65536 B/op    1 allocs/op
BenchmarkReadBlock/1MB-10              18434     64674 ns/op    16213.14 MB/s 1048580 B/op    1 allocs/op
BenchmarkReadBlock/4MB-10               9268    113935 ns/op    36813.16 MB/s 4194310 B/op    1 allocs/op

BenchmarkReadBlockRange/1KB_start-10    10239274     119.9 ns/op   8543.54 MB/s    1024 B/op    1 allocs/op
BenchmarkReadBlockRange/1KB_middle-10    9485768     119.9 ns/op   8542.99 MB/s    1024 B/op    1 allocs/op
BenchmarkReadBlockRange/64KB_start-10     300465    4061 ns/op    16138.53 MB/s   65536 B/op    1 allocs/op
BenchmarkReadBlockRange/64KB_middle-10    301113    4072 ns/op    16095.43 MB/s   65536 B/op    1 allocs/op

BenchmarkWriteBlock_Parallel-10         336319      3549 ns/op    18464.55 MB/s   65560 B/op    2 allocs/op
```

**Key characteristics:**
- Single allocation per operation (data copy only)
- Consistent performance regardless of offset (range reads)
- Parallel writes scale well with goroutines

### Filesystem Store

Local filesystem storage with atomic writes (temp file + rename pattern).

```
BenchmarkWriteBlock/1KB-10              6675    177602 ns/op      5.77 MB/s    1320 B/op   13 allocs/op
BenchmarkWriteBlock/64KB-10             6256    180667 ns/op    362.75 MB/s    1320 B/op   13 allocs/op
BenchmarkWriteBlock/1MB-10              2071    484249 ns/op   2165.36 MB/s    1320 B/op   13 allocs/op
BenchmarkWriteBlock/4MB-10              1033   1004987 ns/op   4173.49 MB/s    1321 B/op   13 allocs/op

BenchmarkReadBlock/1KB-10              82122     14362 ns/op     71.30 MB/s    1688 B/op    6 allocs/op
BenchmarkReadBlock/64KB-10             59276     22129 ns/op   2961.56 MB/s   74264 B/op    6 allocs/op
BenchmarkReadBlock/1MB-10              10000    102610 ns/op  10219.08 MB/s 1057308 B/op    6 allocs/op
BenchmarkReadBlock/4MB-10               2870    404544 ns/op  10367.97 MB/s 4203036 B/op    6 allocs/op

BenchmarkReadBlockRange/1KB_start-10    83851     14473 ns/op     70.75 MB/s    1560 B/op    6 allocs/op
BenchmarkReadBlockRange/1KB_middle-10   84792     14365 ns/op     71.28 MB/s    1560 B/op    6 allocs/op
BenchmarkReadBlockRange/64KB_start-10   57453     21832 ns/op   3001.86 MB/s   66072 B/op    6 allocs/op
BenchmarkReadBlockRange/64KB_middle-10  60877     20857 ns/op   3142.14 MB/s   66072 B/op    6 allocs/op

BenchmarkWriteBlock_Parallel-10         6666    187068 ns/op    350.33 MB/s    1326 B/op   13 allocs/op
```

**Key characteristics:**
- Write latency dominated by fsync (atomic write pattern)
- Small writes (1KB) have high overhead due to fixed filesystem costs
- Read throughput approaches memory bandwidth for large blocks
- Range reads have same performance as full reads (seek is O(1))

### S3 Store (Localstack)

S3-compatible storage via AWS SDK. Tested against Localstack container.

```
BenchmarkWriteBlock/1KB-10               340   3423864 ns/op      0.30 MB/s   65544 B/op  701 allocs/op
BenchmarkWriteBlock/64KB-10              318   3695782 ns/op     17.73 MB/s   97560 B/op  701 allocs/op
BenchmarkWriteBlock/1MB-10               128   9831242 ns/op    106.66 MB/s   97319 B/op  701 allocs/op
BenchmarkWriteBlock/4MB-10                45  25302039 ns/op    165.77 MB/s   96089 B/op  710 allocs/op

BenchmarkReadBlock/1KB-10                379   3337463 ns/op      0.31 MB/s   60751 B/op  670 allocs/op
BenchmarkReadBlock/64KB-10               334   4042239 ns/op     16.21 MB/s  344543 B/op  690 allocs/op
BenchmarkReadBlock/1MB-10                183   6784343 ns/op    154.56 MB/s 5304431 B/op  714 allocs/op
BenchmarkReadBlock/4MB-10                 76  14376197 ns/op    291.75 MB/s 21172428 B/op 721 allocs/op

BenchmarkReadBlockRange/1KB_start-10     422   2772734 ns/op      0.37 MB/s   62115 B/op  673 allocs/op
BenchmarkReadBlockRange/1KB_middle-10    436   2943933 ns/op      0.35 MB/s   62127 B/op  674 allocs/op
BenchmarkReadBlockRange/64KB_start-10    378   3051611 ns/op     21.48 MB/s  345599 B/op  692 allocs/op
BenchmarkReadBlockRange/64KB_middle-10   385   3109545 ns/op     21.08 MB/s  345557 B/op  693 allocs/op

BenchmarkWriteBlock_Parallel-10          516   2152563 ns/op     30.45 MB/s  102636 B/op  700 allocs/op
```

**Key characteristics:**
- Latency dominated by HTTP round-trips (~3ms minimum)
- High allocation count due to AWS SDK request/response handling
- Throughput improves significantly with larger blocks (amortizes HTTP overhead)
- Parallel writes show ~1.5x improvement due to connection reuse
- Range reads use HTTP Range header (efficient for partial reads)

**Note:** These benchmarks use Localstack (local container). Real AWS S3 will have:
- Higher latency (network round-trip to AWS region)
- Similar throughput characteristics for large objects
- Better parallel performance (distributed backend)

## Recommendations

| Use Case | Recommended Store |
|----------|-------------------|
| Unit tests | Memory |
| Integration tests | Memory or Filesystem |
| Local development | Filesystem |
| Single-server deployment | Filesystem |
| Production (durability needed) | S3 |
| High-throughput workloads | S3 with caching layer |

## Running Benchmarks

```bash
# Memory store (runs without special flags)
go test -bench=. -benchmem ./pkg/payload/store/memory/

# Filesystem store (requires integration tag)
go test -tags=integration -bench=. -benchmem ./pkg/payload/store/fs/

# S3 store (requires integration tag + Docker for Localstack)
go test -tags=integration -bench=. -benchmem ./pkg/payload/store/s3/

# All stores at once
go test -tags=integration -bench=. -benchmem ./pkg/payload/store/...
```
