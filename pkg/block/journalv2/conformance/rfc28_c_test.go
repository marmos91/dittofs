//go:build journal_conformance

package conformance

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
	"github.com/marmos91/dittofs/pkg/sim"
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
	return nil, 0
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
// catalog: n fewer by catalog, and at least n more by scan. A failure is
// prefixed with where, when given, so a sweep names its pass.
func wantScanned(t *testing.T, where string, f *journalv2test.Fixture, before journalv2.RecoveryStats, n int) {
	t.Helper()

	if where != "" {
		where += ": "
	}

	r := f.J.Stats().Recovery
	if r.CatalogSegments != before.CatalogSegments-n {
		t.Fatalf("%srecovery read %d segments by catalog, want %d: %d before, %d without a footer now", where, r.CatalogSegments, before.CatalogSegments-n, before.CatalogSegments, n)
	}

	if r.ScannedSegments < before.ScannedSegments+n {
		t.Fatalf("%srecovery read %d segments by scan, want at least %d: %d before, %d without a footer now", where, r.ScannedSegments, before.ScannedSegments+n, before.ScannedSegments, n)
	}
}

// TestRFC28C_NeighboursSurvive checks that one corrupt record in a segment
// read by its catalog costs that record alone: the damage is found by the
// read that would serve it, not at open, and every other record of the
// segment still reads.
//
// decision: this is also the check of the edits table's row for the bytes
// of a record changed from outside: noticed on the read that serves it, not
// at open when a catalog is used, and the extent dropped and reported.
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
	other := writeSynced(t, f, 1, 1, 0, 64<<10)
	bLoc := entryAt(t, f, id, b.extent.Off).Location

	// The damage is made durable by a sync of the whole file, so C, which
	// must stay unsynced, is written after it.
	before := f.J.Stats()
	corrupt(t, f, id, a.p)
	write(t, h, id, 128<<10, pattern("c", 64<<10)) // C, never synced
	f.Crash()

	ev := lastLoss(t, f, before)
	if ev.File != id || ev.Extent != a.extent || ev.Reason != journalv2.LossCorrupt {
		t.Fatalf("loss event %+v, want A's extent %+v dropped as corrupt", ev, a.extent)
	}

	if got := entryAt(t, f, id, b.extent.Off).Location; got != bLoc {
		t.Fatalf("B moved from %+v to %+v across the crash; it was synced where it was", bLoc, got)
	}

	wantBytes(t, h, id, b.extent.Off, b.p)

	// A record's header, then a write's 8-byte modification time, then its
	// payload: B's end is where no truncation may reach.
	durable, err := f.Seam.Durable(segmentName(bLoc.Segment))
	if err != nil {
		t.Fatalf("read B's segment: %v", err)
	}

	if end := bLoc.Offset + raw.RecordHeaderSize + 8 + int64(len(b.p)); int64(len(durable)) < end {
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
	// A record of another extent, synced into the segment before A, keeps
	// the segment live once A is re-appended out of it, so it stays until a
	// repack retires it.
	kept := writeSynced(t, f, 0, 0, 1<<20, 64<<10)
	a := rec{0, id, journalv2.Extent{Off: 0, Len: 64 << 10}, journalv2.Version{}, pattern("a", 64<<10)}
	a.version = write(t, h, id, 0, a.p)
	loc := entryAt(t, f, id, 0).Location
	name := segmentName(loc.Segment)
	if keptSeg := entryAt(t, f, kept.id, kept.extent.Off).Location.Segment; keptSeg != loc.Segment {
		t.Fatalf("precondition: the synced record landed in segment %d and A in %d, want both in one", keptSeg, loc.Segment)
	}

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
		t.Fatalf("precondition: the journal failed the window (%v); this check needs the re-append", err)
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
	wantBytes(t, f.Share(0), kept.id, kept.extent.Off, kept.p)
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
	var synced []rec
	for n := range 6 {
		for s := range 2 {
			r := writeRec(t, f, s, s, int64(n)*size, size)
			if n < 3 {
				synced = append(synced, r)
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
//
// decision: this is also the check of the edits table's row for a sealed
// segment truncated from outside: noticed at open when a later segment names
// it as predecessor, reported as corruption, and the newest segment of a
// stream treated as a crash.
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
		t.Fatalf("precondition: segment %d names predecessor %d, want %d", s2, h2.Predecessor, s1)
	}

	// The cut keeps the header of the record at the cut and 100 bytes of
	// its body, and takes every record after it whole.
	cut := len(recs) / 2
	cutLoc := entryAt(t, f, id, recs[cut].extent.Off).Location
	truncateSegment(t, f, segmentName(s1), cutLoc.Offset+raw.RecordHeaderSize+100)
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
	// Only the record whose header survived the cut can be named by a loss:
	// the records past it have no header left, so no journal can name them.
	lost := map[int64]bool{}
	for _, ev := range s.Losses {
		if ev.File == id && ev.Reason == journalv2.LossCorrupt {
			lost[ev.Extent.Off] = true
		}
	}

	for i, r := range recs {
		switch {
		case i < cut && lost[r.extent.Off]:
			t.Fatalf("the record at %d, before the cut, is reported as corrupt: %+v", r.extent.Off, s.Losses)
		case i == cut && !lost[r.extent.Off]:
			t.Fatalf("the record at %d, whose header survived the cut, is not reported as corrupt: %+v", r.extent.Off, s.Losses)
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
	truncateSegment(t, g, segmentName(seg), cutLoc.Offset+raw.RecordHeaderSize+100)
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

// sealedSegments returns the sealed segments the journal owns whose file ends
// in a trailer, in id order: the ones recovery reads by catalog.
func sealedSegments(t *testing.T, f *journalv2test.Fixture) []journalv2.SegmentID {
	t.Helper()

	var ids []journalv2.SegmentID
	for _, s := range f.J.Inspect().Segments() {
		if !s.Sealed {
			continue
		}
		b := durable(t, f, segmentName(s.ID))
		if len(b) >= raw.TrailerSize && string(b[len(b)-raw.TrailerSize:][:4]) == "DJT1" {
			ids = append(ids, s.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// TestRFC28C_SourcesAgree checks that the index built from catalogs and the
// one built by scanning the same segments are identical: every record kind a
// catalog must carry is written before the segment seals, the index is read
// once by catalog, every trailer is damaged, and the index read by scan must
// match it entry by entry.
func TestRFC28C_SourcesAgree(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	const size = 64 << 10

	// Every kind a catalog must carry, through file 0's share before file 0's
	// fill rolls the stream: an extent marked and released, one marked and
	// unmarked, a deallocate, a truncate and a delete, each left unsettled.
	id := journalv2test.File(2)
	var ws []rec
	for n := range 4 {
		ws = append(ws, writeRec(t, f, 0, 2, int64(n)*size, size))
	}
	mark(t, h, id, ws[0].extent, ws[0].version)
	wantRelease(t, h, id, ws[0].extent, true)
	mark(t, h, id, ws[1].extent, ws[1].version)
	unmark(t, h, id, ws[1].extent)

	_, err := h.Deallocate(t.Context(), id, journalv2.Extent{Off: ws[2].extent.Off + 16<<10, Len: 16 << 10})
	stub(t, "Deallocate", err)
	if err != nil {
		t.Fatalf("deallocate inside a write: %v", err)
	}
	_, err = h.Truncate(t.Context(), id, ws[3].extent.Off+32<<10)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("truncate inside a write: %v", err)
	}

	gone := journalv2test.File(3)
	writeRec(t, f, 0, 3, 0, size)
	_, err = h.Delete(t.Context(), gone)
	stub(t, "Delete", err)
	if err != nil {
		t.Fatalf("delete a file: %v", err)
	}
	syncFiles(t, h, id, gone)

	s := sealedSetup(t, f)

	flipped := sealedSegments(t, f)
	if !slices.Contains(flipped, s.seg) {
		t.Fatalf("segment %d, sealed full, carries no trailer; sealed segments with one: %v", s.seg, flipped)
	}
	for _, seg := range flipped {
		name := segmentName(seg)
		flipAt(t, f, name, int64(len(durable(t, f, name)))-raw.TrailerSize)
	}
	f.Reopen()

	r := f.J.Stats().Recovery
	if r.CatalogSegments != 0 {
		t.Fatalf("recovery read %d segments by catalog with every trailer damaged", r.CatalogSegments)
	}
	if want := s.recovery.ScannedSegments + len(flipped); r.ScannedSegments < want {
		t.Fatalf("recovery read %d segments by scan, want at least %d: %d before and %d trailers damaged", r.ScannedSegments, want, s.recovery.ScannedSegments, len(flipped))
	}
	wantSameIndex(t, f, s.index)
}

// wantNoFooter fails the test unless the open after the damage to s's
// footer read that segment by scan in place of its catalog, built the same
// index, reported no loss and no damage, and every record still reads.
func wantNoFooter(t *testing.T, f *journalv2test.Fixture, s sealed) {
	t.Helper()

	wantScanned(t, "", f, s.recovery, 1)
	wantSameIndex(t, f, s.index)

	if losses := f.J.Stats().Losses; len(losses) != 0 {
		t.Fatalf("a footer that does not verify was reported as a loss: %+v", losses)
	}
	if segmentInfo(t, f, s.seg).Damaged {
		t.Fatalf("segment %d is reported damaged; a footer that does not verify is only no footer", s.seg)
	}

	for _, r := range s.recs {
		wantBytes(t, f.Share(0), r.id, r.extent.Off, r.p)
	}
	wantBytes(t, f.Share(1), s.other.id, 0, s.other.p)
}

// wantBadFooter fails the test unless Verify names segment seg among those
// whose footer did not verify, which verification reports rather than
// repairs (RFC 1 section 4.4, line 1713).
func wantBadFooter(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) {
	t.Helper()

	report, err := f.J.Inspect().Verify(t.Context())
	stub(t, "Inspect.Verify", err)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !slices.Contains(report.BadFooters, seg) {
		t.Fatalf("Verify names %v as the segments whose footer did not verify, want segment %d among them", report.BadFooters, seg)
	}
}

// TestRFC28C_TrailerTorn checks that a footer cut short behind its seal
// marker, and separately a footer with one entry byte changed, are each
// treated as no footer: the segment is scanned, the index is the one the
// catalog gave, nothing is reported as damage, and Verify names the footer.
func TestRFC28C_TrailerTorn(t *testing.T) {
	f := journalv2test.New(t)
	s := sealedSetup(t, f)
	footerOffset, _ := trailerOf(t, durable(t, f, s.name))
	truncateSegment(t, f, s.name, footerOffset+10)
	f.Reopen()
	wantNoFooter(t, f, s)
	wantBadFooter(t, f, s.seg)

	g := journalv2test.NewUnwatched(t)
	s = sealedSetup(t, g)
	footerOffset, _ = trailerOf(t, durable(t, g, s.name))
	flipAt(t, g, s.name, footerOffset+30)
	g.Reopen()
	wantNoFooter(t, g, s)
	wantBadFooter(t, g, s.seg)
}

// TestRFC28C_UnknownVersion checks that a trailer carrying a catalog layout
// version the journal does not know is only no footer: the open succeeds,
// the segment is scanned, and nothing is reported as damage.
func TestRFC28C_UnknownVersion(t *testing.T) {
	f := journalv2test.New(t)
	s := sealedSetup(t, f)
	b := durable(t, f, s.name)
	footerOffset, entries := trailerOf(t, b)

	// The same trailer, with version 2 in place of 1 and its checksum over
	// that: a reader that checks the version last meets a trailer that
	// verifies.
	overwrite(t, f, s.name, int64(len(b))-raw.TrailerSize, raw.Trailer(2, uint64(footerOffset), entries...))
	f.Reopen()
	wantNoFooter(t, f, s)
}

// TestRFC28C_ScanResynchronises checks that a scan meeting a record whose
// header does not verify refuses that record alone and resumes at the next
// header that does, counting the resynchronisation once; and that a copy of
// a segment held inside a record's payload hands the scan no record, because
// a header verifies only in the segment it names.
func TestRFC28C_ScanResynchronises(t *testing.T) {
	f := journalv2test.New(t)
	s := sealedSetup(t, f)
	mid := s.recs[len(s.recs)/2]
	loc := entryAt(t, f, mid.id, mid.extent.Off).Location
	before := f.J.Stats()

	// The trailer first, so the segment has no catalog; then the length
	// field of a record in the middle, so the scan cannot step over it.
	flipAt(t, f, s.name, int64(len(durable(t, f, s.name)))-raw.TrailerSize)
	flipAt(t, f, s.name, loc.Offset+raw.RecordLengthOffset)
	f.Reopen()

	wantScanned(t, "", f, s.recovery, 1)
	ev := lastLoss(t, f, before)
	if ev.File != mid.id || ev.Extent != mid.extent || ev.Reason != journalv2.LossCorrupt {
		t.Fatalf("loss event %+v, want file %v, extent %+v, dropped as corrupt", ev, mid.id, mid.extent)
	}
	wantMissing(t, f.Share(0), mid.id, mid.extent)
	for _, r := range s.recs {
		if r.extent != mid.extent {
			wantBytes(t, f.Share(0), r.id, r.extent.Off, r.p)
		}
	}
	if got := f.J.Stats().Counters.ScanResyncs; got != 1 {
		t.Fatalf("ScanResyncs = %d after one bad header with records behind it, want 1", got)
	}
	wantBytes(t, f.Share(1), s.other.id, 0, s.other.p)

	// The first 128 KiB of the sealed segment, its header and first records
	// among them, as file 1's content on the other stream; the crash makes
	// the segment holding that record one the open scans.
	g := journalv2test.NewUnwatched(t)
	s = sealedSetup(t, g)
	copied := durable(t, g, s.name)[:128<<10]
	const off = 1 << 20
	write(t, g.Share(1), s.other.id, off, copied)
	syncFiles(t, g.Share(1), s.other.id)
	want := indexSnapshot(t, g)[journalv2test.File(0)]
	g.Crash()

	got, err := g.J.Inspect().File(journalv2test.File(0))
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect file 0: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("file 0 is indexed as\n%+v\nafter a scan over a copy of its segment, want\n%+v", got, want)
	}
	wantBytes(t, g.Share(1), s.other.id, off, copied)

	st := g.J.Stats()
	if st.Counters.ScanResyncs != 0 || len(st.Losses) != 0 || st.DamagedSegments != 0 {
		t.Fatalf("the copied headers were taken for records: ScanResyncs = %d, losses %+v, DamagedSegments = %d", st.Counters.ScanResyncs, st.Losses, st.DamagedSegments)
	}
}

// TestRFC28C_SealMarkerCopiedIntoAPayload checks that a seal marker that
// verifies for the segment and the offset it lies at, placed there as the
// first bytes of a write's payload, does not end a scan: a scan tells a slot
// only where the record lengths lead it, never inside a payload.
func TestRFC28C_SealMarkerCopiedIntoAPayload(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 64 << 10
	other := writeSynced(t, f, 1, 1, 0, size)
	x := writeRec(t, f, 0, 0, 0, size)
	loc := entryAt(t, f, id, 0).Location

	// The next slot of X's stream is X's end: its header, its modification
	// time and its payload, which ends 8-aligned. The marker names the
	// payload's first byte there, so it verifies exactly where it lies.
	slot := loc.Offset + raw.RecordHeaderSize + 8 + size
	marker := raw.SealMarker{Identity: journalIdentity(t, f), Segment: uint64(loc.Segment), Offset: uint64(slot + raw.WriteMTimeOffset + 8)}
	p := append(marker.Bytes(), pattern(fmtTag("m", 0, 0, size), size-raw.SealMarkerSize)...)
	write(t, h, id, size, p)
	if got := entryAt(t, f, id, size).Location; got != (journalv2.RecordRef{Segment: loc.Segment, Offset: slot}) {
		t.Fatalf("the record carrying the marker landed at %+v, not at the predicted slot %d of segment %d: the marker names the wrong place and this check proves nothing", got, slot, loc.Segment)
	}

	b := writeSynced(t, f, 0, 0, 2*size, size)
	f.Crash()

	wantBytes(t, f.Share(0), id, 2*size, b.p)
	wantBytes(t, f.Share(0), id, size, p)
	wantBytes(t, f.Share(0), id, 0, x.p)
	st := f.J.Stats()
	if st.Counters.TornTails != 0 || len(st.Losses) != 0 {
		t.Fatalf("the scan ended at the copied marker: TornTails = %d, losses %+v", st.Counters.TornTails, st.Losses)
	}
	wantBytes(t, f.Share(1), other.id, 0, other.p)
}

// releaseRecs marks and releases every record of recs but the last, and
// syncs their file, so the segment holding them is retired by repack alone,
// which carries the last record forward.
//
// decision: one record is left held. A segment left with nothing live is
// retired by the journal on its own once the releases are durable, at a
// moment a check cannot order against its own repack call, so the two paths
// would race for the segment and the check could read the directory between
// them.
func releaseRecs(t *testing.T, f *journalv2test.Fixture, recs []rec) {
	t.Helper()

	h := f.Share(recs[0].share)
	for _, r := range recs[:len(recs)-1] {
		mark(t, h, r.id, r.extent, r.version)
		wantRelease(t, h, r.id, r.extent, true)
	}
	syncFiles(t, h, recs[0].id)
}

// retired is how a retirement left a segment: recycled under a new name, or
// removed.
type retired struct {
	recycled bool
	name     string // the new name, when recycled
}

// retirement reads how the segment file name was retired since the directory
// held files: recycled, under the one segment file the directory gained,
// whose id is above every id it named before, or removed. One of the two
// must have happened exactly once.
func retirement(t *testing.T, f *journalv2test.Fixture, name string, files []string) retired {
	t.Helper()

	removed, renamed := f.Seam.CallsFor(journalv2test.OpRemove, name), f.Seam.CallsFor(journalv2test.OpRename, name)
	switch {
	case removed == 1 && renamed == 0:
		return retired{}
	case renamed == 1 && removed == 0:
		var added []string
		for _, n := range segmentFiles(t, f) {
			if !slices.Contains(files, n) {
				added = append(added, n)
			}
		}
		if len(added) != 1 {
			t.Fatalf("the directory gained %v after %s was recycled, want one segment file", added, name)
		}

		var newest journalv2.SegmentID
		for _, n := range files {
			newest = max(newest, idOf(t, n))
		}
		if idOf(t, added[0]) <= newest {
			t.Fatalf("the recycled segment %s has an id at or below %d, an id the directory already named", added[0], newest)
		}
		return retired{true, added[0]}
	default:
		t.Fatalf("segment %s was removed %d times and renamed %d times, want exactly one of the two", name, removed, renamed)
		return retired{}
	}
}

// retire repacks segment seg, which must be retired by it, and returns how.
func retire(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) retired {
	t.Helper()

	files := segmentFiles(t, f)
	_, err := f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
	stub(t, "Repack", err)
	if err != nil {
		t.Fatalf("repack segment %d: %v", seg, err)
	}

	return retirement(t, f, segmentName(seg), files)
}

// awaitSpare runs roll, a write whose roll takes a spare, and returns once
// the journal has made the spare that replaces it: it waits at the directory
// sync that follows the new spare's rename into place, the one directory
// sync a take leads to, and checks that the directory gained a segment file
// by then.
func awaitSpare(t *testing.T, f *journalv2test.Fixture, roll func()) {
	t.Helper()

	before := segmentFiles(t, f)
	held := f.Seam.Hold(t, journalv2test.OpSyncDir, "*")
	roll()
	<-held.Reached()

	if after := segmentFiles(t, f); len(after) != len(before)+1 {
		t.Fatalf("the directory sync was reached with %d segment files, want one more than %d: not the sync after a spare's rename", len(after), len(before))
	}
	held.Release()
}

// pool is what the spare pool and the reserves look like from outside: the
// spare names, the create and allocate calls the seam has seen, and what the
// journal accounts to its storage and its reserves.
type pool struct {
	spares                  []string
	creates, allocates      int
	used, headroom, reserve int64
}

func poolNow(t *testing.T, f *journalv2test.Fixture) pool {
	t.Helper()

	s := f.J.Stats()
	return pool{spareNames(t, f), f.Seam.Calls(journalv2test.OpCreate), f.Seam.Calls(journalv2test.OpAllocate), s.UsedBytes, s.HeadroomBytes, s.RepackReserveBytes}
}

// wantReadopted fails the test unless, after an open, every spare of before
// is a spare again, no segment was created or allocated since, the storage
// and the reserves are accounted as before, and nothing is reported as
// rejected or damaged.
func wantReadopted(t *testing.T, f *journalv2test.Fixture, before pool) {
	t.Helper()

	now := poolNow(t, f)
	for _, name := range before.spares {
		if !slices.Contains(now.spares, name) {
			t.Fatalf("spare %s is not a spare after the open; spares now: %v", name, now.spares)
		}
	}
	if now.creates != before.creates || now.allocates != before.allocates {
		t.Fatalf("the open created %d and allocated %d segments; every spare was on disk", now.creates-before.creates, now.allocates-before.allocates)
	}
	if now.used != before.used || now.headroom != before.headroom || now.reserve != before.reserve {
		t.Fatalf("after the open UsedBytes = %d, HeadroomBytes = %d, RepackReserveBytes = %d, want %d, %d, %d as before", now.used, now.headroom, now.reserve, before.used, before.headroom, before.reserve)
	}

	s := f.J.Stats()
	if s.RejectedSegments != 0 || s.DamagedSegments != 0 {
		t.Fatalf("RejectedSegments = %d, DamagedSegments = %d after re-adopting spares, want 0 and 0", s.RejectedSegments, s.DamagedSegments)
	}
}

// rollTakesSpare writes 64 KiB records on file 0 until its stream rolls and
// fails the test unless the segment the roll took is among spares.
func rollTakesSpare(t *testing.T, f *journalv2test.Fixture, spares []string) {
	t.Helper()

	const size = 64 << 10
	recs, _ := fillSegment(t, f, 0, 0, size)
	seg := entryAt(t, f, journalv2test.File(0), int64(len(recs))*size).Location.Segment
	if !slices.Contains(spares, segmentName(seg)) {
		t.Fatalf("the roll took segment %d, not one of the spares %v", seg, spares)
	}
}

// rollSynced writes 64 KiB records on file 0 through share 0, each synced,
// until one lands in a new segment, and returns that segment; the write that
// rolled is not synced.
func rollSynced(t *testing.T, f *journalv2test.Fixture) journalv2.SegmentID {
	t.Helper()

	const size = 64 << 10
	id := journalv2test.File(0)
	var first journalv2.SegmentID
	for n := 0; n <= 2*journalv2test.FixtureSegmentCap/size; n++ {
		off := int64(n) * size
		write(t, f.Share(0), id, off, pattern(fmtTag("q", 0, 0, off), size))
		seg := entryAt(t, f, id, off).Location.Segment
		if n == 0 {
			first = seg
		}
		if seg != first {
			return seg
		}
		syncFiles(t, f.Share(0), id)
	}
	t.Fatalf("%d bytes of records never rolled a segment of %d", 2*journalv2test.FixtureSegmentCap, journalv2test.FixtureSegmentCap)
	return 0
}

// TestRFC28C_SparesAreReAdopted checks that a crash costs no spare: every
// record-less segment comes back as a spare, the reserves are whole without
// a new allocation, nothing is reported as rejected or damaged, and a later
// roll takes one of them. Once with every spare untaken, and once with a
// spare a stream took but never synced, which the crash makes record-less
// again.
func TestRFC28C_SparesAreReAdopted(t *testing.T) {
	// A take's refill is awaited and the journal reopened, so the pool is
	// at its target as the open counts it and no spare-making is in flight
	// when the pool is read.
	f := journalv2test.New(t)
	awaitSpare(t, f, func() { writeSynced(t, f, 0, 2, 0, journalv2test.FixtureSegmentCap) })
	f.Reopen()
	before := poolNow(t, f)
	f.Crash()
	wantReadopted(t, f, before)
	rollTakesSpare(t, f, before.spares)

	// Spare-making is parked, so no spare is made between the take and
	// the crash, and the pool is as it was before the take. The create and
	// allocate counts are taken once the parked spare maker was released
	// into the crashed storage, so only the open's own calls are compared.
	g := journalv2test.NewUnwatched(t)
	other := writeSynced(t, g, 1, 1, 0, 64<<10)
	before = poolNow(t, g)
	held := g.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	taken := rollSynced(t, g)
	if err := g.Seam.Crash(); err != nil {
		t.Fatalf("crash: %v", err)
	}

	held.Release()
	before.creates, before.allocates = g.Seam.Calls(journalv2test.OpCreate), g.Seam.Calls(journalv2test.OpAllocate)
	g.Crash()

	if !slices.Contains(spareNames(t, g), segmentName(taken)) {
		t.Fatalf("segment %d, taken and never synced, is not a spare again after the crash", taken)
	}
	wantReadopted(t, g, before)
	rollTakesSpare(t, g, before.spares)
	wantBytes(t, g.Share(1), other.id, 0, other.p)
}

// spareSteps are the seam calls that make a spare, in order, every one on the
// spare's temporary name: create, allocate, zero-fill, sync, rename.
var spareSteps = []string{journalv2test.OpCreate, journalv2test.OpAllocate, journalv2test.OpWrite, journalv2test.OpSync, journalv2test.OpRename}

// wantWholeOrAbsent fails the test unless the directory holds no temporary
// spare, every spare is at its cap, and the storage allocated covers every
// segment file's size, so no segment was adopted short or sparse.
func wantWholeOrAbsent(t *testing.T, f *journalv2test.Fixture) {
	t.Helper()

	if tmps := tmpNames(t, f); len(tmps) != 0 {
		t.Fatalf("temporary spares %v survive the open; it unlinks every one it finds", tmps)
	}

	var total int64
	for _, name := range segmentFiles(t, f) {
		total += int64(len(durable(t, f, name)))
	}
	for _, name := range spareNames(t, f) {
		if size := len(durable(t, f, name)); size != journalv2test.FixtureSegmentCap {
			t.Fatalf("spare %s is %d bytes, want its cap %d", name, size, journalv2test.FixtureSegmentCap)
		}
	}
	if got := f.Allocated(); got < total {
		t.Fatalf("%d bytes allocated for %d bytes of segment files: a segment is not fully allocated", got, total)
	}
}

// TestRFC28C_SpareIsWholeOrAbsent checks that a crash at any step of making a
// spare leaves either no segment file for it or a whole one: nothing short,
// nothing sparse, no temporary name, and the headroom still there for a
// release when the filesystem is full. Then, with a temporary spare's
// directory entry made durable by another operation's directory sync, that
// the open unlinks it and the next spare's id is above its id.
//
// decision: the last subtest, a temporary spare whose entry another
// operation's directory sync made durable, checks that the open unlinks it
// and draws the next spare's id above it: the row's last two sentences.
//
// decision: the zero-fill step is waited for at the spare's first write or
// at its sync, whichever comes first, since a platform's allocation call may
// zero the segment with no write of the journal's own.
func TestRFC28C_SpareIsWholeOrAbsent(t *testing.T) {
	const size = 64 << 10
	for i, op := range spareSteps {
		t.Run(op, func(t *testing.T) {
			f := journalv2test.NewUnwatched(t)
			if i == 0 {
				f = journalv2test.New(t)
			}

			a := writeSynced(t, f, 1, 1, 0, size)
			mark(t, f.Share(1), a.id, a.extent, a.version)
			syncFiles(t, f.Share(1), a.id)

			// The roll takes a spare and the journal makes its replacement,
			// which parks at the step under test. File 0 is synced so
			// nothing is torn at the crash and no segment is cut at
			// recovery.
			holds := []*journalv2test.Hold{f.Seam.Hold(t, op, "*.spare.tmp")}
			if op == journalv2test.OpWrite {
				holds = append(holds, f.Seam.Hold(t, journalv2test.OpSync, "*.spare.tmp"))
			}

			recs, _ := fillSegment(t, f, 0, 0, size)
			syncFiles(t, f.Share(0), recs[0].id)
			if len(holds) == 1 {
				<-holds[0].Reached()
			} else {
				select {
				case <-holds[0].Reached():
				case <-holds[1].Reached():
				}
			}

			if n, want := len(tmpNames(t, f)), min(i, 1); n != want {
				t.Fatalf("precondition: %d temporary spares at the held %s, want %d", n, op, want)
			}

			if err := f.Seam.Crash(); err != nil {
				t.Fatalf("crash: %v", err)
			}

			for _, h := range holds {
				h.Release()
			}

			f.Crash()
			wantWholeOrAbsent(t, f)

			// A release at a full filesystem: the stream rolls into a
			// re-adopted spare first, so the record lands in one.
			fillSegment(t, f, 0, 0, size)
			schedule(t, f, failing(journalv2test.OpCreate, "*", syscall.ENOSPC), failing(journalv2test.OpAllocate, "*", syscall.ENOSPC))
			wantRelease(t, f.Share(1), a.id, a.extent, true)
			f.Seam.SetFault(nil)
		})
	}

	t.Run("durable_temporary_entry", func(t *testing.T) {
		// The spare's rename is held after its sync; the retirement of the
		// full segment, whichever way it goes, syncs the directory and so
		// makes the temporary name's entry durable.
		g := journalv2test.NewUnwatched(t)
		held := g.Seam.Hold(t, journalv2test.OpRename, "*.spare.tmp")
		recs, seg := fillSegment(t, g, 0, 0, size)
		other := writeSynced(t, g, 1, 1, 0, size)
		syncFiles(t, g.Share(0), recs[0].id)
		<-held.Reached()
		tmps := tmpNames(t, g)
		if len(tmps) != 1 {
			t.Fatalf("precondition: %d temporary spares at the held rename, want 1", len(tmps))
		}

		tmp := tmps[0]

		releaseRecs(t, g, recs)
		syncs := g.Seam.Calls(journalv2test.OpSyncDir)
		r := retire(t, g, seg)
		if g.Seam.Calls(journalv2test.OpSyncDir) == syncs {
			t.Fatalf("precondition: the retirement synced the directory no time; the temporary spare's entry is not durable")
		}

		if !r.recycled {
			t.Fatalf("precondition: the full segment was unlinked with the pool one short; its recycle is what fills the pool before the crash")
		}

		crashHeld(t, g, held)
		if got := g.Seam.CallsFor(journalv2test.OpRemove, tmp); got != 1 {
			t.Fatalf("the open removed %s %d times, want once", tmp, got)
		}

		if slices.Contains(list(t, g), tmp) {
			t.Fatalf("%s is still in the directory after the open", tmp)
		}

		// The pool is whole, so the next spare is made for the next roll,
		// and it is held at its rename while its name is read.
		next := g.Seam.Hold(t, journalv2test.OpRename, "*.spare.tmp")
		fillSegment(t, g, 0, 0, size)
		<-next.Reached()
		tmps = tmpNames(t, g)
		if len(tmps) != 1 {
			t.Fatalf("precondition: %d temporary spares at the held rename, want 1", len(tmps))
		}

		if idOf(t, tmps[0]) <= idOf(t, tmp) {
			t.Fatalf("the next spare %s has an id at or below the removed %s", tmps[0], tmp)
		}

		next.Release()
		wantBytes(t, g.Share(1), other.id, 0, other.p)
	})
}

// zero reports whether every byte of b is zero.
func zero(b []byte) bool {
	return bytes.IndexFunc(b, func(r rune) bool { return r != 0 }) < 0
}

// writeUntilRefused writes 64 KiB records on file 0 through share 0 from
// offset from, straight through WriteAt so a refusal for want of a spare is
// returned rather than retried, until one is refused, and returns the
// refusal.
func writeUntilRefused(t *testing.T, f *journalv2test.Fixture, from int64) error {
	t.Helper()

	const size = 64 << 10
	id := journalv2test.File(0)
	for n := range 2 * journalv2test.FixtureMaxBytes / size {
		off := from + int64(n)*size
		_, err := f.Share(0).WriteAt(t.Context(), id, off, pattern(fmtTag("w", 0, 0, off), size), 0)
		stub(t, "WriteAt", err)
		if err != nil {
			return err
		}
	}
	t.Fatalf("%d bytes written were never refused", 2*journalv2test.FixtureMaxBytes)
	return nil
}

// wantOnlySpare fails the test unless the pool holds the spare name alone.
//
// decision: a leg that crashes with the pool empty but for a recycled
// segment, and then needs the next roll to take that segment, depends on the
// open making no spare for a short pool, which RFC 28 section 2.2, lines
// 313 to 315, reads as: a new spare is made at Init, when the maximum is
// raised, or when no full-size retired segment is to hand. No design that
// drives the take through the seam alone avoids that dependence, so a
// journal that refills the pool at open fails here, named, and the rule is
// a question for the RFC's owner.
func wantOnlySpare(t *testing.T, f *journalv2test.Fixture, name string) {
	t.Helper()

	if got := spareNames(t, f); len(got) != 1 || got[0] != name {
		t.Fatalf("precondition: the pool holds %v after the open, want the re-adopted %s alone: the open made spares for a short pool", got, name)
	}
}

// wantTakenClean fails the test unless nothing of segment seg's old life is
// attached, the next roll of file 0's stream takes seg, the only spare the
// pool holds, and the segment then carries a fresh header and a zeroed
// trailer slot: both slots are zeroed before a stream may take it, which an
// open may leave to the take.
func wantTakenClean(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) {
	t.Helper()

	for id, entries := range f.J.Inspect().All() {
		for _, e := range entries {
			if e.Location.Segment == seg {
				t.Fatalf("file %v holds %+v from the old life of the recycled segment %d", id, e, seg)
			}
		}
	}

	const size = 64 << 10
	recs, _ := fillSegment(t, f, 0, 0, size)
	if got := entryAt(t, f, journalv2test.File(0), int64(len(recs))*size).Location.Segment; got != seg {
		t.Fatalf("the roll took segment %d, want the re-adopted %d, the only spare in the pool", got, seg)
	}

	b := liveBytes(t, f, segmentName(seg))
	hdr, err := raw.ParseSegmentHeader(b[:raw.SegmentHeaderSize])
	if err != nil || hdr.ID != uint64(seg) {
		t.Fatalf("the taken segment %d has no fresh header: %+v, %v", seg, hdr, err)
	}
	if !zero(b[len(b)-raw.TrailerSize:]) {
		t.Fatalf("the trailer slot of the taken segment %d is not zeros: the old life's trailer could be read as its catalog", seg)
	}
}

// TestRFC28C_RetiredSegmentsAreRecycled checks the rules of retiring a
// segment: one sealed full is recycled as a spare exactly when the pool is
// below its target at the retirement, by a rename and then the zeroing of
// its two slots, with no fill and an id above every id the directory named,
// and is unlinked otherwise; one sealed early, or failed, is unlinked; a
// crash on either side of the zeroing sync leaves a spare and nothing else;
// and a roll with no spare ready is refused at once naming the pool. Seven
// legs, each on its own journal, since each needs the pool at a level of its
// own.
//
// decision: the row's last sentence, a roll refused with ENOSPC once making a
// spare has failed with ENOSPC and no full-size retired segment is to hand,
// is not checked. The refusal depends on the journal having recorded that
// failure, and the moment it records it is not observable from outside: the
// failed call returns through the seam into the journal's own goroutine, no
// seam call is bound to follow the record in every design, and no Stats
// field reports it. A check could only wait in real time or race the record,
// and either can fail a correct journal. It is checked once the journal
// reports a pool it cannot refill, through Stats or a seam point of its own.
//
// decision: the pool's target is not on the public surface, so a leg steers
// the pool through the seam: a hold on the create of a temporary spare parks
// spare-making, and one roll then leaves the pool one short, since a spare
// counts only once renamed into place whole; releasing the hold and waiting
// for that rename's directory sync, then reopening, puts the pool at its
// target as the open counts it.
func TestRFC28C_RetiredSegmentsAreRecycled(t *testing.T) {
	const size = 64 << 10
	capBytes := int64(journalv2test.FixtureSegmentCap)

	// Below target: recycled by a rename, then the slots zeroed and synced,
	// the old records' bytes still between them. Both streams roll, so the
	// pool is empty and the recycled segment is the next take.
	f := journalv2test.New(t)
	held := f.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	recs, seg := fillSegment(t, f, 0, 0, size)
	others, _ := fillSegment(t, f, 1, 1, size)
	syncFiles(t, f.Share(0), recs[0].id)
	syncFiles(t, f.Share(1), others[0].id)
	name := segmentName(seg)
	if got := int64(len(durable(t, f, name))); got != capBytes {
		t.Fatalf("segment %d, sealed full, is %d bytes, want its cap %d: it was truncated", seg, got, capBytes)
	}

	releaseRecs(t, f, recs)
	allocated := f.Allocated()
	r := retire(t, f, seg)
	if !r.recycled {
		t.Fatalf("segment %d was unlinked with the pool below its target, want it recycled", seg)
	}

	b := durable(t, f, r.name)
	if int64(len(b)) != capBytes {
		t.Fatalf("the recycled segment %s is %d bytes, want its cap %d", r.name, len(b), capBytes)
	}
	if !zero(b[:raw.SegmentHeaderSize]) || !zero(b[capBytes-raw.TrailerSize:]) {
		t.Fatalf("the recycled segment %s has its header slot zeroed: %t, its trailer slot zeroed: %t, want both", r.name, zero(b[:raw.SegmentHeaderSize]), zero(b[capBytes-raw.TrailerSize:]))
	}
	if !bytes.Contains(b, recs[0].p[:64]) {
		t.Fatalf("the recycled segment %s no longer holds the old records' bytes: it was zero-filled", r.name)
	}
	if got := f.Allocated(); got != allocated {
		t.Fatalf("Allocated = %d after a recycle, want %d unchanged: a recycle allocates nothing", got, allocated)
	}
	if f.Seam.CallsFor(journalv2test.OpSync, r.name) == 0 {
		t.Fatalf("the recycled segment %s was never synced after its slots were zeroed", r.name)
	}

	// After the zeroing sync, a crash: the spare is re-adopted and the next
	// roll takes it clean. The parked spare maker is released into the
	// crashed storage before the dead journal is closed, since the close
	// joins it.
	crashHeld(t, f, held)
	s := f.J.Stats()
	if s.RejectedSegments != 0 || s.DamagedSegments != 0 {
		t.Fatalf("RejectedSegments = %d, DamagedSegments = %d after re-adopting a recycled segment, want 0 and 0", s.RejectedSegments, s.DamagedSegments)
	}

	wantOnlySpare(t, f, r.name)
	wantTakenClean(t, f, idOf(t, r.name))

	// At target: the roll's spare is replaced before the retirement, which
	// then unlinks.
	g := journalv2test.NewUnwatched(t)
	awaitSpare(t, g, func() { recs, seg = fillSegment(t, g, 0, 0, size) })
	syncFiles(t, g.Share(0), recs[0].id)
	g.Reopen()
	releaseRecs(t, g, recs)
	used := g.J.Stats().UsedBytes
	if r := retire(t, g, seg); r.recycled {
		t.Fatalf("segment %d was recycled as %s with the pool at its target, want it unlinked", seg, r.name)
	}
	if got := g.J.Stats().UsedBytes; got != used-capBytes {
		t.Fatalf("UsedBytes = %d after unlinking a segment of %d from %d", got, capBytes, used)
	}

	// Crossing: sealed with the pool one short, retired once it is whole
	// again, so unlinked: the pool is read at the retirement, never at the
	// seal.
	k := journalv2test.NewUnwatched(t)
	held = k.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	recs, seg = fillSegment(t, k, 0, 0, size)
	syncFiles(t, k.Share(0), recs[0].id)
	awaitSpare(t, k, held.Release)
	k.Reopen()
	releaseRecs(t, k, recs)
	if r := retire(t, k, seg); r.recycled {
		t.Fatalf("segment %d was recycled as %s: the pool was short at its seal and whole at its retirement", seg, r.name)
	}

	// Sealed early: truncated, and unlinked even with the pool short.
	m := journalv2test.NewUnwatched(t)
	a := writeSynced(t, m, 0, 0, 0, 128<<10)
	seg = sealIdle(t, m, a)
	if !segmentInfo(t, m, seg).Sealed {
		t.Fatalf("segment %d holding A is not sealed after %v idle", seg, journalv2test.IdleSealAge)
	}
	if got := int64(len(durable(t, m, segmentName(seg)))); got >= capBytes {
		t.Fatalf("segment %d, sealed early, is %d bytes, want it truncated below its cap %d", seg, got, capBytes)
	}
	held = m.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	fillSegment(t, m, 1, 1, size)
	if r := retire(t, m, seg); r.recycled {
		t.Fatalf("segment %d, sealed early, was recycled as %s, want it unlinked", seg, r.name)
	}
	wantBytes(t, m.Share(0), a.id, 0, a.p)
	held.Release()

	// A crash between the rename and the zeroing sync: the renamed file
	// still carries the old header, records and trailer under a name none
	// of them verifies for, and is re-adopted as a spare.
	e := journalv2test.NewUnwatched(t)
	held = e.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	recs, seg = fillSegment(t, e, 0, 0, size)
	others, _ = fillSegment(t, e, 1, 1, size)
	syncFiles(t, e.Share(0), recs[0].id)
	syncFiles(t, e.Share(1), others[0].id)
	name = segmentName(seg)
	releaseRecs(t, e, recs)
	files := segmentFiles(t, e)
	sync := e.Seam.Hold(t, journalv2test.OpSync, "*.seg")
	done := make(chan error, 1)
	go func() {
		_, err := e.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
		done <- err
	}()
	select {
	case <-sync.Reached():
	case err := <-done:
		stub(t, "Repack", err)
		t.Fatalf("repack returned (%v) without syncing a segment", err)
	}
	if got := e.Seam.CallsFor(journalv2test.OpRename, name); got != 1 {
		t.Fatalf("precondition: the held sync is not the recycle's: %s was renamed %d times by then", name, got)
	}

	r = retirement(t, e, name, files)
	if err := e.Seam.Crash(); err != nil {
		t.Fatalf("crash: %v", err)
	}

	sync.Release()
	held.Release()
	<-done // a call the crash took; what it returned means nothing
	e.Crash()

	s = e.J.Stats()
	if s.RejectedSegments != 0 || s.DamagedSegments != 0 {
		t.Fatalf("RejectedSegments = %d, DamagedSegments = %d after a crash inside a recycle, want 0 and 0", s.RejectedSegments, s.DamagedSegments)
	}

	wantOnlySpare(t, e, r.name)
	wantTakenClean(t, e, idOf(t, r.name))

	// A segment whose sync failed is unlinked, never recycled, even with
	// the pool short. A is written and not synced, so the failing sync
	// covers it, and the journal either re-appends it or fails the window.
	n := journalv2test.NewUnwatched(t)
	held = n.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	fillSegment(t, n, 1, 1, size)
	a = writeRec(t, n, 0, 0, 0, size)
	seg = entryAt(t, n, a.id, 0).Location.Segment
	n.FailNext(journalv2test.OpSync, segmentName(seg), 1)
	err := n.Share(0).Sync(t.Context(), a.id)
	stub(t, "Sync", err)
	var se *journalv2.SyncError
	if err != nil && !errors.As(err, &se) {
		t.Fatalf("sync after a failed sync: %v", err)
	}

	if !segmentInfo(t, n, seg).Failed {
		t.Fatalf("precondition: segment %d is not reported failed after its sync failed", seg)
	}

	if r := retire(t, n, seg); r.recycled {
		t.Fatalf("segment %d, whose sync failed, was recycled as %s, want it unlinked", seg, r.name)
	}

	if errors.As(err, &se) {
		wantMissing(t, n.Share(0), a.id, a.extent)
	} else {
		wantBytes(t, n.Share(0), a.id, 0, a.p)
	}

	held.Release()

	// No spare ready: the roll is refused at once, naming the pool, while
	// spare-making is still parked, and a header-only record still appends.
	o := journalv2test.NewUnwatched(t)
	held = o.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	err = writeUntilRefused(t, o, 0)
	wantNoSpace(t, err, journalv2.LimitSpare, "a roll with no spare ready")
	if held.Waiting() == 0 {
		t.Fatalf("the roll was refused with no spare-making parked at the seam")
	}
	if got := o.J.Stats().Counters.SpareRefusals; got != 1 {
		t.Fatalf("SpareRefusals = %d after one roll refused for want of a spare, want 1", got)
	}
	_, err = o.Share(0).Truncate(t.Context(), journalv2test.File(0), 4096)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("a header-only record with no spare ready: %v; it draws on the headroom", err)
	}
	held.Release()
}

// recycled is a segment a stream took after its recycle, with the old
// life's trailer still on the device because the zeroing writes were dropped
// at the seam.
type recycled struct {
	id     journalv2.SegmentID
	name   string
	marker int64 // the old life's seal marker offset
	first  rec   // the first record of the new life, synced
	other  rec   // a record of file 1 on the other stream
}

// recycledSegment fills both streams' first segments with spare-making
// parked, so the pool is empty, retires the first of them so it is recycled,
// with the zeroing writes after its rename dropped, rolls file 0's stream
// into it with a record of file 2, and then lets spare-making go on.
func recycledSegment(t *testing.T, f *journalv2test.Fixture) recycled {
	t.Helper()

	const size = 64 << 10
	held := f.Seam.Hold(t, journalv2test.OpCreate, "*.spare.tmp")
	recs, seg := fillSegment(t, f, 0, 0, size)
	others, _ := fillSegment(t, f, 1, 1, size)
	syncFiles(t, f.Share(0), recs[0].id)
	syncFiles(t, f.Share(1), others[0].id)
	name := segmentName(seg)
	footerOffset, _ := trailerOf(t, durable(t, f, name))
	releaseRecs(t, f, recs)

	// The writes after the rename are the zeroing of the two slots, and
	// are dropped, so the recycle's sync makes nothing of them durable.
	files := segmentFiles(t, f)
	rename := f.Seam.Hold(t, journalv2test.OpRename, name)
	done := make(chan error, 1)
	go func() {
		_, err := f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
		done <- err
	}()
	select {
	case <-rename.Reached():
	case err := <-done:
		stub(t, "Repack", err)
		t.Fatalf("repack returned (%v) without renaming the segment", err)
	}
	schedule(t, f, sim.Rule{Op: journalv2test.OpWrite, Key: "*.seg", Action: sim.FaultAction{Kind: sim.Drop}})
	writes := f.Seam.Calls(journalv2test.OpWrite)
	rename.Release()
	err := <-done
	stub(t, "Repack", err)
	if err != nil {
		t.Fatalf("repack segment %d: %v", seg, err)
	}
	f.Seam.SetFault(nil)

	r := retirement(t, f, name, files)
	if !r.recycled {
		t.Fatalf("segment %d was unlinked with the pool empty, want it recycled", seg)
	}
	if n := f.Seam.Calls(journalv2test.OpWrite) - writes; n < 2 || n != f.Seam.CallsFor(journalv2test.OpWrite, r.name) {
		t.Fatalf("%d writes after the rename, %d of them to %s, want at least two and all of them there", n, f.Seam.CallsFor(journalv2test.OpWrite, r.name), r.name)
	}
	if b := durable(t, f, r.name); string(b[len(b)-raw.TrailerSize:][:4]) != "DJT1" {
		t.Fatalf("the trailer slot of %s is not the old trailer on the device; the zeroing was not dropped", r.name)
	}

	// The recycled segment is the only spare in the pool, so the next roll
	// takes it.
	id := idOf(t, r.name)
	fill, _ := fillSegment(t, f, 0, 2, size)
	off := int64(len(fill)) * size
	first := rec{0, journalv2test.File(2), journalv2.Extent{Off: off, Len: size}, journalv2.Version{}, pattern(fmtTag("r", 0, 2, off), size)}
	e := entryAt(t, f, first.id, off)
	if e.Location.Segment != id {
		t.Fatalf("the roll took segment %d, want the recycled %d, the only spare in the pool", e.Location.Segment, id)
	}
	first.version = e.Version
	syncFiles(t, f.Share(0), first.id)
	held.Release()

	return recycled{id, r.name, footerOffset - raw.SealMarkerSize, first, others[0]}
}

// wantNewLifeOnly fails the test unless, after an open, the recycled
// segment serves its new life's records and nothing of its old one: every
// record in recs reads, no entry of file 0, the old life's file, is located
// in it, the unreclaimed bytes and the segment's held and retained bytes are
// as before the open, which an old record attached as superseded would
// raise, and nothing is reported as a loss or as damage.
func wantNewLifeOnly(t *testing.T, f *journalv2test.Fixture, rc recycled, recs []rec, unreclaimed int64, info journalv2.SegmentInfo) {
	t.Helper()

	for _, r := range recs {
		wantBytes(t, f.Share(r.share), r.id, r.extent.Off, r.p)
	}
	wantBytes(t, f.Share(1), rc.other.id, 0, rc.other.p)

	for _, e := range heldEntries(t, f, journalv2test.File(0)) {
		if e.Location.Segment == rc.id {
			t.Fatalf("file 0 holds %+v from the old life of the recycled segment %d", e, rc.id)
		}
	}

	s := f.J.Stats()
	if s.UnreclaimedBytes != unreclaimed {
		t.Fatalf("UnreclaimedBytes = %d after the open, want %d as before: the old life's records were attached as superseded", s.UnreclaimedBytes, unreclaimed)
	}
	if now := segmentInfo(t, f, rc.id); now.HeldBytes != info.HeldBytes || now.RetainedBytes != info.RetainedBytes {
		t.Fatalf("the recycled segment holds %d and retains %d bytes after the open, want %d and %d as before", now.HeldBytes, now.RetainedBytes, info.HeldBytes, info.RetainedBytes)
	}
	if len(s.Losses) != 0 || s.DamagedSegments != 0 {
		t.Fatalf("the old life was reported: losses %+v, DamagedSegments = %d", s.Losses, s.DamagedSegments)
	}
}

// heldEntries returns the index entries of file id.
func heldEntries(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID) []journalv2.IndexEntry {
	t.Helper()

	entries, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect a file: %v", err)
	}
	return entries
}

// wantLeftSegment fails the test unless the stream of file 2 left segment
// seg for one naming no predecessor, seg is reported failed, a reopen
// reports no damage, and seg is then unlinked by repack, never recycled,
// with kept, a record synced into it before the failure, reading after the
// retirement and after another reopen.
func wantLeftSegment(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID, kept rec, off int64) {
	t.Helper()

	const size = 64 << 10
	next := writeSynced(t, f, 0, 2, off, size)
	nseg := entryAt(t, f, next.id, off).Location.Segment
	if nseg == seg {
		t.Fatalf("the stream appended into segment %d after a failed append there", seg)
	}
	hdr, err := raw.ParseSegmentHeader(durable(t, f, segmentName(nseg))[:raw.SegmentHeaderSize])
	if err != nil {
		t.Fatalf("parse the next segment's header: %v", err)
	}
	if hdr.Predecessor != 0 {
		t.Fatalf("segment %d names predecessor %d after a failed append ended the stream's segment, want none", nseg, hdr.Predecessor)
	}
	if !segmentInfo(t, f, seg).Failed {
		t.Fatalf("segment %d is not reported failed after a failed append", seg)
	}

	f.Reopen()
	if got := f.J.Stats().DamagedSegments; got != 0 {
		t.Fatalf("DamagedSegments = %d after a reopen with a failed append's slot on disk, want 0: that slot is torn, never corrupt", got)
	}
	wantBytes(t, f.Share(0), kept.id, kept.extent.Off, kept.p)
	wantBytes(t, f.Share(0), next.id, off, next.p)

	if r := retire(t, f, seg); r.recycled {
		t.Fatalf("segment %d, ended by a failed append, was recycled as %s, want it unlinked", seg, r.name)
	}
	wantBytes(t, f.Share(0), kept.id, kept.extent.Off, kept.p)
	f.Reopen()
	wantBytes(t, f.Share(0), kept.id, kept.extent.Off, kept.p)
}

// TestRFC28C_RecycledSegmentsOldCatalogIsNeverRead checks that a recycled
// segment's old life stays dead whatever is left of it on the device: its
// trailer, kept by a dropped zeroing, is never read as a catalog, its records
// never attach, and the bytes past the new life's records are a torn tail;
// that a failed append into it, or a short write into a fresh segment, ends
// the segment without recycling it; and that a crash keeping a later append
// and losing an earlier one attaches the later one, seals nothing, and moves
// the stream on with no predecessor.
func TestRFC28C_RecycledSegmentsOldCatalogIsNeverRead(t *testing.T) {
	const size = 64 << 10

	// A clean reopen, then, on a journal of its own, a crash: the clean
	// close may seal the segment and truncate the old life away, so the
	// crash leg starts from a fresh recycle.
	f := journalv2test.New(t)
	rc := recycledSegment(t, f)
	recs := []rec{rc.first}
	for n := range 3 {
		recs = append(recs, writeSynced(t, f, 0, 2, rc.first.extent.Off+int64(n+1)*size, size))
	}
	unreclaimed, info := f.J.Stats().UnreclaimedBytes, segmentInfo(t, f, rc.id)
	f.Reopen()
	wantNewLifeOnly(t, f, rc, recs, unreclaimed, info)
	// Left unsealed by the close, the segment carries only the old life's
	// trailer, which no open may read as a catalog: it is read by scan.
	if r := f.J.Stats().Recovery; !segmentInfo(t, f, rc.id).Sealed && r.CatalogSegments != 0 {
		t.Fatalf("recovery read %d segments by catalog with the recycled segment %d unsealed; its old life's trailer is no catalog", r.CatalogSegments, rc.id)
	}

	g := journalv2test.NewUnwatched(t)
	rc = recycledSegment(t, g)
	recs = []rec{rc.first}
	for n := range 3 {
		recs = append(recs, writeSynced(t, g, 0, 2, rc.first.extent.Off+int64(n+1)*size, size))
	}
	unreclaimed, info = g.J.Stats().UnreclaimedBytes, segmentInfo(t, g, rc.id)
	g.Crash()
	wantNewLifeOnly(t, g, rc, recs, unreclaimed, info)
	if got := g.J.Stats().Counters.TornTails; got != 1 {
		t.Fatalf("TornTails = %d after a crash left the old life's bytes past the new records, want 1", got)
	}
	if got := int64(len(durable(t, g, rc.name))); got >= rc.marker {
		t.Fatalf("the recycled segment is %d bytes after recovery, want it cut below the old seal marker at %d", got, rc.marker)
	}

	// A failed append into the recycled segment while a later one
	// completes, after a record was synced there.
	h := journalv2test.NewUnwatched(t)
	rc = recycledSegment(t, h)
	hh, id := h.Share(0), journalv2test.File(2)
	off := rc.first.extent.Off + size
	var rs [2]*journalv2.Reservation
	for i := range rs {
		r, err := hh.Reserve(size, 1)
		stub(t, "Reserve", err)
		if err != nil {
			t.Fatalf("reserve a write: %v", err)
		}
		rs[i] = r
	}
	h.FailNext(journalv2test.OpWrite, rc.name, 1)
	_, err := rs[0].WriteAt(t.Context(), id, off, pattern(fmtTag("x", 0, 2, off), size), 0)
	stub(t, "WriteAt", err)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("the write whose storage call failed returned %v, want the device's error", err)
	}
	later := pattern(fmtTag("y", 0, 2, off+size), size)
	_, err = rs[1].WriteAt(t.Context(), id, off+size, later, 0)
	stub(t, "WriteAt", err)
	if err != nil {
		t.Fatalf("the later write: %v", err)
	}
	for _, r := range rs {
		if err := r.Close(); err != nil {
			t.Fatalf("close a reservation: %v", err)
		}
	}
	err = hh.Sync(t.Context(), id)
	stub(t, "Sync", err)
	var se *journalv2.SyncError
	switch {
	case errors.As(err, &se):
		wantMissing(t, hh, id, journalv2.Extent{Off: off + size, Len: size})
	case err != nil:
		t.Fatalf("sync after a failed append: %v", err)
	default:
		if got := h.J.Stats().Counters.SyncReappendedBytes; got < size {
			t.Fatalf("SyncReappendedBytes = %d after a sync that neither failed nor re-appended the later write", got)
		}
		wantBytes(t, hh, id, off+size, later)
	}
	wantLeftSegment(t, h, rc.id, rc.first, off+2*size)

	// A short write into a fresh segment: the one the stream moved to.
	pre := writeSynced(t, h, 0, 2, off+3*size, size)
	fresh := entryAt(t, h, pre.id, pre.extent.Off).Location.Segment
	schedule(t, h, sim.Rule{Op: journalv2test.OpWrite, Key: segmentName(fresh), Count: 1, Action: sim.FaultAction{Kind: sim.Tear, N: 4096, Err: syscall.EIO}})
	_, err = tryWrite(t, h.Share(0), id, off+4*size, pattern(fmtTag("z", 0, 2, off+4*size), size))
	h.Seam.SetFault(nil)
	stub(t, "WriteAt", err)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("the short write returned %v, want the device's error", err)
	}
	wantLeftSegment(t, h, fresh, pre, off+5*size)

	// A crash keeping a later append and losing an earlier one: the earlier
	// write is dropped at the seam, the later made durable by the device.
	k := journalv2test.NewUnwatched(t)
	rc = recycledSegment(t, k)
	off = rc.first.extent.Off + size
	schedule(t, k, sim.Rule{Op: journalv2test.OpWrite, Key: rc.name, Count: 1, Action: sim.FaultAction{Kind: sim.Drop}})
	w1 := writeRec(t, k, 0, 2, off, size)
	w2 := writeRec(t, k, 0, 2, off+size, size)
	k.Seam.SetFault(nil)
	w2Loc := entryAt(t, k, w2.id, w2.extent.Off).Location
	if w1Seg := entryAt(t, k, w1.id, off).Location.Segment; w1Seg != rc.id || w2Loc.Segment != rc.id {
		t.Fatalf("the two writes landed in segments %d and %d, want both in the recycled %d", w1Seg, w2Loc.Segment, rc.id)
	}
	deviceSync(t, k, rc.name)
	k.Crash()

	wantBytes(t, k.Share(0), w2.id, w2.extent.Off, w2.p)
	wantMissing(t, k.Share(0), w1.id, w1.extent)
	s := k.J.Stats()
	if len(s.Losses) != 0 || s.DamagedSegments != 0 {
		t.Fatalf("the lost earlier write was reported: losses %+v, DamagedSegments = %d", s.Losses, s.DamagedSegments)
	}
	if segmentInfo(t, k, rc.id).Sealed {
		t.Fatalf("the recycled segment %d was sealed by recovery behind a slot holding the old life's bytes", rc.id)
	}
	b := durable(t, k, rc.name)
	if end := w2Loc.Offset + raw.RecordHeaderSize + 8 + size; int64(len(b)) >= end+4 && string(b[end:end+4]) == "DJS1" {
		t.Fatalf("recovery wrote a seal marker after the later write in segment %d", rc.id)
	}
	next := writeSynced(t, k, 0, 2, off+2*size, size)
	nseg := entryAt(t, k, next.id, next.extent.Off).Location.Segment
	if nseg == rc.id {
		t.Fatalf("the stream appended into segment %d after recovery ended it", rc.id)
	}
	hdr, err := raw.ParseSegmentHeader(durable(t, k, segmentName(nseg))[:raw.SegmentHeaderSize])
	if err != nil {
		t.Fatalf("parse the next segment's header: %v", err)
	}
	if hdr.Predecessor != 0 {
		t.Fatalf("segment %d names predecessor %d after recovery ended the stream's segment without sealing it, want none", nseg, hdr.Predecessor)
	}
	wantBytes(t, k.Share(1), rc.other.id, 0, rc.other.p)
}

// TestRFC28C_StaleExtent checks that content older than what the remote
// tier holds, brought back by a segment restored from outside, is dropped at
// the reseed as stale: the extent reads missing, the event names it stale,
// and the loss generation does not move, since the next read fetches the
// current content.
//
// decision: this is also the check of the edits table's row for one old
// segment of this journal restored: noticed at the reseed, by version, its
// extents dropped as stale and refetched.
func TestRFC28C_StaleExtent(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 64 << 10

	// A and its offloaded record in segment 1, with file 2's records
	// filling it to the roll; a copy of it taken once sealed.
	a := writeSynced(t, f, 0, 0, 0, size)
	mark(t, h, a.id, a.extent, a.version)
	syncFiles(t, h, a.id)
	seg1 := entryAt(t, f, id, 0).Location.Segment
	fill1, first := fillSegment(t, f, 0, 2, size)
	if first != seg1 {
		t.Fatalf("file 2's records landed in segment %d, want A's segment %d filled to the roll", first, seg1)
	}
	other := writeSynced(t, f, 1, 1, 0, size)
	syncFiles(t, h, journalv2test.File(2))
	copied := durable(t, f, segmentName(seg1))

	// B over A in segment 2, with its offloaded and release records, and
	// file 3's records filling it to the roll, so everything that outranks
	// A goes with that segment.
	b := writeSynced(t, f, 0, 0, 0, size)
	seg2 := entryAt(t, f, id, 0).Location.Segment
	if seg2 == seg1 {
		t.Fatalf("B landed in A's segment %d", seg1)
	}
	mark(t, h, b.id, b.extent, b.version)
	wantRelease(t, h, b.id, b.extent, true)
	fill2, _ := fillSegment(t, f, 0, 3, size)
	syncFiles(t, h, id, journalv2test.File(3))

	// Segment 1 is retired first: B's release counts segment 1 as still on
	// disk until then, and only once segment 1's unlink is durable may the
	// release be dropped with segment 2 (RFC 1 section 7.2).
	releaseRecs(t, f, fill1)
	retire(t, f, seg1)
	releaseRecs(t, f, fill2)
	retire(t, f, seg2)
	plant(t, f, segmentName(seg1), copied)
	f.Reopen()
	h = f.Share(0)

	// The row's premise: A is back, held at its version and marked by its
	// own offloaded record, with nothing on disk outranking it.
	if e := entryAt(t, f, id, 0); e.Version != a.version || !e.Offloaded {
		t.Fatalf("precondition: A is held as %+v after the restore, want its version %v and its offloaded bit", e, a.version)
	}

	before := f.J.Stats()
	err := h.MarkOffloaded(t.Context(), id, []journalv2.Extent{a.extent}, b.version, b.version)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark at B's versions: %v", err)
	}

	ev := lastLoss(t, f, before)
	if ev.File != id || ev.Extent != a.extent || ev.Reason != journalv2.LossStale {
		t.Fatalf("loss event %+v, want file %v, extent %+v, dropped as stale", ev, id, a.extent)
	}
	if got := f.J.Stats().Counters.StaleExtents; got != 1 {
		t.Fatalf("StaleExtents = %d after one extent dropped as stale, want 1", got)
	}
	wantMissing(t, h, id, a.extent)
	if got := f.J.Stats().LossGeneration; got != before.LossGeneration {
		t.Fatalf("LossGeneration = %d after a stale loss, want %d unchanged", got, before.LossGeneration)
	}
	wantBytes(t, f.Share(1), other.id, 0, other.p)
}

// TestRFC28C_VersionFloor checks that a journal opened with a floor above
// every version on disk issues its next version above the floor, and that a
// mark whose newest version is the floor leaves that write unmarked: the
// write is newer than what the mark vouches for.
func TestRFC28C_VersionFloor(t *testing.T) {
	f := journalv2test.New(t)
	a := writeSynced(t, f, 1, 1, 0, 64<<10)

	// The fixture opens with the last limit as floor, far above every
	// version on disk.
	f.Reopen()
	floor := f.Floor()
	if a.version.Compare(floor) >= 0 {
		t.Fatalf("the floor %v is not above A's version %v, which the row needs", floor, a.version)
	}

	b := writeSynced(t, f, 0, 0, 0, 64<<10)
	if b.version.Compare(floor) <= 0 {
		t.Fatalf("B's version %v is not above the floor %v", b.version, floor)
	}

	h := f.Share(0)
	err := h.MarkOffloaded(t.Context(), b.id, []journalv2.Extent{b.extent}, floor, floor)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark with the floor as newest: %v", err)
	}
	wantRelease(t, h, b.id, b.extent, false)
	if entryAt(t, f, b.id, 0).Offloaded {
		t.Fatalf("B's offloaded bit is set after a mark whose newest version is below B's")
	}
	wantBytes(t, f.Share(1), a.id, 0, a.p)
	wantBytes(t, h, b.id, 0, b.p)
}

// TestRFC28C_JournalGone checks that a journal that has gone is not replaced
// in silence: an open with the recorded identity fails on an emptied
// directory, on a missing one and on one holding another journal, creating
// nothing, and only Init then makes a new journal; and that an open with no
// recorded identity fails naming the journal it found and its installation.
//
// decision: this is also the check of the edits table's row for the whole
// directory deleted, emptied or replaced: noticed at open by the identity
// the caller expects, the open failing and no journal created until an
// operator acknowledges the loss.
//
// decision: the missing directory is removed on the real filesystem, the one
// edit this suite makes past the seam, because the seam has no call that
// removes a directory; the open then sees the seam's listing fail as the
// operating system's would. The leg with another journal in the directory's
// place runs on a fixture of its own, since the seam keeps the names it has
// seen and cannot create them again once the directory was removed under it.
func TestRFC28C_JournalGone(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)
	writeSynced(t, f, 0, 0, 0, size)
	writeSynced(t, f, 1, 1, 0, size)
	old := f.ID()
	f.Shutdown()
	emptyDir(t, f)
	before := dirNow(t, f)

	err := f.TryReopen()
	stub(t, "Open", err)
	if !errors.Is(err, journalv2.ErrNoJournal) {
		t.Fatalf("opening the emptied directory returned %v, want ErrNoJournal", err)
	}
	wantUntouched(t, f, before)

	id, err := f.TryInit()
	stub(t, "Init", err)
	if err != nil {
		t.Fatalf("init on the emptied directory: %v", err)
	}
	if id == old {
		t.Fatalf("Init gave the new journal the gone journal's identity %x", id)
	}
	if err := f.TryReopen(); err != nil {
		t.Fatalf("open the new journal: %v", err)
	}
	for id, entries := range f.J.Inspect().All() {
		t.Fatalf("the new journal holds %+v of file %v", entries, id)
	}
	a := writeSynced(t, f, 0, 0, 0, size)
	wantBytes(t, f.Share(0), a.id, 0, a.p)

	// The directory itself gone.
	g := journalv2test.NewUnwatched(t)
	writeSynced(t, g, 0, 0, 0, size)
	g.Shutdown()
	if err := os.RemoveAll(g.Dir); err != nil {
		t.Fatalf("remove the directory: %v", err)
	}
	err = g.TryReopen()
	stub(t, "Open", err)
	if !errors.Is(err, journalv2.ErrNoJournal) {
		t.Fatalf("opening the missing directory returned %v, want ErrNoJournal", err)
	}
	if _, err := os.Stat(g.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the open created the missing directory: %v", err)
	}

	// Another journal in its place.
	k := journalv2test.NewUnwatched(t)
	writeSynced(t, k, 0, 0, 0, size)
	other := journalv2test.NewUnwatched(t)
	writeSynced(t, other, 0, 5, 0, size)
	other.Shutdown()
	k.Shutdown()
	emptyDir(t, k)
	for _, name := range list(t, other) {
		plant(t, k, name, durable(t, other, name))
	}
	before = dirNow(t, k)
	wantIdentityError(t, k.TryReopen(), other.ID(), journalv2test.FixtureInstall, true)
	wantUntouched(t, k, before)

	// No recorded identity: refused naming what is there, before and after a
	// stream takes a segment.
	m := journalv2test.NewUnwatched(t)
	m.Shutdown()
	before = dirNow(t, m)
	wantIdentityError(t, m.TryReopenAs(journalv2.JournalID{}), m.ID(), journalv2test.FixtureInstall, false)
	wantUntouched(t, m, before)
	if err := m.TryReopen(); err != nil {
		t.Fatalf("open with the recorded identity: %v", err)
	}
	writeSynced(t, m, 0, 0, 0, size)
	m.Shutdown()
	wantIdentityError(t, m.TryReopenAs(journalv2.JournalID{}), m.ID(), journalv2test.FixtureInstall, true)
}

