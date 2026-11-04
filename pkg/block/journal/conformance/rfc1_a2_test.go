//go:build journal_conformance

package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest"
)

// The second half of the checks that fail by serving or destroying the wrong
// bytes without an error: repack, sync windows and losses.

// sourceSegment returns a repack scope aimed at the one segment that holds
// the record of file id's first held entry.
func sourceSegment(t *testing.T, f *journaltest.Fixture, id journal.FileID) journal.RepackScope {
	t.Helper()

	seg := heldEntry(t, f, id).Location.Segment
	return journal.RepackScope{Segments: []journal.SegmentID{seg}}
}

// TestRFC1A_RepackCopiesLivePart checks that a repack of a segment holding a
// half-released offloaded record copies the record's kept ends whole at the
// record's version, drops its released middle, still answers a mark of the
// copies at that version, and unlinks the original, before and after a crash.
func TestRFC1A_RepackCopiesLivePart(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const mib = 1 << 20
	middle := ext(mib/4, mib/2)

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		p := distinct(k, mib)
		v := write(t, h, id, 0, p)
		// Only the middle is marked: the ends stay unmarked, so the marks
		// below are the first the copies ever take, and a repack that forgot
		// its copies' versions cannot answer them.
		mark(t, h, id, middle, v)
		release(t, h, id, middle)
		syncFile(t, h, id)

		scope := sourceSegment(t, f, id)
		if freed := repackAll(t, f, scope); freed <= 0 {
			t.Fatalf("share %d: the repack of the record's segment returned no freed bytes", k)
		}

		r := read(t, h, id, 0, mib)
		wantMissing(t, r, 0, []journal.Extent{middle}, "the released middle after its segment was repacked")
		if !bytes.Equal(r.p[:mib/4], p[:mib/4]) {
			t.Fatalf("share %d: the kept head reads other bytes than were written", k)
		}
		if !bytes.Equal(r.p[mib-mib/4:], p[mib-mib/4:]) {
			t.Fatalf("share %d: the kept end reads other bytes than were written", k)
		}

		// The copies keep the record's original versions, and marking the
		// offloaded bits at those versions marks the copies.
		for _, e := range entries(t, f, id) {
			if e.Kind == journal.EntryHeld && overlap(e.Extent, ext(0, mib/4)) > 0 && e.Version != v {
				t.Fatalf("share %d: the copied head carries version %v, want the record's %v", k, e.Version, v)
			}
		}
		wantBits(t, f, id, ext(0, mib/4), false, "the unmarked head after the repack")
		wantBits(t, f, id, ext(mib-mib/4, mib/4), false, "the unmarked end after the repack")
		mark(t, h, id, ext(0, mib/4), v)
		mark(t, h, id, ext(mib-mib/4, mib/4), v)
		wantBits(t, f, id, ext(0, mib/4), true, "the bit of a head marked at the record's version after the repack")
		wantBits(t, f, id, ext(mib-mib/4, mib/4), true, "the bit of an end marked at the record's version after the repack")

		// The marks of the ends are synced before the check's crash: an
		// offloaded record need not be synced when written, and a crash that
		// loses the bit loses the answer the crash leg below reads, so the
		// sync makes the marks durable whatever the journal does on its own.
		syncFile(t, h, id)

		if !retired(f, scope.Segments[0]) {
			t.Fatalf("share %d: the repacked source segment is still in the directory", k)
		}
	}

	f.Crash()
	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		p := distinct(k, mib)
		r := read(t, h, id, 0, mib)
		wantMissing(t, r, 0, []journal.Extent{middle}, "the released middle after a repack and a crash")
		if !bytes.Equal(r.p[:mib/4], p[:mib/4]) || !bytes.Equal(r.p[mib-mib/4:], p[mib-mib/4:]) {
			t.Fatalf("share %d: the kept ends read other bytes after a crash than before", k)
		}
		wantBits(t, f, id, ext(0, mib/4), true, "the kept head after a crash")
	}
}

// TestRFC1A_CrashMidRepack checks that a crash between a repack's copy and
// its unlink selects the copies on reopen: every held extent the pass copied
// still reads, no extent sits twice in the index, and nothing the pass
// copied is served from its source any more.
//
// decision: the crash lands with the unlinks the repacks made reported to the
// journal but dropped at the seam, dropping only the directory syncs that run
// once the pass's reclamation of its source is in sight — the copy's own
// creation and sync were reported durable before it — which is the one state
// between a copy made durable and its source's unlink durable the fixture can
// crash in. Its ceiling: a pass that reclaims before its copies verify makes
// no directory change after the drop is armed, so the crash reads as a plain
// crash of a complete pass, and a design that unlinks before its copies verify
// still passes. Overturned if the seam grows a crash that keeps one chosen
// directory sync, when the pass can be stopped at the unlink itself.
func TestRFC1A_CrashMidRepack(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const size = 256 << 10

	type copied struct {
		share  int
		p      []byte
		source journal.SegmentID
	}
	pieces := make([]copied, 2)
	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		p := distinct(k, size)
		write(t, h, id, 0, p)
		syncFile(t, h, id)
		seg := heldEntry(t, f, id).Location.Segment

		// The pass copies the record and syncs it, and only then reclaims
		// the source. Its reclamation is held at the source's unlink-or-
		// rename — the copy is durable by then, and the directory syncs
		// before it kept it — and every directory sync from that point on is
		// reported to the journal and made durable by nothing: the crash
		// puts the directory back with the source still in it and the copy
		// durable beside it. Faults replace each other at the seam, so the
		// drop is armed only once the reclamation is in sight, and a sync_dir
		// lands with no other call held; the reclamation's release frees it
		// before the crash runs, or the crash would strand a call at a hold.
		// The reclamation is one call on the source's name — its unlink or
		// the rename that recycles it — so the holds stop whichever it turns
		// out to be and match nothing else.
		unlink := f.Seam.HoldFirst(t, journaltest.OpRemove, segmentName(seg))
		rename := f.Seam.HoldFirst(t, journaltest.OpRename, segmentName(seg))
		repacked := make(chan error, 1)
		go func() {
			_, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{seg}})
			repacked <- err
		}()
		select {
		case <-unlink.Reached():
		case <-rename.Reached():
		case err := <-repacked:
			stub(t, "Repack", err)
			t.Fatalf("share %d: the repack returned (%v) with its source unheld, without reclaiming it", k, err)
		}
		f.Drop(journaltest.OpSyncDir, "*")
		unlink.Release()
		rename.Release()
		err := <-repacked
		stub(t, "Repack", err)
		if err != nil {
			t.Fatalf("share %d: the repack under a crash: %v", k, err)
		}
		f.Crash()
		f.Seam.SetFault(nil)
		pieces[k] = copied{k, p, seg}
	}

	// No extent is duplicated in the index: every held extent the repacks
	// copied sits once.
	for _, es := range f.J.Inspect().All() {
		var held []journal.IndexEntry
		for _, e := range es {
			if e.Kind == journal.EntryHeld {
				held = append(held, e)
			}
		}
		for i, a := range held {
			for _, b := range held[i+1:] {
				if overlap(a.Extent, b.Extent) > 0 {
					t.Fatalf("index entries %+v and %+v of one file both cover the same byte", a, b)
				}
			}
		}
	}
	for _, c := range pieces {
		h, id := f.Share(c.share), journaltest.File(c.share)
		got, missing := readBack(t, h, id, 0, size)
		if len(missing) != 0 || !bytes.Equal(got, c.p) {
			t.Fatalf("share %d: the copied extent reads missing %+v or other bytes after a crash mid-repack", c.share, missing)
		}
		// Recovery selected the copies: nothing the file holds is served from
		// the source any more. A reopen that rebuilt the index from the
		// source alone would hold its record there again.
		for _, e := range entries(t, f, id) {
			if e.Kind != journal.EntryMarker && e.Location.Segment == c.source {
				t.Fatalf("share %d: after a crash mid-repack %+v is still served from the repacked source %d", c.share, e, c.source)
			}
		}
	}
}

// TestRFC1A_RepackUnderRead checks that a read spanning a repack's copy and
// unlink returns the bytes from the old or the new location, never both, and
// never missing with the caller's bytes touched.
//
// decision: the read is answered from wherever its records are by the time it
// reads, so the check asserts only that it returns, that its bytes are the
// written ones, and that its sentinel stayed where the journal served nothing.
// A journal that holds reclamation back for the whole read passes too, with
// the read served from the old location.
func TestRFC1A_RepackUnderRead(t *testing.T) {
	f := journaltest.New(t)

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		p := distinct(k, 3<<20)
		v := write(t, h, id, 0, p)
		mark(t, h, id, ext(0, int64(len(p))), v)
		syncFile(t, h, id)

		var e journal.IndexEntry
		for _, en := range entries(t, f, id) {
			s, _ := segment(f, en.Location.Segment)
			if en.Kind == journal.EntryHeld && s.Sealed && s.HeldBytes == en.Extent.Len && s.RetainedBytes == 0 {
				e = en
				break
			}
		}
		if e.Kind == 0 {
			t.Fatalf("share %d: no sealed segment holds a piece of the 3 MiB write and nothing else", k)
		}
		name := segmentName(e.Location.Segment)

		// Every hold is in place before anything runs, then the user read is
		// the first to reach one, so the pass cannot slip past the choreography.
		// The read's hold stops only the first matching call, so once it has
		// taken the user's read the pass's own read of its source runs to the
		// unlink; the unlink and rename holds stop the pass between its copy
		// and its reclamation of the source.
		held := f.Seam.HoldFirst(t, "read*", name)
		unlink := f.Seam.Hold(t, journaltest.OpRemove, name)
		rename := f.Seam.Hold(t, journaltest.OpRename, name)
		type result struct {
			r   readOut
			err error
		}
		done := make(chan result, 1)
		go func() {
			r, err := readErr(t.Context(), h, id, e.Extent.Off, e.Extent.Len)
			done <- result{r, err}
		}()
		select {
		case res := <-done:
			stub(t, "ReadAt", res.err)
			t.Fatalf("share %d: the read returned (%v) without reading its segment", k, res.err)
		case <-held.Reached():
		}

		repacked := make(chan error, 1)
		go func() {
			_, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{e.Location.Segment}})
			repacked <- err
		}()

		// A bounded window decides what the journal does with a read parked
		// at storage while its segment's reclamation runs. An unlink or a
		// rename arriving within it is a design that reclaims behind the
		// read without taking the segment exclusively, and the read is
		// released with the reclamation already at its hold. Neither
		// arriving is a design that takes the segment exclusively, which
		// under the reader guard parks the reclamation behind the read's
		// hold, so after the window the read is released first and the two
		// holds after it, in that order. Both answers complete the repack
		// and serve the read, and the read is judged only by what it
		// returns.
		var res result
		select {
		case <-unlink.Reached():
			held.Release()
		case <-rename.Reached():
			held.Release()
		case err := <-repacked:
			stub(t, "Repack", err)
			t.Fatalf("share %d: the repack returned (%v) with its source still held and read, without unlinking or renaming it", k, err)
		case <-time.After(200 * time.Millisecond):
			held.Release()
		}
		select {
		case res = <-done:
		case <-time.After(backgroundWait):
			t.Fatalf("share %d: the read under the repack of its segment did not return within %v", k, backgroundWait)
		}
		// The unlink and rename holds are released once the read has
		// returned. In the exclusive branch the reclamation spent the
		// window parked behind the read's hold, so releasing beside it
		// would race the guard again; in the free branch a call of the
		// reclamation may already sit at one of the two holds, and the same
		// release lets it go. Either way the pass can only complete once
		// the read has, which is the ordering the row asks the read to
		// survive.
		unlink.Release()
		rename.Release()
		select {
		case err := <-repacked:
			stub(t, "Repack", err)
			if err != nil {
				t.Fatalf("share %d: the repack under the read: %v", k, err)
			}
		case <-time.After(backgroundWait):
			t.Fatalf("share %d: the repack under the read did not return within %v", k, backgroundWait)
		}
		if !retired(f, e.Location.Segment) {
			t.Fatalf("share %d: the repacked source segment still owns what it held", k)
		}

		stub(t, "ReadAt", res.err)
		if res.err != nil {
			t.Fatalf("share %d: the read under the repack of its segment: %v", k, res.err)
		}
		if len(res.r.missing) == 0 {
			wantServed(t, res.r, p[e.Extent.Off:e.Extent.Off+e.Extent.Len], "a read under the repack of its segment's last extent")
		} else {
			wantMissing(t, res.r, e.Extent.Off, []journal.Extent{e.Extent}, "a read under the repack of its segment's last extent")
		}
	}
}

