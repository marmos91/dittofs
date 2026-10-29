//go:build journal_conformance

package conformance

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
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
	wantExtentMissing(t, f.Share(a.share), a.id, a.extent)

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
	// every client resends the writes it has not seen confirmed stable (RFC 1
	// section 3.8).
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

	// A loss the remote tier repairs leaves the generation where it was (RFC
	// 1 section 3.8).
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
	first, third := entryAt(t, f, id, 0).Location, entryAt(t, f, id, 2*size).Location
	if slot := first.Offset + raw.RecordHeaderSize + 8 + size; third.Offset < slot+raw.RecordHeaderSize+8+size {
		t.Fatalf("precondition: the third record at %d does not lie past the slot at %d the failed write took", third.Offset, slot)
	}

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

	if end := third.Offset + raw.RecordHeaderSize + 8 + size; int64(len(durable)) < end {
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

	// A mark over D, an extent of file 0 outside A and C, covers neither: A
	// stays unmarked and C stays marked.
	d := writeSynced(t, f, 0, 0, 128<<10, 64<<10)
	mark(t, h, d.id, d.extent, d.version)
	if entryAt(t, f, a.id, 0).Offloaded {
		t.Fatalf("A's offloaded bit is set after a mark that did not cover it")
	}
	if !entryAt(t, f, c.id, c.extent.Off).Offloaded {
		t.Fatalf("C's offloaded bit is unset after a mark that did not cover it")
	}

	wantRelease(t, h, a.id, a.extent, false)
	wantBytes(t, f.Share(1), b.id, 0, b.p)
}

