package journal

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// IsRangeResident reports whether every byte in [offset, offset+length) has a
// live, non-cold interval. Dirty bytes qualify: they are the newest local copy.
// This is an index-only snapshot, not an integrity check or an eviction pin.
// Its cost is O(log N + K), where K is the number of intersected intervals.
func (s *Store) IsRangeResident(ctx context.Context, id FileID, offset, length int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.closed.Load() {
		return false, errClosed
	}
	if offset < 0 || length < 0 || length > math.MaxInt64-offset {
		return false, fmt.Errorf("journal: invalid residency range offset=%d length=%d", offset, length)
	}
	if length == 0 {
		return true, nil
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return false, nil
	}
	end := offset + length
	i := sort.Search(len(fi.ivs), func(i int) bool { return fi.ivs[i].end() > offset })
	for pos := offset; i < len(fi.ivs); i++ {
		iv := fi.ivs[i]
		if iv.fileOff > pos || iv.cold {
			return false, nil
		}
		pos = iv.end()
		if pos >= end {
			return true, nil
		}
	}
	return false, nil
}