// TestRFC1A_RemovalOutlivesItsSegment checks that a delete marker is not
// dropped by the reclamation of the segments around it: the file stays gone
// across a crash once every segment holding it, the one carrying the delete
// and the sealed ones carrying its old bytes, has been repacked. The settled
// repeat reclaims both segments under a drop that reports the first pass's
// directory syncs as success without committing them — and on this seam the
// second pass's own successful sync commits the waiting log with them, so
// the crash runs with the old bytes durably gone; the leg pins only that the
// file still reads as gone there. The settled marker's drop gate is not
// checked: a decision at the leg names why this seam cannot represent an
// unlink the journal holds a warrant for and the disk does not, and the
// trace row names the issue tracking a seam that can.
func TestRFC1A_RemovalOutlivesItsSegment(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		old := distinct(k, recordCap/2)
		v0 := write(t, h, id, 0, old)
		// The write's own version is the only one its extent holds, so the
		// mark names exactly it; a mark at no version sets no bit and the
		// release below refuses it.
		mark(t, h, id, ext(0, recordCap/2), v0)
		oldSeg := heldEntry(t, f, id).Location.Segment
		release(t, h, id, ext(0, recordCap/2))
		// The segment of released old bytes is sealed on its own, so the
		// delete and the overwrite beside it land in another segment and no
		// pass reclaims marker and old bytes together.
		sealNow(t, f, oldSeg)
		write(t, h, id, 0, distinct(20+k, 256<<10))
		dv, err := h.Delete(t.Context(), id)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("share %d: delete: %v", k, err)
		}
		syncFile(t, h, id)

		// The leg with the marker unsettled runs plain, every unlink durable:
		// reclaim until the segment holding the delete is reclaimed too,
		// crash, and the file stays gone.
		repackAll(t, f, journal.RepackScope{})
		f.Crash()

		h = f.Share(k)
		var deleted bool
		for _, c := range since(t, h, id, journal.Version{}) {
			deleted = deleted || c.Kind == journal.ChangeDelete && c.Version == dv
		}
		if !deleted {
			t.Fatalf("share %d: Since no longer yields the delete marker at %v once its segment was repacked", k, dv)
		}
		r := read(t, h, id, 0, 256<<10)
		wantMissing(t, r, 0, []journal.Extent{ext(0, 256<<10)}, "the deleted file once the segments around its marker were repacked")

		// The settled repeat, with fresh old bytes under it: the deleted
		// file is written and deleted again with its delete settled, and
		// reclamation runs over both the old bytes' segment and the delete's
		// own. The leg checks one thing only — that the file still reads as
		// gone after a crash on the far side of both passes — and checks
		// nothing about the moment the delete's marker may have been
		// dropped, which the decision marker below states.
		//
		// decision: the settled marker's drop gate — the unlinks it counted
		// on durable before the marker itself is dropped — is not checked
		// here, and no assertion on the settled marker after the crash can
		// be, on this seam. Leaving an issued unlink unflushed takes a
		// directory sync that reports success and commits nothing, and a
		// returned success is the journal's whole warrant for durability: a
		// correct journal drops the settled marker with that warrant in
		// hand, so asserting the crash still yields the settled marker
		// fails a journal the rules call correct. The other route — parking
		// the old unlink and letting the delete's reclamation run beside
		// it — fails sooner: a parked call parks the journal's own
		// goroutine, and a crash waits for the journal's calls in flight, so
		// reclamation cannot reach the delete's segment while the old unlink
		// is held; nor can the marker be dropped there by any design, wrong
		// gate or not, while the old segment's records still hold the
		// overlap that pins the marker live. Overturned when the seam can
		// end a process with a call in flight that never returned, so the
		// journal holds no warrant for the unlink the crash reopens beside.
		h = f.Share(k)
		h.Settle(id, dv)
		over := distinct(50+k, recordCap/2)
		v2 := write(t, h, id, 0, over)
		// The settled leg's old bytes, fresh on disk: written, marked and
		// released, their segment sealed on its own so the delete beside
		// them lands in another one and the two passes below reclaim one
		// segment each.
		mark(t, h, id, ext(0, recordCap/2), v2)
		overSeg := heldEntry(t, f, id).Location.Segment
		release(t, h, id, ext(0, recordCap/2))
		sealNow(t, f, overSeg)
		write(t, h, id, 0, distinct(60+k, 256<<10))
		deleteSeg := heldEntry(t, f, id).Location.Segment
		dv2, err := h.Delete(t.Context(), id)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("share %d: the settled leg's delete: %v", k, err)
		}
		syncFile(t, h, id)
		h.Settle(id, dv2)
		// The first pass reclaims the old bytes' segment with its directory
		// sync reported to the journal and made durable by nothing: the seam
		// keeps the removal waiting in its directory log. The fault is
		// cleared before the second pass, whose own directory syncs succeed
		// — and a success commits the whole waiting log, the old segment's
		// removal included — so on this seam the crash finds the old bytes'
		// segment durably gone, never restored, whatever the journal did
		// with the settled delete in the pass over its segment.
		f.Drop(journaltest.OpSyncDir, "*")
		repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{overSeg}})
		f.Seam.SetFault(nil)
		repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{deleteSeg}})
		f.Crash()

		// What the leg pins: after both passes and the crash, no extent of
		// the deleted file reads as content. The crash cannot have brought
		// anything back on this seam — the old segment's removal was
		// committed by the second pass's own syncs — so the read fails only
		// a journal whose recovery or passes let content through: the two
		// writes are gone by their release and their delete, and neither
		// marker the passes may have dropped or carried can turn bytes back
		// on. What the settled delete's own marker came to — dropped with
		// the warrant its unlinks' durability gave, or carried forward — is
		// not asserted, per the decision above: both are states the rules
		// allow a correct journal, and only a seam that can crash between a
		// call and its returned success can tell them from a wrong one's.
		h = f.Share(k)
		r = read(t, h, id, 0, recordCap/2)
		wantMissing(t, r, 0, []journal.Extent{ext(0, recordCap/2)},
			"the settled deleted file after both reclaims and the crash")
	}
}

// TestRFC1A_RemovalRecordsCarriedForward checks that repacks carry removal
// markers forward with the records they cover, and drop them only when their
// version decides: each removal kind still trims after every segment holding
// the removed records but the oldest was repacked away, the records still
// live in that oldest segment, and a truncate whose own marker is repacked
// with its segment is still yielded by Since after a crash.
func TestRFC1A_RemovalRecordsCarriedForward(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const size = 256 << 10

	names := []string{"truncate", "delete", "deallocate"}
	for i, what := range names {
		k := i % 2
		h, id := f.Share(k), journaltest.File(i)
		p := distinct(i, int(recordCap+size))
		v := write(t, h, id, 0, p)
		mark(t, h, id, ext(0, int64(len(p))), v)
		syncFile(t, h, id)

		// A spans at least two segments; the oldest holds its head. Every
		// other segment holding A is repacked with the marker over it. The
		// segments are collected ascending, so holds[0] is the oldest and
		// holds[1:] names every other one however the entries were yielded.
		var holds []journal.SegmentID
		for _, e := range entries(t, f, id) {
			if e.Kind == journal.EntryHeld && !slices.Contains(holds, e.Location.Segment) {
				holds = append(holds, e.Location.Segment)
			}
		}
		slices.Sort(holds)
		if len(holds) < 2 {
			t.Fatalf("%s: A spans %d segment(s), want at least two", what, len(holds))
		}

		var err error
		switch what {
		case "truncate":
			_, err = h.Truncate(t.Context(), id, 0)
		case "delete":
			_, err = h.Delete(t.Context(), id)
		case "deallocate":
			_, err = h.Deallocate(t.Context(), id, ext(0, int64(len(p))))
		}
		stub(t, what, err)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		syncFile(t, h, id)

		repackAll(t, f, journal.RepackScope{Segments: holds[1:]})
		f.Crash()
		r := read(t, f.Share(k), id, 0, int64(len(p)))
		wantMissing(t, r, 0, []journal.Extent{ext(0, int64(len(p)))}, "the "+what+"d extent once every holding segment but the oldest was repacked")

		if what == "truncate" {
			// A marker unsettled, repacked with its own segment, is still
			// what Since yields. The file is a fresh one whose older records
			// are all gone before its truncate: they are released, and every
			// segment holding them is repacked away, so the marker is the
			// last record the file has anywhere and a journal that keeps a
			// marker only while it covers a record still has nothing else to
			// pin it on. The truncate's own segment is named by a probe
			// write made just after it, to the same file beyond the
			// truncate's new size — whichever stream the file maps to, the
			// probe's record and the truncate's marker were appended to the
			// same one — with its own segment asserted retired once the
			// pass over it returns.
			h := f.Share(k)
			fresh := journaltest.File(30 + i)
			fv := write(t, h, fresh, 0, distinct(31+i, size))
			// The fresh write's own version is all its extent holds, so the
			// mark names exactly it.
			mark(t, h, fresh, ext(0, size), fv)
			release(t, h, fresh, ext(0, size))
			syncFile(t, h, fresh)
			repackAll(t, f, journal.RepackScope{})
			if len(entries(t, f, fresh)) != 0 {
				t.Fatalf("%s: the fresh file's records survive their repack: %+v", what, entries(t, f, fresh))
			}

			v2, err := h.Truncate(t.Context(), fresh, 0)
			stub(t, "Truncate", err)
			if err != nil {
				t.Fatalf("the fresh file's truncate: %v", err)
			}
			syncFile(t, h, fresh)
			write(t, h, fresh, size, distinct(41+i, 4096))
			scope := sourceSegment(t, f, fresh)
			repackAll(t, f, scope)
			if !retired(f, scope.Segments[0]) {
				t.Fatalf("%s: the truncate's own segment still owns what it held after the pass over it", what)
			}
			f.Crash()
			var yielded bool
			for _, c := range since(t, f.Share(k), fresh, journal.Version{}) {
				yielded = yielded || c.Kind == journal.ChangeTruncate && c.Version == v2
			}
			if !yielded {
				t.Fatalf("Since no longer yields the truncate marker after its own segment was repacked")
			}
		}
	}
}

