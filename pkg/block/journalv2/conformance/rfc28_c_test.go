//go:build journal_conformance

package conformance

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// The checks here fail by coming back up describing something other than
// what is on disk.

// fillSegment writes records of size bytes on file i through share s, from
// offset 0, until the stream rolls into a second segment, and returns the
// writes that landed in the first segment and that segment's id. The first
// segment is then sealed full and carries a catalog.
func fillSegment(t *testing.T, f *journalv2test.Fixture, s, i int, size int) ([]rec, journalv2.SegmentID) {
	t.Helper()

	id := journalv2test.File(i)
	var recs []rec
	var first journalv2.SegmentID
	for n := 0; n <= 2*journalv2test.FixtureSegmentCap/size; n++ {
		off := int64(n) * int64(size)
		p := pattern(fmtTag("r", s, i, off), size)
		v := write(t, f.Share(s), id, off, p)
		seg := entryAt(t, f, id, off).Location.Segment
		if n == 0 {
			first = seg
		}

		if seg != first {
			return recs, first
		}

		recs = append(recs, rec{s, id, journalv2.Extent{Off: off, Len: int64(size)}, v, p})
	}
	t.Fatalf("%d records of %d bytes never rolled a segment of %d", len(recs), size, journalv2test.FixtureSegmentCap)
	panic("unreachable")
}

// sealed is a journal with one segment sealed full and catalogued, as the
// checks of the catalog start from: file 0's records in that segment, a
// record of file 1 on the other stream, how the open that read the catalog
// rebuilt the journal, and the index it built.
type sealed struct {
	recs     []rec
	seg      journalv2.SegmentID
	name     string
	other    rec
	recovery journalv2.RecoveryStats
	index    map[journalv2.FileID][]journalv2.IndexEntry
}

// sealedSetup fills file 0's first segment with 64 KiB records until its
// stream rolls, writes file 1 on the other stream, syncs both and reopens the
// journal cleanly, so the sealed segment's catalog is what the open read.
func sealedSetup(t *testing.T, f *journalv2test.Fixture) sealed {
	t.Helper()

	recs, seg := fillSegment(t, f, 0, 0, 64<<10)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	syncFiles(t, f.Share(0), journalv2test.File(0))
	f.Reopen()

	recovery := f.J.Stats().Recovery
	if recovery.CatalogSegments == 0 {
		t.Fatalf("recovery read no segment by its catalog: %+v", recovery)
	}
	if !segmentInfo(t, f, seg).Sealed {
		t.Fatalf("segment %d is not sealed after its stream rolled past it", seg)
	}

	return sealed{recs, seg, segmentName(seg), other, recovery, indexSnapshot(t, f)}
}

// wantScanned fails the test unless the last open read n segments by scan
// that the open before, which rebuilt the journal as before says, read by
// catalog: n fewer by catalog, and at least n more by scan.
func wantScanned(t *testing.T, f *journalv2test.Fixture, before journalv2.RecoveryStats, n int) {
	t.Helper()

	r := f.J.Stats().Recovery
	if r.CatalogSegments != before.CatalogSegments-n {
		t.Fatalf("recovery read %d segments by catalog, want %d: %d before, %d without a footer now", r.CatalogSegments, before.CatalogSegments-n, before.CatalogSegments, n)
	}
	if r.ScannedSegments < before.ScannedSegments+n {
		t.Fatalf("recovery read %d segments by scan, want at least %d: %d before, %d without a footer now", r.ScannedSegments, before.ScannedSegments+n, before.ScannedSegments, n)
	}
}

