package fs

import (
	"time"

	"github.com/google/btree"
)

// interval is a dirty range recorded in the per-file interval tree (D-16).
//
// Offset is the byte offset in the logical payload file; Length is the
// byte length of the dirty region; Touched is the wall-clock time the
// interval was last updated (used by EarliestStable to honor the
// stabilization window before the rollup consumes a region).
type interval struct {
	Offset  uint64
	Length  uint32
	Touched time.Time
}

// less orders intervals by Offset, breaking ties by Touched so two
// entries with the same Offset and Touched are still distinguishable by
// the btree — otherwise ReplaceOrInsert would coalesce them and the tree
// would drop legitimate appends at the same offset (see D-35 "last record
// in log wins; rollup respects log order" — the interval tree keeps both
// entries; log order is the tiebreaker the rollup uses).
func (a *interval) less(b *interval) bool {
	if a.Offset != b.Offset {
		return a.Offset < b.Offset
	}
	if !a.Touched.Equal(b.Touched) {
		return a.Touched.Before(b.Touched)
	}
	if a.Length != b.Length {
		return a.Length < b.Length
	}
	return false
}

// intervalTree is a per-file dirty-region tracker.
// Backed by github.com/google/btree (BTreeG) for O(log n) insert + scan.
//
// Not safe for concurrent use — callers must serialize via the per-file
// mutex (D-32). AppendWrite acquires the mutex around writeRecord + Insert,
// the rollup acquires it around EarliestStable + ConsumeUpTo.
type intervalTree struct {
	t *btree.BTreeG[*interval]
}

func newIntervalTree() *intervalTree {
	return &intervalTree{
		t: btree.NewG(32, func(a, b *interval) bool { return a.less(b) }),
	}
}

// Len returns the number of dirty intervals currently tracked.
func (it *intervalTree) Len() int { return it.t.Len() }

// Insert records a dirty region (off, off+length) touched at `now`.
// Overlapping regions keep both entries; merging is the rollup's job
// (D-35). O(log n).
func (it *intervalTree) Insert(off uint64, length uint32, now time.Time) {
	it.t.ReplaceOrInsert(&interval{Offset: off, Length: length, Touched: now})
}

// EarliestStable returns the interval with the smallest Offset whose
// Touched timestamp is older than `now - stabilization` (inclusive).
//
// The scan walks from low to high Offset. If the LOWEST-offset interval
// is still unstable (Touched > threshold), no stable interval can be
// returned: skipping ahead to a later-offset stable entry would let the
// rollup advance past data that has not yet met the stabilization window,
// filling the resulting gap with zeros when reconstructStream materializes
// the contiguous byte buffer (D-16, D-35). The correct behavior is to
// wait (return not-found) rather than emit out-of-order / zero-holed
// chunks.
//
// Invariant preserved: rollup_offset only advances over intervals that
// ALL met the stabilization window at the time of the pass.
func (it *intervalTree) EarliestStable(now time.Time, stabilization time.Duration) (interval, bool) {
	threshold := now.Add(-stabilization)
	var found *interval
	it.t.Ascend(func(iv *interval) bool {
		if iv.Touched.After(threshold) {
			// Lowest-offset interval (or lowest-offset after any already-
			// accepted stable prefix) is still unstable — cannot return
			// anything at or past this offset without risking a hole-fill
			// with zeros on reconstruction. Bail out; the rollup ticker
			// will retry once this interval stabilizes.
			found = nil
			return false
		}
		found = iv
		return false // earliest stable wins; stop
	})
	if found == nil {
		return interval{}, false
	}
	return *found, true
}

// ConsumeUpTo drops every interval whose end (Offset+Length) is <=
// endExclusive. Called by the rollup after chunks covering [0, endExclusive)
// have been durably committed.
func (it *intervalTree) ConsumeUpTo(endExclusive uint64) {
	var toDelete []*interval
	it.t.Ascend(func(iv *interval) bool {
		if uint64(iv.Length)+iv.Offset <= endExclusive {
			toDelete = append(toDelete, iv)
		}
		return true
	})
	for _, iv := range toDelete {
		it.t.Delete(iv)
	}
}

// DropAbove removes intervals whose Offset >= boundary, and clips
// intervals where Offset < boundary < Offset+Length so they end at
// boundary. Used by Truncate (D-29).
func (it *intervalTree) DropAbove(boundary uint64) {
	var toDelete []*interval
	var toClip []*interval
	it.t.Ascend(func(iv *interval) bool {
		if iv.Offset >= boundary {
			toDelete = append(toDelete, iv)
		} else if iv.Offset+uint64(iv.Length) > boundary {
			toClip = append(toClip, iv)
		}
		return true
	})
	for _, iv := range toDelete {
		it.t.Delete(iv)
	}
	for _, iv := range toClip {
		newLen := uint32(boundary - iv.Offset)
		it.t.Delete(iv)
		it.t.ReplaceOrInsert(&interval{Offset: iv.Offset, Length: newLen, Touched: iv.Touched})
	}
}