// TestRFC28C_UnidentifiedDirectory checks that a directory the journal cannot
// identify is refused and left as it is: one holding segments and no format
// file fails to open with ErrUnidentified, is never taken for an empty
// journal, and Init refuses it; one holding only files the journal never
// writes fails to open, and Init refuses it with ErrNotEmpty; and an empty
// directory fails to open, while Init creates a journal there whose format
// file carries the installation ID it was given.
//
// decision: the directory lost+found is made on the real filesystem, past
// the seam, which has no call that makes a directory, and that leg reads the
// real directory for its untouched check for the same reason; the seam's
// call counts still show every call the journal made. Its open is asserted
// to fail with ErrNoJournal: the format says a directory with neither a
// segment nor a format file holds no journal, and that is the error the
// open gives a directory that holds none.
func TestRFC28C_UnidentifiedDirectory(t *testing.T) {
	const size = 64 << 10

	// Segments and no format file.
	f := journalv2test.New(t)
	writeSynced(t, f, 0, 0, 0, size)
	writeSynced(t, f, 1, 1, 0, size)
	f.Shutdown()
	if err := f.Seam.Remove("format"); err != nil {
		t.Fatalf("remove the format file: %v", err)
	}

	if err := f.Seam.SyncDir(); err != nil {
		t.Fatalf("sync the directory: %v", err)
	}

	before := dirNow(t, f)
	err := f.TryReopen()
	stub(t, "Open", err)
	if !errors.Is(err, journalv2.ErrUnidentified) {
		t.Fatalf("opening a directory with segments and no format file returned %v, want ErrUnidentified", err)
	}

	wantUntouched(t, f, before)

	// The interface names ErrNotEmpty for any entry, while Init's contract
	// names ErrJournalExists for a segment, so both are accepted until the
	// two agree.
	_, err = f.TryInit()
	stub(t, "Init", err)
	if !errors.Is(err, journalv2.ErrNotEmpty) && !errors.Is(err, journalv2.ErrJournalExists) {
		t.Fatalf("Init on a directory with segments and no format file returned %v, want ErrNotEmpty or ErrJournalExists", err)
	}

	wantUntouched(t, f, before)

	// Only unrelated files: a plain file and a directory, neither a name the
	// journal writes.
	g := journalv2test.NewUnwatched(t)
	g.Shutdown()
	emptyDir(t, g)
	plant(t, g, "notes.txt", pattern("notes", 4096))
	if err := os.Mkdir(filepath.Join(g.Dir, "lost+found"), 0o755); err != nil {
		t.Fatalf("make lost+found: %v", err)
	}

	unrelated := realDirNow(t, g)
	err = g.TryReopen()
	stub(t, "Open", err)
	if !errors.Is(err, journalv2.ErrNoJournal) {
		t.Fatalf("opening a directory holding only unrelated files returned %v, want ErrNoJournal", err)
	}

	wantRealUntouched(t, g, unrelated)
	_, err = g.TryInit()
	stub(t, "Init", err)
	if !errors.Is(err, journalv2.ErrNotEmpty) {
		t.Fatalf("Init on a directory holding only unrelated files returned %v, want ErrNotEmpty", err)
	}

	wantRealUntouched(t, g, unrelated)

	// An empty directory.
	k := journalv2test.NewUnwatched(t)
	k.Shutdown()
	emptyDir(t, k)
	err = k.TryReopen()
	stub(t, "Open", err)
	if !errors.Is(err, journalv2.ErrNoJournal) {
		t.Fatalf("opening an empty directory returned %v, want ErrNoJournal", err)
	}

	_, err = k.TryInit()
	stub(t, "Init", err)
	if err != nil {
		t.Fatalf("Init on an empty directory: %v", err)
	}

	if err := k.TryReopen(); err != nil {
		t.Fatalf("open the new journal: %v", err)
	}

	format, err := raw.ParseFormat(durable(t, k, "format"))
	if err != nil {
		t.Fatalf("the new journal's format file does not parse: %v", err)
	}

	if format.Installation != [16]byte(journalv2test.FixtureInstall) {
		t.Fatalf("the new journal's format file carries installation %x, want the %x Init was given", format.Installation, journalv2test.FixtureInstall)
	}
}