// TestRFC28C_NeighboursSurvive checks that one corrupt record in a segment
// read by its catalog costs that record alone: the damage is found by the
// read that would serve it, not at open, and every other record of the
// segment still reads.
func TestRFC28C_NeighboursSurvive(t *testing.T) {
	f := journalv2test.New(t)
	recs, seg := fillSegment(t, f, 0, 0, 32<<10)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	syncFiles(t, f.Share(0), recs[0].id)

	mid := recs[len(recs)/2]
	corruptRecord(t, f, entryAt(t, f, mid.id, mid.extent.Off).Location, mid.p)
	f.Reopen()

	s := f.J.Stats()
	if s.Recovery.CatalogSegments == 0 {
		t.Fatalf("recovery read no segment by its catalog: %+v", s.Recovery)
	}

	if len(s.Losses) != 0 {
		t.Fatalf("the damage was noticed at open, with the catalog in use: %+v", s.Losses)
	}

	wantMissing(t, f.Share(0), mid.id, mid.extent)
	ev := lastLoss(t, f, s)
	if ev.File != mid.id || ev.Extent != mid.extent || ev.Reason != journalv2.LossCorrupt {
		t.Fatalf("loss event %+v, want file %v, extent %+v, dropped as corrupt", ev, mid.id, mid.extent)
	}

	for _, r := range recs {
		if r.extent != mid.extent {
			wantBytes(t, f.Share(0), r.id, r.extent.Off, r.p)
		}
	}

	wantBytes(t, f.Share(1), other.id, 0, other.p)

	if got := f.J.Stats().DamagedSegments; got != 1 {
		t.Fatalf("DamagedSegments = %d with one segment holding a record that does not verify, want 1", got)
	}

	if !segmentInfo(t, f, seg).Damaged {
		t.Fatalf("segment %d holds the corrupt record and is not reported damaged", seg)
	}
}

// TestRFC28C_SyncedDamageIsNotATornTail checks that damage to a synced record
// near the end of the newest segment is reported as corruption, and that the
// synced records after it are kept, never cut off as a torn tail: the later
// record's header proves the earlier one was synced.
func TestRFC28C_SyncedDamageIsNotATornTail(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	a := writeSynced(t, f, 0, 0, 0, 64<<10)
	b := writeSynced(t, f, 0, 0, 64<<10, 64<<10)
	write(t, h, id, 128<<10, pattern("c", 64<<10)) // C, never synced
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	bLoc := entryAt(t, f, id, b.extent.Off).Location

	before := f.J.Stats()
	corrupt(t, f, id, a.p)
	f.Crash()

	ev := lastLoss(t, f, before)
	if ev.File != id || ev.Extent != a.extent || ev.Reason != journalv2.LossCorrupt {
		t.Fatalf("loss event %+v, want A's extent %+v dropped as corrupt", ev, a.extent)
	}

	if got := entryAt(t, f, id, b.extent.Off).Location; got != bLoc {
		t.Fatalf("B moved from %+v to %+v across the crash; it was synced where it was", bLoc, got)
	}

	wantBytes(t, h, id, b.extent.Off, b.p)

	// The segment's header is 96 bytes, then a write's 8-byte modification
	// time, then its payload: B's end is where no truncation may reach.
	durable, err := f.Seam.Durable(segmentName(bLoc.Segment))
	if err != nil {
		t.Fatalf("read B's segment: %v", err)
	}

	if end := bLoc.Offset + 96 + 8 + int64(len(b.p)); int64(len(durable)) < end {
		t.Fatalf("B's segment is %d bytes after recovery, truncated below B's end at %d", len(durable), end)
	}

	wantBytes(t, f.Share(1), other.id, 0, other.p)
}

// recordAt returns the offset of the first record header of kind at or past
// from in the segment bytes b, searching at each 8-byte slot, or -1. A
// header starts with the magic DJR1 and carries its kind in the fifth byte.
func recordAt(b []byte, from int64, kind byte) int64 {
	for off := from; off+5 <= int64(len(b)); off += 8 {
		if string(b[off:off+4]) == "DJR1" && b[off+4] == kind {
			return off
		}
	}
	return -1
}

// wantDamagedOnly fails the test unless the journal reports no loss event
// and marks segment seg, the one whose sync failed, as damaged.
func wantDamagedOnly(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) {
	t.Helper()

	if losses := f.J.Stats().Losses; len(losses) != 0 {
		t.Fatalf("a loss was recorded while a copy of the record verifies: %+v", losses)
	}
	if !segmentInfo(t, f, seg).Damaged {
		t.Fatalf("the failed segment %d holds a copy that does not verify and is not reported damaged", seg)
	}
}

