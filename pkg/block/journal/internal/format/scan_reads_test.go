package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/core"
)

// The tests in this file hold the scanner's read paths to their contracts:
// a read that fails reaches the caller as itself and fails only at bytes
// the scan asked for, the window hands back exactly the bytes a slot asks
// for, and the records around and past the window's whole span attach.

// TestScanReturnsReadErrorsAsThemselves pins that a read that fails is
// returned as itself, never as damage and never as a zero slot — unreadable
// never counts as empty — with the partial Scan it had built: everything
// attached before the failing read, every range closed before it, End at the
// offset the read was made at. The scan reads a record's body by its own
// readFull call, and reads its slots through a window the first offending
// slot refills, so an injected failure lands where the scan asks the reader
// for the bytes: at the body's own read, or at the edge of a window the
// walk has crossed — the header slot a later refill starts at, or the slot
// a forward search steps to — each of which the subtests name. A bodyless
// unmark record, 96 bytes and no more, is the cheapest way to walk a
// segment past one whole window of a megabyte, so that is what the two
// window-crossing subtests build.
func TestScanReturnsReadErrorsAsThemselves(t *testing.T) {
	injected := errors.New("format: an injected read failure")

	t.Run("at a body", func(t *testing.T) {
		// The first record is a write with a 13-byte payload, so its
		// 21-byte body read starts at its header's own end: a reader
		// failing there fails the record under examination, nothing
		// attached yet and no range open, End at the record's slot.
		seg, offs := scanSegment(t, testIdentity, scanTestRecords()...)
		bodyAt := offs[0] + RecordHeaderSize // the write's own body read starts here

		got, err := ScanSegment(readAtFailer{b: seg, off: bodyAt, err: injected}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error", err)
		}
		if errors.Is(err, ErrDamaged) {
			t.Error("the injected error washed into ErrDamaged, want it kept as itself")
		}
		if len(got.Records) != 0 || got.End != offs[0] {
			t.Errorf("records %d end %d, want none attached and end at the record's own slot %d", len(got.Records), got.End, offs[0])
		}
		if len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("ranges %d sealed %v resynced %v, want no range open for a record still under examination", len(got.Ranges), got.Sealed, got.Resynced)
		}
	})

	t.Run("inside the forward search", func(t *testing.T) {
		// The write at the first slot with its length damaged opens a
		// search, and nothing after it verifies — zeros the whole
		// megabyte the search must cross. The first read the search
		// cannot be served from the refill the damaged write was judged
		// under is the slot whose own header crosses that window's edge,
		// so the reader fails exactly there. The search dies mid-way:
		// End at the read's own offset, the bad write's range closed
		// at the failing read and kept, for every byte of it was read
		// before the one that failed.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		h, body := scanKindBody(KindWrite)
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		binary.LittleEndian.PutUint64(seg[48 : 48+96][40:48], 4<<20)
		// the first slot whose header read crosses the window [48, 48+scanWindow)
		failAt := int64(SegmentHeaderSize + scanWindow - RecordHeaderSize + 8)
		seg = append(seg, make([]byte, failAt+96-int64(len(seg)))...)

		got, err := ScanSegment(readAtFailer{b: seg, off: failAt, err: injected}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error", err)
		}
		if got.End != failAt {
			t.Errorf("end %d, want the read's own offset %d", got.End, failAt)
		}
		if len(got.Records) != 0 || got.Resynced || got.Sealed {
			t.Error("records, seal or resync from a search whose read failed, want nothing")
		}
		// The bad write's range closes at the failing read and is kept —
		// every range closed before the failing offset is — as bad, for
		// the damaged record's own bytes are not zeros.
		want := ScanRange{Off: SegmentHeaderSize, End: failAt, Kind: RangeBad}
		if len(got.Ranges) != 1 || got.Ranges[0] != want {
			t.Errorf("ranges = %+v, want the one closed at the failing read, %+v", got.Ranges, want)
		}
	})

	t.Run("at the first slot of a window refill", func(t *testing.T) {
		// The window is refilled at the slot being examined, so a
		// reader failing at the refill's own start fails the walk at
		// that slot with everything attached before it kept. One
		// refill covers a whole megabyte, so the subtest walks unmark
		// records — 96 bytes each and bodyless, the cheapest there
		// are — one window's worth and then one more: every one the
		// first refill holds attaches, and the record past its reach
		// is the one the failing refill is made for.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		const unmarks = scanWindow / 96 // exactly one window of records
		for i := 0; i < unmarks+1; i++ {
			h, body := scanKindBody(KindUnmark)
			seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		}
		refillAt := int64(len(seg)) - 96 // the first record past the first refill's reach

		got, err := ScanSegment(readAtFailer{b: seg, off: refillAt, err: injected}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error", err)
		}
		if got.End != refillAt {
			t.Errorf("end %d, want the refill's own start %d, the offset the read was made at", got.End, refillAt)
		}
		if len(got.Records) != unmarks {
			t.Errorf("records %d, want the %d the first window holds, not the record examined under the failing refill", len(got.Records), unmarks)
		}
		if len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("ranges %d sealed %v resynced %v, want a clean walk before the failure", len(got.Ranges), got.Sealed, got.Resynced)
		}
	})

	t.Run("a reader shorter than size", func(t *testing.T) {
		seg, _ := scanSegment(t, testIdentity, scanTestRecords()...)
		got, err := ScanSegment(readAtShort{b: seg[:len(seg)-1]}, int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ScanSegment = %v, want io.ErrUnexpectedEOF", err)
		}
		if errors.Is(err, ErrDamaged) {
			t.Error("the short read washed into ErrDamaged, want it kept as itself")
		}
		if got.Sealed || got.Resynced {
			t.Errorf("sealed %v resynced %v, want neither from a short read", got.Sealed, got.Resynced)
		}
		if got.End < SegmentHeaderSize || got.End > int64(len(seg)) {
			t.Errorf("end %d, want one inside the segment", got.End)
		}
		for _, r := range got.Records {
			if r.Header.Offset+RecordHeaderSize > got.End {
				t.Errorf("attached a record at %d over the end %d", r.Header.Offset, got.End)
			}
		}
	})
}

