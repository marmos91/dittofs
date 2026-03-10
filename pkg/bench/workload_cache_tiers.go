package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// DefaultCacheTiersSizes are the file sizes benchmarked by default: 10 MB, 100 MB, 1 GB.
var DefaultCacheTiersSizes = []int64{
	10 << 20,  // 10 MiB
	100 << 20, // 100 MiB
	1 << 30,   // 1 GiB
}

// CacheTiersConfig holds configuration for the cache-tiers benchmark.
type CacheTiersConfig struct {
	// MountPoint is the NFS/SMB mount path for file I/O.
	MountPoint string

	// ShareName is the DittoFS share name for cache API calls.
	ShareName string

	// Client is an authenticated API client for cache eviction/stats.
	Client *apiclient.Client

	// FileSizes are the file sizes to benchmark (default: 10 MB, 100 MB, 1 GB).
	FileSizes []int64
}

// CacheTiersResult holds the complete cache-tiers benchmark output.
type CacheTiersResult struct {
	// Timestamp is when the benchmark started.
	Timestamp time.Time `json:"timestamp"`

	// ShareName is the share that was benchmarked.
	ShareName string `json:"share_name"`

	// Results contains per-size benchmark results.
	Results []CacheTiersSizeResult `json:"results"`

	// TotalDuration is the wall-clock time for the entire benchmark.
	TotalDuration time.Duration `json:"total_duration"`
}

// CacheTiersSizeResult holds results for a single file size.
type CacheTiersSizeResult struct {
	FileSize      int64   `json:"file_size"`
	WriteStats    IOStats `json:"write_stats"`
	ColdReadStats IOStats `json:"cold_read_stats"`
	WarmReadStats IOStats `json:"warm_read_stats"`
	L2OnlyStats   IOStats `json:"l2_only_stats"`
}

// IOStats holds throughput and cache metrics for a single I/O step.
type IOStats struct {
	ThroughputMBps float64 `json:"throughput_mbps"`
	DurationMs     int64   `json:"duration_ms"`
	L1HitRate      float64 `json:"l1_hit_rate"` // -1 means not applicable
}

// CacheTiersBenchmark runs the 6-step cache tier benchmark.
type CacheTiersBenchmark struct {
	cfg CacheTiersConfig
}

// NewCacheTiersBenchmark creates a new cache-tiers benchmark runner.
func NewCacheTiersBenchmark(cfg CacheTiersConfig) *CacheTiersBenchmark {
	if len(cfg.FileSizes) == 0 {
		cfg.FileSizes = DefaultCacheTiersSizes
	}
	return &CacheTiersBenchmark{cfg: cfg}
}

// Run executes the 6-step cache-tiers benchmark for each file size.
func (b *CacheTiersBenchmark) Run(ctx context.Context, logf func(string, ...any)) (*CacheTiersResult, error) {
	result := &CacheTiersResult{
		Timestamp: time.Now().UTC(),
		ShareName: b.cfg.ShareName,
		Results:   make([]CacheTiersSizeResult, 0, len(b.cfg.FileSizes)),
	}

	start := time.Now()

	for _, size := range b.cfg.FileSizes {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		sizeResult, err := b.runForSize(ctx, size, logf)
		if err != nil {
			logf("ERROR: %s file size failed: %v (continuing)\n", FormatSize(size), err)
			continue
		}

		result.Results = append(result.Results, *sizeResult)
	}

	result.TotalDuration = time.Since(start)
	return result, nil
}

