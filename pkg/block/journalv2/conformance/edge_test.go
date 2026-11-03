//go:build journal_conformance

package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// The checks here fail by breaking an extreme the ordinary checks never
// reach: empty and enormous writes, overwrites at every scale, removals that
// leave a hole, refusals at an exact fit, and a directory opened from
// nothing.

// edgeMaxOffset is the largest exclusive end a write can name: a marker to
// the end of a file carries it, and a write whose extent ends there is the
// last one accepted, since one past it has no int64 offset for its final
// byte.
const edgeMaxOffset int64 = math.MaxInt64

// writeStats is the slice of Stats a check compares over a call that must
// append nothing, so the comparison reads one record rather than four reads
// spread over it.
type writeStats struct {
	extentCount int64
	heldBytes   int64
	dirtyBytes  int64
	nextVersion journalv2.Version
}

func writeStatsOf(s journalv2.Stats) writeStats {
	return writeStats{s.ExtentCount, s.HeldBytes, s.DirtyBytes, s.NextVersion}
}

// TestEdge_ZeroLengthWrite checks that an empty write appends nothing, and
// that its file holds the bytes it did: no record reaches a segment, no count
// the stats keep moves save the version counter when a nil return draws from
// it, Since yields no change wherever the empty write falls.
//
// decision: the format refuses a zero length only for Deallocate and
// CloneTarget. This check accepts a zero-length write returning nil, which
// draws a version, or ErrZeroLength, and holds the journal to appending
// nothing under either: the refused outcome leaves every stat where it was,
// the accepted one moves the counter by exactly the versions it drew, and no
// bytes move under either. Overturned once the written interface names the
// write's outcome.
func TestEdge_ZeroLengthWrite(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		want := pattern(fmt.Sprint("zero", k), 4096)
		v := write(t, h, id, 0, want)
		syncFile(t, h, id)

		before := writeStatsOf(f.J.Stats())
		writes := segmentWrites(t, f)
		// The counter's floor before any empty write: an accepted one moves
		// NextVersion by exactly the versions it drew, so the rise is
		// measured on the counter half, whose byte-for-byte floor is not a
		// Version the journal issued.
		floor := f.J.Stats().NextVersion.Counter

		drawn := 0
		for _, off := range []int64{0, 2048} {
			pre := writeStatsOf(f.J.Stats())
			got, err := h.WriteAt(t.Context(), id, off, nil, 0)
			stub(t, "WriteAt", err)
			if err != nil {
				if !errors.Is(err, journalv2.ErrZeroLength) {
					t.Fatalf("share %d: an empty write at %d returned %v, want nil or ErrZeroLength", k, off, err)
				}
				if post := writeStatsOf(f.J.Stats()); post != pre {
					t.Fatalf("share %d: an empty write refused with ErrZeroLength moved the stats from %+v to %+v", k, pre, post)
				}
				continue
			}
			drawn++
			if got.Compare(v) <= 0 {
				t.Fatalf("share %d: an empty write at %d drew version %v, not above the write before it (%v)", k, off, got, v)
			}
			if got.Compare(pre.nextVersion) < 0 {
				t.Fatalf("share %d: an empty write at %d drew version %v, below the counter's floor before it (%v)", k, off, got, pre.nextVersion)
			}
		}

		if after := segmentWrites(t, f); after != writes {
			t.Fatalf("share %d: %d write calls reached segments over two empty writes, want none", k, after-writes)
		}
		got := writeStatsOf(f.J.Stats())
		if got.extentCount != before.extentCount || got.dirtyBytes != before.dirtyBytes || got.heldBytes != before.heldBytes {
			t.Fatalf("share %d: two empty writes moved the stats for %d drawn versions from %+v to %+v", k, drawn, before, got)
		}
		if rose := got.nextVersion.Counter - floor; rose != uint64(drawn) {
			t.Fatalf("share %d: two empty writes drew %d versions but moved the counter by %d", k, drawn, rose)
		}
		if cs := since(t, h, id, v); len(cs) != 0 {
			t.Fatalf("share %d: two empty writes yielded %+v since the write before them", k, cs)
		}
		wantBytes(t, h, id, 0, want, "after two empty writes")
	}
}

// TestEdge_WriteNearLargestOffset checks that a write ending exactly at the
// largest int64 offset syncs and reads back byte-exact across a crash, that
// the range before it is reported missing exactly, never widened by the
// giant offset, and that a write whose end would pass it fails naming some
// error and appends nothing.
//
// decision: no interface names a largest file offset or the error for a write
// past it; markers to the end make MaxInt64 − Off the file's last byte. This
// check reads an offset as int64 to MaxInt64 and asks only that a write past
// it fail with nothing appended. Overturned when the interface names either.
func TestEdge_WriteNearLargestOffset(t *testing.T) {
	const n = 4096
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := pattern(fmt.Sprint("far", k), n)

		off := edgeMaxOffset - n // the write's extent ends at the largest offset, never past it
		write(t, h, id, off, p)
		syncFile(t, h, id)
		wantBytes(t, h, id, off, p, "the write ending at the largest offset")

		r := read(t, h, id, off-n, 2*n)
		wantMissing(t, r, off-n, []journalv2.Extent{ext(off-n, n)}, "the range below the write at the largest offset")

		before := writeStatsOf(f.J.Stats())
		writes := segmentWrites(t, f)
		_, err := h.WriteAt(t.Context(), id, edgeMaxOffset, []byte{1}, 0)
		stub(t, "WriteAt", err)
		if err == nil {
			t.Fatalf("share %d: a write whose last byte sits past the largest offset was accepted", k)
		}
		if after := segmentWrites(t, f); after != writes {
			t.Fatalf("share %d: %d write calls reached segments over a write past the end, want none", k, after-writes)
		}
		if got := writeStatsOf(f.J.Stats()); got != before {
			t.Fatalf("share %d: a write past the largest offset moved the stats from %+v to %+v", k, before, got)
		}

		f.Crash()
		h = f.Share(k)
		wantBytes(t, h, id, off, p, "the write ending at the largest offset, after a crash")
		r = read(t, h, id, off-n, 2*n)
		wantMissing(t, r, off-n, []journalv2.Extent{ext(off-n, n)}, "the range below the write at the largest offset, after a crash")
	}
}

// TestEdge_ThreeWaySplit checks that an overwrite of the middle of one record
// splits the one held extent into three entries once its sync settles: the
// outer pieces keep the first write's version and the record it made, the
// middle takes the second write's, and the held bytes are the three extents
// once, whatever cutting or coalescing changed underneath.
func TestEdge_ThreeWaySplit(t *testing.T) {
	const chunk = 4096
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		old := distinct(k, 3*chunk)
		v1 := write(t, h, id, 0, old)
		mid := distinct(10+k, chunk)
		v2 := write(t, h, id, chunk, mid)

		want := slices.Concat(old[:chunk], mid, old[2*chunk:])
		wantBytes(t, h, id, 0, want, "an extent overwritten in its middle")

		syncFile(t, h, id)
		es := heldEntries(t, f, id)
		if len(es) != 3 {
			t.Fatalf("share %d: the file holds %d held entries after the sync, want 3: %+v", k, len(es), es)
		}
		wantExtents := []journalv2.Extent{ext(0, chunk), ext(chunk, chunk), ext(2*chunk, chunk)}
		wantVersions := []journalv2.Version{v1, v2, v1}
		for i := range 3 {
			if es[i].Extent != wantExtents[i] || es[i].Version != wantVersions[i] {
				t.Fatalf("share %d: entry %d is %+v, want extent %+v at version %v", k, i, es[i], wantExtents[i], wantVersions[i])
			}
		}
		if es[0].Location != es[2].Location {
			t.Fatalf("share %d: the outer pieces of the split hold %+v and %+v, want the first write's one record", k, es[0].Location, es[2].Location)
		}
		if es[0].Location == es[1].Location {
			t.Fatalf("share %d: the middle piece shares the first write's record %+v", k, es[0].Location)
		}
		if s := f.J.Stats().Shares[journalv2test.Tag(k)]; s.HeldBytes != 3*chunk {
			t.Fatalf("share %d: HeldBytes = %d over a three-way split, want %d", k, s.HeldBytes, 3*chunk)
		}
	}
}

// TestEdge_OverwriteSpansManyExtents checks that an overwrite covering parts
// of two extents and three whole ones leaves only the flanks at their own
// records' versions, its own bytes at its own, and the file's held bytes
// unchanged, across a crash.
func TestEdge_OverwriteSpansManyExtents(t *testing.T) {
	const chunk = 4096
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		old := distinct(k, 5*chunk)
		var versions [5]journalv2.Version
		for i := range 5 {
			versions[i] = write(t, h, id, int64(i)*chunk, old[i*chunk:(i+1)*chunk])
		}
		newer := distinct(10+k, 5*chunk)
		vNew := write(t, h, id, chunk/2, newer[chunk/2:9*chunk/2])
		syncFile(t, h, id)

		want := slices.Concat(old[:chunk/2], newer[chunk/2:9*chunk/2], old[9*chunk/2:])
		wantBytes(t, h, id, 0, want, "an overwrite over the middle of five extents")

		es := heldEntries(t, f, id)
		wantEntries := []struct {
			e journalv2.Extent
			v journalv2.Version
		}{
			{ext(0, chunk/2), versions[0]},
			{ext(chunk/2, 4*chunk), vNew},
			{ext(9*chunk/2, chunk/2), versions[4]},
		}
		if len(es) != len(wantEntries) {
			t.Fatalf("share %d: the file holds %d held entries after the sync, want %d: %+v", k, len(es), len(wantEntries), es)
		}
		for i, w := range wantEntries {
			if es[i].Extent != w.e || es[i].Version != w.v {
				t.Fatalf("share %d: entry %d is %+v, want extent %+v at version %v", k, i, es[i], w.e, w.v)
			}
		}
		if s := f.J.Stats().Shares[journalv2test.Tag(k)]; s.HeldBytes != 5*chunk {
			t.Fatalf("share %d: HeldBytes = %d after an overwrite in place, want the file's %d", k, s.HeldBytes, 5*chunk)
		}

		f.Crash()
		wantBytes(t, f.Share(k), id, 0, want, "an overwrite over the middle of five extents, after a crash")
	}
}

// TestEdge_ManyOverwritesFlatHeldBytes checks that a thousand overwrites of
// the same extent leave the journal holding the last bytes once, per file,
// the churn of the overwrites leaving the index at the two live entries and
// every counter it owes never below zero, and every version drawn strictly
// rising.
func TestEdge_ManyOverwritesFlatHeldBytes(t *testing.T) {
	const (
		rounds = 1000
		chunk  = 4096
	)
	f := journalv2test.New(t)

	var last [2]journalv2.Version
	s0 := f.J.Stats()
	next := s0.NextVersion
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		for i := range rounds {
			v := write(t, h, id, 0, pattern(fmt.Sprint("flat", k, i), chunk))
			if v.Compare(last[k]) <= 0 {
				t.Fatalf("share %d: overwrite %d drew version %v, not above the last's %v", k, i, v, last[k])
			}
			last[k] = v
			if i%100 == 99 {
				syncFile(t, h, id)
			}
		}
	}
	// Measured once, after both files' rounds: each of the two files' writes
	// drew a version, so the journal's counter rose by at least one per write.
	if rose := f.J.Stats().NextVersion.Counter - next.Counter; rose < 2*rounds {
		t.Fatalf("NextVersion rose by %d over %d overwrites of each of two files, want at least %d", rose, rounds, 2*rounds)
	}
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		syncFile(t, h, id)
		if es := heldEntries(t, f, id); len(es) != 1 || es[0].Extent != ext(0, chunk) || es[0].Version != last[k] {
			t.Fatalf("share %d: the file holds %+v after %d overwrites in place, want one extent at the last version %v", k, es, rounds, last[k])
		}
		s := f.J.Stats()
		if ss := s.Shares[journalv2test.Tag(k)]; ss.HeldBytes != chunk {
			t.Fatalf("share %d: HeldBytes = %d after a thousand overwrites of %d bytes in place, want exactly the last bytes", k, ss.HeldBytes, chunk)
		}
		wantBytes(t, h, id, 0, pattern(fmt.Sprint("flat", k, rounds-1), chunk), "the last of a thousand overwrites")
	}

	// The index still names the two extents the files hold. Every superseded
	// one is gone: the write that replaced it carried no removal marker, and
	// once its replacement synced its retained extent left the index, so the
	// count is the two live entries and no more. Where the superseded bytes
	// went no check can hold, because a segment left holding only synced,
	// superseded records is one the journal retires on its own, and in
	// either retire path its bytes leave every gauge at once - unlinked,
	// they are freed; recycled, they are a spare at its cap - so no lower
	// bound survives a journal that retires at the sync.
	s := f.J.Stats()
	if s.ExtentCount != 2 {
		t.Fatalf("ExtentCount = %d with two extents held, want the two live entries alone", s.ExtentCount)
	}
	if s.UnreclaimedBytes < 0 {
		t.Fatalf("UnreclaimedBytes = %d after a thousand overwrites of two files, want it never below zero", s.UnreclaimedBytes)
	}
}

