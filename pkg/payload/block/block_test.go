package block

import (
	"testing"
)

func TestIndexForOffset(t *testing.T) {
	tests := []struct {
		name           string
		offsetInChunk  uint32
		wantBlockIndex uint32
	}{
		{"zero offset", 0, 0},
		{"within first block", 1000, 0},
		{"at block boundary", Size, 1},
		{"just after boundary", Size + 1, 1},
		{"middle of second block", Size + Size/2, 1},
		{"third block", 2 * Size, 2},
		{"last byte of first block", Size - 1, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IndexForOffset(tt.offsetInChunk)
			if got != tt.wantBlockIndex {
				t.Errorf("IndexForOffset(%d) = %d, want %d", tt.offsetInChunk, got, tt.wantBlockIndex)
			}
		})
	}
}

func TestOffsetInBlock(t *testing.T) {
	tests := []struct {
		name          string
		offsetInChunk uint32
		wantOffset    uint32
	}{
		{"zero offset", 0, 0},
		{"within first block", 1000, 1000},
		{"at block boundary", Size, 0},
		{"just after boundary", Size + 1, 1},
		{"middle of second block", Size + 1000, 1000},
		{"last byte of first block", Size - 1, Size - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OffsetInBlock(tt.offsetInChunk)
			if got != tt.wantOffset {
				t.Errorf("OffsetInBlock(%d) = %d, want %d", tt.offsetInChunk, got, tt.wantOffset)
			}
		})
	}
}

func TestRange(t *testing.T) {
	tests := []struct {
		name       string
		offset     uint32
		length     uint32
		wantStart  uint32
		wantEnd    uint32
	}{
		{"zero length", 1000, 0, 0, 0},
		{"single block", 0, 1000, 0, 0},
		{"full block", 0, Size, 0, 0},
		{"spans two blocks", Size - 1000, 2000, 0, 1},
		{"starts at boundary", Size, 1000, 1, 1},
		{"spans three blocks", Size - 100, Size + 200, 0, 2},
		{"exact two blocks", 0, 2 * Size, 0, 1},
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
		blockIdx  uint32
		wantStart uint32
		wantEnd   uint32
	}{
		{"first block", 0, 0, Size},
		{"second block", 1, Size, 2 * Size},
		{"third block", 2, 2 * Size, 3 * Size},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := Bounds(tt.blockIdx)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("Bounds(%d) = (%d, %d), want (%d, %d)",
					tt.blockIdx, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestPerChunk(t *testing.T) {
	tests := []struct {
		name      string
		chunkSize uint32
		want      uint32
	}{
		{"64MB chunk", 64 * 1024 * 1024, 16},
		{"32MB chunk", 32 * 1024 * 1024, 8},
		{"128MB chunk", 128 * 1024 * 1024, 32},
		{"single block chunk", Size, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PerChunk(tt.chunkSize)
			if got != tt.want {
				t.Errorf("PerChunk(%d) = %d, want %d", tt.chunkSize, got, tt.want)
			}
		})
	}
}

func TestConstants(t *testing.T) {
	// Verify constants have expected values
	if Size != 4*1024*1024 {
		t.Errorf("Size = %d, want %d", Size, 4*1024*1024)
	}
	if MinSize != 1*1024*1024 {
		t.Errorf("MinSize = %d, want %d", MinSize, 1*1024*1024)
	}
	if MaxSize != 16*1024*1024 {
		t.Errorf("MaxSize = %d, want %d", MaxSize, 16*1024*1024)
	}

	// Verify relationships
	if MinSize > Size {
		t.Error("MinSize should be <= Size")
	}
	if Size > MaxSize {
		t.Error("Size should be <= MaxSize")
	}
}
