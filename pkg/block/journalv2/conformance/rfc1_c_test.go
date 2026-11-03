//go:build journal_conformance

package conformance

// The checks here fail by coming back up describing something other than
// what is on disk.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// cOffload offloads the dirty extents of file id through h, every one a
// positive limit allows, and marks exactly the extents it was offered,
// reported as Offer carries them: an offer's extents are its to name, cut
// as the journal cut them. The offer is returned so a check can re-mark
// from it after losing one.
func cOffload(t *testing.T, h *journalv2.Handle, id journalv2.FileID, through journalv2.Version) (o journalv2.Offer) {
	t.Helper()

	offload(t, h, id, through, journalv2test.FixtureMaxBytes, 0, func(got journalv2.Offer, report func([]journalv2.Extent) error) error {
		o = got
		return report(extentsOf(got.Dirty))
	})
	return o
}

// TestRFC1C_ReseedAfterRepack checks that after a repack has moved an
// extent's record into another segment, `MarkOffloaded` at the versions the
// offload was offered with still marks the extent and permits its release,
// across a crash.
func TestRFC1C_ReseedAfterRepack(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 64 << 10
	e := ext(0, size)

	w := write(t, h, id, 0, distinct(0, size))
	o := cOffload(t, h, id, w)
	repackAll(t, f, journalv2.RepackScope{Share: journalv2test.Tag(0)})
	f.Crash()

	err := f.Share(0).MarkOffloaded(t.Context(), id, []journalv2.Extent{e}, o.Oldest, o.Newest)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark the extent at the offloaded versions after a repack and a crash: %v", err)
	}
	wantBits(t, f, id, e, true, "the extent after the reseed")
	wantRelease(t, f.Share(0), id, e, true)
}

// TestRFC1C_ReleaseSurvivesRestart checks that a release survives a restart:
// once B, written over the offloaded A, is itself offloaded and released,
// a reopen keeps the extent missing rather than serving A's older bytes.
func TestRFC1C_ReleaseSurvivesRestart(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 64 << 10
	e := ext(0, size)

	a := write(t, h, id, 0, distinct(0, size))
	cOffload(t, h, id, a)
	segA := segmentOf(t, f, id, 0)
	sealNow(t, f, segA)

	b := write(t, h, id, 0, distinct(1, size))
	if segB := segmentOf(t, f, id, 0); segB == segA {
		t.Fatalf("the second write landed in segment %d, the same one as the first", segB)
	}
	cOffload(t, f.Share(0), id, b)
	release(t, f.Share(0), id, e)
	f.Reopen()

	r := read(t, f.Share(0), id, 0, size)
	wantMissing(t, r, 0, []journalv2.Extent{e}, "the extent after the restart")
	if bytes.Contains(r.p, distinct(0, size)[:64]) {
		t.Fatalf("the extent serves A's older bytes after the release survived the restart")
	}
}

// TestRFC1C_ReleaseRecordLost checks that a crash before the release record
// syncs leaves the segment in place and the extent held with its original
// bytes.
func TestRFC1C_ReleaseRecordLost(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 64 << 10
	e := ext(0, size)
	p := distinct(0, size)

	w := write(t, h, id, 0, p)
	cOffload(t, h, id, w)
	// The row releases the segment's last held extent, so the segment is
	// sealed first and the release record lands in the stream's next one.
	// Release returns once the record is written, never durable, so the
	// crash below cuts the first sync that could make it durable; the hold
	// is on any segment, since release records never go to a sealed one.
	seg := segmentOf(t, f, id, 0)
	sealNow(t, f, seg)
	name := segmentName(seg)

	err := h.Release(t.Context(), id, []journalv2.Extent{e})
	stub(t, "Release", err)
	if err != nil {
		t.Fatalf("release the last held extent: %v", err)
	}

	hold := f.Seam.HoldFirst(t, journalv2test.OpSync, "*.seg")
	done := make(chan error, 1)
	go func() { done <- h.Sync(t.Context(), id) }()
	<-hold.Reached()
	crashHeld(t, f, hold)
	synced := <-done
	stub(t, "Sync", synced)
	// A Sync racing the crash returns the seam's fs.ErrClosed or the context's
	// error, never a journal refusal that asserts anything.

	// The segment was not unlinked: the release cut by the crash is no
	// acknowledgment that the released bytes are reclaimable, since recovery
	// rebuilds the extent it stood for.
	if !listed(t, f, name) {
		t.Fatalf("segment %s was unlinked before the sync of its last release record made that safe", name)
	}
	if got := f.Seam.CallsFor(journalv2test.OpRemove, name); got != 0 {
		t.Fatalf("%d removes of the sealed segment were attempted before the sync", got)
	}
	r := read(t, f.Share(0), id, 0, size)
	wantServed(t, r, p, "the extent after the release record was lost")
	wantHeld(t, r, []journalv2.Held{{Extent: e, Version: w}}, "the extent after the release record was lost")
	wantBits(t, f, id, e, true, "the extent's offloaded bit after the crash")
}

// TestRFC1C_FillBeatsOlderWrite checks that a Fill of B's bytes at B's
// version beats the older offloaded A it covers: after a crash and reopen
// the extent reads B, never A.
func TestRFC1C_FillBeatsOlderWrite(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 64 << 10
	e := ext(0, size)

	a := write(t, h, id, 0, distinct(0, size))
	syncFile(t, h, id)
	cOffload(t, h, id, a)

	bp := distinct(1, size)
	b := write(t, h, id, 0, bp)
	syncFile(t, h, id)
	cOffload(t, h, id, b)
	release(t, h, id, e)
	syncFile(t, h, id)
	wantExtentMissing(t, h, id, e, "the released extent before the fill")

	fill(t, h, id, 0, bp, b)
	syncFile(t, h, id)
	f.Crash()

	wantBytes(t, f.Share(0), id, 0, bp, "the extent after the crash")
}

// TestRFC1C_StaleFill checks that a Fill at an `asOf` older than the
// content the journal already holds writes nothing, whether the newer
// content came from a write or from a truncate down and back up.
func TestRFC1C_StaleFill(t *testing.T) {
	for _, leg := range []struct {
		name  string
		evict bool // the write leg's evictable content: the truncate leg holds nothing to offload
		newer func(t *testing.T, f *journalv2test.Fixture, h *journalv2.Handle, id journalv2.FileID) journalv2.Version
	}{
		{name: "write", evict: true, newer: func(t *testing.T, f *journalv2test.Fixture, h *journalv2.Handle, id journalv2.FileID) journalv2.Version {
			return write(t, h, id, 0, distinct(1, 4096))
		}},
		{name: "truncate", newer: func(t *testing.T, f *journalv2test.Fixture, h *journalv2.Handle, id journalv2.FileID) journalv2.Version {
			v, err := h.Truncate(t.Context(), id, 0)
			stub(t, "Truncate", err)
			if err != nil {
				t.Fatalf("truncate down: %v", err)
			}
			v2, err := h.Truncate(t.Context(), id, 4096)
			stub(t, "Truncate", err)
			if err != nil {
				t.Fatalf("truncate back up: %v", err)
			}
			if v2.Compare(v) <= 0 {
				t.Fatalf("the truncate back up was given version %+v, not above %+v", v2, v)
			}
			return v2
		}},
	} {
		t.Run(leg.name, func(t *testing.T) {
			f := journalv2test.New(t)
			h, id := f.Share(0), journalv2test.File(0)
			const size = 4096
			e := ext(0, size)

			r := read(t, h, id, 0, size)
			wantMissing(t, r, 0, []journalv2.Extent{e}, "the extent before anything was written")
			asOf := r.asOf
			before := len(heldEntries(t, f, id))

			w := leg.newer(t, f, h, id)
			syncFile(t, h, id)
			if leg.evict {
				cOffload(t, h, id, w)
				release(t, h, id, e)
			}

			p := distinct(2, size)
			err := h.Fill(t.Context(), id, 0, p, asOf, w)
			stub(t, "Fill", err)
			if err != nil && !errors.Is(err, journalv2.ErrStaleFill) {
				t.Fatalf("the stale Fill returned %v, want nothing written or ErrStaleFill", err)
			}

			after := read(t, h, id, 0, size)
			wantMissing(t, after, 0, []journalv2.Extent{e}, "the extent after the stale Fill")
			if !bytes.Equal(after.p, sentinels(size)) {
				t.Fatalf("the stale Fill wrote bytes: %v", after.p)
			}
			if got := len(heldEntries(t, f, id)); got != before {
				t.Fatalf("the stale Fill added %d index entries, want none", got-before)
			}
		})
	}
}