// TestRFC28C_BadCopyDoesNotMaskAGoodOne checks that a record whose original
// rots on a failed segment after it was re-appended is served from the good
// copy, and that no loss record is written for the rotten one: a loss names
// a sequence number, and both copies carry it, so a loss for the dead copy
// would take the live one at the next open.
//
// The first leg is the row's: a content write, re-appended. The second forces
// the re-append with a truncate, which the journal always re-appends and
// never fails, so the rule is reached on a journal that fails every content
// window too.
//
// decision: the row crashes before the failed segment is unlinked; this check
// closes cleanly and reopens instead. A failed sync leaves the window's
// writes as changes no sync made durable, so the seam's plain crash drops
// them with the rest, and its torn and reordered crashes keep them only by
// chance: the rotten original the row needs on disk would be gone. A clean
// close leaves the file as a dead process's page cache would, which is the
// state the failed-sync rule describes, and recovery then meets both copies
// as the row intends.
func TestRFC28C_BadCopyDoesNotMaskAGoodOne(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	a := rec{0, id, journalv2.Extent{Off: 0, Len: 64 << 10}, journalv2.Version{}, pattern("a", 64<<10)}
	a.version = write(t, h, id, 0, a.p)
	loc := entryAt(t, f, id, 0).Location
	name := segmentName(loc.Segment)

	f.FailNext(journalv2test.OpSync, name, 1)
	err := h.Sync(t.Context(), id)
	stub(t, "Sync", err)
	// decision: a journal may resolve a failed window by failing it, and this
	// row then has nothing to check: the bug it exists for needs a good copy
	// for the rotten original to mask. The check fails rather than passes on
	// that branch, so a journal that always fails content windows is seen
	// here, not hidden; its window handling is RFC 1's failed-window checks'
	// to judge.
	var se *journalv2.SyncError
	if errors.As(err, &se) {
		t.Fatalf("the journal failed the window (%v); this check needs the re-append", err)
	}

	if err != nil {
		t.Fatalf("sync after a failed sync: %v", err)
	}

	if got := entryAt(t, f, id, 0).Location; got.Segment == loc.Segment {
		t.Fatalf("A is still served from the failed segment at %+v, not re-appended", got)
	}

	if got := f.J.Stats().Counters.SyncReappendedBytes; got < uint64(len(a.p)) {
		t.Fatalf("SyncReappendedBytes = %d after re-appending %d bytes", got, len(a.p))
	}

	corruptRecord(t, f, loc, a.p)
	f.Reopen()
	wantBytes(t, f.Share(0), id, 0, a.p)
	wantDamagedOnly(t, f, loc.Segment)

	// The forced leg, on a journal of its own.
	g := journalv2test.NewUnwatched(t)
	h, id = g.Share(1), journalv2test.File(1)
	x := writeSynced(t, g, 1, 1, 0, 64<<10)
	loc = entryAt(t, g, id, 0).Location
	name = segmentName(loc.Segment)

	g.FailNext(journalv2test.OpSync, name, 1)
	_, err = h.Truncate(t.Context(), id, 4096)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("truncate whose sync failed: %v; a record without bytes is re-appended, never failed", err)
	}

	// The truncate's original is the first record of its kind past X's in
	// the failed segment's bytes as a read sees them; one byte of its
	// header, inside the header checksum's range, is flipped.
	const kindTruncate = 4
	live := liveBytes(t, g, name)
	off := recordAt(live, loc.Offset+8, kindTruncate)
	if off < 0 {
		t.Fatalf("no truncate record past offset %d in %s", loc.Offset, name)
	}

	flipAt(t, g, name, off+16)
	g.Reopen()
	h = g.Share(1)

	p, missing, _ := readAt(t, h, id, 0, len(x.p))
	if len(missing) != 1 || missing[0] != (journalv2.Extent{Off: 4096, Len: int64(len(x.p)) - 4096}) {
		t.Fatalf("read of the truncated file reports missing %+v, want exactly the bytes past the truncate", missing)
	}

	if string(p[:4096]) != string(x.p[:4096]) {
		t.Fatalf("the bytes below the truncate changed")
	}

	found := false
	for c, err := range h.Since(t.Context(), id, journalv2.Version{}) {
		stub(t, "Since", err)
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if c.Kind == journalv2.ChangeTruncate && c.Extent.Off == 4096 {
			found = true
		}
	}

	if !found {
		t.Fatalf("Since yields no truncate at 4096; the re-appended copy was not held")
	}

	wantDamagedOnly(t, g, loc.Segment)
}