// realDirImage is a directory as the real filesystem shows it, for a
// directory holding entries the seam did not make: each name with whether it
// is a directory and, for a file, its bytes, and the seam's counts of the
// calls that change a directory or a file.
type realDirImage struct {
	dirs  map[string]bool
	files map[string][]byte
	calls map[string]int
}

// realDirNow reads the directory's image from the real filesystem.
func realDirNow(t *testing.T, f *journalv2test.Fixture) realDirImage {
	t.Helper()

	img := realDirImage{dirs: map[string]bool{}, files: map[string][]byte{}, calls: map[string]int{}}
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			img.dirs[e.Name()] = true
			continue
		}

		b, err := os.ReadFile(filepath.Join(f.Dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		img.files[e.Name()] = b
	}

	for _, op := range changingOps {
		img.calls[op] = f.Seam.Calls(op)
	}
	return img
}

// wantRealUntouched fails the test unless the real directory holds the same
// entries with the same bytes as before, and no call that changes a
// directory or a file was made since.
func wantRealUntouched(t *testing.T, f *journalv2test.Fixture, before realDirImage) {
	t.Helper()

	now := realDirNow(t, f)
	if !maps.Equal(now.dirs, before.dirs) {
		t.Fatalf("the directories are %v, were %v; the directory was to be left alone", slices.Sorted(maps.Keys(now.dirs)), slices.Sorted(maps.Keys(before.dirs)))
	}

	for name, b := range before.files {
		if got, ok := now.files[name]; !ok || !bytes.Equal(got, b) {
			t.Fatalf("%s was changed or removed; the directory was to be left alone", name)
		}
	}

	for name := range now.files {
		if _, ok := before.files[name]; !ok {
			t.Fatalf("%s was created; the directory was to be left alone", name)
		}
	}

	for _, op := range changingOps {
		if now.calls[op] != before.calls[op] {
			t.Fatalf("%d %s calls were made; the directory was to be left alone", now.calls[op]-before.calls[op], op)
		}
	}
}

