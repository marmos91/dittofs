package segment_test

import (
	"bytes"
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// endAppendResult is one concurrent append's pair of answers, gathered over a
// channel so a check can wait for each append's own word.
type endAppendResult struct {
	p   segment.Placed
	err error
}

// endAppend runs one append of a write of 64 bytes at off, of the file with
// the given index, on its own goroutine, returning its end.
func endAppend(e streamEnv, file int, off int64, seed byte) <-chan endAppendResult {
	h, body := endWrite(file, off, seed)
	done := make(chan endAppendResult, 1)
	go func() {
		p, err := e.s.Append(h, body)
		done <- endAppendResult{p: p, err: err}
	}()
	return done
}

// endWrite builds one write record's header and body: a distinct file, an
// offset and a seed, all one place.
func endWrite(file int, off int64, seed byte) (format.RecordHeader, format.RecordBody) {
	return streamWriteHeader(streamFileID(file), off, streamTestBody(64, seed), 7, core.Version{Counter: uint64(file)})
}

// endRawBytes reads sz bytes at offset off of segment id's file, raw through
// the seam's own filesystem, so a check can assert what a scan would find on
// disk rather than what the stream reports.
func endRawBytes(t *testing.T, e streamEnv, id core.SegmentID, off, sz int64) []byte {
	t.Helper()

	f, err := e.seam.FS().Open(streamSegName(id))
	if err != nil {
		t.Fatalf("opening segment %d's file: %v", id, err)
	}
	defer func() { _ = f.Close() }()
	raw := make([]byte, sz)
	n, err := f.ReadAt(raw, off)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reading segment %d's bytes at %d: %v", id, off, err)
	}
	if int64(n) != sz {
		t.Fatalf("reading segment %d's bytes at %d read %d, want %d", id, off, n, sz)
	}
	return raw
}

// endCountNonZero returns how many bytes of p are not zero, so a failure
// message can report the weight of the bytes it saw.
func endCountNonZero(p []byte) int {
	n := 0
	for _, b := range p {
		if b != 0 {
			n++
		}
	}
	return n
}

// endHeaderOf reads a segment's 48-byte header back and decodes it.
func endHeaderOf(t *testing.T, e streamEnv, id core.SegmentID) format.SegmentHeader {
	t.Helper()

	raw := make([]byte, format.SegmentHeaderSize)
	if _, err := e.s.ReadAt(id, raw, 0); err != nil {
		t.Fatalf("ReadAt of segment %d's header: %v", id, err)
	}
	hdr, err := format.DecodeSegmentHeader(raw, streamIdentity)
	if err != nil {
		t.Fatalf("segment %d's header does not decode: %v", id, err)
	}
	return hdr
}