// TestRFC1C_IncrementalReport checks that the report of an offload callback
// is incremental: extents reported mid-callback before the callback fails
// stay marked offloaded and releasable, while the rest of the offer stays
// neither, across a crash.
func TestRFC1C_IncrementalReport(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 32 << 10

	// Two writes sealed into different segments, so the offer spans at least
	// two blocks of the placement index.
	var ws []journalv2.Version
	for i := range 2 {
		ws = append(ws, write(t, h, id, int64(i)*size, distinct(i, size)))
		syncFile(t, h, id)
		sealNow(t, f, segmentOf(t, f, id, int64(i)*size))
	}

	var offer []journalv2.OfferedExtent
	err := h.Offload(t.Context(), id, ws[len(ws)-1], journalv2test.FixtureMaxBytes, 0, func(_ context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
		offer = o.Dirty
		var (
			reported []journalv2.OfferedExtent
			rest     []journalv2.OfferedExtent
		)
		for _, x := range o.Dirty {
			if x.Extent.Off == 0 {
				reported = append(reported, x)
			} else {
				rest = append(rest, x)
			}
		}
		if len(reported) == 0 || len(rest) == 0 {
			t.Errorf("the offer holds %+v, want extents of more than one segment", o.Dirty)
			return nil
		}
		if err := report(extentsOf(reported)); err != nil {
			return err
		}
		return called("Offload callback", errors.New("the offload failed mid-flight"))
	})
	stub(t, "Offload", err)
	if err == nil {
		t.Fatalf("the offload failing mid-callback returned nil")
	}

	// The row asserts the marks the report made and the ones it did not; its
	// records are appends the journal keeps in memory until they sync, so no
	// crash is made here — the row has none, and a crash could lose the
	// reported record on its way to storage on a correct journal too.
	for _, x := range offer {
		wantBits(t, f, id, x.Extent, x.Extent.Off == 0, "the extent after the failed offload")
		if x.Extent.Off != 0 {
			wantRelease(t, f.Share(0), id, x.Extent, false)
		}
	}
	wantRelease(t, f.Share(0), id, ext(0, size), true)
}

// TestRFC1C_EpochHalfIsZero checks that a record whose version carries a
// non-zero epoch half fails the open, as a record of an unknown kind does,
// rather than being rebuilt into the index. The second leg plants a record
// of an unknown kind beside it and asserts the same refusal.
func TestRFC1C_EpochHalfIsZero(t *testing.T) {
	for i, leg := range []struct {
		name   string
		record func(t *testing.T, f *journalv2test.Fixture, identity [16]byte) []byte
	}{
		{name: "epoch half", record: func(t *testing.T, f *journalv2test.Fixture, identity [16]byte) []byte {
			id := newestID(t, f) + 1
			r := raw.Record{
				Identity:     identity,
				Kind:         1,
				Segment:      uint64(id),
				FileID:       journalv2test.File(0).Bytes(),
				Offset:       1 << 20,
				Length:       4096,
				Seq:          1 << 40,
				Version:      raw.Version{Epoch: 1, Counter: 1 << 40},
				Tag:          uint32(journalv2test.Tag(0)),
				RecordOffset: raw.SegmentHeaderSize,
				Write:        &raw.WriteBody{Payload: pattern(fmtTag("e", 0, 0, 1<<20), 4096)},
			}
			return append(raw.SegmentHeader{Identity: identity, ID: uint64(id)}.Bytes(), r.Bytes()...)
		}},
		{name: "unknown kind", record: func(t *testing.T, f *journalv2test.Fixture, identity [16]byte) []byte {
			id := newestID(t, f) + 1
			r := raw.Record{
				Identity:     identity,
				Kind:         15,
				Segment:      uint64(id),
				FileID:       journalv2test.File(0).Bytes(),
				Offset:       1 << 20,
				Length:       4096,
				Seq:          1 << 40,
				Version:      raw.Version{Counter: 1 << 40},
				Tag:          uint32(journalv2test.Tag(0)),
				RecordOffset: raw.SegmentHeaderSize,
				Write:        &raw.WriteBody{Payload: pattern(fmtTag("k", 0, 0, 1<<20), 4096)},
			}
			return append(raw.SegmentHeader{Identity: identity, ID: uint64(id)}.Bytes(), r.Bytes()...)
		}},
	} {
		t.Run(leg.name, func(t *testing.T) {
			// The first leg is watched; the second runs unwatched, the
			// profiler allowing one watched fixture a test.
			var f *journalv2test.Fixture
			if i == 0 {
				f = journalv2test.New(t)
			} else {
				f = journalv2test.NewUnwatched(t)
			}
			writeSynced(t, f, 0, 0, 0, 64<<10)
			f.Shutdown()
			id := newestID(t, f) + 1
			plant(t, f, segmentName(id), leg.record(t, f, journalIdentity(t, f)))

			err := f.TryReopen()
			stub(t, "Open", err)
			if !errors.Is(err, journalv2.ErrUnknownFormat) {
				t.Fatalf("opening with the planted segment returned %v, want ErrUnknownFormat", err)
			}
		})
	}
}

// cSeqMax returns the highest record sequence number among the sealed
// segments' catalogued records on disk, parsed from the record header each
// entry names. An unsealed segment has no catalog to read, and a correct
// journal may leave one behind a crash, so it is skipped.
func cSeqMax(t *testing.T, f *journalv2test.Fixture) uint64 {
	t.Helper()

	var max uint64
	for _, name := range segmentFiles(t, f) {
		b := durable(t, f, name)
		if !endsInTrailer(b) {
			continue
		}
		_, entries := trailerOf(t, b)
		for _, e := range entries {
			at := int64(binary.LittleEndian.Uint32(e[24:28])) // the record's offset in its segment
			if at < raw.SegmentHeaderSize || at+raw.RecordHeaderSize > int64(len(b)) {
				t.Fatalf("%s names a record at %d, which its durable bytes do not hold", name, at)
			}
			if seq := binary.LittleEndian.Uint64(b[at+48 : at+56]); seq > max {
				max = seq
			}
		}
	}
	return max
}

