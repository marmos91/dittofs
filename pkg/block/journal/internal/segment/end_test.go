package segment_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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

// endAttempts bounds the spins an event check makes before it gives up: the
// notifier runs on a goroutine the test cannot hold, so a check that must see
// no event yields this many times to be certain a wrongly-timed hand-over
// would have landed — and one that waits for an event crosses the same bound.
const endAttempts = 1 << 18

// endRecorder is a recording consumer of a stream's end events: a mutex and a
// slice, never blocking, so a check can read what arrived on its own turn.
type endRecorder struct {
	mu sync.Mutex
	ev []segment.EndEvent
}

// OnEnd is the consumer's own method, handed to Stream.OnEnd.
func (r *endRecorder) OnEnd(ev segment.EndEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, ev)
}

// events returns a copy of what arrived so far.
func (r *endRecorder) events() []segment.EndEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ev)
}

// count returns how many events arrived so far, for the checks that poll it.
func (r *endRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ev)
}

// awaitCount fails the test unless at least n events arrived within a bounded
// spin, and returns all that did: a check whose scenario hands over more
// than one event waits for the last of them without pinning the ones before.
func (r *endRecorder) awaitCount(t *testing.T, n int, what string) []segment.EndEvent {
	t.Helper()

	for range endAttempts {
		if r.count() >= n {
			break
		}
		runtime.Gosched()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ev) < n {
		t.Fatalf("%s: %d end event(s) arrived, want at least %d", what, len(r.ev), n)
	}
	return slices.Clone(r.ev)
}

// one returns the single event the recorder holds, failing the test when it
// holds any other count.
func (r *endRecorder) one(t *testing.T) segment.EndEvent {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ev) != 1 {
		t.Fatalf("the consumer holds %d end events, want one", len(r.ev))
	}
	return r.ev[0]
}

// awaitOne fails the test unless exactly one event arrived within a bounded
// spin, and returns it.
func (r *endRecorder) awaitOne(t *testing.T, what string) segment.EndEvent {
	t.Helper()

	for range endAttempts {
		if r.count() == 0 {
			runtime.Gosched()
			continue
		}
		return r.one(t)
	}
	t.Fatalf("%s: no end event arrived within the bounded wait", what)
	return segment.EndEvent{}
}

// awaitNone fails the test if any event arrives within a bounded spin: the
// notifier is a goroutine the test cannot hold, so a check that must see no
// event yields this many times rather than sleeping, certain that a wrongly
// timed hand-over would have landed within them.
func (r *endRecorder) awaitNone(t *testing.T, what string) {
	t.Helper()

	for i := range endAttempts {
		if n := r.count(); n > 0 {
			t.Fatalf("%s: %d end event(s) arrived, want none: %v", what, n, r.events())
		}
		if i%64 == 0 {
			runtime.Gosched()
		}
	}
}

// endEventsStable fails the test unless the recorder's event count stays where
// it stands through a bounded spin: the "no second event" checks read it, for
// the one event a segment hands over has already arrived by the time they run
// and the bound they keep is that nothing more lands.
func endEventsStable(t *testing.T, r *endRecorder, what string) {
	t.Helper()

	n := r.count()
	for i := range endAttempts {
		if got := r.count(); got != n {
			t.Fatalf("%s: the consumer now holds %d end events, want the %d it held: the one end a segment owns hands over once", what, got, n)
		}
		if i%64 == 0 {
			runtime.Gosched()
		}
	}
}

// endZeroSlotWithConsumer runs the zero-slot choreography over a stream whose
// consumer is already registered, and returns the environment, the recording
// consumer, the ended segment's id, the failed slot's offset and the first
// record's end. The env's stream is built the way streamOver builds it — the
// same cap class helpers read — with the consumer handed to it before any
// append, the only time OnEnd accepts one.
func endZeroSlotWithConsumer(t *testing.T) (e streamEnv, rec *endRecorder, id core.SegmentID, cOff, aEnd int64) {
	t.Helper()

	seg := streamSegName(1)
	rec = &endRecorder{}
	e = streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// Record A lands and is synced, so the segment and its window hold a
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

	// C's write parks at the second one-shot hold, D's passes both and
	// lands, and C's fails with the seam's EIO at its resumed release.
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	cDone := endAppend(e, 3, 128, 0x7d)
	<-holdC.Reached()
	dDone := endAppend(e, 4, 192, 0x13)
	d := <-dDone
	if d.err != nil {
		t.Fatalf("the append past the held ones: %v", d.err)
	}
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdC.Release()
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}

	// B's write lands last: its record completed into the segment the
	// failure ended, and its end is the failed slot's own offset.
	holdB.Release()
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	if b.p.Loc.Off >= d.p.Loc.Off {
		t.Fatalf("the slots came out ordered B %d, D %d: B must hold the lower slot", b.p.Loc.Off, d.p.Loc.Off)
	}
	return e, rec, id, b.p.Loc.Off + b.p.Loc.Len, aEnd
}

// TestEndTheEventIsHandedOverOnce checks R49 as epic 8 revises it: a failed
// append — the zero-slot choreography — hands its consumer exactly one end
// event, naming the stream, the ended segment, the append's cause, the failed
// slot, the failure itself and the synced-through the window reads from, and
// no stop. More appends and the stream's Close add no second event.
func TestEndTheEventIsHandedOverOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec, id, cOff, aEnd := endZeroSlotWithConsumer(t)
	ev := rec.awaitOne(t, "the end of the failed append")

	if ev.Stream != 1 {
		t.Fatalf("the event names stream %d, want 1", ev.Stream)
	}
	if ev.Segment != id {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, id)
	}
	if ev.Cause != segment.EndAppend {
		t.Fatalf("the event's cause is %s, want the append of %s", ev.Cause, segment.EndAppend)
	}
	if ev.Failed != cOff {
		t.Fatalf("the event names the failed slot %d, want C's own offset %d", ev.Failed, cOff)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the fault's EIO", ev.Err)
	}
	if ev.SyncedThrough != aEnd {
		t.Fatalf("the event's synced-through is %d, want A's synced value %d: the window the durability code reads is the records at or above it", ev.SyncedThrough, aEnd)
	}
	if ev.Stopped {
		t.Fatal("the event names the stream stopped, want it rolling")
	}

	// More appends and the stream's Close add no second event: the ended
	// segment's one end is the one word its consumer ever gets.
	nextPayload := streamTestBody(64, 0x3c)
	nh, nbody := streamWriteHeader(streamFileID(5), 0, nextPayload, 7, core.Version{Counter: 5})
	if _, err := e.s.Append(nh, nbody); err != nil {
		t.Fatalf("the append past the end: %v", err)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the Sync of the new segment: %v", err)
	}
	endEventsStable(t, rec, "the append past the end")
	if err := e.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	endEventsStable(t, rec, "the Close")
}

// TestEndTheEventWaitsForAnEarlierSlotStillWriting checks the wait the
// hand-over makes: the failure's end holds its event until every slot the
// segment reserved has completed, the earlier-held B's among them. While B
// is still held at the seam no event arrives, the failed append has already
// returned, and another file's append on a second stream of the same Streams
// completes — the wait parks on the hand-over's own cond, off every appender's
// locks. Releasing B hands the event, and B has returned its location by
// then.
func TestEndTheEventWaitsForAnEarlierSlotStillWriting(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// Two streams share the seam, so the check can prove one stream's held
	// write never blocks another's append: the second stream takes a spare
	// outside the pinned maximum — build over the open rollMax, whose
	// worker keeps the class ready, so the other stream's append needs no
	// hand-holding to land.
	e := rollBuild(t, streamCap, segment.Targets{Content: 2}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	ss := segment.NewStreams(2, streamIdentity, e.set, e.pool, e.cache)
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Errorf("the streams' Close: %v", err)
		}
	})
	sa, sb := ss.Stream(0), ss.Stream(1)
	rec := &endRecorder{}
	sa.OnEnd(rec.OnEnd)

	// Record A lands and is synced on the first stream, so its end
	// event's window has a value the checks read back; the segment it
	// takes is the one the holds below work on.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	se := streamEnv{seam: e.seam, set: e.set, maker: e.maker, cache: e.cache, budget: e.budget, s: sa, cap: streamCap}
	aPlaced := streamAppendWrite(t, se, ah, abody)
	if err := sa.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}
	seg := streamSegName(aPlaced.Loc.Segment)

	// B parks at the seam; C fails. The fault is armed once B's write is
	// parked and C's is set loose, so the rule's one round fires on C's
	// consult alone.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(se, 2, 64, 0x55)
	<-holdB.Reached()
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	cDone := endAppend(se, 3, 128, 0x7d)
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}

	// While B's write is still held, no event arrives; the failed append
	// has returned; and the second stream completes an append of its own,
	// for the hand-over's wait holds no appender's lock.
	rec.awaitNone(t, "the event while an earlier slot still writes")
	otherDone := make(chan error, 1)
	go func() {
		payload := streamTestBody(64, 0x99)
		h, body := streamWriteHeader(streamFileID(6), 512, payload, 7, core.Version{Counter: 6})
		_, err := sb.Append(h, body)
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("the append of another file's stream during the hold: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second stream's append never returned while the first stream's end waited on a held write: the hand-over's wait holds an appender's lock")
	}

	// Releasing B hands the event over — and B has returned before it, for
	// its completion is what the wait was waiting for.
	holdB.Release()
	ev := rec.awaitOne(t, "the end after the earlier slot completed")
	if ev.Cause != segment.EndAppend {
		t.Fatalf("the event's cause is %s, want the append of %s", ev.Cause, segment.EndAppend)
	}
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	if ev.Failed != b.p.Loc.Off+b.p.Loc.Len {
		t.Fatalf("the event names the failed slot %d, want the held record's end %d", ev.Failed, b.p.Loc.Off+b.p.Loc.Len)
	}
}

