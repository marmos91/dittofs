package cache

import (
	"context"
	"fmt"
	"testing"
)

func TestWriteSlice_Basic(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("hello world")

	err := c.WriteSlice(ctx, payloadID, 0, data, 0)
	if err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Verify data was written
	result := make([]byte, len(data))
	found, err := c.ReadSlice(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if !found {
		t.Fatal("expected to find data")
	}
	if string(result) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", result, data)
	}
}

func TestWriteSlice_SequentialOptimization(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write sequential chunks (simulating NFS 32KB writes)
	for i := 0; i < 10; i++ {
		data := make([]byte, 1024)
		offset := uint32(i * 1024)
		if err := c.WriteSlice(ctx, payloadID, 0, data, offset); err != nil {
			t.Fatalf("WriteSlice failed: %v", err)
		}
	}

	// Get dirty slices - should be coalesced into 1 due to sequential optimization
	slices, err := c.GetDirtySlices(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}
	if len(slices) != 1 {
		t.Errorf("expected 1 coalesced slice, got %d", len(slices))
	}
	if slices[0].Length != 10*1024 {
		t.Errorf("expected length 10240, got %d", slices[0].Length)
	}
}

func TestWriteSlice_Prepend(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write at offset 100 first
	data1 := []byte("WORLD")
	_ = c.WriteSlice(ctx, payloadID, 0, data1, 100)

	// Prepend at offset 95 (ends where previous starts)
	data2 := []byte("HELLO")
	_ = c.WriteSlice(ctx, payloadID, 0, data2, 95)

	// Should be coalesced into one slice by tryExtendAdjacentSlice
	slices, _ := c.GetDirtySlices(ctx, payloadID)
	if len(slices) != 1 {
		t.Errorf("expected 1 coalesced slice after prepend, got %d", len(slices))
	}
	if slices[0].Length != 10 {
		t.Errorf("expected length 10, got %d", slices[0].Length)
	}
}

func TestWriteSlice_InvalidOffset(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Try to write past chunk boundary
	data := make([]byte, 100)
	err := c.WriteSlice(ctx, payloadID, 0, data, ChunkSize-50)
	if err != ErrInvalidOffset {
		t.Errorf("expected ErrInvalidOffset, got %v", err)
	}
}

func TestWriteSlice_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.WriteSlice(ctx, "test", 0, []byte("data"), 0)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestWriteSlice_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Close()

	err := c.WriteSlice(context.Background(), "test", 0, []byte("data"), 0)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

func TestWriteSlice_MultipleChunks(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write to different chunks
	_ = c.WriteSlice(ctx, payloadID, 0, []byte("chunk0"), 0)
	_ = c.WriteSlice(ctx, payloadID, 1, []byte("chunk1"), 0)
	_ = c.WriteSlice(ctx, payloadID, 2, []byte("chunk2"), 0)

	slices, _ := c.GetDirtySlices(ctx, payloadID)
	if len(slices) != 3 {
		t.Errorf("expected 3 slices (one per chunk), got %d", len(slices))
	}

	// Verify sorted by chunk index
	for i, s := range slices {
		if s.ChunkIndex != uint32(i) {
			t.Errorf("slice[%d].ChunkIndex = %d, want %d", i, s.ChunkIndex, i)
		}
	}
}

// ============================================================================
// Write Benchmarks
// ============================================================================

// BenchmarkWriteSlice_Sequential measures sequential write performance.
// This is the critical path for NFS file copies - must achieve >3 GB/s.
func BenchmarkWriteSlice_Sequential(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"4KB", 4 * 1024},
		{"16KB", 16 * 1024},
		{"32KB", 32 * 1024},   // Typical NFS write size
		{"64KB", 64 * 1024},   // Large NFS write
		{"128KB", 128 * 1024}, // Maximum NFS write
	}

	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()
			payloadID := "bench-file"
			data := make([]byte, s.size)
			for i := range data {
				data[i] = byte(i % 256)
			}

			b.SetBytes(int64(s.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				offset := uint32(i * s.size)
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize

				if err := c.WriteSlice(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWriteSlice_SequentialExtend measures the sequential write optimization.
// Tests how well adjacent writes are coalesced into single slices.
func BenchmarkWriteSlice_SequentialExtend(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "bench-file"
	data := make([]byte, 32*1024) // 32KB writes

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	// All writes should extend the same slice (sequential optimization)
	for i := 0; i < b.N; i++ {
		offset := uint32(i * len(data))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize

		if err := c.WriteSlice(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()

	// Report slice count (should be ~1 per chunk due to sequential optimization)
	stats := c.Stats()
	b.ReportMetric(float64(stats.SliceCount), "slices")
}

// BenchmarkWriteSlice_Random measures random write performance.
// Simulates database workloads with scattered writes.
func BenchmarkWriteSlice_Random(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "bench-file"
	dataSize := 4 * 1024 // 4KB writes
	data := make([]byte, dataSize)

	// Max offset within chunk to ensure data fits
	maxOffsetInChunk := ChunkSize - uint32(dataSize)

	b.SetBytes(int64(dataSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Pseudo-random chunk and offset
		chunkIdx := uint32((i * 7919) % 1000) // Spread across 1000 chunks
		offsetInChunk := uint32((i * 7907) % int(maxOffsetInChunk))

		if err := c.WriteSlice(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteSlice_MultiFile measures writes across multiple files.
// Simulates multiple concurrent file operations.
func BenchmarkWriteSlice_MultiFile(b *testing.B) {
	fileCounts := []int{10, 100, 1000}

	for _, fileCount := range fileCounts {
		b.Run(fmt.Sprintf("files=%d", fileCount), func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()
			data := make([]byte, 32*1024)

			b.SetBytes(int64(len(data)))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				payloadID := fmt.Sprintf("file-%d", i%fileCount)
				offset := uint32((i / fileCount) * len(data))
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize

				if err := c.WriteSlice(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWriteSlice_Concurrent measures concurrent write throughput.
// Tests lock contention under parallel access.
func BenchmarkWriteSlice_Concurrent(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, 32*1024)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			payloadID := fmt.Sprintf("file-%d", i%100)
			offset := uint32((i / 100) * len(data))
			chunkIdx := offset / ChunkSize
			offsetInChunk := offset % ChunkSize

			if err := c.WriteSlice(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// BenchmarkMemory_SliceAllocation measures slice allocation overhead.
func BenchmarkMemory_SliceAllocation(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, 32*1024)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("file-%d", i)
		if err := c.WriteSlice(ctx, payloadID, 0, data, 0); err != nil {
			b.Fatal(err)
		}
	}
}