// cVersionMax returns the highest content version of the live index entries
// of every file the index holds.
func cVersionMax(t *testing.T, f *journalv2test.Fixture) journalv2.Version {
	t.Helper()

	var max journalv2.Version
	for _, es := range f.J.Inspect().All() {
		for _, en := range es {
			if max.Compare(en.Version) < 0 {
				max = en.Version
			}
		}
	}
	return max
}

// TestRFC1C_Monotonicity checks that after a crash the next sequence number
// and the next assigned content version each exceed every one on disk, and
// that recovery run in a shuffled segment order yields an identical index.
func TestRFC1C_Monotonicity(t *testing.T) {
	f := journalv2test.NewWithSegmentCap(t, 3<<10)
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		// Several sealed segments per stream, so the shuffle has an order to
		// hide.
		for i := range 3 {
			write(t, h, id, int64(i)*512, distinct(10*k+i, 512))
			syncFile(t, h, id)
			sealNow(t, f, segmentOf(t, f, id, int64(i)*512))
		}
	}
	f.Crash()

	onDisk := cVersionMax(t, f)
	next := f.J.Stats().NextVersion
	if next.Compare(onDisk) <= 0 {
		t.Fatalf("the next version %+v does not exceed the highest on disk %+v", next, onDisk)
	}
	seqMax := cSeqMax(t, f)
	v := write(t, f.Share(0), journalv2test.File(0), 3*512, distinct(30, 512))
	if v.Compare(next) < 0 {
		t.Fatalf("the write after the reopen was given %+v, below the reported next version %+v", v, next)
	}
	// The new record's sequence is read from its own segment's catalog, so
	// the segment is sealed first: an unsealed one carries no catalog.
	syncFile(t, f.Share(0), journalv2test.File(0))
	sealNow(t, f, segmentOf(t, f, journalv2test.File(0), 3*512))
	if got := cSeqMax(t, f); got <= seqMax {
		t.Fatalf("the record written after the reopen carries sequence %d, which does not exceed the highest %d on disk", got, seqMax)
	}

	want := indexSnapshot(t, f)
	for _, seed := range []uint64{1, 2, 3} {
		f.Shutdown()
		f.Seam.ShuffleList(seed)
		f.Reopen()
		wantSameIndex(t, f, want)
	}
}

// TestRFC1C_AttachedShareRaisesCounter checks that attaching a share with a
// version floor raises the journal's counter above it, so the first write
// through the share is assigned a version above the floor, above the
// imported refs a design that attaches without raising would assign.
func TestRFC1C_AttachedShareRaisesCounter(t *testing.T) {
	f := journalv2test.New(t)
	if got := f.J.Stats().NextVersion.Counter; got >= 9e8 {
		t.Fatalf("the fresh journal's counter stands at %d, want one near zero", got)
	}

	tag := journalv2test.Tag(7)
	h, err := f.J.Share(tag, 0, journalv2.Version{Counter: 9e8}, journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach a share at a floor of 9e8: %v", err)
	}

	v := write(t, h, journalv2test.File(0), 0, distinct(0, 4096))
	if v.Counter <= 9e8 || v.Epoch != 0 {
		t.Fatalf("the first write through the attached share was given %+v, want a counter above 9e8", v)
	}
}

// TestRFC1C_UnclaimedTagsAreTheCallersToName checks that a reopen attaches
// no share yet forgets nothing: tags T and P stay listed, G still reads
// under P once P attaches, and F, offloaded under T, serves nothing once
// the caller forgets T.
func TestRFC1C_UnclaimedTagsAreTheCallersToName(t *testing.T) {
	f := journalv2test.New(t)
	tt, pp := journalv2test.Tag(3), journalv2test.Tag(4)

	th, err := f.J.Share(tt, 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach T: %v", err)
	}
	ph, err := f.J.Share(pp, 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach P: %v", err)
	}
	fid, gid := journalv2test.File(0), journalv2test.File(1)

	const size = 64 << 10
	fp := distinct(0, size)
	w := write(t, th, fid, 0, fp)
	cOffload(t, th, fid, w)

	gp := distinct(1, size)
	write(t, ph, gid, 0, gp)
	syncFile(t, ph, gid)

	f.Reopen()

	// U is attached first, and neither T nor P on its own: a journal that
	// forgets every unattached tag at an attach drops them here.
	_, err = f.J.Share(journalv2test.Tag(5), 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach U, which the caller names: %v", err)
	}
	for _, tag := range []journalv2.ShareTag{tt, pp} {
		if _, ok := f.J.Stats().Shares[tag]; !ok {
			t.Fatalf("after attaching U, tag %d is no longer in Stats().Shares: the journal forgot a tag the caller did not name", tag)
		}
	}

	ph, err = f.J.Share(pp, 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach P after the reopen: %v", err)
	}
	wantBytes(t, ph, gid, 0, gp, "G once P attaches")

	err = f.J.Forget(t.Context(), tt)
	stub(t, "Forget", err)
	if err != nil {
		t.Fatalf("forget T, which the caller names: %v", err)
	}

	th, err = f.J.Share(tt, 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("re-attach T: %v", err)
	}
	if got := read(t, th, fid, 0, size); len(got.missing) == 0 {
		t.Fatalf("F still reads through the re-attached tag the caller forgot: %+v", got)
	}
	if got := len(since(t, th, fid, journalv2.Version{})); got != 0 {
		t.Fatalf("Since over F through the forgotten tag yielded %d changes, want none", got)
	}
}