// TestRFC1A_CopyLosesToNewerWrite checks that a copy a pass would make loses
// to a write that supersedes its source while the pass is stopped: with the
// overwrite synced before the pass goes on, the source is dead, no copy of
// the superseded record is appended and no index entry of its version points
// outside it, and the newer bytes are served before and after a reopen. With
// the overwrite unsynced, the copy is appended as retained, the source
// unlinked, and failing the overwrite's window serves the retained bytes,
// never missing and never both versions at once.
//
// decision: the seam cannot stop a pass between its read of a record and its
// decision to append — its read and its append are one unbroken stretch of
// journal code, and no call between them reaches storage. So the synced leg
// stops the pass at the source read itself, before any byte of the record is
// in its hands: the overwrite and its Sync land while the read is parked, and
// the pass goes on to read a source its file's records no longer need. The
// ceiling: a pass that had already read the record and was stopped inside its
// own append would also have to skip the copy once the overwrite is synced,
// and that ordering this check cannot force. Overturned if the pass reports
// its decision points to the seam, when a hold can stop it after its read and
// before its append.
//
// decision: the unsynced leg parks the pass inside the copy's own write,
// after the pass has decided to append, so the overwrite must land while that
// write is parked, on a goroutine, so a journal that serialises appends on
// its stream does not hang the run behind it.
func TestRFC1A_CopyLosesToNewerWrite(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const size = 512 << 10

	// overwriteAt starts a goroutine writing p over the extent, so the write
	// does not hang behind a pass parked inside its own append, and returns
	// once the seam has counted the overwrite's own write on a segment file.
	// The count is read before the goroutine starts, so the first rise past
	// it is the overwrite's append, not the pass's parked copy — a call held
	// at a hold is counted already, which is how the parked copy is told from
	// the overwrite arriving behind it. The wait is bounded and fails the
	// check: with nothing observed the pass would be released before the
	// overwrite is in flight, and the ordering the check exists to force
	// would be left to the scheduler.
	overwriteAt := func(h *journal.Handle, id journal.FileID, p []byte) <-chan error {
		before := segmentWrites(t, f)
		done := make(chan error, 1)
		go func() {
			_, err := tryWrite(t, h, id, 0, p)
			done <- err
		}()
		deadline := time.Now().Add(backgroundWait)
		for segmentWrites(t, f) <= before {
			if time.Now().After(deadline) {
				t.Fatalf("the overwrite did not reach a segment file within %v, so nothing ordered it before the released pass", backgroundWait)
			}
			time.Sleep(time.Millisecond)
		}
		return done
	}
	joinWrite := func(what string, done <-chan error) {
		select {
		case err := <-done:
			stub(t, what, err)
			if err != nil {
				t.Fatalf("%s behind the parked pass: %v", what, err)
			}
		case <-time.After(backgroundWait):
			t.Fatalf("%s did not return behind the parked pass within %v", what, backgroundWait)
		}
	}

	// noCopyInstalled reports whether no index entry of file id at version v
	// points outside the segment the record was read from.
	noCopyInstalled := func(f *journaltest.Fixture, id journal.FileID, v journal.Version, from journal.SegmentID) bool {
		for _, e := range entries(t, f, id) {
			if e.Version == v && e.Kind != journal.EntryMarker && e.Location.Segment != from {
				return false
			}
		}
		return true
	}

	for k := range 2 {
		// The re-append counter is read per share: share 1's leg must judge
		// its own Sync against its own baseline, not the total share 0's
		// leg already raised past.
		reappendBase := f.J.Stats().Counters.SyncReappendedBytes
		h, id := f.Share(k), journaltest.File(k)
		vOld := write(t, h, id, 0, distinct(k, size))
		syncFile(t, h, id)
		seg := heldEntry(t, f, id).Location.Segment
		// The source is sealed idle, so the stream has rolled past it before
		// the pass runs: the overwrite below appends elsewhere, and every
		// record the pass could retire out of the source belongs to the one
		// superseded write.
		sealNow(t, f, seg)

		// The pass is stopped at its first read of the source's segment
		// file, and the overwrite and its Sync land while that read waits.
		reading := f.Seam.HoldFirst(t, "read*", segmentName(seg))
		repacked := make(chan error, 1)
		go func() {
			_, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{seg}})
			repacked <- err
		}()
		select {
		case <-reading.Reached():
		case err := <-repacked:
			stub(t, "Repack", err)
			t.Fatalf("share %d: the repack returned (%v) before reading its source", k, err)
		}

		newer := distinct(20+k, size)
		overwritten := make(chan error, 1)
		go func() {
			_, err := tryWrite(t, h, id, 0, newer)
			overwritten <- err
		}()
		joinWrite("the overwrite", overwritten)
		syncFile(t, h, id)

		// The pass goes on past a source its file no longer needs: between
		// the released read and its return nothing lands on a segment file,
		// so no copy of the superseded record was appended.
		before := segmentWrites(t, f)
		reading.Release()
		select {
		case err := <-repacked:
			stub(t, "Repack", err)
			if err != nil {
				t.Fatalf("share %d: the repack past the synced overwrite: %v", k, err)
			}
		case <-time.After(backgroundWait):
			t.Fatalf("share %d: the repack kept running %v past its released read", k, backgroundWait)
		}
		if got := segmentWrites(t, f); got != before {
			t.Fatalf("share %d: %d record(s) were appended once the pass's source was superseded and synced, want no copy of it", k, got-before)
		}

		got, missing := readBack(t, h, id, 0, size)
		if len(missing) != 0 || !bytes.Equal(got, newer) {
			t.Fatalf("share %d: the newer write reads missing %+v or other bytes once the pass let go", k, missing)
		}
		eventually(t, fmt.Sprintf("share %d: the repacked source segment was not retired", k), func() bool {
			return !listed(t, f, segmentName(seg))
		})
		if !noCopyInstalled(f, id, vOld, seg) {
			t.Fatalf("share %d: an index entry of the superseded version %v points outside its source", k, vOld)
		}
		f.Crash()
		got, missing = readBack(t, f.Share(k), id, 0, size)
		if len(missing) != 0 || !bytes.Equal(got, newer) {
			t.Fatalf("share %d: the newer write reads missing %+v or other bytes after a crash", k, missing)
		}

		// Round 2: the overwrite is unsynced, so the copy is appended as
		// retained, and the armed window's resolution leaves the extent with
		// a backing the pass put there — the retained copy's, or the
		// overwrite's own bytes once the copy's sync made them durable.
		//
		// decision: failing the window below either fails the Sync or the
		// journal resolves it by re-appending the overwritten write from a
		// copy it kept, and the check asserts whichever state that answer
		// implies, as the other window checks do. A journal that always
		// re-appends leaves the retained-copy half unexercised here.
		h = f.Share(k)
		synced := distinct(30+k, size)
		vSynced := write(t, h, id, 0, synced)
		syncFile(t, h, id)
		seg = heldEntry(t, f, id).Location.Segment
		// The source is sealed idle before the pass runs, as the first leg
		// seals its own: a pass that declines an active segment would return
		// without appending, and a journal that appended into the source
		// could land the unsynced overwrite below in it, ending the check
		// over a segment that is its own copy's target. The overwrite below
		// rolls into the next segment.
		sealNow(t, f, seg)
		copying := f.Seam.HoldFirst(t, journaltest.OpWrite, "*.seg")
		repacked = make(chan error, 1)
		go func() {
			_, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{seg}})
			repacked <- err
		}()
		select {
		case <-copying.Reached():
		case err := <-repacked:
			stub(t, "Repack", err)
			t.Fatalf("share %d: the second repack returned (%v) before appending its first copy", k, err)
		}

		unsynced := distinct(40+k, size)
		unsyncedWritten := overwriteAt(h, id, unsynced)
		copying.Release()
		joinWrite("the unsynced overwrite", unsyncedWritten)
		select {
		case err := <-repacked:
			stub(t, "Repack", err)
			if err != nil {
				t.Fatalf("share %d: the second repack past its unsynced overwrite: %v", k, err)
			}
		case <-time.After(backgroundWait):
			t.Fatalf("share %d: the second repack kept running %v past its released copy", k, backgroundWait)
		}

		seg2 := segmentOf(t, f, id, 0)
		// What the copies' own sync already made durable decides the leg,
		// and only the overwrite's own payload in segment seg2's durable
		// image proves it durable: the seam's per-name durable byte totals
		// are running counters over every byte a file's syncs ever made
		// durable — a spare's zero-fill alone has already banked its whole
		// cap — so a total never says which write a sync saved. RFC 1 §4.3
		// keeps a superseded record only until "the record that superseded
		// it is synced": when the unsynced overwrite shared the copies'
		// segment, the sync that saved the copies saved it too, so the
		// journal may drop the old copy already, and Sync has nothing left
		// to save — §3.1 returns it once the file's writes are durable, a
		// §5.3 failure is reported only to "each file that had a write in"
		// the window, and the re-append counter may stay where it was. The
		// payload is unique: the gate held the pass at its first write of
		// its first copy, and every leg before wrote other bytes.
		overwriteDurable := false
		if img, err := f.Seam.Durable(segmentName(seg2)); err == nil {
			overwriteDurable = len(img) >= len(unsynced) && bytes.Contains(img, unsynced)
		}

		// The copy is installed as retained outside the source, and the
		// source is unlinked — asserted only while the overwrite is still
		// unsynced: once its sync saved it, §4.3 lets the journal drop the
		// old copy, and Sync — §3.1 — has nothing left to save, so both a
		// pass that installed a retained copy and a sync that merely
		// answered its file are correct answers. While it is unsynced the
		// copy is the one thing keeping the extent servable through the
		// window below, so a pass that skipped its just-retained source
		// leaves the leg nothing to serve, or nothing retired.
		if !overwriteDurable {
			var retainedCopy bool
			for _, e := range entries(t, f, id) {
				retainedCopy = retainedCopy || e.Kind == journal.EntryRetained && e.Version == vSynced && e.Location.Segment != seg
			}
			if !retainedCopy {
				t.Fatalf("share %d: the copy of the version the unsynced overwrite superseded is not installed as retained: %+v", k, entries(t, f, id))
			}
			if !retired(f, seg) {
				t.Fatalf("share %d: the repacked source segment is still owned once its copy is installed", k)
			}
		}
		failWindow(t, f, seg2)
		syncErr := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", syncErr)
		failed := errors.Is(syncErr, journal.ErrSyncFailed)
		if syncErr != nil && !failed {
			t.Fatalf("share %d: the Sync over the failed window returned %v, want a SyncError or a re-append", k, syncErr)
		}
		if syncErr == nil && !overwriteDurable {
			if got := f.J.Stats().Counters.SyncReappendedBytes; got <= reappendBase {
				t.Fatalf("share %d: a Sync that returned success over a failed window re-appended no bytes", k)
			}
		}

		// decision: the overlap the row's retained-copy half asks for cannot
		// be forced, only answered, and the overlap is the failed window's
		// alone. The copy is appended under the file's own serialized
		// append, behind the unsynced overwrite, so on one stream the two
		// share the active segment more often than not, and the sync that
		// made the copies durable — the copies before the unlink, as the
		// ordering rules have it — covered the overwrite with them. When the
		// overwrite was already durable by then (the durable read above),
		// §4.3 lets the journal drop the old copy once the record that
		// superseded it is synced, §3.1 has Sync return once the file's
		// writes are durable, and §5.3 reports a failed window only to a
		// file with a write in it — with nothing left to save, the Sync
		// returns nil without writing again, and that answer is every bit a
		// correct one, so the re-append assertion is gated on the read, and
		// the check below demands the overwrite's own durable bytes only
		// after the Sync returned success — a Sync that answered the window
		// with a loss report may also take the extent back to the retained
		// copy, even when the read above had the overwrite durable, so the
		// answer to a failed window is judged on shape alone. Only a roll
		// that put the overwrite in its own segment leaves it unsynced
		// past the pass, and failing its window is what makes the
		// retained copy serve —
		// the row's stated outcome, but one the check cannot schedule. So
		// only the read past a Sync that failed accepts either backing, and
		// asserts only what a pass that skipped the just-retained source
		// cannot provide: some backing at all, the same one before and after
		// the crash. A Sync that returned success acknowledged the overwrite
		// it was handed — and a re-append that acknowledges it still lets
		// the retained copy rank above it would acknowledge nothing, so that
		// read must serve the overwrite whole, or it is the silent loss the
		// check here exists for. Overturned if the placement of a copy
		// becomes observable enough to schedule the roll.
		served, servingNew := func() ([]byte, bool) {
			got, missing := readBack(t, h, id, 0, size)
			if len(missing) != 0 {
				return nil, false
			}
			return got, bytes.Equal(got, unsynced)
		}()
		if syncErr == nil && (served == nil || !servingNew) {
			t.Fatalf("share %d: after a Sync that returned success over the failed window, the extent reads missing or the old write's bytes: it acknowledged the overwrite — made durable already when the read above said so — and nothing can have taken that back", k)
		}
		if served == nil {
			t.Fatalf("share %d: after the armed window, the extent reads missing: the pass has left it nothing — skipping a source that only just became retained loses the write when the window fails", k)
		}
		if !servingNew && !bytes.Equal(served, synced) {
			t.Fatalf("share %d: after the armed window, the extent serves other bytes than either write's", k)
		}
		f.Crash()
		got, missing = readBack(t, f.Share(k), id, 0, size)
		if len(missing) != 0 || !bytes.Equal(got, served) {
			t.Fatalf("share %d: after a crash the extent reads missing %+v or other bytes than the %d-byte write it served before, which nothing can have taken from it", k, missing, len(served))
		}
	}
}

