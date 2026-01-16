package cache

import (
	"context"
	"fmt"
	"testing"
)

func TestReadSlice_Basic(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("hello world")

	_ = c.WriteSlice(ctx, payloadID, 0, data, 0)

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

func TestReadSlice_NotFound(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	result := make([]byte, 10)
	found, err := c.ReadSlice(context.Background(), "nonexistent", 0, 0, 10, result)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if found {
		t.Error("expected not found for nonexistent file")
	}
}

func TestReadSlice_PartialRead(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("hello world")

	_ = c.WriteSlice(ctx, payloadID, 0, data, 0)

	// Read only "world"
	result := make([]byte, 5)
	found, err := c.ReadSlice(ctx, payloadID, 0, 6, 5, result)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if !found {
		t.Fatal("expected to find data")
	}
	if string(result) != "world" {
		t.Errorf("data mismatch: got %q, want %q", result, "world")
	}
}

func TestReadSlice_NewestWins(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write overlapping data (newer overwrites older)
	_ = c.WriteSlice(ctx, payloadID, 0, []byte("AAAAAAAAAA"), 0) // 10 A's at offset 0
	_ = c.WriteSlice(ctx, payloadID, 0, []byte("BBB"), 3)        // 3 B's at offset 3

	// Read full range - should be AAABBBAAAA
	result := make([]byte, 10)
	found, _ := c.ReadSlice(ctx, payloadID, 0, 0, 10, result)
	if !found {
		t.Fatal("expected to find data")
	}
	expected := "AAABBBAAAA"
	if string(result) != expected {
		t.Errorf("newest-wins failed: got %q, want %q", result, expected)
	}
}

func TestReadSlice_MultipleOverlaps(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write multiple overlapping slices (in order: oldest to newest)
	// Each newer write overwrites older data at the same positions
	_ = c.WriteSlice(ctx, payloadID, 0, []byte("1111111111"), 0) // Base: 1111111111
	_ = c.WriteSlice(ctx, payloadID, 0, []byte("22"), 2)         // Now:  1122111111
	_ = c.WriteSlice(ctx, payloadID, 0, []byte("33"), 5)         // Now:  1122133111
	_ = c.WriteSlice(ctx, payloadID, 0, []byte("4"), 3)          // Now:  1124133111

	result := make([]byte, 10)
	found, _ := c.ReadSlice(ctx, payloadID, 0, 0, 10, result)
	if !found {
		t.Fatal("expected to find data")
	}
	expected := "1124133111"
	if string(result) != expected {
		t.Errorf("multi-overlap failed: got %q, want %q", result, expected)
	}
}

func TestReadSlice_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := make([]byte, 10)
	_, err := c.ReadSlice(ctx, "test", 0, 0, 10, result)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestReadSlice_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.WriteSlice(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	result := make([]byte, 4)
	_, err := c.ReadSlice(context.Background(), "test", 0, 0, 4, result)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

func TestReadSlice_WrongChunk(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.WriteSlice(ctx, payloadID, 0, []byte("chunk0"), 0)

	// Try to read from chunk 1 (empty)
	result := make([]byte, 6)
	found, _ := c.ReadSlice(ctx, payloadID, 1, 0, 6, result)
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

	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 100), 0)

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

	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 50), 0)

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

func TestIsRangeCovered_MultipleSlices(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write non-adjacent slices
	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 50), 0)
	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 50), 50)

	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 100)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if !covered {
		t.Error("expected range to be covered by multiple slices")
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

func TestIsRangeCovered_OverlappingSlices(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write two completely overlapping slices at offset 0
	// This tests the bug fix: the old implementation would sum overlaps
	// and incorrectly report coverage for bytes beyond the slices
	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 5), 0) // bytes 0-5
	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 5), 0) // bytes 0-5 (overlap)

	// The old buggy code would count 5+5=10 bytes of overlap
	// and incorrectly report bytes 0-10 as covered
	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 10)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if covered {
		t.Error("expected range 0-10 to NOT be covered (only 0-5 is covered)")
	}

	// But 0-5 should be covered
	covered, err = c.IsRangeCovered(ctx, payloadID, 0, 0, 5)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if !covered {
		t.Error("expected range 0-5 to be covered")
	}
}

func TestIsRangeCovered_PartialOverlap(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write partially overlapping slices
	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 10), 0) // bytes 0-10
	_ = c.WriteSlice(ctx, payloadID, 0, make([]byte, 10), 5) // bytes 5-15

	// Together they cover 0-15
	covered, err := c.IsRangeCovered(ctx, payloadID, 0, 0, 15)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if !covered {
		t.Error("expected range 0-15 to be covered")
	}

	// But 0-20 should not be covered
	covered, err = c.IsRangeCovered(ctx, payloadID, 0, 0, 20)
	if err != nil {
		t.Fatalf("IsRangeCovered failed: %v", err)
	}
	if covered {
		t.Error("expected range 0-20 to NOT be covered")
	}
}

// ============================================================================
// Read Benchmarks
// ============================================================================

// BenchmarkReadSlice measures read performance at various sizes.
func BenchmarkReadSlice(b *testing.B) {
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
				_ = c.WriteSlice(ctx, payloadID, chunkIdx, writeData, offsetInChunk)
			}

			dest := make([]byte, s.size)
			b.SetBytes(int64(s.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				offset := uint32((i * s.size) % (64 * 1024 * 1024))
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize

				if _, err := c.ReadSlice(ctx, payloadID, chunkIdx, offsetInChunk, uint32(s.size), dest); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReadSlice_SliceMerging measures read performance with overlapping slices.
// Tests the newest-wins merge algorithm efficiency.
func BenchmarkReadSlice_SliceMerging(b *testing.B) {
	sliceCounts := []int{1, 5, 10, 25, 50}

	for _, count := range sliceCounts {
		b.Run(fmt.Sprintf("slices=%d", count), func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()
			payloadID := "bench-file"

			// Create overlapping slices (each 4KB, overlapping by 2KB)
			for i := 0; i < count; i++ {
				data := make([]byte, 4*1024)
				for j := range data {
					data[j] = byte(i)
				}
				offset := uint32(i * 2 * 1024)
				_ = c.WriteSlice(ctx, payloadID, 0, data, offset)
			}

			readSize := uint32(32 * 1024)
			dest := make([]byte, readSize)
			b.SetBytes(int64(readSize))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := c.ReadSlice(ctx, payloadID, 0, 0, readSize, dest); err != nil {
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
	_ = c.WriteSlice(ctx, payloadID, 0, data, 0)

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