// writeRec writes one record of size bytes at off of file i through share s
// and does not sync it.
func writeRec(t *testing.T, f *journalv2test.Fixture, s, i int, off int64, size int) rec {
	t.Helper()

	id := journalv2test.File(i)
	p := pattern(fmtTag("u", s, i, off), size)
	v := write(t, f.Share(s), id, off, p)

	return rec{s, id, journalv2.Extent{Off: off, Len: int64(size)}, v, p}
}

// crashHeld simulates a power loss while the calls stopped at h are in
// flight, and opens the journal again.
//
// decision: the seam's crash runs first, so nothing a held call does once
// released reaches the directory; the hold is released second, so the calls
// return; the fixture's crash runs last, because closing the dead journal
// waits for its calls in flight. The fixture's crash repeats the seam's,
// which then finds nothing pending to drop.
func crashHeld(t *testing.T, f *journalv2test.Fixture, h *journalv2test.Hold) {
	t.Helper()

	if err := f.Seam.Crash(); err != nil {
		t.Fatalf("crash: %v", err)
	}
	h.Release()
	f.Crash()
}

// TestRFC28C_Rebuild checks that after a crash in the middle of a write the
// rebuilt index serves every extent a returned Sync covered, holds nothing
// past the last record that verifies, and seals the segment there.
func TestRFC28C_Rebuild(t *testing.T) {
	f := journalv2test.New(t)
	const size = 32 << 10
	var synced, unsynced []rec
	for n := range 6 {
		for s := range 2 {
			r := writeRec(t, f, s, s, int64(n)*size, size)
			if n < 3 {
				synced = append(synced, r)
			} else {
				unsynced = append(unsynced, r)
			}
		}
		if n == 2 {
			syncFiles(t, f.Share(0), journalv2test.File(0))
			syncFiles(t, f.Share(1), journalv2test.File(1))
		}
	}

	// The last synced record of each segment is where the rebuilt index
	// must end.
	tail := map[journalv2.SegmentID]int64{}
	for _, r := range synced {
		loc := entryAt(t, f, r.id, r.extent.Off).Location
		tail[loc.Segment] = max(tail[loc.Segment], loc.Offset)
	}

	held := f.Seam.Hold(t, journalv2test.OpWrite, "*.seg")
	wrote := make(chan error, 1)
	go func() {
		_, err := f.Share(0).WriteAt(t.Context(), journalv2test.File(0), 6*size, pattern("mid", size), 0)
		wrote <- err
	}()
	select {
	case <-held.Reached():
	case err := <-wrote:
		stub(t, "WriteAt", err)
		t.Fatalf("the write returned (%v) without reaching storage", err)
	}
	crashHeld(t, f, held)
	<-wrote // a write the power cut took; what it returned means nothing

	for _, r := range synced {
		wantBytes(t, f.Share(r.share), r.id, r.extent.Off, r.p)
	}
	for _, r := range unsynced {
		wantMissing(t, f.Share(r.share), r.id, r.extent)
	}
	if losses := f.J.Stats().Losses; len(losses) != 0 {
		t.Fatalf("writes no Sync covered were reported as lost: %+v", losses)
	}

	for seg, end := range tail {
		if !segmentInfo(t, f, seg).Sealed {
			t.Fatalf("segment %d is not sealed after recovery cut it at its last verifying record", seg)
		}
		for id, entries := range f.J.Inspect().All() {
			for _, e := range entries {
				if e.Kind == journalv2.EntryHeld && e.Location.Segment == seg && e.Location.Offset > end {
					t.Fatalf("file %v holds %+v past the last synced record of segment %d at %d", id, e, seg, end)
				}
			}
		}
	}
}