// TestRFC1C_CancelledCallChangesNothingOrCompletes checks what a cancelled
// call leaves behind: with the context already done, WriteAt, Release and
// Forget append nothing and a crash finds nothing of them; cancelled after
// the record is appended, a WriteAt completes and reads back; a cancelled
// Sync returns the context's error and the failure it raced still reports
// on the next Sync; a fill to a share's limit takes no reservation for a
// call with a done context.
func TestRFC1C_CancelledCallChangesNothingOrCompletes(t *testing.T) {
	t.Run("done context appends nothing", func(t *testing.T) {
		f := journalv2test.New(t)
		h, id := f.Share(0), journalv2test.File(0)
		const size = 64 << 10
		e := ext(0, size)

		w := write(t, h, id, 0, distinct(0, size))
		cOffload(t, h, id, w)

		// The forgotten tag is one with content and no handle open, so a
		// journal that checks ErrHandleOpen first cannot fail the leg on an
		// unrelated refusal, and the append the leg forbids is one the
		// journal could otherwise have made.
		qt := journalv2test.Tag(8)
		qh, err := f.J.Share(qt, 0, f.Floor(), journalv2.Bounded)
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach Q, whose content the leg forgets: %v", err)
		}
		qid := journalv2test.File(2)
		qp := distinct(7, size)
		write(t, qh, qid, 0, qp)
		if err := qh.Close(); err != nil {
			t.Fatalf("close Q's handle: %v", err)
		}

		next := f.J.Stats().NextVersion
		writes := segmentCalls(t, f)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = h.WriteAt(ctx, journalv2test.File(1), 0, distinct(9, size), 0)
		stub(t, "WriteAt", err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteAt with a done context returned %v, want the context's error", err)
		}
		err = h.Release(ctx, id, []journalv2.Extent{e})
		stub(t, "Release", err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Release with a done context returned %v, want the context's error", err)
		}
		err = f.J.Forget(ctx, qt)
		stub(t, "Forget", err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Forget with a done context returned %v, want the context's error", err)
		}

		if got := segmentCalls(t, f); !maps.Equal(got, writes) {
			t.Fatalf("the refused calls appended records: %v, want %v", got, writes)
		}
		if got := f.J.Stats().NextVersion; got != next {
			t.Fatalf("the refused calls moved the next version to %+v, want %+v", got, next)
		}
		f.Crash()
		if es := heldEntries(t, f, journalv2test.File(1)); len(es) != 0 {
			t.Fatalf("the refused WriteAt left %d entries after the crash: %+v", len(es), es)
		}
		wantBytes(t, f.Share(0), id, 0, distinct(0, size), "the extent of the refused Release")
		wantBytes(t, f.Share(0), qid, 0, qp, "the content of the refused Forget")
	})

	t.Run("cancelled after the append completes", func(t *testing.T) {
		f := journalv2test.New(t)
		h, id := f.Share(0), journalv2test.File(0)
		const size = 64 << 10
		p := distinct(0, size)

		// No segment is looked up first, since the file holds nothing to
		// find one with: the hold is on any segment's write.
		hold := f.Seam.HoldFirst(t, journalv2test.OpWrite, "*.seg")
		ctx, cancel := context.WithCancel(t.Context())
		type wrote struct {
			v   journalv2.Version
			err error
		}
		done := make(chan wrote, 1)
		go func() {
			v, err := h.WriteAt(ctx, id, 0, p, 0)
			done <- wrote{v, err}
		}()
		<-hold.Reached()
		cancel()
		hold.Release()

		got := <-done
		stub(t, "WriteAt", got.err)
		if got.err != nil {
			t.Fatalf("the WriteAt cancelled after its record was appended returned %v, want its version", got.err)
		}
		wantBytes(t, h, id, 0, p, "the extent of the cancelled write")

		// decision: the seam cannot see the moment the journal commits the
		// record to its own memory, only the write to the segment. The held
		// segment write is the first point at which the record's bytes are
		// committed to storage, so cancelling there is cancelling after the
		// append: the call must complete rather than leave a write the caller
		// was told failed but that recovery serves. Overturned only when the
		// journal gains an observable commit point below the segment write.
	})

	t.Run("a cancelled sync still reports the failure", func(t *testing.T) {
		f := journalv2test.New(t)
		h, id := f.Share(0), journalv2test.File(0)
		const size = 64 << 10
		p := distinct(0, size)
		write(t, h, id, 0, p)
		seg := segmentOf(t, f, id, 0)
		name := segmentName(seg)

		f.FailNext(journalv2test.OpSync, name, 1)
		hold := f.Seam.HoldFirst(t, journalv2test.OpSync, name)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- h.Sync(ctx, id) }()
		<-hold.Reached()
		cancel()
		hold.Release()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled Sync returned %v, want the context's error", err)
		}
		f.Seam.SetFault(nil)
		err := h.Sync(t.Context(), id)
		stub(t, "Sync", err)
		if !errors.Is(err, journalv2.ErrSyncFailed) {
			t.Fatalf("the Sync after the cancelled one returned %v, want the failure it raced", err)
		}
	})

	t.Run("a done-context sync still reports a later failure", func(t *testing.T) {
		f := journalv2test.New(t)
		h, id := f.Share(0), journalv2test.File(0)
		const size = 64 << 10

		// A fresh write, so the second failing sync has its own window on a
		// segment the first failure never touched; the failed segment takes
		// no further append, so the fault is armed on any segment.
		write(t, h, id, 0, distinct(0, size))
		f.FailNext(journalv2test.OpSync, "*.seg", 1)
		before := f.J.Stats().Counters.SyncFailures
		f.Advance(journalv2.SyncBound)
		eventually(t, "the background sync of the bounded policy hits the failing one", func() bool {
			return f.J.Stats().Counters.SyncFailures > before
		})
		f.Seam.SetFault(nil)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := h.Sync(ctx, id)
		stub(t, "Sync", err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync with a done context returned %v, want the context's error", err)
		}
		err = h.Sync(t.Context(), id)
		stub(t, "Sync", err)
		if !errors.Is(err, journalv2.ErrSyncFailed) {
			t.Fatalf("the Sync after the refused one returned %v, want the failure to still report", err)
		}
	})

	t.Run("a done context takes no reservation at the limit", func(t *testing.T) {
		f := journalv2test.New(t)
		tag := journalv2test.Tag(9)
		const limit, chunk = 4 << 10, 1 << 10
		h := limitedShare(t, f, tag, limit)
		id := journalv2test.File(0)

		// The share is filled to its limit with successful writes, until the
		// next is refused by the limit itself: the limit counts the records'
		// header bytes beside their payload, so a first payload as large as
		// the limit would be refused and fill nothing.
		filled := 0
		for ; ; filled++ {
			_, err := tryWrite(t, h, id, int64(filled)*chunk, distinct(filled, chunk))
			if errors.Is(err, journalv2.ErrNoSpace) {
				refusal(t, err, "WriteAt", journalv2.LimitShare)
				break
			}
			stub(t, "WriteAt", err)
			if err != nil {
				t.Fatalf("fill the share to its limit: %v", err)
			}
		}
		if filled == 0 {
			t.Fatalf("the share's first write was refused, so its limit leaves no room for any record")
		}
		before := f.J.Stats().Shares[tag]

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		// The done-context write is larger than anything the limit could
		// hold, so a journal that checked capacity first would answer the
		// share's refusal: the row asks for the context's error, so the
		// context is checked first.
		_, err := h.WriteAt(ctx, id, 64<<10, distinct(filled+90, chunk), 0)
		stub(t, "WriteAt", err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteAt with a done context at the share's limit returned %v, want the context's error", err)
		}
		if got := f.J.Stats().Shares[tag]; got.ReservedBytes != before.ReservedBytes {
			t.Fatalf("the refused call reserved %d bytes, want %d", got.ReservedBytes, before.ReservedBytes)
		}
	})
}

// TestRFC1C_RestampAfterLostStore checks that a restamp to a new
// installation is durable — the format file names the new installation and
// the written file still reads after a reopen — and that a restamp with
// another journal's identity is refused and changes nothing.
func TestRFC1C_RestampAfterLostStore(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	const size = 64 << 10
	p := distinct(0, size)
	write(t, h, id, 0, p)
	syncFile(t, h, id)

	var i2, i3 journalv2.InstallationID
	copy(i2[:], "restamp-2")
	copy(i3[:], "restamp-3")

	err := f.TryRestamp(i2)
	stub(t, "Restamp", err)
	if err != nil {
		t.Fatalf("restamp to the new installation: %v", err)
	}
	f.Reopen()

	form, err := raw.ParseFormat(durable(t, f, "format"))
	if err != nil {
		t.Fatalf("parse the format file after the restamp: %v", err)
	}
	if form.Installation != i2 {
		t.Fatalf("the format file names installation %x, want %x", form.Installation, i2)
	}
	wantBytes(t, f.Share(0), id, 0, p, "the file after the restamp")

	var other journalv2.JournalID
	copy(other[:], "another journal")

	// The row refuses a restamp with another journal's identity against the
	// journal shut, as the fixture's own TryRestamp runs it. It reads no time
	// and no setting, so Restamp ignores every option and none is passed.
	f.Shutdown()
	before := dirNow(t, f)
	err = journalv2.Restamp(t.Context(), f.Dir, other, i3)
	stub(t, "Restamp", err)
	if !errors.Is(err, journalv2.ErrIdentityMismatch) {
		t.Fatalf("restamping with another journal's identity returned %v, want the identity mismatch", err)
	}
	wantUntouched(t, f, before)
	f.Reopen()
	wantBytes(t, f.Share(0), id, 0, p, "the file after the refused restamp")
}