// runForSize executes the 6-step workflow for a single file size.
func (b *CacheTiersBenchmark) runForSize(ctx context.Context, fileSize int64, logf func(string, ...any)) (*CacheTiersSizeResult, error) {
	sizeLabel := FormatSize(fileSize)

	// Create a temp directory for this test file.
	dir := filepath.Join(b.cfg.MountPoint, benchDir, fmt.Sprintf("cache_tiers_%d", fileSize))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create bench dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	testFile := filepath.Join(dir, "testfile.dat")

	result := &CacheTiersSizeResult{
		FileSize: fileSize,
	}

	// Step 1: Write
	logf("  %s: Step 1/6 - Write...\n", sizeLabel)
	writeStats, err := b.writeFile(ctx, testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	result.WriteStats = *writeStats

	// Step 2: Evict all cache
	logf("  %s: Step 2/6 - Evict all cache...\n", sizeLabel)
	if err := b.evictAll(ctx, logf); err != nil {
		logf("  WARNING: evict-all failed: %v (cold read may not be accurate)\n", err)
	}

	// Step 3: Cold read (data from remote store)
	logf("  %s: Step 3/6 - Cold read...\n", sizeLabel)
	coldStats, err := b.readFile(ctx, testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("cold read: %w", err)
	}
	coldStats.L1HitRate = b.getL1HitRate(logf)
	result.ColdReadStats = *coldStats

	// Step 4: Warm read (data in L1 + local cache)
	logf("  %s: Step 4/6 - Warm read...\n", sizeLabel)
	warmStats, err := b.readFile(ctx, testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("warm read: %w", err)
	}
	warmStats.L1HitRate = b.getL1HitRate(logf)
	result.WarmReadStats = *warmStats

	// Step 5: Evict L1 only (keep local FS blocks)
	logf("  %s: Step 5/6 - Evict L1 cache only...\n", sizeLabel)
	if err := b.evictL1(ctx, logf); err != nil {
		logf("  WARNING: L1-evict failed: %v (L2 read may not be accurate)\n", err)
	}

	// Step 6: L2-only read (data from local FS cache, not L1 memory)
	logf("  %s: Step 6/6 - L2-only read...\n", sizeLabel)
	l2Stats, err := b.readFile(ctx, testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("L2 read: %w", err)
	}
	l2Stats.L1HitRate = b.getL1HitRate(logf)
	result.L2OnlyStats = *l2Stats

	return result, nil
}

// writeFile writes a test file of the given size and returns I/O stats.
func (b *CacheTiersBenchmark) writeFile(_ context.Context, path string, size int64) (*IOStats, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, seqChunkSize)
	for i := range buf {
		buf[i] = byte(i)
	}

	start := time.Now()
	var written int64

	for written < size {
		n := size - written
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}

		w, err := f.Write(buf[:n])
		written += int64(w)
		if err != nil {
			return nil, fmt.Errorf("write at offset %d: %w", written, err)
		}
	}

	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}

	elapsed := time.Since(start)

	return &IOStats{
		ThroughputMBps: float64(written) / elapsed.Seconds() / (1 << 20),
		DurationMs:     elapsed.Milliseconds(),
		L1HitRate:      -1, // Not applicable for writes
	}, nil
}

// readFile reads an entire file and returns I/O stats.
func (b *CacheTiersBenchmark) readFile(_ context.Context, path string, size int64) (*IOStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, seqChunkSize)

	start := time.Now()
	var totalRead int64

	for totalRead < size {
		n, err := f.Read(buf)
		totalRead += int64(n)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read at offset %d: %w", totalRead, err)
		}
	}

	elapsed := time.Since(start)

	return &IOStats{
		ThroughputMBps: float64(totalRead) / elapsed.Seconds() / (1 << 20),
		DurationMs:     elapsed.Milliseconds(),
		L1HitRate:      0, // Will be populated from cache stats
	}, nil
}

// evictAll evicts both L1 and local cache for the share.
func (b *CacheTiersBenchmark) evictAll(ctx context.Context, logf func(string, ...any)) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	_, err := b.cfg.Client.CacheEvictForShare(b.cfg.ShareName, &apiclient.CacheEvictRequest{})
	if err != nil {
		return fmt.Errorf("cache evict all: %w", err)
	}
	return nil
}

// evictL1 evicts only the L1 (memory) cache for the share.
func (b *CacheTiersBenchmark) evictL1(ctx context.Context, logf func(string, ...any)) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	_, err := b.cfg.Client.CacheEvictForShare(b.cfg.ShareName, &apiclient.CacheEvictRequest{L1Only: true})
	if err != nil {
		return fmt.Errorf("cache evict L1: %w", err)
	}
	return nil
}

// getL1HitRate fetches cache stats and computes the L1 hit rate.
// Returns 0 if stats are unavailable.
func (b *CacheTiersBenchmark) getL1HitRate(logf func(string, ...any)) float64 {
	stats, err := b.cfg.Client.CacheStatsForShare(b.cfg.ShareName)
	if err != nil {
		logf("  WARNING: could not fetch cache stats: %v\n", err)
		return 0
	}

	// Compute L1 hit rate from L1 entries vs total blocks.
	total := stats.Totals.BlocksTotal
	if total == 0 {
		return 0
	}

	return float64(stats.Totals.L1Entries) / float64(total) * 100
}
