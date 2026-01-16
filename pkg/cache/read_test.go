package cache

import (
	"context"
	"fmt"
	"testing"
)

func TestRead_Basic(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("hello world")

	_ = c.Write(ctx, payloadID, 0, data, 0)

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

func TestRead_NotFound(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	result := make([]byte, 10)
	found, err := c.Read(context.Background(), "nonexistent", 0, 0, 10, result)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if found {
		t.Error("expected not found for nonexistent file")
	}
}

func TestRead_PartialRead(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("hello world")

	_ = c.Write(ctx, payloadID, 0, data, 0)

	// Read only "world"
	result := make([]byte, 5)
	found, err := c.Read(ctx, payloadID, 0, 6, 5, result)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !found {
		t.Fatal("expected to find data")
	}
	if string(result) != "world" {
		t.Errorf("data mismatch: got %q, want %q", result, "world")
	}
}

func TestRead_NewestWins(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write overlapping data (newer overwrites older)
	_ = c.Write(ctx, payloadID, 0, []byte("AAAAAAAAAA"), 0) // 10 A's at offset 0
	_ = c.Write(ctx, payloadID, 0, []byte("BBB"), 3)        // 3 B's at offset 3

	// Read full range - should be AAABBBAAAA
	result := make([]byte, 10)
	found, _ := c.Read(ctx, payloadID, 0, 0, 10, result)
	if !found {
		t.Fatal("expected to find data")
	}
	expected := "AAABBBAAAA"
	if string(result) != expected {
		t.Errorf("newest-wins failed: got %q, want %q", result, expected)
	}
}

func TestRead_MultipleOverlaps(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write multiple overlapping regions (in order: oldest to newest)
	// Each newer write overwrites older data at the same positions
	_ = c.Write(ctx, payloadID, 0, []byte("1111111111"), 0) // Base: 1111111111
	_ = c.Write(ctx, payloadID, 0, []byte("22"), 2)         // Now:  1122111111
	_ = c.Write(ctx, payloadID, 0, []byte("33"), 5)         // Now:  1122133111
	_ = c.Write(ctx, payloadID, 0, []byte("4"), 3)          // Now:  1124133111

	result := make([]byte, 10)
	found, _ := c.Read(ctx, payloadID, 0, 0, 10, result)
	if !found {
		t.Fatal("expected to find data")
	}
	expected := "1124133111"
	if string(result) != expected {
		t.Errorf("multi-overlap failed: got %q, want %q", result, expected)
	}
}

func TestRead_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := make([]byte, 10)
	_, err := c.Read(ctx, "test", 0, 0, 10, result)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRead_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Write(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	result := make([]byte, 4)
	_, err := c.Read(context.Background(), "test", 0, 0, 4, result)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

func TestRead_WrongChunk(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, []byte("chunk0"), 0)

	// Try to read from chunk 1 (empty)
	result := make([]byte, 6)
	found, _ := c.Read(ctx, payloadID, 1, 0, 6, result)
	if found {
		t.Error("expected not found for empty chunk")
	}
}

// ============================================================================
// IsRangeCovered Tests
// ============================================================================

func TestIsRangeCovered_FullyCovered(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, make([]byte, 100), 0)

	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 100)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if !covered {
		t.Error("expected range to be covered")
	}
}

func TestIsRangeCovered_PartiallyCovered(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, make([]byte, 50), 0)

	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 100)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if covered {
		t.Error("expected range to NOT be covered (only half)")
	}
}

func TestIsRangeCovered_NotCovered(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	covered, err := c.IsRangeCovered(context.Background(), "nonexistent", 0, 0, 100)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if covered {
		t.Error("expected range to NOT be covered")
	}
}

func TestIsRangeCovered_MultipleWrites(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write adjacent data that together covers a range
	// Coverage granularity is 64 bytes, so write at granularity boundaries
	_ = c.Write(ctx, payloadID, 0, make([]byte, 64), 0)
	_ = c.Write(ctx, payloadID, 0, make([]byte, 64), 64)

	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 128)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if !covered {
		t.Error("expected range to be covered by multiple writes")
	}
}

