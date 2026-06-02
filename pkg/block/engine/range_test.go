package engine

import (
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// mkBlocks builds a sorted []BlockRef from (offset, size) pairs.
// Sizes default to 4 MiB when omitted.
func mkBlocks(offsetsAndSizes ...uint64) []block.BlockRef {
	const defaultSize = 4 << 20
	if len(offsetsAndSizes)%2 != 0 {
		// caller passed only offsets — pair them with default size.
		out := make([]block.BlockRef, len(offsetsAndSizes))
		for i, off := range offsetsAndSizes {
			out[i] = block.BlockRef{Offset: off, Size: defaultSize}
		}
		return out
	}
	out := make([]block.BlockRef, len(offsetsAndSizes)/2)
	for i := 0; i < len(offsetsAndSizes); i += 2 {
		out[i/2] = block.BlockRef{Offset: offsetsAndSizes[i], Size: uint32(offsetsAndSizes[i+1])}
	}
	return out
}

// TestFindBlocksForRange_Empty: an empty []BlockRef returns (0, 0) for
// any range.
func TestFindBlocksForRange_Empty(t *testing.T) {
	cases := []struct{ offset, size uint64 }{
		{0, 0},
		{0, 1024},
		{1 << 20, 4 << 20},
	}
	for _, c := range cases {
		start, end := findBlocksForRange(nil, c.offset, c.size)
		if start != 0 || end != 0 {
			t.Errorf("offset=%d size=%d: got (%d, %d), want (0, 0)", c.offset, c.size, start, end)
		}
	}
}

// TestFindBlocksForRange_RangeBeforeFirstBlock: a range fully before the
// first block returns (0, 0). The dual-read shim or zero-fill
// upstream handles serving the bytes.
func TestFindBlocksForRange_RangeBeforeFirstBlock(t *testing.T) {
	blocks := mkBlocks(1<<20, 4<<20, 5<<20, 4<<20) // [1M..5M), [5M..9M)
	start, end := findBlocksForRange(blocks, 0, 1<<20)
	if start != 0 || end != 0 {
		t.Errorf("range before first block: got (%d, %d), want (0, 0)", start, end)
	}
}

// TestFindBlocksForRange_RangeAfterLastBlock: a range fully after the
// last block returns (len, len).
func TestFindBlocksForRange_RangeAfterLastBlock(t *testing.T) {
	blocks := mkBlocks(0, 4<<20, 4<<20, 4<<20) // [0..4M), [4M..8M)
	const fileEnd = 8 << 20
	start, end := findBlocksForRange(blocks, fileEnd+1, 1024)
	if start != len(blocks) || end != len(blocks) {
		t.Errorf("range after last block: got (%d, %d), want (%d, %d)", start, end, len(blocks), len(blocks))
	}
}

// TestFindBlocksForRange_ExactBlockBoundary: a range exactly at one
// BlockRef boundary returns indices for that single block.
func TestFindBlocksForRange_ExactBlockBoundary(t *testing.T) {
	blocks := mkBlocks(0, 1024, 1024, 1024, 2048, 1024)
	start, end := findBlocksForRange(blocks, 1024, 1024)
	if start != 1 || end != 2 {
		t.Errorf("exact-block range: got (%d, %d), want (1, 2)", start, end)
	}
	if got := end - start; got != 1 {
		t.Errorf("expected 1 block in range, got %d", got)
	}
}

// TestFindBlocksForRange_SpanThreeContiguousBlocks: a range spanning 3
// contiguous BlockRefs returns start, end with end-start == 3.
func TestFindBlocksForRange_SpanThreeContiguousBlocks(t *testing.T) {
	blocks := mkBlocks(0, 1024, 1024, 1024, 2048, 1024, 3072, 1024)
	start, end := findBlocksForRange(blocks, 512, 2560) // [512..3072)
	if got := end - start; got != 3 {
		t.Errorf("span-3 range: got [%d, %d) covering %d blocks, want 3", start, end, got)
	}
}

// TestFindBlocksForRange_SpanWithSparseHole: a range spanning a sparse
// hole (gap between BlockRef N and N+1) returns indices covering both —
// caller zero-fills the gap.
func TestFindBlocksForRange_SpanWithSparseHole(t *testing.T) {
	// Two blocks with a gap [1024..4096)
	blocks := mkBlocks(0, 1024, 4096, 1024)
	start, end := findBlocksForRange(blocks, 0, 5120) // [0..5120) crosses the hole
	if got := end - start; got != 2 {
		t.Errorf("span-with-hole: got [%d, %d) covering %d blocks, want 2", start, end, got)
	}
}

// TestFindBlocksForRange_ZeroSizeReturnsEmpty: a zero-size range always
// returns (0, 0).
func TestFindBlocksForRange_ZeroSizeReturnsEmpty(t *testing.T) {
	blocks := mkBlocks(0, 1024, 1024, 1024)
	start, end := findBlocksForRange(blocks, 512, 0)
	if start != 0 || end != 0 {
		t.Errorf("zero-size: got (%d, %d), want (0, 0)", start, end)
	}
}

// TestFindBlocksForRange_PartialOverlapAtHead: a range that starts
// inside a block returns that block in the result.
func TestFindBlocksForRange_PartialOverlapAtHead(t *testing.T) {
	blocks := mkBlocks(0, 4096, 4096, 4096)
	start, end := findBlocksForRange(blocks, 1024, 1024) // [1024..2048) inside block 0
	if start != 0 || end != 1 {
		t.Errorf("partial-head: got (%d, %d), want (0, 1)", start, end)
	}
}

// TestFindBlocksForRange_Property: for a sorted random []BlockRef and a
// random (offset, size), every BlockRef within [start, end) overlaps
// the range, and no BlockRef outside that index range overlaps.
//
// caller-snapshot-wins: sortedness is the caller's
// invariant; this test asserts that given a sorted slice, the helper
// produces the unique correct index range.
func TestFindBlocksForRange_Property(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const trials = 1000
	for trial := 0; trial < trials; trial++ {
		// Build sorted random blocks. Cursor monotonically increases so
		// the slice stays sorted; gap > 0 introduces sparse holes.
		n := rng.Intn(20) + 1
		blocks := make([]block.BlockRef, n)
		cursor := uint64(0)
		for i := range blocks {
			gap := uint64(rng.Intn(2_000_000))
			sz := uint32(rng.Intn(4_000_000) + 1)
			blocks[i] = block.BlockRef{Offset: cursor + gap, Size: sz}
			cursor = blocks[i].Offset + uint64(sz)
		}
		offset := uint64(rng.Intn(int(cursor) + 5_000_000))
		size := uint64(rng.Intn(8_000_000) + 1)
		start, end := findBlocksForRange(blocks, offset, size)

		// Invariant 1: every block in [start, end) overlaps the range.
		rangeEnd := offset + size
		for i := start; i < end; i++ {
			bEnd := blocks[i].Offset + uint64(blocks[i].Size)
			if blocks[i].Offset >= rangeEnd || bEnd <= offset {
				t.Fatalf("trial %d: block[%d] {off=%d, size=%d} does not overlap range [%d, %d)",
					trial, i, blocks[i].Offset, blocks[i].Size, offset, rangeEnd)
			}
		}
		// Invariant 2: no block outside [start, end) overlaps.
		for i := 0; i < len(blocks); i++ {
			if i >= start && i < end {
				continue
			}
			bEnd := blocks[i].Offset + uint64(blocks[i].Size)
			if blocks[i].Offset < rangeEnd && bEnd > offset {
				t.Fatalf("trial %d: block[%d] {off=%d, size=%d} overlaps range [%d, %d) but is outside [%d, %d)",
					trial, i, blocks[i].Offset, blocks[i].Size, offset, rangeEnd, start, end)
			}
		}
	}
}