// endZeroSlotScenario runs RFC 28 §4.2's zero-slot choreography and returns
// the environment, the three appends' results in their slot order B, C, D —
// B's and C's writes held at the seam, D completing past them, C failing
// with the seam's EIO at the slot between B and D — the ended segment's id,
// the failed slot's offset and the first record's end. The record A lands
// and is synced first, so the segment holds one synced record before the
// failure, and C's slot is the second record slot of the segment: A's end.
//
// The choreography the RFC's row names — hold the middle append's write and
// let the third complete, then fail the middle one — is staged with two
// one-shot holds: B's write parks at the first, C's at the second, and D's
// passes both and completes. The fault is armed only once D has landed, so
// the rule's one round fires on the first call that consults it armed:
// C's, resumed by the second hold's release. Releasing the first hold then
// lets B's write land into the segment the failure ended.
func endZeroSlotScenario(t *testing.T) (e streamEnv, id core.SegmentID, b, c, d endAppendResult, cOff, aEnd int64) {
	t.Helper()

	seg := streamSegName(1)
	e = streamOver(t, streamCap, func(streamEnv) {}).built(t)
	// Closed by a cleanup rather than the caller's defer: a check that
	// fails inside the scenario must not leave the stream holding the ref
	// the cache's Shutdown waits on.
	t.Cleanup(func() { _ = e.s.Close() })

	// Record A lands and is synced, so the segment and its window have a
	// value the later checks read back.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, e, ah, abody)
	aEnd = placed.Loc.Off + placed.Loc.Len
	id = placed.Loc.Segment
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// B's write parks at the seam: its slot is reserved — the lowest of the
	// three — but its bytes have not landed.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()

	// C's write parks at the second one-shot hold: the first is spent on
	// B's write, so the next write the seam sees — C's, reserved after B's
	// while B's is parked — is the one it stops.
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	cDone := endAppend(e, 3, 128, 0x7d)
	<-holdC.Reached()

	// D's write — reserved after C's, so the highest slot of the three —
	// passes both holds and completes, acknowledged with its place.
	dDone := endAppend(e, 4, 192, 0x13)
	d = <-dDone
	if d.err != nil {
		t.Fatalf("the append past the held ones: %v", d.err)
	}

	// The middle write fails where it stands: the fault is armed once D has
	// landed, so the rule's one round is spent on the first call that
	// consults it — C's, resumed here. C's slot is the third record slot of
	// the segment, right past B's: the lowest slot the failure found still
	// unwritten above the synced record A.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdC.Release()
	c = <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}

	// B's write lands: its record was written into the segment the failure
	// ended, and it is acknowledged with its place. Its end is the failed
	// slot's own offset, for B holds the slot right below C's.
	holdB.Release()
	b = <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	cOff = b.p.Loc.Off + b.p.Loc.Len
	return e, id, b, c, d, cOff, aEnd
}