func TestIsRangeCovered_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.IsRangeCovered(ctx, "test", 0, 0, 100)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestIsRangeCovered_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Close()

	_, err := c.IsRangeCovered(context.Background(), "test", 0, 0, 100)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

func TestIsRangeCovered_OverlappingWrites(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Coverage bitmap has 64-byte granularity.
	// Write overlapping data at the same offset - should not extend coverage.
	_ = c.Write(ctx, payloadID, 0, make([]byte, 64), 0) // bytes 0-64
	_ = c.Write(ctx, payloadID, 0, make([]byte, 64), 0) // bytes 0-64 (overlap)

	// Range 0-64 should be covered
	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 64)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if !covered {
		t.Error("expected range 0-64 to be covered")
	}

	// Range 0-128 should NOT be covered (only first 64 bytes written)
	covered, err = c.IsRangeCovered(ctx, payloadID, 0, 0, 128)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if covered {
		t.Error("expected range 0-128 to NOT be covered (only 0-64 is covered)")
	}
}

func TestIsRangeCovered_PartialOverlap(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Coverage bitmap has 64-byte granularity.
	// Write partially overlapping data that together cover a contiguous range.
	_ = c.Write(ctx, payloadID, 0, make([]byte, 128), 0)  // bytes 0-128
	_ = c.Write(ctx, payloadID, 0, make([]byte, 128), 64) // bytes 64-192

	// Together they cover 0-192
	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 192)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if !covered {
		t.Error("expected range 0-192 to be covered")
	}

	// But 0-256 should not be covered
	covered, err = c.IsRangeCovered(ctx, payloadID, 0, 0, 256)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if covered {
		t.Error("expected range 0-256 to NOT be covered")
	}
}

// ============================================================================
// Read Benchmarks
// ============================================================================

// BenchmarkRead measures read performance at various sizes.
func BenchmarkRead(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"4KB", 4 * 1024},
		{"32KB", 32 * 1024},
		{"64KB", 64 * 1024},
		{"128KB", 128 * 1024},
	}

	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()
			payloadID := "bench-file"

			// Pre-populate with 64MB of data using 32KB writes
			writeData := make([]byte, 32*1024)
			for i := 0; i < 2048; i++ {
				offset := uint32(i * len(writeData))
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize
				_ = c.Write(ctx, payloadID, chunkIdx, writeData, offsetInChunk)
			}

			dest := make([]byte, s.size)
			b.SetBytes(int64(s.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				offset := uint32((i * s.size) % (64 * 1024 * 1024))
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize

				if _, err := c.Read(ctx, payloadID, chunkIdx, offsetInChunk, uint32(s.size), dest); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRead_OverlappingWrites measures read performance with overlapping writes.
// Tests the newest-wins overwrite behavior efficiency.
func BenchmarkRead_OverlappingWrites(b *testing.B) {
	writeCounts := []int{1, 5, 10, 25, 50}

	for _, count := range writeCounts {
		b.Run(fmt.Sprintf("writes=%d", count), func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()
			payloadID := "bench-file"

			// Create overlapping writes (each 4KB, overlapping by 2KB)
			for i := 0; i < count; i++ {
				data := make([]byte, 4*1024)
				for j := range data {
					data[j] = byte(i)
				}
				offset := uint32(i * 2 * 1024)
				_ = c.Write(ctx, payloadID, 0, data, offset)
			}

			readSize := uint32(32 * 1024)
			dest := make([]byte, readSize)
			b.SetBytes(int64(readSize))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := c.Read(ctx, payloadID, 0, 0, readSize, dest); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkIsRangeCovered measures cache hit detection performance.
func BenchmarkIsRangeCovered(b *testing.B) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "bench-file"

	// Create a file with full coverage
	data := make([]byte, ChunkSize)
	_ = c.Write(ctx, payloadID, 0, data, 0)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32((i * 4096) % ChunkSize)
		length := uint32(32 * 1024)
		if offset+length > ChunkSize {
			length = ChunkSize - offset
		}
		_, _ = c.IsRangeCovered(ctx, payloadID, 0, offset, length)
	}
}
