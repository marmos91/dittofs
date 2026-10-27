//go:build journal_conformance

package conformance

import (
	"errors"
	"fmt"
	"slices"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// The checks here fail by serving or destroying the wrong bytes without an
// error, through the on-disk format or recovery.

// rec is one write a check made and the bytes it wrote.
type rec struct {
	share   int
	id      journalv2.FileID
	extent  journalv2.Extent
	version journalv2.Version
	p       []byte
}

// writeSynced writes one record of size bytes at off of file i through share
// s and syncs it.
func writeSynced(t *testing.T, f *journalv2test.Fixture, s, i int, off int64, size int) rec {
	t.Helper()

	id := journalv2test.File(i)
	p := pattern(fmtTag("s", s, i, off), size)
	v := write(t, f.Share(s), id, off, p)
	syncFiles(t, f.Share(s), id)

	return rec{s, id, journalv2.Extent{Off: off, Len: int64(size)}, v, p}
}

// fmtTag names a write by its share, file and offset, so no two writes of a
// check share a pattern.
func fmtTag(kind string, s, i int, off int64) string {
	return fmt.Sprintf("%s%d.f%d@%d", kind, s, i, off)
}

// corruptAndRead damages A's record on disk, reads it, and returns the loss
// event the read raised and the stats from before the damage. The read must
// report A's whole extent missing and leave its buffer untouched.
func corruptAndRead(t *testing.T, f *journalv2test.Fixture, a rec) (journalv2.LossEvent, journalv2.Stats) {
	t.Helper()

	before := f.J.Stats()
	corrupt(t, f, a.id, a.p)
	wantMissing(t, f.Share(a.share), a.id, a.extent)

	ev := lastLoss(t, f, before)
	if ev.Share != journalv2test.Tag(a.share) || ev.File != a.id || ev.Extent != a.extent || ev.Reason != journalv2.LossCorrupt {
		t.Fatalf("loss event %+v, want share %d, file %v, extent %+v, dropped as corrupt", ev, journalv2test.Tag(a.share), a.id, a.extent)
	}

	return ev, before
}

// TestRFC28A_CorruptDirty checks that a corrupt record whose content was
// never offloaded is reported as data loss: the read says the extent is
// missing, writes nothing into the caller's buffer, and the event says the
// bit was unset, since no other copy of those bytes exists.
func TestRFC28A_CorruptDirty(t *testing.T) {
	f := journalv2test.New(t)
	a := writeSynced(t, f, 0, 0, 0, 64<<10)
	b := writeSynced(t, f, 1, 1, 0, 64<<10)

	// The reseed after a restart marks what the remote tier holds: B here,
	// and nothing of A, which stays dirty.
	f.Reopen()
	mark(t, f.Share(1), b.id, b.extent, b.version)

	ev, before := corruptAndRead(t, f, a)
	if ev.Offloaded {
		t.Fatalf("loss event %+v says the bit was set; A was never offloaded", ev)
	}
	// A loss of content not yet offloaded raises the generation by one, so
	// every client resends the writes it has not seen confirmed stable.
	if got, want := f.J.Stats().LossGeneration, before.LossGeneration+1; got != want {
		t.Fatalf("LossGeneration = %d after dropping dirty content, want %d", got, want)
	}

	wantBytes(t, f.Share(1), b.id, 0, b.p)
}

// TestRFC28A_CorruptOffloaded checks that a corrupt record whose content the
// remote tier holds is reported as repairable: the read says the extent is
// missing, the event says the bit was set, and the loss generation does not
// move, since the next read fetches the content and no client needs to
// resend anything.
func TestRFC28A_CorruptOffloaded(t *testing.T) {
	f := journalv2test.New(t)
	a := writeSynced(t, f, 0, 0, 0, 64<<10)
	b := writeSynced(t, f, 1, 1, 0, 64<<10)

	f.Reopen()
	mark(t, f.Share(0), a.id, a.extent, a.version)
	mark(t, f.Share(1), b.id, b.extent, b.version)

	ev, before := corruptAndRead(t, f, a)
	if !ev.Offloaded {
		t.Fatalf("loss event %+v says the bit was unset; A was marked offloaded", ev)
	}

	if got := f.J.Stats().LossGeneration; got != before.LossGeneration {
		t.Fatalf("LossGeneration = %d after dropping offloaded content, want %d unchanged", got, before.LossGeneration)
	}

	wantBytes(t, f.Share(1), b.id, 0, b.p)
}

// TestRFC28A_DirectoryEntrySurvives checks that a record synced into a
// segment the journal took since the last directory sync survives a crash
// that drops every directory change not yet synced: the segment's name must
// be durable before the sync covering the record returns, or the record is
// gone with the name.
func TestRFC28A_DirectoryEntrySurvives(t *testing.T) {
	f := journalv2test.New(t)
	const size = 256 << 10
	recs, first := fillSegment(t, f, 0, 0, size)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)

	// The write that rolled the stream took a segment the journal had made
	// ahead; A follows it there.
	off := int64(len(recs)+1) * size
	a := writeSynced(t, f, 0, 0, off, 64<<10)
	seg := entryAt(t, f, a.id, off).Location.Segment
	if seg == first {
		t.Fatalf("A landed in the first segment %d, not in the one the roll took", seg)
	}
	name := segmentName(seg)

	f.Crash()

	names, err := f.Seam.List()
	if err != nil {
		t.Fatalf("list the journal's directory: %v", err)
	}
	if !slices.Contains(names, name) {
		t.Fatalf("segment %s is gone after a crash dropped the unsynced directory changes; a record synced into it was acknowledged", name)
	}

	wantBytes(t, f.Share(0), a.id, off, a.p)
	wantBytes(t, f.Share(1), other.id, 0, other.p)
}