// TestRFC28C_InterruptedInit checks that a journal whose identity its caller
// never recorded is refused with the identity and installation to record, and
// with no segment reported while nothing was ever acknowledged; that once
// recorded it opens empty and serves; and that with a segment taken it is
// still refused, the segment reported.
//
// decision: the row's crash is what leaves the identity unrecorded; the
// fixture, which is that caller, closes cleanly instead, since the open finds
// the format file either way and a crash would drop only unsynced records,
// of which there are none.
func TestRFC28C_InterruptedInit(t *testing.T) {
	f := journalv2test.New(t)
	wantIdentityError(t, f.TryReopenAs(journalv2.JournalID{}), f.ID(), journalv2test.FixtureInstall, false)

	if err := f.TryReopenAs(f.ID()); err != nil {
		t.Fatalf("open with the identity recorded: %v", err)
	}
	for id, entries := range f.J.Inspect().All() {
		t.Fatalf("the journal holds %+v of file %v, want it empty", entries, id)
	}
	a := writeSynced(t, f, 0, 0, 0, 64<<10)
	wantBytes(t, f.Share(0), a.id, 0, a.p)
	b := writeSynced(t, f, 1, 1, 0, 64<<10)
	wantBytes(t, f.Share(1), b.id, 0, b.p)

	wantIdentityError(t, f.TryReopenAs(journalv2.JournalID{}), f.ID(), journalv2test.FixtureInstall, true)
}