// TestScanReadReturnsTheBytesItWasAskedFor: the window read serves
// exactly the slice asked for, on the refill path exactly as on the
// window-hit one — a refill may read a megabyte into the window, but the
// bytes it hands back are the slot's own, so nothing downstream measures
// any byte past the slot that asked.
func TestScanReadReturnsTheBytesItWasAskedFor(t *testing.T) {
	seg, offs := scanSegment(t, testIdentity, scanTestRecords()...)
	sc := scanner{
		r:        bytes.NewReader(seg),
		size:     int64(len(seg)),
		identity: testIdentity,
		segment:  testSegment,
		window:   make([]byte, min(scanWindow, int64(len(seg)))),
	}

	// The first call is a refill: the window holds nothing yet, so the
	// slot's eight bytes are read by the refill itself. The bytes back
	// are eight, not the megabyte the refill fetched.
	narrow := int64(8)
	b, err := sc.read(offs[1], narrow)
	if err != nil {
		t.Fatalf("read = %v, want no error", err)
	}
	if len(b) != int(narrow) {
		t.Fatalf("read handed back %d bytes, want the %d asked for", len(b), narrow)
	}

	// The second call is a window hit — same answer, same length.
	b, err = sc.read(offs[1], narrow)
	if err != nil {
		t.Fatalf("read = %v, want no error", err)
	}
	if len(b) != int(narrow) {
		t.Fatalf("a window hit handed back %d bytes, want the %d asked for", len(b), narrow)
	}

	// And a call past the window's span refills wide again yet hands
	// back only its own bytes.
	sc.cur, sc.have = 0, 0
	b, err = sc.read(offs[2], RecordHeaderSize)
	if err != nil {
		t.Fatalf("read = %v, want no error", err)
	}
	if len(b) != RecordHeaderSize {
		t.Fatalf("a refill handed back %d bytes, want the %d asked for", len(b), RecordHeaderSize)
	}
}