// TestRFC1A_LossRaisesLossGeneration checks that the loss generation rises
// over what a client must act on and nothing else: a corrupt dirty extent
// raises it, a corrupt offloaded extent and a stale extent are dropped
// without moving it, a window the journal fails raises it before the failing
// Sync returns, and a window resolved by re-appending does not move it.
//
// decision: the offloaded and stale legs drop their extent through the
// journal's own read, and the window legs land on whichever resolution the
// journal chooses. Reads of the failed segment fail too, so a resolution can
// come only from a copy the journal kept in its own memory at write time:
// a journal that always keeps one re-appends, which raises nothing, so the
// window legs' rise is asserted only on the journal's own failed path — and
// an always-re-appending journal leaves the rise half of this check
// unexercised. Overturned if the journal's resolution becomes settable.
func TestRFC1A_LossRaisesLossGeneration(t *testing.T) {
	f := journaltest.New(t)
	const size = 4096
	e := ext(0, size)

	// lostExtents returns the loss events naming file id over e.
	lostExtents := func(reason journal.LossReason, id journal.FileID) int {
		var n int
		for _, l := range f.J.Stats().Losses {
			if l.File == id && overlap(l.Extent, e) > 0 && l.Reason == reason {
				n++
			}
		}
		return n
	}

	for k := range 2 {
		h := f.Share(k)

		// A corrupt dirty extent raises the generation by one.
		dirtyID := journaltest.File(60 + 2*k)
		pd := pattern(fmt.Sprint("dirty", k), size)
		write(t, h, dirtyID, 0, pd)
		syncFile(t, h, dirtyID)
		corrupt(t, f, dirtyID, pd)
		before := f.J.Stats().LossGeneration
		_, _, _, _, err := h.ReadAt(t.Context(), dirtyID, 0, make([]byte, size))
		stub(t, "ReadAt", err)
		if after := f.J.Stats().LossGeneration; after != before+1 {
			t.Fatalf("share %d: dropping a corrupt dirty extent moved the loss generation from %d to %d, want %d", k, before, after, before+1)
		}

		// A corrupt offloaded extent is dropped without raising it.
		offID := journaltest.File(61 + 2*k)
		po := pattern(fmt.Sprint("offloaded", k), size)
		vo := write(t, h, offID, 0, po)
		mark(t, h, offID, e, vo)
		syncFile(t, h, offID)
		corrupt(t, f, offID, po)
		before = f.J.Stats().LossGeneration
		_, _, _, _, err = h.ReadAt(t.Context(), offID, 0, make([]byte, size))
		stub(t, "ReadAt", err)
		if after := f.J.Stats().LossGeneration; after != before {
			t.Fatalf("share %d: dropping a corrupt offloaded extent moved the loss generation from %d to %d, want no move", k, before, after)
		}
		if got := lostExtents(journal.LossCorrupt, offID); got != 1 {
			t.Fatalf("share %d: %d LossCorrupt events name the corrupt offloaded extent, want the one drop: %+v", k, got, f.J.Stats().Losses)
		}
		r := read(t, h, offID, 0, size)
		wantMissing(t, r, 0, []journal.Extent{e}, "the corrupt offloaded extent after its drop")

		// A stale extent is dropped without raising it.
		staleID := journaltest.File(62 + 2*k)
		write(t, h, staleID, 0, pattern(fmt.Sprint("stale", k), size))
		newer, err := f.J.Draw()
		stub(t, "Draw", err)
		if err != nil {
			t.Fatal(err)
		}
		mark(t, h, staleID, e, newer)
		before = f.J.Stats().LossGeneration
		read(t, h, staleID, 0, size)
		if after := f.J.Stats().LossGeneration; after != before {
			t.Fatalf("share %d: dropping a stale extent moved the loss generation from %d to %d, want no move", k, before, after)
		}
		if got := lostExtents(journal.LossStale, staleID); got != 1 {
			t.Fatalf("share %d: %d LossStale events name the stale extent, want the one drop: %+v", k, got, f.J.Stats().Losses)
		}
		r = read(t, h, staleID, 0, size)
		wantMissing(t, r, 0, []journal.Extent{e}, "the stale extent after its drop")

		// A window the journal fails raises the generation before the Sync
		// returns, whichever resolution it chose: a re-append from a copy it
		// kept at write time raises nothing, a failed window raises it, and
		// the Sync's own answer tells the two apart.
		windowID := journaltest.File(63 + 2*k)
		write(t, h, windowID, 0, pattern(fmt.Sprint("window", k), size))
		seg := segmentOf(t, f, windowID, 0)
		before = f.J.Stats().LossGeneration
		reappended := f.J.Stats().Counters.SyncReappendedBytes
		failWindow(t, f, seg)
		syncErr := h.Sync(t.Context(), windowID)
		f.Seam.SetFault(nil)
		stub(t, "Sync", syncErr)
		if err := syncErr; err != nil {
			if !errors.Is(err, journal.ErrSyncFailed) {
				t.Fatalf("share %d: the Sync over a window whose reads fail returned %v, want a SyncError or a re-append", k, err)
			}
			if after := f.J.Stats().LossGeneration; after <= before {
				t.Fatalf("share %d: a Sync whose window failed returned before the loss generation rose: %d, want above %d", k, after, before)
			}
		} else if after := f.J.Stats().LossGeneration; after != before {
			t.Fatalf("share %d: a Sync resolved by re-appending moved the loss generation from %d to %d, want no move", k, before, after)
		} else if got := f.J.Stats().Counters.SyncReappendedBytes; got <= reappended {
			t.Fatalf("share %d: a Sync that failed its window returned neither an error nor any re-appended bytes", k)
		}

		// A window the journal resolves by re-appending from what it kept
		// raises nothing: once resolved, whether a re-append follows its one
		// failure is the journal's own choice, and a failed one is the
		// previous leg's answer.
		reappID := journaltest.File(64 + 2*k)
		write(t, h, reappID, 0, pattern(fmt.Sprint("reappend", k), size))
		rseg := segmentOf(t, f, reappID, 0)
		f.FailNext(journaltest.OpSync, segmentName(rseg), 1)
		before = f.J.Stats().LossGeneration
		reappended = f.J.Stats().Counters.SyncReappendedBytes
		err = h.Sync(t.Context(), reappID)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		if err != nil {
			if !errors.Is(err, journal.ErrSyncFailed) {
				t.Fatalf("share %d: the Sync over the failed one returned %v, want a SyncError or a re-append", k, err)
			}
		} else if after := f.J.Stats().LossGeneration; after != before {
			t.Fatalf("share %d: a Sync resolved by re-appending moved the loss generation from %d to %d, want no move", k, before, after)
		} else if got := f.J.Stats().Counters.SyncReappendedBytes; got <= reappended {
			t.Fatalf("share %d: a Sync that returned success over a failed sync re-appended no bytes", k)
		}
	}
}