// TestEndTheEventWaitsForALaterSlotToo checks decision 2: the hand-over's
// wait covers the slots reserved after the failed one as well as those before
// it. D — reserved above C, its write still parked at the seam when C's fails
// — holds the event until it lands, so the record a later append completed is
// never lost from the window. A notifier that waited only for the slots
// below the failure would hand the event over while D still writes, and the
// second no-event check below catches it.
func TestEndTheEventWaitsForALaterSlotToo(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	rec := &endRecorder{}
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// A lands and is synced, so the end event's window has a value.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// The three one-shot holds take the writes in their reservation order —
	// B parks at the first, C at the second, D at the third — so every slot
	// is reserved, none has completed, and D's reservation stands above
	// the failed one's while its own write is still parked.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	cDone := endAppend(e, 3, 128, 0x7d)
	<-holdC.Reached()
	holdD := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	dDone := endAppend(e, 4, 192, 0x13)
	<-holdD.Reached()

	// The fault is armed while every write is parked: each consults it only
	// at its hold's release, so the rule's one round fires on the first
	// released write — C's — and leaves B's and D's to pass.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdC.Release()
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}

	// While B — a slot reserved before the failed one — is still held, no
	// event arrives.
	rec.awaitNone(t, "the event while the lower slot still writes")

	// B lands into the segment its fellow append's failure ended, and still
	// no event arrives: D, reserved above the failed slot, is still
	// writing, and the wait covers it too.
	holdB.Release()
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	rec.awaitNone(t, "the event while the later slot still writes")

	// D lands, the last reservation the segment held completes, and the
	// event hands over at once.
	holdD.Release()
	d := <-dDone
	if d.err != nil {
		t.Fatalf("the later held Append: %v", d.err)
	}
	ev := rec.awaitOne(t, "the end after the later slot completed")
	if ev.Cause != segment.EndAppend {
		t.Fatalf("the event's cause is %s, want the append of %s", ev.Cause, segment.EndAppend)
	}
	cOff := b.p.Loc.Off + b.p.Loc.Len
	if ev.Failed != cOff {
		t.Fatalf("the event names the failed slot %d, want C's own offset %d", ev.Failed, cOff)
	}
	if b.p.Loc.Off >= cOff || cOff >= d.p.Loc.Off {
		t.Fatalf("the slots came out ordered B %d, C %d, D %d: B must hold the slot below the failed one and D the slot above it", b.p.Loc.Off, cOff, d.p.Loc.Off)
	}
}

// TestEndASealEndedSegmentSendsOneEvent checks the seal's ends: a seal whose
// record sync fails ends its segment and hands one event naming the seal as
// its cause, and an explicit Seal of a segment a failed write already ended
// adds no second event — it is refused with ErrSegmentEnded and owns no end
// of its own.
func TestEndASealEndedSegmentSendsOneEvent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The seal's first sync — the records' — fails on the segment the roll
	// seals, as in the roll checks: the straddling write triggers a roll
	// whose seal's record sync the fault takes.
	e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	rec := &endRecorder{}
	e.s.OnEnd(rec.OnEnd)

	warm := make([]byte, 1024)
	wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
	p1 := streamAppendWrite(t, e.stream(t), wh, wb)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, streamSegName(p1.Loc.Segment), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	straddle := streamTestBody(3072, 0xa5)
	h, body := streamWriteHeader(streamFileID(2), 4096, straddle, 7, core.Version{Counter: 2})
	p2, err := e.s.AppendRoll(h, body, segment.Roll{})
	if err != nil {
		t.Fatalf("the straddling AppendRoll: %v", err)
	}
	if p2.N == 0 || p2.N >= int64(len(straddle)) {
		t.Fatalf("the straddling AppendRoll placed %d bytes, want a piece", p2.N)
	}

	// The append of the rest carries the seal's ended failure, and the one
	// event names the seal, not the seal's caller.
	rest := straddle[p2.N:]
	h2, b2 := streamWriteHeader(streamFileID(2), 4096+p2.N, rest, 7, core.Version{Counter: 2})
	if _, err := e.s.AppendRoll(h2, b2, segment.Roll{}); err == nil {
		t.Fatal("the AppendRoll of the rest answered nil, want the seal's ended failure carried to its caller")
	} else if !errors.Is(err, segment.ErrSegmentEnded) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("the AppendRoll of the rest = %v, want ErrSegmentEnded carrying the fault's EIO", err)
	}
	ev := rec.awaitOne(t, "the end of the failed seal")
	if ev.Segment != p1.Loc.Segment {
		t.Fatalf("the event names segment %d, want the seal-ended one %d", ev.Segment, p1.Loc.Segment)
	}
	if ev.Cause != segment.EndSeal {
		t.Fatalf("the event's cause is %s, want the seal of %s", ev.Cause, segment.EndSeal)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the fault's EIO", ev.Err)
	}
	if !errors.Is(ev.Err, segment.ErrSegmentEnded) {
		t.Fatalf("the event's error is %v, want the seal's ended sentinel named", ev.Err)
	}

	// An explicit Seal of a segment a failed write already ended is refused
	// and owns no end of its own: the failed append's event covered the
	// segment, and no second one arrives for the seal.
	recSeal := &endRecorder{}
	seg := streamSegName(1)
	se := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = se.s.Close() })
	se.s.OnEnd(recSeal.OnEnd)
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, abody)
	if err := se.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("arming the write fault on the seal's env: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := se.s.Append(bh, bbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append on the seal's env = %v, want the fault's EIO", err)
	}
	recSeal.awaitOne(t, "the write-ended segment's end")
	if _, err := se.s.Seal(false); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("Seal of the write-ended segment = %v, want ErrSegmentEnded", err)
	}
	endEventsStable(t, recSeal, "the Seal of an already-ended segment")
}

// TestEndRequestedAfterAFailedSync checks the durability code's own request:
// a failed Sync is answered by an End of the segment it failed on — cause
// handed, one event naming the requested end and the stream's next free slot
// as the end of the window — and the next append rolls to a segment naming
// no predecessor. A second End of the same segment returns nil with no event,
// an End of a sealed segment is refused, an End past the Close is refused,
// and an End with no cause panics.
func TestEndRequestedAfterAFailedSync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	rec := &endRecorder{}
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// One record lands; the next Sync fails.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, e, ah, abody)
	next := placed.Loc.Off + placed.Loc.Len
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	syncErr := fmt.Errorf("the failed sync: %w", syscall.EIO)
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Sync = %v, want the fault's EIO", err)
	}

	// The durability code ends the segment itself. One event arrives,
	// naming the requested cause, the failure, and the stream's next free
	// slot as the end of every reservation the segment holds.
	if err := e.s.End(placed.Loc.Segment, syncErr); err != nil {
		t.Fatalf("End of the sync-failed segment = %v, want nil", err)
	}
	ev := rec.awaitOne(t, "the requested end")
	if ev.Segment != placed.Loc.Segment {
		t.Fatalf("the event names segment %d, want %d", ev.Segment, placed.Loc.Segment)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if ev.Failed != next {
		t.Fatalf("the event names the failed slot %d, want the stream's next free slot %d: no write failed, so the end is the end of every reservation the segment held", ev.Failed, next)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the caller's cause", ev.Err)
	}

	// The next append rolls to a segment naming no predecessor.
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the sync fault: %v", err)
	}
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	c := streamAppendWrite(t, e, ch, cbody)
	if c.Loc.Segment == placed.Loc.Segment {
		t.Fatalf("the append after the requested end stayed in segment %d, want a rolled-to new one", placed.Loc.Segment)
	}
	if hdr := endHeaderOf(t, e, c.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a requested end names no successor either", hdr.Predecessor)
	}

	// A second End of the same segment is a no-op nil, not a second event.
	if err := e.s.End(placed.Loc.Segment, syncErr); err != nil {
		t.Fatalf("the second End of the ended segment = %v, want nil", err)
	}
	endEventsStable(t, rec, "the second End")

	// An End of a sealed segment is refused, and sends nothing. The new
	// segment is sealed whole: a full Seal keeps its cap, and the segment
	// the stream now holds is rolled past first.
	dPayload := streamTestBody(64, 0x3c)
	dh, dbody := streamWriteHeader(streamFileID(4), 0, dPayload, 7, core.Version{Counter: 4})
	d := streamAppendWrite(t, e, dh, dbody)
	if _, err := e.s.Seal(false); err != nil {
		t.Fatalf("the Seal of the new segment: %v", err)
	}
	if err := e.s.End(d.Loc.Segment, syncErr); !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("End of the sealed segment = %v, want ErrWrongState", err)
	}
	endEventsStable(t, rec, "the End of a sealed segment")

	// An End past the stream's Close is refused.
	if err := e.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.s.End(d.Loc.Segment, syncErr); !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("End past the Close = %v, want ErrStreamClosed", err)
	}

	// An End with no cause is a caller bug.
	closed := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		_ = e.s.End(d.Loc.Segment, nil)
		return
	}
	if !closed() {
		t.Fatal("End with a nil cause returned, want a panic: a nil cause names no failure")
	}
}