// TestRFC1C_ForgetLeavesNothingToSettle checks that forgetting a tag leaves
// nothing of it to settle: the marker of the deleted file G goes with the
// forget, not a crash later, and a journal brought back up builds no marker
// of T for a later attach's Since to yield; and that a handle open on T
// refuses the forget.
func TestRFC1C_ForgetLeavesNothingToSettle(t *testing.T) {
	f := journalv2test.New(t)
	tt := journalv2test.Tag(3)
	th, err := f.J.Share(tt, 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach T: %v", err)
	}
	fid, gid := journalv2test.File(0), journalv2test.File(1)
	const size = 4096

	fp := distinct(0, size)
	write(t, th, fid, 0, fp)
	gp := distinct(1, size)
	dv := write(t, th, gid, 0, gp)
	syncFile(t, th, gid)

	markers := f.J.Stats().RemovalMarkers
	_, err = th.Delete(t.Context(), gid)
	stub(t, "Delete", err)
	if err != nil {
		t.Fatalf("delete G, settling nothing: %v", err)
	}
	if got := f.J.Stats().RemovalMarkers; got <= markers {
		t.Fatalf("deleting G left the removal marker count at %d, want one more", got)
	}

	// The handle is open, so the forget is refused.
	err = f.J.Forget(t.Context(), tt)
	stub(t, "Forget", err)
	if !errors.Is(err, journalv2.ErrHandleOpen) {
		t.Fatalf("Forget with a handle of T open returned %v, want ErrHandleOpen", err)
	}

	if err := th.Close(); err != nil {
		t.Fatalf("close T's handle: %v", err)
	}
	err = f.J.Forget(t.Context(), tt)
	stub(t, "Forget", err)
	if err != nil {
		t.Fatalf("forget T once its handles are closed: %v", err)
	}
	f.Crash()

	for id, es := range f.J.Inspect().All() {
		for _, en := range es {
			if en.Tag == tt {
				t.Fatalf("entry %+v of file %v still holds T's tag after the forget and the crash", en, id)
			}
		}
	}
	if got := f.J.Stats().RemovalMarkers; got != 0 {
		t.Fatalf("%d removal markers survive the forget and the crash, want none of T", got)
	}

	th, err = f.J.Share(tt, 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach T again after the crash: %v", err)
	}
	if got := len(since(t, th, fid, journalv2.Version{})); got != 0 {
		t.Fatalf("Since over F through the re-attached tag yielded %d changes, want none", got)
	}
	if got := len(since(t, th, gid, journalv2.Version{})); got != 0 {
		t.Fatalf("Since over the deleted G through the re-attached tag yielded %d changes, want none", got)
	}

	// A fresh write under the re-attached tag is held and served across a
	// crash, and its version exceeds the forgotten content's, so the forget
	// settled the old content without burying the tag or restarting its
	// versions at one a caller has already recorded.
	fp2 := distinct(2, size)
	after := write(t, th, fid, 0, fp2)
	if after.Compare(dv) <= 0 {
		t.Fatalf("the write after the forget was given version %+v, not above the forgotten content's %+v", after, dv)
	}
	syncFile(t, th, fid)
	f.Crash()
	wantBytes(t, f.Share(3), fid, 0, fp2, "the new write after the forget")
}

// TestRFC1C_UnclaimedTagReported checks that a tag the reopen did not attach
// is neither forgotten nor lost: Stats still lists it with its held and
// dirty bytes, and forgets it only when the caller names it.
func TestRFC1C_UnclaimedTagReported(t *testing.T) {
	f := journalv2test.New(t)
	tt := journalv2test.Tag(3)
	th, err := f.J.Share(tt, 0, f.Floor(), journalv2.Bounded)
	stub(t, "Share", err)
	if err != nil {
		t.Fatalf("attach T: %v", err)
	}
	hid := journalv2test.File(0)
	const size = 64 << 10
	p := distinct(0, size)
	write(t, th, hid, 0, p)
	syncFile(t, th, hid)

	f.Reopen()
	got, ok := f.J.Stats().Shares[tt]
	if !ok {
		t.Fatalf("the unattached tag T is not in Stats().Shares after the reopen: the journal forgot a tag the caller did not name")
	}
	if got.HeldBytes != int64(size) || got.DirtyBytes != int64(size) {
		t.Fatalf("the unattached tag T reports %d held and %d dirty bytes, want %d and %d", got.HeldBytes, got.DirtyBytes, size, size)
	}

	err = f.J.Forget(t.Context(), tt)
	stub(t, "Forget", err)
	if err != nil {
		t.Fatalf("forget the unattached tag once the caller names it: %v", err)
	}
	if _, ok := f.J.Stats().Shares[tt]; ok {
		t.Fatalf("T is still listed after the caller forgot it")
	}
}

// wantOtherShare fails the test unless err is the refusal of a call on id
// through tag, a share that does not own it, naming owner as the one that
// does.
func wantOtherShare(t *testing.T, err error, call string, id journalv2.FileID, tag, owner journalv2.ShareTag) {
	t.Helper()

	stub(t, call, err)
	var ose *journalv2.OtherShareError
	if !errors.As(err, &ose) || !errors.Is(err, journalv2.ErrOtherShare) {
		t.Fatalf("%s through tag %d on another share's file returned %v, want ErrOtherShare", call, tag, err)
	}
	if ose.File != id || ose.Tag != tag || ose.Owner != owner {
		t.Fatalf("%s through tag %d named file %x, tag %d, owner %d, want file %x, tag %d, owner %d",
			call, tag, ose.File.Bytes(), ose.Tag, ose.Owner, id.Bytes(), tag, owner)
	}
}

// wantConflictRefusal fails the test unless err is the refusal of a call on
// id, a file the journal rebuilt under two tags, made through tag. Which of
// the two tags the journal names as the owner of a conflicted file is the
// contract's to leave open, so only the file and the share called through are
// pinned.
func wantConflictRefusal(t *testing.T, err error, call string, id journalv2.FileID, tag journalv2.ShareTag) {
	t.Helper()

	stub(t, call, err)
	var ose *journalv2.OtherShareError
	if !errors.As(err, &ose) || !errors.Is(err, journalv2.ErrOtherShare) {
		t.Fatalf("%s through tag %d on the conflicted file returned %v, want ErrOtherShare", call, tag, err)
	}
	if ose.File != id || ose.Tag != tag {
		t.Fatalf("%s through tag %d named file %x and tag %d, want file %x and tag %d",
			call, tag, ose.File.Bytes(), ose.Tag, id.Bytes(), tag)
	}
}

