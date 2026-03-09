package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const metaFileContent = "dittofs-bench-metadata-test-file\n" // 33 bytes

// runMetadata measures metadata operation throughput: create, stat, delete.
// Creates MetaFiles small files, stats them all, then deletes them all.
// Reports combined ops/sec and per-phase latency.
func runMetadata(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	metaDir := filepath.Join(dir, "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return nil, fmt.Errorf("create meta dir: %w", err)
	}

	data := []byte(metaFileContent)
	names := make([]string, cfg.MetaFiles)
	for i := range names {
		names[i] = filepath.Join(metaDir, fmt.Sprintf("file_%06d", i))
	}

	totalPhases := 3
	var (
		latencies  = make([]time.Duration, 0, cfg.MetaFiles*totalPhases)
		totalOps   int64
		totalBytes int64
		errors     int64
	)

	start := time.Now()

	// Phase 1: Create files.
	for i, name := range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		opStart := time.Now()
		err := os.WriteFile(name, data, 0o644)
		lat := time.Since(opStart)

		latencies = append(latencies, lat)
		totalOps++
		totalBytes += int64(len(data))
		if err != nil {
			errors++
		}

		if progress != nil && i%100 == 0 {
			progress(Metadata, float64(i)/float64(cfg.MetaFiles*totalPhases))
		}
	}

	// Phase 2: Stat all files.
	for i, name := range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		opStart := time.Now()
		_, err := os.Stat(name)
		lat := time.Since(opStart)

		latencies = append(latencies, lat)
		totalOps++
		if err != nil {
			errors++
		}

		if progress != nil && i%100 == 0 {
			pct := float64(cfg.MetaFiles+i) / float64(cfg.MetaFiles*totalPhases)
			progress(Metadata, pct)
		}
	}

	// Phase 3: Delete all files.
	for i, name := range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		opStart := time.Now()
		err := os.Remove(name)
		lat := time.Since(opStart)

		latencies = append(latencies, lat)
		totalOps++
		if err != nil {
			errors++
		}

		if progress != nil && i%100 == 0 {
			pct := float64(2*cfg.MetaFiles+i) / float64(cfg.MetaFiles*totalPhases)
			progress(Metadata, pct)
		}
	}

	// Clean up the metadata directory.
	os.Remove(metaDir) //nolint:errcheck

	elapsed := time.Since(start)
	stats := computePercentiles(latencies)

	return &WorkloadResult{
		Workload:     Metadata,
		OpsPerSec:    float64(totalOps) / elapsed.Seconds(),
		LatencyP50Us: stats.P50,
		LatencyP95Us: stats.P95,
		LatencyP99Us: stats.P99,
		LatencyAvgUs: stats.Avg,
		TotalOps:     totalOps,
		TotalBytes:   totalBytes,
		Errors:       errors,
		Duration:     elapsed,
	}, nil
}
