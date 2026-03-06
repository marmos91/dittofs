package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const seqChunkSize = 1 << 20 // 1 MiB per write/read operation

// runSeqWrite writes FileSize bytes per thread in seqChunkSize chunks.
// Returns a WorkloadResult with throughput and latency stats.
func runSeqWrite(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	chunks := int(cfg.FileSize / seqChunkSize)
	if chunks == 0 {
		chunks = 1
	}
	totalOps := int64(chunks * cfg.Threads)

	buf := make([]byte, seqChunkSize)
	// Fill with a non-zero pattern to avoid sparse file optimizations.
	for i := range buf {
		buf[i] = byte(i)
	}

	var (
		latencies  = make([]time.Duration, 0, totalOps)
		totalBytes int64
		errors     int64
	)

	start := time.Now()

	for t := range cfg.Threads {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		fname := filepath.Join(dir, fmt.Sprintf("seq_write_%d.dat", t))
		f, err := os.Create(fname)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", fname, err)
		}

		for c := range chunks {
			writeSize := seqChunkSize
			if remaining := cfg.FileSize - int64(c)*seqChunkSize; remaining < seqChunkSize {
				writeSize = int(remaining)
			}

			opStart := time.Now()
			n, err := f.Write(buf[:writeSize])
			lat := time.Since(opStart)

			latencies = append(latencies, lat)
			totalBytes += int64(n)
			if err != nil {
				errors++
			}

			if progress != nil {
				done := int64(t*chunks+c+1) * int64(cfg.Threads)
				progress(SeqWrite, float64(done)/float64(totalOps))
			}
		}

		if err := f.Sync(); err != nil {
			errors++
		}
		_ = f.Close()
	}

	elapsed := time.Since(start)
	stats := computePercentiles(latencies)

	return &WorkloadResult{
		Workload:       SeqWrite,
		ThroughputMBps: float64(totalBytes) / elapsed.Seconds() / (1 << 20),
		LatencyP50Us:   stats.P50,
		LatencyP95Us:   stats.P95,
		LatencyP99Us:   stats.P99,
		LatencyAvgUs:   stats.Avg,
		TotalOps:       int64(len(latencies)),
		TotalBytes:     totalBytes,
		Errors:         errors,
		Duration:       elapsed,
	}, nil
}

// runSeqRead reads back the files created by runSeqWrite.
// Files must already exist in dir.
func runSeqRead(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	chunks := int(cfg.FileSize / seqChunkSize)
	if chunks == 0 {
		chunks = 1
	}
	totalOps := int64(chunks * cfg.Threads)

	buf := make([]byte, seqChunkSize)

	var (
		latencies  = make([]time.Duration, 0, totalOps)
		totalBytes int64
		errors     int64
	)

	start := time.Now()

	for t := range cfg.Threads {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		fname := filepath.Join(dir, fmt.Sprintf("seq_write_%d.dat", t))
		f, err := os.Open(fname)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", fname, err)
		}

		// Try to bypass OS cache on macOS (F_NOCACHE).
		disableCache(f)

		for c := range chunks {
			readSize := seqChunkSize
			if remaining := cfg.FileSize - int64(c)*seqChunkSize; remaining < seqChunkSize {
				readSize = int(remaining)
			}

			opStart := time.Now()
			n, err := f.Read(buf[:readSize])
			lat := time.Since(opStart)

			latencies = append(latencies, lat)
			totalBytes += int64(n)
			if err != nil {
				errors++
			}

			if progress != nil {
				done := int64(t*chunks+c+1) * int64(cfg.Threads)
				progress(SeqRead, float64(done)/float64(totalOps))
			}
		}

		_ = f.Close()
	}

	elapsed := time.Since(start)
	stats := computePercentiles(latencies)

	return &WorkloadResult{
		Workload:       SeqRead,
		ThroughputMBps: float64(totalBytes) / elapsed.Seconds() / (1 << 20),
		LatencyP50Us:   stats.P50,
		LatencyP95Us:   stats.P95,
		LatencyP99Us:   stats.P99,
		LatencyAvgUs:   stats.Avg,
		TotalOps:       int64(len(latencies)),
		TotalBytes:     totalBytes,
		Errors:         errors,
		Duration:       elapsed,
	}, nil
}