// TestRFC1C_FileIsItsSharesShare checks that a file belongs to the share
// whose records built it: no other share can read, change, offload or
// release it, forgetting the owner hands it to no one until reclaimed, a
// file no tag owns yet is claimable by exactly one racing first write, and
// a second tag's records planted beside the first's surface a conflict both
// tags refuse until one is forgotten.
func TestRFC1C_FileIsItsSharesShare(t *testing.T) {
	const size = 64 << 10
	a, b := journalv2test.Tag(0), journalv2test.Tag(1)

	t.Run("every call through the other share is refused", func(t *testing.T) {
		f := journalv2test.New(t)
		ah, bh, fid := f.Share(0), f.Share(1), journalv2test.File(0)
		p := distinct(0, size)
		w := write(t, ah, fid, 0, p)
		e := ext(0, size)

		before := heldEntries(t, f, fid)
		writes := segmentCalls(t, f)
		entriesEqual := func() bool { return slices.Equal(heldEntries(t, f, fid), before) }

		// ReadAt leaves the caller's buffer untouched.
		buf := sentinels(size)
		_, _, _, _, err := bh.ReadAt(t.Context(), fid, 0, buf)
		wantOtherShare(t, err, "ReadAt", fid, b, a)
		if !bytes.Equal(buf, sentinels(size)) {
			t.Fatalf("the refused ReadAt wrote into the caller's buffer")
		}

		wp := distinct(9, size)
		_, err = bh.WriteAt(t.Context(), fid, 0, wp, 0)
		wantOtherShare(t, err, "WriteAt", fid, b, a)
		if !bytes.Equal(wp, distinct(9, size)) {
			t.Fatalf("the refused WriteAt changed the caller's bytes")
		}
		q := sentinels(size)
		err = bh.Fill(t.Context(), fid, 0, q, w, draw(t, f))
		wantOtherShare(t, err, "Fill", fid, b, a)
		if !bytes.Equal(q, sentinels(size)) {
			t.Fatalf("the refused Fill wrote into the caller's bytes")
		}
		_, err = bh.Truncate(t.Context(), fid, size)
		wantOtherShare(t, err, "Truncate", fid, b, a)
		_, err = bh.Deallocate(t.Context(), fid, e)
		wantOtherShare(t, err, "Deallocate", fid, b, a)
		_, err = bh.Delete(t.Context(), fid)
		wantOtherShare(t, err, "Delete", fid, b, a)
		_, err = bh.CloneTarget(t.Context(), fid, journalv2.CloneSpec{
			SrcShare: journalv2.ShareID{1}, Src: journalv2test.File(1), SrcOff: 0, DstOff: 0, Len: size, AsOf: w,
		})
		wantOtherShare(t, err, "CloneTarget", fid, b, a)
		wantOtherShare(t, bh.Offload(t.Context(), fid, w, journalv2test.FixtureMaxBytes, 0, func(_ context.Context, _ journalv2.Offer, _ func([]journalv2.Extent) error) error {
			return called("Offload", errors.New("the extent is another share's"))
		}), "Offload", fid, b, a)
		wantOtherShare(t, bh.Release(t.Context(), fid, []journalv2.Extent{e}), "Release", fid, b, a)
		wantOtherShare(t, bh.MarkOffloaded(t.Context(), fid, []journalv2.Extent{e}, w, w), "MarkOffloaded", fid, b, a)

		if got := segmentCalls(t, f); !maps.Equal(got, writes) {
			t.Fatalf("the refused calls appended records under B: %v, want %v", got, writes)
		}
		if !entriesEqual() {
			t.Fatalf("a call through the other share changed F's index entries: %+v, want %+v", heldEntries(t, f, fid), before)
		}
		wantBytes(t, f.Share(0), fid, 0, p, "F after the refused calls")
	})

	t.Run("forgetting the owner hands the file to no one", func(t *testing.T) {
		f := journalv2test.New(t)
		ah, bh, fid := f.Share(0), f.Share(1), journalv2test.File(0)
		write(t, ah, fid, 0, distinct(0, size))

		err := f.J.Forget(t.Context(), a)
		stub(t, "Forget", err)
		if !errors.Is(err, journalv2.ErrHandleOpen) {
			t.Fatalf("Forget with A's handle open returned %v, want ErrHandleOpen", err)
		}
		if err := ah.Close(); err != nil {
			t.Fatalf("close A's handle: %v", err)
		}
		err = f.J.Forget(t.Context(), a)
		stub(t, "Forget", err)
		if err != nil {
			t.Fatalf("forget A once its handles are closed: %v", err)
		}

		if _, err := bh.WriteAt(t.Context(), fid, 0, distinct(1, size), 0); err != nil {
			stub(t, "WriteAt", err)
			t.Fatalf("the write through B after A was forgotten: %v", err)
		}
	})

	t.Run("offloaded content stays A's until reclaimed", func(t *testing.T) {
		f := journalv2test.New(t)
		ah, bh := f.Share(0), f.Share(1)
		gid := journalv2test.File(1)

		gp := distinct(0, size)
		w := write(t, ah, gid, 0, gp)
		cOffload(t, ah, gid, w)
		// G's write, offloaded and release records all land in G's one
		// active segment: the write leaves it well past the idle-seal size,
		// so repack's own advance seals it before the pass retires it.
		// Sealing it by hand first would strand the release record in a
		// fresh segment too small ever to be reclaimed.
		gseg := segmentOf(t, f, gid, 0)
		release(t, ah, gid, ext(0, size))
		if es := heldEntries(t, f, gid); len(es) != 0 {
			t.Fatalf("G's index entry survived its release: %+v", es)
		}

		err := bh.Fill(t.Context(), gid, 0, sentinels(size), w, draw(t, f))
		wantOtherShare(t, err, "Fill", gid, b, a)
		f.Reopen()
		err = f.Share(1).Fill(t.Context(), gid, 0, sentinels(size), w, draw(t, f))
		wantOtherShare(t, err, "Fill after the reopen", gid, b, a)

		bh = f.Share(1)
		repackAll(t, f, journalv2.RepackScope{})
		if listed(t, f, segmentName(gseg)) {
			t.Fatalf("the segment holding G's records survived the repack, so a record of G is still on disk")
		}
		if _, err := bh.WriteAt(t.Context(), gid, 0, distinct(2, size), 0); err != nil {
			stub(t, "WriteAt", err)
			t.Fatalf("the write of G through B once no segment holds its record: %v", err)
		}
	})

	t.Run("a deleted file is not claimable through its deleter", func(t *testing.T) {
		f := journalv2test.New(t)
		ah, bh := f.Share(0), f.Share(1)
		hID := journalv2test.File(9)

		_, err := bh.Delete(t.Context(), hID)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("delete a file no tag owns through B: %v", err)
		}
		_, err = ah.WriteAt(t.Context(), hID, 0, distinct(0, size), 0)
		wantOtherShare(t, err, "WriteAt", hID, a, b)
	})

	t.Run("a first write claims the file for exactly one racing tag", func(t *testing.T) {
		f := journalv2test.New(t)
		cid := journalv2test.File(4)
		start := make(chan struct{})
		var wg sync.WaitGroup
		out := make(chan error, 2)
		for _, h := range []*journalv2.Handle{f.Share(0), f.Share(1)} {
			wg.Add(1)
			go func(h *journalv2.Handle) {
				defer wg.Done()
				<-start
				_, err := h.WriteAt(t.Context(), cid, 0, distinct(0, size), 0)
				out <- err
			}(h)
		}
		close(start)
		wg.Wait()
		close(out)

		var ok, refused int
		for err := range out {
			if err == nil {
				ok++
				continue
			}
			if !errors.Is(err, journalv2.ErrOtherShare) {
				t.Fatalf("a racing first write returned %v, want success or ErrOtherShare", err)
			}
			refused++
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("%d racing first writes succeeded and %d were refused, want one of each", ok, refused)
		}
	})

	t.Run("a planted second tag surfaces a conflict", func(t *testing.T) {
		g := journalv2test.NewUnwatched(t)
		gh, gid := g.Share(0), journalv2test.File(0)
		p := distinct(0, size)
		w := write(t, gh, gid, 0, p)
		syncFile(t, gh, gid)

		g.Shutdown()
		id := newestID(t, g) + 1
		record := raw.Record{
			Kind: 1, FileID: gid.Bytes(), Offset: 0, Length: size,
			Seq: 1 << 40, Version: raw.Version{Epoch: w.Epoch, Counter: w.Counter}, Tag: uint32(b),
			Write: &raw.WriteBody{Payload: distinct(1, size)},
		}
		// The catalog entry is what a correct open reads a sealed segment by,
		// so without it the record is never rebuilt and no conflict surfaces.
		entry := raw.Entry{
			FileID: gid.Bytes(), Offset: 0, RecordOffset: raw.SegmentHeaderSize, Length: size,
			Seq: 1<<40 + 1, Version: raw.Version{Epoch: w.Epoch, Counter: w.Counter}, Kind: 1, Tag: uint32(b),
		}
		plant(t, g, segmentName(id), sealedSegmentBytes(journalIdentity(t, g), id, []raw.Record{record}, []raw.Entry{entry}))
		g.Reopen()

		if cs := g.J.Stats().TagConflicts; len(cs) != 1 {
			t.Fatalf("the reopen rebuilt the file under two tags, but TagConflicts holds %+v, want one naming F with A and B", cs)
		} else if cs[0].File != gid ||
			(cs[0].Tags != [2]journalv2.ShareTag{a, b} && cs[0].Tags != [2]journalv2.ShareTag{b, a}) {
			t.Fatalf("the conflict holds %+v, want file F with tags A and B in either order", cs[0])
		}

		// Every call on F is refused through either tag, and only which tag
		// the call went through is pinned: which of the two the journal names
		// as the owner of a conflicted file is its own choice, so long as the
		// error is an OtherShareError of this file and this share.
		for _, tag := range []journalv2.ShareTag{a, b} {
			h, err := g.J.Share(tag, 0, g.Floor(), journalv2.Bounded)
			stub(t, "Share", err)
			if err != nil {
				t.Fatalf("attach tag %d over the conflict: %v", tag, err)
			}
			for _, call := range []struct {
				name string
				call func() error
			}{
				{"ReadAt", func() error { _, _, _, _, err := h.ReadAt(t.Context(), gid, 0, sentinels(size)); return err }},
				{"WriteAt", func() error { _, err := h.WriteAt(t.Context(), gid, 0, distinct(2, size), 0); return err }},
				{"Fill", func() error { return h.Fill(t.Context(), gid, 0, sentinels(size), w, draw(t, g)) }},
				{"Truncate", func() error { _, err := h.Truncate(t.Context(), gid, size); return err }},
				{"Deallocate", func() error { _, err := h.Deallocate(t.Context(), gid, ext(0, size)); return err }},
				{"Delete", func() error { _, err := h.Delete(t.Context(), gid); return err }},
				{"CloneTarget", func() error {
					_, err := h.CloneTarget(t.Context(), gid, journalv2.CloneSpec{SrcShare: journalv2.ShareID{1}, Src: journalv2test.File(1), Len: size, AsOf: w})
					return err
				}},
				{"Offload", func() error {
					return h.Offload(t.Context(), gid, w, journalv2test.FixtureMaxBytes, 0, func(_ context.Context, _ journalv2.Offer, _ func([]journalv2.Extent) error) error {
						return errors.New("the conflicted file is not offloadable")
					})
				}},
				{"Release", func() error { return h.Release(t.Context(), gid, []journalv2.Extent{ext(0, size)}) }},
				{"MarkOffloaded", func() error { return h.MarkOffloaded(t.Context(), gid, []journalv2.Extent{ext(0, size)}, w, w) }},
			} {
				wantConflictRefusal(t, call.call(), call.name, gid, tag)
			}
			if err := h.Close(); err != nil {
				t.Fatalf("close the handle over the conflict: %v", err)
			}
		}

		// Forgetting one of the tags clears the conflict and leaves the
		// file to the other. The fixture re-attached A's handle at the
		// reopen, so it is closed first.
		if err := g.Share(0).Close(); err != nil {
			t.Fatalf("close A's re-attached handle: %v", err)
		}
		if err := g.J.Forget(t.Context(), a); err != nil {
			stub(t, "Forget", err)
			t.Fatalf("forget A over the conflict: %v", err)
		}
		if cs := g.J.Stats().TagConflicts; len(cs) != 0 {
			t.Fatalf("the conflict survives forgetting A: %+v", cs)
		}
		h, err := g.J.Share(b, 0, g.Floor(), journalv2.Bounded)
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach B once A is forgotten: %v", err)
		}
		if _, err := h.WriteAt(t.Context(), gid, 0, distinct(3, size), 0); err != nil {
			stub(t, "WriteAt", err)
			t.Fatalf("the write through B once A is forgotten: %v", err)
		}
	})
}