// TestRFC28A_AZeroSlotIsNotTheEnd checks that a write whose storage call
// failed, leaving its slot zeros between two writes that succeeded and
// synced, costs nothing but itself: the write after it reads back, nothing
// is reported as damage or as a torn tail, and the segment is not cut below
// it.
func TestRFC28A_AZeroSlotIsNotTheEnd(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	const size = 64 << 10

	// Three reservations on one file, so three slots of one stream, taken in
	// order.
	var rs [3]*journalv2.Reservation
	var ps [3][]byte
	for i := range rs {
		r, err := h.Reserve(size, 1)
		stub(t, "Reserve", err)
		if err != nil {
			t.Fatalf("reserve a write: %v", err)
		}
		rs[i] = r
		ps[i] = pattern(fmtTag("z", 0, 0, int64(i)), size)
	}

	_, err := rs[0].WriteAt(t.Context(), id, 0, ps[0], 0)
	stub(t, "WriteAt", err)
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	name := segmentName(entryAt(t, f, id, 0).Location.Segment)

	f.FailNext(journalv2test.OpWrite, name, 1)
	_, err = rs[1].WriteAt(t.Context(), id, size, ps[1], 0)
	stub(t, "WriteAt", err)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("the write whose storage call failed returned %v, want the device's error", err)
	}

	_, err = rs[2].WriteAt(t.Context(), id, 2*size, ps[2], 0)
	stub(t, "WriteAt", err)
	if err != nil {
		t.Fatalf("the third write: %v", err)
	}
	syncFiles(t, h, id)
	third := entryAt(t, f, id, 2*size).Location
	for _, r := range rs {
		if err := r.Close(); err != nil {
			t.Fatalf("close a reservation: %v", err)
		}
	}

	f.Crash()

	wantBytes(t, f.Share(0), id, 2*size, ps[2])
	s := f.J.Stats()
	if len(s.Losses) != 0 || s.DamagedSegments != 0 {
		t.Fatalf("the failed write's zero slot was reported as damage: losses %+v, DamagedSegments = %d", s.Losses, s.DamagedSegments)
	}
	if s.Counters.TornTails != 0 {
		t.Fatalf("TornTails = %d; the zero slot is neither damage nor a torn tail", s.Counters.TornTails)
	}

	durable, err := f.Seam.Durable(name)
	if err != nil {
		t.Fatalf("read the segment: %v", err)
	}
	if end := third.Offset + 96 + 8 + size; int64(len(durable)) < end {
		t.Fatalf("the segment is %d bytes after recovery, cut below the third record's end at %d", len(durable), end)
	}

	wantBytes(t, f.Share(1), other.id, 0, other.p)
}

// TestRFC28A_UnmarkClears checks that an unmark outlives a restart and that a
// mark over other extents changes no bit: a bit the metadata cannot justify
// would let eviction drop the only copy of the content.
func TestRFC28A_UnmarkClears(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	a := writeSynced(t, f, 0, 0, 0, 64<<10)
	c := writeSynced(t, f, 0, 0, 64<<10, 64<<10)
	b := writeSynced(t, f, 1, 1, 0, 64<<10)

	mark(t, h, a.id, a.extent, a.version)
	mark(t, h, c.id, c.extent, c.version)
	unmark(t, h, a.id, a.extent)
	syncFiles(t, h, a.id)
	wantRelease(t, h, a.id, a.extent, false)

	f.Crash()
	h = f.Share(0)
	wantRelease(t, h, a.id, a.extent, false)

	// A mark over B's extent covers neither A nor C: A stays unmarked and C
	// stays marked.
	mark(t, f.Share(1), b.id, b.extent, b.version)
	if entryAt(t, f, a.id, 0).Offloaded {
		t.Fatalf("A's offloaded bit is set after a mark that did not cover it")
	}
	if !entryAt(t, f, c.id, c.extent.Off).Offloaded {
		t.Fatalf("C's offloaded bit is unset after a mark that did not cover it")
	}
	wantRelease(t, h, a.id, a.extent, false)
}