// TestEndTheZeroSlotRowWriteHalf checks RFC 28 §4.2's zero-slot row, the
// write half (R77, R10, R48): three concurrent appends B, C and D, ordered so
// their slots are B < C < D; B's and C's writes are held at the seam, D
// completes past them, C's write then fails with EIO at its resumed release,
// and B's lands into the segment that failure ended. C returns its EIO, B
// and D return locations in the same segment whose synced-through stays at
// or below C's slot offset, C's slot is zeros on disk, the segment is Ended
// in the ledger, and the next append lands in a new segment naming no
// predecessor — and the ended segment takes no seal marker.
func TestEndTheZeroSlotRowWriteHalf(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, id, b, c, d, cOff, _ := endZeroSlotScenario(t)
	if c.p != (segment.Placed{}) {
		t.Fatal("the failed append returned a location")
	}

	// B and D landed in the segment the failure ended: a record reserved
	// before the roll stays in the segment it reserved in, and the writer
	// owes it its place.
	for name, res := range map[string]endAppendResult{"B": b, "D": d} {
		if res.p.Loc.Segment != id {
			t.Fatalf("append %s landed in segment %d, want the ended segment %d: a record reserved before the roll stays in the segment it reserved in", name, res.p.Loc.Segment, id)
		}
	}
	if b.p.Loc.Off >= cOff || cOff >= d.p.Loc.Off {
		t.Fatalf("the slots came out ordered B %d, C %d, D %d: B must hold the slot below the failed one and D the slot above it", b.p.Loc.Off, cOff, d.p.Loc.Off)
	}

	// The failed slot's offset bounds every record that landed around it:
	// B's and D's synced-through stay at or below it (R10).
	for name, res := range map[string]endAppendResult{"B": b, "D": d} {
		got, _ := streamReadRecord(t, e, res.p.Loc)
		if got.SyncedThrough > cOff {
			t.Fatalf("record %s carries SyncedThrough %d, want at most the failed slot's offset %d", name, got.SyncedThrough, cOff)
		}
	}

	// The failed slot is zeros on disk: the write was refused and its bytes
	// never rewritten (R87).
	if raw := endRawBytes(t, e, id, cOff, d.p.Loc.Len); !bytes.Equal(raw, make([]byte, d.p.Loc.Len)) {
		t.Fatalf("the failed slot holds %d non-zero bytes, want zeros", endCountNonZero(raw))
	}

	// The segment is Ended in the ledger, never sealed.
	if state, err := e.set.State(id); err != nil || state != segment.Ended {
		t.Fatalf("the failed segment's state is %v, %v, want Ended", state, err)
	}

	// The segment takes no seal marker: the bytes at the next free slot are
	// zeros after the next append rolls, and the seam saw no further write
	// or sync of the segment past the three records' own.
	writesBefore := e.seam.CallsFor(journaltest.OpWrite, streamSegName(id))
	syncsBefore := e.seam.CallsFor(journaltest.OpSync, streamSegName(id))
	nextDone := endAppend(e, 5, 0, 0x3c)
	nextRes := <-nextDone
	if nextRes.err != nil {
		t.Fatalf("the append after the failure: %v", nextRes.err)
	}
	if nextRes.p.Loc.Segment == id {
		t.Fatalf("the append after the failure stayed in segment %d, want a rolled-to new one", id)
	}
	if hdr := endHeaderOf(t, e, nextRes.p.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a failed append ends its segment, and the stream continues in a segment naming no predecessor", hdr.Predecessor)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, streamSegName(id)); got != writesBefore {
		t.Fatalf("the ended segment took %d writes in all, want it at %d and still: no seal marker is written into a segment a failure ended", got, writesBefore)
	}
	if got := e.seam.CallsFor(journaltest.OpSync, streamSegName(id)); got != syncsBefore {
		t.Fatalf("the ended segment took %d syncs in all, want it at %d and still", got, syncsBefore)
	}

	// The bytes at the segment's next free slot — where a seal's marker
	// would sit — are zeros. The reservations of B, C and D each advanced
	// the stream's position, so the slot is the end of D's record; the
	// failed C consumed its own reservation the same way.
	nextSlot := d.p.Loc.Off + d.p.Loc.Len
	if raw := endRawBytes(t, e, id, nextSlot, format.SealMarkerSize); !bytes.Equal(raw, make([]byte, format.SealMarkerSize)) {
		t.Fatalf("the ended segment's next free slot holds %d non-zero bytes, want zeros: no seal marker is written into a segment a failure ended", endCountNonZero(raw))
	}
}

// TestEndAShortWriteEndsTheSegment checks R48 on a fresh segment: a WriteAt
// that lands all but 8 bytes and reports nil ends the segment as a refusal
// does, so the next append rolls into a new segment naming no predecessor.
func TestEndAShortWriteEndsTheSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	streamFS := &streamShortFS{FS: newSeam(t).FS(), name: seg}
	e := streamOverFS(t, streamCap, streamFS, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// One record lands whole and is synced, so the stream has a value.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// The next record's write lands short with a nil error: a failure the
	// stream must count, and end its segment for.
	streamFS.armed.Store(true)
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	_, err := e.s.Append(bh, bbody)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("the short Append = %v, want io.ErrShortWrite: a write that lands short with a nil error is a failure", err)
	}

	// The share was not charged for the failed record.
	if got := e.set.ShareBytes(7); got != a.Loc.Len {
		t.Fatalf("the share holds %d bytes after the short write, want the landed record's %d alone: the failed record is never charged", got, a.Loc.Len)
	}

	// The segment is Ended, and the next append lands in a new segment
	// whose predecessor is none.
	if state, err := e.set.State(a.Loc.Segment); err != nil || state != segment.Ended {
		t.Fatalf("the short-written segment's state is %v, %v, want Ended", state, err)
	}
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	c := streamAppendWrite(t, e, ch, cbody)
	if c.Loc.Segment == a.Loc.Segment {
		t.Fatalf("the append after the short write stayed in segment %d, want a rolled-to new one", a.Loc.Segment)
	}
	if hdr := endHeaderOf(t, e, c.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a short write ends its segment as a refusal does", hdr.Predecessor)
	}
}

