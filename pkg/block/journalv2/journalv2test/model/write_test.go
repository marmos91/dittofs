package model

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// A later write supersedes the part of an earlier one it overlaps, and the
// earlier write keeps the part it does not: the held extents afterwards are
// (0, 4K) at v7 and (4K, 8K) at v8.
func TestWriteSupersedesWhereItOverlaps(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 8<<10), 0, ver(7), nil))
	ok(t, m.WriteAt(shareA, fid(1), 4<<10, fill('B', 8<<10), 0, ver(8), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 4 << 10, 7, false},
		{4 << 10, 8 << 10, 8, false},
	})
}

// An overwrite inside one held extent splits it into three.
func TestWriteInsideAnExtentSplitsItInThree(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 300), 0, ver(1), nil))
	ok(t, m.WriteAt(shareA, fid(1), 100, fill('B', 100), 0, ver(2), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 100, 1, false},
		{100, 100, 2, false},
		{200, 100, 1, false},
	})
	if got := string(heldBytes(t, m, fid(1), 99, 3)); got != "ABB" {
		t.Fatalf("bytes 99..102 = %q, want %q", got, "ABB")
	}
}

// Held content need not begin at offset zero nor be contiguous: what was never
// written is simply not held.
func TestHeldContentNeedNotStartAtZeroNorBeContiguous(t *testing.T) {
	m := New()
	far := int64(1) << 40
	ok(t, m.WriteAt(shareA, fid(1), far, fill('A', 10), 0, ver(1), nil))
	ok(t, m.WriteAt(shareA, fid(1), far+20, fill('B', 10), 0, ver(2), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{far, 10, 1, false},
		{far + 20, 10, 2, false},
	})
}

// Every version the journal assigns exceeds every version before it, so a
// write reporting a version not above the highest seen is not a journal the
// RFC allows.
func TestWriteVersionMustExceedEveryVersionSeen(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 1), 0, ver(5), nil))
	refused(t, m.WriteAt(shareA, fid(2), 0, fill('A', 1), 0, ver(5), nil))
	refused(t, m.WriteAt(shareA, fid(2), 0, fill('A', 1), 0, ver(4), nil))
	refused(t, m.WriteAt(shareA, fid(2), 0, fill('A', 1), 0, journalv2.Version{}, nil))
}

// A refused write changes nothing, and an error the model has no rule for is
// reported rather than absorbed.
func TestWriteErrorsTheModelHasNoRuleForAreReported(t *testing.T) {
	m := New()
	refused(t, m.WriteAt(shareA, fid(1), 0, fill('A', 1), 0, ver(1), journalv2.ErrCorrupt))
	if got := heldSpans(t, m, fid(1)); len(got) != 0 {
		t.Fatalf("a failed write left %+v held", got)
	}
}

// A file belongs to the share whose handle first appended to it: a call
// through another share's handle is refused with ErrOtherShare while the file
// holds anything.
func TestAFileIsItsSharesWhileItHoldsAnything(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))

	refused(t, m.WriteAt(shareB, fid(1), 0, fill('B', 4), 0, ver(2), nil))
	ok(t, m.WriteAt(shareB, fid(1), 0, fill('B', 4), 0, journalv2.Version{}, journalv2.ErrOtherShare))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 4, 1, false}})

	// Refusing a file no other share holds is wrong.
	refused(t, m.WriteAt(shareA, fid(2), 0, fill('A', 4), 0, journalv2.Version{}, journalv2.ErrOtherShare))
}

// Once a file holds nothing, another share's call on it may claim it: the
// journal keeps the tag only while a record of the file is on disk, which the
// caller cannot see.
func TestAnEmptiedFileMayBeClaimedByAnotherShare(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 4), ver(2), nil))
	m.Settle(shareA, fid(1), ver(2))
	ok(t, m.WriteAt(shareB, fid(1), 0, fill('B', 4), 0, ver(3), nil))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 4, 3, false}})
}