// TestRFC1A_RecycledSegmentKeepsNewRecords checks that a segment sealed full,
// retired and recycled rather than unlinked serves only its new records after
// a clean close, a crash and a reopen, and that a failed append into a
// recycled segment, with an unsynced record of the file sitting beside the
// failure, makes it a repack candidate only once that append's window is
// resolved — resolved to a Sync that answers, the segment retired by the next
// pass, unlinked rather than recycled, after the pass carried the records
// beside the failure forward; resolved by a failure, the segment declines
// still.
//
// decision: whether a full retired segment is recycled or unlinked is the
// journal's alone, made against the spare pool at retirement, so the check
// does not fault an unlink into a failure: it fills and retires segments,
// bounded, until a retirement recycles one. A recycle takes the segment's
// file to a name nothing has issued, so the recycled segment is identified
// by the new name that appeared beside the renamed-away one. Overturned if
// the journal reports its retirements, when the recycle can be reached
// without the loop.
func TestRFC1A_RecycledSegmentKeepsNewRecords(t *testing.T) {
	// The cap leaves room for a small writer's records beside a full
	// segment's, and the spare pool starts low: a journal recycles a
	// retired full segment only while its pool is short of the target, so
	// one spare per stream is the pool the check's first retirements meet,
	// each filling it back toward the target.
	f := journaltest.NewWithSegmentCap(t, 2*recordCap, func(s *journal.Settings) {
		s.SpareTarget = 1
	})
	const size = 128 << 10
	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)

		// Full segments, one retirement at a time, until one is recycled: a
		// full retired segment is recycled only while the spare pool sits
		// under its target, so a single retirement may be an unlink on a
		// journal whose pool is full. Each round fills a segment to its cap,
		// releases every extent it holds and syncs, so old bytes are durable
		// before the pass retires the segment, and the pool the next round
		// retires under is one segment short of where the last round left it.
		//
		// A retire is judged by the file names the pass leaves: a recycle
		// renames the retired segment's file to a segment id nothing has
		// issued yet, so no record on disk ever carries the id a recycle
		// takes, and the recycled segment is the name that appeared — a
		// rename under the retired name, plus a new .seg beside, with writes
		// on it once the stream rolls into it.
		var recycledName string
		off := int64(0)
		for round := 0; ; round++ {
			if round > 32 {
				t.Fatalf("share %d: no retired full segment was recycled over %d retirements, so no write can land in a recycled segment", k, round)
			}
			write(t, h, id, off, pattern(fmt.Sprint("old", k, off), size))
			seg := heldEntry(t, f, id).Location.Segment
			for o := off + size; ; o += size {
				if s, _ := segment(f, seg); s.Sealed {
					break
				}
				if o > off+4*recordCap {
					t.Fatalf("share %d: segment %d is not sealed after %d bytes of its file", k, seg, o-off)
				}
				write(t, h, id, o, pattern(fmt.Sprint("old", k, off), size))
			}
			off += 4 * recordCap
			for _, en := range entries(t, f, id) {
				if en.Kind == journal.EntryHeld && en.Location.Segment == seg {
					mark(t, h, id, en.Extent, en.Version)
					release(t, h, id, en.Extent)
				}
			}
			if s, _ := segment(f, seg); s.HeldBytes != 0 || s.RetainedBytes != 0 {
				t.Fatalf("share %d: segment %d still holds %+v after every extent of the file in it was released", k, seg, s)
			}
			if retired(f, seg) {
				t.Fatalf("share %d: segment %d was retired without a pass once its extents were released", k, seg)
			}
			syncFile(t, h, id)
			before := slices.Clone(segmentNames(t, f))
			name := segmentName(seg)
			renames := f.Seam.CallsFor(journaltest.OpRename, name)
			repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{seg}})
			f.Seam.SetFault(nil)
			if f.Seam.CallsFor(journaltest.OpRename, name) <= renames {
				continue // this retirement unlinked; the pool is full again
			}
			gained := slices.DeleteFunc(slices.Clone(segmentNames(t, f)), func(n string) bool {
				return slices.Contains(before, n)
			})
			if len(gained) == 0 {
				t.Fatalf("share %d: the retirement renamed segment %d away but left no new segment file", k, seg)
			}
			// The recycle frees the fresh segment's header and trailer slots
			// of the old life's entries, so a name the journal has since
			// written to names the recycled segment; unlinking a different
			// segment and creating a new one leaves a name no write touches
			// until the stream rolls into it, and renaming a retired segment
			// to a spare's temporary name does not end in .seg at all.
			var marked []string
			for _, n := range gained {
				if f.Seam.CallsFor(journaltest.OpWrite, n) > 0 {
					marked = append(marked, n)
				}
			}
			switch {
			case len(marked) == 1:
				recycledName = marked[0]
			case len(gained) == 1:
				recycledName = gained[0]
			default:
				t.Fatalf("share %d: the retirement of segment %d left no single recycled segment to name: gained %v, written-to %v", k, seg, gained, marked)
			}
			break
		}

		// The recycled segment's new life: the stream rolls into the spare
		// the recycle made, so A, written into it, stops short of the old
		// seal marker — the segment kept its seal marker through the
		// recycle, and A's records sit before it. A survives a clean close,
		// a crash and a reopen, and nothing of the old life is held. The
		// rolls are probed by the byte last written, which is the byte the
		// index holds, and the loop is bounded by what nothing can outlast:
		// the stream cannot pass two fills without serving the spare.
		rolledAt := int64(0)
		for next := off + size; next <= off+2*2*recordCap; next += size {
			write(t, h, id, next, pattern(fmt.Sprint("roll", k, next), size))
			if segmentName(segmentOf(t, f, id, next)) == recycledName {
				rolledAt = next
				break
			}
		}
		if rolledAt == 0 {
			t.Fatalf("share %d: the stream never rolled into the recycled segment %s within %d bytes", k, recycledName, 2*2*recordCap)
		}
		// A: written into the recycled segment, stopping short of the old
		// seal marker its recycle kept.
		fresh := distinct(10+k, size)
		write(t, h, id, rolledAt+size, fresh)
		if at := segmentOf(t, f, id, rolledAt+size); segmentName(at) != recycledName {
			t.Fatalf("share %d: A was written into segment %d, want the recycled segment %s", k, at, recycledName)
		}
		syncFile(t, h, id)
		for _, reopen := range []func(){f.Reopen, func() { f.Crash() }} {
			reopen()
			h = f.Share(k)
			got, missing := readBack(t, h, id, rolledAt+size, size)
			if len(missing) != 0 || !bytes.Equal(got, fresh) {
				t.Fatalf("share %d: the fresh write reads missing %+v or other bytes after the journal was reopened", k, missing)
			}
			rolledSeg := segmentOf(t, f, id, rolledAt+size)
			for _, en := range entries(t, f, id) {
				if en.Kind == journal.EntryHeld && en.Location.Segment == rolledSeg && en.Extent.Off < rolledAt {
					t.Fatalf("share %d: an extent of the recycled segment's old life is held: %+v", k, en)
				}
			}
		}

		// B, and the append made to fail after it, go into the same recycled
		// segment A filled: on a recycled segment a failed append ends the
		// segment — its slot holds the old life's bytes, which the torn-tail
		// rule cannot step over — so the segment's not-a-candidate window and
		// its unlink-only retirement below are the recycled path, not a fresh
		// segment's, where a failed append that left zeros is stepped over.
		// The writes are placed by segmentOf before the fault is armed, and
		// the room the recycled segment has left stops the section's records
		// short of the old cap's seal. C is left unsynced beside B, so the
		// failed append's window holds a record: the pass below runs while
		// that window is unresolved, before any Sync of the file.
		h = f.Share(k)
		next := distinct(20+k, size)
		write(t, h, id, rolledAt+2*size, next)
		seg2 := segmentOf(t, f, id, rolledAt+2*size)
		if name2 := segmentName(seg2); name2 != recycledName {
			t.Fatalf("share %d: B was written into segment %d, want the recycled segment %s", k, seg2, recycledName)
		}
		name2 := segmentName(seg2)
		syncFile(t, h, id)
		tail := distinct(60+k, 4096)
		write(t, h, id, rolledAt+4*size, tail)
		if at := segmentOf(t, f, id, rolledAt+4*size); at != seg2 {
			t.Fatalf("share %d: C was written into segment %d, want the recycled segment %d beside B", k, at, seg2)
		}
		f.FailNext(journaltest.OpWrite, name2, 1)
		if _, err := tryWrite(t, h, id, rolledAt+3*size, distinct(30+k, 10<<10)); err == nil {
			t.Fatalf("share %d: the append the fault was set to fail succeeded", k)
		}
		f.Seam.SetFault(nil)
		got, missing := readBack(t, h, id, rolledAt+2*size, size)
		if len(missing) != 0 || !bytes.Equal(got, next) {
			t.Fatalf("share %d: the write beside the failed append reads missing %+v or other bytes", k, missing)
		}
		if s, ok := segment(f, seg2); !ok || !s.Failed {
			t.Fatalf("share %d: the recycled segment %d does not report its failed append: %+v", k, seg2, s)
		}

		// While the failed append's window is unresolved, no pass selects the
		// segment: it still holds the record beside the failure. A pass that
		// declines it returns at once, which is the right answer, so the pass
		// is joined and judged by what it did, not by how long it took.
		notCandidate := func() {
			t.Helper()
			var freed int64
			err := within(t, f, "a pass over the failed segment", func() (err error) {
				freed, err = f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{seg2}})
				return err
			})
			stub(t, "Repack", err)
			if err != nil {
				t.Fatalf("share %d: the pass over the failed segment %d: %v", k, seg2, err)
			}
			if freed != 0 {
				t.Fatalf("share %d: a pass over the failed segment %d freed %d bytes before its window was resolved", k, seg2, freed)
			}
			if _, ok := segmentInfo(f, seg2); !ok {
				t.Fatalf("share %d: a pass retired the failed segment %d before its window was resolved", k, seg2)
			}
		}
		notCandidate()

		// The file's Sync resolves the failed append's window either way it
		// goes — re-appending C, or failing C out of the index — and either
		// resolution makes the segment a candidate: a pass then carries B and
		// A forward and retires it, unlinked rather than recycled.
		h = f.Share(k)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		if err != nil && !errors.Is(err, journal.ErrSyncFailed) {
			t.Fatalf("share %d: the Sync that resolved the failed append's window returned %v, want success or a SyncError", k, err)
		}
		renamed2 := f.Seam.CallsFor(journaltest.OpRename, name2)
		repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{seg2}})
		if !retired(f, seg2) {
			t.Fatalf("share %d: the failed segment %d is still owned after the pass that followed its window's resolution, which carried its new record forward", k, seg2)
		}
		if n := f.Seam.CallsFor(journaltest.OpRename, name2) - renamed2; n > 0 {
			t.Fatalf("share %d: the failed segment %d was renamed %d time(s) into a spare rather than unlinked", k, seg2, n)
		}
		// B, carried forward by the retiring pass, reads back between the
		// retirement and the crash, through the copy the pass made of it.
		got, missing = readBack(t, h, id, rolledAt+2*size, size)
		if len(missing) != 0 || !bytes.Equal(got, next) {
			t.Fatalf("share %d: B reads missing %+v or other bytes once its failed segment was retired", k, missing)
		}

		f.Crash()
		got, missing = readBack(t, f.Share(k), id, rolledAt+2*size, size)
		if len(missing) != 0 || !bytes.Equal(got, next) {
			t.Fatalf("share %d: the write beside the failed append reads missing %+v or other bytes after a crash", k, missing)
		}
		// A, carried forward by the retiring pass, reads back after the crash
		// through the copy the pass made, not the failed segment.
		gotA, missingA := readBack(t, f.Share(k), id, rolledAt+size, size)
		if len(missingA) != 0 || !bytes.Equal(gotA, fresh) {
			t.Fatalf("share %d: A reads missing %+v or other bytes after the failed segment's retirement and a crash", k, missingA)
		}
	}
}

// TestRFC1A_FailedSyncWindow checks that when a segment's sync fails with the
// unsynced writes lost, a Sync for the writes it held either fails, or
// returns only after they were made durable elsewhere, and the journal's
// state after a crash agrees with whichever the Sync reported.
//
// decision: whether the Sync fails outright or waits for the re-append is the
// journal's to choose; the check asserts only that it did one of the two and
// that a crash does not contradict its answer.
func TestRFC1A_FailedSyncWindow(t *testing.T) {
	f := journaltest.New(t)

	type round struct {
		share  int
		failed bool
		pa, pb []byte
	}
	var rounds []round
	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		pa := distinct(k, 64<<10)
		pb := distinct(20+k, 64<<10)
		write(t, h, id, 0, pa)
		write(t, h, id, 64<<10, pb)
		seg := segmentOf(t, f, id, 0)
		if at := segmentOf(t, f, id, 64<<10); at != seg {
			t.Fatalf("share %d: the second write of the window sits in segment %d, want the first's %d", k, at, seg)
		}
		failWindow(t, f, seg)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		failed := errors.Is(err, journal.ErrSyncFailed)
		if err != nil && !failed {
			t.Fatalf("share %d: the Sync over the failed sync returned %v, want a SyncError or a re-append", k, err)
		}

		// A later sync of the segment, or of whatever now holds the writes,
		// succeeds: the failed window does not poison the stream.
		write(t, h, id, 128<<10, distinct(30+k, 64<<10))
		if err := h.Sync(t.Context(), id); err != nil {
			stub(t, "Sync", err)
			t.Fatalf("share %d: the Sync after the failed window: %v; the stream is wedged", k, err)
		}
		rounds = append(rounds, round{k, failed, pa, pb})
	}

	f.Crash()
	for _, r := range rounds {
		h, id := f.Share(r.share), journaltest.File(r.share)
		for _, c := range []struct {
			off int64
			p   []byte
		}{{0, r.pa}, {64 << 10, r.pb}} {
			got, missing := readBack(t, h, id, c.off, 64<<10)
			if r.failed {
				// The writes are lost, or the re-append of their window made
				// them durable; the Sync's answer decides which, and a
				// correct journal that served the bytes by re-appending
				// failed its Sync.
				continue
			}
			if len(missing) != 0 || !bytes.Equal(got, c.p) {
				t.Fatalf("share %d: a Sync that returned success left the write at %d missing %+v after a crash", r.share, c.off, missing)
			}
		}
	}
}

