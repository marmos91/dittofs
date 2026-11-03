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
// every superseded byte accounted as unreclaimed or freed rather than held,
// and every version drawn strictly rising.
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
		if s.UnreclaimedBytes < 0 {
			t.Fatalf("share %d: UnreclaimedBytes = %d, want it never below zero", k, s.UnreclaimedBytes)
		}
		wantBytes(t, h, id, 0, pattern(fmt.Sprint("flat", k, rounds-1), chunk), "the last of a thousand overwrites")
	}

	// The index still names the two extents the files hold. Every
	// superseded one is gone: the write that replaced it carried no removal
	// marker, and once its replacement synced its retained extent left the
	// index, so the count is the two live entries and no more.
	s := f.J.Stats()
	if s.ExtentCount != 2 {
		t.Fatalf("ExtentCount = %d with two extents held, want the two live entries alone", s.ExtentCount)
	}
	// The superseded records are accounted while they wait for repack:
	// unreclaimed is what they are until their segment is unlinked or
	// recycled. The journal-wide count is checked once, after both files'
	// rounds, and the per-share counts beside it.
	//
	// decision: a bound on the journal-wide UnreclaimedBytes alone holds
	// only while no segment these records land in has retired: the gauge
	// names storage in segments not yet unlinked that backs no live record,
	// and once a segment retires, by unlink or by recycle, its superseded
	// bytes leave the gauge and reach no other number Stats reports. The
	// rounds sync every hundred writes and a synced segment nothing holds
	// can be retired off the write path, so the per-share rows - the record
	// bytes of the share's records still on disk, wherever they lie - carry
	// the account a retired segment drops from the journal's own gauge. The
	// check accepts either bound, and the true lower bound wants a count of
	// the retirements the seam saw, which its remove and rename counts name
	// only over the whole directory. Overturned once they can name segment
	// files alone.
	rec := int64(writeRecordBytes(chunk))
	due := 2 * int64(rounds-1) * rec
	whole := s.UnreclaimedBytes
	var perShare int64
	for k := range 2 {
		if ss, ok := s.Shares[journalv2test.Tag(k)]; ok {
			perShare += ss.UnreclaimedBytes
		}
	}
	if perShare < due && whole < due {
		t.Fatalf("the thousand overwrites superseded %d bytes of records and left UnreclaimedBytes at %d per share and %d journal-wide, want at least %d under either reading", due, perShare, whole, due)
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

		// Records of the test write's own size until the segment the stream
		// sits in cannot hold one more of them, so the crossing write is the
		// one that has to roll.
		p := distinct(20+k, chunk)
		off := fillToTail(t, h, f, id, "cross"+fmt.Sprint(k), chunk, writeRecordBytes(chunk))
		e := ext(off, int64(len(p)))
		v := write(t, h, id, off, p)
		syncFile(t, h, id)

		es := indexEntries(t, f, id)
		if n := wantPieces(t, es, e, fmt.Sprintf("share %d: the write crossing a cap", k)); n < 2 {
			t.Fatalf("share %d: the crossing write is %d held entries, want at least two", k, n)
		}
		if n := segmentsOver(es, e, v); n < 2 {
			t.Fatalf("share %d: the crossing write's pieces lie in %d segments, want at least two", k, n)
		}

		wantBytes(t, h, id, off, p, "the write crossing a segment cap")
		f.Crash()
		wantBytes(t, f.Share(k), id, off, p, "the write crossing a segment cap, after a crash")
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
// leaves the journal's set, the accounted storage moves in whole caps and
// by no more than the segment's account, and the freed counter debits
// exactly the account on the retire path that unlinks and nothing more it
// owes on the one that recycles.
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
	// pool. Both are accounted at a whole cap, so a footprint that cannot
	// see spares does not tell the paths apart, and the freed counter is
	// checked per path through wantFreed.
	wantRetireAccounted(t, f, "a retire without a repack", seg, beforeUsed, info.Allocated)
	wantFreed(t, f, "a retire without a repack", name, before.Counters.FreedBytes, uint64(info.Allocated))
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
	fit := journalv2test.File(40)
	capB := int64(journalv2test.FixtureSegmentCap)
	at := int64(0)
	var tail int64
	for rolls := 0; ; {
		room := limit - f.J.Stats().Shares[limitedEdgeA].UsedBytes
		if at > 0 {
			last := entryAt(t, f, fit, at-1)
			tail = last.Location.Offset + writeRecordBytes(int(last.Extent.Len))
		} else {
			tail = 0 // a fresh file: its first record lands wherever its stream stands
		}
		tailroom := capB - tail%capB
		if tailroom < writeRecordBytes(1) || tail == 0 {
			tailroom = capB // the next record cannot fit: it rolls into a fresh segment
		}
		if tailroom >= room && room >= writeRecordBytes(1) {
			break // one record's charge is the whole room left: the exact fit
		}
		p := (min(room, tailroom) - raw.RecordHeaderSize - 8) &^ 7
		if p >= 4096 {
			// nothing yet
		} else if p >= 8 && tailroom >= 2*writeRecordBytes(1) {
			p = 4096 // steer: a 4096-byte payload, one piece, no split
			if room-writeRecordBytes(4096) < 2*writeRecordBytes(1) || tailroom-writeRecordBytes(4096) < 2*writeRecordBytes(1) {
				p = max(8, min(room, tailroom)-2*raw.RecordHeaderSize) &^ 7 // keep the endgame reachable
			}
		} else if room >= writeRecordBytes(1) {
			// the tail cannot hold what the room can: the next write rolls
			tail = 0
			rolls++
			if int64(rolls) > 2*limit/capB+2 {
				t.Fatalf("the fills never reached a room one record fills: %d of room left", room)
			}
			continue
		} else {
			t.Fatalf("precondition: the fills left %d of room, not one whole record's charge", room)
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
	if n < 8 {
		t.Fatalf("precondition after the fills: %d of room over a %d limit, no record's payload fits", room, limit)
	}
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

// TestEdge_ReservationExactFitJournal checks that one segment draw short of
// the journal's maximum, a reservation of exactly the space left is taken and
// one a byte over is refused at once naming the journal's limit and the byte,
// and that once the fills reach the maximum the reservations that fit are
// taken — each drawing the spare its records need from the pool, a spare
// accounted against the maximum from its making, so drawing one there is the
// take the maximum allows — and that once the pool is dry, the next one is
// refused at once naming the journal's limit and the shortfall of the whole
// segment the writes need, never the record's own bytes, without reaching a
// segment.
func TestEdge_ReservationExactFitJournal(t *testing.T) {
	const fill = 256 << 10
	// A maximum a whole number of segment caps, so the space the fills leave
	// below the maximum is whole segments or none, and a reservation can be
	// sized to it exactly.
	capB := int64(journalv2test.FixtureSegmentCap)
	f := journalv2test.NewWith(t, func(s *journalv2.Settings) {
		s.MaxBytes = 8 * capB
	})

	a := f.Share(0)
	// One stream alone, and the account settled before every read below:
	// each roll the fills make allocates one spare against the maximum, a
	// refill that lands between two reads moves UsedBytes by a whole cap,
	// and a used account read mid-flight buys no reservation, so the fills
	// wait for the settles, not for a quiet moment.
	id := journalv2test.File(10)
	var at int64
	wantAccounted(t, f, "before the fills")
	settledUsed := f.J.Stats().UsedBytes
	for {
		write(t, a, id, at, pattern(fmt.Sprint("fill", at), fill))
		at += fill
		used := f.J.Stats().UsedBytes
		if settledUsed-used == 0 {
			continue // a release running below the writes can move the account
		}
		if settledUsed-used != capB {
			break // the account settled on a smaller value: the room ran out
		}
		settledUsed = used
	}
	// One spare draw short of the maximum, exactly: the last roll's refill
	// moved the settled account by one cap and the loop stopped on it. The
	// reservation below is refused against the room its own refusal counts,
	// not against a MaxBytes-UsedBytes read racing a refill.
	wantAccounted(t, f, "before the asks near the maximum")

	// The journal's own answer names the exact room: one byte over asks for
	// more room than any reservation drew, and its refusal's missing count
	// is the room the journal sees, whole bytes it cannot give. The count is
	// one over the byte asked whether footer and pool charges are baked in.
	err := refusedAtOnce(t, f, "a reservation a byte over the journal's remaining space", func() error {
		_, err := a.Reserve(2*capB+1, 1)
		return err
	})
	ns := refusal(t, err, "a reservation a byte over the journal's remaining space", journalv2.LimitJournal, journalv2.UnitBytes)
	gap := int64(ns.Missing)
	if gap <= 0 || gap >= capB || gap%8 != 0 {
		t.Fatalf("a reservation one byte over the remaining space names %d bytes missing, want a positive whole unit short of the unlocked cap", ns.Missing)
	}

	// A reservation of exactly that count, padded to eight as a record is, is
	// taken: it fills the room the refusal named, whatever footer and pool
	// entries the count bakes in. One padded unit past it is refused at
	// once, naming the journal's limit.
	first := reserve(t, a, gap-8, 1)
	if err := first.Close(); err != nil {
		stub(t, "Reservation.Close", err)
		t.Fatalf("closing the reservation sized to the room the refusal named: %v", err)
	}
	err = refusedAtOnce(t, f, "a reservation one padded unit past the journal's remaining space", func() error {
		_, err := a.Reserve(gap, 1)
		return err
	})
	ns = refusal(t, err, "a reservation one padded unit past the journal's remaining space", journalv2.LimitJournal, journalv2.UnitBytes)

	// The fills go on to the maximum itself: the last spare draw the maximum
	// allows lands, and the write past it is refused naming the journal's
	// limit, the fill loop ending at the capacity the drain below runs at.
	for {
		_, err := tryWrite(t, a, id, at, pattern(fmt.Sprint("fill", at), fill))
		stub(t, "WriteAt", err)
		if err != nil {
			wantNoSpace(t, err, journalv2.LimitJournal, "a write past the journal's maximum")
			break
		}
		at += fill
	}
	s := f.J.Stats()
	atCapacity(t, s)
	room := s.MaxBytes - s.UsedBytes
	if room < 0 || room >= capB || room%capB != 0 {
		t.Fatalf("precondition: %d bytes used of a %d maximum left %d, want whole segments short of one or none", s.UsedBytes, s.MaxBytes, room)
	}

	// Record bytes one padded unit past the smallest record: at the maximum
	// this is the ask a byte over the space left, whichever tail a record
	// could still fit, because only a segment can room it.
	over := writeRecordBytes(1) + 8

	// Take the reservations that fit, each held open, so the spare it draws
	// stays taken and the pool really empties rather than the same spare
	// going round. The pool's target is one spare per stream, so a handful
	// of takes drains it and no refill can complete at the maximum; the
	// bound is against a pool no reservation ever drew from. A refusal on
	// the first attempt is what the journal must never do with spares ready,
	// so the takes are counted and the test fails on none.
	taken := 0
	for range 16 {
		if err := journalv2test.RetrySpare(func() (err error) {
			_, err = a.Reserve(over, 1)
			return err
		}); err != nil {
			break
		}
		taken++
	}
	if taken == 0 {
		t.Fatal("no reservation was taken at the maximum with spares ready")
	}

	// The pool drained: this ask is the one past the space left, and it
	// must be refused at once, with no call reaching a segment.
	err = refusedAtOnce(t, f, "a reservation one record past the journal's maximum", func() error {
		return journalv2test.RetrySpare(func() (err error) {
			_, err = a.Reserve(over, 1)
			return err
		})
	})
	ns = refusal(t, err, "a reservation one record past the journal's maximum", journalv2.LimitJournal, journalv2.UnitBytes, journalv2.UnitSegments)
	// decision: the shortfall is checked from below, not for equality. A
	// reservation draws spares over-reserved - pieces counted interleaved
	// with the stream's other writes - so its refusal may name more than
	// the one segment a single record needs; the check holds the journal to
	// the bound the space row of the contract names, at least the whole
	// segment beyond the room left, never the record's own bytes.
	// Overturned when the reservation's spare draw is pinned exactly.
	perUnit := int64(capB)
	if ns.Unit == journalv2.UnitSegments {
		perUnit = 1
	}
	if ns.Missing*perUnit+room < capB {
		t.Fatalf("the refusal names %d %v short with %d of room, want at least the whole segment the writes need, never their record's own bytes", ns.Missing, ns.Unit, room)
	}
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
	var err error
	for range 2 * journalv2test.FixtureMaxBytes / chunk {
		_, err = tryWrite(t, h, id, at, pattern("over", chunk))
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
	// The device is whole again, so the very next roll's spare draw can
	// succeed - but nothing pins when a journal retries a refill that the
	// device refused: a backoff the journal armed reads the fixture's clock,
	// which moves only through the fixture, so a journal waiting out its own
	// timer is given the passes it needs and the writes keep trying. The
	// first write cleared of the fault then lands, and the writes go on
	// below the journal's own maximum, the device alone having been full.
	//
	// decision: the check reads the write succeeding under the
	// immediate-retry reading of a cleared ENOSPC - a roll refused for want of
	// a spare while the device accepts writes again is a spare-refusal, which
	// RetrySpare yields through, and a refill the journal retried at once is
	// the serialisation the fixture clock also serves; a journal that
	// retries only off a timer the test never advances is beyond what the
	// seam can reach, and it surfaces as the same ENOSPC, so the retries are
	// bounded and the fault is reported rather than hung. Overturned once a
	// seam point names a retry the journal schedules.
	var cleared error
	for range 2 * journalv2test.FixtureMaxBytes / (64 << 10) {
		cleared = journalv2test.RetrySpare(func() (err error) {
			_, err = tryWrite(t, h, id, at, pattern("after", 64<<10))
			return err
		})
		var ns *journalv2.NoSpaceError
		if cleared == nil || !errors.As(cleared, &ns) || ns.Limit != journalv2.LimitSpare {
			break // landed, or the device itself refuses again
		}
		f.Advance(journalv2.SyncBound)
		at += 64 << 10
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
		// after the failure returns clean.
		ho, other := f.Share(1-k), journalv2test.File(2*k+1)
		po := pattern(fmt.Sprint("other", k), 4096)
		write(t, ho, other, 0, po)

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
		if err == nil {
			if f.J.Stats().Counters.SyncReappendedBytes <= reappended {
				t.Fatalf("share %d: the Sync over a re-appended window returned nil without SyncReappendedBytes rising", k)
			}
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
			if loss := lastLoss(t, f, beforeFail); loss.File != id || loss.Extent != (journalv2.Extent{Off: 0, Len: 4096}) || loss.Version != v1 {
				t.Fatalf("share %d: the loss event is %+v, want the failed window's write at its extent and version", k, loss)
			}
			got, missing := readBack(t, h, id, 0, 4096)
			if len(missing) == 0 || !slices.Equal(joinedExtents(missing), []journalv2.Extent{{Off: 0, Len: 4096}}) {
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
	if got := f.J.Stats().Counters.HeadroomDraws - draws; got < 4 {
		t.Fatalf("HeadroomDraws moved by %d over a truncate, a delete, a release and a seal at the maximum, want at least 4", got)
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

// TestEdge_SegmentRetiredByEvictionAndRepack checks that when a segment's
// own retirement, once every record of it is released and made durable, and a
// repack aimed at it both select it, the segment leaves the journal once, the
// accounted storage moving in whole caps and by no more than the segment's
// account, the double retire counted whenever both selectors reached the
// segment, whichever of the two reaches the retire first.
func TestEdge_SegmentRetiredByEvictionAndRepack(t *testing.T) {
	for _, firstIs := range []string{"repack", "eviction"} {
		t.Run(firstIs, func(t *testing.T) {
			f := journalv2test.New(t)

			h, id := f.Share(0), journalv2test.File(0)
			s, es := soleSegment(t, f, h, id)
			seg := s.ID
			// A record on the other stream, stayed held, so the retire has a
			// journal to keep and no share is empty.
			writeSynced(t, f, 1, 1, 0, 64<<10)
			// The extents are released and made durable before anything is
			// armed: a released record is neither held nor retained, so the
			// repack carries nothing forward through them and its pass over
			// the segment reaches the retire alone, and a segment whose
			// releases are still pending is one a repack may skip rather than
			// sync another stream's records itself, so the releases are
			// durable before the repack can be asked to select it at all.
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
			syncFile(t, h, id)

			// The first selector parks at the first storage call its retire
			// makes on the segment's file, an unlink or a rename, so the
			// moment the retire starts is named whichever path it takes; only
			// that one call is held, not every later one, because a retire
			// holds the segment exclusively from the moment it starts and the
			// other selector's retire may wait on it without ever reaching
			// the file.
			name := segmentName(seg)
			heldRemove := f.Seam.HoldFirst(t, journalv2test.OpRemove, name)
			heldRename := f.Seam.HoldFirst(t, journalv2test.OpRename, name)
			before := f.J.Stats()
			// The accounted storage settles before it is read, so a spare
			// refill racing the snapshot is waited out, not read mid-flight.
			wantAccounted(t, f, "before the two selectors ran")
			beforeUsed := f.J.Stats().UsedBytes

			// The selectors are opened in the order named; either may park
			// at the holds, and the other runs beside it. Each reports
			// through its channel, never by failing the test off the test
			// goroutine.
			repacked := make(chan error, 1)
			evicted := make(chan error, 1)
			evict := func() { go func() { evicted <- h.Sync(t.Context(), id) }() }
			if firstIs == "repack" {
				go func() { repacked <- repackSegment(t, f, seg) }()
				select {
				case <-heldRemove.Reached():
				case <-heldRename.Reached():
				case err := <-repacked:
					stub(t, "Repack", err)
					t.Fatalf("the repack of a released segment returned %v before it selected anything", err)
				case <-time.After(backgroundWait):
					t.Fatalf("the repack did not reach its segment's retire within %v", backgroundWait)
				}
				// The repack's retire is parked at its storage call, the
				// segment's bytes not yet subtracted: let the journal's own
				// retirement select the same segment beside it, then let the
				// parked retire through.
				evict()
			} else {
				// The journal's own retirement opens first and parks at its
				// storage call; the repack runs beside it.
				evict()
				select {
				case <-heldRemove.Reached():
				case <-heldRename.Reached():
				case <-time.After(backgroundWait):
					t.Fatalf("the journal's own retirement did not reach the segment's retire within %v", backgroundWait)
				}
				go func() { repacked <- repackSegment(t, f, seg) }()
			}

			// A short breath for the second selector to arrive at the segment
			// - it may park behind the first's exclusive hold or select the
			// segment and find it leaving - and then the parked call is let
			// through, in whatever order the two retires land.
			eventually(t, "neither selector reached the segment's retire", func() bool {
				return heldRemove.Waiting()+heldRename.Waiting() >= 1
			})
			heldRemove.Release()
			heldRename.Release()

			// Both selectors must finish once the holds are opened, and
			// neither reports an error: the repack selected the segment and
			// its retire, double or not, ran to its end, and the Sync that
			// opened the reclamation retire returned what its window decided,
			// a double retire never failing it.
			gotRepacked, gotEvicted := false, false
			for !gotRepacked || !gotEvicted {
				select {
				case err := <-repacked:
					stub(t, "Repack", err)
					if err != nil {
						t.Fatalf("the repack beside the segment's reclamation: %v", err)
					}
					gotRepacked = true
				case err := <-evicted:
					stub(t, "Sync", err)
					if err != nil {
						t.Fatalf("the Sync beside the segment's retirement: %v", err)
					}
					gotEvicted = true
				case <-time.After(backgroundWait):
					t.Fatalf("neither selector returned within %v once their retire was let through", backgroundWait)
				}
			}

			eventually(t, "the segment was not retired once its removal was let through", func() bool { return retired(f, seg) })

			after := f.J.Stats()
			// The retire is idempotent: a retire that finds the segment gone
			// subtracts nothing, counts as one double retire and never moves
			// the storage twice. Two selectors reached the segment when the
			// seam saw both act on the file - the parked call and one more,
			// whichever paths they took - and no journal can let them run
			// and keep the count at zero; a journal that serialises its
			// retires lets the second find the segment leaving without
			// touching the file, so there the count is at most one double.
			//
			// decision: the double retire is reachable only where the
			// journal's design lets both selectors run to the segment's own
			// retirement, because the per-segment guard an unlink or a
			// recycle takes exclusively serialises the two, and a selector
			// may also find the segment already out of the set and subtract
			// nothing without a file call. So the count is required exactly
			// only on the calls the seam saw, and the exact one is what two
			// selectors that each reached the file leave: one call to each is
			// the retire that landed and the retire that found it leaving,
			// never a second subtract, since a retire that decides its path
			// first makes exactly one call on the segment's old name. Where
			// the seam saw nothing doubled, at most one counted double is
			// held, no minimum: a conforming journal that claims its
			// candidate before any storage call selects, retires and
			// counts inside its own serialisation, invisible here.
			seamSaw := f.Seam.CallsFor(journalv2test.OpRemove, name) + f.Seam.CallsFor(journalv2test.OpRename, name)
			if got := after.Counters.DoubleRetires - before.Counters.DoubleRetires; got > 1 || (seamSaw >= 2 && got != 1) {
				t.Fatalf("DoubleRetires rose by %d over a selection the seam watched in %d file calls, want 1 when both selectors reached the segment and never more", got, seamSaw)
			}
			if _, ok := segmentInfo(f, seg); ok {
				t.Fatalf("segment %d is still owned after both selectors retired it", seg)
			}
			// The retire that landed took one of the two paths, and both are
			// visible at the seam: an unlink of the segment's name - which a
			// failed recycle's fallback also is - or a rename to a fresh one
			// for the spare pool. Both are accounted at a whole cap, so the
			// move is checked in whole caps, bounded by the account, and the
			// freed counter is checked per path through wantFreed.
			wantRetireAccounted(t, f, "a double selection's retire", seg, beforeUsed, s.Allocated)
			wantFreed(t, f, "a double selection's retire", name, before.Counters.FreedBytes, uint64(s.Allocated))
			if after.UsedBytes < 0 {
				t.Fatalf("UsedBytes = %d after the retirement, want it never below zero", after.UsedBytes)
			}
		})
	}
}

// TestEdge_WriteLargerThanSegmentRest checks that a write larger than the
// room its segment has left is cut at the cap, one record per segment, one
// version returned once every piece is written, the pieces held contiguous
// and exact, reading back whole across a crash. It relies on the fixture's
// 1 MiB segment cap.
func TestEdge_WriteLargerThanSegmentRest(t *testing.T) {
	const (
		chunk = 256 << 10
		large = 256 << 10
	)
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)

		// Records of the large write's own size until the segment the stream
		// sits in cannot hold one more of them, so the large write is the
		// one that has to roll.
		p := distinct(20+k, large)
		off := fillToTail(t, h, f, id, "rest"+fmt.Sprint(k), chunk, writeRecordBytes(len(p)))
		e := ext(off, int64(len(p)))
		v := write(t, h, id, off, p)
		syncFile(t, h, id)

		es := indexEntries(t, f, id)
		if n := wantPieces(t, es, e, fmt.Sprintf("share %d: a write larger than its segment's tail", k)); n < 2 {
			t.Fatalf("share %d: the write is %d held entries over a short tail, want at least two", k, n)
		}
		if n := segmentsOver(es, e, v); n < 2 {
			t.Fatalf("share %d: the write over a short tail lies in %d segments, want at least two", k, n)
		}

		wantBytes(t, h, id, off, p, "a write larger than its segment's tail")
		f.Crash()
		wantBytes(t, f.Share(k), id, off, p, "a write larger than its segment's tail, after a crash")
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

		// A synced record, so the stream's active segment holds something
		// and the large write's first piece lands in it with room to
		// spare. The head's placement names that segment's id beforehand:
		// the first piece inherits it, whatever its own length turns out
		// to be.
		head := distinct(40, 4096)
		write(t, h, id, 0, head)
		syncFile(t, h, id)
		seg := segmentOf(t, f, id, 0)

		// The spare-making calls are held first, before the pool is read:
		// a refill the journal is running can race the listing, and a spare
		// whose rename lands after the listing would have no hold, so the
		// create and the rename are parked for the whole of what follows.
		spareMaking := []*journalv2test.Hold{
			f.Seam.Hold(t, journalv2test.OpCreate, "*"),
			f.Seam.Hold(t, journalv2test.OpRename, "*"),
		}
		// A roll takes a whole spare, so the spares must exist before the
		// write starts: the wait is for the pool to hold them, which the
		// journal does off the write's own path, and the account settling
		// waits the refills out so the spare names below are the whole pool.
		eventually(t, "the spare pool held no whole spare a roll could take", func() bool {
			return len(spareNames(t, f)) > 0
		})
		wantAccounted(t, f, "before the roll-parked write")
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
		if m.Off+m.Len != 4096+int64(len(p)) {
			t.Fatalf("the missing tail ends at %d, want the write's end %d", m.Off+m.Len, 4096+int64(len(p)))
		}
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
		if !bytes.Equal(r.p[l:], sentinels(int64(len(p))-l)) {
			t.Fatalf("the missing tail's bytes were written to")
		}
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
// until the segment the stream sits in holds less than a record of need
// bytes, and returns the offset the next write lands at. The room is
// measured from where the last record ended — the entry's location plus the
// record its own extent took, honest even where a record was cut at the cap —
// not from the segment's account, which stays at its cap until the segment
// is sealed short, truncated or unlinked.
func fillToTail(t *testing.T, h *journalv2.Handle, f *journalv2test.Fixture, id journalv2.FileID, tag string, chunk, need int64) int64 {
	t.Helper()

	var at int64
	for {
		write(t, h, id, at, pattern(fmt.Sprint(tag, at), int(chunk)))
		last := entryAt(t, f, id, at)
		used := last.Location.Offset + writeRecordBytes(int(last.Extent.Len))
		if journalv2test.FixtureSegmentCap-used < need {
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
// accounts moved in whole segment caps, falling by no more than the
// segment's account. The caller reads before before the retire, settles the
// spare pool on both sides, and checks the freed counter per path through
// wantFreed: a footprint that cannot see spares does not tell the unlink
// path from the recycle, both segments being accounted at a whole cap, so
// the move is checked, not the path.
//
// decision: the move is compared modulo whole segment caps and bounded above
// by the account alone, because the journal accounts spares in UsedBytes
// without listing them and Stats does not report them, so an equality to the
// byte races a spare refill the journal is allowed to run between the two
// reads. A refill moves the after read upward, so no correct move falls by
// more than the account. The ceiling this leaves: on the recycle path the
// segment's bytes leave the accounted footprint with the segment itself,
// and a recycle that subtracted the cap only to re-add it as the spare falls
// by exactly one cap, indistinguishable here from a spare refill - the
// footprint check cannot see it; the segment leaving the set and the freed
// counter staying whole are what the recycle path holds. On the unlink path
// a double subtraction falls past the account and fails the bound. The
// congruence holds for a segment accounted at a whole cap, which is what
// the callers pass. Overturned once Stats reports the spares, when the two
// reads subtract them and the fall compares exactly, per path.
func wantRetireAccounted(t *testing.T, f *journalv2test.Fixture, what string, seg journalv2.SegmentID, before, allocated int64) {
	t.Helper()

	wantAccounted(t, f, "after the retirement")
	if _, ok := segmentInfo(f, seg); ok {
		t.Fatalf("%s left segment %d still listed", what, seg)
	}
	after := f.J.Stats().UsedBytes
	if fell := before - after; !sameBut(before, after, 0) || fell > allocated {
		t.Fatalf("%s moved the accounted storage from %d to %d, want a move in whole segment caps that falls by at most the segment's %d account", what, before, after, allocated)
	}
}

// wantFreed checks the freed counter over one retire of a segment accounted
// at allocated, named by what. Freed counts storage actually returned by
// unlinking segments: an unlink returns the segment's whole account to the
// filesystem exactly once - a second debit is a retire that left the bytes
// twice, a short one storage returned with no record of it - while a recycle
// returns the cap to the spare pool, which is not freed, and must not move
// the counter above the nothing it owes.
//
// decision: unlinked names a recycle-fallback unlink too - a journal whose
// recycle is refused may unlink the segment instead, and the bytes were
// still returned to the filesystem - so the exact equality is asked only on
// the calls the seam counted, never on the path the retire meant to take.
func wantFreed(t *testing.T, f *journalv2test.Fixture, what, name string, before uint64, allocated uint64) {
	t.Helper()

	unlinked := f.Seam.CallsFor(journalv2test.OpRemove, name) > 0
	freed := f.J.Stats().Counters.FreedBytes - before
	if unlinked && freed != allocated {
		t.Fatalf("%s that unlinked a segment accounted at %d moved freed by %d, want exactly its account", what, allocated, freed)
	}
	if !unlinked && freed > allocated {
		t.Fatalf("%s that recycled a segment accounted at %d moved freed by %d, want nothing it owes above the account", what, allocated, freed)
	}
}
