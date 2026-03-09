package bench

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// closeFiles closes all non-nil files, ignoring errors. Intended for use with defer.
func closeFiles(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// runRandWrite performs random writes of BlockSize bytes for Duration.
// Pre-creates one file per thread, then seeks randomly and writes.
func runRandWrite(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	// Pre-create files filled with zeros.
	files := make([]*os.File, cfg.Threads)
	for t := range cfg.Threads {
		fname := filepath.Join(dir, fmt.Sprintf("rand_write_%d.dat", t))
		f, err := os.Create(fname)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", fname, err)
		}
		if err := f.Truncate(cfg.FileSize); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("truncate %s: %w", fname, err)
		}
		files[t] = f
	}
	defer closeFiles(files)

	buf := make([]byte, cfg.BlockSize)
	for i := range buf {
		buf[i] = byte(i)
	}

	maxOffset := max(cfg.FileSize-cfg.BlockSize, 0)

	var (
		latencies  = make([]time.Duration, 0, 1024)
		totalBytes int64
		errors     int64
	)

	start := time.Now()
	deadline := start.Add(cfg.Duration)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		for _, f := range files {
			if time.Now().After(deadline) {
				break
			}
			offset := rand.Int64N(maxOffset + 1)

			opStart := time.Now()
			n, err := f.WriteAt(buf, offset)
			lat := time.Since(opStart)

			latencies = append(latencies, lat)
			totalBytes += int64(n)
			if err != nil {
				errors++
			}
		}

		if progress != nil {
			elapsed := time.Since(start).Seconds()
			progress(RandWrite, elapsed/cfg.Duration.Seconds())
		}
	}

	elapsed := time.Since(start)

	wr := &WorkloadResult{
		Workload:   RandWrite,
		IOPS:       float64(len(latencies)) / elapsed.Seconds(),
		TotalOps:   int64(len(latencies)),
		TotalBytes: totalBytes,
		Errors:     errors,
		Duration:   elapsed,
	}
	applyLatencyStats(wr, latencies)

	return wr, nil
}

// runRandRead performs random reads of BlockSize bytes for Duration.
// Uses files created by runRandWrite.
func runRandRead(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	files := make([]*os.File, cfg.Threads)
	for t := range cfg.Threads {
		fname := filepath.Join(dir, fmt.Sprintf("rand_write_%d.dat", t))
		f, err := os.Open(fname)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", fname, err)
		}
		disableCache(f)
		files[t] = f
	}
	defer closeFiles(files)

	buf := make([]byte, cfg.BlockSize)
	maxOffset := max(cfg.FileSize-cfg.BlockSize, 0)

	var (
		latencies  = make([]time.Duration, 0, 1024)
		totalBytes int64
		errors     int64
	)

	start := time.Now()
	deadline := start.Add(cfg.Duration)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		for _, f := range files {
			if time.Now().After(deadline) {
				break
			}
			offset := rand.Int64N(maxOffset + 1)

			opStart := time.Now()
			n, err := f.ReadAt(buf, offset)
			lat := time.Since(opStart)

			latencies = append(latencies, lat)
			totalBytes += int64(n)
			if err != nil {
				errors++
			}
		}

		if progress != nil {
			elapsed := time.Since(start).Seconds()
			progress(RandRead, elapsed/cfg.Duration.Seconds())
		}
	}

	elapsed := time.Since(start)

	wr := &WorkloadResult{
		Workload:   RandRead,
		IOPS:       float64(len(latencies)) / elapsed.Seconds(),
		TotalOps:   int64(len(latencies)),
		TotalBytes: totalBytes,
		Errors:     errors,
		Duration:   elapsed,
	}
	applyLatencyStats(wr, latencies)

	return wr, nil
}
