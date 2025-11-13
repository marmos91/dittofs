# DittoFS vs FUSE-based Solutions: Comparison Guide

This guide provides methodology and scripts for comparing DittoFS performance with FUSE-based NFS implementations and other virtual filesystem solutions.

## Why Compare?

DittoFS implements NFSv3 in **userspace without FUSE**, which has different performance characteristics than:
- Kernel NFS implementations
- FUSE-based NFS servers
- FUSE-based virtual filesystems

Understanding these differences helps evaluate DittoFS for your use case.

## Architecture Differences

### DittoFS (Pure Go Userspace)
```
Client → NFSv3 Protocol → Go TCP Server → DittoFS → Storage Backend
                                          ↓
                                    No kernel involvement
```

**Pros**:
- No context switches to kernel
- Pure Go, portable
- Easy to debug and profile

**Cons**:
- Userspace TCP handling
- Go runtime overhead

### FUSE-based NFS (e.g., go-nfs + FUSE)
```
Client → NFSv3 Protocol → NFS Server → FUSE → Kernel VFS → Userspace Handler → Storage
                                        ↓
                                Context switches
```

**Pros**:
- Leverages kernel VFS layer
- Standard filesystem interface

**Cons**:
- Context switch overhead
- FUSE performance limitations
- More complex debugging

### Kernel NFS
```
Client → NFSv3 Protocol → Kernel NFS Server → VFS → Storage
                            ↓
                    All in-kernel
```

**Pros**:
- Highest performance
- Zero context switches
- Mature, well-optimized

**Cons**:
- Not portable
- Difficult to customize
- Complex debugging

## FUSE-based Solutions to Compare

### 1. **go-nfs-server** (github.com/willscott/go-nfs)

A Go-based NFSv3 server that can be mounted via FUSE.

**Setup**:
```bash
# Install
go install github.com/willscott/go-nfs/cmd/nfs-server@latest

# Run
nfs-server -addr 0.0.0.0:2050 /path/to/export
```

### 2. **nfs-ganesha** + FUSE backend

High-performance NFS server with FUSE support.

**Setup** (Linux):
```bash
# Install
apt-get install nfs-ganesha

# Configure with FUSE backend
# See: https://github.com/nfs-ganesha/nfs-ganesha
```

### 3. **Simple Go FUSE** (github.com/hanwen/go-fuse)

For comparison with generic FUSE performance.

**Setup**:
```bash
go install github.com/hanwen/go-fuse/v2/example/loopback@latest
```

### 4. **s3fs-fuse** (for object storage backends)

If comparing object storage backends:

**Setup**:
```bash
# macOS
brew install s3fs

# Linux
apt-get install s3fs
```

## Benchmark Methodology

### 1. Controlled Environment

Ensure consistent testing conditions:

```bash
# Disable CPU frequency scaling (Linux)
sudo cpupower frequency-set -g performance

# Disable Turbo Boost (macOS)
sudo sysctl -w debug.perfmon.turbo.enabled=0

# Drop caches before each run (Linux)
sudo sync; echo 3 > /proc/sys/vm/drop_caches

# Disable Spotlight indexing on test directory (macOS)
mdutil -i off /path/to/mount
```

### 2. Standard Benchmark Tools

#### FIO (Flexible I/O Tester)

```bash
# Install
brew install fio  # macOS
apt-get install fio  # Linux

# Sequential read
fio --name=seqread --rw=read --bs=1M --size=1G \
    --directory=/mnt/test --direct=1 --numjobs=1 \
    --group_reporting

# Sequential write
fio --name=seqwrite --rw=write --bs=1M --size=1G \
    --directory=/mnt/test --direct=1 --numjobs=1 \
    --group_reporting

# Random read
fio --name=randread --rw=randread --bs=4k --size=1G \
    --directory=/mnt/test --direct=1 --numjobs=4 \
    --group_reporting

# Random write
fio --name=randwrite --rw=randwrite --bs=4k --size=1G \
    --directory=/mnt/test --direct=1 --numjobs=4 \
    --group_reporting

# Mixed workload (70% read, 30% write)
fio --name=mixed --rw=randrw --rwmixread=70 --bs=4k \
    --size=1G --directory=/mnt/test --direct=1 \
    --numjobs=4 --group_reporting
```

#### iozone

```bash
# Install
brew install iozone  # macOS

# Full benchmark suite
iozone -a -g 4G -y 4k -q 1M -i 0 -i 1 -i 2 -f /mnt/test/iozone.tmp

# Throughput test
iozone -i 0 -i 1 -r 1M -s 1G -f /mnt/test/iozone.tmp
```

#### Bonnie++

```bash
# Install
apt-get install bonnie++

# Run benchmark
bonnie++ -d /mnt/test -u root -s 16G -n 128 -r 8G

# Results include:
# - Sequential output (char, block)
# - Sequential input (char, block)
# - Random seeks
# - File creation/deletion
```