// TestScanAttachesRecordsAroundAndPastTheWindow: the walk attaches the
// records the scan's window alone cannot serve — a body read larger than a
// whole window, and a record whose header sits on one side of a refill and
// whose body stretches past it — because a record's body is read by its own
// readFull into a buffer grown for it, never through the slot window.
func TestScanAttachesRecordsAroundAndPastTheWindow(t *testing.T) {
	// bigWrite builds a write of n payload bytes with a header whose
	// patterns the golden tests carry.
	bigWrite := func(n int64) (RecordHeader, RecordBody) {
		h := RecordHeader{
			Kind:          KindWrite,
			Segment:       testSegment,
			File:          testRecordFile,
			Seq:           testRecordSeq,
			Version:       testRecordVersion,
			Tag:           testRecordTag,
			SyncedThrough: testRecordSyncedThrough,
			Extent:        core.Extent{Off: testRecordFileOffset, Len: n},
		}
		return h, RecordBody{MTime: testWriteMTime, Payload: scanPayload(int(n))}
	}

	t.Run("a body larger than the window", func(t *testing.T) {
		// A write of a window and a half of payload: both the one
		// before it and it itself attach, its 21-byte record and its
		// one-and-a-half-megabyte body read whole by one call that
		// grows the scan's own body buffer past the window.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		h, body := scanKindBody(KindWrite)
		seg, first := scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		h, body = bigWrite(int64(scanWindow) + scanWindow/2)
		seg, big := scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if len(got.Records) != 2 {
			t.Fatalf("records = %d, want the small write and the one past the window, both", len(got.Records))
		}
		for i, want := range []int64{first, big} {
			if got.Records[i].Header.Offset != want {
				t.Errorf("record %d at %d, want %d", i, got.Records[i].Header.Offset, want)
			}
		}
		if got.Records[1].Header.Extent.Len != int64(scanWindow)+int64(scanWindow)/2 {
			t.Errorf("the big write's length = %d, want it whole", got.Records[1].Header.Extent.Len)
		}
		if got.Records[1].Body.Payload != nil {
			t.Error("the big write survived with a payload, want it dropped")
		}
		if got.End != int64(len(seg)) || len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("end %d ranges %d sealed %v resynced %v, want a clean walk to the size", got.End, len(got.Ranges), got.Sealed, got.Resynced)
		}
	})

	t.Run("a record straddling a window refill", func(t *testing.T) {
		// Unmarks up to a refill's edge, then a write whose header
		// sits at the first slot of the next window and whose body
		// stretches two windows past it: the header asks for a refill,
		// the body for a read the window never serves, and both hold.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		const unmarks = scanWindow / 96
		for i := 0; i < unmarks; i++ {
			h, body := scanKindBody(KindUnmark)
			seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		}
		h, body := bigWrite(int64(2*scanWindow) + 56)
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})

		got, err := ScanSegment(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error", err)
		}
		if len(got.Records) != unmarks+1 {
			t.Fatalf("records = %d, want the %d unmarks and the straddling write", len(got.Records), unmarks+1)
		}
		last := got.Records[len(got.Records)-1]
		if last.Header.Kind != KindWrite || last.Header.Extent.Len != int64(2*scanWindow)+56 {
			t.Errorf("the straddling write came back as %d, length %d", last.Header.Kind, last.Header.Extent.Len)
		}
		if last.Body.Payload != nil {
			t.Error("the straddling write survived with a payload, want it dropped")
		}
		if got.End != int64(len(seg)) || len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("end %d ranges %d sealed %v resynced %v, want a clean walk to the size", got.End, len(got.Ranges), got.Sealed, got.Resynced)
		}
	})
}