// segmentsOver returns how many segments the held entries of es within e at
// version v lie in: the pieces of one write counted by segment. A held entry
// of another version inside e is the caller's to assert first, so none is
// counted.
func segmentsOver(es []journalv2.IndexEntry, e journalv2.Extent, v journalv2.Version) int {
	segs := map[journalv2.SegmentID]bool{}
	for _, en := range es {
		if en.Kind == journalv2.EntryHeld && en.Version == v && overlap(en.Extent, e) > 0 {
			segs[en.Location.Segment] = true
		}
	}
	return len(segs)
}

// crossesTail checks that a write of payload, too large for the room its
// segment has left, is cut at the cap into one record per segment, WriteAt
// returning one version once every piece is written, the pieces held
// contiguous at that version in a segment each, reading back whole across a
// crash. The file is first filled with records of the write's own size until
// its segment holds less than one more of them, so the write is the one that
// has to roll: fill names that fill's pattern, need is the record bytes one
// fill write takes, and said names the write in failures. It relies on the
// fixture's 1 MiB segment cap.
func crossesTail(t *testing.T, f *journalv2test.Fixture, h *journalv2.Handle, id journalv2.FileID, k int, payload []byte, said, fill string, need int64) {
	t.Helper()

	// Records of the test write's own size until the segment the stream sits
	// in cannot hold one more of them, so the crossing write is the one that
	// has to roll.
	off := fillToTail(t, h, f, id, fill, int64(len(payload)), need)
	e := ext(off, int64(len(payload)))
	v := write(t, h, id, off, payload)
	syncFile(t, h, id)

	es := indexEntries(t, f, id)
	if n := wantPieces(t, es, e, fmt.Sprintf("share %d: %s", k, said)); n < 2 {
		t.Fatalf("share %d: %s is %d held entries, want at least two", k, said, n)
	}
	if n := segmentsOver(es, e, v); n < 2 {
		t.Fatalf("share %d: %s's pieces lie in %d segments, want at least two", k, said, n)
	}

	wantBytes(t, h, id, off, payload, said)
	f.Crash()
	wantBytes(t, f.Share(k), id, off, payload, said+", after a crash")
}

// TestEdge_WriteCrossesSegment checks that a record larger than what its
// segment has left is cut at the cap into one record per segment, WriteAt
// returning one version once every piece is written, the pieces held
// contiguous at that version in a segment each, reading back whole across a
// crash. It relies on the fixture's 1 MiB segment cap.
func TestEdge_WriteCrossesSegment(t *testing.T) {
	const chunk = 64 << 10
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		crossesTail(t, f, h, id, k, distinct(20+k, chunk),
			"the write crossing a segment cap", "cross"+fmt.Sprint(k), writeRecordBytes(chunk))
	}
}

// TestEdge_RecordExactlyFillsSegment checks that a record sized so its
// segment ends exactly at the cap — header, records, seal marker, two footer
// entries and trailer included — lands whole in it, seals it and is
// accounted at its cap, and that eight more payload bytes make it not fit,
// so it is split at the cap instead. It relies on the fixture's 1 MiB
// segment cap and on the format's header, record, seal, footer-entry and
// trailer sizes.
func TestEdge_RecordExactlyFillsSegment(t *testing.T) {
	const probe = 4096
	capB := int64(journalv2test.FixtureSegmentCap)

	fillToCap := func(t *testing.T, size int64, whole bool) {
		t.Helper()
		f := journalv2test.New(t)

		h, id := f.Share(0), journalv2test.File(0)
		write(t, h, id, 0, pattern("probe", probe))
		first := entryAt(t, f, id, 0)
		// decision: the payload arithmetic holds only where the segment
		// holds this check's records from its first record offset. A
		// journal that puts anything else there fails the precondition,
		// not the check. Overturned when the format states a different
		// first-record offset.
		if first.Location.Offset != raw.SegmentHeaderSize {
			t.Fatalf("precondition: the probe record sits at %d, want the segment's first record offset %d", first.Location.Offset, raw.SegmentHeaderSize)
		}
		seg := first.Location.Segment

		exact := pattern("exact", int(size))
		write(t, h, id, probe, exact)
		third := pattern("third", 512)
		write(t, h, id, probe+size, third)
		syncFiles(t, h, id)

		s, ok := segmentInfo(f, seg)
		if !ok || !s.Sealed {
			t.Fatalf("segment %d is not sealed after its records filled it", seg)
		}
		if whole {
			if s.Allocated != capB {
				t.Fatalf("segment %d, filled exactly, is accounted at %d, want its cap %d", seg, s.Allocated, capB)
			}
			if es := heldIn(t, f, id, seg); len(es) != 2 {
				t.Fatalf("segment %d holds %d of the file's entries, want the probe and the record that filled it: %+v", seg, len(es), es)
			}
			if e := entryAt(t, f, id, probe+size); e.Location.Segment == seg {
				t.Fatalf("the marker of the record filling the segment at %d landed in it, past its cap", probe+size)
			}
		} else {
			if n := wantPieces(t, indexEntries(t, f, id), ext(probe, size), "the record that does not fit"); n != 2 {
				t.Fatalf("the record that does not fit is %d pieces, want two", n)
			}
			es := heldIn(t, f, id, seg)
			if len(es) != 2 {
				t.Fatalf("segment %d holds %d of the file's entries, want the probe and the first piece of the record cut at the cap: %+v", seg, len(es), es)
			}
			var piece bool
			for _, e := range es {
				if overlap(e.Extent, ext(probe, size)) > 0 {
					piece = true
				}
			}
			if !piece {
				t.Fatalf("segment %d holds %+v, none of them a piece of the record cut at the cap", seg, es)
			}
		}

		wantBytes(t, h, id, 0, slices.Concat(pattern("probe", probe), exact, third), "a record sized to its segment")
		other := pattern("other", 4096)
		write(t, f.Share(1), journalv2test.File(1), 0, other)
		syncFile(t, f.Share(1), journalv2test.File(1))
		f.Crash()
		wantBytes(t, f.Share(0), id, 0, slices.Concat(pattern("probe", probe), exact, third), "a record sized to its segment, after a crash")
		wantBytes(t, f.Share(1), journalv2test.File(1), 0, other, "a write on the other stream, after a crash")
	}

	// header + probe record + this record + seal + 2 footer entries +
	// trailer == cap, the payload a multiple of 8.
	room := capB - raw.SegmentHeaderSize - writeRecordBytes(probe) -
		(raw.RecordHeaderSize + 8) - raw.SealMarkerSize - 2*raw.CatalogEntrySize - raw.TrailerSize
	room &^= 7

	t.Run("Exact", func(t *testing.T) { fillToCap(t, room, true) })
	t.Run("DoesNotFit", func(t *testing.T) { fillToCap(t, room+8, false) })
}

// TestEdge_FirstHeldByteFarFromZero checks that a file whose first held byte
// sits a tebibyte out reports the hole before it exactly, serves its own
// bytes and lists its one extent, across a crash: the journal holds no
// assumption that a file begins at offset zero.
func TestEdge_FirstHeldByteFarFromZero(t *testing.T) {
	const (
		far  = 1 << 40
		size = 4096
	)
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := pattern(fmt.Sprint("far", k), size)
		write(t, h, id, far, p)
		syncFile(t, h, id)

		r := read(t, h, id, far-size, 2*size)
		wantMissing(t, r, far-size, []journalv2.Extent{ext(far-size, size)}, "the hole before the first held byte")
		if !bytes.Equal(r.p[size:], p) {
			t.Fatalf("share %d: the bytes at the far offset were not served", k)
		}

		es, err := h.Extents(id)
		stub(t, "Extents", err)
		if err != nil {
			t.Fatalf("share %d: list the file's extents: %v", k, err)
		}
		if !slices.Equal(es, []journalv2.Extent{ext(far, size)}) {
			t.Fatalf("share %d: Extents = %+v, want the one extent at the far offset", k, es)
		}

		f.Crash()
		h = f.Share(k)
		r = read(t, h, id, far-size, 2*size)
		wantMissing(t, r, far-size, []journalv2.Extent{ext(far-size, size)}, "the hole before the first held byte, after a crash")
		if !bytes.Equal(r.p[size:], p) {
			t.Fatalf("share %d: the bytes at the far offset were not served after a crash", k)
		}
	}
}

// TestEdge_TruncateInsideExtent checks that a truncate into the middle of one
// held extent narrows it: what it cuts reads missing exactly, what it keeps
// serves the bytes it did, Since yields the removal marker at the truncate's
// version and to the end of the file, and a crash keeps every piece.
func TestEdge_TruncateInsideExtent(t *testing.T) {
	const size = 8192
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := distinct(k, size)
		vw := write(t, h, id, 0, p)
		v, err := h.Truncate(t.Context(), id, 3000)
		stub(t, "Truncate", err)
		if err != nil {
			t.Fatalf("share %d: truncate inside an extent: %v", k, err)
		}

		r := read(t, h, id, 0, size)
		wantMissing(t, r, 0, []journalv2.Extent{ext(3000, size-3000)}, "the range the truncate cut")
		if !bytes.Equal(r.p[:3000], p[:3000]) {
			t.Fatalf("share %d: the range below the truncate was not served as it was", k)
		}

		if cs := since(t, h, id, vw); !sameChanges(cs, []change{{journalv2.ChangeTruncate, toEnd(3000), v}}) {
			t.Fatalf("share %d: Since yielded %+v for the truncate, want its marker at %+v and version %v", k, cs, toEnd(3000), v)
		}

		syncFile(t, h, id)
		f.Crash()
		h = f.Share(k)
		r = read(t, h, id, 0, size)
		wantMissing(t, r, 0, []journalv2.Extent{ext(3000, size-3000)}, "the range the truncate cut, after a crash")
		if !bytes.Equal(r.p[:3000], p[:3000]) {
			t.Fatalf("share %d: the range below the truncate was not served as it was, after a crash", k)
		}
	}
}

// TestEdge_TruncateToZeroAndWriteAgain checks that a truncate to zero holds
// nothing of the file, that a write over the truncated range is served and
// draws a version above the truncate's, and that the hole past the new write
// survives a crash.
func TestEdge_TruncateToZeroAndWriteAgain(t *testing.T) {
	const size = 8192
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		write(t, h, id, 0, distinct(k, size))
		vt, err := h.Truncate(t.Context(), id, 0)
		stub(t, "Truncate", err)
		if err != nil {
			t.Fatalf("share %d: truncate to zero: %v", k, err)
		}

		fresh := distinct(10+k, 4096)
		vw := write(t, h, id, 0, fresh)
		if vw.Compare(vt) <= 0 {
			t.Fatalf("share %d: the write over the truncated range drew version %v, not above the truncate's %v", k, vw, vt)
		}

		r := read(t, h, id, 0, size)
		if !bytes.Equal(r.p[:4096], fresh) {
			t.Fatalf("share %d: the write over the truncated range was not served", k)
		}
		wantMissing(t, r, 0, []journalv2.Extent{ext(4096, size-4096)}, "the range past the new write")

		syncFile(t, h, id)
		f.Crash()
		h = f.Share(k)
		r = read(t, h, id, 0, size)
		if !bytes.Equal(r.p[:4096], fresh) {
			t.Fatalf("share %d: the write over the truncated range was not served after a crash", k)
		}
		wantMissing(t, r, 0, []journalv2.Extent{ext(4096, size-4096)}, "the range past the new write, after a crash")
	}
}

// TestEdge_TruncateDownAndBackUp checks that a truncate growing the file back
// does not resurrect what the earlier one cut: the range reads missing, never
// zeros and never the old bytes, a fill carrying a change version from before
// the first truncate is refused stale and writes nothing, and the hole
// survives a crash.
func TestEdge_TruncateDownAndBackUp(t *testing.T) {
	const size = 8192
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := distinct(k, size)
		write(t, h, id, 0, p)
		asOf := read(t, h, id, 0, size).asOf
		if _, err := h.Truncate(t.Context(), id, 2048); err != nil {
			stub(t, "Truncate", err)
			t.Fatalf("share %d: truncate down: %v", k, err)
		}
		if _, err := h.Truncate(t.Context(), id, size); err != nil {
			stub(t, "Truncate", err)
			t.Fatalf("share %d: truncate back up: %v", k, err)
		}

		r := read(t, h, id, 0, size)
		wantMissing(t, r, 0, []journalv2.Extent{ext(2048, size-2048)}, "the range cut and grown back")
		if !bytes.Equal(r.p[:2048], p[:2048]) {
			t.Fatalf("share %d: the range below both truncates was not served as it was", k)
		}

		v := draw(t, f)
		fillP := pattern(fmt.Sprint("backfill", k), size)
		refused := f.J.Stats().Counters.FillRefused
		err := h.Fill(t.Context(), id, 0, fillP, asOf, v)
		stub(t, "Fill", err)
		if !errors.Is(err, journalv2.ErrStaleFill) {
			t.Fatalf("share %d: a fill carrying a change version from before the truncates returned %v, want ErrStaleFill", k, err)
		}
		if got := f.J.Stats().Counters.FillRefused; got != refused+1 {
			t.Fatalf("share %d: FillRefused rose by %d over one stale fill, want 1", k, got-refused)
		}
		r = read(t, h, id, 2048, size-2048)
		wantMissing(t, r, 2048, []journalv2.Extent{ext(2048, size-2048)}, "the range a refused fill left")

		syncFile(t, h, id)
		f.Crash()
		h = f.Share(k)
		r = read(t, h, id, 0, size)
		wantMissing(t, r, 0, []journalv2.Extent{ext(2048, size-2048)}, "the range cut and grown back, after a crash")
	}
}