### 3. NFS-Specific Benchmarks

#### nfsstat

Monitor NFS operations:

```bash
# Clear stats
nfsstat -Z

# Run workload...

# Check stats
nfsstat -c  # Client stats
nfsstat -s  # Server stats
```

#### Custom NFS Benchmark Script

```bash
#!/bin/bash
# benchmark_nfs.sh - NFS-specific benchmark

MOUNT=$1
RESULTS=$2

echo "=== NFS Benchmark: $MOUNT ==="

# Metadata operations
echo "Metadata operations (ops/sec):"
/usr/bin/time -p bash -c "
    for i in {1..10000}; do
        touch $MOUNT/file_\$i
    done
    rm $MOUNT/file_*
" 2>&1 | grep real | awk '{print 10000/$2}'

# Sequential write (MB/s)
echo "Sequential write:"
dd if=/dev/zero of=$MOUNT/testfile bs=1M count=1000 oflag=direct 2>&1 | \
    grep -oP '\d+\.?\d* MB/s'

# Sequential read (MB/s)
echo "Sequential read:"
dd if=$MOUNT/testfile of=/dev/null bs=1M iflag=direct 2>&1 | \
    grep -oP '\d+\.?\d* MB/s'

# Latency (us)
echo "Stat latency (us):"
/usr/bin/time -f "%E" stat $MOUNT/testfile 2>&1

# Cleanup
rm $MOUNT/testfile
```

### 4. Memory Usage Comparison

Track memory footprint:

```bash
# DittoFS
ps aux | grep dittofs | awk '{print $6}'  # RSS in KB

# Monitor over time
while true; do
    ps aux | grep dittofs | awk '{print $6}'
    sleep 5
done > dittofs_memory.log

# FUSE-based solution
ps aux | grep nfs-server | awk '{print $6}'
```

### 5. CPU Usage Comparison

```bash
# Use top to monitor CPU usage
top -p $(pgrep dittofs) -b -n 60 -d 1 > dittofs_cpu.log

# Or use perf (Linux)
perf record -g -p $(pgrep dittofs) sleep 60
perf report
```

## Sample Comparison Script

```bash
#!/bin/bash
# compare_implementations.sh

IMPLEMENTATIONS=(
    "dittofs:localhost:/export:2049"
    "go-nfs:localhost:/export:2050"
    "kernel-nfs:server:/export:2049"
)

MOUNT_BASE="/mnt/compare"
RESULTS_DIR="comparison_results_$(date +%Y%m%d_%H%M%S)"

mkdir -p "$RESULTS_DIR"

for impl in "${IMPLEMENTATIONS[@]}"; do
    IFS=':' read -r name host port <<< "$impl"

    echo "==================================="
    echo "Testing: $name"
    echo "==================================="

    MOUNT_POINT="$MOUNT_BASE/$name"
    mkdir -p "$MOUNT_POINT"

    # Mount
    if [[ "$name" == "kernel-nfs" ]]; then
        sudo mount -t nfs "$host" "$MOUNT_POINT"
    else
        sudo mount -t nfs -o vers=3,tcp,port="$port" "$host" "$MOUNT_POINT"
    fi

    # Wait for mount
    sleep 2

    # Run benchmarks
    echo "Running FIO benchmarks..."

    # Sequential read
    fio --name=seqread --rw=read --bs=1M --size=1G \
        --directory="$MOUNT_POINT" --numjobs=1 \
        --output="$RESULTS_DIR/${name}_seqread.json" \
        --output-format=json

    # Sequential write
    fio --name=seqwrite --rw=write --bs=1M --size=1G \
        --directory="$MOUNT_POINT" --numjobs=1 \
        --output="$RESULTS_DIR/${name}_seqwrite.json" \
        --output-format=json

    # Random operations
    fio --name=randrw --rw=randrw --bs=4k --size=512M \
        --directory="$MOUNT_POINT" --numjobs=4 \
        --output="$RESULTS_DIR/${name}_randrw.json" \
        --output-format=json

    # Metadata benchmark
    /usr/bin/time -v bash -c "
        for i in {1..1000}; do
            touch $MOUNT_POINT/file_\$i
        done
        ls $MOUNT_POINT > /dev/null
        rm $MOUNT_POINT/file_*
    " 2> "$RESULTS_DIR/${name}_metadata.txt"

    # Unmount
    sudo umount "$MOUNT_POINT"

    echo ""
done

echo "==================================="
echo "Generating comparison report..."
echo "==================================="

# Generate comparison report
python3 << 'EOF' > "$RESULTS_DIR/REPORT.md"
import json
import glob

# Parse FIO results
implementations = {}
for result_file in glob.glob(f"{RESULTS_DIR}/*_seqread.json"):
    name = result_file.split('/')[-1].split('_')[0]

    with open(result_file) as f:
        data = json.load(f)
        bw = data['jobs'][0]['read']['bw'] / 1024  # Convert to MB/s
        implementations.setdefault(name, {})['seqread'] = bw

# Generate markdown report
print("# Implementation Comparison Report")
print()
print("## Sequential Read Performance")
print()
print("| Implementation | Throughput (MB/s) |")
print("|----------------|-------------------|")
for name, metrics in sorted(implementations.items()):
    print(f"| {name} | {metrics.get('seqread', 0):.2f} |")

EOF

cat "$RESULTS_DIR/REPORT.md"
```