// TestEndRequestedWaitsForAWriteInFlight checks the requested end's wait: End
// returns while a write is still held at the seam — the request is not held
// hostage to a write it never owned — and the event hands over only once that
// write completes.
func TestEndRequestedWaitsForAWriteInFlight(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	rec := &endRecorder{}
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// One record lands, so the stream holds its first segment; the Sync
	// after it fails, so the durability code may ask for the end.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, e, ah, abody)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Sync = %v, want the fault's EIO", err)
	}

	// B's write parks at the seam: its reservation is placed, its bytes are
	// not. The durability code asks for the end while B is still in
	// flight.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()
	endDone := make(chan error, 1)
	go func() {
		endDone <- e.s.End(placed.Loc.Segment, fmt.Errorf("the failed sync: %w", syscall.EIO))
	}()
	select {
	case err := <-endDone:
		if err != nil {
			t.Fatalf("End during the held write: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("End never returned while a write was held at the seam: the requested end waits on a write it never owned")
	}

	// The event waits for the held write's completion before it hands over.
	rec.awaitNone(t, "the requested end's event while the write still writes")
	holdB.Release()
	ev := rec.awaitOne(t, "the requested end after the held write completed")
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if ev.Failed != b.p.Loc.Off+b.p.Loc.Len {
		t.Fatalf("the event names the failed slot %d, want the next free slot past the held record %d", ev.Failed, b.p.Loc.Off+b.p.Loc.Len)
	}
}

// TestEndCloseWaitsForTheNotifier checks the join Close makes: the consumer
// runs on the notifier's own goroutine, and a Close that races a consumer
// still running its event does not return until the consumer has.
func TestEndCloseWaitsForTheNotifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// The consumer blocks on a channel until the test lets it pass; it is
	// registered before anything appends, as OnEnd's gate demands.
	blocked := make(chan struct{})
	entered := make(chan struct{})
	e.s.OnEnd(func(ev segment.EndEvent) {
		close(entered)
		<-blocked
	})

	// One record lands; the next write fails, ending the segment and
	// starting a notifier whose consumer blocks on the channel.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, ah, abody)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", err)
	}

	// The consumer has its event and parks on the channel. Close must not
	// return until it has.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the consumer never received the end event")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	for i := range endAttempts {
		select {
		case err := <-closed:
			t.Fatalf("Close returned %v while the consumer still ran", err)
		default:
		}
		if i%64 == 0 {
			runtime.Gosched()
		}
	}
	close(blocked)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close after the consumer passed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned after the consumer was let pass")
	}
}

// TestEndOnEndAfterTheFirstAppendPanics checks OnEnd's own gate: the consumer
// is registered before the first append, and a later call — after a
// reservation, a roll or an end — panics rather than racing an event already
// in flight.
func TestEndOnEndAfterTheFirstAppendPanics(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// Before the first append, registering is free — twice over, for the
	// caller that changes its mind.
	e.s.OnEnd(nil)
	e.s.OnEnd(func(segment.EndEvent) {})

	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, ah, abody)

	// After the first reservation, registering a consumer is a bug: an
	// event already in flight would be handed to a consumer that saw none
	// of the segment's history.
	late := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		e.s.OnEnd(func(segment.EndEvent) {})
		return
	}
	if !late() {
		t.Fatal("OnEnd after the first append returned, want a panic")
	}
}

// TestEndEveryEndedSegmentHandsOneEventUnderRace checks the one-end contract
// under failure: appenders over two streams, a fault that fails segment-file
// writes at random, and rolls in between; every event names a segment that is
// Ended in the ledger, no segment is named twice — the one end the stream
// owns over it — and the race detector reads clean.
//
// The env is built by hand rather than rollBuild for one difference: its
// descriptor budget is wide. A segment a failed write ended never syncs
// again — Sync answers ErrSegmentEnded — so its descriptor keeps the dirty
// pin until the process ends, and the fault's ended segments alone can pin
// more descriptors than the shared builder's 64.
func TestEndEveryEndedSegmentHandsOneEventUnderRace(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const raceCap = int64(4096)
	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(raceCap, nil)
	maker := segment.NewMaker(seam.FS(), set, streamMax, 0, nil)
	budget := core.NewDescriptorBudget(512)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, budget)
	pool := segment.NewPool(maker, segment.Targets{Content: 4}, nil)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	awaitCounters(t, "the content spares ready", pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 4 })
	ss := segment.NewStreams(2, streamIdentity, set, pool, cache)
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Errorf("the streams' Close: %v", err)
		}
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	rec := &endRecorder{}
	ss.OnEnd(rec.OnEnd)

	// The fault: a quarter of every write of a segment file fails with EIO,
	// so the ends and the rolls interleave under the streams' own locks. The
	// maker's writes run under its spare names, which no *.seg rule matches,
	// so the fault leaves the spare making alone.
	if err := seam.Schedule(journaltest.FailingRandomly(journaltest.OpWrite, "*.seg", 0.25, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	const appenders = 8
	const perAppender = 24
	var failed atomic.Int64
	var wg sync.WaitGroup
	for i := range appenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := streamFileID(i)
			st := ss.For(f)
			for j := range perAppender {
				payload := streamTestBody(64, byte(i*16+j))
				h, body := streamWriteHeader(f, int64(j)*64, payload, 7, core.Version{Counter: uint64(i*perAppender + j + 1)})
				for {
					_, err := st.AppendRoll(h, body, segment.Roll{})
					if err == nil {
						// A landed record syncs its segment here, so the
						// descriptor a clean roll leaves behind loses its
						// dirty pin and the budget stays wide for the
						// ended ones, which never sync again.
						if serr := st.Sync(); serr != nil && !errors.Is(serr, segment.ErrSegmentEnded) {
							t.Errorf("appender %d's sync: %v", i, serr)
							return
						}
						break
					}
					// Every failure but a spare drought ends a segment — the
					// plain EIO of a write the fault refused, the ended-seal word
					// of a roll whose seal found one, the stream's own stop — and
					// the events below must match them one for one, so each counts
					// and the appender moves on: no retry serves any of them, for a
					// stopped stream takes no further spare.
					if errors.Is(err, syscall.EIO) || errors.Is(err, segment.ErrSegmentEnded) || errors.Is(err, segment.ErrStreamStopped) {
						failed.Add(1)
						break
					}
					var ns *core.NoSpaceError
					if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
						t.Errorf("appender %d's append: %v", i, err)
						return
					}
					pool.Kick()
					ready := false
					for k := range poolAttempts {
						if k%4096 == 0 {
							pool.Kick()
						}
						runtime.Gosched()
						if c := pool.Counters(); c.Spares+c.HeadroomBytes/raceCap+c.RepackReserveBytes/raceCap > 0 {
							ready = true
							break
						}
					}
					if !ready {
						t.Errorf("appender %d's roll found no spare again: %v", i, err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// The streams close before any event is read, so every notifier the run
	// started is joined and its event handed over: reading them earlier
	// races the last ends' own hand-overs against the checks below, and the
	// join is what the closed do — the streams never serve again.
	if err := ss.Close(); err != nil {
		t.Fatalf("the streams' Close after the run: %v", err)
	}

	// The fault must have fired for the run to say anything: the count the
	// appenders kept bounds the check below, and a schedule that failed on
	// nothing is the run's own bug, not a pass.
	if failures := failed.Load(); failures == 0 {
		t.Fatal("the fault failed no write: the run proved nothing about the ends")
	}
	// Every event names a segment that is Ended in the ledger, and no
	// segment is named twice: the one end a stream owns over it.
	evs := rec.events()
	if len(evs) == 0 {
		t.Fatal("no end event arrived over the failures the appenders saw: the hand-over dropped its events")
	}
	seen := map[core.SegmentID]int{}
	for _, ev := range evs {
		seen[ev.Segment]++
		state, err := set.State(ev.Segment)
		if err != nil {
			t.Fatalf("the state of the event's segment %d: %v", ev.Segment, err)
		}
		if state != segment.Ended {
			t.Fatalf("segment %d, named by an event, stands %v, want Ended", ev.Segment, state)
		}
		if int(ev.Stream) >= ss.Len() {
			t.Fatalf("an event names stream %d, above the streams the run built", ev.Stream)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("segment %d handed %d events, want the one end a stream owns over each ended segment", id, n)
		}
	}

	// Every EIO the appenders saw ended a segment, and each ended segment
	// hands exactly one event, so the events never outnumber the failures
	// the run itself counted.
	if failures := failed.Load(); int64(len(evs)) > failures {
		t.Fatalf("%d events arrived over %d failures, want the events to name only ends the appends made", len(evs), failures)
	}

	// The other direction holds too: every Ended segment the run left behind
	// is named by an event, so a notifier that dropped some ends cannot pass
	// as one that handed them over. The take-unwind segments — a spare the
	// roll's successor-header write failed on, ended with no event by its own
	// exemption — are excluded the one way the disk reads them back: their
	// segment header was never written, so the whole file below the cap's
	// trailer reads as zeros, while every segment an append landed in starts
	// with its 48-byte header written.
	//
	// The maker's spare creation is the same zero-filled file, but the
	// run's spares never left the pool's ready set — the takes drew them
	// into the ledger — and the ledger's Ended memberships alone are the
	// candidates below.
	for id := range endStressEndedSegments(t, set, seam, raceCap) {
		if seen[id] != 1 {
			t.Fatalf("Ended segment %d handed %d events, want the one end it owes: a notifier dropped it", id, seen[id])
		}
	}
}

// endStressEndedSegments lists every Ended segment of the set that an append
// reached — a written header at offset 0 tells it from a spare the take's
// unwind ended before any record was reserved there.
func endStressEndedSegments(t *testing.T, set *segment.Set, seam *journaltest.Seam, cap int64) map[core.SegmentID]struct{} {
	t.Helper()

	// The set names no enumerator of its own, so the ids walked here are
	// bounded by the run's own footprint: a make of the open maximum drew
	// ids up from 1, and every segment the run ended sits below the takes
	// the run made. Walk ids until the ledger answers ErrNotInSet for a
	// run of spans wide enough to have passed the last one.
	ended := map[core.SegmentID]struct{}{}
	const span = 32
	for id := core.SegmentID(1); ; id++ {
		if _, err := set.State(id); err != nil {
			if int64(id) > span {
				break
			}
			continue
		}
		st, err := set.State(id)
		if err != nil || st != segment.Ended {
			continue
		}
		// A header write reaching the file tells an append's segment from a
		// take's unwind: the maker creates a spare zeroed whole, and the
		// roll writes the header before anything else, so a non-zero byte
		// in the first 48 names a segment the stream reached.
		raw := make([]byte, 4)
		f, ferr := seam.FS().Open(streamSegName(id))
		if ferr != nil {
			t.Fatalf("opening Ended segment %d's file: %v", id, ferr)
		}
		_, _ = f.ReadAt(raw, 0)
		if err := f.Close(); err != nil {
			t.Fatalf("closing Ended segment %d's file: %v", id, err)
		}
		if endCountNonZero(raw) > 0 {
			ended[id] = struct{}{}
		}
	}
	return ended
}

// endStopBuilder builds the environment the stop checks run over: an open
// maker maximum (a banned stream may still owe its windows to other checks'
// streams sharing the pool), four content spares the takes below draw in id
// order, and a recording consumer registered before anything appends.
func endStopBuilder(t *testing.T) (e rollEnv, rec *endRecorder) {
	t.Helper()

	e = rollBuild(t, streamCap, segment.Targets{Content: 4}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 4 })
	e = e.withStream(t, 1)
	rec = &endRecorder{}
	e.s.OnEnd(rec.OnEnd)
	return e, rec
}

// endStopStreamEnv adapts the roll env to the stream checks' helpers.
func endStopStreamEnv(e rollEnv) streamEnv {
	return streamEnv{seam: e.seam, set: e.set, maker: e.maker, cache: e.cache, budget: e.budget, s: e.s, cap: streamCap}
}

// endStopEndsSegment sets up a single segment with one failed append, waiting
// for its event, and returns its segment id. The caller holds pool counters
// and seam writes to confirm the later stop consumed nothing.
func endStopEndsSegment(t *testing.T, e rollEnv, rec *endRecorder, fail error) core.SegmentID {
	t.Helper()

	se := endStopStreamEnv(e)
	seg := streamSegName(1)
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, se, ah, abody)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, fail)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, fail) {
		t.Fatalf("the failed Append = %v, want the fault's %v", err, fail)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	evs := rec.awaitCount(t, 1, "the end of the failed append")
	if ev := evs[len(evs)-1]; ev.Segment != placed.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, placed.Loc.Segment)
	}
	return placed.Loc.Segment
}

