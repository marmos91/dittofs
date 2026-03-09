package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const seqChunkSize = 1 << 20 // 1 MiB per write/read operation

// seqChunkCount returns the number of seqChunkSize chunks needed for fileSize.
// Uses ceiling division so non-multiple sizes are fully covered.
func seqChunkCount(fileSize int64) int {
	if fileSize <= 0 {
		return 1
	}
	return int((fileSize + seqChunkSize - 1) / seqChunkSize)
}

// chunkBytes returns the byte count for chunk c of a file with the given total size.
// The last chunk may be smaller than seqChunkSize.
func chunkBytes(c int, fileSize int64) int {
	remaining := fileSize - int64(c)*seqChunkSize
	if remaining < seqChunkSize {
		return int(remaining)
	}
	return seqChunkSize
}

// runSeqWrite writes FileSize bytes per thread in seqChunkSize chunks.
// Returns a WorkloadResult with throughput and latency stats.
func runSeqWrite(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	chunks := seqChunkCount(cfg.FileSize)
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
			writeSize := chunkBytes(c, cfg.FileSize)

			opStart := time.Now()
			written := 0
			for written < writeSize {
				n, err := f.Write(buf[written:writeSize])
				written += n
				if err != nil {
					errors++
					break
				}
			}
			lat := time.Since(opStart)

			latencies = append(latencies, lat)
			totalBytes += int64(written)

			if progress != nil {
				done := int64(t*chunks + c + 1)
				progress(SeqWrite, float64(done)/float64(totalOps))
			}
		}

		if err := f.Sync(); err != nil {
			errors++
		}
		if err := f.Close(); err != nil {
			errors++
		}
	}

	elapsed := time.Since(start)

	wr := &WorkloadResult{
		Workload:       SeqWrite,
		ThroughputMBps: float64(totalBytes) / elapsed.Seconds() / (1 << 20),
		TotalOps:       int64(len(latencies)),
		TotalBytes:     totalBytes,
		Errors:         errors,
		Duration:       elapsed,
	}
	applyLatencyStats(wr, latencies)

	return wr, nil
}

// runSeqRead reads back the files created by runSeqWrite.
// Files must already exist in dir.
func runSeqRead(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	chunks := seqChunkCount(cfg.FileSize)
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

		disableCache(f)

		for c := range chunks {
			readSize := chunkBytes(c, cfg.FileSize)

			opStart := time.Now()
			n, err := io.ReadFull(f, buf[:readSize])
			lat := time.Since(opStart)

			latencies = append(latencies, lat)
			totalBytes += int64(n)
			if err != nil {
				errors++
			}

			if progress != nil {
				done := int64(t*chunks + c + 1)
				progress(SeqRead, float64(done)/float64(totalOps))
			}
		}

		_ = f.Close()
	}

	elapsed := time.Since(start)

	wr := &WorkloadResult{
		Workload:       SeqRead,
		ThroughputMBps: float64(totalBytes) / elapsed.Seconds() / (1 << 20),
		TotalOps:       int64(len(latencies)),
		TotalBytes:     totalBytes,
		Errors:         errors,
		Duration:       elapsed,
	}
	applyLatencyStats(wr, latencies)

	return wr, nil
}