// wantLeftAlone fails the test unless the file name still holds bytes b and
// no call changed it since the call counts before: no write, truncate,
// rename or remove.
func wantLeftAlone(t *testing.T, f *journalv2test.Fixture, name string, b []byte, before map[string]int) {
	t.Helper()

	if !bytes.Equal(durable(t, f, name), b) {
		t.Fatalf("%s was changed; it was to be left alone", name)
	}
	for _, op := range []string{journalv2test.OpWrite, journalv2test.OpTruncate, journalv2test.OpRename, journalv2test.OpRemove} {
		if got := f.Seam.CallsFor(op, name); got != before[op] {
			t.Fatalf("%s was %sd %d times; it was to be left alone", name, op, got-before[op])
		}
	}
}

// callsFor returns the seam's count of each of the changing calls on name.
func callsFor(f *journalv2test.Fixture, name string) map[string]int {
	calls := map[string]int{}
	for _, op := range changingOps {
		calls[op] = f.Seam.CallsFor(op, name)
	}
	return calls
}

// TestRFC28C_ForeignSegment checks that a segment of another journal, copied
// into the directory, is not attached: no read serves its bytes, it is
// counted as rejected, and it is left on disk as it was.
//
// decision: this is also the check of the edits table's row for a valid
// segment copied in from another journal: noticed at open by its journal
// identity, not attached, reported and left alone.
func TestRFC28C_ForeignSegment(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)
	g := journalv2test.NewUnwatched(t)
	recs, seg := fillSegment(t, g, 0, 5, size)
	syncFiles(t, g.Share(0), recs[0].id)
	foreign := durable(t, g, segmentName(seg))

	a := writeSynced(t, f, 0, 0, 0, size)
	b := writeSynced(t, f, 1, 1, 0, size)
	name := segmentName(newestID(t, f) + 1)
	plant(t, f, name, foreign)
	before := callsFor(f, name)
	f.Reopen()

	if got := f.J.Stats().RejectedSegments; got != 1 {
		t.Fatalf("RejectedSegments = %d with a foreign segment in the directory, want 1", got)
	}
	if entries := heldEntries(t, f, journalv2test.File(5)); len(entries) != 0 {
		t.Fatalf("the foreign segment's file is indexed as %+v", entries)
	}
	wantMissing(t, f.Share(0), journalv2test.File(5), recs[0].extent)
	wantLeftAlone(t, f, name, foreign, before)
	wantBytes(t, f.Share(0), a.id, 0, a.p)
	wantBytes(t, f.Share(1), b.id, 0, b.p)
}

