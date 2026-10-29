package index

import (
	"maps"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// SegmentBytes is what a segment backs: the bytes of the held extents and of
// the retained extents whose record sits in it.
//
// decision: held and retained are kept as two counters rather than one "live
// bytes" figure. Repack's candidacy wants their sum, while per-segment
// inspection and hold accounting want them apart; the consumer adds them.
// Overturned if no consumer ever reads them apart. The inspection read-out
// (E8) must report Held+Retained as §4.4's "held bytes", since RFC 1 §3.11
// counts retained extents in them; if it ever reports the fields separately
// there, the RFC wording needs aligning rather than the counter.
//
// Held and Retained, loaded one beside the other, are not a consistent
// snapshot: a re-hold moves bytes between the two counters with nothing
// guarding the pair, so a read between the two loads sees the move in both
// fields or in neither. live() is the consistent figure for reclamation's
// candidacy; it never dips below what the sets hold while an update runs,
// while the sum of the two fields can.
type SegmentBytes struct {
	Held     int64
	Retained int64
}

// segCount is one segment's counters. They are signed so that a negative value —
// an accounting bug — is visible to tests instead of wrapping.
type segCount struct {
	// live is Held + Retained, kept beside the two so a reader can take one
	// consistent figure without a lock: the mutating hooks add before they
	// subtract and failWindow holds a non-written segment's subtractions
	// pending (see retainedSet.holdGone), so live is never below what the sets
	// hold while an update runs.
	live     atomic.Int64
	held     atomic.Int64
	retained atomic.Int64
}

// segments is the store-wide table of per-segment counters: one entry per
// segment that backs anything, answered in O(1) by reclamation and inspection.
//
// ponytail: a copy-on-write map: every segment creation and retirement copies
// the map, O(S) for the S live segments, once per segment written (256 MiB); in
// exchange the hot path — every append's accounting, every query — takes no
// lock and allocates nothing. Upgrade to a sharded map when segment counts make
// that copy show in a profile.
type segments struct {
	// mu is a leaf lock, exempt from the rule that no lock spans the store:
	// it is taken about once per segment created or retired, never held
	// across I/O or a file's lock, and nothing is acquired after it — the
	// O(S) map copy is its only work, and the hot path takes it not at all.
	// Overturned if a profile shows write-path stalls on it, which many
	// segments created at once across many files could cause.
	mu sync.Mutex                                   // serialises creating and retiring entries
	m  atomic.Pointer[map[core.SegmentID]*segCount] // copy-on-write; nil = empty
}

// add changes seg's counters by held and retained, creating the entry when the
// segment backs nothing yet. The hot path — a delta on an existing entry —
// takes no lock and allocates nothing; only a missing entry copies the map.
// A delta of zero on one field skips that field's adds.
func (ss *segments) add(seg core.SegmentID, held, retained int64) {
	if m := ss.m.Load(); m != nil {
		if c := (*m)[seg]; c != nil {
			c.add(held, retained)
			return
		}
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	m := ss.m.Load()
	if m == nil || (*m)[seg] == nil {
		// The copy starts empty with the new entry so a first add allocates
		// only its map.
		copied := map[core.SegmentID]*segCount{seg: new(segCount)}
		if m != nil {
			maps.Copy(copied, *m)
		}
		ss.m.Store(&copied)
		m = &copied
	}
	(*m)[seg].add(held, retained)
}

// bytes returns seg's counters, in O(1) and allocating nothing. A segment the
// table holds no entry for — one that never backed anything, or one retired —
// reads zero. The two fields are loaded one beside the other with no guard,
// so a move between them can be seen twice or not at all; live answers what
// the segment backs as one figure.
func (ss *segments) bytes(seg core.SegmentID) SegmentBytes {
	if c := ss.entry(seg); c != nil {
		return SegmentBytes{Held: c.held.Load(), Retained: c.retained.Load()}
	}
	return SegmentBytes{}
}

// live returns the bytes seg backs as held and retained extents, in O(1) and
// allocating nothing. Unlike the two fields of bytes, this is one counter, so
// a re-hold moving bytes between held and retained cannot hide them from or
// double them to a concurrent read.
//
// decision: a sealed segment with no unresolved window that reads live 0 is
// empty for good. No update lowers a segment below both its value before and
// after it: the hooks add before they subtract, and failWindow holds the
// subtractions of a segment it does not write to until it has finished its
// pass (see retainedSet.holdGone), while the dip on the failed record's own
// segment is a segment an unresolved window still pins. So reclamation reading
// 0 on such a segment may retire it without quiescing anything. Overturned if
// a path is found that empties a segment and is later undone.
func (ss *segments) live(seg core.SegmentID) int64 {
	if c := ss.entry(seg); c != nil {
		return c.live.Load()
	}
	return 0
}

// entry returns seg's entry, or nil when the table has none.
func (ss *segments) entry(seg core.SegmentID) *segCount {
	if m := ss.m.Load(); m != nil {
		return (*m)[seg]
	}
	return nil
}

// retire removes seg's entry and returns what it still counted, and whether it
// was there. A second retire of a segment reports false and zeros, so
// retiring a segment is idempotent; counting and logging the double retire is
// the caller's.
//
// decision: retire neither walks the files nor refuses: by the time the index
// hears of an unlink the file is gone. The contract is that a segment is
// retired only once it backs nothing — its live byte count is zero, reached
// with no unresolved window on it (see live) — and the leftover is returned so
// the caller can log a violation. A delta arriving after retire for that
// segment would recreate the entry and break walk equality — a caller bug, not
// handled here — and so is an add that loaded the entry before retire's store
// landed on the orphaned entry and was lost with it: the same caller-bug class
// (retire while an update of the segment is in flight). Overturned if a
// reclamation path is found that must retire a segment still backing content.
func (ss *segments) retire(seg core.SegmentID) (SegmentBytes, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	m := ss.m.Load()
	if m == nil {
		return SegmentBytes{}, false
	}
	c := (*m)[seg]
	if c == nil {
		return SegmentBytes{}, false
	}
	copied := maps.Clone(*m)
	delete(copied, seg)
	ss.m.Store(&copied)
	return SegmentBytes{Held: c.held.Load(), Retained: c.retained.Load()}, true
}

// add changes the counters by held and retained, keeping live beside them.
// The three are independent atomics with no ordering to preserve: the
// mutating hooks already order every add before its subtract, so nothing in
// here can dip a counter a reader is watching. A delta of zero on a field skips
// that field's atomics.
func (c *segCount) add(held, retained int64) {
	c.live.Add(held + retained)
	if held != 0 {
		c.held.Add(held)
	}
	if retained != 0 {
		c.retained.Add(retained)
	}
}