// TestEdge_DeallocateInsideAndAcross checks that a deallocation inside one
// record and one spanning several leaves exactly what it named missing, the
// rest serving the bytes it did, that a zero-length deallocation is refused
// with nothing appended, and that Since carries each removal's exact extent
// and version, across a crash.
func TestEdge_DeallocateInsideAndAcross(t *testing.T) {
	const chunk = 4096
	f := journalv2test.New(t)

	// A: a hole inside one record.
	ha, a := f.Share(0), journalv2test.File(0)
	pa := distinct(0, 3*chunk)
	va := write(t, ha, a, 0, pa)
	vda, err := ha.Deallocate(t.Context(), a, ext(chunk, chunk))
	stub(t, "Deallocate", err)
	if err != nil {
		t.Fatalf("deallocate inside one record: %v", err)
	}

	// B: a hole across three records.
	hb, b := f.Share(1), journalv2test.File(1)
	pb := distinct(1, 3*chunk)
	var vb journalv2.Version
	for i := range 3 {
		vb = write(t, hb, b, int64(i)*chunk, pb[i*chunk:(i+1)*chunk])
	}
	var vdb journalv2.Version
	vdb, err = hb.Deallocate(t.Context(), b, ext(chunk/2, 2*chunk))
	stub(t, "Deallocate", err)
	if err != nil {
		t.Fatalf("deallocate across records: %v", err)
	}

	// The zero-length deallocation changes nothing.
	before := writeStatsOf(f.J.Stats())
	if _, err := ha.Deallocate(t.Context(), a, ext(chunk, 0)); !errors.Is(err, journalv2.ErrZeroLength) {
		stub(t, "Deallocate", err)
		t.Fatalf("a zero-length deallocate returned %v, want ErrZeroLength", err)
	}
	if got := writeStatsOf(f.J.Stats()); got != before {
		t.Fatalf("a zero-length deallocate moved the stats from %+v to %+v", before, got)
	}
	if cs := since(t, ha, a, va); len(cs) != 1 {
		t.Fatalf("a zero-length deallocate left %d changes for the file, want the one real deallocation's", len(cs))
	}

	syncFile(t, ha, a)
	syncFile(t, hb, b)

	// A: the hole inside one record, exact.
	r := read(t, ha, a, 0, 3*chunk)
	wantMissing(t, r, 0, []journalv2.Extent{ext(chunk, chunk)}, "the hole cut inside one record")
	if !bytes.Equal(r.p[:chunk], pa[:chunk]) || !bytes.Equal(r.p[2*chunk:], pa[2*chunk:]) {
		t.Fatalf("the ranges beside the hole inside one record were not served as they were")
	}
	held := heldOnly(heldEntries(t, f, a))
	if len(held) != 2 {
		t.Fatalf("file A holds %d held entries after a sync over its deallocation, want the two ranges beside the hole: %+v", len(held), held)
	}

	// B: the hole across three records, exact.
	r = read(t, hb, b, 0, 3*chunk)
	wantMissing(t, r, 0, []journalv2.Extent{ext(chunk/2, 2*chunk)}, "the hole cut across records")
	if !bytes.Equal(r.p[:chunk/2], pb[:chunk/2]) || !bytes.Equal(r.p[5*chunk/2:], pb[5*chunk/2:]) {
		t.Fatalf("the ranges beside the hole across records were not served as they were")
	}

	if cs := since(t, ha, a, va); !sameChanges(cs, []change{{journalv2.ChangeDeallocate, ext(chunk, chunk), vda}}) {
		t.Fatalf("Since yielded %+v for the deallocation inside one record, want its exact extent at the deallocate's own version", cs)
	}
	if cs := since(t, hb, b, vb); !sameChanges(cs, []change{{journalv2.ChangeDeallocate, ext(chunk/2, 2*chunk), vdb}}) {
		t.Fatalf("Since yielded %+v for the deallocation across records, want its exact extent at the deallocate's own version", cs)
	}

	f.Crash()
	ha, hb = f.Share(0), f.Share(1)
	r = read(t, ha, a, 0, 3*chunk)
	wantMissing(t, r, 0, []journalv2.Extent{ext(chunk, chunk)}, "the hole cut inside one record, after a crash")
	r = read(t, hb, b, 0, 3*chunk)
	wantMissing(t, r, 0, []journalv2.Extent{ext(chunk/2, 2*chunk)}, "the hole cut across records, after a crash")
}

// TestEdge_FillOverHeldDirtyAndAbsent checks that a fill over a range one
// part offloaded, one part dirty and one part absent writes only the absent
// part, sets its offloaded bit there, and leaves the held parts serving their
// own bytes.
func TestEdge_FillOverHeldDirtyAndAbsent(t *testing.T) {
	const chunk = 4096
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		clean := distinct(k, chunk)
		dirty := distinct(5+k, chunk)
		v := write(t, h, id, 0, clean)
		mark(t, h, id, ext(0, chunk), v)
		write(t, h, id, chunk, dirty)

		dv := draw(t, f)
		fillP := pattern(fmt.Sprint("fill", k), 3*chunk)
		asOf := read(t, h, id, 0, 3*chunk).asOf
		filled := f.J.Stats().Counters.FilledBytes
		if err := journalv2test.RetrySpare(func() error { return h.Fill(t.Context(), id, 0, fillP, asOf, dv) }); err != nil {
			stub(t, "Fill", err)
			t.Fatalf("share %d: fill over held, dirty and absent bytes: %v", k, err)
		}

		r := read(t, h, id, 0, 3*chunk)
		wantServed(t, r, slices.Concat(clean, dirty, fillP[2*chunk:]), "the fill over the three states of content")
		wantBits(t, f, id, ext(0, chunk), true, "the extent offloaded before the fill")
		wantBits(t, f, id, ext(chunk, chunk), false, "the extent held dirty before the fill")
		wantBits(t, f, id, ext(2*chunk, chunk), true, "the extent the fill placed")
		if got := f.J.Stats().Counters.FilledBytes - filled; got != chunk {
			t.Fatalf("share %d: FilledBytes rose by %d over a fill of %d absent bytes, want %d", k, got, chunk, chunk)
		}
	}
}

// TestEdge_OffloadReportsUnofferedOrNone checks that an offload's report
// names only what the offer held: one outside it is rejected and marks
// nothing, a report of nothing marks nothing, a partial report before an
// error is honoured, and a report after the callback has returned fails.
func TestEdge_OffloadReportsUnofferedOrNone(t *testing.T) {
	const size = 64 << 10
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		v1 := write(t, h, id, 0, pattern(fmt.Sprint("o1", k), size))
		v2 := write(t, h, id, size, pattern(fmt.Sprint("o2", k), size))

		// Leg 1: a report of an extent the offer did not carry. The offer
		// stops at the first write, so the second extent is not offered.
		offered, outside := ext(0, size), ext(size, size)
		err := h.Offload(t.Context(), id, v1, journalv2test.FixtureMaxBytes, 0, func(_ context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
			if os := coalesce(o.Dirty); len(os) != 1 || os[0].Extent != offered {
				return fmt.Errorf("share %d: the offer through the first write carried %+v, want the first extent alone", k, o.Dirty)
			}
			if err := report([]journalv2.Extent{outside}); !errors.Is(err, journalv2.ErrNotOffered) {
				return called("report", err)
			}
			return nil
		})
		stub(t, "Offload", err)
		if err != nil {
			t.Fatalf("share %d: the offload whose report was rejected: %v", k, err)
		}
		wantBits(t, f, id, offered, false, "the offered extent after a rejected report")
		wantBits(t, f, id, outside, false, "the extent outside the offer after a rejected report")

		// Leg 2: a callback that reports nothing and returns nil.
		before := f.J.Stats()
		offload(t, h, id, v2, journalv2test.FixtureMaxBytes, 0, func(_ journalv2.Offer, _ func([]journalv2.Extent) error) error { return nil })
		s := f.J.Stats()
		wantBits(t, f, id, offered, false, "the offered extent after a report of nothing")
		if s.Counters.MarkedBytes != before.Counters.MarkedBytes {
			t.Fatalf("share %d: MarkedBytes rose by %d over a report of nothing", k, s.Counters.MarkedBytes-before.Counters.MarkedBytes)
		}
		if s.DirtyBytes != before.DirtyBytes {
			t.Fatalf("share %d: DirtyBytes moved by %d over a report of nothing", k, s.DirtyBytes-before.DirtyBytes)
		}

		// Leg 3: a report of the first half of the first offered extent —
		// the offer through v2 carries [0,64K) at v1 and [64K,128K) at v2,
		// and coalesce keeps them apart — then the callback returns its own
		// error.
		own := errors.New("the upload failed")
		err = h.Offload(t.Context(), id, v2, journalv2test.FixtureMaxBytes, 0, func(_ context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
			os := coalesce(o.Dirty)
			if len(os) == 0 {
				return fmt.Errorf("share %d: the offer through the second write carried nothing", k)
			}
			shard := ext(os[0].Extent.Off, os[0].Extent.Len/2)
			if err := report([]journalv2.Extent{shard}); err != nil {
				return called("report", err)
			}
			return own
		})
		stub(t, "Offload", err)
		if !errors.Is(err, own) {
			t.Fatalf("share %d: the offload that reported a shard returned %v, want the callback's own error", k, err)
		}
		wantBits(t, f, id, ext(0, size/2), true, "the shard reported before the callback failed")
		wantBits(t, f, id, ext(size/2, size), false, "the rest of the first extent, never reported")
		wantBits(t, f, id, ext(size, size), false, "the second extent, beyond this report's shard")

		// Leg 4: a report after the callback has returned.
		var late func([]journalv2.Extent) error
		err = h.Offload(t.Context(), id, v2, journalv2test.FixtureMaxBytes, 0, func(_ context.Context, _ journalv2.Offer, report func([]journalv2.Extent) error) error {
			late = report
			return nil
		})
		stub(t, "Offload", err)
		if err != nil {
			t.Fatalf("share %d: the offload whose report was kept: %v", k, err)
		}
		if late == nil {
			t.Fatalf("share %d: the offload returned without running its callback, so no report was kept to call late", k)
		}
		if err := late([]journalv2.Extent{ext(0, size)}); !errors.Is(err, journalv2.ErrOfferEnded) {
			t.Fatalf("share %d: a report after the offer ended returned %v, want ErrOfferEnded", k, err)
		}
	}
}

// TestEdge_ReleasePartlyOffloaded checks that a release of a range only part
// of which is offloaded is refused naming exactly the extent that is not,
// that it makes no partial progress — nothing is released, no counter moves —
// and that releasing the offloaded part alone then succeeds.
func TestEdge_ReleasePartlyOffloaded(t *testing.T) {
	const size = 8192
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := pattern(fmt.Sprint("rel", k), size)
		v := write(t, h, id, 0, p)
		syncFile(t, h, id)
		mark(t, h, id, ext(0, size/2), v)

		before := f.J.Stats()
		err := h.Release(t.Context(), id, []journalv2.Extent{ext(0, size)})
		wantNotOffloaded(t, err, id, []journalv2.Extent{ext(size/2, size/2)}, "a release of a half-offloaded range")

		got, missing := readBack(t, h, id, 0, size)
		if len(missing) != 0 || !bytes.Equal(got, p) {
			t.Fatalf("share %d: the refused release left the range missing %+v or other bytes", k, missing)
		}
		after := f.J.Stats()
		if after.Counters.ReleasedBytes != before.Counters.ReleasedBytes {
			t.Fatalf("share %d: ReleasedBytes rose by %d over a refused release", k, after.Counters.ReleasedBytes-before.Counters.ReleasedBytes)
		}
		if after.UnreclaimedBytes != before.UnreclaimedBytes {
			t.Fatalf("share %d: UnreclaimedBytes moved by %d over a refused release", k, after.UnreclaimedBytes-before.UnreclaimedBytes)
		}

		release(t, h, id, ext(0, size/2))
		wantExtentMissing(t, h, id, ext(0, size/2), "the offloaded half, released alone")
		wantBytes(t, h, id, size/2, p[size/2:], "the half a refused release left held")
	}
}