// TestRFC1A_RepackVerifiesWholeSource checks that a repack verifies the whole
// payload of every record it copies: a record whose released middle rotted is
// dropped as corrupt whole, nothing of it is copied, a loss event names it,
// and both its ends read missing, before and after a crash.
func TestRFC1A_RepackVerifiesWholeSource(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const mib = 1 << 20
	middle := ext(mib/4, mib/2)

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		p := distinct(k, mib)
		v := write(t, h, id, 0, p)
		mark(t, h, id, ext(0, mib), v)
		release(t, h, id, middle)
		syncFile(t, h, id)
		seg := heldEntry(t, f, id).Location.Segment

		// The rot lands in the released middle, where no served byte of the
		// record lives. The pass then drops the record whole: nothing of it
		// is copied, so no index entry of the file points outside its source
		// once the pass is done.
		corruptAt(t, f, id, p, mib/4+mib/2-1)
		repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{seg}})
		for _, e := range entries(t, f, id) {
			if e.Version == v && e.Kind != journal.EntryMarker && e.Location.Segment != seg {
				t.Fatalf("share %d: the repack copied %+v of the record it found corrupt", k, e)
			}
		}

		r := read(t, h, id, 0, mib)
		wantMissing(t, r, 0, []journal.Extent{ext(0, mib)}, "the record whose released middle rotted")

		var lost bool
		for _, l := range f.J.Stats().Losses {
			lost = lost || (l.File == id && l.Reason == journal.LossCorrupt)
		}
		if !lost {
			t.Fatalf("share %d: no loss event names the record the repack found corrupt: %+v", k, f.J.Stats().Losses)
		}

		f.Crash()
		r = read(t, f.Share(k), id, 0, mib)
		wantMissing(t, r, 0, []journal.Extent{ext(0, mib)}, "the record the repack found corrupt, after a crash")
	}
}

// TestRFC1A_RetainedExtentsSurviveRepack checks that a snapshot hold keeps
// superseded content through the repack of its segment: after the repack and
// a crash the next offer still carries the held version in Superseded, and
// it leaves the index only once the offer reports that extent offloaded.
//
// decision: the hold stays on across the offer. Content under a mark is
// offered as superseded until it is offloaded; releasing the hold first
// would let the journal reclaim the retained version before the offer sees
// it, which is not what the check names.
func TestRFC1A_RetainedExtentsSurviveRepack(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const mib = 1 << 20

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		v1 := write(t, h, id, 0, distinct(k, mib))
		v1Seg := heldEntry(t, f, id).Location.Segment

		share := journal.ShareID{byte(k + 1)}
		cut := journal.SnapshotCut(k + 1)
		err := h.Hold(t.Context(), share, cut, map[journal.FileID]journal.Version{id: v1})
		stub(t, "Hold", err)
		if err != nil {
			t.Fatalf("share %d: hold: %v", k, err)
		}
		v2 := write(t, h, id, 0, distinct(20+k, mib))
		repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{v1Seg}})

		f.Crash()
		h = f.Share(k)
		got, missing := readBack(t, h, id, 0, mib)
		if len(missing) != 0 || !bytes.Equal(got, distinct(20+k, mib)) {
			t.Fatalf("share %d: the newer version reads missing %+v or other bytes after the repack of the held one", k, missing)
		}

		// The retained version came through with the hold still on it and is
		// offered as superseded; reporting that extent retires it from the
		// index.
		var superseded []journal.OfferedExtent
		offload(t, h, id, v2, journaltest.FixtureMaxBytes, 0, func(o journal.Offer, report func([]journal.Extent) error) error {
			superseded = o.Superseded
			var put []journal.Extent
			for _, se := range o.Superseded {
				if se.Version == v1 {
					put = append(put, se.Extent)
				}
			}
			if err := report(put); err != nil {
				return called("report", err)
			}
			return nil
		})
		var foundV1 bool
		for _, se := range superseded {
			foundV1 = foundV1 || se.Version == v1
		}
		if !foundV1 {
			t.Fatalf("share %d: the offer under the hold carries nothing of the superseded version %v: %+v", k, v1, superseded)
		}
		for _, en := range entries(t, f, id) {
			if en.Kind == journal.EntryRetained && en.Version == v1 {
				t.Fatalf("share %d: the retained version %v is still in the index after its offload was reported", k, v1)
			}
		}
	}
}

// TestRFC1A_RemovalDroppedByVersion checks the row of the same name: with a
// hold retaining v1, a truncate at v2, and v1's copy repacked so its
// sequence number is above the truncate's, releasing the hold and settling
// the marker must still leave the extent missing after a repack scoped over
// the truncate's own segment and a crash — the marker's version decides, not
// a copy's sequence number. The pass that makes the copy must really make
// one: this fixture's idle-seal size is set below v1's 128 KiB — the
// default's sixteenth of the cap would leave a segment too small for the
// journal to seal idle — and the copy's landing outside its source, and
// outside the sealed segment holding the truncate, is asserted, not
// assumed. The truncate's segment is named by observation, not the index: a
// marker's entry carries no location, so no rule reading the index can name
// it, and the one segment file that took the writes across the truncate and
// the filler beside it is what the second pass is scoped over. A layout
// that scattered them is refused loudly rather than aimed into blind.
func TestRFC1A_RemovalDroppedByVersion(t *testing.T) {
	for k := range 2 {
		// Each share runs on its own journal and its own file — both shares
		// and both files per the suite's rule, one journal each — because a
		// shared journal would carry the first share's past between the
		// legs: recovery keeps a stream's unsealed newest segment active,
		// so share 0 would leave one behind holding the first pass's
		// copies, past this fixture's idle-seal size, and share 1's first
		// clock advance could seal it during the write-count window below —
		// marker, footer and trailer writes, no records — adding its name
		// among the gained segments and failing a correct journal. A fresh
		// journal inherits no other leg's segment, but it does not make
		// the window free of seal writes: each leg begins with the idle
		// seal of its own v1 segment, whose I/O can trail behind the seal
		// the clock took it at, and the snapshot check below therefore
		// counts no segment the journal already reports sealed. Each
		// subtest builds its own fixture, which also keeps the seam's
		// watch inside the subtest: one watch ends with the subtest before
		// the next share's begins.
		t.Run(fmt.Sprintf("share %d", k), func(t *testing.T) {
			// The idle-seal size sits below v1's 128 KiB — the default's
			// sixteenth of the cap, at this fixture's cap, is 256 KiB,
			// which v1 never reaches — so v1's segment seals idle, and so
			// does the truncate's once the filler below lands in it.
			f := journaltest.NewWithSegmentCap(t, recordCap, func(s *journal.Settings) {
				s.IdleSealBytes = 64 << 10
			})

			// segWrites maps every segment file now in the journal's
			// directory to how many writes the seam counted on it.
			segWrites := func() map[string]int {
				counts := map[string]int{}
				for _, name := range segmentNames(t, f) {
					counts[name] = f.Seam.CallsFor(journaltest.OpWrite, name)
				}
				return counts
			}
			// sealedNames names every segment the journal reports sealed:
			// RFC 28 §2.2 forbids appending to one, so a write that lands
			// there is the seal's own I/O — marker, footer, trailer — and
			// never a record.
			sealedNames := func() map[string]bool {
				names := map[string]bool{}
				for _, s := range f.J.Inspect().Segments() {
					if s.Sealed {
						names[segmentName(s.ID)] = true
					}
				}
				return names
			}
			// gainedSegs names the segment files that took record writes
			// between two snapshots — a snapshot of the write counts plus
			// the segments sealed at the first one, since a sealed segment
			// takes no further append and any write that lands on one
			// after it is the seal's own I/O, not a record: the segments
			// the appends in between landed in.
			gainedSegs := func(before, after map[string]int, sealedBefore map[string]bool) []string {
				var gained []string
				for name, n := range after {
					if n > before[name] && !sealedBefore[name] {
						gained = append(gained, name)
					}
				}
				slices.Sort(gained)
				return gained
			}

			h, id := f.Share(k), journaltest.File(k)
			v1 := write(t, h, id, 0, distinct(k, 128<<10))
			syncFile(t, h, id)
			v1Seg := heldEntry(t, f, id).Location.Segment
			sealNow(t, f, v1Seg)

			share := journal.ShareID{byte(k + 1)}
			cut := journal.SnapshotCut(k + 1)
			err := h.Hold(t.Context(), share, cut, map[journal.FileID]journal.Version{id: v1})
			stub(t, "Hold", err)
			if err != nil {
				t.Fatalf("share %d: hold: %v", k, err)
			}
			// The truncate's marker entry carries no index location, so no read
			// of the index can name its segment. What names it is the writes the
			// appends took: the truncate's record and the filler below it go to
			// the one segment the file's stream stands at — a spare of the
			// fixture's cap holding, before the filler's 128 KiB, a few hundred
			// bytes of the hold's and the truncate's records, so every record
			// fits what remains of it and RFC 28 §2.2 rolls nothing between them.
			// A journal whose appends named more than the one segment reports a
			// layout this check refuses rather than scopes over blindly. The
			// segments sealed at the snapshot are left out of the count: the
			// seal of v1's segment above may still be writing its marker, footer
			// and trailer after sealNow reported it sealed, and those writes
			// count on a name a sealed segment is anyway barred from taking a
			// record on, so they are the seal's own I/O, not a record landing.
			writesBefore, sealedBefore := segWrites(), sealedNames()
			v2, err := h.Truncate(t.Context(), id, 0)
			stub(t, "Truncate", err)
			if err != nil {
				t.Fatalf("share %d: truncate: %v", k, err)
			}
			syncFile(t, h, id)
			// The filler writes past the extent the truncate removed —
			// [128 KiB, 256 KiB) of a file whose [0, 128 KiB) the truncate took
			// away — so it refills nothing the marker covers, and its 128 KiB
			// raises the truncate's segment past the idle-seal size so the seal
			// below can take it.
			write(t, h, id, 128<<10, distinct(80+k, 128<<10))
			syncFile(t, h, id)
			truncateSeg := heldEntry(t, f, id).Location.Segment
			if gained := gainedSegs(writesBefore, segWrites(), sealedBefore); len(gained) != 1 || gained[0] != segmentName(truncateSeg) {
				t.Fatalf("share %d: the records across the truncate and the filler landed in %v, want only segment %d holding the filler, which the second pass's scope hangs on", k, gained, truncateSeg)
			}
			sealNow(t, f, truncateSeg)

			// The first pass, over v1's segment, copies the live records it
			// holds — v1, retained by the hold — under the stream's serialized
			// append into the segment the stream stands at then: the copy keeps
			// v1's content version under a sequence number the appends recorded
			// after it put above the truncate's. Both segments of the file's
			// stream are sealed by now, so a copy landing in either would be a
			// journal appending to a sealed segment, which RFC 28 §2.2 forbids.
			repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{v1Seg}})
			var carriedOutside bool
			for _, e := range entries(t, f, id) {
				carriedOutside = carriedOutside || e.Kind == journal.EntryRetained && e.Version == v1 && e.Location.Segment != v1Seg && e.Location.Segment != truncateSeg
			}
			if !carriedOutside {
				t.Fatalf("share %d: the pass over v1's segment copied no retained v1 outside the sealed segments, so the row's later sequence number was never given one: %+v", k, entries(t, f, id))
			}

			// The hold is released and the marker settled, as the row's steps
			// have them: nothing pins the removal any more but its own reach —
			// the copy's record is still on disk, below the truncate's pair.
			err = h.Unhold(t.Context(), share, cut)
			stub(t, "Unhold", err)
			if err != nil {
				t.Fatalf("share %d: unhold: %v", k, err)
			}
			h.Settle(id, v2)
			// The second pass, scoped to the truncate's own segment, is the
			// row's "repack until the truncate's segment is selected", and it
			// must retire the segment it selected, or no later read judges
			// anything it decided. A rule dropping a removal by sequence alone
			// drops the truncate here — the copy's later sequence reads as
			// superseding — and unlinks it with its segment, so the crash
			// rebuilds no marker over the copy and the copy comes back held,
			// reading v1's bytes where the truncate removed them. The row's
			// rule keeps the truncate: the copy's record overlaps its extent
			// only below its (version, sequence) pair, so it is carried beside
			// the filler, and the extent reads missing after the crash.
			repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{truncateSeg}})
			syncFile(t, h, id)
			if !retired(f, truncateSeg) {
				t.Fatalf("share %d: the pass over the truncate's segment %d left it owned, so nothing it could have decided was ever judged", k, truncateSeg)
			}

			f.Crash()
			r := read(t, f.Share(k), id, 0, 128<<10)
			wantMissing(t, r, 0, []journal.Extent{ext(0, 128<<10)}, "the truncated-away extent after a repack decided its removal by sequence alone")
		})
	}
}