// deviceSync makes the pending writes of the segment file name durable
// through the seam, as the operating system's own writeback would, without
// the journal seeing a sync.
func deviceSync(t *testing.T, f *journalv2test.Fixture, name string) {
	t.Helper()

	file, err := f.Seam.Open(name)
	if err != nil {
		t.Fatalf("sync %s behind the journal's back: %v", name, err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		t.Fatalf("sync %s behind the journal's back: %v", name, err)
	}
}

// abandonHeld ends the process while the calls stopped at h are in flight,
// and opens the journal again.
//
// decision: the seam's abandon runs first, so a held call, once released,
// fails as a dead process's; the hold is released second, so the call
// returns; the fixture's abandon runs last, repeating the seam's with nothing
// left to close, and opens the journal on what the process left.
func abandonHeld(t *testing.T, f *journalv2test.Fixture, h *journalv2test.Hold) {
	t.Helper()

	f.Seam.Abandon()
	h.Release()
	f.Abandon()
}

// unproven is a pair of records past the last sync the journal knows of: A
// reached the device behind the journal's back, B only the cache.
type unproven struct {
	a, b       rec
	aLoc, bLoc journalv2.RecordRef
	name       string // the segment holding both
}

// writeUnproven writes A and B on file 0, syncing the segment through the
// seam between them, so A is on the device and B is not, and the journal has
// seen no sync of either.
func writeUnproven(t *testing.T, f *journalv2test.Fixture) unproven {
	t.Helper()

	a := writeRec(t, f, 0, 0, 0, 64<<10)
	aLoc := entryAt(t, f, a.id, 0).Location
	name := segmentName(aLoc.Segment)
	deviceSync(t, f, name)
	b := writeRec(t, f, 0, 0, 64<<10, 64<<10)
	bLoc := entryAt(t, f, b.id, b.extent.Off).Location

	return unproven{a, b, aLoc, bLoc, name}
}

// wantVerifiedThenReappended fails the test unless, after the open that
// followed the process's death, A and B read back with their original
// versions, A where it was and B in another segment, and unless both still
// read back after a crash that drops what no sync covered.
func wantVerifiedThenReappended(t *testing.T, f *journalv2test.Fixture, u unproven) {
	t.Helper()

	check := func(when string) {
		for _, r := range []rec{u.a, u.b} {
			if got := entryAt(t, f, r.id, r.extent.Off).Version; got != r.version {
				t.Fatalf("%s, the record at %d is held at version %v, want its original %v", when, r.extent.Off, got, r.version)
			}
			wantBytes(t, f.Share(0), r.id, r.extent.Off, r.p)
		}
	}
	check("after the open")

	if got := entryAt(t, f, u.a.id, 0).Location; got != u.aLoc {
		t.Fatalf("A moved from %+v to %+v; it verified on the device and was to stay in place", u.aLoc, got)
	}
	if got := entryAt(t, f, u.b.id, u.b.extent.Off).Location; got.Segment == u.bLoc.Segment {
		t.Fatalf("B is still served from segment %d; it did not verify on the device and was to be re-appended", got.Segment)
	}
	if got := f.J.Stats().Counters.SyncReappendedBytes; got < uint64(len(u.b.p)) {
		t.Fatalf("SyncReappendedBytes = %d after re-appending %d bytes", got, len(u.b.p))
	}

	f.Crash()
	check("after the crash")
}

// TestRFC28A_UnprovenTailVerifiedAtOpen checks that a journal whose process
// died trusts no record it cannot prove synced: at open it syncs the newest
// segment, re-reads every record past the last proven sync point from the
// device, keeps the ones that verify there and re-appends the others from
// the copy the cache still serves, so a power loss after the restart takes
// nothing a client was answered. Three legs: a journal with room, one at its
// maximum, and one whose segment's sync failed before the process died.
//
// decision: in the first two legs the open's own sync of the segment is
// dropped at the seam, so it reports success and puts nothing on the device:
// without that, the open would make B durable before it re-read it, and the
// row's case of a record that does not verify on the device could not be
// reached. The third leg drops nothing: its segment's sync failed before the
// process died, so the open re-appends B by the failed window's rule.
func TestRFC28A_UnprovenTailVerifiedAtOpen(t *testing.T) {
	f := journalv2test.New(t)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	u := writeUnproven(t, f)
	f.Drop(journalv2test.OpSync, u.name)
	f.Abandon()
	f.Seam.SetFault(nil)
	wantVerifiedThenReappended(t, f, u)
	wantBytes(t, f.Share(1), other.id, 0, other.p)

	// At the maximum: the re-append draws on the repack reserve, never on a
	// share's limit, so the open succeeds. The fills are synced, so B is the
	// only record the open has to re-append.
	g := journalv2test.NewUnwatched(t)
	u = writeUnproven(t, g)
	filled := map[journalv2.FileID]bool{}
	for _, w := range fillUntilRefused(t, g, []int{1}, 2, 256<<10) {
		filled[w.id] = true
	}
	for id := range filled {
		syncFiles(t, g.Share(1), id)
	}
	g.Drop(journalv2test.OpSync, u.name)
	g.Abandon()
	g.Seam.SetFault(nil)
	wantVerifiedThenReappended(t, g, u)

	// A segment whose sync failed, the process dying before the window was
	// resolved: the journal's resolution, a re-append or a loss record, is a
	// write to a new segment, and the process dies with that write held.
	// The segment names no predecessor, so recovery treats it as one that
	// could have been open at the crash, and the outcome is the same.
	k := journalv2test.NewUnwatched(t)
	u = writeUnproven(t, k)
	k.FailNext(journalv2test.OpSync, u.name, 1)
	held := k.Seam.Hold(t, journalv2test.OpWrite, "*.seg")
	synced := make(chan error, 1)
	go func() { synced <- k.Share(0).Sync(t.Context(), u.a.id) }()
	select {
	case <-held.Reached():
	case err := <-synced:
		stub(t, "Sync", err)
		t.Fatalf("Sync returned (%v) without writing the window's resolution", err)
	}
	abandonHeld(t, k, held)
	<-synced // a call the process died in; what it returned means nothing
	wantVerifiedThenReappended(t, k, u)
}

// sealedSegmentBytes returns the bytes of a segment of this journal sealed
// early: its header, records from the first slot on, each under identity and
// the segment's id, a seal marker at the slot after the last, the entries as
// its footer, and the trailer, with nothing past it.
func sealedSegmentBytes(identity [16]byte, id journalv2.SegmentID, records []raw.Record, entries []raw.Entry) []byte {
	b := raw.SegmentHeader{Identity: identity, ID: uint64(id)}.Bytes()
	for _, r := range records {
		r.Identity, r.Segment = identity, uint64(id)
		b = append(b, r.Bytes()...)
	}
	b = append(b, raw.SealMarker{Identity: identity, Segment: uint64(id), Offset: uint64(len(b))}.Bytes()...)

	footerOffset := uint64(len(b))
	var es [][raw.CatalogEntrySize]byte
	for _, e := range entries {
		eb := e.Bytes()
		es = append(es, eb)
		b = append(b, eb[:]...)
	}
	return append(b, raw.Trailer(1, footerOffset, es...)...)
}

// TestRFC28A_FooterIsNotAuthoritative checks that a footer is only an
// accelerator: with the whole footer overwritten, the segment is scanned and
// the index is the one the footer gave; and with a footer that verifies but
// describes its record wrongly, the open trusts the footer and the read
// through its entry finds the header disagreeing and refuses the record as
// corruption rather than serving what the entry says. The segment is not
// required to be reported damaged: its record verifies, and only the
// catalog entry describing it is wrong (RFC 1 section 3.7).
func TestRFC28A_FooterIsNotAuthoritative(t *testing.T) {
	f := journalv2test.New(t)
	s := sealedSetup(t, f)
	footerOffset, entries := trailerOf(t, durable(t, f, s.name))
	junk := make([]byte, len(entries)*raw.CatalogEntrySize)
	if _, err := rand.NewChaCha8([32]byte{'f', 'o', 'o', 't', 'e', 'r'}).Read(junk); err != nil {
		t.Fatalf("draw the footer's junk: %v", err)
	}
	overwrite(t, f, s.name, footerOffset, junk)
	f.Reopen()
	wantNoFooter(t, f, s)

	// A planted segment whose one entry carries a sequence number the
	// record's header does not, with the length right, so the extent the
	// loss names is the record's own.
	g := journalv2test.NewUnwatched(t)
	x := writeSynced(t, g, 0, 0, 0, 64<<10)
	other := writeSynced(t, g, 1, 1, 0, 64<<10)
	g.Reopen()
	before := g.J.Stats()

	const kindWrite = 1
	const size = 64 << 10
	extent := journalv2.Extent{Off: 1 << 20, Len: size}
	id := newestID(t, g) + 1
	version := raw.Version{Epoch: x.version.Epoch, Counter: x.version.Counter + 1}
	p := pattern(fmtTag("p", 0, 0, extent.Off), size)
	record := raw.Record{
		Kind: kindWrite, FileID: x.id.Bytes(), Offset: uint64(extent.Off), Length: size,
		Seq: 1 << 40, Version: version, Tag: uint32(journalv2test.Tag(0)),
		Write: &raw.WriteBody{Payload: p},
	}
	entry := raw.Entry{
		FileID: x.id.Bytes(), Offset: uint64(extent.Off), RecordOffset: raw.SegmentHeaderSize, Length: size,
		Seq: 1<<40 + 1, Version: version, Kind: kindWrite, Tag: uint32(journalv2test.Tag(0)),
	}
	plant(t, g, segmentName(id), sealedSegmentBytes(journalIdentity(t, g), id, []raw.Record{record}, []raw.Entry{entry}))
	g.Reopen()

	r := g.J.Stats().Recovery
	if r.CatalogSegments != before.Recovery.CatalogSegments+1 {
		t.Fatalf("recovery read %d segments by catalog, want %d: the planted segment's footer verifies", r.CatalogSegments, before.Recovery.CatalogSegments+1)
	}
	if got := entryAt(t, g, x.id, extent.Off).Location; got.Segment != id {
		t.Fatalf("the extent at %d is held from %+v, want the planted segment %d, whose catalog describes it", extent.Off, got, id)
	}

	wantExtentMissing(t, g.Share(0), x.id, extent)
	ev := lastLoss(t, g, before)
	if ev.File != x.id || ev.Extent != extent || ev.Reason != journalv2.LossCorrupt {
		t.Fatalf("loss event %+v, want file %v, extent %+v, dropped as corrupt", ev, x.id, extent)
	}

	wantBytes(t, g.Share(0), x.id, 0, x.p)
	wantBytes(t, g.Share(1), other.id, 0, other.p)
}

// wantMtime fails the test unless Since yields r's extent as a write
// carrying modification time mtime.
func wantMtime(t *testing.T, h *journalv2.Handle, r rec, mtime int64) {
	t.Helper()

	for c, err := range h.Since(t.Context(), r.id, journalv2.Version{}) {
		stub(t, "Since", err)
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if c.Kind == journalv2.ChangeWrite && c.Extent == r.extent {
			if c.Mtime != mtime {
				t.Fatalf("Since yields %+v with modification time %d, want %d", r.extent, c.Mtime, mtime)
			}
			return
		}
	}
	t.Fatalf("Since yields no write for extent %+v", r.extent)
}

// wantLost fails the test unless Stats holds a loss event for r's extent,
// dropped as corrupt.
func wantLost(t *testing.T, f *journalv2test.Fixture, r rec) {
	t.Helper()

	losses := f.J.Stats().Losses
	for _, ev := range losses {
		if ev.File == r.id && ev.Extent == r.extent && ev.Reason == journalv2.LossCorrupt {
			return
		}
	}
	t.Fatalf("no loss event names extent %+v of file %v as corrupt: %+v", r.extent, r.id, losses)
}

// TestRFC28A_WriteCarriesItsMtime checks that a write's modification time
// comes back from its record after a crash, the time the client saw for A
// and the zero of a suspended time for B, and that the time is under the
// payload checksum: with one byte of it flipped, the record no longer
// serves.
func TestRFC28A_WriteCarriesItsMtime(t *testing.T) {
	f := journalv2test.New(t)
	const t1 = int64(1_767_225_600_123_456_789) // 2026-01-01T00:00:00.123456789Z
	const size = 64 << 10
	a := rec{0, journalv2test.File(0), journalv2.Extent{Off: 0, Len: size}, journalv2.Version{}, pattern(fmtTag("t", 0, 0, 0), size)}
	v, err := tryWriteMtime(t, f.Share(0), a.id, 0, a.p, t1)
	stub(t, "WriteAt", err)
	if err != nil {
		t.Fatalf("write A with a modification time: %v", err)
	}
	a.version = v
	syncFiles(t, f.Share(0), a.id)
	b := writeSynced(t, f, 1, 1, 0, size) // its time is zero
	f.Crash()

	wantMtime(t, f.Share(0), a, t1)
	wantMtime(t, f.Share(1), b, 0)

	for _, r := range []rec{a, b} {
		loc := entryAt(t, f, r.id, r.extent.Off).Location
		flipAt(t, f, segmentName(loc.Segment), loc.Offset+raw.WriteMTimeOffset)
	}
	f.Reopen()

	for _, r := range []rec{a, b} {
		wantExtentMissing(t, f.Share(r.share), r.id, r.extent)
		wantLost(t, f, r)
	}
}

// tryWriteMtime is tryWrite with the write's modification time.
func tryWriteMtime(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, p []byte, mtime int64) (journalv2.Version, error) {
	var v journalv2.Version
	err := journalv2test.RetrySpare(func() (err error) {
		v, err = h.WriteAt(t.Context(), id, off, p, mtime)
		return err
	})
	return v, err
}