// TestScanReadsOnlyTheBytesItNeeds pins the two halves of a refill's
// honesty: the window may reach ahead a megabyte, but a byte the scan never
// asked for must not fail it, and a byte it did ask for must fail it at the
// slot whose own read could not be made — the records before that slot
// attached, End at it. A device fails every read covering a bad sector, not
// only the read starting at it, so the reader here fails every read that
// covers one byte.
func TestScanReadsOnlyTheBytesItNeeds(t *testing.T) {
	injected := errors.New("format: an injected read failure")

	// coverFailer serves seg but fails every read covering byte off.
	coverFailer := func(seg []byte, off int64) *readAtCover {
		return &readAtCover{b: seg, off: off, err: injected}
	}

	t.Run("an unreadable byte in the footer is never read", func(t *testing.T) {
		// Five records, a verifying marker, then footer bytes holding
		// one unreadable byte. The scan ends at the marker and reads
		// no byte past it, so the footer's unreadable byte cannot
		// fail the walk — the fallback open runs exactly when the
		// footer is the damaged part.
		seg, offs := scanSegment(t, testIdentity, scanTestRecords()...)
		seal := scanSeal(t, &seg, testIdentity, testSegment)
		seg = append(seg, make([]byte, 256)...)
		bad := int64(len(seg)) - 100

		got, err := ScanSegment(coverFailer(seg, bad), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Fatalf("ScanSegment = %v, want no error: the scan ends at the marker and never reads the footer", err)
		}
		if !got.Sealed || got.End != seal {
			t.Errorf("sealed %v end %d, want sealed at the marker's own slot %d", got.Sealed, got.End, seal)
		}
		if len(got.Records) != 5 || got.Records[4].Header.Offset != offs[4] {
			t.Errorf("records = %d at %v, want the five before the marker", len(got.Records), got.Records)
		}
	})

	t.Run("an unreadable byte loses only its own record", func(t *testing.T) {
		// One unreadable byte inside the third record's header. The
		// refill the third record is examined under covers the bad
		// byte, but the scan needs only that record's own 96 bytes:
		// they cover the unreadable byte, so the read of them fails,
		// the record is not attached and End is its own slot — while
		// the two records before it, every one of their bytes
		// readable, attach whole.
		seg, offs := scanSegment(t, testIdentity, scanTestRecords()...)
		bad := offs[2] + 40

		got, err := ScanSegment(coverFailer(seg, bad), int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error: the record's own bytes do cover it", err)
		}
		if errors.Is(err, ErrDamaged) {
			t.Error("the injected error washed into ErrDamaged, want it kept as itself")
		}
		if got.End != offs[2] {
			t.Errorf("end %d, want the slot whose own bytes cannot be read %d", got.End, offs[2])
		}
		if len(got.Records) != 2 || got.Records[1].Header.Offset != offs[1] {
			t.Errorf("records = %+v, want the two readable ones, every byte of them readable, attached", got.Records)
		}
		if len(got.Ranges) != 0 || got.Resynced {
			t.Errorf("ranges %+v resynced %v, want no range and no resync from a read that failed", got.Ranges, got.Resynced)
		}
	})

	t.Run("a readable record past a window refill still attaches", func(t *testing.T) {
		// Two records per megabyte: the unreadable byte sits inside
		// the last record's header, whose slot lies inside the first
		// refill's own reach. Every slot the walk examines before it
		// needs only bytes the refill serves readably, so the
		// records before the bad byte attach — a refill that covers
		// it must fail only when the slot it was made for needs the
		// unreadable bytes itself.
		seg, _ := scanSegment(t, testIdentity, scanTestRecords()[:2]...)
		const unmarks = scanWindow/96 - 2 // readable slots wholly before the bad byte's own slot
		for i := 0; i < unmarks; i++ {
			h, body := scanKindBody(KindUnmark)
			seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		}
		last := int64(len(seg))
		h, body := scanKindBody(KindUnmark)
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		bad := int64(len(seg)) - 8

		got, err := ScanSegment(coverFailer(seg, bad), int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error: the last record's own bytes cover it", err)
		}
		if got.End != last {
			t.Errorf("end %d, want the last record's own slot %d, whose 96 bytes cover the bad byte", got.End, last)
		}
		if len(got.Records) == 0 || got.Records[len(got.Records)-1].Header.Offset >= last {
			t.Errorf("records = %d, want the ones before the unreadable byte attached", len(got.Records))
		}
	})

	t.Run("an unreadable byte in a zero run reaches the caller as itself", func(t *testing.T) {
		// The pinned scenario: the first record's length
		// damaged opens a search over more than a whole window of
		// zeros — the zero tail the search must walk slot by slot to
		// tell the range's kind — with one record past them. The
		// zero-telling rides the same window the search reads, so a
		// byte of the range that no read can reach — the cover failer
		// fails the refill and its narrow retry alike — must reach
		// the caller as itself: a byte nobody read can manufacture
		// neither a bad range nor a zero one, and no resync is
		// counted on the back of one. A merely transient failure the
		// narrow retry does serve is by design absorbed, so the
		// failing reader here is the persistent one.
		var seg []byte
		seg = AppendSegmentHeader(seg, testSegmentHeader)
		h, body := scanKindBody(KindWrite)
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		binary.LittleEndian.PutUint64(seg[48 : 48+96][40:48], 4<<20)
		seg = append(seg, make([]byte, scanWindow+64)...) // zeros the search walks
		h, body = scanKindBody(KindUnmark)
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		size := int64(len(seg))

		// The bad byte sits in the zero run past the first window's
		// reach, so the first read to cover it is the 96-byte
		// examination of the slot 88 bytes below it — the refill that
		// examination asks for and its narrow retry both reach it,
		// so both fail and the error is that slot's own. The range
		// before the failing slot closes and is kept — every byte of
		// it was read — as bad, for the damaged record's own bytes at
		// its head are not zeros.
		//
		// Once that refill has failed, the scan serves every later
		// slot narrow: the wide read past the failed one can reach
		// nothing the scan still needs before the unreadable byte,
		// so a refill tried anyway would only fail again. The counter
		// holds the wide reads past the first failed one — the refills
		// whose span outruns the slot's own bytes — which stickiness
		// holds at zero: without it, every slot the search steps to
		// pays one failed megabyte read, and the scan of a segment
		// whose next megabyte holds a bad sector dies under a
		// hundred-thousand-odd of them.
		bad := int64(SegmentHeaderSize + scanWindow + 64)
		failSlot := bad - 88 // the slot whose own examination first covers the bad byte
		want := ScanRange{Off: SegmentHeaderSize, End: failSlot, Kind: RangeBad}

		failer := coverFailer(seg, bad)
		got, err := ScanSegment(failer, size, testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Fatalf("ScanSegment = %v, want the injected error: bytes the range's kind is told from must be read", err)
		}
		if errors.Is(err, ErrDamaged) {
			t.Error("the injected error washed into ErrDamaged, want it kept as itself")
		}
		if got.Resynced {
			t.Error("a resync counted off the back of a failed read, want none")
		}
		if len(got.Records) != 0 {
			t.Errorf("records = %d, want none attached past a search whose read failed", len(got.Records))
		}
		if len(got.Ranges) != 1 || got.Ranges[0] != want || got.End != failSlot {
			t.Errorf("ranges = %+v end %d, want the one closed at the failing slot %d, %+v", got.Ranges, got.End, failSlot, want)
		}
		if failer.wide != 1 {
			t.Errorf("wide reads past the first failed one = %d, want the one that failed alone, with none after it", failer.wide)
		}
	})
}

