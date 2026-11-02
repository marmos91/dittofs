package format

import (
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// scanWindow is the bytes one refill of the scan's sliding window reads, so
// a walk over a segment pays about one read of each byte of it and a forward
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
		seed:     identitySeed(identity),
		identity: identity,
		segment:  segment,
		window:   make([]byte, scanWindow),
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
			if err := sc.search(off); err != nil {
				return sc.Scan, err
			}
			off = sc.End
		}
	}
	sc.End = size
	return sc.Scan, nil
}

// scanner is one segment's scan: the reader, its bounds, the identity's seed
// the checksums verify under, and the sliding window every slot is read
// through, refilled at the slot being examined.
type scanner struct {
	r        io.ReaderAt
	size     int64
	seed     uint32
	identity core.JournalID
	segment  core.SegmentID
	window   []byte
	cur      int64 // the window's first byte's offset in the segment
	have     int64 // the bytes the window holds
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
	// end may hold fewer: such a slot holds neither a prefix to tell by
	// nor a record, so whatever its bytes are, the forward search reports
	// them in the range they end in. The grid being 8, at most four of
	// the telling bytes can exist without the slot being whole anyway.

	if n < 4 {
		// A slot fewer than 4 bytes long — the segment ends inside it —
		// holds neither a prefix to tell by nor a record: whatever its
		// bytes, the forward search reports them in the range they end
		// in, and the slot contributes no run of its own.
		return slot{outcome: slotNothing}
	}

	switch {
	case zero(b[:4]):
		return slot{outcome: slotNothing} // a zero slot; its range's kind is its bytes' own
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
		return slot{outcome: slotNothing} // a bad slot: unknown bytes
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
	bs := h.BodySize()
	if int64(cap(sc.body)) < bs {
		sc.body = make([]byte, max(int64(cap(sc.body))*2, bs))
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
// verifying seal marker to stop at, or an outcome to stop and return.
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
func (sc *scanner) search(off int64) error {
	end := sc.size
	found := slot{outcome: slotNothing}
	for p := off + 8; p < sc.size; p += 8 {
		if s := sc.examine(p); s.outcome != slotNothing {
			found = s
			end = p
			break
		}
	}

	// decision: the range's kind is decided by its bytes — every byte zero,
	// or at least one not — and never by the slot that started it, so a
	// zero slot followed by junk then a header reports the junk as bad
	// bytes, not as the zeros the first slot's four bytes promised.
	// Recovery classes the range torn or corrupt from the synced-through
	// offsets and seal markers it holds; the scan's job is the bytes.
	// Overturned if the zero-slot rule rules a failed write's zeros apart
	// from damage inside one range.
	kind := RangeBad
	if sc.bytesAllZero(off, end) {
		kind = RangeZero
	}

	// decision: the range's kind is decided by its bytes — every byte zero
	// or at least one not — and never by the slot that started it, so a
	// zero slot followed by junk then a header reports the junk as bad
	// bytes, not as the zeros the slot's first four bytes promised.
	// Recovery classes the range from the synced-through offsets and seal
	// markers it holds; the scan's job is the bytes. Overturned if the
	// zero-slot rule distinguishes a failed write's zeros from damage
	// inside one range.
	sc.Ranges = append(sc.Ranges, ScanRange{Off: off, End: end, Kind: kind})
	sc.End = end
	switch found.outcome {
	case slotNothing:
		// Nothing verifies to the segment's end: the range stands as the
		// whole tail and the scan is over. The zero-slot rule's "only
		// zeros with no verifying header anywhere after them" — plus the
		// residue of any bad record — is what the caller asked for.
		return nil
	case slotRecord:
		if kind == RangeBad {
			sc.Resynced = true // a range of bad bytes, a record found past it
		}
		sc.Records = append(sc.Records, found.record)
		sc.End = end + found.record.Header.Size()
		return nil
	case slotSeal:
		sc.Sealed = true
		return nil
	case slotStop:
		return found.err
	}
	panic("format: scan search switch is not exhaustive")
}

// bytesAllZero reports whether every byte of [off, end) is zero, read
// window by window so a range a segment's whole cap long costs no more
// memory than the scan's one window.
func (sc *scanner) bytesAllZero(off, end int64) bool {
	const chunk = int64(scanWindow)
	for p := off; p < end; p += chunk {
		n := min(chunk, end-p)
		b, err := sc.read(p, n)
		if err != nil || !zero(b) {
			return false
		}
	}
	return true
}

// read fills the scan's window so that [off, off+n) lies in it, and returns
// it. A refill reads scanWindow bytes at off, so a slot the scan walks to
// finds its bytes in hand unless the walk crossed the window's whole span,
// and any read error — a short read's io.ErrUnexpectedEOF or the reader's
// own error — is returned wrapped with the offset it was read at, as itself.
func (sc *scanner) read(off, n int64) ([]byte, error) {
	if off < sc.cur || off+n > sc.cur+sc.have {
		want := min(int64(scanWindow), sc.size-off)
		if n > want {
			want = n
		}
		if err := readFull(sc.r, sc.window[:want], off); err != nil {
			return nil, fmt.Errorf("format: read slot at %d: %w", off, err)
		}
		sc.cur, sc.have = off, want
	}
	return sc.window[off-sc.cur : off-sc.cur+n], nil
}

// isStop reports whether err is an outcome the scan stops at rather than
// evidence a slot is bad: a kind this format version does not define — one
// no length may be read from, because "an unknown kind may be a removal, and
// stepping over a removal resurrects what it removed" — or a non-zero half
// of a content version, which this format version forbids outright.
func isStop(err error) bool {
	return errors.Is(err, ErrUnknownKind) || errors.Is(err, ErrNonZeroEpoch)
}
