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

// DefaultStorageTiersSizes are the file sizes benchmarked by default: 10 MB, 100 MB, 1 GB.
var DefaultStorageTiersSizes = []int64{
	10 << 20,  // 10 MiB
	100 << 20, // 100 MiB
	1 << 30,   // 1 GiB
}

// StorageTiersConfig holds configuration for the storage-tiers benchmark.
type StorageTiersConfig struct {
	// MountPoint is the NFS/SMB mount path for file I/O.
	MountPoint string

	// ShareName is the DittoFS share name for block store API calls.
	ShareName string

	// Client is an authenticated API client for block store eviction/stats.
	Client *apiclient.Client

	// FileSizes are the file sizes to benchmark (default: 10 MB, 100 MB, 1 GB).
	FileSizes []int64
}

// StorageTiersResult holds the complete storage-tiers benchmark output.
type StorageTiersResult struct {
	// Timestamp is when the benchmark started.
	Timestamp time.Time `json:"timestamp"`

	// ShareName is the share that was benchmarked.
	ShareName string `json:"share_name"`

	// Results contains per-size benchmark results.
	Results []StorageTiersSizeResult `json:"results"`

	// TotalDuration is the wall-clock time for the entire benchmark.
	TotalDuration time.Duration `json:"total_duration"`
}

// StorageTiersSizeResult holds results for a single file size.
type StorageTiersSizeResult struct {
	FileSize       int64   `json:"file_size"`
	WriteStats     IOStats `json:"write_stats"`
	ColdReadStats  IOStats `json:"cold_read_stats"`
	WarmReadStats  IOStats `json:"warm_read_stats"`
	LocalOnlyStats IOStats `json:"local_only_stats"`
}

// IOStats holds throughput and storage metrics for a single I/O step.
type IOStats struct {
	ThroughputMBps float64 `json:"throughput_mbps"`
	DurationMs     int64   `json:"duration_ms"`
	L1HitRate      float64 `json:"l1_hit_rate"` // -1 means not applicable
}

// StorageTiersBenchmark runs the 6-step storage tier benchmark.
type StorageTiersBenchmark struct {
	cfg StorageTiersConfig
}

// NewStorageTiersBenchmark creates a new storage-tiers benchmark runner.
func NewStorageTiersBenchmark(cfg StorageTiersConfig) *StorageTiersBenchmark {
	if len(cfg.FileSizes) == 0 {
		cfg.FileSizes = DefaultStorageTiersSizes
	}
	return &StorageTiersBenchmark{cfg: cfg}
}

// Sizes returns the resolved file sizes (after applying defaults).
func (b *StorageTiersBenchmark) Sizes() []int64 { return b.cfg.FileSizes }

// Run executes the 6-step storage-tiers benchmark for each file size.
func (b *StorageTiersBenchmark) Run(ctx context.Context, logf func(string, ...any)) (*StorageTiersResult, error) {
	result := &StorageTiersResult{
		Timestamp: time.Now().UTC(),
		ShareName: b.cfg.ShareName,
		Results:   make([]StorageTiersSizeResult, 0, len(b.cfg.FileSizes)),
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
func (b *StorageTiersBenchmark) runForSize(ctx context.Context, fileSize int64, logf func(string, ...any)) (*StorageTiersSizeResult, error) {
	sizeLabel := FormatSize(fileSize)

	dir := filepath.Join(b.cfg.MountPoint, benchDir, fmt.Sprintf("storage_tiers_%d", fileSize))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create bench dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	testFile := filepath.Join(dir, "testfile.dat")

	result := &StorageTiersSizeResult{
		FileSize: fileSize,
	}

	// Step 1: Write
	logf("  %s: Step 1/6 - Write...\n", sizeLabel)
	writeStats, err := b.writeFile(testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	result.WriteStats = *writeStats

	// Step 2: Evict all block store data
	logf("  %s: Step 2/6 - Evict all block store data...\n", sizeLabel)
	if err := b.evictAll(ctx); err != nil {
		logf("  WARNING: evict-all failed: %v (cold read may not be accurate)\n", err)
	}

	// Step 3: Cold read (data from remote store)
	logf("  %s: Step 3/6 - Cold read...\n", sizeLabel)
	coldStats, err := b.readFile(testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("cold read: %w", err)
	}
	coldStats.L1HitRate = b.getReadBufferHitRate(logf)
	result.ColdReadStats = *coldStats

	// Step 4: Warm read (data in read buffer + local store)
	logf("  %s: Step 4/6 - Warm read...\n", sizeLabel)
	warmStats, err := b.readFile(testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("warm read: %w", err)
	}
	warmStats.L1HitRate = b.getReadBufferHitRate(logf)
	result.WarmReadStats = *warmStats

	// Step 5: Evict read buffer only (keep local FS blocks)
	logf("  %s: Step 5/6 - Evict read buffer only...\n", sizeLabel)
	if err := b.evictReadBuffer(ctx); err != nil {
		logf("  WARNING: read-buffer evict failed: %v (local-only read may not be accurate)\n", err)
	}

	// Step 6: Local-only read (data from local FS store, not read buffer)
	logf("  %s: Step 6/6 - Local-only read...\n", sizeLabel)
	localOnlyStats, err := b.readFile(testFile, fileSize)
	if err != nil {
		return nil, fmt.Errorf("local-only read: %w", err)
	}
	localOnlyStats.L1HitRate = b.getReadBufferHitRate(logf)
	result.LocalOnlyStats = *localOnlyStats

	return result, nil
}

// writeFile writes a test file of the given size and returns I/O stats.
func (b *StorageTiersBenchmark) writeFile(path string, size int64) (*IOStats, error) {
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
		n := min(size-written, int64(len(buf)))
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
		L1HitRate:      -1,
	}, nil
}

// readFile reads an entire file and returns I/O stats.
func (b *StorageTiersBenchmark) readFile(path string, size int64) (*IOStats, error) {
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
	}, nil
}

// evictBlockStore evicts block store data for the share with the given options.
// Returns ctx.Err() if the context is already cancelled.
func (b *StorageTiersBenchmark) evictBlockStore(ctx context.Context, req *apiclient.BlockStoreEvictOptions, label string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := b.cfg.Client.BlockStoreEvictForShare(b.cfg.ShareName, req)
	if err != nil {
		return fmt.Errorf("block store evict %s: %w", label, err)
	}
	return nil
}

// evictAll evicts both read buffer and local disk data for the share.
func (b *StorageTiersBenchmark) evictAll(ctx context.Context) error {
	return b.evictBlockStore(ctx, &apiclient.BlockStoreEvictOptions{}, "all")
}

// evictReadBuffer evicts only the read buffer (memory) for the share.
func (b *StorageTiersBenchmark) evictReadBuffer(ctx context.Context) error {
	return b.evictBlockStore(ctx, &apiclient.BlockStoreEvictOptions{ReadBufferOnly: true}, "read-buffer")
}

// getReadBufferHitRate fetches block store stats and computes the read buffer hit rate.
// Returns 0 if stats are unavailable.
func (b *StorageTiersBenchmark) getReadBufferHitRate(logf func(string, ...any)) float64 {
	stats, err := b.cfg.Client.BlockStoreStatsForShare(b.cfg.ShareName)
	if err != nil {
		logf("  WARNING: could not fetch block store stats: %v\n", err)
		return 0
	}

	total := stats.Totals.BlocksTotal
	if total == 0 {
		return 0
	}

	return float64(stats.Totals.ReadBufferEntries) / float64(total) * 100
}
