package chunk

import (
	"testing"
)

// ============================================================================
// BlockRanges Iterator Tests
// ============================================================================

func TestBlockRanges_SingleBlock(t *testing.T) {
	// 32KB write at offset 0 - single block range
	var ranges []BlockRange
	for br := range BlockRanges(0, 32*1024) {
		ranges = append(ranges, br)
	}

	if len(ranges) != 1 {
		t.Fatalf("Expected 1 range, got %d", len(ranges))
	}

	br := ranges[0]
	if br.ChunkIndex != 0 {
		t.Errorf("ChunkIndex = %d, want 0", br.ChunkIndex)
	}
	if br.BlockIndex != 0 {
		t.Errorf("BlockIndex = %d, want 0", br.BlockIndex)
	}
	if br.Offset != 0 {
		t.Errorf("Offset = %d, want 0", br.Offset)
	}
	if br.Length != 32*1024 {
		t.Errorf("Length = %d, want %d", br.Length, 32*1024)
	}
	if br.BufOffset != 0 {
		t.Errorf("BufOffset = %d, want 0", br.BufOffset)
	}
}

func TestBlockRanges_CrossBlockBoundary(t *testing.T) {
	// 5MB write at offset 2MB - crosses into second block
	offset := uint64(2 * 1024 * 1024)   // 2MB
	length := 5 * 1024 * 1024           // 5MB

	var ranges []BlockRange
	for br := range BlockRanges(offset, length) {
		ranges = append(ranges, br)
	}

	if len(ranges) != 2 {
		t.Fatalf("Expected 2 ranges, got %d", len(ranges))
	}

	// First range: 2MB in block 0 (from 2MB to 4MB)
	if ranges[0].ChunkIndex != 0 {
		t.Errorf("Range 0: ChunkIndex = %d, want 0", ranges[0].ChunkIndex)
	}
	if ranges[0].BlockIndex != 0 {
		t.Errorf("Range 0: BlockIndex = %d, want 0", ranges[0].BlockIndex)
	}
	if ranges[0].Offset != 2*1024*1024 {
		t.Errorf("Range 0: Offset = %d, want %d", ranges[0].Offset, 2*1024*1024)
	}
	if ranges[0].Length != 2*1024*1024 {
		t.Errorf("Range 0: Length = %d, want %d", ranges[0].Length, 2*1024*1024)
	}
	if ranges[0].BufOffset != 0 {
		t.Errorf("Range 0: BufOffset = %d, want 0", ranges[0].BufOffset)
	}

	// Second range: 3MB in block 1 (from 0 to 3MB)
	if ranges[1].ChunkIndex != 0 {
		t.Errorf("Range 1: ChunkIndex = %d, want 0", ranges[1].ChunkIndex)
	}
	if ranges[1].BlockIndex != 1 {
		t.Errorf("Range 1: BlockIndex = %d, want 1", ranges[1].BlockIndex)
	}
	if ranges[1].Offset != 0 {
		t.Errorf("Range 1: Offset = %d, want 0", ranges[1].Offset)
	}
	if ranges[1].Length != 3*1024*1024 {
		t.Errorf("Range 1: Length = %d, want %d", ranges[1].Length, 3*1024*1024)
	}
	if ranges[1].BufOffset != 2*1024*1024 {
		t.Errorf("Range 1: BufOffset = %d, want %d", ranges[1].BufOffset, 2*1024*1024)
	}
}

func TestBlockRanges_CrossChunkBoundary(t *testing.T) {
	// Write that spans chunk boundary (at 64MB)
	offset := uint64(63 * 1024 * 1024)  // 1MB before chunk boundary
	length := 4 * 1024 * 1024           // 4MB write

	var ranges []BlockRange
	for br := range BlockRanges(offset, length) {
		ranges = append(ranges, br)
	}

	if len(ranges) != 2 {
		t.Fatalf("Expected 2 ranges (spans chunk boundary), got %d", len(ranges))
	}

	// First range: last 1MB of chunk 0 (block 15)
	if ranges[0].ChunkIndex != 0 {
		t.Errorf("Range 0: ChunkIndex = %d, want 0", ranges[0].ChunkIndex)
	}
	if ranges[0].BlockIndex != 15 {
		t.Errorf("Range 0: BlockIndex = %d, want 15", ranges[0].BlockIndex)
	}
	if ranges[0].Length != 1*1024*1024 {
		t.Errorf("Range 0: Length = %d, want %d", ranges[0].Length, 1*1024*1024)
	}

	// Second range: first 3MB of chunk 1 (block 0)
	if ranges[1].ChunkIndex != 1 {
		t.Errorf("Range 1: ChunkIndex = %d, want 1", ranges[1].ChunkIndex)
	}
	if ranges[1].BlockIndex != 0 {
		t.Errorf("Range 1: BlockIndex = %d, want 0", ranges[1].BlockIndex)
	}
	if ranges[1].Length != 3*1024*1024 {
		t.Errorf("Range 1: Length = %d, want %d", ranges[1].Length, 3*1024*1024)
	}
}