// TestRFC1C_CloneTargetSurvivesCrash checks that a clone target's marker,
// brought back up after a crash, still yields its full spec through Since,
// so an engine resuming the clone has the range to finish rather than
// reading zeros, and that the destination's extent itself is still missing.
func TestRFC1C_CloneTargetSurvivesCrash(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	dst, src := journalv2test.File(0), journalv2test.File(1)
	const size = 64 << 10

	w := write(t, h, src, 0, distinct(0, size))
	syncFile(t, h, src)
	spec := journalv2.CloneSpec{
		SrcShare: journalv2.ShareID{1},
		Src:      src,
		SrcOff:   0,
		DstOff:   0,
		Len:      size,
		AsOf:     w,
	}
	_, err := h.CloneTarget(t.Context(), dst, spec)
	stub(t, "CloneTarget", err)
	if err != nil {
		t.Fatalf("clone target: %v", err)
	}
	syncFile(t, h, dst)
	f.Crash()

	var found bool
	for c, err2 := range f.Share(0).Since(t.Context(), dst, journalv2.Version{}) {
		stub(t, "Since", err2)
		if err2 != nil {
			t.Fatalf("since the destination: %v", err2)
		}
		if c.Kind != journalv2.ChangeCloneTarget {
			continue
		}
		if found {
			t.Fatalf("Since yielded the clone target's marker twice: %+v", c)
		}
		found = true
		if c.Extent != (journalv2.Extent{Off: spec.DstOff, Len: spec.Len}) {
			t.Fatalf("the marker covers %+v, want the clone's destination extent", c.Extent)
		}
		if c.Clone == nil {
			t.Fatalf("the marker carries no clone spec")
		}
		if *c.Clone != spec {
			t.Fatalf("the marker's spec %+v, want the one CloneTarget was given %+v", *c.Clone, spec)
		}
	}
	if !found {
		t.Fatalf("Since yielded no clone-target marker for D")
	}
	r := read(t, f.Share(0), dst, 0, size)
	wantMissing(t, r, 0, []journalv2.Extent{ext(0, size)}, "the destination after the crash")
}

