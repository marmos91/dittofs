//go:build journal_conformance

package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// The checks here fail by breaking an extreme the ordinary checks never
// reach: empty and enormous writes, overwrites at every scale, removals that
// leave a hole, refusals at an exact fit, and a directory opened from
// nothing.

// edgeMaxOffset is the last byte offset a write can name: the largest int64,
// which markers to the end of a file also use.
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
// the stats keep moves, Since yields no change wherever the empty write
// falls.
//
// decision: the format refuses a zero length only for Deallocate and
// CloneTarget. This check accepts a zero-length write returning nil, which
// draws a version, or ErrZeroLength, and holds the journal to appending
// nothing under either. Overturned when the RFC names the write's outcome.
func TestEdge_ZeroLengthWrite(t *testing.T) {
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		want := pattern(fmt.Sprint("zero", k), 4096)
		v := write(t, h, id, 0, want)
		syncFile(t, h, id)

		before := writeStatsOf(f.J.Stats())
		writes := segmentWrites(t, f)

		for _, off := range []int64{0, 2048} {
			got, err := h.WriteAt(t.Context(), id, off, nil, 0)
			stub(t, "WriteAt", err)
			if err != nil && !errors.Is(err, journalv2.ErrZeroLength) {
				t.Fatalf("share %d: an empty write at %d returned %v, want nil or ErrZeroLength", k, off, err)
			}
			if err == nil && got.Compare(v) <= 0 {
				t.Fatalf("share %d: an empty write at %d drew version %v, not above the write before it (%v)", k, off, got, v)
			}
		}

		if after := segmentWrites(t, f); after != writes {
			t.Fatalf("share %d: %d write calls reached segments over two empty writes, want none", k, after-writes)
		}
		if got := writeStatsOf(f.J.Stats()); got != before {
			t.Fatalf("share %d: two empty writes moved the stats from %+v to %+v", k, before, got)
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

		off := edgeMaxOffset - n + 1
		write(t, h, id, off, p)
		syncFile(t, h, id)
		wantBytes(t, h, id, off, p, "the write ending at the largest offset")

		r := read(t, h, id, off-n, 2*n)
		wantMissing(t, r, off-n, []journalv2.Extent{ext(off-n, n)}, "the range below the write at the largest offset")

		before := writeStatsOf(f.J.Stats())
		writes := segmentWrites(t, f)
		_, err := h.WriteAt(t.Context(), id, edgeMaxOffset-n+2, pattern("over", n), 0)
		stub(t, "WriteAt", err)
		if err == nil {
			t.Fatalf("share %d: a write ending past the largest offset was accepted", k)
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
			{ext(chunk/2, 2*chunk), vNew},
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

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		next := f.J.Stats().NextVersion
		var last journalv2.Version
		for i := range rounds {
			v := write(t, h, id, 0, pattern(fmt.Sprint("flat", k, i), chunk))
			if v.Compare(last) <= 0 {
				t.Fatalf("share %d: overwrite %d drew version %v, not above the last's %v", k, i, v, last)
			}
			last = v
			if i%100 == 99 {
				syncFile(t, h, id)
			}
		}
		rose := f.J.Stats().NextVersion.Counter - next.Counter
		if rose < 2*rounds {
			t.Fatalf("share %d: NextVersion rose by %d over %d overwrites of two files, want at least %d", k, rose, rounds, 2*rounds)
		}

		syncFile(t, h, id)
		s := f.J.Stats()
		if ss := s.Shares[journalv2test.Tag(k)]; ss.HeldBytes != chunk {
			t.Fatalf("share %d: HeldBytes = %d after %d overwrites in place, want the file's %d", k, ss.HeldBytes, rounds, chunk)
		}
		if es := heldEntries(t, f, id); len(es) != 1 || es[0].Extent != ext(0, chunk) || es[0].Version != last {
			t.Fatalf("share %d: the file holds %+v after %d overwrites in place, want one extent at the last version %v", k, es, rounds, last)
		}
		if s.UnreclaimedBytes == 0 && s.Counters.FreedBytes == 0 {
			t.Fatalf("share %d: the superseded records are neither unreclaimed nor freed: their bytes left the accounts", k)
		}
		wantBytes(t, h, id, 0, pattern(fmt.Sprint("flat", k, rounds-1), chunk), "the last of a thousand overwrites")
	}

	// The index still names the two extents the files hold; what it keeps
	// of their thousand superseded ones — removal markers, retained
	// extents — is the implementation's to choose, so long as none reads
	// as held.
	s := f.J.Stats()
	if s.ExtentCount < 2 {
		t.Fatalf("ExtentCount = %d with two extents held, want at least the two live entries", s.ExtentCount)
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
	const chunk = 256 << 10
	f := journalv2test.New(t)

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)

		// Write on until the stream rolls, so the segment it has rolled
		// into holds only one record: the next write must straddle the cap
		// to fit its tail.
		var at int64
		var first journalv2.SegmentID
		for {
			write(t, h, id, at, pattern(fmt.Sprint("cross", k, at), chunk))
			seg := entryAt(t, f, id, at).Location.Segment
			if first == 0 {
				first = seg
			} else if seg != first {
				break
			}
			at += chunk
			if at > 4*journalv2test.FixtureSegmentCap {
				t.Fatalf("share %d: writes of %d bytes never rolled a segment of %d", k, chunk, journalv2test.FixtureSegmentCap)
			}
		}

		off := at + chunk // one record into the new segment
		p := distinct(20+k, 64<<10)
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
			if es := heldIn(t, f, id, seg); len(es) != 1 {
				t.Fatalf("segment %d holds %d of the file's entries, want the probe alone: %+v", seg, len(es), es)
			}
		}

		wantBytes(t, h, id, 0, slices.Concat(pattern("probe", probe), exact, third), "a record sized to its segment")
		other := pattern("other", 4096)
		write(t, f.Share(1), journalv2test.File(1), 0, other)
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
		write(t, h, id, 0, p)
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

		if cs := since(t, h, id, journalv2.Version{}); !sameChanges(cs, []change{{journalv2.ChangeTruncate, toEnd(3000), v}}) {
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
	write(t, ha, a, 0, pa)
	va, err := ha.Deallocate(t.Context(), a, ext(chunk, chunk))
	stub(t, "Deallocate", err)
	if err != nil {
		t.Fatalf("deallocate inside one record: %v", err)
	}

	// B: a hole across three records.
	hb, b := f.Share(1), journalv2test.File(1)
	pb := distinct(1, 3*chunk)
	for i := range 3 {
		write(t, hb, b, int64(i)*chunk, pb[i*chunk:(i+1)*chunk])
	}
	vb, err := hb.Deallocate(t.Context(), b, ext(chunk/2, 2*chunk))
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
	if cs := since(t, ha, a, journalv2.Version{}); len(cs) != 1 {
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
	if es := heldEntries(t, f, a); len(es) != 2 {
		t.Fatalf("file A holds %d held entries after a sync over its deallocation, want 2: %+v", len(es), es)
	}

	// B: the hole across three records, exact.
	r = read(t, hb, b, 0, 3*chunk)
	wantMissing(t, r, 0, []journalv2.Extent{ext(chunk/2, 2*chunk)}, "the hole cut across records")
	if !bytes.Equal(r.p[:chunk/2], pb[:chunk/2]) || !bytes.Equal(r.p[5*chunk/2:], pb[5*chunk/2:]) {
		t.Fatalf("the ranges beside the hole across records were not served as they were")
	}

	if cs := since(t, ha, a, journalv2.Version{}); !sameChanges(cs, []change{{journalv2.ChangeDeallocate, ext(chunk, chunk), va}}) {
		t.Fatalf("Since yielded %+v for the deallocation inside one record, want its exact extent and version", cs)
	}
	if cs := since(t, hb, b, journalv2.Version{}); !sameChanges(cs, []change{{journalv2.ChangeDeallocate, ext(chunk/2, 2*chunk), vb}}) {
		t.Fatalf("Since yielded %+v for the deallocation across records, want its exact extent and version", cs)
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

		// Leg 3: a report of the first half of the offered extent, then the
		// callback returns its own error.
		own := errors.New("the upload failed")
		err = h.Offload(t.Context(), id, v2, journalv2test.FixtureMaxBytes, 0, func(_ context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
			var shard journalv2.Extent
			for _, d := range coalesce(o.Dirty) {
				shard = ext(d.Extent.Off, d.Extent.Len/2)
			}
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
		wantBits(t, f, id, ext(size/2, 3*size/4), false, "the rest of the offered extent, never reported")

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
			if got := f.J.Stats().UnreclaimedBytes - before.UnreclaimedBytes; got < step.e.Len {
				t.Fatalf("share %d: %s: UnreclaimedBytes rose by %d, want at least the released %d", k, step.at, got, step.e.Len)
			}
			if _, ok := segmentInfo(f, seg); !ok {
				t.Fatalf("share %d: %s: segment %d left the journal while part of its record is still held", k, step.at, seg)
			}
			if f.Seam.CallsFor(journalv2test.OpRemove, name)+f.Seam.CallsFor(journalv2test.OpRename, name) > 0 {
				t.Fatalf("share %d: %s: segment %d was retired while part of its record is still held", k, step.at, seg)
			}
		}
		if after := f.J.Stats().UsedBytes; after < usedBefore {
			t.Fatalf("share %d: UsedBytes fell from %d to %d while part of the record is held", k, usedBefore, after)
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
// are durable, without a repack and without one read of it: its storage in
// use falls by its cap and the bytes are marked freed.
func TestEdge_FullyReleasedSegmentReclaimedWithoutRead(t *testing.T) {
	f := journalv2test.New(t)

	h, id := f.Share(0), journalv2test.File(0)
	recs, seg := fillSegment(t, f, 0, 0, 64<<10)
	for _, r := range recs {
		mark(t, f.Share(r.share), r.id, r.extent, r.version)
	}
	syncFiles(t, h, id)
	other := writeSynced(t, f, 1, 1, 0, 64<<10)

	name := segmentName(seg)
	before := f.J.Stats()
	reads := f.Seam.CallsFor(journalv2test.OpRead, name) + f.Seam.CallsFor(journalv2test.OpReadDirect, name)
	beforeInUse := f.J.Stats().UsedBytes

	for _, r := range recs {
		release(t, h, r.id, r.extent)
	}
	syncFiles(t, h, id)
	eventually(t, "a segment released by every record of it was not retired without a repack", func() bool { return retired(f, seg) })

	after := f.J.Stats()
	if after.Counters.FreedBytes <= before.Counters.FreedBytes {
		t.Fatalf("FreedBytes did not rise over the retirement of a fully released segment")
	}
	if got := f.Seam.CallsFor(journalv2test.OpRead, name) + f.Seam.CallsFor(journalv2test.OpReadDirect, name); got != reads {
		t.Fatalf("%d reads of segment %d over its releases and retirement, want the %d before: it was retired through a read", got-reads, seg, reads)
	}
	afterInUse := f.J.Stats().UsedBytes
	if fell := beforeInUse - afterInUse; !sameBut(beforeInUse, afterInUse, journalv2test.FixtureSegmentCap) {
		t.Fatalf("the storage in use fell by %d over the retirement, want one cap %d, spares aside", fell, journalv2test.FixtureSegmentCap)
	}
	_ = other
}

// TestEdge_ReservationExactFitShare checks that a reservation sized to the
// last room a limited share holds is taken, and one record's bytes over is
// refused at once naming the share's limit and the bytes missing, while the
// unlimited share beside it still writes.
func TestEdge_ReservationExactFitShare(t *testing.T) {
	const limit = 4 << 20
	f := journalv2test.New(t)

	b := f.Share(1)
	a := limitedShare(t, f, journalv2test.Tag(0), limit)

	ws, _ := fillShare(t, a, []journalv2.FileID{journalv2test.File(20), journalv2test.File(21)}, 4096)
	consumed := int64(len(ws)) * writeRecordBytes(4096)
	room := limit - consumed
	room &^= 7 // the RFC counts record bytes, padded to its unit
	if room < writeRecordBytes(1) {
		t.Fatalf("precondition: the writes consumed %d of a %d limit, leaving no record's room", consumed, limit)
	}

	n := room - raw.RecordHeaderSize - 8 // the payload whose record takes exactly room
	r := reserve(t, a, writeRecordBytes(int(n)), 1)
	_, err := r.WriteAt(t.Context(), journalv2test.File(0), 0, pattern("fit", int(n)), 0)
	stub(t, "Reservation.WriteAt", err)
	if err != nil {
		t.Fatalf("a write through a reservation of the share's last room: %v", err)
	}
	if got := f.J.Stats().Shares[journalv2test.Tag(0)].UsedBytes; got != limit {
		t.Fatalf("the share's UsedBytes is %d after the reserved write, want its limit %d", got, limit)
	}

	// One padded unit over the smallest record-size step is refused at once,
	// with no call reaching a segment.
	err = refusedAtOnce(t, f, "a reservation over a full share", func() error {
		_, err := a.Reserve(writeRecordBytes(1)+8, 1)
		return err
	})
	ns := refusal(t, err, "a reservation over a full share", journalv2.LimitShare, journalv2.UnitBytes)
	if ns.Missing > 8+writeRecordBytes(1) {
		t.Fatalf("the refusal names %d bytes missing, want at most one padded record's unit over", ns.Missing)
	}

	// One byte over directly, on a fresh share: the missing count is the
	// padded difference, positive and within one unit of the ask.
	fresh := limitedShare(t, f, journalv2test.Tag(2), 4<<20)
	freshWs, _ := fillShare(t, fresh, []journalv2.FileID{journalv2test.File(30), journalv2test.File(31)}, 4096)
	consumed = int64(len(freshWs)) * writeRecordBytes(4096)
	if err := refusedAtOnce(t, f, "a reservation a byte over a full share", func() error {
		_, err := fresh.Reserve(4<<20-consumed+1, 1)
		return err
	}); err == nil {
		t.Fatal("a reservation one byte over the limit was accepted")
	} else {
		if ns := refusal(t, err, "a reservation one byte over a full share", journalv2.LimitShare, journalv2.UnitBytes); ns.Missing > 8+7 {
			t.Fatalf("a reservation one byte over names %d bytes missing, want at most one padded unit", ns.Missing)
		}
	}

	v := write(t, b, journalv2test.File(8), 0, pattern("unlimited", 4096))
	if v == (journalv2.Version{}) {
		t.Fatalf("the unlimited share's write drew no version")
	}
}

// TestEdge_ReservationExactFitJournal checks that at the journal's maximum a
// reservation taking the last segment's bytes is taken, and one needing a
// segment beyond it is refused at once naming the journal's limit and the
// whole missing segment, without reaching a segment.
func TestEdge_ReservationExactFitJournal(t *testing.T) {
	f := journalv2test.New(t)

	fillUntilRefused(t, f, []int{0, 1}, 0, 256<<10)
	s := f.J.Stats()
	atCapacity(t, s)
	capB := int64(journalv2test.FixtureSegmentCap)

	a := limitedShare(t, f, journalv2test.Tag(0), 0) // no share limit below the journal's
	room := capB - (s.UsedBytes % capB)
	if room < writeRecordBytes(1) {
		t.Fatalf("precondition: UsedBytes %d leaves room %d under a cap of %d, want a record's", s.UsedBytes, room, capB)
	}

	// A reservation taking what is left within the maximum succeeds. It is
	// closed untouched, so the refusal leg asks for the segment beyond.
	r, err := a.Reserve(room, 1)
	stub(t, "Reserve", err)
	if err != nil {
		t.Fatalf("a reservation of the journal's last %d bytes was refused: %v", room, err)
	}
	if err := r.Close(); err != nil {
		stub(t, "Reservation.Close", err)
		t.Fatalf("closing the reservation that took the last room: %v", err)
	}

	err = refusedAtOnce(t, f, "a reservation past the journal's maximum", func() error {
		_, err := a.Reserve(capB, 1)
		return err
	})
	ns := refusal(t, err, "a reservation past the journal's maximum", journalv2.LimitJournal, journalv2.UnitBytes, journalv2.UnitSegments)
	if ns.Missing < capB {
		t.Fatalf("the refusal names %d missing, want the whole segment of %d bytes the write needs", ns.Missing, capB)
	}
}

// TestEdge_FilesystemENOSPCIsNotErrNoSpace checks that a write refused by the
// filesystem holding the journal reports the filesystem's own ENOSPC, not the
// journal's no-space refusal, and that the fault cleared the journal writes
// again, the journal's own maximum never reached.
func TestEdge_FilesystemENOSPCIsNotErrNoSpace(t *testing.T) {
	f := journalv2test.New(t)

	// Fill the active segments' tails, then a new segment is what the next
	// write needs, with the journal far below its own maximum.
	hs := []*journalv2.Handle{f.Share(0), f.Share(1)}
	written := false
	for i := range int64(2 * journalv2test.FixtureSegmentCap / (64 << 10)) {
		_, err := tryWrite(t, hs[i%2], journalv2test.File(5+int(i%2)), i/2*64<<10, pattern(fmt.Sprint("near", i), 64<<10))
		stub(t, "WriteAt", err)
		if err != nil {
			wantENOSPC(t, err, "a write before the device is full")
			written = true
			break
		}
	}
	if !written {
		t.Fatal("the waits before the fault never failed, so the fill's bounds are wrong")
	}

	fullDevice(t, f)
	_, err := tryWrite(t, hs[0], journalv2test.File(0), 0, pattern("over", 256<<10))
	wantENOSPC(t, err, "a content write on a full device")

	f.Seam.SetFault(nil)
	if _, err := tryWrite(t, hs[0], journalv2test.File(0), 0, pattern("after", 64<<10)); err != nil {
		stub(t, "WriteAt", err)
		t.Fatalf("a write after the device fault was cleared: %v", err)
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
		h, id := f.Share(k), journalv2test.File(k)
		p1 := pattern(fmt.Sprint("failed", k), 4096)
		v1 := write(t, h, id, 0, p1)
		seg := segmentOf(t, f, id, 0)
		s := f.J.Stats()
		failures, gen, reappended := s.Counters.SyncFailures, s.LossGeneration, s.Counters.SyncReappendedBytes

		f.FailNext(journalv2test.OpSync, segmentName(seg), 1)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		if err != nil && (!errors.Is(err, journalv2.ErrSyncFailed) || !errors.Is(err, syscall.EIO)) {
			t.Fatalf("share %d: a Sync whose segment failed with EIO returned %v, want ErrSyncFailed wrapping it", k, err)
		}
		s = f.J.Stats()
		if s.Counters.SyncFailures <= failures {
			t.Fatalf("share %d: SyncFailures did not rise over a sync that failed", k)
		}
		if s.LastSyncError == nil || !errors.Is(s.LastSyncError.Err, syscall.EIO) {
			t.Fatalf("share %d: LastSyncError is %+v after a sync failed with EIO, want it carrying the error", k, s.LastSyncError)
		}

		// The other stream's file, written before the failure, is untouched
		// by it and syncs clean.
		ho, other := f.Share(1-k), journalv2test.File(1-k)
		po := pattern(fmt.Sprint("other", k), 4096)
		write(t, ho, other, 0, po)
		syncFile(t, ho, other)

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

		// The write of the failed window is kept or lost, and the check
		// accepts either; what it cannot accept is half of it.
		//
		// decision: a journal resolves a failed sync window by re-appending
		// it from a copy it kept or by failing it, and the interface names
		// neither the choice nor a way to set it. The kept leg asserts the
		// re-append's bytes and counter; the lost leg asserts a loss for
		// exactly the write. Overturned when the choice is observable.
		got, missing := readBack(t, h, id, 0, 4096)
		if len(missing) == 0 {
			if !bytes.Equal(got, p1) {
				t.Fatalf("share %d: the write of the failed window reads other bytes than written", k)
			}
			if f.J.Stats().Counters.SyncReappendedBytes <= reappended {
				t.Fatalf("share %d: the failed window reads as kept without SyncReappendedBytes rising", k)
			}
			if f.J.Stats().LossGeneration != gen {
				t.Fatalf("share %d: LossGeneration rose although no write was lost", k)
			}
		} else {
			if !slices.Equal(joinedExtents(missing), []journalv2.Extent{{Off: 0, Len: 4096}}) {
				t.Fatalf("share %d: the write of the failed window reads missing %+v, want its whole extent", k, missing)
			}
			if loss := lastLoss(t, f, s); loss.File != id || loss.Extent != (journalv2.Extent{Off: 0, Len: 4096}) || loss.Version != v1 {
				t.Fatalf("share %d: the loss event is %+v, want the failed window's write at its extent and version", k, loss)
			}
			if f.J.Stats().LossGeneration <= gen {
				t.Fatalf("share %d: LossGeneration did not rise over a lost failed window", k)
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
	beforeTruncate := f.J.Stats().Counters.HeadroomDraws

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
	if afterTruncate <= beforeTruncate {
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
	wantBits(t, f, w.id, w.extent, true, "the extent marked at the maximum")
	entries, err := f.J.Inspect().File(journalv2test.File(12))
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect the deleted file: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the deleted file still holds %+v after a delete at the maximum", entries)
	}

	wantBytes(t, h1, journalv2test.File(11), 0, pattern("pre1", 64<<10), "the other share's file, beside records at the maximum")
	got, missing := readBack(t, h0, journalv2test.File(10), 0, 4096)
	if len(missing) != 0 || !bytes.Equal(got, pattern("pre0", 64<<10)[:4096]) {
		t.Fatalf("the range below the truncate reads missing %+v or other bytes %q", missing, got)
	}
}

// TestEdge_SegmentRetiredByEvictionAndRepack checks that when a segment's
// own retirement, once every record of it is released, and a repack aimed at
// it both select it, the segment leaves the journal once, the storage in use
// falls by exactly one cap, never more, and a second retire that finds the
// work done subtracts nothing, whichever of the two runs first.
func TestEdge_SegmentRetiredByEvictionAndRepack(t *testing.T) {
	for _, firstIs := range []string{"repack", "eviction"} {
		t.Run(firstIs, func(t *testing.T) {
			f := journalv2test.New(t)

			h, id := f.Share(0), journalv2test.File(0)
			s, es := soleSegment(t, f, h, id)
			seg := s.ID
			other := writeSynced(t, f, 1, 1, 0, 64<<10)

			// Hold the segment's removal so the second selector finds the
			// first one still in flight. A retire that recycles the segment
			// instead of unlinking it never reaches the hold; the select
			// below lets both paths on, so the check pins no recycle choice.
			held := f.Seam.Hold(t, journalv2test.OpRemove, segmentName(seg))
			before := f.J.Stats()
			beforeInUse := f.J.Stats().UsedBytes

			var wg sync.WaitGroup
			repacked := make(chan error, 1)
			repack := func() { repacked <- repackSegment(t, f, seg) }
			evict := func() {
				extents := make([]journalv2.Extent, 0, len(es))
				for _, e := range es {
					extents = append(extents, e.Extent)
				}
				mark(t, h, id, extents[0], es[0].Version)
				err := h.MarkOffloaded(t.Context(), id, extents, es[0].Version, es[len(es)-1].Version)
				stub(t, "MarkOffloaded", err)
				if err != nil {
					t.Fatalf("mark the segment's extents offloaded: %v", err)
				}
				if err := h.Release(t.Context(), id, extents); err != nil {
					stub(t, "Release", err)
					t.Fatalf("release the segment's extents: %v", err)
				}
				syncNow(t, h, id)
			}

			if firstIs == "repack" {
				wg.Add(2)
				go func() {
					defer wg.Done()
					repack()
				}()
				select {
				case <-held.Reached():
					go func() {
						defer wg.Done()
						evict()
					}()
				case err := <-repacked:
					if err != nil {
						t.Errorf("the repack that selected the segment first: %v", err)
					}
					go func() {
						defer wg.Done()
						evict()
					}()
				}
			} else {
				wg.Add(1)
				go func() {
					defer wg.Done()
					evict()
				}()

				// The journal's own retirement selects the segment as the
				// releases land; the repack beside it races to the same one.
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := repackSegment(t, f, seg); err != nil {
						t.Errorf("the repack that ran beside the eviction: %v", err)
					}
				}()
				select {
				case <-held.Reached():
				case <-repacked:
				}
			}

			held.Release()
			wg.Wait()
			eventually(t, "the segment was not retired once its removal was let through", func() bool { return retired(f, seg) })

			after := f.J.Stats()
			if got := after.Counters.DoubleRetires - before.Counters.DoubleRetires; got > 1 {
				t.Fatalf("DoubleRetires rose by %d over one segment's double selection, want at most 1", got)
			}
			afterInUse := f.J.Stats().UsedBytes
			if fell := beforeInUse - afterInUse; !sameBut(beforeInUse, afterInUse, journalv2test.FixtureSegmentCap) {
				t.Fatalf("the storage in use fell by %d over the retirement, want one cap %d, spares aside", fell, journalv2test.FixtureSegmentCap)
			}
			if after.UsedBytes < 0 {
				t.Fatalf("UsedBytes = %d after the retirement, want it never below zero", after.UsedBytes)
			}
			_ = other
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

		// Write until the stream rolls, so the segment it rolled into is
		// short enough that a large write into it cannot finish there.
		var at int64
		var first journalv2.SegmentID
		for {
			write(t, h, id, at, pattern(fmt.Sprint("rest", k, at), chunk))
			seg := entryAt(t, f, id, at).Location.Segment
			if first == 0 {
				first = seg
			} else if seg != first {
				break
			}
			at += chunk
			if at > 4*journalv2test.FixtureSegmentCap {
				t.Fatalf("share %d: writes of %d bytes never rolled a segment of %d", k, chunk, journalv2test.FixtureSegmentCap)
			}
		}

		off := at + chunk // one record into the new segment
		p := distinct(20+k, large)
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

	// The power cut between the pieces: a hold on every segment's writes
	// parks the large write once its first piece is placed; the piece whose
	// segment is then synced through the seam survives the crash, at its own
	// offsets, and the rest reads missing, never other bytes.
	//
	// decision: writes are held on every segment file, because which file
	// the second piece names is not observable before the write returns; the
	// first piece's segment is read from the index, the one place the write
	// has reported anything. Overturned when a check can name the second
	// piece's segment before the write returns.
	t.Run("CrashBetweenPieces", func(t *testing.T) {
		f := journalv2test.New(t)
		id := journalv2test.File(0)

		head := distinct(40, 3<<20)
		write(t, f.Share(0), id, 0, head)
		f.Crash() // the head is durable, its segment sealed

		var placed int64
		for _, e := range indexEntries(t, f, id) {
			if e.Kind == journalv2.EntryHeld {
				placed = max(placed, e.Extent.Off+e.Extent.Len)
			}
		}

		held := f.Seam.Hold(t, journalv2test.OpWrite, "*.seg")
		p := distinct(50, 2<<20)
		wrote := make(chan error, 1)
		go func() {
			wrote <- journalv2test.RetrySpare(func() (err error) {
				_, err = f.Share(0).WriteAt(t.Context(), id, placed, p, 0)
				return err
			})
		}()
		<-held.Reached() // the write parked at the hold, its first piece placed

		// The first piece of the new write is the bytes now held past the
		// crash's head, in the segment the seal cut. Its segment is the one
		// synced through the seam; the hold keeps every later piece from
		// reaching its segment before the cut.
		var piece journalv2.IndexEntry
		for _, e := range indexEntries(t, f, id) {
			if e.Kind == journalv2.EntryHeld && e.Extent.Off >= placed {
				piece = e
				break
			}
		}
		if piece.Extent.Len == 0 {
			t.Fatalf("the write parked at its first piece is not in the index: no piece was placed")
		}
		file, err := f.Seam.Open(segmentName(piece.Location.Segment))
		if err != nil {
			t.Fatalf("open the first piece's segment: %v", err)
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			t.Fatalf("sync the first piece's segment through the seam: %v", err)
		}

		crashHeld(t, f, held)
		<-wrote

		h := f.Share(0)
		wantBytes(t, h, id, 0, head, "the head, durable before the cut")
		got, missing := readBack(t, h, id, piece.Extent.Off, piece.Extent.Len)
		if len(missing) != 0 || !bytes.Equal(got, p[piece.Extent.Off-placed:piece.Extent.Off-placed+piece.Extent.Len]) {
			t.Fatalf("the synced piece reads missing %+v or bytes other than its own", missing)
		}
		r := read(t, h, id, piece.Extent.Off+piece.Extent.Len, int64(len(p))-piece.Extent.Len)
		wantMissing(t, r, piece.Extent.Off+piece.Extent.Len,
			[]journalv2.Extent{ext(piece.Extent.Off+piece.Extent.Len, int64(len(p))-piece.Extent.Len)}, "the pieces past the synced one")
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
	syncFiles(t, f.Share(0), journalv2test.File(0))
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

	if err := f.TryReopenAs(journalv2.JournalID{}); err != nil {
		wantIdentityError(t, err, formatID, journalv2test.FixtureInstall, false)
	}
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
	k := journalv2test.New(t)
	k.Shutdown()
	emptyDir(t, k)
	plant(t, k, segmentName(seg), segmentBytes)
	beforeK := dirNow(t, k)
	if _, err := k.TryInit(); !errors.Is(err, journalv2.ErrNotEmpty) {
		stub(t, "Init", err)
		t.Fatalf("an Init on a directory holding a lone segment file returned %v, want ErrNotEmpty", err)
	}
	wantUntouched(t, k, beforeK)
	if err := k.TryReopen(); !errors.Is(err, journalv2.ErrUnidentified) {
		stub(t, "Open", err)
		t.Fatalf("opening a directory holding a lone segment file returned %v, want ErrUnidentified", err)
	}
	wantUntouched(t, k, beforeK)
}

// repackSegment repacks one segment at the cap so a retire is what the call
// does to it, as the C tests' repack of a named segment does.
func repackSegment(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) error {
	t.Helper()

	_, err := f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
	stub(t, "Repack", err)
	return err
}