// TestEndASecondFailureInARowStopsTheStream checks RFC 1 §5.4's stop: a
// stream that took its spare to leave a failed segment meets a failure in
// it before any sync there has succeeded, and the stream stops — the
// second segment's event names the stop, every later Append answers
// ErrStreamStopped wrapping the second failure, and no third spare is
// taken: the pool's ready count and the seam's create and write counts are
// exactly what the two takes left them.
func TestEndASecondFailureInARowStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The second failure. The roll onto segment 2 happens inside the append
	// below: its header write lands and the record after it fails. The rule
	// is armed while the header write is parked at the hold, from the
	// second matching call on — From 2 — so the parked header, the first,
	// passes and the record write, the second, takes the fault. No sync of
	// the segment has succeeded, the fault landing before any, so a failure
	// there is the second segment in a row (RFC 1 §5.4).
	seg2 := streamSegName(2)
	holdH := e.seam.HoldFirst(t, journaltest.OpWrite, seg2)
	appendDone := make(chan error, 1)
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	go func() {
		_, err := e.s.Append(ch, cbody)
		appendDone <- err
	}()
	<-holdH.Reached()
	if err := e.seam.Schedule(journaltest.FailingFrom(journaltest.OpWrite, seg2, 2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdH.Release()
	if err := <-appendDone; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the second failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, seg2); got != 2 {
		t.Fatalf("segment 2 saw %d writes in all, want the header's and the failed record's one alone", got)
	}
	// The counts the stop's appends must leave untouched, pinned once the
	// failure has settled: a pin taken before would race the worker's
	// refill, for this env's maker maximum is open and the pool keeps
	// refilling behind its takes.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	writes := 0
	for _, op := range []string{journaltest.OpCreate, journaltest.OpWrite, journaltest.OpOpen, journaltest.OpAllocate} {
		writes += e.seam.Calls(op)
	}

	if state, err := e.set.State(2); err != nil || state != segment.Ended {
		t.Fatalf("the second segment's state is %v, %v, want Ended", state, err)
	}
	// The second segment's event names the stop; the first segment's own
	// stands in the recorder ahead of it.
	evs := rec.awaitCount(t, 2, "the end of the second failed append")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the second segment 2", ev.Segment)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the fault's EIO", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stream stopped, want Stopped: a second segment in a row failed before any sync in it succeeded")
	}

	// Every later Append answers ErrStreamStopped, wrapping the second
	// failure itself, and takes no spare: the pool's ready count and the
	// seam's create and write counts are unchanged by them.
	for i := range 4 {
		payload := streamTestBody(64, byte(0x20+i))
		h, body := streamWriteHeader(streamFileID(4+i), int64(i)*64, payload, 7, core.Version{Counter: uint64(4 + i)})
		_, err := e.s.Append(h, body)
		if !errors.Is(err, segment.ErrStreamStopped) {
			t.Fatalf("Append %d after the stop = %v, want ErrStreamStopped", i, err)
		}
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("Append %d after the stop = %v, want the second failure's EIO wrapped in it", i, err)
		}
	}
	// The pool's ready count and the seam's create count may rise behind
	// the stop — the worker refills on its own over the open maker
	// maximum — but a third take would spend one of the ready spares, so
	// the count must not have fallen.
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop's appends, want at least the %d the takes left: no third spare is taken", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop's appends, want at least the %d the two takes made", got, createsBefore)
	}
	// The four ops' totals may rise behind the stop the same way — a
	// worker refill makes, writes and opens its own — but the stop's own
	// appends made none, which the two counts above already witness.
	after := 0
	for _, op := range []string{journaltest.OpCreate, journaltest.OpWrite, journaltest.OpOpen, journaltest.OpAllocate} {
		after += e.seam.Calls(op)
	}
	if after < writes {
		t.Fatalf("the seam saw %d of the four I/O ops after the stop's appends, want at least the %d before them", after, writes)
	}

	// The ledger holds the three takes' segments — two ended, one spared
	// aside — and the two events are all the consumer ever sees.
	c := e.set.Counters()
	if c.Segments != 2 {
		t.Fatalf("the ledger holds %d segments after the stop, want the two the takes drew %d", c.Segments, len(evs))
	}
	endEventsStable(t, rec, "the appends after the stop")
}

// TestEndAFailureAfterTheSparesFirstSyncRolls checks the other side of RFC 1
// §5.4's second-refusal rule: a sync of the spare taken to leave a failed
// segment has succeeded, so a failure in it is not the second in a row and
// the stream keeps rolling — its event does not name a stop, and the next
// append lands in a third segment.
func TestEndAFailureAfterTheSparesFirstSyncRolls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The spare's own Sync succeeds, so the failure below meets a segment
	// whose first sync did. A first landed record is what the Sync covers.
	lPayload := streamTestBody(64, 0x3c)
	lh, lbody := streamWriteHeader(streamFileID(3), 0, lPayload, 7, core.Version{Counter: 3})
	if p := streamAppendWrite(t, endStopStreamEnv(e), lh, lbody); p.Loc.Segment != 2 {
		t.Fatalf("the spare's first append landed in segment %d, want the taken spare 2", p.Loc.Segment)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the spare's first Sync: %v", err)
	}

	// The failure there: not the second in a row, for the spare's first
	// sync succeeded.
	seg2 := streamSegName(2)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	fPayload := streamTestBody(64, 0x7d)
	fh, fbody := streamWriteHeader(streamFileID(4), 64, fPayload, 7, core.Version{Counter: 4})
	if _, err := e.s.Append(fh, fbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append in the synced spare = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}

	// The event is handed over, and it names no stop.
	evs := rec.awaitCount(t, 2, "the end of the synced spare's failed append")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the second segment 2", ev.Segment)
	}
	if ev.Stopped {
		t.Fatal("the event names the stream stopped, want it rolling: the spare's first sync succeeded before the failure")
	}

	// The next append rolls to a third segment, and is served.
	nPayload := streamTestBody(64, 0x13)
	nh, nbody := streamWriteHeader(streamFileID(5), 0, nPayload, 7, core.Version{Counter: 5})
	next := streamAppendWrite(t, endStopStreamEnv(e), nh, nbody)
	if next.Loc.Segment != 3 {
		t.Fatalf("the append after the synced spare's failure landed in segment %d, want the next one 3", next.Loc.Segment)
	}
	if hdr := rollHeader(t, e, next.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a failed append ends its segment whatever syncs it saw", hdr.Predecessor)
	}
}