// TestRFC28C_TornTailCutOff checks that a torn record at the end of the
// newest segment is cut off and the segment sealed, so the next write lands
// in a new segment and a second crash cannot lose it behind the torn bytes.
func TestRFC28C_TornTailCutOff(t *testing.T) {
	f := journalv2test.New(t)
	id := journalv2test.File(0)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	x := writeRec(t, f, 0, 0, 0, 64<<10)
	xSeg := entryAt(t, f, id, 0).Location.Segment

	// Only X's segment is torn: its last unsynced write is cut in half,
	// whatever the journal wrote elsewhere since.
	f.Crash(journalv2test.TornIn(segmentName(xSeg), 32<<10))

	if got := f.J.Stats().Counters.TornTails; got != 1 {
		t.Fatalf("TornTails = %d after a torn record, want 1", got)
	}
	if !segmentInfo(t, f, xSeg).Sealed {
		t.Fatalf("the torn segment %d was not sealed", xSeg)
	}
	wantMissing(t, f.Share(0), id, x.extent)

	a := writeSynced(t, f, 0, 0, 64<<10, 64<<10)
	if seg := entryAt(t, f, id, a.extent.Off).Location.Segment; seg == xSeg {
		t.Fatalf("A was appended into the torn segment %d", seg)
	}

	f.Crash()
	wantBytes(t, f.Share(0), id, a.extent.Off, a.p)
	wantBytes(t, f.Share(1), other.id, 0, other.p)
}

// truncateSegment cuts the segment file name to size bytes through the seam
// and syncs it, as an edit from outside would.
func truncateSegment(t *testing.T, f *journalv2test.Fixture, name string, size int64) {
	t.Helper()

	file, err := f.Seam.Open(name)
	if err != nil {
		t.Fatalf("truncate %s: %v", name, err)
	}
	err = file.Truncate(size)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		t.Fatalf("truncate %s: %v", name, err)
	}
}

// wantMissingFrom fails the test unless a read of extent e of file id
// reports its start missing: the record was dropped, whether whole or, for
// a write split at a segment cap, its first piece.
func wantMissingFrom(t *testing.T, h *journalv2.Handle, id journalv2.FileID, e journalv2.Extent) {
	t.Helper()

	_, missing, _ := readAt(t, h, id, e.Off, int(e.Len))
	if len(missing) == 0 || missing[0].Off != e.Off {
		t.Fatalf("read of %+v reports missing %+v, want its start missing", e, missing)
	}
}

// TestRFC28C_TruncatedSealedSegment checks that recovery tells a truncation
// from a crash by the predecessor link: a segment a later one names as its
// predecessor was sealed before the later one existed, so a short one is
// corruption and is reported; the newest segment of a stream cut the same way
// is a torn tail.
func TestRFC28C_TruncatedSealedSegment(t *testing.T) {
	f := journalv2test.New(t)
	const size = 64 << 10
	recs, s1 := fillSegment(t, f, 0, 0, size)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	id := journalv2test.File(0)
	syncFiles(t, f.Share(0), id)

	s2 := entryAt(t, f, id, int64(len(recs))*size).Location.Segment
	hdr, err := f.Seam.Durable(segmentName(s2))
	if err != nil {
		t.Fatalf("read the successor segment: %v", err)
	}
	h2, err := raw.ParseSegmentHeader(hdr[:raw.SegmentHeaderSize])
	if err != nil {
		t.Fatalf("parse the successor's header: %v", err)
	}
	if h2.Predecessor != uint64(s1) {
		t.Fatalf("segment %d names predecessor %d, want %d", s2, h2.Predecessor, s1)
	}

	cut := len(recs) / 2
	cutLoc := entryAt(t, f, id, recs[cut].extent.Off).Location
	truncateSegment(t, f, segmentName(s1), cutLoc.Offset+96+100)
	f.Reopen()

	s := f.J.Stats()
	if s.Counters.TornTails != 0 {
		t.Fatalf("TornTails = %d; a truncated segment with a successor is corruption, not a torn tail", s.Counters.TornTails)
	}
	if !segmentInfo(t, f, s1).Damaged {
		t.Fatalf("the truncated segment %d is not reported as corruption", s1)
	}
	for i, r := range recs {
		if i < cut {
			wantBytes(t, f.Share(0), id, r.extent.Off, r.p)
		} else {
			wantMissingFrom(t, f.Share(0), id, r.extent)
		}
	}
	lost := map[int64]bool{}
	for _, ev := range s.Losses {
		if ev.File == id && ev.Reason == journalv2.LossCorrupt {
			lost[ev.Extent.Off] = true
		}
	}
	for i, r := range recs {
		if (i >= cut) != lost[r.extent.Off] {
			t.Fatalf("record at %d: cut %t, reported as corrupt %t; losses %+v", r.extent.Off, i >= cut, lost[r.extent.Off], s.Losses)
		}
	}
	wantBytes(t, f.Share(1), other.id, 0, other.p)

	// The newest segment of a stream, cut the same way.
	g := journalv2test.NewUnwatched(t)
	var rs []rec
	for n := range 4 {
		rs = append(rs, writeSynced(t, g, 0, 0, int64(n)*size, size))
	}
	seg := entryAt(t, g, id, 0).Location.Segment
	cutLoc = entryAt(t, g, id, rs[2].extent.Off).Location
	truncateSegment(t, g, segmentName(seg), cutLoc.Offset+96+100)
	g.Reopen()

	s = g.J.Stats()
	if s.Counters.TornTails != 1 {
		t.Fatalf("TornTails = %d after cutting the newest segment, want 1", s.Counters.TornTails)
	}
	if len(s.Losses) != 0 {
		t.Fatalf("a cut newest segment reported losses: %+v", s.Losses)
	}
	if !segmentInfo(t, g, seg).Sealed {
		t.Fatalf("the newest segment %d was not sealed after its torn tail was cut off", seg)
	}
	for i, r := range rs {
		if i < 2 {
			wantBytes(t, g.Share(0), id, r.extent.Off, r.p)
		} else {
			wantMissingFrom(t, g.Share(0), id, r.extent)
		}
	}
}

