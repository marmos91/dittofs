package index

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// maxDropRanges is the most ranges kept for one file in one segment.
const maxDropRanges = 8

// dropRange covers the bytes [start, end) of a file in one segment and carries
// the lowest pair of any record of the file there inside it.
//
// ponytail: eight ranges per (file, segment) keep a removal alive while any
// range of an older record's segment overlaps it, so a file whose small writes
// scatter across a segment can pin its removals until that segment is
// repacked. Upgrade to a coarse bitmap per (file, segment) — one bit per fixed
// run of the file, with the lowest pair per set run — when the headroom gauge
// shows carried removal records pinning it on a random-overwrite workload.
type dropRange struct {
	start, end int64
	low        Numbers
}

// segRecords is one segment holding records of a file, with at most
// maxDropRanges ranges covering the extents of those records. The ranges are
// sorted by start and never overlap, though two may touch.
type segRecords struct {
	seg    core.SegmentID
	ranges []dropRange
}

// note covers e with a range carrying a pair no greater than n, and returns
// the change in the number of ranges, which is at most +1.
//
// decision: every record with an extent is noted, whatever its kind: content,
// fill, repack copy, removal, release, loss, offload. The drop query asks only
// that no record of the file in another segment overlap the extent below the
// pair, so noting more records only over-covers, which keeps a removal
// longer and never drops one it needs. A record with no extent marks its
// segment and adds no range.
func (s *segRecords) note(e core.Extent, n Numbers) int {
	if e.Len == 0 {
		// A record with no extent marks the segment only.
		return 0
	}
	lo, hi := e.Off, e.Off+e.Len
	before := len(s.ranges)
	// The run of ranges strictly overlapping [lo, hi): ones that only touch
	// do not overlap, so a record ending where another range begins stays a
	// range of its own.
	i, j := 0, 0
	for i < len(s.ranges) && s.ranges[i].end <= lo {
		i++
	}
	j = i
	for j < len(s.ranges) && s.ranges[j].start < hi {
		j++
	}
	low := n
	if j > i {
		// decision: a record that overlaps existing ranges is merged into
		// them rather than counted toward the ninth. Merging over-covers,
		// which keeps a removal longer and never drops one it needs, and it
		// keeps the ranges disjoint, so every record lies inside exactly one
		// range. Overturn it if the headroom gauge shows removals pinned by
		// merged overlaps.
		for _, r := range s.ranges[i:j] {
			if r.low.Compare(low) < 0 {
				low = r.low
			}
		}
		merged := dropRange{start: min(lo, s.ranges[i].start), end: max(hi, s.ranges[j-1].end), low: low}
		s.ranges = append(s.ranges[:i], append([]dropRange{merged}, s.ranges[j:]...)...)
	} else {
		r := dropRange{start: lo, end: hi, low: n}
		s.ranges = append(s.ranges, dropRange{})
		copy(s.ranges[i+1:], s.ranges[i:])
		s.ranges[i] = r
	}
	// A ninth range merges the adjacent pair with the smallest gap between
	// them. Step 2 adds at most one range, so one merge is ever needed.
	if len(s.ranges) > maxDropRanges {
		// decision: which two ranges the ninth-note merges depends on the
		// order records arrive in, so a rebuild that notes the same records
		// in another order can give different ranges. Rebuilt ranges are
		// therefore ever only compared with an incremental build for
		// soundness — the same segments, and never under-covering the
		// records — and not for equality. Overturn it if a consumer needs
		// identical ranges, which would cost keeping records rather than
		// ranges.
		k, gap := 0, int64(^uint64(0)>>1)
		for m := 1; m < len(s.ranges); m++ {
			if g := s.ranges[m].start - s.ranges[m-1].end; g < gap {
				k, gap = m-1, g
			}
		}
		s.ranges[k].end = s.ranges[k+1].end
		if s.ranges[k+1].low.Compare(s.ranges[k].low) < 0 {
			s.ranges[k].low = s.ranges[k+1].low
		}
		s.ranges = append(s.ranges[:k+1], s.ranges[k+2:]...)
	}
	return len(s.ranges) - before
}

// overlaps reports whether the range [r.start, r.end) strictly overlaps
// [lo, hi): one that only touches does not.
func (r dropRange) overlaps(lo, hi int64) bool { return r.start < hi && lo < r.end }
