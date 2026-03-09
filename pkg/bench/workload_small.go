package bench

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// smallFileSizeMin and smallFileSizeMax define the range of random file sizes
// for the small-files workload. Each file gets a random size in [1KB, 32KB].
const (
	smallFileSizeMin = 1 << 10  // 1 KiB
	smallFileSizeMax = 32 << 10 // 32 KiB
)

// runSmallFiles measures bulk small file creation, read, and deletion throughput.
// Creates SmallFileCount files with random sizes (1-32 KB), reads them back,
// then deletes them. Each operation (create, read, delete) is counted separately.
// Measures combined ops/sec and latency across all phases.
func runSmallFiles(ctx context.Context, cfg Config, dir string, progress ProgressFunc) (*WorkloadResult, error) {
	smallDir := filepath.Join(dir, "small_files")
	if err := os.MkdirAll(smallDir, 0o755); err != nil {
		return nil, fmt.Errorf("create small files dir: %w", err)
	}

	count := cfg.SmallFileCount
	if count == 0 {
		count = 10000
	}

	// Generate random file contents upfront to avoid measuring RNG time.
	// Use a shared buffer and slice it for each file.
	maxBuf := make([]byte, smallFileSizeMax)
	if _, err := rand.Read(maxBuf); err != nil {
		return nil, fmt.Errorf("generate random data: %w", err)
	}

	// Determine file sizes (random in [min, max]).
	sizes := make([]int, count)
	for i := range sizes {
		// Simple deterministic size distribution based on index.
		sizes[i] = smallFileSizeMin + (i*(smallFileSizeMax-smallFileSizeMin))/count
	}

	// Create subdirectories to avoid single-directory bottleneck.
	// 100 files per subdirectory.
	numDirs := (count + 99) / 100
	for d := range numDirs {
		subDir := filepath.Join(smallDir, fmt.Sprintf("d%04d", d))
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			return nil, fmt.Errorf("create subdir: %w", err)
		}
	}

	filePath := func(i int) string {
		return filepath.Join(smallDir, fmt.Sprintf("d%04d", i/100), fmt.Sprintf("f%06d", i))
	}

	totalPhases := 4 // create, read, stat, delete
	var (
		mu         sync.Mutex
		latencies  = make([]time.Duration, 0, count*totalPhases)
		totalOps   int64
		totalBytes int64
		errors     int64
	)

	start := time.Now()

	// Phase 1: Create files (parallel with cfg.Threads goroutines).
	if err := parallelFor(ctx, cfg.Threads, count, func(i int) {
		path := filePath(i)
		data := maxBuf[:sizes[i]]

		opStart := time.Now()
		err := os.WriteFile(path, data, 0o644)
		lat := time.Since(opStart)

		mu.Lock()
		latencies = append(latencies, lat)
		totalOps++
		totalBytes += int64(len(data))
		if err != nil {
			errors++
		}
		mu.Unlock()

		if progress != nil && i%500 == 0 {
			progress(SmallFiles, float64(i)/float64(count*totalPhases))
		}
	}); err != nil {
		return nil, err
	}

	// Phase 2: Read all files back.
	if err := parallelFor(ctx, cfg.Threads, count, func(i int) {
		path := filePath(i)

		opStart := time.Now()
		data, err := os.ReadFile(path)
		lat := time.Since(opStart)

		mu.Lock()
		latencies = append(latencies, lat)
		totalOps++
		totalBytes += int64(len(data))
		if err != nil {
			errors++
		}
		mu.Unlock()

		if progress != nil && i%500 == 0 {
			pct := float64(count+i) / float64(count*totalPhases)
			progress(SmallFiles, pct)
		}
	}); err != nil {
		return nil, err
	}

	// Phase 3: Stat all files.
	if err := parallelFor(ctx, cfg.Threads, count, func(i int) {
		path := filePath(i)

		opStart := time.Now()
		_, err := os.Stat(path)
		lat := time.Since(opStart)

		mu.Lock()
		latencies = append(latencies, lat)
		totalOps++
		if err != nil {
			errors++
		}
		mu.Unlock()

		if progress != nil && i%500 == 0 {
			pct := float64(2*count+i) / float64(count*totalPhases)
			progress(SmallFiles, pct)
		}
	}); err != nil {
		return nil, err
	}

	// Phase 4: Delete all files.
	if err := parallelFor(ctx, cfg.Threads, count, func(i int) {
		path := filePath(i)

		opStart := time.Now()
		err := os.Remove(path)
		lat := time.Since(opStart)

		mu.Lock()
		latencies = append(latencies, lat)
		totalOps++
		if err != nil {
			errors++
		}
		mu.Unlock()

		if progress != nil && i%500 == 0 {
			pct := float64(3*count+i) / float64(count*totalPhases)
			progress(SmallFiles, pct)
		}
	}); err != nil {
		return nil, err
	}

	// Clean up subdirectories.
	os.RemoveAll(smallDir) //nolint:errcheck

	elapsed := time.Since(start)
	stats := computePercentiles(latencies)

	return &WorkloadResult{
		Workload:     SmallFiles,
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

// parallelFor runs fn(i) for i in [0, count) using nWorkers goroutines.
func parallelFor(ctx context.Context, nWorkers, count int, fn func(i int)) error {
	ch := make(chan int, nWorkers*2)
	var wg sync.WaitGroup

	// Spawn workers.
	for range nWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				fn(i)
			}
		}()
	}

	// Feed work items.
	for i := range count {
		select {
		case <-ctx.Done():
			close(ch)
			wg.Wait()
			return ctx.Err()
		case ch <- i:
		}
	}
	close(ch)
	wg.Wait()
	return nil
}