// TestRFC1C_HeaderOnlyRecordsReappended checks that records without payload
// bytes — a truncate and a release — are re-appended whole after a failed
// sync rather than failed: no loss event names their files, the removals
// they describe still read missing before and after a reopen, and the
// failed segment never gains the seal marker its re-appended records would
// have needed to be read back from it.
func TestRFC1C_HeaderOnlyRecordsReappended(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	bid := journalv2test.File(1)
	const size = 64 << 10

	// A's file: write and sync, so the truncate has synced bytes below it.
	aid := journalv2test.File(0)
	write(t, h, aid, 0, distinct(0, size))
	syncFile(t, h, aid)

	// B's offloaded extent, set up beforehand, whose release in the window
	// has no payload to verify.
	bp := distinct(1, size)
	w := write(t, h, bid, 0, bp)
	syncFile(t, h, bid)
	cOffload(t, h, bid, w)

	seg := segmentOf(t, f, aid, 0)
	name := segmentName(seg)
	before := segmentCalls(t, f)
	writesFailed := before[journalv2test.OpWrite+" "+name]

	// The failure is armed and the segment serves stale bytes while the
	// window's two header-only records are still unsynced: a Truncate is
	// otherwise durable on return, so its record would be synced before the
	// failure could cover it.
	f.FailNext(journalv2test.OpSync, name, 1)
	f.Seam.StaleReads(name)
	_, err := h.Truncate(t.Context(), aid, size/2)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("truncate A's file: %v", err)
	}
	err = h.Release(t.Context(), bid, []journalv2.Extent{ext(0, size)})
	f.Seam.SetFault(nil) // the window may have resolved during the Release; the Sync still names it
	stub(t, "Release", err)
	if err != nil {
		t.Fatalf("release B's offloaded extent: %v", err)
	}
	err = h.Sync(t.Context(), aid, bid)
	f.Seam.SetFault(nil)
	stub(t, "Sync", err)

	// A correct journal re-appends both header-only records into another
	// segment and fails nothing: the Sync's window resolves by the re-append,
	// which §5.3 allows to surface as either a nil error or the failure it
	// raced.
	failed := errors.Is(err, journalv2.ErrSyncFailed)
	if err != nil && !failed {
		t.Fatalf("the Sync over the failed window returned %v, want ErrSyncFailed or a re-append", err)
	}
	if got := f.J.Stats().Counters.SyncReappendedBytes; got == 0 {
		t.Fatalf("the window resolved without re-appending a byte of the truncate or the release")
	}

	// The failed segment takes no write of the window, and the re-append
	// lands in another one, an existing segment or a fresh spare alike: its
	// writes are counted under their own segment's name, so a write count
	// rising on any file but the failed one pins it wherever it went.
	after := segmentCalls(t, f)
	if got := after[journalv2test.OpWrite+" "+name]; got != writesFailed {
		t.Fatalf("%d writes were appended to the failed segment during the window, want %d", got, writesFailed)
	}
	prefix := journalv2test.OpWrite + " "
	suffix := " " + name
	rose := false
	for key, n := range after {
		if n > before[key] && strings.HasPrefix(key, prefix) && !strings.HasSuffix(key, suffix) {
			rose = true
		}
	}
	if !rose {
		t.Fatalf("no record of the window was re-appended outside segment %d: %v, want %v with a write count risen", seg, after, before)
	}

	// No record of the window is failed: neither file gains a loss event,
	// whatever names them, and the removals the two records describe still
	// hold before and after a reopen. A's synced half below the truncation
	// stays held from the failed segment by §5.3's rule, so it is exempt.
	for _, l := range f.J.Stats().Losses {
		for _, id := range []journalv2.FileID{aid, bid} {
			if l.File == id {
				t.Fatalf("a loss event names %v, one of the window's files: %+v", id, l)
			}
		}
	}
	if es := heldEntries(t, f, aid); len(es) != 1 || es[0].Extent != ext(0, size/2) {
		t.Fatalf("A holds %+v after the window, want its synced half only", es)
	}

	// Both extents read missing before and after the reopen.
	wantExtentMissing(t, f.Share(0), aid, ext(size/2, size/2))
	wantExtentMissing(t, f.Share(0), bid, ext(0, size))
	f.Reopen()
	wantExtentMissing(t, f.Share(0), aid, ext(size/2, size/2))
	wantExtentMissing(t, f.Share(0), bid, ext(0, size))

	// The failed segment is never sealed, and never takes another record.
	// The journal may have retired it to a spare or unlinked it once its
	// records were carried forward, in which case there are no bytes to
	// read; either way no seal marker caps what is left of it.
	if listed(t, f, name) {
		b := durable(t, f, name)
		if endsInTrailer(b) {
			t.Fatalf("the failed segment ends in a trailer, want a torn tail that no seal marker caps")
		}
		marker := raw.SealMarker{Identity: journalIdentity(t, f), Segment: uint64(seg), Offset: uint64(len(b) - raw.SealMarkerSize)}
		if bytes.HasSuffix(b, marker.Bytes()) {
			t.Fatalf("the failed segment ends in a seal marker")
		}
	}
}

// TestRFC1C_LossSurvivesRestart checks that a loss found at open still
// reports after the restart that found it: the event is in Losses marked
// found at open, the dropped record is not held even once the corruption is
// repaired on disk, and a second reopen reports nothing new.
func TestRFC1C_LossSurvivesRestart(t *testing.T) {
	f := journalv2test.New(t)
	id := journalv2test.File(0)
	const size = 64 << 10
	e := ext(0, size)

	p := distinct(0, size)
	write(t, f.Share(0), id, 0, p)
	syncFile(t, f.Share(0), id)

	loc := entryAt(t, f, id, 0).Location
	name := segmentName(loc.Segment)
	data := liveBytes(t, f, name)
	start := bytes.Index(data[loc.Offset:], p)
	if start < 0 {
		t.Fatalf("the payload is not in segment %d past offset %d", loc.Segment, loc.Offset)
	}
	flipAt(t, f, name, loc.Offset+int64(start)+int64(len(p)/2))

	// The read that finds the corruption raises the loss; the event is not
	// read out before the crash. A successful journal keeps it on disk as
	// a loss record.
	wantExtentMissing(t, f.Share(0), id, e)
	f.Crash()

	losses := f.J.Stats().Losses
	if len(losses) != 1 {
		t.Fatalf("%d loss events after the reopen, want the one found at open: %+v", len(losses), losses)
	}
	l := losses[0]
	if l.File != id || l.Extent != e || l.Reason != journalv2.LossCorrupt || !l.AtOpen {
		t.Fatalf("the loss event %+v, want file %v, extent %+v, found at open as corrupt", l, id, e)
	}

	// Repairing the corruption on disk does not bring the record back: the
	// journal recorded the loss, so the extent is still not held.
	f.Shutdown()
	flipAt(t, f, name, loc.Offset+int64(start)+int64(len(p)/2))
	f.Reopen()
	wantExtentMissing(t, f.Share(0), id, e)

	generation := f.J.Stats().LossGeneration
	seqs := map[uint64]bool{}
	for _, x := range f.J.Stats().Losses {
		seqs[x.SeqNum] = true
	}
	f.Reopen()
	after := f.J.Stats().Losses
	if got := f.J.Stats().LossGeneration; got != generation {
		t.Fatalf("a second reopen raised the loss generation from %d to %d", generation, got)
	}
	for _, x := range after {
		if !seqs[x.SeqNum] {
			t.Fatalf("a second reopen reported the loss %+v as new", x)
		}
	}
}