// TestRFC1A_DropsDecidedAfterCopies checks that a pass decides the removal
// records it drops only once its own copies are appended: a settled truncate
// over a snapshot-held v1, both in one segment holding nothing else of the
// file anywhere, survives a pass scoped over that one segment, and v1's copy
// lands in a fresh segment outside the pass's scope. A pass that decides its
// drops before its copies unlinks the truncate away with nothing carried in
// its place, and the crash leaves v1 held where zeros belong.
func TestRFC1A_DropsDecidedAfterCopies(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const size = 256 << 10

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		v1 := write(t, h, id, 0, distinct(k, size))
		syncFile(t, h, id)
		v1Seg := heldEntry(t, f, id).Location.Segment

		share := journal.ShareID{byte(k + 1)}
		cut := journal.SnapshotCut(k + 1)
		err := h.Hold(t.Context(), share, cut, map[journal.FileID]journal.Version{id: v1})
		stub(t, "Hold", err)
		if err != nil {
			t.Fatalf("share %d: hold: %v", k, err)
		}
		v2, err := h.Truncate(t.Context(), id, 0)
		stub(t, "Truncate", err)
		if err != nil {
			t.Fatalf("share %d: truncate: %v", k, err)
		}
		h.Settle(id, v2)
		// The row puts the truncate and the retained v1 under it in one
		// segment, and nothing of the file anywhere else: the truncate lands
		// however many segments past v1 the journal's own appends took, so
		// sharing one segment is asserted, not assumed — a journal whose
		// records straddled a roll here is one whose pass scopes differently
		// below, and the check names it rather than judging its drops on a
		// scope the row did not give. Markers are exempt: their location is
		// zero, wherever their record sits.
		for _, en := range entries(t, f, id) {
			if en.Kind != journal.EntryMarker && en.Location.Segment != v1Seg {
				t.Fatalf("share %d: the file holds %+v outside the one segment the pass is scoped to", k, en)
			}
		}
		// The segment is sealed idle — v1's 256 KiB retained is past the
		// idle-seal size — before the pass runs, so the copies the pass
		// makes append to the stream's fresh active segment, outside the
		// pass's scope: the pass cannot sit over its own copies' target,
		// where no pass retires it and no design ever drops the truncate.
		sealNow(t, f, v1Seg)
		repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{v1Seg}})

		f.Crash()
		h = f.Share(k)
		r := read(t, h, id, 0, size)
		wantMissing(t, r, 0, []journal.Extent{ext(0, size)}, "the truncated extent after a repack that carried its marker")

		// The truncate came through the crash with the retained v1 it was
		// carried beside: it is still what Since yields, and the retained
		// copy of v1 sits outside its original segment.
		var carried bool
		for _, c := range since(t, h, id, journal.Version{}) {
			carried = carried || c.Kind == journal.ChangeTruncate && c.Version == v2
		}
		if !carried {
			t.Fatalf("share %d: the settle truncate marker did not survive the repack with the retained content", k)
		}
		var retainedElsewhere bool
		for _, e := range entries(t, f, id) {
			if e.Kind == journal.EntryRetained && e.Version == v1 && e.Location.Segment != v1Seg {
				retainedElsewhere = true
			}
		}
		if !retainedElsewhere {
			t.Fatalf("share %d: no retained copy of v1 survives outside its original segment: %+v", k, entries(t, f, id))
		}
	}
}

// TestRFC1A_CarriedReleaseKeepsItsSequence checks that a release record a
// repack carries keeps its place in the record order relative to the fill
// that refills the extent it emptied, so the filled bytes still read after a
// crash.
func TestRFC1A_CarriedReleaseKeepsItsSequence(t *testing.T) {
	f := journaltest.NewWithSegmentCap(t, recordCap, nil)
	const mib = 1 << 20
	e := ext(0, 256<<10)

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		v7 := write(t, h, id, 0, distinct(k, 256<<10))
		mark(t, h, id, e, v7)
		release(t, h, id, e)
		syncFile(t, h, id)

		// The release record was appended to the segment active when the
		// release ran, not to the one holding the extent. A probe write to
		// the same file, at an offset the release does not cover, made
		// before anything else lands, identifies that segment: whichever
		// stream the file maps to, the probe and the release record were
		// appended to the same one.
		write(t, h, id, mib, pattern(fmt.Sprint("probe", k), 4096))
		scope := journal.RepackScope{Segments: []journal.SegmentID{segmentOf(t, f, id, mib)}}

		filled := distinct(20+k, 256<<10)
		fill(t, h, id, 0, filled, v7)
		repackAll(t, f, scope)

		f.Crash()
		got, missing := readBack(t, f.Share(k), id, 0, 256<<10)
		if len(missing) != 0 || !bytes.Equal(got, filled) {
			t.Fatalf("share %d: the filled extent reads missing %+v or other bytes after a repack of its release's segment", k, missing)
		}
	}
}

// TestRFC1A_ReappendTrustsNoPageCache checks that a re-append after a failed
// sync trusts no cached bytes: with the device serving, for every read of the
// failed segment, the bytes it held before the failure, the re-appended
// record either verifies against what the write stored or the window fails
// with a loss event, and no record is appended to the failed segment again,
// not even once the window has resolved and the file is written further.
func TestRFC1A_ReappendTrustsNoPageCache(t *testing.T) {
	f := journaltest.New(t)
	const size = 64 << 10

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		p := distinct(k, size)
		write(t, h, id, 0, p)
		seg := segmentOf(t, f, id, 0)
		name := segmentName(seg)

		writes := f.Seam.CallsFor(journaltest.OpWrite, name)
		f.FailNext(journaltest.OpSync, name, 1)
		f.Seam.StaleReads(name)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		failed := errors.Is(err, journal.ErrSyncFailed)
		if err != nil && !failed {
			t.Fatalf("share %d: the Sync over the failed one returned %v, want a SyncError or a re-append", k, err)
		}
		if got := f.Seam.CallsFor(journaltest.OpWrite, name); got != writes {
			t.Fatalf("share %d: %d records were appended to the failed segment before its sync was retried", k, got-writes)
		}

		got, missing := readBack(t, h, id, 0, size)
		if failed {
			if len(missing) == 0 {
				t.Fatalf("share %d: the write of the failed window reads back: no re-append of it may serve stale bytes", k)
			}
			var lost bool
			for _, l := range f.J.Stats().Losses {
				lost = lost || (l.File == id && l.Extent.Off < size && l.Reason == journal.LossFailedWindow)
			}
			if !lost {
				t.Fatalf("share %d: no loss event names the write of the failed window: %+v", k, f.J.Stats().Losses)
			}
		} else if len(missing) != 0 || !bytes.Equal(got, p) {
			t.Fatalf("share %d: the re-appended write reads missing %+v or other bytes than were written", k, missing)
		}

		// No record is appended to the failed segment afterwards: once it
		// failed, the segment takes no further append, so the file's next
		// write records land in the stream's next segment. The write leaves
		// the failed segment, a roll, which a correct journal can refuse for
		// want of a spare, so it is retried as one that returns to the pool
		// can be.
		if got := f.Seam.CallsFor(journaltest.OpWrite, name); got != writes {
			t.Fatalf("share %d: %d records were appended to the failed segment after its sync failed", k, got-writes)
		}
		if _, err := tryWrite(t, h, id, size, distinct(40+k, size)); err != nil {
			stub(t, "WriteAt", err)
			t.Fatalf("share %d: the write after the window resolved: %v", k, err)
		}
		if got := f.Seam.CallsFor(journaltest.OpWrite, name); got != writes {
			t.Fatalf("share %d: %d records were appended to the failed segment after the window resolved and the file was written again", k, got-writes)
		}

		f.Crash()
		got, missing = readBack(t, f.Share(k), id, 0, size)
		if failed {
			if len(missing) == 0 {
				t.Fatalf("share %d: the write of the failed window reads back after a crash", k)
			}
		} else if len(missing) != 0 || !bytes.Equal(got, p) {
			t.Fatalf("share %d: the re-appended write reads missing %+v or other bytes after a crash", k, missing)
		}
	}
}