// TestEndAnEndedSegmentTakesNoSealAndNoAppend checks R1 and R48 on an ended
// segment: after a failed append, Seal — early and full — returns
// ErrSegmentEnded and writes nothing, and Sync returns ErrSegmentEnded,
// while the next append still rolls.
func TestEndAnEndedSegmentTakesNoSealAndNoAppend(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := endFailedAppendEnv(t)
	id, seg := core.SegmentID(1), streamSegName(1)

	// The ended segment takes no seal of either kind, and answers
	// ErrSegmentEnded for each, writing nothing.
	writesBefore := e.seam.CallsFor(journaltest.OpWrite, seg)
	for _, early := range []bool{false, true} {
		if _, err := e.s.Seal(early); !errors.Is(err, segment.ErrSegmentEnded) {
			t.Fatalf("Seal(%v) of the ended segment = %v, want ErrSegmentEnded", early, err)
		}
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, seg); got != writesBefore {
		t.Fatalf("the seals wrote %d more writes into the ended segment, want none", got-writesBefore)
	}

	// Sync answers ErrSegmentEnded: no sync may claim the ended segment's
	// bytes durable.
	if err := e.s.Sync(); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("Sync of the ended segment = %v, want ErrSegmentEnded", err)
	}

	// The next append still rolls: the stream is not wedged on its ended
	// segment.
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	c := streamAppendWrite(t, e, ch, cbody)
	if c.Loc.Segment == id {
		t.Fatalf("the append after the failed one stayed in the ended segment %d, want a rolled-to new one", id)
	}
}

// endFailedAppendEnv builds a stream whose first segment holds one landed,
// synced record and one failed write, so the segment is ended: the base of
// every check that starts past a failed append.
func endFailedAppendEnv(t *testing.T) streamEnv {
	t.Helper()

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	// Closed by a cleanup, not the caller's defer: a refusal inside the
	// builder must not leave the stream holding the ref the cache's
	// Shutdown waits on.
	t.Cleanup(func() { _ = e.s.Close() })

	// One record lands and is synced; the next one's write fails.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	if _, err := e.s.Append(ah, abody); err != nil {
		t.Fatalf("the warm Append: %v", err)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	if state, err := e.set.State(1); err != nil || state != segment.Ended {
		t.Fatalf("the failed segment's state is %v, %v, want Ended", state, err)
	}
	return e
}

// TestEndTheEndedSegmentStaysAtItsCap checks R50 and R43: the ended segment
// is accounted at its full cap, is refused a recycle, and retires exactly its
// cap with its landed records' share bytes released with it.
func TestEndTheEndedSegmentStaysAtItsCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// One landed, synced record; one failed write; the roll past the end.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	nextPayload := streamTestBody(64, 0x7d)
	nh, nbody := streamWriteHeader(streamFileID(3), 0, nextPayload, 7, core.Version{Counter: 3})
	next := streamAppendWrite(t, e, nh, nbody)

	// The ended segment counts exactly its cap in the segments' own
	// footprint, and the new segment its own.
	c := e.set.Counters()
	if got := segmentsBytes(e, c); got != 2*streamCap {
		t.Fatalf("the segments account for %d after the roll, want the ended one and the new one at their caps %d", got, 2*streamCap)
	}

	// Recycle of the ended segment is refused: an ended segment is unlinked
	// when retired, never recycled.
	var fresh core.SegmentID = 1 << 20
	if err := e.set.Recycle(a.Loc.Segment, fresh); !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("Recycle of the ended segment = %v, want ErrWrongState: an ended segment is unlinked, never recycled", err)
	}

	// Retire frees exactly the cap, and the share's bytes of the records the
	// ended segment held — the landed one's alone, for the failed write was
	// never charged — are released with it, leaving the new segment's own.
	shareBefore := e.set.ShareBytes(7)
	freed, err := e.set.Retire(a.Loc.Segment)
	if err != nil {
		t.Fatalf("the retire of the ended segment: %v", err)
	}
	if freed != streamCap {
		t.Fatalf("the retire freed %d, want the ended segment's kept cap %d", freed, streamCap)
	}
	if got := e.set.ShareBytes(7); got != shareBefore-a.Loc.Len {
		t.Fatalf("the share holds %d after the retire, want the ended segment's landed record's %d released", got, shareBefore-a.Loc.Len)
	}
	if got := e.set.ShareBytes(7); got != next.Loc.Len {
		t.Fatalf("the share holds %d after the retire, want the new segment's record's %d alone", got, next.Loc.Len)
	}
}

