package chunk

import (
	"testing"
)

func TestSlices_SingleChunk(t *testing.T) {
	// Range entirely within first chunk
	var slices []Slice
	for s := range Slices(1000, 5000) {
		slices = append(slices, s)
	}

	if len(slices) != 1 {
		t.Fatalf("Expected 1 slice, got %d", len(slices))
	}

	s := slices[0]
	if s.ChunkIndex != 0 {
		t.Errorf("ChunkIndex = %d, want 0", s.ChunkIndex)
	}
	if s.Offset != 1000 {
		t.Errorf("Offset = %d, want 1000", s.Offset)
	}
	if s.Length != 5000 {
		t.Errorf("Length = %d, want 5000", s.Length)
	}
	if s.BufOffset != 0 {
		t.Errorf("BufOffset = %d, want 0", s.BufOffset)
	}
}

func TestSlices_SpansTwoChunks(t *testing.T) {
	// Range spans two chunks: starts 1000 bytes before chunk boundary
	offset := uint64(Size - 1000)
	length := uint64(2000) // 1000 in chunk 0, 1000 in chunk 1

	var slices []Slice
	for s := range Slices(offset, length) {
		slices = append(slices, s)
	}

	if len(slices) != 2 {
		t.Fatalf("Expected 2 slices, got %d", len(slices))
	}

	// First slice: last 1000 bytes of chunk 0
	s0 := slices[0]
	if s0.ChunkIndex != 0 {
		t.Errorf("Slice 0: ChunkIndex = %d, want 0", s0.ChunkIndex)
	}
	if s0.Offset != Size-1000 {
		t.Errorf("Slice 0: Offset = %d, want %d", s0.Offset, Size-1000)
	}
	if s0.Length != 1000 {
		t.Errorf("Slice 0: Length = %d, want 1000", s0.Length)
	}
	if s0.BufOffset != 0 {
		t.Errorf("Slice 0: BufOffset = %d, want 0", s0.BufOffset)
	}

	// Second slice: first 1000 bytes of chunk 1
	s1 := slices[1]
	if s1.ChunkIndex != 1 {
		t.Errorf("Slice 1: ChunkIndex = %d, want 1", s1.ChunkIndex)
	}
	if s1.Offset != 0 {
		t.Errorf("Slice 1: Offset = %d, want 0", s1.Offset)
	}
	if s1.Length != 1000 {
		t.Errorf("Slice 1: Length = %d, want 1000", s1.Length)
	}
	if s1.BufOffset != 1000 {
		t.Errorf("Slice 1: BufOffset = %d, want 1000", s1.BufOffset)
	}
}

func TestSlices_SpansThreeChunks(t *testing.T) {
	// Range spans three chunks
	offset := uint64(Size - 100)
	length := uint64(Size + 200) // 100 in chunk 0, full chunk 1, 100 in chunk 2

	var slices []Slice
	for s := range Slices(offset, length) {
		slices = append(slices, s)
	}

	if len(slices) != 3 {
		t.Fatalf("Expected 3 slices, got %d", len(slices))
	}

	// First slice: last 100 bytes of chunk 0
	if slices[0].ChunkIndex != 0 || slices[0].Length != 100 {
		t.Errorf("Slice 0: got chunk=%d len=%d, want chunk=0 len=100",
			slices[0].ChunkIndex, slices[0].Length)
	}

	// Second slice: full chunk 1
	if slices[1].ChunkIndex != 1 || slices[1].Length != Size {
		t.Errorf("Slice 1: got chunk=%d len=%d, want chunk=1 len=%d",
			slices[1].ChunkIndex, slices[1].Length, Size)
	}

	// Third slice: first 100 bytes of chunk 2
	if slices[2].ChunkIndex != 2 || slices[2].Length != 100 {
		t.Errorf("Slice 2: got chunk=%d len=%d, want chunk=2 len=100",
			slices[2].ChunkIndex, slices[2].Length)
	}

	// Verify BufOffsets are correct
	expectedBufOffset := 0
	for i, s := range slices {
		if s.BufOffset != expectedBufOffset {
			t.Errorf("Slice %d: BufOffset = %d, want %d", i, s.BufOffset, expectedBufOffset)
		}
		expectedBufOffset += int(s.Length)
	}

	// Total length should match
	if expectedBufOffset != int(length) {
		t.Errorf("Total length = %d, want %d", expectedBufOffset, length)
	}
}

func TestSlices_ZeroLength(t *testing.T) {
	var count int
	for range Slices(1000, 0) {
		count++
	}

	if count != 0 {
		t.Errorf("Expected 0 slices for zero length, got %d", count)
	}
}

func TestSlices_ExactChunkBoundary(t *testing.T) {
	// Start exactly at chunk 1 boundary
	offset := uint64(Size)
	length := uint64(1000)

	var slices []Slice
	for s := range Slices(offset, length) {
		slices = append(slices, s)
	}

	if len(slices) != 1 {
		t.Fatalf("Expected 1 slice, got %d", len(slices))
	}

	if slices[0].ChunkIndex != 1 {
		t.Errorf("ChunkIndex = %d, want 1", slices[0].ChunkIndex)
	}
	if slices[0].Offset != 0 {
		t.Errorf("Offset = %d, want 0", slices[0].Offset)
	}
}

func TestSlices_FullChunk(t *testing.T) {
	// Exactly one full chunk
	offset := uint64(0)
	length := uint64(Size)

	var slices []Slice
	for s := range Slices(offset, length) {
		slices = append(slices, s)
	}

	if len(slices) != 1 {
		t.Fatalf("Expected 1 slice, got %d", len(slices))
	}

	if slices[0].Length != Size {
		t.Errorf("Length = %d, want %d", slices[0].Length, Size)
	}
}

func TestSlices_EarlyBreak(t *testing.T) {
	// Test that the iterator respects early termination
	offset := uint64(Size - 100)
	length := uint64(Size + 200) // Would span 3 chunks

	var count int
	for range Slices(offset, length) {
		count++
		if count >= 2 {
			break // Stop after 2 slices
		}
	}

	if count != 2 {
		t.Errorf("Expected to process 2 slices before break, got %d", count)
	}
}

// ============================================================================
// Chunk Helper Function Tests
// ============================================================================

func TestIndexForOffset(t *testing.T) {
	tests := []struct {
		name       string
		offset     uint64
		wantIndex  uint32
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
		name          string
		chunkIdx      uint32
		fileOffset    uint64
		length        uint64
		wantOffset    uint32
		wantLength    uint32
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
