package fs

import (
	"context"
	"sort"
)

// DataExtents returns the sorted, non-overlapping byte ranges the fs local
// tier's append log knows hold data within [0, fileSize). It derives them from
// the per-payload logIndex — the same oracle ReadPayloadAt's index-assisted
// replay uses — so SEEK / READ_PLUS see exactly the pre-rollup bytes a READ
// would reconstruct from the log.
//
// It intentionally excludes the CAS FileChunk manifest (and the logIndex's
// consumedCoverage, which mirrors regions already rolled into CAS): the engine
// unions the persisted CAS extents on top of this result, so reporting them
// here too would be redundant. The append log is the ONLY place pre-rollup
// bytes live, and that is the gap the persisted block list misses (#1481).
//
// Implements local.LocalStore.
func (bc *FSStore) DataExtents(ctx context.Context, payloadID string, fileSize uint64) ([][2]uint64, error) {
	if fileSize == 0 {
		return nil, nil
	}
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sh := bc.shardFor(payloadID)
	sh.mu.RLock()
	idx := sh.logIndices[payloadID]
	sh.mu.RUnlock()
	if idx == nil {
		return nil, nil
	}

	// Every committed append-log record overlapping [0, fileSize) contributes
	// its file-offset extent. EntriesForInterval returns arrival order; we
	// re-sort + coalesce below into the canonical sorted/merged form.
	entries := idx.EntriesForInterval(0, fileSize, nil)
	if len(entries) == 0 {
		return nil, nil
	}
	ext := make([][2]uint64, 0, len(entries))
	for _, e := range entries {
		start := e.fileOff
		if start >= fileSize {
			continue
		}
		end := e.fileEnd()
		if end > fileSize {
			end = fileSize
		}
		if end <= start {
			continue
		}
		ext = append(ext, [2]uint64{start, end})
	}
	return coalesceExtents(ext), nil
}

// coalesceExtents sorts ext by start and merges overlapping/adjacent ranges,
// returning the canonical sorted, non-overlapping form. Mutates and reuses the
// input backing array. Returns nil for an empty input.
func coalesceExtents(ext [][2]uint64) [][2]uint64 {
	if len(ext) == 0 {
		return nil
	}
	sort.Slice(ext, func(i, j int) bool { return ext[i][0] < ext[j][0] })
	merged := ext[:1]
	for _, e := range ext[1:] {
		last := &merged[len(merged)-1]
		if e[0] <= last[1] { // touching or overlapping -> extend
			if e[1] > last[1] {
				last[1] = e[1]
			}
			continue
		}
		merged = append(merged, e)
	}
	return merged
}
