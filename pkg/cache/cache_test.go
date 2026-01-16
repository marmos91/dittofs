package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache/wal"
)

// ============================================================================
// Test Helpers
// ============================================================================

// newTestCacheWithWal creates a cache with WAL persistence for testing.
func newTestCacheWithWal(t testing.TB, dir string, maxSize uint64) *Cache {
	t.Helper()
	persister, err := wal.NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister failed: %v", err)
	}
	c, err := NewWithWal(maxSize, persister)
	if err != nil {
		t.Fatalf("NewWithWal failed: %v", err)
	}
	return c
}

// ============================================================================
// WAL Integration Tests
// ============================================================================

func TestCache_WalPersistence(t *testing.T) {
	dir := t.TempDir()

	c := newTestCacheWithWal(t, dir, 0)

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("persistent data")

	if err := c.Write(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Verify WAL file exists
	walFile := filepath.Join(dir, "cache.dat")
	if _, err := os.Stat(walFile); os.IsNotExist(err) {
		t.Fatal("WAL file should exist")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen and recover
	c2 := newTestCacheWithWal(t, dir, 0)
	defer func() { _ = c2.Close() }()

	result := make([]byte, len(data))
	found, err := c2.Read(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !found {
		t.Fatal("expected to find recovered data")
	}
	if string(result) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", result, data)
	}
}

func TestCache_WalRemovePersistence(t *testing.T) {
	dir := t.TempDir()

	c := newTestCacheWithWal(t, dir, 0)

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("to be removed")

	if err := c.Write(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if err := c.Remove(ctx, payloadID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen
	c2 := newTestCacheWithWal(t, dir, 0)
	defer func() { _ = c2.Close() }()

	result := make([]byte, len(data))
	found, _ := c2.Read(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if found {
		t.Error("expected data to be removed after recovery")
	}
}

func TestCache_WalSync(t *testing.T) {
	dir := t.TempDir()

	c := newTestCacheWithWal(t, dir, 0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	if err := c.Write(ctx, "test", 0, []byte("sync test"), 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if err := c.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
}

func TestCache_WalMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	c := newTestCacheWithWal(t, dir, 0)

	ctx := context.Background()

	// Write to multiple files
	for i := 0; i < 5; i++ {
		payloadID := "file-" + string(rune('0'+i))
		data := []byte("data for file " + string(rune('0'+i)))
		if err := c.Write(ctx, payloadID, 0, data, 0); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen
	c2 := newTestCacheWithWal(t, dir, 0)
	defer func() { _ = c2.Close() }()

	files := c2.ListFiles()
	if len(files) != 5 {
		t.Errorf("expected 5 files, got %d", len(files))
	}

	// Verify data
	for i := 0; i < 5; i++ {
		payloadID := "file-" + string(rune('0'+i))
		expected := "data for file " + string(rune('0'+i))
		result := make([]byte, len(expected))
		found, err := c2.Read(ctx, payloadID, 0, 0, uint32(len(expected)), result)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if !found {
			t.Errorf("file-%d: expected to find data", i)
			continue
		}
		if string(result) != expected {
			t.Errorf("file-%d: data mismatch: got %q, want %q", i, result, expected)
		}
	}
}

// ============================================================================
// End-to-End Benchmarks
// ============================================================================

// BenchmarkE2E_FileCopy simulates a complete NFS file copy operation.
func BenchmarkE2E_FileCopy(b *testing.B) {
	fileSizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
	}

	for _, fs := range fileSizes {
		b.Run(fs.name, func(b *testing.B) {
			c := New(0)
			ctx := context.Background()

			writeSize := 32 * 1024
			totalWrites := fs.size / writeSize
			data := make([]byte, writeSize)

			b.SetBytes(int64(fs.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				payloadID := fmt.Sprintf("file-%d", i)

				// Sequential writes (file copy)
				for j := 0; j < totalWrites; j++ {
					offset := uint32(j * writeSize)
					chunkIdx := offset / ChunkSize
					offsetInChunk := offset % ChunkSize

					if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
						b.Fatal(err)
					}
				}

				// Note: No coalescing needed in block buffer model - writes go directly to blocks
			}

			b.StopTimer()
			_ = c.Close()
		})
	}
}

// BenchmarkE2E_WriteReadWrite simulates mixed read/write workload.
func BenchmarkE2E_WriteReadWrite(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "bench-file"
	data := make([]byte, 32*1024)
	dest := make([]byte, 32*1024)

	// Pre-populate
	for i := 0; i < 1024; i++ {
		offset := uint32(i * len(data))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize
		_ = c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk)
	}

	b.SetBytes(int64(len(data) * 3)) // write + read + write
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32((i * len(data)) % (32 * 1024 * 1024))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize

		// Write
		if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}

		// Read
		if _, err := c.Read(ctx, payloadID, chunkIdx, offsetInChunk, uint32(len(dest)), dest); err != nil {
			b.Fatal(err)
		}

		// Write again
		if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkE2E_ConcurrentFileCopies simulates multiple concurrent file copies.
func BenchmarkE2E_ConcurrentFileCopies(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	fileSize := 1 * 1024 * 1024 // 1MB per file
	writeSize := 32 * 1024
	totalWrites := fileSize / writeSize
	data := make([]byte, writeSize)

	b.SetBytes(int64(fileSize))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		fileNum := 0
		for pb.Next() {
			payloadID := fmt.Sprintf("file-%d-%d", b.N, fileNum)

			for j := 0; j < totalWrites; j++ {
				offset := uint32(j * writeSize)
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize

				if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}

			// Note: No coalescing needed in block buffer model

			fileNum++
		}
	})
}

// ============================================================================
// WAL vs In-Memory Comparison Benchmarks
// ============================================================================

// BenchmarkWAL_InMemory_Write measures write performance without WAL.
func BenchmarkWAL_InMemory_Write(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "bench-file"
	data := make([]byte, 32*1024)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32(i * len(data))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize
		if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWAL_Mmap_Write measures write performance with WAL persistence.
func BenchmarkWAL_Mmap_Write(b *testing.B) {
	dir := b.TempDir()
	persister, err := wal.NewMmapPersister(dir)
	if err != nil {
		b.Fatal(err)
	}
	c, err := NewWithWal(0, persister)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "bench-file"
	data := make([]byte, 32*1024)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32(i * len(data))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize
		if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWAL_FileCopy_Comparison compares file copy with/without WAL.
func BenchmarkWAL_FileCopy_Comparison(b *testing.B) {
	fileSize := 10 * 1024 * 1024 // 10MB
	writeSize := 32 * 1024
	totalWrites := fileSize / writeSize
	data := make([]byte, writeSize)

	b.Run("InMemory", func(b *testing.B) {
		c := New(0)
		ctx := context.Background()

		b.SetBytes(int64(fileSize))
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			payloadID := fmt.Sprintf("file-%d", i)
			for j := 0; j < totalWrites; j++ {
				offset := uint32(j * writeSize)
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize
				if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}
			// Note: No coalescing needed in block buffer model
		}
		_ = c.Close()
	})

	b.Run("WithWAL", func(b *testing.B) {
		dir := b.TempDir()
		persister, err := wal.NewMmapPersister(dir)
		if err != nil {
			b.Fatal(err)
		}
		c, err := NewWithWal(0, persister)
		if err != nil {
			b.Fatal(err)
		}
		ctx := context.Background()

		b.SetBytes(int64(fileSize))
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			payloadID := fmt.Sprintf("file-%d", i)
			for j := 0; j < totalWrites; j++ {
				offset := uint32(j * writeSize)
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize
				if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}
			// Note: No coalescing needed in block buffer model
		}
		_ = c.Close()
	})
}
