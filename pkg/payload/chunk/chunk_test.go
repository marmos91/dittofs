package chunk

import (
	"testing"
)

func TestBlockRanges_SingleChunk(t *testing.T) {
	// Range entirely within first chunk
	var ranges []BlockRange
	for r := range BlockRanges(1000, 5000) {
		ranges = append(ranges, r)
	}

	if len(ranges) == 0 {
		t.Fatalf("Expected at least 1 range, got 0")
	}

	r := ranges[0]
	if r.ChunkIndex != 0 {
		t.Errorf("ChunkIndex = %d, want 0", r.ChunkIndex)
	}
}

func TestBlockRanges_ZeroLength(t *testing.T) {
	var count int
	for range BlockRanges(1000, 0) {
		count++
	}

	if count != 0 {
		t.Errorf("Expected 0 ranges for zero length, got %d", count)
	}
}

func TestBlockRanges_EarlyBreak(t *testing.T) {
	// Test that the iterator respects early termination
	// Large range that would span many blocks
	var count int
	for range BlockRanges(0, 100*1024*1024) { // 100MB would span many blocks
		count++
		if count >= 2 {
			break // Stop after 2 ranges
		}
	}

	if count != 2 {
		t.Errorf("Expected to process 2 ranges before break, got %d", count)
	}
}

// ============================================================================
// Chunk Helper Function Tests
// ============================================================================

func TestIndexForOffset(t *testing.T) {
	tests := []struct {
		name      string
		offset    uint64
		wantIndex uint32
	}{
		{"zero offset", 0, 0},
		{"within first chunk", 1000, 0},
		{"at chunk boundary", Size, 1},
		{"just after boundary", Size + 1, 1},
		{"middle of second chunk", Size + Size/2, 1},
		{"third chunk", 2 * Size, 2},
		{"last byte of first chunk", Size - 1, 0},
		{"large offset", 10 * Size, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IndexForOffset(tt.offset)
			if got != tt.wantIndex {
				t.Errorf("IndexForOffset(%d) = %d, want %d", tt.offset, got, tt.wantIndex)
			}
		})
	}
}

func TestOffsetInChunk(t *testing.T) {
	tests := []struct {
		name       string
		offset     uint64
		wantOffset uint32
	}{
		{"zero offset", 0, 0},
		{"within first chunk", 1000, 1000},
		{"at chunk boundary", Size, 0},
		{"just after boundary", Size + 1, 1},
		{"middle of second chunk", Size + 1000, 1000},
		{"last byte of first chunk", Size - 1, Size - 1},
		{"large offset same position", 10*Size + 5000, 5000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OffsetInChunk(tt.offset)
			if got != tt.wantOffset {
				t.Errorf("OffsetInChunk(%d) = %d, want %d", tt.offset, got, tt.wantOffset)
			}
		})
	}
}

func TestRange(t *testing.T) {
	tests := []struct {
		name      string
		offset    uint64
		length    uint64
		wantStart uint32
		wantEnd   uint32
	}{
		{"zero length", 1000, 0, 0, 0},
		{"single chunk", 0, 1000, 0, 0},
		{"full chunk", 0, Size, 0, 0},
		{"spans two chunks", Size - 1000, 2000, 0, 1},
		{"starts at boundary", Size, 1000, 1, 1},
		{"spans three chunks", Size - 100, Size + 200, 0, 2},
		{"exact two chunks", 0, 2 * Size, 0, 1},
		{"starts in second chunk", Size + 1000, 500, 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := Range(tt.offset, tt.length)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("Range(%d, %d) = (%d, %d), want (%d, %d)",
					tt.offset, tt.length, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestBounds(t *testing.T) {
	tests := []struct {
		name      string
		chunkIdx  uint32
		wantStart uint64
		wantEnd   uint64
	}{
		{"first chunk", 0, 0, Size},
		{"second chunk", 1, Size, 2 * Size},
		{"third chunk", 2, 2 * Size, 3 * Size},
		{"tenth chunk", 10, 10 * Size, 11 * Size},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := Bounds(tt.chunkIdx)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("Bounds(%d) = (%d, %d), want (%d, %d)",
					tt.chunkIdx, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestClipToChunk(t *testing.T) {
	tests := []struct {
		name       string
		chunkIdx   uint32
		fileOffset uint64
		length     uint64
		wantOffset uint32
		wantLength uint32
	}{
		{"entirely within chunk 0", 0, 1000, 5000, 1000, 5000},
		{"entire first chunk", 0, 0, Size, 0, Size},
		{"starts before chunk", 1, 0, Size + 1000, 0, 1000},
		{"ends after chunk", 0, Size - 1000, 2000, Size - 1000, 1000},
		{"no overlap - before", 1, 0, 100, 0, 0},
		{"no overlap - after", 0, Size, 100, 0, 0},
		{"spans entire chunk", 1, Size - 100, Size + 200, 0, Size},
		{"starts at chunk boundary", 1, Size, 1000, 0, 1000},
		{"ends at chunk boundary", 0, Size - 1000, 1000, Size - 1000, 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset, length := ClipToChunk(tt.chunkIdx, tt.fileOffset, tt.length)
			if offset != tt.wantOffset || length != tt.wantLength {
				t.Errorf("ClipToChunk(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tt.chunkIdx, tt.fileOffset, tt.length,
					offset, length, tt.wantOffset, tt.wantLength)
			}
		})
	}
}

func TestConstants(t *testing.T) {
	// Verify constants have expected values
	if Size != 64*1024*1024 {
		t.Errorf("Size = %d, want %d", Size, 64*1024*1024)
	}
	if DefaultMaxSlicesPerChunk != 16 {
		t.Errorf("DefaultMaxSlicesPerChunk = %d, want 16", DefaultMaxSlicesPerChunk)
	}
}