// TestEndEROFSStopsTheStream checks the read-only half of RFC 1 §5.4: a
// failure an EROFS carries, wherever it lands, stops the stream with the
// end — the event says so — and the next append takes no spare at all.
func TestEndEROFSStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)

	// The first landed record gives the segment a sync and the count a
	// value, so the fault below takes the second write alone.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	landed := streamAppendWrite(t, endStopStreamEnv(e), ah, abody)
	seg1 := streamSegName(landed.Loc.Segment)

	// The second write carries EROFS: the device is read-only, so the
	// stream stops with this very end.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EROFS)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the EROFS Append = %v, want the fault's EROFS", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}

	// The event names the stop: an EROFS failure ends the stream's rolling
	// wherever it met it.
	ev := rec.awaitOne(t, "the end of the EROFS append")
	if ev.Segment != landed.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one 1", ev.Segment)
	}
	if !errors.Is(ev.Err, syscall.EROFS) {
		t.Fatalf("the event's error is %v, want the fault's EROFS", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stream stopped, want Stopped: an EROFS says the journal calls would keep failing")
	}

	// The next Append answers ErrStreamStopped with the EROFS wrapped in it,
	// and takes no spare: the counts are pinned after the failure has
	// settled, so the worker's own refill is not read as the stream's.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	cPayload := streamTestBody(64, 0x66)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	_, err := e.s.Append(ch, cbody)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the EROFS = %v, want ErrStreamStopped", err)
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Append after the EROFS = %v, want the EROFS wrapped in the stop", err)
	}
	// The pool's ready count may have risen behind the stop — this env's
	// maker maximum is open, so the worker refills on its own — but it must
	// not have fallen: a stopped stream takes nothing.
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d it held: a stopped stream takes none", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop, want at least the %d before it: a stopped stream makes no spare", got, createsBefore)
	}
	if state, serr := e.set.State(landed.Loc.Segment); serr != nil || state != segment.Ended {
		t.Fatalf("the EROFS segment's state is %v, %v, want Ended", state, serr)
	}
	endEventsStable(t, rec, "the Appends after the EROFS stop")
}

// TestEndAFailedSuccessorHeaderStopsTheStream checks the stop's third path:
// after a failed append, the roll onto the successor fails at the
// successor's own header write — the write the spare's name first sees.
// The append carries the header's refusal, the taken spare ends with no
// event, the stream stays on its ended segment, and every later Append
// answers ErrStreamStopped and takes no further spare.
func TestEndAFailedSuccessorHeaderStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The successor's header write: the next write the seam sees after the
	// first segment's hurts, for the stream sits on an ended segment and
	// the next append rolls. Its refusal is the second the stream meets
	// leaving the ended segment, so the stream stops.
	seg2 := streamSegName(2)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	if _, err := e.s.Append(ch, cbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the Append whose successor header write failed = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	if state, err := e.set.State(2); err != nil || state != segment.Ended {
		t.Fatalf("the failed successor's state is %v, %v, want Ended: the spare the failed header write left is held at its cap", state, err)
	}

	// No second event for the spare — nothing was ever reserved in it — and
	// the one event the recorder holds is the first segment's own.
	rec.awaitCount(t, 1, "the first segment's end")
	endEventsStable(t, rec, "the spare the failed header write ended")

	// Every later Append answers ErrStreamStopped, wrapping the header
	// write's own refusal, and no further spare is taken: the two takes
	// spent two of the maker's spares and the count says so.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	dPayload := streamTestBody(64, 0x13)
	dh, dbody := streamWriteHeader(streamFileID(4), 0, dPayload, 7, core.Version{Counter: 4})
	_, err := e.s.Append(dh, dbody)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the stopped roll = %v, want ErrStreamStopped", err)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("the Append after the stopped roll = %v, want the header write's EIO wrapped in the stop", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d before it: no further spare is taken", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop, want at least the %d before it", got, createsBefore)
	}
	// The next spare never entered the ledger as a segment: State answers
	// ErrNotInSet, for spares and segments sit in different tables.
	if _, err := e.set.State(3); !errors.Is(err, segment.ErrNotInSet) {
		t.Fatalf("the third spare's state answered %v, want ErrNotInSet: no further take ever drew it", err)
	}
}

// TestEndRequestedSecondFailureStops checks the requested end's half of the
// stop: the durability code ends a segment whose Sync failed, in the spare
// the stream took to leave a failed segment, before any sync of that spare
// has succeeded, and the requested end's event says the stream stopped.
func TestEndRequestedSecondFailureStops(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The spare's first Sync fails before anything of its own has
	// succeeded, and the durability code asks for its end.
	seg2 := streamSegName(2)
	aPayload := streamTestBody(64, 0x3c)
	ah, abody := streamWriteHeader(streamFileID(3), 0, aPayload, 7, core.Version{Counter: 3})
	if p := streamAppendWrite(t, endStopStreamEnv(e), ah, abody); p.Loc.Segment != 2 {
		t.Fatalf("the spare's first append landed in segment %d, want the taken spare 2", p.Loc.Segment)
	}
	syncFault := fmt.Errorf("the failed sync: %w", syscall.EIO)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the spare's faulted Sync = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the sync fault: %v", err)
	}
	if err := e.s.End(core.SegmentID(2), syncFault); err != nil {
		t.Fatalf("the requested End of the sync-failed spare = %v, want nil", err)
	}

	// The requested end's event names the stop: no sync of the spare
	// succeeded before the failure, so the end is the second refusal the
	// stream met in a row.
	evs := rec.awaitCount(t, 2, "the requested end of the unsynced spare")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the spare 2", ev.Segment)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the caller's EIO", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stream stopped, want Stopped: the spare's first sync failed before any succeeded")
	}

	// The later appends answer the stop, and take nothing.
	sparesBefore := e.pool.Counters().Spares
	nPayload := streamTestBody(64, 0x13)
	nh, nbody := streamWriteHeader(streamFileID(4), 0, nPayload, 7, core.Version{Counter: 4})
	_, err := e.s.Append(nh, nbody)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the requested stop = %v, want ErrStreamStopped", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d before it", got, sparesBefore)
	}
	endEventsStable(t, rec, "the Append after the requested stop")
}

// TestEndAppenderAlreadyInsideTheRollIsStoppedAtTheLoopTop checks the stop's
// inner check: an append that passed the head check before the failure that
// stopped the stream — one already running AppendRoll's loop when the second
// refusal lands — wakes at the loop top, re-reads the stop there and takes no
// third spare. The park hook holds the second append at the loop top past the
// head check, and the third's failure stops the stream while it is held.
func TestEndAppenderAlreadyInsideTheRollIsStoppedAtTheLoopTop(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}
	se := endStopStreamEnv(e)

	// X lands in segment 2, so the stream's spare is active there.
	xh, xb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 1), 7, core.Version{Counter: 3})
	if p := streamAppendWrite(t, se, xh, xb); p.Loc.Segment != 2 {
		t.Fatalf("X landed in segment %d, want the taken spare 2", p.Loc.Segment)
	}

	// Y parks at the loop top past the head check, holding no lock.
	parked := make(chan struct{})
	release := make(chan struct{})
	segment.StreamSetAppendParkHook(func() {
		segment.StreamSetAppendParkHook(nil)
		close(parked)
		<-release
	})
	t.Cleanup(func() { segment.StreamSetAppendParkHook(nil) })
	yDone := endAppend(se, 4, 64, 2)
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("Y never entered the loop")
	}

	// Z's write in segment 2 fails: an unsynced spare taken to leave an ended
	// segment, the second refusal in a row, and the stream stops while Y
	// sits parked at the loop top.
	seg2 := streamSegName(2)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg2, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	zDone := endAppend(se, 5, 128, 3)
	z := <-zDone
	if !errors.Is(z.err, syscall.EIO) {
		t.Fatalf("Z's append = %v, want the fault's EIO", z.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	evs := rec.awaitCount(t, 2, "the second refusal's end")
	if !evs[len(evs)-1].Stopped {
		t.Fatal("the second refusal's event does not name the stop")
	}

	// Y wakes at the loop top and must answer the stop, not take a third
	// spare: no further take happens, and segment 3 stays a spare.
	close(release)
	y := <-yDone
	if !errors.Is(y.err, segment.ErrStreamStopped) {
		t.Fatalf("the append parked across the stop = %v, want ErrStreamStopped", y.err)
	}
	if _, serr := e.set.State(3); !errors.Is(serr, segment.ErrNotInSet) {
		t.Fatalf("a third spare was taken after the stop: Y landed in %d, State(3) err = %v", y.p.Loc.Segment, serr)
	}
	endEventsStable(t, rec, "the parked append past the stop")
}

// TestEndCloseDuringTheRollsInstallWait checks the close race at the roll's
// install: a Close that lands while the roll waits out the ended segment's
// in-flight appends hand-over unwinds the taken spare itself, so a successor
// is never installed over a closed stream — neither its segment nor its ref.
func TestEndCloseDuringTheRollsInstallWait(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollBuild(t, streamCap, segment.Targets{Content: 2}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	e = e.withStream(t, 1)
	rec := &endRecorder{}
	e.s.OnEnd(rec.OnEnd)
	se := endStopStreamEnv(e)
	seg := streamSegName(1)

	// A lands, B's write parks at the seam, and C's fails past it, so the
	// segment ends while B is still in flight.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(se, 2, 64, 0x55)
	<-holdB.Reached()
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 128, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// D rolls past the ended segment; the roll writes segment 2's header and
	// parks at the install wait, held there by B's still-parked write.
	dDone := endAppend(se, 4, 192, 0x11)
	deadline := time.Now().Add(10 * time.Second)
	for e.seam.CallsFor(journaltest.OpWrite, streamSegName(2)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the roll never wrote the successor's header")
		}
		time.Sleep(time.Millisecond)
	}

	// The Close lands while the roll is parked at install: the taken spare is
	// the close's to unwind once the roll releases the wait, for the stream
	// publishes no location past a close. Close parks in its in-flight wait —
	// D's append is counted — so the check holds B's write back and asserts
	// the close has not returned over a bounded wait.
	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while the roll was still parked at install: it must wait the append out", err)
	case <-time.After(200 * time.Millisecond):
	}

	// B's write lands — the wait the roll parks on goes false for it, the
	// close's flag carries, and the roll answers the close.
	holdB.Release()
	if b := <-bDone; b.err != nil {
		t.Fatalf("B's held append = %v", b.err)
	}
	d := <-dDone
	if !errors.Is(d.err, segment.ErrStreamClosed) {
		t.Fatalf("D's append = %v, want ErrStreamClosed: the roll found the close at the install wait", d.err)
	}

	// The taken spare is ended in the ledger as a closed take's unwind leaves
	// it, never Active: no successor was installed over the closed stream.
	if state, serr := e.set.State(2); serr != nil || state != segment.Ended {
		t.Fatalf("the closed take's spare stands %v, %v, want Ended by the unwind the close owned", state, serr)
	}
	// The event of the ended segment 1 still hands over: the close's
	// exemption covers the take's spare, not the segment B and C's own.
	rec.awaitOne(t, "the ended segment's event past the close")
	endEventsStable(t, rec, "the close's take of segment 2")
}