// TestEdge_ReleaseMiddleEndsAndAll checks that a record's extents released
// in four calls — middle, far end, head, other middle — read missing the
// moment each is released while the rest serves, move UnreclaimedBytes by
// the released length and keep the segment listed and its storage used,
// until nothing is held and a sync settles it; and that every hole survives
// a crash.
func TestEdge_ReleaseMiddleEndsAndAll(t *testing.T) {
	const size = 16 << 10
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := pattern(fmt.Sprint("mid", k), size)
		v := write(t, h, id, 0, p)
		syncFile(t, h, id)
		mark(t, h, id, ext(0, size), v)

		seg := segmentOf(t, f, id, 0)
		name := segmentName(seg)
		usedBefore := f.J.Stats().UsedBytes
		var missing []journalv2.Extent
		for _, step := range []struct {
			e  journalv2.Extent
			at string
		}{
			{ext(4096, 4096), "the middle released first"},
			{ext(3*4096, 4096), "the far end released second"},
			{ext(0, 4096), "the head released third"},
			{ext(2*4096, 4096), "the other middle released last"},
		} {
			before := f.J.Stats()
			release(t, h, id, step.e)
			missing = joined(append(missing, step.e))

			r := read(t, h, id, 0, size)
			wantMissing(t, r, 0, missing, step.at)
			if got := f.J.Stats().UnreclaimedBytes - before.UnreclaimedBytes; got != step.e.Len {
				t.Fatalf("share %d: %s: UnreclaimedBytes rose by %d, want the released %d", k, step.at, got, step.e.Len)
			}
			if after := f.J.Stats().UsedBytes; after < usedBefore {
				t.Fatalf("share %d: %s: UsedBytes fell from %d to %d while the record's releases are not durable", k, step.at, usedBefore, after)
			}
			if _, ok := segmentInfo(f, seg); !ok {
				t.Fatalf("share %d: %s: segment %d left the journal before its releases were made durable", k, step.at, seg)
			}
			if f.Seam.CallsFor(journalv2test.OpRemove, name)+f.Seam.CallsFor(journalv2test.OpRename, name) > 0 {
				t.Fatalf("share %d: %s: segment %d was retired before its releases were made durable", k, step.at, seg)
			}
		}

		syncFile(t, h, id)
		if s := f.J.Stats().Shares[journalv2test.Tag(k)]; s.HeldBytes != 0 {
			t.Fatalf("share %d: HeldBytes = %d after every extent of the file was released, want 0", k, s.HeldBytes)
		}
		es, err := h.Extents(id)
		stub(t, "Extents", err)
		if err != nil {
			t.Fatalf("share %d: list the file's extents after every release: %v", k, err)
		}
		if len(es) != 0 {
			t.Fatalf("share %d: Extents = %+v after every extent of the file was released, want none", k, es)
		}

		f.Crash()
		h = f.Share(k)
		r := read(t, h, id, 0, size)
		wantMissing(t, r, 0, missing, "every released range, after a crash")
	}
}

// TestEdge_FullyReleasedSegmentReclaimedWithoutRead checks that a segment
// whose every record is released is retired on its own, once its releases
// are durable, without a repack and without one read of it: the segment
// leaves the journal's set, the accounted storage moves by its path's rule
// (an unlink falls by the segment's account, a recycle leaves it where it
// was), and the freed counter is exactly the account on either path.
func TestEdge_FullyReleasedSegmentReclaimedWithoutRead(t *testing.T) {
	f := journalv2test.New(t)

	h, id := f.Share(0), journalv2test.File(0)
	recs, seg := fillSegment(t, f, 0, 0, 64<<10)
	for _, r := range recs {
		mark(t, f.Share(r.share), r.id, r.extent, r.version)
	}
	syncFiles(t, h, id)
	// A record on the other stream, stayed held, so neither share is empty.
	writeSynced(t, f, 1, 1, 0, 64<<10)

	name := segmentName(seg)
	before := f.J.Stats()
	reads := f.Seam.CallsFor(journalv2test.OpRead, name) + f.Seam.CallsFor(journalv2test.OpReadDirect, name)
	info, _ := segmentInfo(f, seg)
	// The snapshot of the accounted storage settles before it is read, so a
	// spare refill racing the snapshot is waited out, not read mid-flight.
	wantAccounted(t, f, "before the releases")
	beforeUsed := f.J.Stats().UsedBytes
	createsBefore := f.Seam.Calls(journalv2test.OpCreate)

	for _, r := range recs {
		release(t, h, r.id, r.extent)
	}
	syncFiles(t, h, id)
	eventually(t, "a segment released by every record of it was not retired without a repack", func() bool { return retired(f, seg) })

	if got := f.Seam.CallsFor(journalv2test.OpRead, name) + f.Seam.CallsFor(journalv2test.OpReadDirect, name); got != reads {
		t.Fatalf("%d reads of segment %d over its releases and retirement, want the %d before: it was retired through a read", got-reads, seg, reads)
	}
	if _, ok := segmentInfo(f, seg); ok {
		t.Fatalf("segment %d is still owned after every record of it was released", seg)
	}
	// The retire is one of two paths, and both are visible at the seam: the
	// retire unlinks the segment's name - a recycle that fails lands there
	// too, its fallback unlink - or renames it to a fresh one for the spare
	// pool. The accounted storage is checked per path and the freed counter
	// is the segment's account on either.
	//
	// With one retire and nothing held, the path is read from the calls the
	// seam saw on the old name: a rename with no unlink is the recycle, any
	// unlink the unlink path, a recycle's fallback included.
	recycled := f.Seam.CallsFor(journalv2test.OpRename, name) > 0 && f.Seam.CallsFor(journalv2test.OpRemove, name) == 0
	wantRetireAccounted(t, f, "a retire without a repack", seg, beforeUsed, info.Allocated, createsBefore, recycled)
	wantFreed(t, f, "a retire without a repack", before.Counters.FreedBytes, uint64(info.Allocated))
}

// TestEdge_ReservationExactFitShare checks that a reservation sized to the
// last room a limited share holds is taken, and one record's bytes over is
// refused at once naming the share's limit and the bytes missing, while the
// unlimited share beside it still writes.
func TestEdge_ReservationExactFitShare(t *testing.T) {
	const limit = 4 << 20
	// Tags clear of the fixture's own shares and of the limited tags the
	// other checks attach, so each share's accounting is read under the
	// tag its own handle attached. Tag is a function, so these sit in vars.
	var (
		limitedEdgeA = journalv2test.Tag(8)
		limitedEdgeB = journalv2test.Tag(9)
	)
	f := journalv2test.New(t)

	b := f.Share(1)
	a := limitedShare(t, f, limitedEdgeA, limit)

	// The share is filled on the fresh file alone, every write a whole
	// record in one piece: the payload is the by-eight floor of the smaller
	// of the room the share's limit holds and the room the segment's tail
	// holds, less the record's own header and time - a charge padded to a
	// whole unit of eight, so the room the fills walk down through stays a
	// multiple of eight and reaches one record's charge exactly. A record
	// that does not fit the tail is cut at the cap into one record per
	// segment, and each piece charges its own header and time against the
	// share, bytes no room-shaped ask names; sizing every fill to the tail
	// keeps the pieces whole. The tail is measured from the run's own
	// ending in the index, and a tail too small for any record means the
	// next one rolls, so the loop steers.
	// The tail's room for one more record is smaller than the cap's
	// remainder shows: a segment cannot seal until its footer names every
	// record in it, so the record's charge must also fit beside the seal
	// marker, the footer's entries and the trailer, or the journal cuts it
	// at the cap and each piece charges the share its own header and time -
	// bytes the room-shaped asks below do not name. Measuring every fill
	// against the sealed tail keeps the pieces whole.
	segMargin := int64(raw.SealMarkerSize) + raw.TrailerSize + 4*raw.CatalogEntrySize

	fit := journalv2test.File(40)
	capB := int64(journalv2test.FixtureSegmentCap)
	at := int64(0)
	var tail int64
	for {
		room := limit - f.J.Stats().Shares[limitedEdgeA].UsedBytes
		if at > 0 {
			last := entryAt(t, f, fit, at-1)
			tail = last.Location.Offset + writeRecordBytes(int(last.Extent.Len))
		} else {
			tail = 0 // a fresh file: its first record lands wherever its stream stands
		}
		tailroom := capB - tail%capB - segMargin
		if tailroom < writeRecordBytes(1) || tail == 0 {
			tailroom = capB - segMargin // the next record cannot fit: it rolls into a fresh segment
		}
		if tailroom >= room && room >= writeRecordBytes(1) {
			break // one record's charge is the whole room left: the exact fit
		}
		p := (min(room, tailroom) - raw.RecordHeaderSize - 8) &^ 7
		if p < 4096 {
			// The smaller side holds less than a 4096-byte record's room, so
			// a 4096-byte fill would leave less than two minimal records
			// on one side: the fill instead leaves room for two minimal
			// records, keeping the endgame reachable.
			if p < 8 || tailroom < 2*writeRecordBytes(1) {
				t.Fatalf("the fills left %d of room with a %d tail, no state one record fills", room, tailroom)
			}
			p = max(8, min(room, tailroom)-2*raw.RecordHeaderSize) &^ 7
		}
		if room-writeRecordBytes(int(p)) < 0 || tailroom-writeRecordBytes(int(p)) < 0 {
			t.Fatalf("precondition: a %d-byte fill over %d of room and a %d tail left the share in no state one record fills", p, room, tailroom)
		}
		write(t, a, fit, at, pattern(fmt.Sprint("fillfit", at), int(p)))
		at += int64(p)
	}
	room := limit - f.J.Stats().Shares[limitedEdgeA].UsedBytes

	// The exact fit is the room itself: the record's charge is its payload
	// and its header and time, one piece in the segment whose tail the loop
	// measured to hold it.
	n := room - raw.RecordHeaderSize - 8
	r := reserve(t, a, writeRecordBytes(int(n)), 1)
	_, err := r.WriteAt(t.Context(), fit, at, pattern("fit", int(n)), 0)
	stub(t, "Reservation.WriteAt", err)
	if err != nil {
		t.Fatalf("a write through a reservation of the share's last room: %v", err)
	}
	if got := f.J.Stats().Shares[limitedEdgeA].UsedBytes; got != limit {
		t.Fatalf("the share's UsedBytes is %d after the reserved write, want its limit %d", got, limit)
	}

	// One padded unit over the smallest record-size step is refused at once,
	// with no call reaching a segment.
	err = refusedAtOnce(t, f, "a reservation over a full share", func() error {
		_, err := a.Reserve(writeRecordBytes(1)+8, 1)
		return err
	})
	ns := refusal(t, err, "a reservation over a full share", journalv2.LimitShare, journalv2.UnitBytes)
	if ns.Missing != writeRecordBytes(1)+8 {
		t.Fatalf("the refusal names %d bytes missing, want exactly the ask of %d", ns.Missing, writeRecordBytes(1)+8)
	}

	// One byte over directly, on a fresh share: the missing count is the
	// padded difference, at most one record unit over the ask's tail.
	fresh := limitedShare(t, f, limitedEdgeB, 4<<20)
	_, _ = fillShare(t, fresh, []journalv2.FileID{journalv2test.File(30), journalv2test.File(31)}, 4096)
	freshRoom := 4<<20 - f.J.Stats().Shares[limitedEdgeB].UsedBytes
	if err := refusedAtOnce(t, f, "a reservation a byte over a full share", func() error {
		_, err := fresh.Reserve(freshRoom+1, 1)
		return err
	}); err == nil {
		t.Fatal("a reservation one byte over the limit was accepted")
	} else {
		if ns := refusal(t, err, "a reservation one byte over a full share", journalv2.LimitShare, journalv2.UnitBytes); ns.Missing < 1 || ns.Missing > 8 {
			t.Fatalf("a reservation one byte over names %d bytes missing, want the byte, padded to at most a unit", ns.Missing)
		}
	}

	v := write(t, b, journalv2test.File(8), 0, pattern("unlimited", 4096))
	if v == (journalv2.Version{}) {
		t.Fatalf("the unlimited share's write drew no version")
	}
}

