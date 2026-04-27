//go:build linux || darwin

// Phase 12 perf gate (D-43) — unix-only mmap hot-path supporting
// gate. Asserts that the readFromCAS mmap path (Plan 12-10) does not
// regress vs the os.ReadFile baseline on warm files. Mmap is supposed
// to be at-least-as-fast as ReadFile for >= 64 KiB chunks; if the
// mmap path becomes >5% slower we want to localise the regression
// here rather than have it surface as a flaky rand-read gate.
package engine

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPerfGate_Phase12_MmapHotPath measures readFromCAS (mmap on
// linux/darwin) vs os.ReadFile across a warm tempdir of 4 MiB CAS
// chunks. Asserts mmap_throughput >= 0.95 × ReadFile_throughput so a
// regression in cache_mmap_unix.go's mmap path is caught at the perf
// gate layer rather than purely the rand-read gate.
//
// The bench is read-warm (the tempdir is freshly written so the page
// cache is hot) — we are isolating the mmap codepath cost, not disk
// I/O. Trial count is small (32 chunks × 4 MiB) so the test stays
// under a second on dev hardware.
func TestPerfGate_Phase12_MmapHotPath(t *testing.T) {
	if testing.Short() {
		t.Skip("Phase 12 D-43 mmap hot-path supporting gate; skip under -short")
	}

	const chunkCount = 32
	const chunkSize = 4 * 1024 * 1024
	dir := t.TempDir()

	paths := make([]string, chunkCount)
	buf := make([]byte, chunkSize)
	for i := 0; i < chunkCount; i++ {
		if _, err := rand.Read(buf); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		p := filepath.Join(dir, formatChunkName(i))
		if err := os.WriteFile(p, buf, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		paths[i] = p
	}

	// Warm the page cache once via os.ReadFile so both paths start
	// from the same cache state.
	for _, p := range paths {
		if _, err := os.ReadFile(p); err != nil {
			t.Fatalf("warm ReadFile: %v", err)
		}
	}

	dest := make([]byte, chunkSize)
	const reps = 16

	// ReadFile baseline.
	startBaseline := time.Now()
	for r := 0; r < reps; r++ {
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			copy(dest, b)
		}
	}
	baseline := time.Since(startBaseline)

	// Mmap path (readFromCAS unix variant).
	startMmap := time.Now()
	for r := 0; r < reps; r++ {
		for _, p := range paths {
			if _, err := readFromCAS(p, 0, dest); err != nil {
				t.Fatalf("readFromCAS: %v", err)
			}
		}
	}
	mmap := time.Since(startMmap)

	baselineThroughput := float64(reps*chunkCount*chunkSize) / baseline.Seconds()
	mmapThroughput := float64(reps*chunkCount*chunkSize) / mmap.Seconds()
	ratio := mmapThroughput / baselineThroughput

	t.Logf("Phase 12 mmap hot-path: ReadFile=%.0f MB/s mmap=%.0f MB/s ratio=%.2f (limit >= 0.95)",
		baselineThroughput/(1<<20), mmapThroughput/(1<<20), ratio)

	if ratio < 0.95 {
		t.Fatalf("D-43 supporting gate FAILED: mmap throughput %.2f× ReadFile (< 0.95× — likely regression in cache_mmap_unix.go)", ratio)
	}
}

func formatChunkName(i int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for j := 0; j < 8; j++ {
		out[7-j] = hex[(i>>(j*4))&0xf]
	}
	return string(out) + ".cas"
}
