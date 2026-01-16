package cache

import (
	"context"
	"fmt"
	"testing"
)

func TestWrite_Basic(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("hello world")

	err := c.Write(ctx, payloadID, 0, data, 0)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Verify data was written
	result := make([]byte, len(data))
	found, err := c.Read(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !found {
		t.Fatal("expected to find data")
	}
	if string(result) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", result, data)
	}
}

func TestWrite_SequentialToSameBlock(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write sequential chunks (simulating NFS 32KB writes)
	// All writes go to the same block since 10KB < 4MB block size
	for i := 0; i < 10; i++ {
		data := make([]byte, 1024)
		offset := uint32(i * 1024)
		if err := c.Write(ctx, payloadID, 0, data, offset); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Get dirty blocks - should be 1 block since all writes fit in one block
	blocks, err := c.GetDirtyBlocks(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetDirtyBlocks failed: %v", err)
	}
	if len(blocks) != 1 {
		t.Errorf("expected 1 block, got %d", len(blocks))
	}
	// DataSize should be 10KB
	if blocks[0].DataSize != 10*1024 {
		t.Errorf("expected dataSize 10240, got %d", blocks[0].DataSize)
	}
}

func TestWrite_AdjacentWrites(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write at offset 100 first
	data1 := []byte("WORLD")
	_ = c.Write(ctx, payloadID, 0, data1, 100)

	// Write at offset 95 (adjacent/overlapping)
	data2 := []byte("HELLO")
	_ = c.Write(ctx, payloadID, 0, data2, 95)

	// All writes go to the same block (block 0)
	blocks, _ := c.GetDirtyBlocks(ctx, payloadID)
	if len(blocks) != 1 {
		t.Errorf("expected 1 block, got %d", len(blocks))
	}
	// DataSize should reflect the highest written offset: 100 + 5 = 105
	if blocks[0].DataSize != 105 {
		t.Errorf("expected dataSize 105, got %d", blocks[0].DataSize)
	}
}

func TestWrite_InvalidOffset(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Try to write past chunk boundary
	data := make([]byte, 100)
	err := c.Write(ctx, payloadID, 0, data, ChunkSize-50)
	if err != ErrInvalidOffset {
		t.Errorf("expected ErrInvalidOffset, got %v", err)
	}
}

func TestWrite_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Write(ctx, "test", 0, []byte("data"), 0)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestWrite_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Close()

	err := c.Write(context.Background(), "test", 0, []byte("data"), 0)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

func TestWrite_MultipleChunks(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write to different chunks
	_ = c.Write(ctx, payloadID, 0, []byte("chunk0"), 0)
	_ = c.Write(ctx, payloadID, 1, []byte("chunk1"), 0)
	_ = c.Write(ctx, payloadID, 2, []byte("chunk2"), 0)

	blocks, _ := c.GetDirtyBlocks(ctx, payloadID)
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks (one per chunk), got %d", len(blocks))
	}

	// Verify sorted by chunk index
	for i, blk := range blocks {
		if blk.ChunkIndex != uint32(i) {
			t.Errorf("block[%d].ChunkIndex = %d, want %d", i, blk.ChunkIndex, i)
		}
	}
}

// ============================================================================
// Write Benchmarks
// ============================================================================

// BenchmarkWrite_Sequential measures sequential write performance.
// This is the critical path for NFS file copies - must achieve >3 GB/s.
func BenchmarkWrite_Sequential(b *testing.B) {
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

				if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWrite_SequentialExtend measures the sequential write optimization.
// Tests how well sequential writes go to same block buffers.
func BenchmarkWrite_SequentialExtend(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "bench-file"
	data := make([]byte, 32*1024) // 32KB writes

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	// All writes within same block go to same buffer
	for i := 0; i < b.N; i++ {
		offset := uint32(i * len(data))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize

		if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()

	// Report block count (should be ~16 blocks per chunk, fewer if writes are small)
	stats := c.Stats()
	b.ReportMetric(float64(stats.BlockCount), "blocks")
}

// BenchmarkWrite_Random measures random write performance.
// Simulates database workloads with scattered writes.
func BenchmarkWrite_Random(b *testing.B) {
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

		if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWrite_MultiFile measures writes across multiple files.
// Simulates multiple concurrent file operations.
func BenchmarkWrite_MultiFile(b *testing.B) {
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

				if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWrite_Concurrent measures concurrent write throughput.
// Tests lock contention under parallel access.
func BenchmarkWrite_Concurrent(b *testing.B) {
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

			if err := c.Write(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// BenchmarkMemory_BlockAllocation measures block buffer allocation overhead.
func BenchmarkMemory_BlockAllocation(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, 32*1024)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("file-%d", i)
		if err := c.Write(ctx, payloadID, 0, data, 0); err != nil {
			b.Fatal(err)
		}
	}
}