// TestEdge_ReservationExactFitJournal checks that an ask past the room,
// under any accounting of it, is refused at once naming the journal's limit,
// that the ask less its own missing count is taken and closed, and that the
// same ask is refused again once nothing is held - the boundary bracketed
// between the ask that fit and the ask that did not, without pinning where
// between them the journal puts it. Once the fills reach the maximum, a
// reservation one record past it is refused at once naming the journal's
// limit and a positive shortfall, without reaching a segment.
func TestEdge_ReservationExactFitJournal(t *testing.T) {
	const fill = 256 << 10
	capB := int64(journalv2test.FixtureSegmentCap)
	// A maximum a whole number of segment caps, so the space left below it
	// is whole segments or none.
	//
	// decision: the pool target is raised here because a reservation takes
	// spares of its own - one per cap its bytes can cross plus one - from
	// the pool's stock, which itself runs at the fixture's
	// one-spare-per-stream target and starves an ask that spans caps. The
	// asks below are sized past the pool as well, one standing in for the
	// journal's whole maximum, because nothing pins how a journal reads
	// its pooled, already-allocated spares against a reservation's bytes:
	// one that charges only newly created segments counts them as room,
	// and an ask of the remaining space under that reading is taken. The
	// take reads the missing count as everything the ask lacked in the
	// unit it names - bytes and the pieces' entries, or the whole segments
	// the ask was short of, converted at the segment cap, the only
	// whole-segment grain a maximum that bounds whole segments accounts
	// its room in - so returning the count leaves an ask the journal
	// itself sized to fit, its spares included where its unit counts
	// them. Overturned when a refusal counts its own unit alone while a
	// second lack - the pool - also refuses the ask.
	//
	// decision: the boundary is bracketed, not pinned. A refusal that
	// names whole segments counts the segments the write needs, converted at
	// the cap, not its record's bytes, and the remaining room is not
	// observable exactly while Stats reports no spare count, so the step
	// between an ask of exactly the remaining space and one a byte over it is
	// not checked: a journal that over-accepts by less than the bracket's
	// several caps passes. Overturned once Stats carries the spare count,
	// when UsedBytes less the spares at the cap names the room exactly.
	f := journalv2test.NewWith(t, func(s *journalv2.Settings) {
		s.MaxBytes = 16 * capB
		s.SpareTarget = 8
	})

	a := f.Share(0)
	// One stream alone, and the account settled before every read below:
	// an unsettled read buys no size, so every loop reads the account
	// after its settles.
	id := journalv2test.File(10)
	var at int64
	for range 256 {
		write(t, a, id, at, pattern(fmt.Sprint("fill", at), fill))
		at += fill
		wantAccounted(t, f, "over one fill write")
		if s := f.J.Stats(); s.MaxBytes-s.UsedBytes <= 2*capB {
			break // settled within one spare draw of the maximum
		}
	}
	wantAccounted(t, f, "before the asks near the maximum")

	// The ask is the journal's whole maximum, held against nothing: bigger
	// than the room under any reading of it - smaller or larger by a
	// stocked pool - and bigger than the pool can spare for it, so the
	// journal's limit is the limit that refuses it, never the pool, which
	// a refusal names only when no other limit would also refuse the ask.
	maxB := f.J.Stats().MaxBytes
	err := refusedAtOnce(t, f, "a reservation of the journal's whole maximum", func() error {
		_, err := a.Reserve(maxB, 1)
		return err
	})
	ns := refusal(t, err, "a reservation of the journal's whole maximum", journalv2.LimitJournal, journalv2.UnitBytes, journalv2.UnitSegments)

	// The ask less its own missing count is the ask that fit - a count is
	// what the ask lacked, so returning it leaves nothing lacking - and it
	// is closed at once, holding nothing. The same ask is then made again
	// and must be refused as it was: the room it was refused against can
	// only have shrunk or stayed, for a take closed holding nothing
	// returns what it took. Together the two bracket the boundary in
	// (exact, ask] without pinning where between them the journal charges
	// it - the charge steps at every cap its bytes cross, so no fixed
	// padding above the boundary holds on every step.
	missingBytes := ns.Missing
	if ns.Unit == journalv2.UnitSegments {
		missingBytes *= capB
		if missingBytes >= maxB {
			t.Fatalf("a reservation of %d bytes names %d segments missing, want fewer than the ask's own whole caps", maxB, ns.Missing)
		}
	}
	exact := maxB - missingBytes
	if exact <= 0 {
		t.Fatalf("the refusal named %d %v missing of a %d-byte ask, leaving no ask that fit", ns.Missing, ns.Unit, maxB)
	}
	r := reserve(t, a, exact, 1)
	if err := r.Close(); err != nil {
		stub(t, "Reservation.Close", err)
		t.Fatalf("closing the reservation sized to what the refusal itself named: %v", err)
	}
	over := journalv2test.RetrySpare(func() (err error) {
		_, err = a.Reserve(maxB, 1)
		return err
	})
	if over == nil {
		t.Fatal("the maximum-sized ask was taken after the ask less its count had been returned")
	}
	refusal(t, over, "the maximum-sized ask a second time", journalv2.LimitJournal, journalv2.UnitBytes, journalv2.UnitSegments)

	// The fills go on to the maximum itself: the last spare draw the
	// maximum allows lands, the write past it is refused naming the
	// journal's limit, and the account settles at the maximum.
	refused := false
	for range 2 * maxB / fill {
		_, err := tryWrite(t, a, id, at, pattern(fmt.Sprint("fill", at), fill))
		stub(t, "WriteAt", err)
		if err != nil {
			wantNoSpace(t, err, journalv2.LimitJournal, "a write past the journal's maximum")
			refused = true
			break
		}
		at += fill
	}
	if !refused {
		t.Fatalf("writes went on past the journal's maximum of %d", maxB)
	}
	wantAccounted(t, f, "once the fills reached the maximum")
	s := f.J.Stats()
	atCapacityMax(t, s, capB)
	if room := s.MaxBytes - s.UsedBytes; room < 0 || room >= capB || room%capB != 0 {
		t.Fatalf("precondition: %d bytes used of a %d maximum left %d, want whole segments short of one or none", s.UsedBytes, s.MaxBytes, room)
	}

	// A reservation one record past the maximum is refused at once, with
	// no call reaching a segment. The journal's maximum is the limit that
	// refuses it - not the spare pool, which a refusal names only when no
	// other limit would also refuse the ask - and the shortfall it counts
	// is the shortfall in the journal's own unit, bytes or the segments
	// the record's bytes need, never a negative or zero count.
	err = refusedAtOnce(t, f, "a reservation one record past the journal's maximum", func() error {
		_, err := a.Reserve(writeRecordBytes(1)+8, 1)
		return err
	})
	refusal(t, err, "a reservation one record past the journal's maximum", journalv2.LimitJournal, journalv2.UnitBytes, journalv2.UnitSegments)
}

// TestEdge_FilesystemENOSPCIsNotErrNoSpace checks that a write refused by the
// filesystem holding the journal reports the filesystem's own ENOSPC, not the
// journal's no-space refusal, and that the fault cleared the journal writes
// again, the journal's own maximum never reached.
func TestEdge_FilesystemENOSPCIsNotErrNoSpace(t *testing.T) {
	const chunk = 256 << 10
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(5)

	// Fill the stream's segment until it holds less than the next write's
	// record, so a fresh segment is what that write needs — with every write
	// required to succeed, since no fault is installed yet and the journal
	// is far below its own maximum.
	at := fillToTail(t, h, f, id, "near", chunk, writeRecordBytes(chunk))

	// The device fills: every write from here that rolls takes a spare the
	// pool already holds ready and succeeds, so the writes go on until the
	// pool is dry and a roll must make a segment the device refuses. That
	// write must surface the filesystem's ENOSPC, never the journal's own
	// no-space refusal — and it reaches there below the journal's maximum,
	// since it is spares the writes were taking, not records refused.
	fullDevice(t, f)
	//
	// decision: a roll refused for want of a spare before the off-path refill
	// has failed is retried, the clock moving between attempts, because the
	// refusal naming the spare pool is the right answer until making a spare
	// has failed with ENOSPC, and the writes cannot see when that happens.
	// The first refusal that is not the spare pool's is the
	// one asserted. Not checked: that the spare refusal ever turns into the
	// filesystem's; the bounded loop reports a journal that never does.
	// Overturned once a seam point names the refill's failure.
	var err error
	for range 2 * journalv2test.FixtureMaxBytes / chunk {
		_, err = tryWrite(t, h, id, at, pattern("over", chunk))
		var ns *journalv2.NoSpaceError
		if errors.As(err, &ns) && ns.Limit == journalv2.LimitSpare {
			f.Advance(journalv2.SyncBound)
			continue
		}
		if err != nil {
			break
		}
		at += chunk
	}
	wantENOSPC(t, err, "a content write on a full device")
	if s := f.J.Stats(); s.UsedBytes >= s.MaxBytes {
		t.Fatalf("the write the device refused sat at the journal's maximum of %d rather than below it: UsedBytes %d", s.MaxBytes, s.UsedBytes)
	}

	f.Seam.SetFault(nil)
	// The device is whole again, but the journal has not made a spare yet:
	// the very next roll is refused the way the fault refused it, and that
	// refusal is what makes the refill run. Both refusals a cleared fault
	// leaves - the spare pool's own, and the raw ENOSPC while the refill the
	// cleared device now serves has not landed - are required reading, and
	// both clear only once the refill lands, so the write is retried over
	// each, the clock moving between attempts. Spares are made off the write
	// path, so nothing else can land them.
	//
	// decision: a roll refused for want of a spare and a roll still refused
	// with the filesystem's ENOSPC are both retried, because both last only
	// until the off-path refill lands and the cleared device serves the
	// refill it retries - a journal that waits out its own timer is given
	// the passes the fixture's clock serves, and one that retries at once
	// is served by the attempts themselves. What retries cannot clear is
	// the ENOSPC persisting through every attempt, and that is the failure
	// the bounded loop reports. Overturned once a seam point names a retry
	// the journal schedules.
	var cleared error
	for range 2 * journalv2test.FixtureMaxBytes / (64 << 10) {
		cleared = journalv2test.RetrySpare(func() (err error) {
			_, err = tryWrite(t, h, id, at, pattern("after", 64<<10))
			return err
		})
		var ns *journalv2.NoSpaceError
		retry := (errors.As(cleared, &ns) && ns.Limit == journalv2.LimitSpare) || errors.Is(cleared, syscall.ENOSPC)
		if !retry {
			break // landed, or a refusal neither clear reading leaves
		}
		// The refusal is one the refill clears: the failed attempt appended
		// nothing, so the next one writes at the same offset, and the clock
		// moves so a refill waiting on its own timer is served too.
		f.Advance(journalv2.SyncBound)
	}
	stub(t, "WriteAt", cleared)
	if errors.Is(cleared, syscall.ENOSPC) {
		t.Fatalf("the device refused a write after the fault was cleared: %v", cleared)
	}
	if cleared != nil {
		t.Fatalf("a write after the device fault was cleared: %v", cleared)
	}
	if s := f.J.Stats(); s.UsedBytes >= s.MaxBytes {
		t.Fatalf("the journal reached its own maximum of %d with the device refusing: UsedBytes %d", s.MaxBytes, s.UsedBytes)
	}
}

// TestEdge_SyncFailsThenSucceeds checks that a sync failing with the device's
// error is reported as a failed sync, that the write after it lands in a
// different segment and syncs clean, that the write before it is kept whole
// or lost whole, and that a crash keeps the synced bytes and the other
// stream's untouched.
func TestEdge_SyncFailsThenSucceeds(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		// Every iteration holds a fresh pair of files: an iteration's failed
		// window must not hold bytes an earlier iteration wrote and synced to
		// the same address, because a journal that fails windows rather than
		// re-appending them would then serve those older bytes again.
		h, id := f.Share(k), journalv2test.File(2*k)
		p1 := pattern(fmt.Sprint("failed", k), 4096)
		v1 := write(t, h, id, 0, p1)
		seg := segmentOf(t, f, id, 0)
		// The other stream's file is written before the failure: a file with
		// no write in the failed window is not told of it, and its own Sync
		// after the failure returns clean. Which stream shares a segment
		// with which file is the journal's own, so the file is chosen by
		// probing: the first write on the other share whose record lands
		// outside the failed segment, up to eight files, is the one; a
		// journal that never puts one there, however many files it is given,
		// keeps every write on the one stream and fails right here at the
		// precondition, not at a leg below.
		ho := f.Share(1 - k)
		var other journalv2.FileID
		var po []byte
		var rejected []journalv2.FileID
		var rejectedAt = map[journalv2.FileID]journalv2.Version{}
		for cand := range 8 {
			cid := journalv2test.File(2*k + 1 + 32*cand)
			cp := pattern(fmt.Sprint("other", k, cand), 4096)
			v := write(t, ho, cid, 0, cp)
			if segmentOf(t, f, cid, 0) != seg {
				other, po = cid, cp
				break
			}
			// The candidate stayed in the failed segment unsynced: it sits
			// inside the window, and on the fail path its write is lost
			// with the window's own, so its version is kept to be named
			// among the loss events as exactly that write.
			rejected = append(rejected, cid)
			rejectedAt[cid] = v
		}
		if other == (journalv2.FileID{}) {
			t.Fatalf("share %d: no write on the other share landed outside segment %d across eight files", k, seg)
		}

		s := f.J.Stats()
		failures, gen, reappended := s.Counters.SyncFailures, s.LossGeneration, s.Counters.SyncReappendedBytes
		beforeFail := s

		f.FailNext(journalv2test.OpSync, segmentName(seg), 1)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		// The failed window resolves one of two ways, and the Sync's return
		// names which: re-appended, its writes' Sync calls wait for the new
		// sync and return nil; failed, this Sync returns the failure. The
		// two legs below each require the whole of their half.
		if err != nil && (!errors.Is(err, journalv2.ErrSyncFailed) || !errors.Is(err, syscall.EIO)) {
			t.Fatalf("share %d: a Sync whose segment failed with EIO returned %v, want ErrSyncFailed wrapping it or nil over a re-append", k, err)
		}
		if err != nil {
			if f.J.Stats().LossGeneration <= gen {
				t.Fatalf("share %d: a Sync that failed the window returned the error without the loss generation rising", k)
			}
		}
		if err := ho.Sync(t.Context(), other); err != nil {
			stub(t, "Sync", err)
			t.Fatalf("share %d: the other stream's Sync beside a failed window: %v", k, err)
		}

		s = f.J.Stats()
		if s.Counters.SyncFailures <= failures {
			t.Fatalf("share %d: SyncFailures did not rise over a sync that failed", k)
		}
		if s.LastSyncError == nil || !errors.Is(s.LastSyncError.Err, syscall.EIO) {
			t.Fatalf("share %d: LastSyncError is %+v after a sync failed with EIO, want it carrying the error", k, s.LastSyncError)
		}

		p2 := pattern(fmt.Sprint("after", k), 4096)
		write(t, h, id, 64<<10, p2)
		next := segmentOf(t, f, id, 64<<10)
		if next == seg {
			t.Fatalf("share %d: the write after a failed sync was appended to the failed segment %d", k, seg)
		}
		syncs := f.Seam.CallsFor(journalv2test.OpSync, segmentName(next))
		if err := h.Sync(t.Context(), id); err != nil {
			stub(t, "Sync", err)
			t.Fatalf("share %d: the Sync after a failed one: %v", k, err)
		}
		if f.Seam.CallsFor(journalv2test.OpSync, segmentName(next)) == syncs {
			t.Fatalf("share %d: the Sync after a failed one returned without syncing segment %d, which holds the write", k, next)
		}

		// The Sync's return named the journal's own choice, so each leg below
		// requires its half whole: a nil means re-appended, and the write must
		// read back and the re-append counter must have risen with the loss
		// generation held; the error means failed, and the write must read
		// missing as a loss event naming exactly it, the generation risen.
		// What neither accepts is half of either.
		//
		// decision: a journal resolves a failed sync window by re-appending
		// it from a copy it kept or by failing it; the re-append waits the
		// writes' Sync calls on the new sync (nil) and the failure returns
		// the error, so the return names the choice, and the interface names
		// no way to set it. Overturned when the choice becomes settable.
		if err == nil {
			got, missing := readBack(t, h, id, 0, 4096)
			if len(missing) != 0 || !bytes.Equal(got, p1) {
				t.Fatalf("share %d: the re-appended window reads missing %+v or other bytes than written", k, missing)
			}
			if f.J.Stats().Counters.SyncReappendedBytes <= reappended {
				t.Fatalf("share %d: the failed window reads as kept without SyncReappendedBytes rising", k)
			}
			if f.J.Stats().LossGeneration != gen {
				t.Fatalf("share %d: LossGeneration rose although no write was lost", k)
			}
		} else {
			// The failed window holds the failed write and any probe write
			// that stayed in the segment, so the loss events name the
			// window's contents, each as its own event. Exactly one must
			// name the failed write at its extent and version; every
			// other must name a rejected candidate at its own write's
			// version - which of them the window held is the journal's
			// own layout, so any subset of them is held, but nothing
			// outside the failed write and the probes is a loss the
			// window can name.
			losses := f.J.Stats().Losses
			if len(losses) <= len(beforeFail.Losses) {
				t.Fatalf("share %d: %d loss events over a failed window, want at least the failed write's", k, len(losses)-len(beforeFail.Losses))
			}
			sawFailed := false
			for _, loss := range losses[len(beforeFail.Losses):] {
				if loss.File == id {
					if sawFailed || loss.Extent != (journalv2.Extent{Off: 0, Len: 4096}) || loss.Version != v1 {
						t.Fatalf("share %d: the failed write's loss events are %+v, want exactly the one at its extent and version", k, losses[len(beforeFail.Losses):])
					}
					sawFailed = true
					continue
				}
				if !slices.Contains(rejected, loss.File) || loss.Version != rejectedAt[loss.File] || loss.Extent != (journalv2.Extent{Off: 0, Len: 4096}) {
					t.Fatalf("share %d: a loss event of the failed window names %+v, want the failed write or a probe write it held at its own version", k, loss)
				}
			}
			if !sawFailed {
				t.Fatalf("share %d: the failed window's loss events hold no event for the failed write", k)
			}
			got, missing := readBack(t, h, id, 0, 4096)
			if !slices.Equal(joinedExtents(missing), []journalv2.Extent{{Off: 0, Len: 4096}}) {
				t.Fatalf("share %d: the write of the failed window reads %+v, want it missing as its whole extent", k, got)
			}
		}

		f.Crash()
		wantBytes(t, f.Share(k), id, 64<<10, p2, "the write synced after a failed sync, across a crash")
		wantBytes(t, f.Share(1-k), other, 0, po, "the other stream's write, across a crash")
	}
}