// TestEndSyncInFlightAcrossAFailureRollOwnsItsOwnSegment checks the stream's
// own Sync against the failure path's roll: a Sync of the ended segment that
// is in flight when the failure lands holds the roll back — the event's
// hand-over waits for it, and the roll waits for the event — so the
// successor starts blank: its tracker takes no window the old segment's fsync
// made durable (R10) and its second-refusal word stays blank until a sync of
// the successor itself succeeds.
func TestEndSyncInFlightAcrossAFailureRollOwnsItsOwnSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	// A and a second record land, so segment 1's completed prefix is well
	// past segment 2's own record slots.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	a2h, a2b := streamWriteHeader(streamFileID(1), 64, streamTestBody(512, 0x43), 7, core.Version{Counter: 2})
	streamAppendWrite(t, se, a2h, a2b)

	// A Sync of segment 1 parks at the seam.
	holdS := e.seam.HoldFirst(t, journaltest.OpSync, seg1)
	syncDone := make(chan error, 1)
	go func() { syncDone <- e.s.Sync() }()
	<-holdS.Reached()

	// C's write fails, ending segment 1 while the sync is in flight. The
	// event and the roll wait for the sync, so it returns first; then the
	// event hands over, and D rolls onto segment 2 and lands there.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 1024, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	holdS.Release()
	if err := <-syncDone; err != nil {
		t.Fatalf("the straddling sync = %v, want nil", err)
	}
	rec.awaitCount(t, 1, "the ended segment's event")
	d := <-endAppend(se, 4, 0, 0x11)
	if d.err != nil || d.p.Loc.Segment != 2 {
		t.Fatalf("D's append = %v in segment %d, want a landing in 2", d.err, d.p.Loc.Segment)
	}

	// The sync's success belongs to segment 1 alone: segment 2's tracker
	// claims no window bytes as synced-through, for no fsync of segment 2 ran.
	ePl := <-endAppend(se, 5, 0, 0x22)
	if ePl.err != nil {
		t.Fatal(ePl.err)
	}
	if got, _ := streamReadRecord(t, se, ePl.p.Loc); got.SyncedThrough > d.p.Loc.Off {
		t.Fatalf("the record in segment 2 claims SyncedThrough %d over bytes its own segment never fsynced, want at most D's offset %d", got.SyncedThrough, d.p.Loc.Off)
	}

	// The straddling sync's success does not reach the second-refusal word
	// either: a failure in segment 2 before any sync of it has succeeded is
	// the second refusal in a row, and the event must say the stream stopped.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, streamSegName(2), 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if f := <-endAppend(se, 6, 0, 0x33); !errors.Is(f.err, syscall.EIO) {
		t.Fatalf("F's append = %v, want the fault's EIO", f.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	evs := rec.awaitCount(t, 2, "the second failure's end")
	if ev := evs[len(evs)-1]; !ev.Stopped {
		t.Fatalf("the event after a failure in the unsynced spare says Stopped=false, want true: the straddling sync proved nothing of segment 2 durable")
	}
}

// TestEndTheStraddlingSyncsFailureMarksItsOwnSegmentAlone checks the other
// half of the straddling sync: a failure of the sync in flight when the
// segment ended neither marks the successor sync-failed nor ends it for a
// failure it never had. It answers its error to its caller, which owns its
// window through End.
func TestEndTheStraddlingSyncsFailureMarksItsOwnSegmentAlone(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	holdS := e.seam.HoldFirst(t, journaltest.OpSync, seg1)
	syncDone := make(chan error, 1)
	go func() { syncDone <- e.s.Sync() }()
	<-holdS.Reached()

	// Two failures queued: C's write fails and ends segment 1, and the held
	// sync itself fails with EIO once D's roll has installed segment 2. Both
	// rules are armed while the sync is parked at its hold — the first rule
	// spends its round on C's write, the next matching write, and the sync
	// rule's round is spent on the fsync the release lets run.
	if err := e.seam.Schedule(
		journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO),
		journaltest.Failing(journaltest.OpSync, seg1, 1, syscall.EIO),
	); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 64, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	// The sync rule stays armed: its one round is what the held fsync
	// consults at its release, and nothing may disarm it before that. The
	// event and the roll wait for the sync, so it returns first.
	holdS.Release()
	if err := <-syncDone; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the straddling sync = %v, want the fault's EIO", err)
	}
	rec.awaitCount(t, 1, "the ended segment's event")
	d := <-endAppend(se, 4, 0, 0x11)
	if d.err != nil || d.p.Loc.Segment != 2 {
		t.Fatalf("D's append = %v in segment %d, want a landing in 2", d.err, d.p.Loc.Segment)
	}

	// The successor is untouched: it keeps its roll's state, takes further
	// appends —no marker, no freeze, no sync-failed state the straddling
	// failure would have leaked into it.
	if state, serr := e.set.State(2); serr != nil || state != segment.Active {
		t.Fatalf("segment 2 stands %v, %v after the straddling sync's failure, want Active: the failure of a sync past its segment owns nothing of the successor", state, serr)
	}
	ePl := <-endAppend(se, 5, 0, 0x22)
	if ePl.err != nil || ePl.p.Loc.Segment != 2 {
		t.Fatalf("the append past the failed straddling sync = %v in segment %d, want a landing in 2", ePl.err, ePl.p.Loc.Segment)
	}
	// A sync of the successor still succeeds: the failure landed on the old
	// segment's own fsync, not on the stream's word for the new one.
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the successor's own sync = %v, want nil", err)
	}
	endEventsStable(t, rec, "the append past the failed straddling sync")
}

// TestEndTheEventWaitsForASyncInFlightOnTheEndedSegment checks the event's
// window against a Sync that straddles the failure: the sync took its prefix
// before the append failed and its fsync is still running, so the window the
// event names is read only after the sync returns. An event handed over at
// the failure would name a SyncedThrough below what the returned Sync
// covered, and a consumer failing "the window's records" would drop records
// a nil-returning Sync already made durable (RFC 1 §5.3).
func TestEndTheEventWaitsForASyncInFlightOnTheEndedSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	a2h, a2b := streamWriteHeader(streamFileID(1), 64, streamTestBody(512, 0x43), 7, core.Version{Counter: 2})
	a2 := streamAppendWrite(t, se, a2h, a2b)
	covered := a2.Loc.Off + a2.Loc.Len

	holdS := e.seam.HoldFirst(t, journaltest.OpSync, seg1)
	syncDone := make(chan error, 1)
	go func() { syncDone <- e.s.Sync() }()
	<-holdS.Reached()

	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 1024, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// The sync is still in flight: its window is not final, so no event.
	rec.awaitNone(t, "the event while a sync of the ended segment is in flight")

	holdS.Release()
	if err := <-syncDone; err != nil {
		t.Fatalf("the straddling sync = %v, want nil", err)
	}
	ev := rec.awaitOne(t, "the event after the straddling sync returned")
	if ev.Segment != 1 {
		t.Fatalf("the event names segment %d, want 1", ev.Segment)
	}
	if ev.SyncedThrough < covered {
		t.Fatalf("the event's SyncedThrough = %d, below %d the returned Sync covered: the window would include records a successful Sync made durable", ev.SyncedThrough, covered)
	}
}

