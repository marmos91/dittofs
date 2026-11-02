package format

import (
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// scanWindow is the bytes one refill of the scan's sliding window reads, so
// a walk over a segment pays about one read of each byte of it — save for
// the record bodies, each read again by its own readFull — and a forward
// search skips whole slots on bytes already in hand.
const scanWindow = 1 << 20

// ScanRangeKind says what a range the scan stepped over holds.
type ScanRangeKind uint8

const (
	RangeZero ScanRangeKind = 1 // every byte of the range is zero
	RangeBad  ScanRangeKind = 2 // at least one byte of the range is not zero
)

// ScanRange is a run of bytes [Off, End) the scan attached nothing from.
type ScanRange struct {
	Off, End int64
	Kind     ScanRangeKind
}

// ScannedRecord is a record the scan attached: its header (Offset and
// SyncedThrough included) and its decoded body with Payload always nil.
//
// decision: the payload is not retained. The scan verifies every body whole
// and hands out only the header and the fixed body fields, because no reader
// may "hand out or copy any part of a record that does not verify" — and the
// records a scan hands out are read again, and their bodies re-verified with
// VerifyRecordBody, by whoever serves bytes from them. Keeping the payload
// would also pin the scan's body buffer alive for every attached record.
// Overturned if a caller is ruled to keep whole verified payloads across the
// open rather than re-reading them.
type ScannedRecord struct {
	Header RecordHeader
	Body   RecordBody
}

// Scan is what a scan of one segment found. Resynced is the scan's
// scan_resyncs contribution — "scans that found a record by resynchronising
// past a bad one" — so it is a bool counting the scan, not the resyncs: two
// bad records stepped over in one scan count one, and a search that ends at
// a seal marker, a stop or the segment's end without attaching a record
// counts none. What a found range was torn or corrupt recovery decides; the
// scan counts only whether it reached a record past one range of bad bytes.
type Scan struct {
	Records  []ScannedRecord // ascending by Header.Offset
	Ranges   []ScanRange     // ascending; with Records they tile [SegmentHeaderSize, End)
	Sealed   bool            // a seal marker that verifies ended the scan at End
	End      int64           // where the scan ended: the seal marker's offset, the failing offset, or size
	Resynced bool            // a RangeBad is followed directly by an attached record
}

// slotOutcome is what a slot holds, in the order the scan stops caring:
// nothing it can use, a record it can attach, a marker that seals the
// segment, or an outcome the scan must stop and hand to the caller at that
// offset — a header whose kind or epoch this format version cannot read.
type slotOutcome uint8

const (
	slotNothing slotOutcome = iota // zero or bad: pass by to the next slot
	slotRecord                     // a record to attach at this slot
	slotSeal                       // a verifying seal marker at this slot
	slotStop                       // an error to stop the scan at this slot
)

// slot is one examination: the outcome, the record when it is one, and the
// stop's own error, its decode's message naming the rule that made it.
type slot struct {
	outcome slotOutcome
	record  ScannedRecord
	err     error
}

// ScanSegment walks a segment with no usable footer record by record from
// the first record slot at SegmentHeaderSize, and resynchronises past damage
// by searching forward at each 8-byte boundary for the next header that
// verifies. The segment header itself is never read: the open decodes it.
//
// It calls nothing damage or torn. It reports the records whose header and
// body both verify — their headers, including their synced-through offsets
// and bodies, with the payload dropped — and the byte runs it attached
// nothing from, with what those bytes are: all zeros, or at least one byte
// not zero. Which of those is a torn tail and which corruption is recovery's
// to decide, from the synced-through offsets and the seal markers it holds.
//
// A scan stops and returns its error at a header this format version cannot
// read over — one of an unknown kind, which "is never skipped by its length",
// or a non-zero epoch — with the record never stepped over and everything
// attached before its offset kept.
//
// decision: a read that fails is returned as itself, wrapped with the offset
// it was read at and never as zeros, as a bad slot or as damage — for
// "a read that fails is not a record that does not verify", and unreadable
// never counts as empty: a failed read says nothing about the bytes it was
// asked for, so it is neither evidence of absence nor evidence of damage, and
// a segment the device cannot read must fail its caller rather than come
// back as a scan over bytes nobody read. Overturned only if the format rules
// a failed read is damage.
//
// On every error return the Scan holds everything attached and every range
// closed before the failing offset, and End is that offset.
func ScanSegment(r io.ReaderAt, size int64, identity core.JournalID, segment core.SegmentID) (Scan, error) {
	if size < SegmentHeaderSize {
		return Scan{}, fmt.Errorf("%w: %d bytes, too short for the first record slot at %d", ErrDamaged, size, SegmentHeaderSize)
	}

	sc := scanner{
		r:        r,
		size:     size,
		identity: identity,
		segment:  segment,
		window:   make([]byte, min(scanWindow, size)),
	}
	sc.End = SegmentHeaderSize
	off := int64(SegmentHeaderSize)
	for off < size {
		switch s := sc.examine(off); s.outcome {
		case slotRecord:
			sc.End = off + s.record.Header.Size()
			sc.Records = append(sc.Records, s.record)
			off = sc.End
		case slotSeal:
			sc.End = off
			sc.Sealed = true
			return sc.Scan, nil
		case slotStop:
			sc.End = off
			return sc.Scan, s.err
		case slotNothing:
			// A zero or bad slot the walk cannot step over by any length
			// it knows: search forward for the next slot it can use, and
			// report the bytes passed over as one range.
			end, s, zeroRun, err := sc.search(off)
			// decision: the range's kind is decided by its bytes — every
			// byte the search read zero, or at least one not — and never
			// by the slot that started it, so a zero slot followed by
			// junk then a header reports the junk as bad bytes, not as
			// the zeros the first slot's four bytes promised. The bytes
			// are told from the slots the search steps on, eight each, so
			// no byte is read twice and a byte nobody read tells
			// nothing. Recovery classes the range torn or corrupt from
			// the synced-through offsets and seal markers it holds; the
			// scan's job is the bytes. Overturned if the zero-slot rule
			// rules a failed write's zeros apart from damage inside one
			// range.
			//
			// The range is closed at end and kept even when the search
			// stopped there on an error, because every byte before end
			// was read before the failing one: its kind is known, and
			// every range closed before the failing offset stays with
			// the Scan. end > off always, for the search's first
			// telling reads from the window the walk's own examination
			// of off filled, and cannot fail.
			kind := RangeBad
			if zeroRun {
				kind = RangeZero
			}
			sc.Ranges = append(sc.Ranges, ScanRange{Off: off, End: end, Kind: kind})
			sc.End = end
			if err != nil {
				return sc.Scan, err
			}
			if s.outcome == slotRecord {
				if kind == RangeBad {
					sc.Resynced = true // a range of bad bytes, a record found past it
				}
				sc.Records = append(sc.Records, s.record)
				sc.End = end + s.record.Header.Size()
			}
			// A seal found by the search is not sealed here: the loop
			// turns to its slot and lets the walk's own examination seal
			// it, so the seal rule lives in exactly one place.
			off = sc.End
		}
	}
	// The loop is over only at off == sc.End == size: every path through
	// it leaves End at the offset the walk stands at, and the walk stands
	// on nothing past the segment's own bytes.
	return sc.Scan, nil
}

// scanner is one segment's scan: the reader, its bounds, the identity the
// checksums verify under, and the sliding window every slot is read
// through, refilled at the slot being examined.
type scanner struct {
	r        io.ReaderAt
	size     int64
	identity core.JournalID
	segment  core.SegmentID
	window   []byte
	cur      int64 // the window's first byte's offset in the segment
	have     int64 // the bytes the window holds
	narrow   bool  // a refill has failed; read each slot's own bytes alone
	body     []byte

	Scan
}

// examine classifies and judges one slot at off, returning what the scan
// does with it. A record is attached by the caller; nothing and its errors
// are the scan's own to carry.
//
// The prefix rule first — "a scan tells a slot by its first four bytes",
// tested cheap before any checksum is computed — and only then the
// decoders, which is what makes a forward search cheap to run.
func (sc *scanner) examine(off int64) slot {
	n := min(int64(RecordHeaderSize), sc.size-off)
	b, err := sc.read(off, n)
	if err != nil {
		return slot{outcome: slotStop, err: err}
	}

	// The prefix rule's telling bytes are four, and a slot at the segment's
	// end may hold fewer: no case below may slice four bytes that were not
	// read, for the window holds stale bytes past the read's own end.
	//
	// A slot fewer than 4 bytes long — the segment ends inside it — holds
	// neither a prefix to tell by nor a record: whatever its bytes, the
	// forward search reports them in the range they end in, and the slot
	// contributes no run of its own.
	if n < 4 {
		return slot{outcome: slotNothing}
	}

	switch {
	case string(b[:4]) == "DJR1":
		return sc.examineRecord(b, off)
	case string(b[:4]) == "DJS1":
		if n < SealMarkerSize {
			// A marker the segment ends in the middle of is not a marker;
			// bad, like a header cut short.
			return slot{outcome: slotNothing}
		}
		if _, err := DecodeSealMarker(b[:SealMarkerSize], sc.identity, sc.segment, off); err == nil {
			return slot{outcome: slotSeal}
		}
		// A marker that does not verify here — a copy inside a payload,
		// another journal's, an earlier life's naming another segment —
		// never ends a scan.
		return slot{outcome: slotNothing}
	default:
		// A bad slot: unknown bytes, or a zero one — a zero slot is
		// nothing here the same as any other untellable bytes, and only
		// the search's zero-telling tells the range's kind, from bytes
		// it reads for itself. Drawing a zero case apart here would
		// change what it returns by nothing at all.
		return slot{outcome: slotNothing}
	}
}

// examineRecord judges a DJR1 slot's whole record: the header decodes, the
// record fits the segment, and the body verifies. A kind or epoch this
// format version cannot read over stops the scan at the slot, whatever the
// record's own condition; a record that overruns the segment or whose body
// fails the checksum is bad, walked on by the forward search.
func (sc *scanner) examineRecord(b []byte, off int64) slot {
	if len(b) < RecordHeaderSize {
		// A header the segment ends short of is bad: nothing about the
		// record can be read, so no length reaches the caller either.
		return slot{outcome: slotNothing}
	}
	h, err := DecodeRecordHeader(b[:RecordHeaderSize], sc.identity, sc.segment, off)
	if err != nil {
		if isStop(err) {
			return slot{outcome: slotStop, err: fmt.Errorf("format: record at %d: %w", off, err)}
		}
		// Damaged or wrong place: a copy written elsewhere, an old life's
		// record in a recycled segment, another journal's record. Bad.
		return slot{outcome: slotNothing}
	}

	// The overrun check comes before any body is read: a record whose body
	// the segment cannot hold is bad on its header alone.
	if off+h.Size() > sc.size {
		return slot{outcome: slotNothing}
	}

	// The body, whole: one checksum covers it all, so no part of a record
	// that does not verify is ever read as content.
	//
	// ponytail: the body is read by its own readFull even when the window
	// already holds it, so every record body costs its bytes a second read
	// and an open's fallback scan of a newest segment pays about twice its
	// content I/O. Copy the bytes the window holds and readFull only the
	// remainder when an open's recovery time shows the double read.
	bs := h.BodySize()
	if int64(cap(sc.body)) < bs {
		// bs fits the segment by the overrun check above, so it is the
		// whole size needed: no doubling growth, which could allocate up
		// to twice the segment for one record.
		sc.body = make([]byte, bs)
	}
	body := sc.body[:bs]
	if err := readFull(sc.r, body, off+RecordHeaderSize); err != nil {
		return slot{outcome: slotStop, err: fmt.Errorf("format: read body at %d: %w", off+RecordHeaderSize, err)}
	}
	verified, err := VerifyRecordBody(body, sc.identity, h)
	if err != nil {
		if isStop(err) {
			return slot{outcome: slotStop, err: fmt.Errorf("format: record at %d: %w", off, err)}
		}
		// A body that does not verify is bad: "a scan that meets a record
		// that does not verify MUST refuse that record and resynchronise".
		return slot{outcome: slotNothing}
	}
	verified.Payload = nil
	return slot{outcome: slotRecord, record: ScannedRecord{Header: h, Body: verified}}
}

// search runs the forward search from a zero or bad slot at off: at each
// 8-byte boundary past it, the same rule the walk applies, until the
// segment's end or a slot the scan can use — a record to attach, a
// verifying seal marker to seal at, or an outcome to stop and return — and
// at the same time tells whether every byte it read over is zero, from the
// bytes it read them from.
//
// decision: the search stops at a verifying seal marker and reports the
// range before it, exactly as the walk does, because "a seal marker that
// verifies proves every record before it synced": a bad record or a zero
// slot before such a marker is corruption for recovery to class, and "bytes
// after a seal marker belong to the footer, never to records" — a scan that
// stepped over the marker would read the footer as records. The marker's
// own iteration proves the bytes before it; the report carries the range.
// Overturned if the format rules a sealed segment is never scanned.
//
// ponytail: "The forward search reads the whole unwritten remainder of each
// newest segment at open, up to one segment cap per stream." Upgrade to
// bounding it by the stream's largest in-flight reservation — a later
// record can start no further past a failed slot than the bytes reserved
// ahead of it — when a recovery time at open shows the zero scan.
//
// The zero-telling rides the bytes the search reads anyway, so the range's
// kind costs no second pass: each slot it steps over contributes the
// min(8, size-p) bytes it holds — together, from the slot at off to the last
// one before end, the whole [off, end) minus whatever residue the final
// partial slot holds, which is bounded by the same reads. A read that fails
// is returned as itself: a byte nobody read tells nothing, so a failed read
// can manufacture neither a bad range — the zero it never saw cannot stand
// as a byte not zero — nor a zero one, and no resync is counted on the back
// of bytes nobody read.
func (sc *scanner) search(off int64) (end int64, found slot, zeroRun bool, err error) {
	end = sc.size
	found = slot{outcome: slotNothing}
	// The walk's examination of off has already put [off, off+8) in the
	// window, so this telling is a window hit: it cannot fail, and the
	// branch below keeps the error contract without ever being taken —
	// end > off below depends on it.
	slotZero, err := sc.zeroSlot(off)
	if err != nil {
		return off, found, false, err
	}
	zeroRun = slotZero
	for p := off + 8; p < sc.size; p += 8 {
		s := sc.examine(p)
		if s.outcome == slotStop {
			// A header this format version cannot read over: the
			// range closes at it and the stop goes to the caller
			// with the records the walk attached before it.
			return p, s, zeroRun, s.err
		}
		if s.outcome != slotNothing {
			end = p
			found = s
			break
		}
		slotZero, err := sc.zeroSlot(p)
		if err != nil {
			// end stays size: bytes past p may not have been read, so
			// the range's own telling stops at p — but every slot the
			// search read before it has told its part, and those are
			// the bytes the walk closes the range over.
			return p, found, zeroRun, err
		}
		zeroRun = zeroRun && slotZero
	}
	return end, found, zeroRun, nil
}

// zeroSlot tells whether the bytes one slot holds — the min(8, size-p) bytes
// it holds past p, always at least one, for every caller passes p < size —
// are all zero, reading through the same window examine reads, so a slot
// whose bytes are already in hand costs nothing at all.
func (sc *scanner) zeroSlot(p int64) (bool, error) {
	b, err := sc.read(p, min(int64(8), sc.size-p))
	if err != nil {
		return false, err
	}
	return zero(b), nil
}

// read fills the scan's window so that [off, off+n) lies in it, and returns
// it. A refill reads scanWindow bytes at off, so a slot the scan walks to
// finds its bytes in hand unless the walk crossed the window's whole span.
//
// A refill reaches ahead of the bytes its slot needs, so a byte that cannot
// be read — a bad sector in the footer past a seal marker, in a record the
// walk has not reached — must not fail the slot that only asked for its own
// bytes: when a refill fails, the exact [off, off+n) the slot needs is
// retried alone, and the scan fails only if those bytes cannot be read. That
// keeps both halves of the read-error rule: a read that fails is returned
// as itself, wrapped with the offset it was read at — and a read the scan
// never asked for, over bytes past its marker or below its next slot, never
// fails it.
//
// Once a refill has failed, the bytes ahead of that slot stay unreadable to
// the wide read, and a scan that marched on would pay one failed refill per
// slot it steps to. The scan then reads each slot's own bytes alone until it
// ends — and it ends at the byte that cannot be read, for every read that
// covers it fails, the narrow ones as surely as the wide ones. The narrow
// mode is sticky for that reason: no slot past the unreadable byte is ever
// served, so no wide refill past it could ever succeed.
func (sc *scanner) read(off, n int64) ([]byte, error) {
	if off >= sc.cur && off+n <= sc.cur+sc.have {
		return sc.window[off-sc.cur : off-sc.cur+n], nil
	}
	want := min(int64(scanWindow), sc.size-off)
	if sc.narrow {
		// Every caller asks only the bytes its own slot holds, min(96,
		// size-off) or fewer, so a healthy refill always serves them;
		// under narrow, the slot's own bytes are all the scan reads.
		want = n
	}
	if err := readFull(sc.r, sc.window[:want], off); err != nil {
		// The refill reached bytes the slot never asked for; only the
		// slot's own bytes are the scan's to need.
		if n < want {
			if err := readFull(sc.r, sc.window[:n], off); err != nil {
				return nil, fmt.Errorf("format: read slot at %d: %w", off, err)
			}
			// The refill found a byte it could not read ahead of this
			// slot; the bytes past that one are unreadable to every
			// wide read the rest of the scan could make. Only a read
			// covering that byte reaches it, and that read fails
			// however narrow it is, so serving slots one by one fails
			// at that byte and never past it. Only [off, off+n) was
			// read: the window holds nothing the scan may trust past
			// it.
			sc.narrow = true
			sc.cur, sc.have = off, n
			return sc.window[:n], nil
		}
		return nil, fmt.Errorf("format: read slot at %d: %w", off, err)
	}
	// The bytes back are always exactly the slot's own [off, off+n); a
	// refill's wider fetch stays in the window, for the callers tell
	// zeroes and decode headers by len(b), so no path may hand back a
	// slice longer than the one asked for.
	sc.cur, sc.have = off, want
	return sc.window[:n], nil
}

// isStop reports whether err is an outcome the scan stops at rather than
// evidence a slot is bad: a kind this format version does not define — one
// no length may be read from, because "an unknown kind may be a removal, and
// stepping over a removal resurrects what it removed" — or a non-zero half
// of a content version, which this format version forbids outright.
func isStop(err error) bool {
	return errors.Is(err, ErrUnknownKind) || errors.Is(err, ErrNonZeroEpoch)
}