// TestEdge_RecordsWithoutBytesAtMaximum checks that a journal at its maximum
// still takes the records that carry no bytes — one truncate, one delete,
// one release with its mark, and an idle seal — none refused for space, each
// drawn on the headroom and each visible in what it changed, while the
// other share's file is untouched and still reads.
func TestEdge_RecordsWithoutBytesAtMaximum(t *testing.T) {
	f := journalv2test.New(t)
	const chunk = 256 << 10

	for k := range 2 {
		write(t, f.Share(k), journalv2test.File(10+k), 0, pattern(fmt.Sprint("pre", k), 64<<10))
		syncNow(t, f.Share(k), journalv2test.File(10+k))
	}
	write(t, f.Share(0), journalv2test.File(12), 0, pattern("deleted", 64<<10))
	ws := fillUntilRefused(t, f, []int{0, 1}, 0, chunk)
	h0, h1 := f.Share(0), f.Share(1)
	fillTails(t, []*journalv2.Handle{h0, h1}, 20, func(err error) {
		wantNoSpace(t, err, journalv2.LimitJournal, "a small write at the journal's maximum")
	})
	s := f.J.Stats()
	atCapacity(t, s)
	draws := s.Counters.HeadroomDraws

	try := func(call string, err error) {
		t.Helper()
		stub(t, call, err)
		if err != nil {
			t.Fatalf("%s at the journal's maximum: %v", call, err)
		}
	}

	_, err := h0.Truncate(t.Context(), journalv2test.File(10), 4096)
	try("Truncate", err)
	afterTruncate := f.J.Stats().Counters.HeadroomDraws
	if afterTruncate <= draws {
		t.Fatalf("HeadroomDraws did not rise over a truncate at the maximum")
	}
	_, err = h0.Delete(t.Context(), journalv2test.File(12))
	try("Delete", err)
	afterDelete := f.J.Stats().Counters.HeadroomDraws
	if afterDelete <= afterTruncate {
		t.Fatalf("HeadroomDraws did not rise over a delete at the maximum")
	}
	w := ws[0]
	try("MarkOffloaded", f.Share(w.share).MarkOffloaded(t.Context(), w.id, []journalv2.Extent{w.extent}, w.version, w.version))
	afterMark := f.J.Stats().Counters.HeadroomDraws
	// The bit is read while the extent is still held: the release below
	// takes the bytes with it, leaving nothing for a held-bytes check.
	wantBits(t, f, w.id, w.extent, true, "the extent marked at the maximum")
	try("Release", f.Share(w.share).Release(t.Context(), w.id, []journalv2.Extent{w.extent}))
	afterRelease := f.J.Stats().Counters.HeadroomDraws
	if afterRelease <= afterMark {
		t.Fatalf("HeadroomDraws did not rise over a release at the maximum")
	}

	sealed := f.J.Stats().Sealed
	f.Advance(2 * journalv2test.IdleSealAge)
	eventually(t, "no seal was made at the journal's maximum", func() bool { return f.J.Stats().Sealed > sealed })
	for _, seg := range f.J.Inspect().Segments() {
		if seg.Failed {
			t.Fatalf("segment %d failed an append at the journal's maximum", seg.ID)
		}
	}
	// The four draws above are each asserted as they happen; this bound is
	// only a coarse check that they were not the whole of what the headroom
	// served, and a seal qualifies only where a journal draws seal records
	// from the headroom at all - one that keeps its seal room inside each
	// segment draws nothing for the seal, so the seal is not read as owed
	// here.
	if got := f.J.Stats().Counters.HeadroomDraws - draws; got < 4 {
		t.Fatalf("HeadroomDraws moved by %d over a truncate, a delete, a mark and a release at the maximum, want at least the four of them", got)
	}

	// Each record's effect is visible: the truncated range and the released
	// one missing, the deleted file gone, the mark's bit set.
	r := read(t, h0, journalv2test.File(10), 0, 64<<10)
	wantMissing(t, r, 0, []journalv2.Extent{ext(4096, 64<<10-4096)}, "the range truncated at the maximum")
	wantMissingFrom(t, f.Share(w.share), w.id, w.extent)
	// A delete not yet settled keeps its removal marker in the index; what
	// the check can hold is that none of the file's entries reads as held.
	entries, err := f.J.Inspect().File(journalv2test.File(12))
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect the deleted file: %v", err)
	}
	if held := heldOnly(entries); len(held) != 0 {
		t.Fatalf("the deleted file still holds %+v after a delete at the maximum", held)
	}

	wantBytes(t, h1, journalv2test.File(11), 0, pattern("pre1", 64<<10), "the other share's file, beside records at the maximum")
	got, missing := readBack(t, h0, journalv2test.File(10), 0, 4096)
	if len(missing) != 0 || !bytes.Equal(got, pattern("pre0", 64<<10)[:4096]) {
		t.Fatalf("the range below the truncate reads missing %+v or other bytes %q", missing, got)
	}
}

// TestEdge_SegmentRetiredByEvictionAndRepack checks that once every record
// of a segment is released and those releases are durable, the segment gets
// exactly one retire whichever selector reaches it first: the holds that
// catch the retire are armed before the sync that makes the releases
// durable - from that moment the journal retires the segment on its own, so
// a repack opened later finds the same retire the journal's own reclamation
// runs, and the two converge on one unlink or recycle rather than two - the
// accounted storage moving by the rule of the path that landed, the double
// retire counted whenever the seam saw both selectors reach its file.
func TestEdge_SegmentRetiredByEvictionAndRepack(t *testing.T) {
	f := journalv2test.New(t)

	h, id := f.Share(0), journalv2test.File(0)
	s, es := soleSegment(t, f, h, id)
	seg := s.ID
	name := segmentName(seg)
	// A record on the other stream, stayed held, so the retire has a journal
	// to keep and no share is empty.
	writeSynced(t, f, 1, 1, 0, 64<<10)
	// The extents are released before anything is armed, so the repack
	// carries nothing forward through them and the segment is one only a
	// retire can take. The releases are not yet durable: the moment they
	// become so is the sync below, which is where the journal's own
	// retirement selects the segment, so the holds are armed before it and
	// the retire is caught whichever goroutine runs it.
	extents := make([]journalv2.Extent, 0, len(es))
	for _, e := range es {
		extents = append(extents, e.Extent)
	}
	if err := h.MarkOffloaded(t.Context(), id, extents, es[0].Version, es[len(es)-1].Version); err != nil {
		stub(t, "MarkOffloaded", err)
		t.Fatalf("mark the sole segment's extents offloaded: %v", err)
	}
	if err := h.Release(t.Context(), id, extents); err != nil {
		stub(t, "Release", err)
		t.Fatalf("release the sole segment's extents: %v", err)
	}

	// The retire parks at the first storage call it makes on the segment's
	// file, an unlink or a rename, so the moment it starts is named
	// whichever path it takes; only that one call is held, not every later
	// one, because a retire holds the segment exclusively from the moment
	// it starts and another retire may wait on it without ever reaching the
	// file.
	heldRemove := f.Seam.HoldFirst(t, journalv2test.OpRemove, name)
	heldRename := f.Seam.HoldFirst(t, journalv2test.OpRename, name)
	before := f.J.Stats()
	// The accounted storage settles before it is read, so a spare refill
	// racing the snapshot is waited out, not read mid-flight.
	wantAccounted(t, f, "before the two selectors ran")
	beforeUsed := f.J.Stats().UsedBytes
	createsBefore := f.Seam.Calls(journalv2test.OpCreate)

	// The sync that makes the releases durable is the journal's own
	// retirement selector: from its return the segment is one the journal
	// retires by itself, so it runs in a goroutine and the repack is opened
	// beside it the moment the retire parks. Which selector lands first is
	// the journal's own; both converge on the one retire the holds name.
	repacked := make(chan error, 1)
	synced := make(chan error, 1)
	go func() { synced <- h.Sync(t.Context(), id) }()
	// A retire always becomes eligible at the durability sync, but nothing
	// in the rule runs it inside that sync: a journal whose reclaimer runs
	// once Sync has returned is conforming, so the sync's return is only
	// recorded here - never taken as the retire having happened - and one
	// of the two holds on the segment's old name must still be reached,
	// whichever goroutine the retire runs on and whichever path it takes,
	// an unlink or a recycle rename, for both are a call on that name.
	//
	// landed is the call the retire that ran first parked on: with no fault
	// installed that call is the one that lands - a rename moves the file, a
	// remove deletes it - and any later call on the old name is the second
	// selector finding it gone, so the path is read from this call and not
	// from every call the seam counted.
	var landed string
	syncDone := false
	select {
	case <-heldRemove.Reached():
		landed = journalv2test.OpRemove
	case <-heldRename.Reached():
		landed = journalv2test.OpRename
	case err := <-synced:
		stub(t, "Sync", err)
		if err != nil {
			t.Fatalf("the sync making the releases durable returned %v, want nil: no fault is installed", err)
		}
		syncDone = true
		select {
		case <-heldRemove.Reached():
			landed = journalv2test.OpRemove
		case <-heldRename.Reached():
			landed = journalv2test.OpRename
		case <-time.After(backgroundWait):
			t.Fatalf("the sync returned and no retire reached the segment's file within %v, want one once the releases are durable", backgroundWait)
		}
	case <-time.After(backgroundWait):
		t.Fatalf("neither the durable-releases sync nor its retire reached the segment's file within %v", backgroundWait)
	}
	go func() { repacked <- repackSegment(t, f, seg) }()

	// Both holds are released without waiting. The one that fired lets the
	// parked retire through; the other never fires. The repack usually
	// arrives after the retire and finds the segment already gone, though
	// it may instead park behind the retire's exclusive hold: either way it
	// is let through here, whichever way the two retires land.
	heldRemove.Release()
	heldRename.Release()

	// Both selectors must finish once the holds are opened. The repack of a
	// segment whose records are all released selects and retires it, or
	// finds it already out: either way it returns nothing owed and no
	// error. The sync returns nil, no fault being installed, and it is
	// waited on only where it was not already taken above.
	select {
	case err := <-repacked:
		stub(t, "Repack", err)
		if err != nil {
			t.Fatalf("the repack beside the segment's reclamation: %v", err)
		}
	case <-time.After(backgroundWait):
		t.Fatalf("the repack did not return within %v beside the retire", backgroundWait)
	}
	if !syncDone {
		select {
		case err := <-synced:
			stub(t, "Sync", err)
			if err != nil {
				t.Fatalf("the sync beside the segment's retirement: %v, want nil: no fault is installed", err)
			}
		case <-time.After(backgroundWait):
			t.Fatalf("the sync did not return within %v beside the retire", backgroundWait)
		}
	}

	eventually(t, "the segment was not retired once its removal was let through", func() bool { return retired(f, seg) })

	after := f.J.Stats()
	// The retire is idempotent: a retire that finds the segment gone
	// subtracts nothing, counts as one double retire and never moves the
	// storage twice. Two selectors reached the segment when the seam saw
	// both act on the file - the parked call and one more, whichever paths
	// they took - and no journal can let them run and keep the count at
	// zero; a journal that serialises its retires lets the second find the
	// segment leaving without touching the file, so there the count is at
	// most one double.
	//
	// decision: the double retire is reachable only where the journal's
	// design lets both selectors run to the segment's own retirement,
	// because the per-segment guard an unlink or a recycle takes
	// exclusively serialises the two, and a selector may also find the
	// segment already out of the set and subtract nothing without a file
	// call. So the count is required exactly only on the calls the seam
	// saw, and the exact one is what two selectors that each reached the
	// file leave: one call to each is the retire that landed and the
	// retire that found it leaving, never a second subtract, since a retire
	// that decides its path first makes exactly one call on the segment's
	// old name. Where the seam saw nothing doubled, at most one counted
	// double is held, no minimum: a conforming journal that claims its
	// candidate before any storage call selects, retires and counts inside
	// its own serialisation, invisible here.
	seamSaw := f.Seam.CallsFor(journalv2test.OpRemove, name) + f.Seam.CallsFor(journalv2test.OpRename, name)
	if got := after.Counters.DoubleRetires - before.Counters.DoubleRetires; got > 1 || (seamSaw >= 2 && got != 1) {
		t.Fatalf("DoubleRetires rose by %d over a selection the seam watched in %d file calls, want 1 when both selectors reached the segment and never more", got, seamSaw)
	}
	if _, ok := segmentInfo(f, seg); ok {
		t.Fatalf("segment %d is still owned after both selectors retired it", seg)
	}
	// The retire that landed took one of the two paths, and both are
	// visible at the seam: an unlink of the segment's name - which a failed
	// recycle's fallback also is - or a rename to a fresh one for the
	// spare pool. The accounted storage is checked for the path of the call
	// that landed and the freed counter is the segment's account on either.
	wantRetireAccounted(t, f, "a double selection's retire", seg, beforeUsed, s.Allocated, createsBefore, landed == journalv2test.OpRename)
	wantFreed(t, f, "a double selection's retire", before.Counters.FreedBytes, uint64(s.Allocated))
	if after.UsedBytes < 0 {
		t.Fatalf("UsedBytes = %d after the retirement, want it never below zero", after.UsedBytes)
	}
}