// TestEndARecordCompletingAfterTheEndIsCharged checks R55 with the ledger's
// own change: an append reserved before the failure that completes after the
// segment is ended in the ledger is charged to its share, for its bytes are
// in the segment.
func TestEndARecordCompletingAfterTheEndIsCharged(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// Record A lands and is synced, so the share carries one charge.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// B's write parks: its reservation is placed, its bytes are not.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()

	// C's write fails, ending the segment in the ledger while B is still
	// in flight.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	cDone := endAppend(e, 3, 128, 0x7d)
	if res := <-cDone; !errors.Is(res.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", res.err)
	}

	// B completes: its bytes are in the ended segment, and the share is
	// charged for them once the append returns.
	if state, err := e.set.State(a.Loc.Segment); err != nil || state != segment.Ended {
		t.Fatalf("the segment's state while B is still held is %v, %v, want Ended", state, err)
	}
	holdB.Release()
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	if got := e.set.ShareBytes(7); got != a.Loc.Len+b.p.Loc.Len {
		t.Fatalf("the share holds %d after the held append completed, want the landed %d and the completed %d together: a record that completes into an ended segment is charged for the bytes it wrote", got, a.Loc.Len, b.p.Loc.Len)
	}
}

// TestEndARecycledSegmentsFailedAppend checks R48's "fresh or recycled": a
// failed append ends a recycled spare the same way, so the next append lands
// in a new segment naming no predecessor and the recycled one is Ended.
func TestEndARecycledSegmentsFailedAppend(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A spare built by hand: a segment file of the cap, its inside filled
	// with non-zero old-life bytes and its header slot and trailer slot
	// zeroed, adopted into the content class before the pool starts, so
	// the stream's first take is the adopted spare.
	const adopted = core.SegmentID(1)
	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(streamCap, nil)
	// The pinned maximum of streamOver's shape holds exactly the two
	// content spares the pool keeps, and the adopted spare already spent
	// one cap of it, so the worker can make just one more: the first take
	// spends the adopted spare — the older of the two — and the refill is
	// refused before any storage is touched. The maker resumes drawing
	// above the adopted id, as open's re-adoption guarantees.
	maker := segment.NewMaker(seam.FS(), set, 2*streamCap, adopted, nil)
	budget := core.NewDescriptorBudget(64)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, budget)
	pool := segment.NewPool(maker, segment.Targets{Content: 2}, nil)
	e := streamEnv{seam: seam, set: set, maker: maker, pool: pool, cache: cache, budget: budget, cap: streamCap}

	// The pool's Adopt accounts the spare in the ledger itself, the way
	// open's re-adoption does.
	if err := pool.Adopt(adopted, segment.Content); err != nil {
		t.Fatalf("the pool's Adopt: %v", err)
	}

	// The adopted spare's file: whole at the cap, old bytes inside, header
	// and trailer slots zeroed, as a recycle's zeroing leaves them.
	f, err := seam.FS().Create(streamSegName(adopted))
	if err != nil {
		t.Fatalf("creating the adopted spare's file: %v", err)
	}
	if err := f.Allocate(0, streamCap); err != nil {
		t.Fatalf("allocating the adopted spare's file: %v", err)
	}
	if _, err := f.WriteAt(streamTestBody(int(streamCap-format.SegmentHeaderSize-format.TrailerSize-16), 0xa5), format.SegmentHeaderSize+8); err != nil {
		t.Fatalf("writing the old-life bytes: %v", err)
	}
	if _, err := f.WriteAt(make([]byte, format.SegmentHeaderSize), 0); err != nil {
		t.Fatalf("zeroing the header slot: %v", err)
	}
	if _, err := f.WriteAt(make([]byte, format.TrailerSize), streamCap-format.TrailerSize); err != nil {
		t.Fatalf("zeroing the trailer slot: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the adopted spare's file: %v", err)
	}

	e.cacheBuilt(t)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	awaitCounters(t, "the adopted and the made content spares ready", pool,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })
	e.s = segment.NewStream(1, streamIdentity, set, pool, cache)
	t.Cleanup(func() { _ = e.s.Close() })

	// The first append lands in the adopted spare.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, abody)
	if a.Loc.Segment != adopted {
		t.Fatalf("the first append landed in segment %d, want the adopted spare %d", a.Loc.Segment, adopted)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// The next write fails there, and the recycled segment is ended.
	if err := seam.Schedule(journaltest.Failing(journaltest.OpWrite, streamSegName(adopted), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", err)
	}
	if err := seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	if state, err := set.State(adopted); err != nil || state != segment.Ended {
		t.Fatalf("the recycled segment's state is %v, %v, want Ended", state, err)
	}

	// The next append lands in a new segment naming no predecessor.
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	c := streamAppendWrite(t, e, ch, cbody)
	if c.Loc.Segment == adopted {
		t.Fatalf("the append after the failure stayed in the recycled segment %d, want a rolled-to new one", adopted)
	}
	if hdr := endHeaderOf(t, e, c.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a failed append ends a recycled segment too", hdr.Predecessor)
	}
}

// TestEndAddRecordServesAnEndedSegment checks the ledger's half of the
// hand-off (R55): AddRecord accounts a record into an Ended segment, whose
// bytes are in storage the segment already covers, and refuses a Sealed one.
func TestEndAddRecordServesAnEndedSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const cap = 4096
	s := segment.NewSet(cap, nil)
	active := core.SegmentID(1)
	ended := core.SegmentID(2)
	sealed := core.SegmentID(3)
	for _, id := range []core.SegmentID{active, ended, sealed} {
		if err := s.AddSpare(id, noMax); err != nil {
			t.Fatalf("AddSpare: %v", err)
		}
		if err := s.Take(id); err != nil {
			t.Fatalf("Take: %v", err)
		}
	}
	if err := s.End(ended); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := s.Seal(sealed, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	const tag = core.ShareTag(7)
	// Active is served, as it always was.
	if err := s.AddRecord(active, tag, 100); err != nil {
		t.Fatalf("AddRecord of an Active segment: %v", err)
	}
	// Ended is served: an append that completes after the failure that
	// ended its segment wrote bytes the segment already covers.
	if err := s.AddRecord(ended, tag, 100); err != nil {
		t.Fatalf("AddRecord of an Ended segment = %v, want the record charged: an append failure ends its segment with appends still in flight, and their bytes belong to the share", err)
	}
	// Sealed is refused: a seal drains every append before it seals, so no
	// charge can arrive past a sealed segment.
	if err := s.AddRecord(sealed, tag, 100); !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("AddRecord of a Sealed segment = %v, want ErrWrongState", err)
	}

	if got := s.ShareBytes(tag); got != 200 {
		t.Fatalf("the share holds %d, want the two charges' 200", got)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after the charges, %v", err)
	}

	// The retired ended segment releases the charge with it.
	if _, err := s.Retire(ended); err != nil {
		t.Fatalf("the retire of the ended segment: %v", err)
	}
	if got := s.ShareBytes(tag); got != 100 {
		t.Fatalf("the share holds %d after the ended segment retired, want the active segment's 100 alone", got)
	}
}