// TestRFC28C_BitsSurviveRestart checks that a synced offloaded bit outlives a
// crash, that a dirty extent comes back without one, and that a mark no sync
// covered is lost with the crash rather than kept: a bit never set where the
// remote tier may not hold the content.
func TestRFC28C_BitsSurviveRestart(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	a := writeSynced(t, f, 0, 0, 0, 64<<10)
	mark(t, h, a.id, a.extent, a.version)
	syncFiles(t, h, a.id)
	b := writeSynced(t, f, 0, 0, 64<<10, 64<<10)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)

	f.Crash()
	h = f.Share(0)
	wantRelease(t, h, a.id, a.extent, true)
	wantRelease(t, h, b.id, b.extent, false)
	wantBytes(t, f.Share(1), other.id, 0, other.p)

	// The mark's record never reaches the device: every sync of its segment
	// reports success and makes nothing durable, so the crash takes it.
	g := journalv2test.NewUnwatched(t)
	h = g.Share(0)
	d := writeSynced(t, g, 0, 0, 0, 64<<10)
	g.Drop(journalv2test.OpSync, segmentName(entryAt(t, g, d.id, 0).Location.Segment))
	mark(t, h, d.id, d.extent, d.version)

	g.Crash()
	h = g.Share(0)
	wantRelease(t, h, d.id, d.extent, false)
	if entryAt(t, g, d.id, 0).Offloaded {
		t.Fatalf("D's offloaded bit is set after a crash took the mark's record")
	}
}

// TestRFC28C_ReseedByVersion checks that a mark names the versions it
// vouches for: an extent overwritten since the offload is not marked by a
// mark at the older versions, so the newer write is never evictable on the
// strength of an older upload.
func TestRFC28C_ReseedByVersion(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	a := writeSynced(t, f, 0, 0, 0, 64<<10)
	mark(t, h, a.id, a.extent, a.version)
	syncFiles(t, h, a.id)
	b := writeSynced(t, f, 0, 0, 0, 64<<10)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)

	f.Crash()
	h = f.Share(0)
	err := h.MarkOffloaded(t.Context(), a.id, []journalv2.Extent{a.extent}, a.version, a.version)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark at A's versions: %v", err)
	}

	wantRelease(t, h, a.id, a.extent, false)
	if e := entryAt(t, f, a.id, 0); e.Version != b.version || e.Offloaded {
		t.Fatalf("the extent is held as %+v, want B's version %v and no offloaded bit", e, b.version)
	}
	wantBytes(t, h, b.id, 0, b.p)
	wantBytes(t, f.Share(1), other.id, 0, other.p)
}
