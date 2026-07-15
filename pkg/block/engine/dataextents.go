package engine

import (
	"context"
	"sort"

	"github.com/marmos91/dittofs/pkg/block"
)

// DataExtents returns the sorted, non-overlapping byte ranges [start, end)
// within [0, fileSize) that hold data across ALL tiers the engine reads from:
// the local append log + in-memory buffer (via local.DataExtents) UNION the
// persisted CAS FileChunk manifest. This is the same data view ReadAt
// reconstructs, expressed as a hole map.
//
// NFSv4.2 SEEK and READ_PLUS use it instead of deriving holes from the
// persisted CAS block list alone: that list is empty/partial for
// written-but-not-yet-rolled-up data, so a CAS-only map reports a hole where
// data exists — which RFC 7862 forbids (a sparse-copy client skips the real
// data, silently losing it). #1481.
func (bs *Store) DataExtents(ctx context.Context, payloadID string, fileSize uint64) ([][2]uint64, error) {
	if err := bs.enter(); err != nil {
		return nil, err
	}
	defer bs.closeMu.RUnlock()
	if fileSize == 0 {
		return nil, nil
	}

	// (a) Bytes the local journal tier knows (dirty or evicted/cold).
	ext, err := bs.local.DataExtents(ctx, payloadID, int64(fileSize))
	if err != nil {
		return nil, err
	}

	// (b) Persisted CAS chunks (post-rollup bytes). Optional store in tests.
	if bs.fileChunkStore != nil {
		rows, lerr := bs.fileChunkStore.ListFileChunks(ctx, payloadID)
		if lerr != nil {
			return nil, lerr
		}
		for _, fb := range rows {
			if fb == nil || fb.Hash.IsZero() {
				continue // pending/incomplete chunk — no committed bytes yet
			}
			off, ok := block.ParseChunkOffset(fb.ID)
			if !ok || off >= fileSize {
				continue
			}
			end := off + uint64(fb.DataSize)
			if end > fileSize {
				end = fileSize
			}
			if end <= off {
				continue
			}
			ext = append(ext, [2]uint64{off, end})
		}
	}

	return coalesceExtents(ext), nil
}

// coalesceExtents sorts ext by start and merges overlapping/adjacent ranges
// into the canonical sorted, non-overlapping form. Mutates the input backing
// array; returns nil for empty input.
func coalesceExtents(ext [][2]uint64) [][2]uint64 {
	if len(ext) == 0 {
		return nil
	}
	sort.Slice(ext, func(i, j int) bool { return ext[i][0] < ext[j][0] })
	merged := ext[:1]
	for _, e := range ext[1:] {
		last := &merged[len(merged)-1]
		if e[0] <= last[1] {
			if e[1] > last[1] {
				last[1] = e[1]
			}
			continue
		}
		merged = append(merged, e)
	}
	return merged
}