// TestRFC1A_LossByExactRecord checks that a failed window's loss record names
// the record of the write it lost, so the extent falls back to the synced
// version below it, before and after a crash.
//
// decision: the loss record's sequence number is not observable on the
// journal's public surface, so the check asserts the loss event's Version —
// the write the window lost, never the version it superseded — and the read
// that falls back to the synced bytes. Overturned if the event ever carries
// a number that fails to identify the lost record itself; do not export one
// for this.
//
// decision: every read of the failed segment fails, so a re-append from the
// segment cannot verify, but a journal may still have kept a verified copy
// at write time and re-append it, in which case nothing was lost and there
// is no loss record to name. The check branches on the Sync's answer, like
// the other window checks, and asserts the loss record's version only on
// the failed path.
func TestRFC1A_LossByExactRecord(t *testing.T) {
	f := journaltest.New(t)
	const size = 64 << 10
	e := ext(0, size)

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		write(t, h, id, 0, distinct(k, size))
		syncFile(t, h, id)
		v10 := write(t, h, id, 0, distinct(20+k, size))

		// The timer's sync is what fails, so the write made no Sync call of
		// its own. The reads of the segment fail too, installed before the
		// timer runs, so the window cannot resolve first by re-appending from
		// a copy it reads back.
		seg := segmentOf(t, f, id, 0)
		failures := f.J.Stats().Counters.SyncFailures
		failWindow(t, f, seg)
		f.Advance(journal.SyncBound)
		eventually(t, fmt.Sprintf("share %d: the write the timer was to sync never gained a failed sync of segment %d", k, seg), func() bool {
			return f.J.Stats().Counters.SyncFailures > failures
		})
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		failed := errors.Is(err, journal.ErrSyncFailed)
		if err != nil && !failed {
			t.Fatalf("share %d: the Sync over the failed timer sync returned %v, want a SyncError or a re-append", k, err)
		}

		// A failed window takes the unsynced write, so the extent falls back
		// to the synced version's bytes; a re-appended one serves the write.
		got, missing := readBack(t, h, id, 0, size)
		if failed {
			if len(missing) != 0 || !bytes.Equal(got, distinct(k, size)) {
				t.Fatalf("share %d: after the window took the unsynced write, the extent reads missing %+v or other bytes, want the synced version's", k, missing)
			}
		} else if len(missing) != 0 || !bytes.Equal(got, distinct(20+k, size)) {
			t.Fatalf("share %d: a Sync that re-appended the window left the write missing %+v or other bytes", k, missing)
		}

		// The loss record names the unsynced write's version, never the
		// synced version it superseded. A re-append lost nothing, so there
		// is nothing to name.
		if failed {
			var loss []journal.LossEvent
			for _, l := range f.J.Stats().Losses {
				if l.File == id && overlap(l.Extent, e) > 0 {
					loss = append(loss, l)
				}
			}
			if len(loss) != 1 {
				t.Fatalf("share %d: %d loss events name one extent: %+v", k, len(loss), f.J.Stats().Losses)
			}
			if loss[0].Version != v10 {
				t.Fatalf("share %d: the loss record names version %v, want the write the window lost, %v", k, loss[0].Version, v10)
			}
		}

		f.Crash()
		got, missing = readBack(t, f.Share(k), id, 0, size)
		if failed {
			if len(missing) != 0 || !bytes.Equal(got, distinct(k, size)) {
				t.Fatalf("share %d: the extent reads missing %+v or other bytes after a crash, want the synced version's", k, missing)
			}
		} else if len(missing) != 0 || !bytes.Equal(got, distinct(20+k, size)) {
			t.Fatalf("share %d: the extent reads missing %+v or other bytes after a crash, want the re-appended write's", k, missing)
		}
	}
}

// TestRFC1A_SyncedLossMasksSuperseded checks that losing a synced write drops
// its extent rather than falling back to the version it superseded: the
// extent reads missing after a reopen that still has the older record on
// disk, never its stale bytes.
func TestRFC1A_SyncedLossMasksSuperseded(t *testing.T) {
	f := journaltest.New(t)
	const size = 64 << 10

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)
		write(t, h, id, 0, distinct(k, size))
		syncFile(t, h, id)
		overwrite := distinct(20+k, size)
		write(t, h, id, 0, overwrite)
		syncFile(t, h, id)

		corrupt(t, f, id, overwrite)
		// The older record stays on disk: no repack is run.
		f.Crash()
		r := read(t, f.Share(k), id, 0, size)
		wantMissing(t, r, 0, []journal.Extent{ext(0, size)}, "the extent whose superseding record rotted")
	}
}

// TestRFC1A_FailedWindowReportedOnce checks that a window the timer's sync
// failed, with no Sync waiting when it failed, is reported once: the next
// Sync of the file whose writes it lost fails, the one after succeeds, and a
// Sync of a file with no write in the window succeeds throughout.
//
// decision: the timer's window may be resolved by a re-append rather than
// left failed, and every read of the failed segment fails too, so the re-append
// can come only from a copy the journal kept in its own memory at write time.
// A journal that always keeps one re-appends, reports nothing and succeeds
// here — which is the right answer, a window resolved is not a window lost —
// leaving the once-only reporting unexercised on it. The check branches on the
// Sync's answer the way the other window checks do, and asserts the single
// report only on the failed path. Overturned if the journal's resolution
// becomes settable, when the failed path can be demanded.
func TestRFC1A_FailedWindowReportedOnce(t *testing.T) {
	f := journaltest.New(t)

	for k := range 2 {
		// A file with no write in the failed window, on the other share,
		// written and synced before F: the mapping of files to streams is the
		// journal's own, and a G sharing F's stream would otherwise have its
		// sync make F's already-written record durable before the fault is
		// armed — a window with nothing of F to fail.
		gID := journaltest.File(70 + k)
		g := distinct(30+k, 64<<10)
		gh := f.Share(1 - k)
		write(t, gh, gID, 0, g)
		syncFile(t, gh, gID)

		h, id := f.Share(k), journaltest.File(k)
		write(t, h, id, 0, distinct(k, 64<<10))
		seg := segmentOf(t, f, id, 0)

		// The timer's sync fails or is re-appended from a kept copy, with no
		// Sync of the file waiting: its next Sync is the first one to see
		// whichever the journal made of the window.
		failures := f.J.Stats().Counters.SyncFailures
		reappended := f.J.Stats().Counters.SyncReappendedBytes
		failWindow(t, f, seg)
		f.Advance(journal.SyncBound)
		eventually(t, fmt.Sprintf("share %d: the timer's sync of the write neither failed nor re-appended", k), func() bool {
			return f.J.Stats().Counters.SyncFailures > failures || f.J.Stats().Counters.SyncReappendedBytes > reappended
		})
		f.Seam.SetFault(nil)

		// A Sync of a file with no write in the window succeeds between the
		// failure and the file's first Sync, and between the file's two, so
		// whatever the window's report reaches, it does not reach a file the
		// window holds nothing of.
		if err := gh.Sync(t.Context(), gID); err != nil {
			stub(t, "Sync", err)
			t.Fatalf("share %d: the Sync of a file with no write in the failed window, before the file's own, returned %v", 1-k, err)
		}

		err := h.Sync(t.Context(), id)
		stub(t, "Sync", err)
		failed := errors.Is(err, journal.ErrSyncFailed)
		if err != nil && !failed {
			t.Fatalf("share %d: the first Sync after the window failed returned %v, want a SyncError or a re-append", k, err)
		}
		if !failed {
			if got := f.J.Stats().Counters.SyncReappendedBytes; got <= reappended {
				t.Fatalf("share %d: the window resolved without failure but re-appended no bytes", k)
			}
		}

		if err := gh.Sync(t.Context(), gID); err != nil {
			stub(t, "Sync", err)
			t.Fatalf("share %d: the Sync of a file with no write in the failed window, between the file's two, returned %v", 1-k, err)
		}
		err = h.Sync(t.Context(), id)
		stub(t, "Sync", err)
		if err != nil {
			t.Fatalf("share %d: the second Sync of the file returned %v, want the window reported once and the file writing on", k, err)
		}
		err = gh.Sync(t.Context(), gID)
		stub(t, "Sync", err)
		if err != nil {
			t.Fatalf("share %d: the Sync of a file with no write in the failed window returned %v", 1-k, err)
		}

		// The file G still reads, and F's next write syncs clean.
		got, missing := readBack(t, gh, gID, 0, 64<<10)
		if len(missing) != 0 || !bytes.Equal(got, g) {
			t.Fatalf("share %d: the file outside the window reads missing %+v or other bytes", 1-k, missing)
		}
	}
}

// TestRFC1A_ReappendKeepsItsNumbers checks that a re-append does not outrank
// a removal over it: after a write whose window was resolved by re-appending
// is truncated and the truncate synced, a crash that leaves the original, the
// copy and the truncate all on disk still reads the extent missing.
//
// decision: the crash is drawn on the seam's reorder, with the mark standing
// from before the original runs and a seed whose leading draws keep it. The
// mark has to stand before the original — the seam marks every later write
// only — and a plain crash undoes the original, whose sync failed, so only the
// copy and the truncate would remain, and the check would pass on the design
// it names. The draw is a coin per data write of the re-append path, so
// whether the original survived is read from the segment's bytes after the
// crash, and a draw that did not keep it leaves the no-reappend state — the
// same one the failed window gives — which the check skips on that observed
// image rather than passing vacuously. Its ceiling: the copy's own number is
// not observable, so a re-append that outranks the truncate is the only
// outcome the missing read can catch. Overturned if the journal reports the
// sequence number a re-append took.
func TestRFC1A_ReappendKeepsItsNumbers(t *testing.T) {
	f := journaltest.New(t)
	const size = 64 << 10

	for k := range 2 {
		h, id := f.Share(k), journaltest.File(k)

		// The mark is taken before the original runs and matches every name,
		// so it covers the original's record, the re-append's copy and the
		// truncate's record wherever each was appended: the seam marks every
		// later write, and the crash's draw below runs over the marked writes
		// alone. A plain crash would undo the original — its window failed,
		// so it is left unsynced with its sync failed — and only the copy the
		// re-append wrote would survive, which is the state the check must
		// not be handed.
		f.Seam.MarkReorder("*")
		p := distinct(k, size)
		write(t, h, id, 0, p)
		seg := segmentOf(t, f, id, 0)

		before := f.J.Stats().Counters.SyncReappendedBytes
		f.FailNext(journaltest.OpSync, segmentName(seg), 1)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		if err != nil {
			if !errors.Is(err, journal.ErrSyncFailed) {
				t.Fatalf("share %d: the Sync over the failure returned %v, want a SyncError or a re-append", k, err)
			}
			// decision: the window failed, so no re-append exists to number
			// and the check has nothing to catch; the fail path is the failed
			// window's own checks' answer.
			continue
		}
		if got := f.J.Stats().Counters.SyncReappendedBytes; got <= before {
			t.Fatalf("share %d: a Sync that returned success re-appended no bytes over its failed window", k)
		}

		_, terr := h.Truncate(t.Context(), id, 0)
		stub(t, "Truncate", terr)
		if terr != nil {
			t.Fatalf("share %d: the truncate over the re-appended write: %v", k, terr)
		}
		syncFile(t, h, id)

		// Seed 404 keeps the first sixteen draws, so the original's record
		// survives the crash whole however many data writes its journal spent
		// on it; a seed whose early draws drop any one of them loses part of
		// the record and reads as a crash that never kept the original. The
		// truncate and the copy are synced, so they are durable whatever the
		// draw keeps.
		f.Crash(journaltest.Reordered(404))

		// The crash is observed by what it left, not by what it was handed:
		// the file's own bytes say whether the original came through it. A
		// segment left without the original means the draw spent its early
		// keeps elsewhere, and the remainder of this leg reads as the plain
		// crash the decision at the head names.
		file, oerr := f.Seam.Open(segmentName(seg))
		stub(t, "Open", oerr)
		if oerr != nil {
			t.Fatalf("share %d: the failed segment's file after the crash: %v", k, oerr)
		}
		image := make([]byte, int64(journaltest.FixtureSegmentCap))
		_, rerr := file.ReadAt(image, 0)
		cerr := file.Close()
		stub(t, "ReadAt/Close", errors.Join(rerr, cerr))
		if (rerr != nil && !errors.Is(rerr, io.EOF)) || cerr != nil {
			t.Fatalf("share %d: the failed segment's file after the crash (read %v, close %v)", k, rerr, cerr)
		}
		// decision: a segment image without the original is the seed's doing,
		// not the journal's, so what follows has no original to keep and no
		// re-append to number. The check fails rather than skips: the seed is
		// fixed, so a draw that drops the original does the same on every run,
		// and a skip that fires once fires for good.
		if !bytes.Contains(image, p) {
			t.Fatalf("share %d: the seeded crash dropped the re-appended write's original, so it left no state the check's row names; pick another seed", k)
		}

		r := read(t, f.Share(k), id, 0, size)
		wantMissing(t, r, 0, []journal.Extent{ext(0, size)}, "the extent truncated after its write was re-appended")
	}
}
