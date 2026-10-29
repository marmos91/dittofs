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
// the change in the number of ranges, which is at most +1. Nothing here grows
// a full pair's array: an overlapping note collapses its run by shifting it in
// the slots the pair holds, and a note that overlaps none merges one adjacent
// pair, in place, before — or around — the range it adds. A pair therefore
// costs at most the eight ranges it keeps, in capacity as well as in length.
//
// decision: every record with an extent is noted, whatever its kind: content,
// fill, repack copy, removal, release, loss, offload. The drop query asks only
// that no record of the file in another segment overlap the extent below the
// pair, so noting more records only over-covers, which keeps a removal
// longer and never drops one it needs. A record with no extent marks its
// segment and adds no range. Overturn it when the headroom gauge shows
// removals pinned by ranges grown from records that can never outrank
// anything, such as the mark of an offload, which claims a record's place
// without content of its own to lose — those kinds would then be skipped at
// noting, and the headroom their over-covering holds would come back.
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
		// The collapsed run is shifted in place: the merged range takes the
		// run's first slot and the rest move left, so a full pair both stays
		// full and allocates nothing.
		s.ranges[i] = merged
		s.ranges = append(s.ranges[:i+1], s.ranges[j:]...)
		return len(s.ranges) - before
	}
	if len(s.ranges) < maxDropRanges {
		r := dropRange{start: lo, end: hi, low: n}
		s.ranges = append(s.ranges, dropRange{})
		copy(s.ranges[i+1:], s.ranges[i:])
		s.ranges[i] = r
		return len(s.ranges) - before
	}
	// The ninth range: the pair is full and the arriving range overlaps none
	// of the eight, so one adjacent pair merges before — or around — it. The
	// pair chosen is the one with the smallest gap of the nine: the seven
	// between old ranges and the two beside the new one, the lowest offsets on
	// a tie. Everything happens in the eight slots the pair holds.
	//
	// decision: which two ranges the ninth-range merge collapses depends
	// on the order records arrive in, so a rebuild that notes the same
	// records in another order can give different ranges. Rebuilt ranges
	// are therefore compared with an incremental build for soundness
	// only — the same segments, and never under-covering the records —
	// and not for equality. Overturn it if a consumer needs identical
	// ranges, which would cost keeping records rather than ranges.
	k, best, lowGap := -1, int64(^uint64(0)>>1), int64(^uint64(0)>>1)
	for m := 1; m < len(s.ranges); m++ {
		if m == i {
			continue // the gap beside the new range, not between old ones
		}
		// The pair's position in the nine: the new range at i lifts every
		// old range above it by one, and a tie takes the lowest.
		p := int64(m - 1)
		if m > i {
			p = int64(m)
		}
		if g := s.ranges[m].start - s.ranges[m-1].end; g < best || (g == best && p < lowGap) {
			k, best, lowGap = m-1, g, p
		}
	}
	// The two gaps beside the new range are weighed with the old pairs' best
	// in one choice, not searched one at a time: a left-first search that
	// commits before the right gap is known merges the wrong side whenever
	// the right gap is the smaller. The candidates carry their position in
	// the nine, so a tie takes the lowest offsets; a gap absent for the
	// ninth sitting at an end is longer than any gap there.
	inf := int64(^uint64(0) >> 1)
	leftGap, leftPos := inf, int64(-1)
	if i > 0 {
		leftGap, leftPos = lo-s.ranges[i-1].end, int64(i-1)
	}
	rightGap, rightPos := inf, int64(i)
	if i < len(s.ranges) {
		rightGap = s.ranges[i].start - hi
	} else {
		rightPos = inf
	}
	switch {
	case (leftGap < best || leftGap == best && leftPos < lowGap) && (leftGap < rightGap || leftGap == rightGap && leftPos < rightPos):
		// the new range merges with its left neighbour, in that slot
		nb := s.ranges[i-1]
		if nb.low.Compare(n) < 0 {
			n = nb.low
		}
		s.ranges[i-1] = dropRange{start: min(lo, nb.start), end: max(hi, nb.end), low: n}
		return len(s.ranges) - before
	case (rightGap < best || rightGap == best && rightPos < lowGap) && (rightGap < leftGap || rightGap == leftGap && rightPos < leftPos):
		// the new range merges with its right neighbour, in that slot
		nb := s.ranges[i]
		if nb.low.Compare(n) < 0 {
			n = nb.low
		}
		s.ranges[i] = dropRange{start: min(lo, nb.start), end: max(hi, nb.end), low: n}
		return len(s.ranges) - before
	}
	// An old pair merges, in place, and the new range takes its slot in the
	// seven that remain.
	if s.ranges[k+1].low.Compare(s.ranges[k].low) < 0 {
		s.ranges[k].low = s.ranges[k+1].low
	}
	s.ranges[k].end = s.ranges[k+1].end
	s.ranges = append(s.ranges[:k+1], s.ranges[k+2:]...)
	at := i
	if k+1 < i {
		at = i - 1
	}
	s.ranges = append(s.ranges, dropRange{})
	copy(s.ranges[at+1:], s.ranges[at:])
	s.ranges[at] = dropRange{start: lo, end: hi, low: n}
	return len(s.ranges) - before
}

// overlaps reports whether the range [r.start, r.end) strictly overlaps
// [lo, hi): one that only touches does not.
func (r dropRange) overlaps(lo, hi int64) bool { return r.start < hi && lo < r.end }