// TestScanNarrowModeCoversOnlyTheFailedRefillsOwnSpan: a wide refill that
// fails once — a transient failure over a newest segment's zero tail, the
// exact shape a fallback open scans — narrows only the span that refill
// covered, and wide reads resume past it: the scan pays one window's worth
// of slots read on their own bytes, then a refill a window again. Narrow
// mode held to the scan's end would pay one read per slot instead, a read
// an 8 bytes for as long as the tail runs — past a million over this
// segment's eight megabytes, thirty times that over a full cap.
func TestScanNarrowModeCoversOnlyTheFailedRefillsOwnSpan(t *testing.T) {
	// A segment of nothing but its header and eight megabytes of zeros:
	// every slot a search must step on, the walk of an unwritten tail.
	var seg []byte
	seg = AppendSegmentHeader(seg, testSegmentHeader)
	seg = append(seg, make([]byte, 8<<20)...)
	size := int64(len(seg))

	failer := &readAtFailFirstWide{b: seg, err: errors.New("format: a transient refill failure")}
	got, err := ScanSegment(failer, size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error: the retry serves the slot's own bytes", err)
	}
	want := ScanRange{Off: SegmentHeaderSize, End: size, Kind: RangeZero}
	if len(got.Ranges) != 1 || got.Ranges[0] != want || len(got.Records) != 0 || got.Sealed || got.Resynced || got.End != size {
		t.Fatalf("scan ranges %+v records %d sealed %v resynced %v end %d, want the one zero range over the whole tail and nothing else", got.Ranges, len(got.Records), got.Sealed, got.Resynced, got.End)
	}

	// One window's worth of slots read narrow — the 131,072 slots a
	// megabyte holds — plus the wide refills the rest of the tail pays
	// one of a window: about 131,080 reads.
	if !failer.failed {
		t.Fatalf("the reader never failed a wide read, want the first refill to fail alone")
	}
	if failer.reads > 132000 {
		t.Errorf("reads = %d, want at most one window of narrow reads and a handful of wide refills, 132,000", failer.reads)
	}
}

// readAtFailFirstWide serves seg but fails its first wide read alone — one
// spanning more than a slot's own bytes, the refill a transient failure
// would hit once — and counts every read it is asked for.
type readAtFailFirstWide struct {
	b      []byte
	err    error
	failed bool  // the one wide read has failed
	reads  int64 // every ReadAt made
}

func (r *readAtFailFirstWide) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if !r.failed && int64(len(p)) > RecordHeaderSize {
		r.failed = true
		return 0, r.err
	}
	return copy(p, r.b[off:]), nil
}

// readAtCover is an io.ReaderAt that serves seg but fails every read
// covering one byte — as a device fails every read over a bad sector —
// and counts the wide covering reads it served: those spanning more than
// the bytes a single slot asks for, the reads the scan's sticky narrow
// mode exists to make none of once one has failed.
type readAtCover struct {
	b    []byte
	off  int64
	err  error
	wide int64 // covering reads spanning more than a slot's own bytes
}

func (r *readAtCover) ReadAt(p []byte, off int64) (int, error) {
	spanning := r.off >= off && r.off < off+int64(len(p))
	if !spanning {
		return copy(p, r.b[off:]), nil
	}
	// A record's body is read by its own readFull and may span more
	// than a slot's bytes by its own right; those reads are not the
	// window's refills, and the wide counter holds the refills alone —
	// the reads that outrun 96 bytes only because the window reaches.
	if int64(len(p)) > RecordHeaderSize {
		r.wide++
	}
	return 0, r.err
}