// TestRFC28C_ForeignHeaderIsForeignNotDamaged checks that a segment written
// under another identity is told foreign by the identity its header names,
// the header verifying on its own: it is counted as rejected, nothing as
// damaged, and nothing of it is attached.
//
// decision: the golden bytes of a segment header and of a seal marker are
// pinned by the raw package's layout tests, and a seal marker copied into a
// payload is checked by the scan check of this suite; this is the row's
// foreign half.
func TestRFC28C_ForeignHeaderIsForeignNotDamaged(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)
	a := writeSynced(t, f, 0, 0, 0, size)
	b := writeSynced(t, f, 1, 1, 0, size)

	other := [16]byte{'o', 't', 'h', 'e', 'r', '-', 'j', 'o', 'u', 'r', 'n', 'a', 'l'}
	id := newestID(t, f) + 1
	extent := journalv2.Extent{Off: 0, Len: size}
	record := raw.Record{
		Kind: 1, FileID: journalv2test.File(5).Bytes(), Offset: 0, Length: size,
		Seq: 1 << 40, Version: raw.Version{Counter: 1 << 40}, Tag: uint32(journalv2test.Tag(0)),
		Write: &raw.WriteBody{Payload: pattern(fmtTag("f", 0, 5, 0), size)},
	}
	entry := raw.Entry{
		FileID: journalv2test.File(5).Bytes(), Offset: 0, RecordOffset: raw.SegmentHeaderSize, Length: size,
		Seq: 1 << 40, Version: raw.Version{Counter: 1 << 40}, Kind: 1, Tag: uint32(journalv2test.Tag(0)),
	}
	segment := sealedSegmentBytes(other, id, []raw.Record{record}, []raw.Entry{entry})
	name := segmentName(id)
	plant(t, f, name, segment)
	before := callsFor(f, name)
	f.Reopen()

	s := f.J.Stats()
	if s.RejectedSegments != 1 || s.DamagedSegments != 0 {
		t.Fatalf("RejectedSegments = %d, DamagedSegments = %d with a segment of another identity, want 1 and 0", s.RejectedSegments, s.DamagedSegments)
	}
	if entries := heldEntries(t, f, journalv2test.File(5)); len(entries) != 0 {
		t.Fatalf("the foreign segment's file is indexed as %+v", entries)
	}
	wantMissing(t, f.Share(0), journalv2test.File(5), extent)
	wantLeftAlone(t, f, name, segment, before)
	wantBytes(t, f.Share(0), a.id, 0, a.p)
	wantBytes(t, f.Share(1), b.id, 0, b.p)
}