// TestEndALedgerRefusalToAnEndStillStartsTheNotifier checks the end's
// notifier against a ledger that refuses the end itself: the refusal returns
// to its caller, but the end's in-flight count is spent by a notifier of its
// own, so neither a Close nor a later roll hangs on it.
func TestEndALedgerRefusalToAnEndStillStartsTheNotifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollBuild(t, streamCap, segment.Targets{Content: 2}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	e = e.withStream(t, 1)
	se := endStopStreamEnv(e)
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, se, ah, ab)

	// The ledger ends the stream's active segment first, so the end below —
	// the durability code's own request — meets a refusal on its set call
	// while the stream's state has taken the end.
	if err := e.set.End(a.Loc.Segment); err != nil {
		t.Fatalf("ending the segment in the ledger by hand: %v", err)
	}
	if err := e.s.End(a.Loc.Segment, syscall.EIO); err == nil {
		t.Fatal("End of the ledger-ended segment answered nil, want the ledger's refusal")
	}

	// The refusal was answered, but the end counted a notifier the stream owes
	// Close: a close that follows must return, not wait forever on the
	// in-flight count the refused path would have leaked.
	done := make(chan error, 1)
	go func() { done <- e.s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close past the refused end = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned past the refused end: the refusal leaked the end's in-flight count")
	}
}

// TestEndALedgerRefusalToASealsEndStillStartsTheNotifier checks the same leak
// through the seal's freeze branch: a seal that finds a failed write ends its
// segment, and a ledger that refuses the end returns its refusal with the
// notifier started anyway, so a Close is not left hanging on it.
func TestEndALedgerRefusalToASealsEndStillStartsTheNotifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A set that refuses set.End: a segment of the journal never added, so
	// the seal's freeze branch ends a segment the ledger does not hold.
	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// One record lands; the segment's Sync fails, so the stream holds a
	// syncFailed word while the segment stands open — the state the seal's
	// freeze branch ends it from. The ledger is ended by hand first, so the
	// seal's own set.End meets a refusal: the freeze branch's caller.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, ab)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Sync = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	// The ledger ends the segment by hand, so the seal's end below is the
	// one the ledger refuses.
	if err := e.set.End(a.Loc.Segment); err != nil {
		t.Fatalf("ending the segment in the ledger by hand: %v", err)
	}
	if res, err := e.s.Seal(false); err == nil {
		t.Fatalf("the Seal of the ledger-ended segment answered nil, want a refusal (res %+v)", res)
	} else if !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the Seal of the ledger-ended segment = %v, want the ended sentinel joined in refusal", err)
	}

	done := make(chan error, 1)
	go func() { done <- e.s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close past the refused seal = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned past the refused seal: the refusal leaked the end's in-flight count")
	}
}

// TestEndTheRollDoesNotInstallBeforeTheEventsSnapshot checks the roll's
// install wait against the notifier itself: an end's event must be handed
// over — its snapshot taken — before the roll installs the successor, for a
// roll that woke past the notifier's last-completion broadcast would have
// the snapshot read the successor's own reset failedAt and blank tracker,
// and the event would name neither the failed slot nor the window it owes
// the durability code. The notify park holds the notifier at its top while
// the completion that would wake it has already landed.
func TestEndTheRollDoesNotInstallBeforeTheEventsSnapshot(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// The consumer parks the event itself, so the check sees the snapshot's
	// word alone: the event keeps its hands off the stream's state, and the
	// park below needs no held lock to hold the notifier back.
	eventIn := make(chan segment.EndEvent, 1)
	e.s.OnEnd(func(ev segment.EndEvent) { eventIn <- ev })

	// A lands and is synced; B's write parks at the seam; the notifier is
	// parked at its own top BEFORE anything can end the segment, so the end
	// C's failure makes is handed over by a notifier the check holds.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, ab)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}
	release := make(chan struct{})
	segment.StreamSetNotifyParkHook(func() { <-release })
	t.Cleanup(func() { segment.StreamSetNotifyParkHook(nil) })
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()

	// C's write fails, ending the segment; the notifier the end starts is
	// parked at its top by the hook, for it was armed before the end.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	cDone := endAppend(e, 3, 128, 0x7d)
	if c := <-cDone; !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// B's write lands — the last reservation the segment held — and D rolls.
	// The roll reaches the install wait and parks there, for the notifier
	// whose hand-over it waits on is itself parked at its own top: neither
	// the event nor the successor may move while the park holds.
	holdB.Release()
	if b := <-bDone; b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	dDone := endAppend(e, 4, 0, 0x11)
	deadline := time.Now().Add(10 * time.Second)
	for e.seam.CallsFor(journaltest.OpWrite, streamSegName(2)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the roll never wrote the successor's header")
		}
		time.Sleep(time.Millisecond)
	}

	// Over a bounded wait, neither the event nor D's append returns: the
	// roll's wait holds the successor off the notifier's hand-over, and the
	// parked notifier holds the event back. The take itself has already
	// drawn segment 2 into the ledger — the roll's own hold-off is proven
	// by D, whose append can only land through an install the park keeps
	// from happening.
	select {
	case ev := <-eventIn:
		t.Fatalf("the event arrived while the notifier was parked: %v", ev)
	case d := <-dDone:
		t.Fatalf("the append installed its successor before the end's record cleared: %v in segment %d", d.err, d.p.Loc.Segment)
	case <-time.After(200 * time.Millisecond):
	}

	// The notifier is let pass: the snapshot reads the ended segment's own
	// failed slot — C's, above A's synced window, below the segment's
	// reservations' end — and only then does the roll install and D's append
	// land. A snapshot after the install would name the successor's reset
	// failedAt (next, the successor's header end 48) and its blank tracker.
	close(release)
	ev := <-eventIn
	if ev.Segment != a.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, a.Loc.Segment)
	}
	if ev.Failed <= a.Loc.Off+a.Loc.Len {
		t.Fatalf("the event's Failed %d is at or below A's end %d, want C's failed slot above it: the snapshot found the successor's reset failedAt instead", ev.Failed, a.Loc.Off+a.Loc.Len)
	}
	if ev.SyncedThrough == 0 {
		t.Fatalf("the event's SyncedThrough is 0, want A's synced window the segment held: the snapshot found the successor's blank tracker instead")
	}
	d := <-dDone
	if d.err != nil || d.p.Loc.Segment != 2 {
		t.Fatalf("the append past the installed successor = %v in segment %d, want a landing in 2", d.err, d.p.Loc.Segment)
	}
}

// TestEndSyncedSegmentBeforeItsFailureStopsOnTheSecondRefusal checks the
// syncedOnce reset at the roll's install: the first segment synced
// successfully before its write failed, so the stream's word that a spare was
// proved durable belongs to the first segment alone. A failure in the second,
// unsynced spare taken to leave that ended segment stops the stream — a stale
// syncedOnce from the first would let the stream take a third spare.
func TestEndSyncedSegmentBeforeItsFailureStopsOnTheSecondRefusal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)

	// Segment 1 lands a record and syncs it, then a second write fails
	// there: an ended segment whose own sync succeeded, the state every
	// other stop check builds without.
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, se, ah, ab)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first segment's Sync: %v", err)
	}
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	bh, bb := streamWriteHeader(streamFileID(2), 64, streamTestBody(64, 0x55), 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bb); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	if state, serr := e.set.State(a.Loc.Segment); serr != nil || state != segment.Ended {
		t.Fatalf("the synced-and-failed segment stands %v, %v, want Ended", state, serr)
	}
	rec.awaitCount(t, 1, "the first segment's end")

	// The failure in the unsynced spare taken to leave that ended segment is
	// the second refusal in a row, and the stream must stop with it. The
	// rule is armed while segment 2's header write is parked at the hold —
	// From 2 — so the parked header passes and the record write, the second
	// matching call, takes the fault, as the first stop check stages it.
	seg2 := streamSegName(2)
	holdH := e.seam.HoldFirst(t, journaltest.OpWrite, seg2)
	appendDone := make(chan error, 1)
	ch, cb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x7d), 7, core.Version{Counter: 3})
	go func() {
		_, err := e.s.Append(ch, cb)
		appendDone <- err
	}()
	<-holdH.Reached()
	if err := e.seam.Schedule(journaltest.FailingFrom(journaltest.OpWrite, seg2, 2, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	holdH.Release()
	if err := <-appendDone; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the second failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	evs := rec.awaitCount(t, 2, "the second refusal's end")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the second segment 2", ev.Segment)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stop, want Stopped: the first segment's sync proved nothing of the second one durable")
	}

	// The stopped stream answers every later append and takes no spare.
	sparesBefore := e.pool.Counters().Spares
	nh, nb := streamWriteHeader(streamFileID(4), 0, streamTestBody(64, 0x13), 7, core.Version{Counter: 4})
	if _, err := e.s.Append(nh, nb); !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the stop = %v, want ErrStreamStopped", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d before it", got, sparesBefore)
	}
}