func TestBlockRanges_LargeWrite(t *testing.T) {
	// 100MB write from offset 0 - should span 25 blocks across 2 chunks
	var ranges []BlockRange
	for br := range BlockRanges(0, 100*1024*1024) {
		ranges = append(ranges, br)
	}

	// 100MB / 4MB = 25 blocks
	if len(ranges) != 25 {
		t.Fatalf("Expected 25 ranges, got %d", len(ranges))
	}

	// Verify first few blocks
	if ranges[0].ChunkIndex != 0 || ranges[0].BlockIndex != 0 {
		t.Errorf("Range 0: got chunk=%d block=%d, want chunk=0 block=0",
			ranges[0].ChunkIndex, ranges[0].BlockIndex)
	}

	// Block 16 should be in chunk 1
	if ranges[16].ChunkIndex != 1 || ranges[16].BlockIndex != 0 {
		t.Errorf("Range 16: got chunk=%d block=%d, want chunk=1 block=0",
			ranges[16].ChunkIndex, ranges[16].BlockIndex)
	}

	// Verify total bytes covered
	totalBytes := 0
	for _, br := range ranges {
		totalBytes += int(br.Length)
	}
	if totalBytes != 100*1024*1024 {
		t.Errorf("Total bytes = %d, want %d", totalBytes, 100*1024*1024)
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

func TestBlockRanges_NegativeLength(t *testing.T) {
	var count int
	for range BlockRanges(1000, -1) {
		count++
	}

	if count != 0 {
		t.Errorf("Expected 0 ranges for negative length, got %d", count)
	}
}

func TestBlockRanges_ExactBlockBoundary(t *testing.T) {
	// Start exactly at block 1 boundary (4MB)
	offset := uint64(BlockSize)
	length := 1000

	var ranges []BlockRange
	for br := range BlockRanges(offset, length) {
		ranges = append(ranges, br)
	}

	if len(ranges) != 1 {
		t.Fatalf("Expected 1 range, got %d", len(ranges))
	}

	if ranges[0].BlockIndex != 1 {
		t.Errorf("BlockIndex = %d, want 1", ranges[0].BlockIndex)
	}
	if ranges[0].Offset != 0 {
		t.Errorf("Offset = %d, want 0", ranges[0].Offset)
	}
}

func TestBlockRanges_FullBlock(t *testing.T) {
	// Exactly one full block
	var ranges []BlockRange
	for br := range BlockRanges(0, BlockSize) {
		ranges = append(ranges, br)
	}

	if len(ranges) != 1 {
		t.Fatalf("Expected 1 range, got %d", len(ranges))
	}

	if ranges[0].Length != uint32(BlockSize) {
		t.Errorf("Length = %d, want %d", ranges[0].Length, BlockSize)
	}
}

func TestBlockRanges_EarlyBreak(t *testing.T) {
	// Test that the iterator respects early termination
	// 100MB write would span 25 blocks
	var count int
	for range BlockRanges(0, 100*1024*1024) {
		count++
		if count >= 5 {
			break // Stop after 5 blocks
		}
	}

	if count != 5 {
		t.Errorf("Expected to process 5 ranges before break, got %d", count)
	}
}

func TestBlockRanges_BufOffsetContinuity(t *testing.T) {
	// Verify that BufOffset values are continuous
	offset := uint64(1 * 1024 * 1024)  // Start at 1MB
	length := 20 * 1024 * 1024         // 20MB - spans 6 blocks

	expectedBufOffset := 0
	for br := range BlockRanges(offset, length) {
		if br.BufOffset != expectedBufOffset {
			t.Errorf("BufOffset = %d, want %d", br.BufOffset, expectedBufOffset)
		}
		expectedBufOffset += int(br.Length)
	}

	if expectedBufOffset != length {
		t.Errorf("Total length = %d, want %d", expectedBufOffset, length)
	}
}

func TestChunkOffsetForBlock(t *testing.T) {
	tests := []struct {
		blockIdx   uint32
		wantOffset uint32
	}{
		{0, 0},
		{1, BlockSize},
		{2, 2 * BlockSize},
		{15, 15 * BlockSize},
	}

	for _, tt := range tests {
		got := ChunkOffsetForBlock(tt.blockIdx)
		if got != tt.wantOffset {
			t.Errorf("ChunkOffsetForBlock(%d) = %d, want %d",
				tt.blockIdx, got, tt.wantOffset)
		}
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
	if BlockSize != 4*1024*1024 {
		t.Errorf("BlockSize = %d, want %d", BlockSize, 4*1024*1024)
	}
	if BlocksPerChunk != 16 {
		t.Errorf("BlocksPerChunk = %d, want 16", BlocksPerChunk)
	}
	if DefaultMaxSlicesPerChunk != 16 {
		t.Errorf("DefaultMaxSlicesPerChunk = %d, want 16", DefaultMaxSlicesPerChunk)
	}
}

// ============================================================================
// Benchmarks
// ============================================================================

func BenchmarkBlockRanges_SmallWrite(b *testing.B) {
	// Typical NFS write: 32KB
	for i := 0; i < b.N; i++ {
		for range BlockRanges(0, 32*1024) {
		}
	}
}

func BenchmarkBlockRanges_MediumWrite(b *testing.B) {
	// 1MB write
	for i := 0; i < b.N; i++ {
		for range BlockRanges(0, 1024*1024) {
		}
	}
}

func BenchmarkBlockRanges_LargeWrite(b *testing.B) {
	// 100MB write (stress test)
	for i := 0; i < b.N; i++ {
		for range BlockRanges(0, 100*1024*1024) {
		}
	}
}

func BenchmarkBlockRanges_UnalignedWrite(b *testing.B) {
	// 5MB write at offset 2MB (crosses block boundary)
	for i := 0; i < b.N; i++ {
		for range BlockRanges(2*1024*1024, 5*1024*1024) {
		}
	}
}