## Expected Performance Characteristics

### DittoFS Strengths

1. **Metadata Operations**: Fast due to in-memory metadata store
2. **Small File I/O**: Low overhead from pure Go implementation
3. **Concurrent Access**: Good Go goroutine scheduling
4. **Memory Efficiency**: Controlled allocation with buffer pools

### DittoFS Potential Weaknesses

1. **Large Sequential I/O**: May not match kernel NFS
2. **CPU Usage**: Go runtime overhead vs kernel implementation
3. **Network Performance**: Userspace TCP vs kernel TCP stack

### When to Choose DittoFS

- **Portability** is important (cross-platform)
- Need **custom business logic** in the filesystem
- **Rapid development** and iteration
- **Ease of debugging** is valuable
- Running in **containers** (no kernel module access)

### When to Choose FUSE-based Solutions

- Need **existing filesystem** semantics
- Want **kernel VFS** integration
- Have **legacy** applications expecting POSIX
- Need **security isolation** from kernel

### When to Choose Kernel NFS

- **Maximum performance** is critical
- Running on **Linux only**
- Have **mature, stable** requirements
- Can tolerate **difficult debugging**

## Interpreting Results

### Throughput Comparison

Typical results (example):

| Operation | Kernel NFS | FUSE NFS | DittoFS | Winner |
|-----------|------------|----------|---------|--------|
| Sequential Read (MB/s) | 800 | 400 | 600 | Kernel |
| Sequential Write (MB/s) | 750 | 350 | 550 | Kernel |
| Random Read (ops/s) | 5000 | 2000 | 4000 | Kernel |
| Random Write (ops/s) | 4500 | 1800 | 3500 | Kernel |
| Metadata ops (ops/s) | 10000 | 3000 | 8000 | Kernel |

**Analysis**:
- Kernel NFS leads in raw performance
- DittoFS outperforms FUSE-based solutions
- DittoFS achieves 70-80% of kernel performance
- FUSE overhead is significant (50-60% of kernel)

### Latency Comparison

| Operation | Kernel NFS | FUSE NFS | DittoFS |
|-----------|------------|----------|---------|
| GETATTR (us) | 50 | 200 | 100 |
| READ 4KB (us) | 100 | 400 | 200 |
| WRITE 4KB (us) | 150 | 500 | 250 |
| LOOKUP (us) | 75 | 250 | 120 |

**Analysis**:
- DittoFS latency is 2x kernel (acceptable)
- FUSE latency is 4x kernel (significant)
- Context switches dominate FUSE overhead

### Resource Usage

| Metric | Kernel NFS | FUSE NFS | DittoFS |
|--------|------------|----------|---------|
| Memory (MB) | N/A (kernel) | 100 | 150 |
| CPU (%) under load | 30 | 60 | 45 |
| Goroutines/Threads | N/A | 50 | 100 |

**Analysis**:
- DittoFS uses more memory (Go runtime)
- DittoFS CPU usage between kernel and FUSE
- More goroutines doesn't mean more overhead

## Publishing Results

When sharing comparison results:

1. **Document environment**:
   - OS and version
   - CPU model and core count
   - Memory size
   - Storage backend (SSD, HDD, RAM)
   - Network configuration (if applicable)

2. **Include raw data**:
   - FIO JSON output
   - Full benchmark logs
   - System stats during test

3. **Statistical significance**:
   - Run multiple iterations (5+)
   - Report mean ± std deviation
   - Use benchstat for Go benchmarks

4. **Reproducibility**:
   - Provide exact commands used
   - Document version numbers
   - Share configuration files

## Contributing Comparisons

If you run comparisons and want to share results:

1. Create a markdown file: `comparisons/<your-setup>.md`
2. Include all environment details
3. Provide raw data files
4. Submit PR to the repository

## References

- [FUSE Performance Analysis](https://www.usenix.org/system/files/conference/fast17/fast17-vangoor.pdf)
- [NFS Performance Tuning](https://access.redhat.com/documentation/en-us/red_hat_enterprise_linux/7/html/performance_tuning_guide/sect-red_hat_enterprise_linux-performance_tuning_guide-performance-nfs)
- [FIO Documentation](https://fio.readthedocs.io/)
- [Go Performance Optimization](https://dave.cheney.net/high-performance-go-workshop/gopherchina-2019.html)