// TestEdge_WriteLargerThanSegmentRest checks that a write larger than the
// room its segment has left is cut at the cap, one record per segment, one
// version returned once every piece is written, the pieces held contiguous
// and exact, reading back whole across a crash. It relies on the fixture's
// 1 MiB segment cap.
func TestEdge_WriteLargerThanSegmentRest(t *testing.T) {
	const large = 256 << 10
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		crossesTail(t, f, h, id, k, distinct(20+k, large),
			"a write larger than its segment's tail", "rest"+fmt.Sprint(k), writeRecordBytes(large))
	}
}

// TestEdge_WriteLargerThanSegment checks that a write more than two segment
// caps long is cut into one record per segment but a single version, its
// pieces contiguous and exact, reading back whole across a crash; and that a
// power cut between its pieces keeps the piece whose segment was synced, its
// bytes at their own offsets, with the rest missing. It relies on the
// fixture's 1 MiB segment cap.
func TestEdge_WriteLargerThanSegment(t *testing.T) {
	const size = 5 << 19 // 2.5 MiB, above two 1 MiB caps
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		p := distinct(30+k, size)
		v := write(t, h, id, 0, p)
		syncFile(t, h, id)

		es := indexEntries(t, f, id)
		if n := wantPieces(t, es, ext(0, size), fmt.Sprintf("share %d: a write over two caps", k)); n < 3 {
			t.Fatalf("share %d: the write is %d held entries, want at least three", k, n)
		}
		if n := segmentsOver(es, ext(0, size), v); n < 3 {
			t.Fatalf("share %d: a write over two caps lies in %d segments, want at least three", k, n)
		}
		wantBytes(t, h, id, 0, p, "a write over two caps")
		f.Crash()
		wantBytes(t, f.Share(k), id, 0, p, "a write over two caps, after a crash")
	}

	// The power cut between the pieces: the write parks at its roll - the
	// whole-spare file it takes is held on its write, and no spare can be
	// made under an unheld name while it runs - after its first piece is
	// placed in the segment the stream sat in, whose id the head's record
	// names beforehand. The segment is synced through the seam while the
	// roll is parked, and the crash then keeps exactly that piece: a
	// non-empty prefix of the write at its own offsets, the rest missing,
	// never other bytes.
	//
	// decision: the roll's cost is the take of an already-named spare, so
	// every whole spare's file is held and the spare-making create and
	// rename are held on every name besides, because which spare the roll
	// takes is not observable before the write parks. The piece writes are
	// left unheld - a held write would stop the first piece before it is
	// placed - and the moment the write parks is read from the holds alone,
	// never from the index, which the interface makes live without promising
	// that a record is indexed before the write that placed it returns: a
	// journal that publishes a write's pieces together on its return never
	// shows the piece while parked, but its roll is held all the same. The
	// pieces land in the file's own order, one per segment at the cap, so
	// the piece past the synced one is exactly what the crash takes.
	// Overturned when a check can name the spare the roll takes beforehand.
	t.Run("CrashBetweenPieces", func(t *testing.T) {
		f := journalv2test.New(t)
		h, id := f.Share(0), journalv2test.File(0)

		// The whole spare pool as it stands before anything the subtest
		// writes takes from it: spares are made off the write path, and the
		// count the head write finds here is the count its own roll's
		// refill has to give back before the holds below are armed, so an
		// in-flight refill is waited out, never parked behind them.
		eventually(t, "the spare pool held no whole spare a roll could take", func() bool {
			return len(spareNames(t, f)) > 0
		})
		wantAccounted(t, f, "before the head write")
		pool := len(spareNames(t, f))

		// A synced record, so the stream's active segment holds something
		// and the large write's first piece lands in it with room to
		// spare. The head's placement names that segment's id beforehand:
		// the first piece inherits it, whatever its own length turns out
		// to be.
		head := distinct(40, 4096)
		write(t, h, id, 0, head)
		syncFile(t, h, id)
		seg := segmentOf(t, f, id, 0)

		// The head's write may have rolled and taken a spare, leaving its
		// refill in flight. The pool is waited back to the count the head
		// found - reserve spares are never taken by a roll, so what fell
		// is what the refill gives back - before the spare-making calls
		// are held, so no refill the head started parks behind the holds
		// for the rest of the subtest and leaves the pool empty.
		eventually(t, "the spare pool did not return to the count the head write found", func() bool {
			return len(spareNames(t, f)) >= pool
		})
		wantAccounted(t, f, "once the pool refilled what the head took")

		// The spare-making calls are held before the pool is read again: a
		// spare whose rename lands after the listing would have no hold, so
		// the create and the rename are parked for the whole of what
		// follows.
		spareMaking := []*journalv2test.Hold{
			f.Seam.Hold(t, journalv2test.OpCreate, "*"),
			f.Seam.Hold(t, journalv2test.OpRename, "*"),
		}
		var holds []*journalv2test.Hold
		for _, name := range spareNames(t, f) {
			holds = append(holds, f.Seam.Hold(t, journalv2test.OpWrite, name))
		}
		p := distinct(50, 2<<20) // two caps, so the write rolls at least once
		wrote := make(chan error, 1)
		// The handle is captured here, before the goroutine: the crash
		// rewrites the fixture's handles, and the goroutine must not read
		// them, fail the test, or call anything that does.
		held := h
		go func() {
			wrote <- journalv2test.RetrySpare(func() (err error) {
				_, err = held.WriteAt(t.Context(), id, 4096, p, 0)
				return err
			})
		}()

		// The first piece is written into the stream's segment and the write
		// parks at its roll, the first storage call it makes past that: the
		// header write on the spare it takes. No spare is written past that,
		// so a hold on a spare's own write is the write-parked signal and
		// the write cannot return before one fires: a returned write is a
		// roll the holds missed.
		eventually(t, "the parked write reached no spare it must hold", func() bool {
			for _, hold := range holds {
				if hold.Waiting() > 0 {
					return true
				}
			}
			return false
		})
		select {
		case err := <-wrote:
			stub(t, "WriteAt", err)
			t.Fatalf("a write of %d bytes returned without a roll: no spare's write was held", len(p))
		default:
		}

		// The write is parked at its roll: its first piece is placed in the
		// segment the head named, and nothing past it is. Syncing that
		// segment through the seam makes the piece durable where it lies,
		// the crash below keeping it and losing the rest.
		file, err := f.Seam.Open(segmentName(seg))
		if err != nil {
			t.Fatalf("open the first piece's segment: %v", err)
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			t.Fatalf("sync the first piece's segment through the seam: %v", err)
		}

		// Every hold is opened before the crash: the fixture's crash waits
		// for its calls in flight, so a call still parked at either hold
		// would hang the shutdown. The seam's crash runs first so nothing a
		// released call does reaches the directory.
		if err := f.Seam.Crash(); err != nil {
			t.Fatalf("crash the seam: %v", err)
		}
		for _, hold := range append(holds, spareMaking...) {
			hold.Release()
		}
		f.Crash()
		select {
		case err := <-wrote:
			stub(t, "WriteAt", err)
		case <-time.After(backgroundWait):
			t.Fatalf("the large write did not return within %v once its roll was let through", backgroundWait)
		}

		h = f.Share(0)
		wantBytes(t, h, id, 0, head, "the head, durable before the cut")

		// The kept piece is a non-empty proper prefix of the write at its
		// own offsets and the rest is missing, whole: the read of the entire
		// write names where the prefix ends, and neither side may carry the
		// other's bytes. A prefix that ends at the write's start is no piece
		// kept at all, one that ends at its end is no cut between the
		// pieces, and missing extents before the end are a hole in the tail
		// or a straddled piece, which no roll-parked cut leaves.
		r := read(t, h, id, 4096, int64(len(p)))
		if len(r.missing) == 0 {
			t.Fatalf("the write reads whole after a cut parked at its roll, want the pieces past the synced one missing")
		}
		if len(r.missing) > 1 {
			t.Fatalf("the write reads missing in %+v extents, want one whole tail of the rest", r.missing)
		}
		m := r.missing[0]
		l := m.Off - 4096
		if l <= 0 {
			t.Fatalf("the missing part starts at the write's first byte, want a non-empty prefix held past the synced piece")
		}
		if l >= int64(len(p)) {
			t.Fatalf("the whole write survived the cut, want a cut between the pieces")
		}
		if !bytes.Equal(r.p[:l], p[:l]) {
			t.Fatalf("the held prefix is not the write's own first %d bytes", l)
		}
		wantMissing(t, r, 4096, []journalv2.Extent{ext(4096+l, int64(len(p))-l)}, "the tail past the kept prefix")
	})
}

// TestEdge_OpenEmptyDirectory checks that opening a directory the journal is
// not in fails without changing it whatever identity the caller names, that
// Init makes an empty directory a journal holding nothing, and that two
// shares on it write and read back.
func TestEdge_OpenEmptyDirectory(t *testing.T) {
	f := journalv2test.New(t)
	old := f.ID()
	write(t, f.Share(0), journalv2test.File(0), 0, pattern("gone", 4096))

	f.Shutdown()
	emptyDir(t, f)
	before := dirNow(t, f)

	if err := f.TryReopen(); !errors.Is(err, journalv2.ErrNoJournal) {
		stub(t, "Open", err)
		t.Fatalf("opening an empty directory returned %v, want ErrNoJournal", err)
	}
	wantUntouched(t, f, before)

	if err := f.TryReopenAs(journalv2.JournalID{}); !errors.Is(err, journalv2.ErrNoJournal) {
		stub(t, "Open", err)
		t.Fatalf("opening an empty directory named as uninitialised returned %v, want ErrNoJournal", err)
	}
	wantUntouched(t, f, before)

	id, err := f.TryInit()
	stub(t, "Init", err)
	if err != nil {
		t.Fatalf("initialising an empty directory: %v", err)
	}
	if id == old {
		t.Fatalf("a fresh Init on the emptied directory kept the identity %x", old)
	}
	if err := f.TryReopen(); err != nil {
		stub(t, "TryReopen", err)
		t.Fatalf("reopening after a fresh Init: %v", err)
	}
	for id, es := range f.J.Inspect().All() {
		for _, e := range es {
			if e.Kind == journalv2.EntryHeld {
				t.Fatalf("the fresh journal holds %+v of file %v, want nothing", e, id)
			}
		}
	}

	for k := range 2 {
		h, fk := f.Share(k), journalv2test.File(k)
		p := pattern(fmt.Sprint("fresh", k), 4096)
		write(t, h, fk, 0, p)
		syncFile(t, h, fk)
		f.Crash()
		wantBytes(t, f.Share(k), fk, 0, p, fmt.Sprintf("share %d's write on the fresh journal, after a crash", k))
	}
}