// plantKind plants a segment of the journal, under a fresh name, holding one
// record of kind kind at its first slot and nothing after it: a write's body
// when kind is a write, no body otherwise, no seal marker and no trailer, as
// a segment left open at a crash.
func plantKind(t *testing.T, f *journalv2test.Fixture, kind uint8) {
	t.Helper()

	const kindWrite = 1
	id := newestID(t, f) + 1
	identity := journalIdentity(t, f)
	r := raw.Record{
		Identity: identity, Kind: kind, Segment: uint64(id), FileID: journalv2test.File(0).Bytes(),
		Seq: 1 << 40, Version: raw.Version{Counter: 1 << 40}, Tag: uint32(journalv2test.Tag(0)),
	}
	if kind == kindWrite {
		r.Offset, r.Length = 1<<20, 4096
		r.Write = &raw.WriteBody{Payload: pattern(fmtTag("k", 0, 0, 1<<20), 4096)}
	}
	b := append(raw.SegmentHeader{Identity: identity, ID: uint64(id)}.Bytes(), r.Bytes()...)
	plant(t, f, segmentName(id), b)
}

// TestRFC28C_KindValuesRefused checks that a verifying header whose kind the
// format does not define, in the reserved range and in the extension range,
// fails the open rather than being skipped by its length. A control with a
// known kind and the same bytes otherwise opens, so the refusal is the
// kind's and not the planted segment's.
//
// decision: the golden vectors of the kinds table are pinned by the raw
// package's record layout test, and their decoding is the journal's own
// code's to test; this is the row's refusal half.
func TestRFC28C_KindValuesRefused(t *testing.T) {
	for i, kind := range []uint8{15, 128} {
		f := journalv2test.NewUnwatched(t)
		if i == 0 {
			f = journalv2test.New(t)
		}
		writeSynced(t, f, 0, 0, 0, 64<<10)
		f.Shutdown()
		plantKind(t, f, kind)

		err := f.TryReopen()
		stub(t, "Open", err)
		if !errors.Is(err, journalv2.ErrUnknownFormat) {
			t.Fatalf("opening with a record of kind %d returned %v, want ErrUnknownFormat", kind, err)
		}
	}

	g := journalv2test.NewUnwatched(t)
	a := writeSynced(t, g, 0, 0, 0, 64<<10)
	g.Shutdown()
	plantKind(t, g, 1)
	if err := g.TryReopen(); err != nil {
		t.Fatalf("opening with a planted record of a known kind: %v; the refusals above are the plant's, not the kind's", err)
	}
	wantBytes(t, g.Share(0), a.id, 0, a.p)
}