// TestEndEROFSOnASuccessorHeaderWriteStopsTheStream checks the read-only half
// of the successor-header gate on an ordinary roll: a spare whose header write
// fails with EROFS — the stream leaving no failed segment behind at all —
// stops the stream, for the read-only device ends its rolling wherever the
// roll meets it. Taken-name assertions aside, the stream holds nothing, so
// the EROFS half of the gate stands alone, not leaning on a failure that
// would stop it anyway.
func TestEndEROFSOnASuccessorHeaderWriteStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)

	// The stream holds nothing yet: its first append rolls from nothing, and
	// the successor header the roll writes is the first write of segment 1.
	// The EROFS rule takes exactly it — a read-only device ends the stream's
	// rolling wherever it meets it, here on a stream that never held a
	// failed segment at all.
	seg1 := streamSegName(1)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EROFS)); err != nil {
		t.Fatal(err)
	}
	h, b := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	_, err := e.s.Append(h, b)
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the append over the EROFS successor header = %v, want the fault's EROFS", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	// The take unwound: the spare holds no record, so its end is the take's
	// alone, and the stream still holds nothing.
	if state, serr := e.set.State(1); serr != nil || state != segment.Ended {
		t.Fatalf("the unwound take stands %v, %v, want Ended", state, serr)
	}

	// The stream stopped with the EROFS: the stopped word wraps the header
	// write's own refusal, no event hands over for the spared take, and the
	// next append answers ErrStreamStopped with no further spare taken.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	nh, nb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x13), 7, core.Version{Counter: 3})
	_, err = e.s.Append(nh, nb)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the EROFS = %v, want ErrStreamStopped", err)
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Append after the EROFS = %v, want the EROFS wrapped in the stop", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d it held", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop, want at least the %d before it", got, createsBefore)
	}
	endEventsStable(t, rec, "the appends past the EROFS stop")

	// The taken spare ended in the ledger with no event: the stream never
	// reserved a record in it, and its end is the take's unwind alone. A
	// second take of the pool's next spare never happened: the stopped
	// stream answers before any roll could draw one.
	if _, serr := e.set.State(2); !errors.Is(serr, segment.ErrNotInSet) {
		t.Fatalf("the pool's next spare stands in the set as %v, want ErrNotInSet: the stopped stream took none of it", serr)
	}
}

// TestEndTheEventsFailedIsTheLowestFailedSlot checks the min the completion
// keeps on failedAt: two failed writes in one segment, the higher completing
// first, and the event names the lower failed slot — the lowest offset whose
// write failed is the window's own lower bound, whichever refusal the
// scheduler answered first.
func TestEndTheEventsFailedIsTheLowestFailedSlot(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	rec := &endRecorder{}
	e.s.OnEnd(rec.OnEnd)

	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, ab)

	// Two one-shot holds park both writes in their reservation order: B
	// first, C above it — its slot is B's end, the failed writes below both
	// assert against the segment's own offsets, read after they landed.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	cDone := endAppend(e, 3, 128, 0x7d)
	<-holdC.Reached()

	// Fail both writes: the count-2 rule spends its rounds on the two
	// released consults. B — the lower slot — is released first, so its
	// failure lands first and the higher one's completion is what the min
	// must not let overwrite it: an unconditional assignment would name C.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 2, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	holdB.Release()
	b := <-bDone
	if !errors.Is(b.err, syscall.EIO) {
		t.Fatalf("the lower failed Append = %v, want the fault's EIO", b.err)
	}
	// While C's slot is still parked, the segment's end has already been
	// made by B's completion — the event only hands over once C lands.
	holdC.Release()
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the higher failed Append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// The failed appends answer with no place, so their slots are read from
	// the reservations the three same-shaped records took in order: A's own
	// end, then one record's width each — B's slot the lower of the two
	// failed ones, C's above it.
	slotLen := a.Loc.Len
	bSlot := a.Loc.Off + slotLen
	cSlot := bSlot + slotLen

	ev := rec.awaitOne(t, "the end of the doubly failed segment")
	if ev.Segment != a.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, a.Loc.Segment)
	}
	if ev.Failed != bSlot {
		t.Fatalf("the event names the failed slot %d, want the lowest failed one %d: the min keeps the lower slot (%d..%d) however the completions ordered themselves", ev.Failed, bSlot, bSlot, cSlot)
	}
}

// TestEndTheEventsFailedCannotNameTheLeftSegment checks the failedAt reset at
// the roll's install: a requested end of the spare taken past an ended
// segment must name the spare's own failure, never the offset a write failed
// at in the segment the stream left behind.
func TestEndTheEventsFailedCannotNameTheLeftSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// A record lands in segment 2, then the durability code's request ends
	// it — a requested end with no failed write of its own.
	se := endStopStreamEnv(e)
	ah, ab := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x3c), 7, core.Version{Counter: 3})
	a := streamAppendWrite(t, se, ah, ab)
	if a.Loc.Segment != 2 {
		t.Fatalf("the record landed in segment %d, want the taken spare 2", a.Loc.Segment)
	}
	syncErr := fmt.Errorf("the failed sync: %w", syscall.EIO)
	if err := e.s.End(core.SegmentID(2), syncErr); err != nil {
		t.Fatalf("the requested End of the spare = %v, want nil", err)
	}
	evs := rec.awaitCount(t, 2, "the requested end of the spare")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the spare 2", ev.Segment)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if ev.Failed < a.Loc.Off || ev.Failed > a.Loc.Off+a.Loc.Len {
		t.Fatalf("the event names the failed slot %d, want the spare's own end %d..%d: a roll must not leave the left segment's failedAt in the successor's event", ev.Failed, a.Loc.Off, a.Loc.Off+a.Loc.Len)
	}
}

// TestEndRegistrationPastARollAndAnEndPanics completes the OnEnd gate's own
// doc: after a reservation — the case its check already names — and after a
// roll and an end too, for each of the three marks the stream started and a
// consumer registered past any of them races an event already in flight.
func TestEndRegistrationPastARollAndAnEndPanics(t *testing.T) {
	prof.Watch(t, prof.Options{})

	callsOnEnd := func(t *testing.T, s *segment.Stream) (panicked bool) {
		t.Helper()
		defer func() { panicked = recover() != nil }()
		s.OnEnd(func(segment.EndEvent) {})
		return
	}

	// A plain fresh stream: registering is free.
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	if panicked := callsOnEnd(t, e.s); panicked {
		t.Fatal("OnEnd before anything panicked, want free registration")
	}

	// Past a roll the gate closes: the first append's roll is a roll the
	// stream made, and the event of the segment the roll's seal may end
	// would be handed to a consumer that saw none of the segment's history.
	er := rollPin(t, streamCap, segment.Targets{Reserve: 2}, nil).withStream(t, 1)
	awaitCounters(t, "the two reserve spares ready", er.pool,
		func(c segment.PoolCounters) bool { return c.RepackReserveBytes == 2*streamCap && c.Spares == 0 })
	payload := streamTestBody(64, 0x3c)
	hh, hb := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	if _, err := er.s.AppendRoll(hh, hb, segment.Roll{Repack: true}); err != nil {
		t.Fatalf("the repack AppendRoll over the roll env: %v", err)
	}
	if panicked := callsOnEnd(t, er.s); !panicked {
		t.Fatal("OnEnd past a roll returned, want a panic")
	}

	// Past a requested end the gate closes the same way, for an end may
	// arrive before any append and still owns its window's event.
	ee := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = ee.s.Close() })
	if err := ee.s.End(0, syscall.EIO); err != nil && !errors.Is(err, segment.ErrNotInSet) {
		t.Fatalf("an End naming a segment out of the set = %v, want the ledger's own refusal", err)
	}
	if panicked := callsOnEnd(t, ee.s); !panicked {
		t.Fatal("OnEnd past an end returned, want a panic")
	}
}

// TestEndACloseDuringTheSealWaitAnswersClosed checks the re-check End makes
// after its wait on a running seal: a Close that lands while the request is
// parked on the drain names the call's whole answer, so End answers
// ErrStreamClosed rather than the nil of a stream that no longer owns the
// segment. A's write is held at the seam, so the seal parks on the drain and
// End parks behind it; Close then runs, and A's release wakes both.
func TestEndACloseDuringTheSealWaitAnswersClosed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// One record lands first, so the stream holds its segment; A is the
	// second append, whose write parks at the seam with its slot reserved.
	h0, b0 := streamWriteHeader(streamFileID(9), 0, streamTestBody(64, 0x40), 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, h0, b0)
	holdA := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	aDone := endAppend(e, 1, 64, 0x41)
	<-holdA.Reached()

	// The seal parks on the drain for A's reservation.
	sealDone := make(chan error, 1)
	go func() {
		_, err := e.s.Seal(false)
		sealDone <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	// parked reports whether a goroutine is blocked in the drain cond's wait
	// inside the named stream method.
	parked := func(method string) bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range bytes.Split(buf, []byte("\n\n")) {
			if bytes.Contains(g, []byte("(*Stream)."+method+"(")) && bytes.Contains(g, []byte("sync.runtime_notifyListWait")) {
				return true
			}
		}
		return false
	}
	for !parked("Seal") {
		if time.Now().After(deadline) {
			t.Fatal("the Seal never parked on the drain while A's write was held")
		}
		runtime.Gosched()
	}

	// End parks on the drain cond behind the seal: seen as a goroutine
	// blocked in the cond's wait inside Stream.End.
	endDone := make(chan error, 1)
	go func() { endDone <- e.s.End(1, fmt.Errorf("the failed sync: %w", syscall.EIO)) }()
	for !parked("End") {
		if time.Now().After(deadline) {
			t.Fatal("End never parked on the drain behind the seal")
		}
		runtime.Gosched()
	}

	// Close runs while both are parked; a Seal answering closed is the
	// observable that it has marked the stream closed.
	closeDone := make(chan error, 1)
	go func() { closeDone <- e.s.Close() }()
	for {
		if _, err := e.s.Seal(false); errors.Is(err, segment.ErrStreamClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never marked the stream closed")
		}
		runtime.Gosched()
	}

	holdA.Release()
	if err := <-endDone; !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("End across a Close that landed during the seal wait = %v, want ErrStreamClosed", err)
	}
	<-sealDone
	<-aDone
	<-closeDone
}
