package engine

import (
	"sort"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// findBlocksForRange returns [start, end) indices into blocks (sorted by
// Offset) covering the byte range [offset, offset+size). Used by ReadAt
// and the prefetch-hint path. O(log n).
//
// Sparse holes (gaps between consecutive BlockRefs that overlap the
// range) are NOT skipped — the caller's responsibility is to zero-fill
// those bytes.
//
// Empty blocks input returns (0, 0). A range entirely before the first
// BlockRef returns (0, 0) (zero-width slice at the head). A range
// entirely after the last returns (len, len) (zero-width slice at the
// tail). A zero-size range always returns (0, 0).
//
// Caller invariant (caller-snapshot-wins): blocks MUST be sorted
// by Offset. The metadata-store conformance suite verifies
// that PutFile/GetFile preserve sort order.
func findBlocksForRange(blocks []blockstore.BlockRef, offset, size uint64) (start, end int) {
	if len(blocks) == 0 || size == 0 {
		return 0, 0
	}
	rangeEnd := offset + size

	// start = first index whose (Offset+Size) > offset (i.e., the first
	// block that actually contains or follows offset).
	start = sort.Search(len(blocks), func(i int) bool {
		return uint64(blocks[i].Size)+blocks[i].Offset > offset
	})

	// end = first index whose Offset >= rangeEnd. Blocks at indices
	// [start, end) overlap the range.
	end = sort.Search(len(blocks), func(i int) bool {
		return blocks[i].Offset >= rangeEnd
	})

	if start > end {
		// Defensive: should not happen with sorted input.
		return 0, 0
	}
	return start, end
}