// TestEdge_OpenFormatOnly checks that a directory holding nothing but the
// journal's format file opens as the journal that file names — an Init
// refused, the named identity read back, another identity refused — that
// writes beside the file survive a crash, and that an Init on the other
// opening states is refused and reported: a lone segment file is unclean
// and unidentified, a lone format.tmp blocks nothing.
func TestEdge_OpenFormatOnly(t *testing.T) {
	f := journalv2test.New(t)
	formatID := f.ID()
	write(t, f.Share(0), journalv2test.File(0), 0, pattern("gone", 4096))
	syncFiles(t, f.Share(0), journalv2test.File(0))
	seg := entryAt(t, f, journalv2test.File(0), 0).Location.Segment
	segmentBytes := durable(t, f, segmentName(seg))

	f.Shutdown()
	emptyDir(t, f)
	plant(t, f, "format", (&raw.Format{Version: 1, Identity: formatID, Installation: journalv2test.FixtureInstall}).Bytes())
	before := dirNow(t, f)

	if _, err := f.TryInit(); !errors.Is(err, journalv2.ErrNotEmpty) {
		stub(t, "Init", err)
		t.Fatalf("an Init on a directory holding a format file returned %v, want ErrNotEmpty", err)
	}
	wantUntouched(t, f, before)

	// An open expecting no identity fails here: the format file names
	// journalv2test.FixtureInstall too, so the error must name both.
	err := f.TryReopenAs(journalv2.JournalID{})
	stub(t, "Open", err)
	wantIdentityError(t, err, formatID, journalv2test.FixtureInstall, false)
	wantUntouched(t, f, before)

	// An identity the format file does not carry is refused naming the one
	// it does, never accepted silently.
	if err := f.TryReopenAs(journalv2.JournalID{9}); err == nil {
		t.Fatalf("an open under another identity was accepted by a format file that names %x", formatID)
	} else {
		wantIdentityError(t, err, formatID, journalv2test.FixtureInstall, false)
	}
	wantUntouched(t, f, before)

	if err := f.TryReopen(); err != nil {
		stub(t, "TryReopen", err)
		t.Fatalf("reopening the directory the format file names: %v", err)
	}
	for fid, es := range f.J.Inspect().All() {
		for _, e := range es {
			if e.Kind == journalv2.EntryHeld {
				t.Fatalf("the journal holds %+v of file %v over a format file alone, want nothing", e, fid)
			}
		}
	}
	kept := [2][]byte{pattern("kept0", 4096), pattern("kept1", 4096)}
	for k := range 2 {
		write(t, f.Share(k), journalv2test.File(k), 0, kept[k])
		syncFile(t, f.Share(k), journalv2test.File(k))
	}
	f.Crash()
	for k := range 2 {
		wantBytes(t, f.Share(k), journalv2test.File(k), 0, kept[k], fmt.Sprintf("share %d's write beside a lone format file, after a crash", k))
	}

	// A lone format.tmp does not block an Init: an interrupted one leaves
	// nothing acknowledged, and a fresh Init clears it.
	g := journalv2test.New(t)
	g.Shutdown()
	emptyDir(t, g)
	plant(t, g, "format.tmp", (&raw.Format{Version: 1, Identity: g.ID(), Installation: journalv2test.FixtureInstall}).Bytes())
	if _, err := g.TryInit(); err != nil {
		stub(t, "Init", err)
		t.Fatalf("an Init beside a leftover format.tmp: %v", err)
	}

	// Init on a directory holding one segment file: refused, the directory
	// untouched, and opening it reports the directory unidentified.
	third := journalv2test.New(t)
	third.Shutdown()
	emptyDir(t, third)
	plant(t, third, segmentName(seg), segmentBytes)
	beforeThird := dirNow(t, third)
	if _, err := third.TryInit(); !errors.Is(err, journalv2.ErrNotEmpty) {
		stub(t, "Init", err)
		t.Fatalf("an Init on a directory holding a lone segment file returned %v, want ErrNotEmpty", err)
	}
	wantUntouched(t, third, beforeThird)
	if err := third.TryReopen(); !errors.Is(err, journalv2.ErrUnidentified) {
		stub(t, "Open", err)
		t.Fatalf("opening a directory holding a lone segment file returned %v, want ErrUnidentified", err)
	}
	wantUntouched(t, third, beforeThird)
}

// TestEdge_OpenUnrecognisedFormatVersion checks that a format file naming a
// version the journal does not know fails the open — never upgraded or
// repaired silently — naming the unknown format, that the open changes
// nothing in the directory, and that an Init on it is refused and reported
// rather than replacing the file.
func TestEdge_OpenUnrecognisedFormatVersion(t *testing.T) {
	f := journalv2test.New(t)
	id := f.ID()
	f.Shutdown()
	emptyDir(t, f)
	plant(t, f, "format", (&raw.Format{Version: 0, Identity: id, Installation: journalv2test.FixtureInstall}).Bytes())
	before := dirNow(t, f)

	// The open refuses the version the file names, and the identity it also
	// names never comes into it: a file the journal cannot read holds no
	// identity to match or mismatch.
	if err := f.TryReopen(); !errors.Is(err, journalv2.ErrUnknownFormat) {
		stub(t, "Open", err)
		t.Fatalf("opening a directory whose format file names version 0 returned %v, want ErrUnknownFormat", err)
	}
	wantUntouched(t, f, before)

	if _, err := f.TryInit(); !errors.Is(err, journalv2.ErrNotEmpty) {
		stub(t, "Init", err)
		t.Fatalf("an Init on a directory holding a version-0 format file returned %v, want ErrNotEmpty", err)
	}
	wantUntouched(t, f, before)

	// decision: this reaches only an unrecognised format version, never a
	// recognised older or newer one, which no migration path to check exists
	// for while version 1 is the only one the format knows; overturn it when
	// a second version lands and the open gets a version-negotiating path to
	// test.
}

// repackSegment repacks one segment at the cap so a retire is what the call
// does to it, as the C tests' repack of a named segment does. The caller
// reports the error: this runs where the caller put it, which may be off the
// test goroutine, and stubs belong on the test goroutine.
func repackSegment(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) error {
	t.Helper()

	_, err := f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
	return err
}

// heldOnly filters es to the entries that read as held. Inspect().File
// returns every entry kind, and an unsettled removal keeps its marker among
// the file's entries, so a count of held entries goes through this.
func heldOnly(es []journalv2.IndexEntry) []journalv2.IndexEntry {
	var out []journalv2.IndexEntry
	for _, e := range es {
		if e.Kind == journalv2.EntryHeld {
			out = append(out, e)
		}
	}
	return out
}

// fillToTail writes records of chunk bytes through h at id back to back
// until the segment the stream sits in cannot room a record of need bytes,
// and returns the offset the next write lands at. The room is measured from
// where the last record ended — the entry's location plus the record its own
// extent took, honest even where a record was cut at the cap — not from the
// segment's account, which stays at its cap until the segment is sealed
// short, truncated or unlinked.
//
// A fill that was itself cut at the cap does not end the loop. A segment
// cannot seal until its footer names every record in it, so the room a
// segment has for one more record is smaller than the cap shows - by the
// seal marker, the footer's entry per record and the trailer - and a fill
// can fit that reduced room as a whole record in the journal's accounting
// while the test's own measure of the cap's remainder says another fit. The
// cut is plain in the index: the fill's first piece is a shorter extent, so
// it is detected, and the loop fills on into the segment behind the cut one,
// measuring that segment the same way, until a whole fill leaves less than
// need bytes of cap behind it.
func fillToTail(t *testing.T, h *journalv2.Handle, f *journalv2test.Fixture, id journalv2.FileID, tag string, chunk, need int64) int64 {
	t.Helper()

	var at int64
	for {
		write(t, h, id, at, pattern(fmt.Sprint(tag, at), int(chunk)))
		last := entryAt(t, f, id, at)
		room := journalv2test.FixtureSegmentCap - last.Location.Offset - writeRecordBytes(int(last.Extent.Len))
		// The next fill fills on whatever room the segment behind has when
		// this one was cut, so only a whole fill's remainder ends the loop.
		if last.Extent.Len == chunk && room < need {
			return at + chunk
		}
		at += chunk
		if at > 4*journalv2test.FixtureSegmentCap {
			t.Fatalf("writes of %d bytes never rolled a segment of %d", chunk, journalv2test.FixtureSegmentCap)
		}
	}
}

// wantRetireAccounted fails the test unless, once the retire named by what
// has run, the segment is no longer listed and the storage the journal
// accounts moved by the retire's own path: an unlink falls by the segment's
// account, a recycle leaves the account where it was. recycled names the
// path the caller read from the call that landed. The caller reads before
// before the retire and settles the spare pool on both sides, and checks the
// freed counter through wantFreed.
//
// decision: where a spare refill may have landed between the two reads - the
// seam saw a file created - the move is compared modulo whole segment caps
// and bounded above by the account, because the journal accounts spares in
// UsedBytes without Stats reporting them, so an equality to the byte races a
// refill the journal is allowed to run between the reads, and a refill moves
// the after read upward. Where no file was created between the reads, no
// refill is owed beyond the retire's own: an unlink then falls by exactly the
// account, and a recycle - which adds the spare it refills with, in
// UsedBytes, at the cap it was accounted at - must not fall at all. The
// recycle leg asks "no fall" rather than equality because a refill booked
// just after the first read may have raised the after read, a move the modulo
// check still holds to whole caps. Not checked: a refill that raised the
// account is not told from the retire's own effect, so a rise by one cap is
// accepted, never required. The specification defines a spare count the two
// reads could subtract; the Go Stats lacks it, so the check tightens to an
// exact move per path, wherever a refill ran, once Stats carries it.
func wantRetireAccounted(t *testing.T, f *journalv2test.Fixture, what string, seg journalv2.SegmentID, before, allocated int64, createsBefore int, recycled bool) {
	t.Helper()

	wantAccounted(t, f, "after the retirement")
	if _, ok := segmentInfo(f, seg); ok {
		t.Fatalf("%s left segment %d still listed", what, seg)
	}
	after := f.J.Stats().UsedBytes
	fell := before - after
	refilled := f.Seam.Calls(journalv2test.OpCreate) > createsBefore
	switch {
	case !refilled && recycled:
		if fell > 0 {
			t.Fatalf("%s recycled a segment accounted at %d and moved the accounted storage from %d to %d, with no spare made between the reads, want a recycle to leave the account where it was", what, allocated, before, after)
		}
	case !refilled:
		if fell != allocated {
			t.Fatalf("%s unlinked a segment accounted at %d and moved the accounted storage from %d to %d, with no spare made between the reads, want the fall to be the account alone", what, allocated, before, after)
		}
		return
	}
	if !sameBut(before, after, 0) || fell > allocated {
		t.Fatalf("%s moved the accounted storage from %d to %d, want a move in whole segment caps that falls by at most the segment's %d account", what, before, after, allocated)
	}
}

// wantFreed checks the freed counter over one retire of a segment accounted
// at allocated, named by what. Freed counts the storage the retire returned,
// to the filesystem by an unlink or to the spare pool by a recycle, and a
// segment is retired once: either path returns the segment's whole account
// exactly once - a second debit is a retire that left the bytes twice, a
// short one storage returned with no record of it - so the move is the
// account on every path and no path need be told.
//
// decision: the counter's recycle reading is the specification's; the Go
// Stats comment on FreedBytes still says "by unlinking segments", which
// reads narrower. Overturned if the specification narrows the counter back
// to unlinks alone, when the exact equality below holds on the unlink path
// only.
func wantFreed(t *testing.T, f *journalv2test.Fixture, what string, before uint64, allocated uint64) {
	t.Helper()

	if freed := f.J.Stats().Counters.FreedBytes - before; freed != allocated {
		t.Fatalf("%s of a segment accounted at %d moved freed by %d, want exactly its account", what, allocated, freed)
	}
}

// atCapacityMax is atCapacity for a journal whose maximum the caller raised in
// whole segment caps of capB: the account is not only within two caps of the
// maximum but also never above it, since spares too book against the maximum,
// so a refusal there means nothing more could be booked at all.
func atCapacityMax(t *testing.T, s journalv2.Stats, capB int64) {
	t.Helper()

	if s.UsedBytes < s.MaxBytes-2*capB || s.UsedBytes > s.MaxBytes {
		t.Fatalf("UsedBytes = %d after a refusal at a maximum of %d, want within two caps of it and never above it", s.UsedBytes, s.MaxBytes)
	}
}