// TestRFC28C_FormatFile checks that a format file of the wrong length, with
// a checksum that does not verify, or of a version the journal does not know
// fails the open, with nothing written to it; and that a crash inside a
// restamp, before the rename of the new file or before the directory sync
// after it, leaves the old file or the new one whole.
//
// decision: the golden 56 bytes of a version 1 format file are pinned by
// the raw package's layout test; this is the row's refusal and crash halves,
// with nothing written to the file either way (RFC 28 section 2.1, line
// 209). It is also
// the check of the edits table's row for the format file changed from
// outside, noticed at open; the row's other half, the file removed, is not
// covered yet.
func TestRFC28C_FormatFile(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)
	a := writeSynced(t, f, 0, 0, 0, size)
	b := writeSynced(t, f, 1, 1, 0, size)
	f.Shutdown()

	format := durable(t, f, "format")
	parsed, err := raw.ParseFormat(format)
	if err != nil {
		t.Fatalf("the journal's format file does not parse: %v", err)
	}

	// decision: a file of the wrong length may be read as a format the
	// journal does not know, a longer one being what a later version would
	// write, or refused as corrupt; either refusal fails the open as the row
	// asks.
	legs := []struct {
		name string
		edit func()
		want []error
	}{
		{"55 bytes", func() { truncateSegment(t, f, "format", raw.FormatSize-1) }, []error{journalv2.ErrCorrupt, journalv2.ErrUnknownFormat}},
		{"57 bytes", func() { overwrite(t, f, "format", raw.FormatSize, []byte{0}) }, []error{journalv2.ErrCorrupt, journalv2.ErrUnknownFormat}},
		{"a bad checksum", func() { flipAt(t, f, "format", 52) }, []error{journalv2.ErrCorrupt}},
		{"version 2", func() {
			overwrite(t, f, "format", 0, raw.Format{Version: 2, Identity: parsed.Identity, Installation: parsed.Installation}.Bytes())
		}, []error{journalv2.ErrUnknownFormat}},
	}
	for _, leg := range legs {
		leg.edit()
		before := callsFor(f, "format")
		creates := f.Seam.CallsFor(journalv2test.OpCreate, "format.tmp")
		err := f.TryReopen()
		stub(t, "Open", err)
		if !slices.ContainsFunc(leg.want, func(want error) bool { return errors.Is(err, want) }) {
			t.Fatalf("opening with a format file of %s returned %v, want one of %v", leg.name, err, leg.want)
		}
		if got := callsFor(f, "format"); got[journalv2test.OpWrite] != before[journalv2test.OpWrite] || got[journalv2test.OpTruncate] != before[journalv2test.OpTruncate] || f.Seam.CallsFor(journalv2test.OpCreate, "format.tmp") != creates {
			t.Fatalf("the open wrote to the format file of %s; it is never repaired or upgraded in silence (RFC 28 section 2.1, line 209)", leg.name)
		}

		// The file put back as it was.
		truncateSegment(t, f, "format", raw.FormatSize)
		overwrite(t, f, "format", 0, format)
	}

	// A restamp crashed before the rename of the new file, then one crashed
	// after the rename and before the directory sync.
	other := journalv2.InstallationID{'o', 't', 'h', 'e', 'r'}
	for _, step := range []struct{ op, key string }{{journalv2test.OpRename, "format.tmp"}, {journalv2test.OpSyncDir, "*"}} {
		f.Shutdown()
		renames := f.Seam.CallsFor(journalv2test.OpRename, "format.tmp")
		held := f.Seam.Hold(t, step.op, step.key)
		done := make(chan error, 1)
		go func() { done <- f.TryRestamp(other) }()
		select {
		case <-held.Reached():
		case err := <-done:
			stub(t, "Restamp", err)
			t.Fatalf("restamp returned (%v) without reaching its %s", err, step.op)
		}
		if step.op == journalv2test.OpSyncDir && f.Seam.CallsFor(journalv2test.OpRename, "format.tmp") != renames+1 {
			t.Fatalf("the held directory sync is not the one after the rename: format.tmp was renamed %d times by then", f.Seam.CallsFor(journalv2test.OpRename, "format.tmp")-renames)
		}
		crashHeld(t, f, held)
		<-done // a call the crash took; what it returned means nothing

		got, err := raw.ParseFormat(durable(t, f, "format"))
		if err != nil {
			t.Fatalf("after a crash inside a restamp at its %s, the format file is not whole: %v", step.op, err)
		}
		if got.Identity != parsed.Identity || (got.Installation != parsed.Installation && got.Installation != other) {
			t.Fatalf("after a crash inside a restamp at its %s, the format file names identity %x and installation %x, want the journal's identity and the old or the new installation", step.op, got.Identity, got.Installation)
		}
		wantBytes(t, f.Share(0), a.id, 0, a.p)
		wantBytes(t, f.Share(1), b.id, 0, b.p)
	}
}
