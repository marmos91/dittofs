package format

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// FuzzFormatFile drives the decoder with any bytes. Whatever arrives, it
// either decodes or matches exactly one outcome class, never a panic; and
// whatever decodes is exactly what the bytes contain, so encoding it again
// reproduces the input with the generation zeroed.
func FuzzFormatFile(f *testing.F) {
	golden := readGolden(f, "format_v1")

	f.Add(golden)
	f.Add(slices.Clone(golden[:55]))
	f.Add(slices.Clone(golden[:5]))
	f.Add(append(slices.Clone(golden), 0x00))
	f.Add(raw.Format{Version: 2, Identity: testIdentity, Installation: testInstallation}.Bytes())

	generation := slices.Clone(golden)
	copy(generation[40:48], []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48})
	rechecksum(generation)
	f.Add(generation)

	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, in []byte) {
		file, err := DecodeFormatFile(in)

		if err == nil {
			if len(in) != FormatFileSize {
				t.Fatalf("decoded %d bytes, want %d", len(in), FormatFileSize)
			}
			if got, want := binary.LittleEndian.Uint32(in[52:56]), wantCRC(in[0:52]); got != want {
				t.Fatalf("decoded bytes whose checksum is 0x%08x, want the CRC32C of [0,52): 0x%08x", got, want)
			}

			// The decoder must yield only what the bytes contain: the
			// input again, with the never-read generation zeroed and
			// the checksum recomputed for it.
			want := slices.Clone(in)
			clear(want[40:48]) // the generation is never read
			rechecksum(want)
			if got := AppendFormatFile(nil, file); !slices.Equal(got, want) {
				t.Fatalf("AppendFormatFile =\n% x\nwant the input with a zero generation\n% x", got, want)
			}
			return
		}

		damaged := errors.Is(err, ErrDamaged)
		version := errors.Is(err, ErrUnknownVersion)
		if damaged == version {
			t.Fatalf("DecodeFormatFile = %v, want an error matching exactly one of ErrDamaged and ErrUnknownVersion", err)
		}
	})
}

// FuzzSegmentHeader drives the decoder with any bytes. Whatever arrives, it
// either decodes or matches exactly one outcome class, never a panic; and
// whatever decodes holds exactly the bytes' fields: on a clean decode
// encoding it again reproduces the input, and on a foreign one the header is
// returned filled — its identity, id, predecessor and stream match the
// input's bytes, though its reserved bytes, which the header has no field
// for, are never reproduced.
func FuzzSegmentHeader(f *testing.F) {
	pred := readGolden(f, "segheader_pred")
	nopred := readGolden(f, "segheader_nopred")

	f.Add(pred)
	f.Add(nopred)
	f.Add(slices.Clone(pred[:47]))
	f.Add(append(slices.Clone(pred), 0x00))

	v2 := slices.Clone(pred)
	binary.LittleEndian.PutUint16(v2[4:6], 2)
	f.Add(v2)

	other := testSegmentHeader
	other.Identity = otherIdentity
	f.Add(AppendSegmentHeader(nil, other))

	// A foreign header whose reserved bytes are not zero: its checksum
	// verifies, its identity is another journal's, and its reserved bytes
	// are never judged here, so it must be told apart as foreign with the
	// fields its bytes carry.
	foreignReserved := AppendSegmentHeader(nil, other)
	copy(foreignReserved[6:8], []byte{0xff, 0xff})
	rechecksumHeader(foreignReserved)
	f.Add(foreignReserved)

	// This journal's own header with non-zero reserved bytes, and one with
	// an id of zero: both verify their checksum, so both reach the field
	// checks and are refused as damage, never as foreign.
	ownReserved := AppendSegmentHeader(nil, testSegmentHeader)
	copy(ownReserved[6:8], []byte{0xff, 0xff})
	rechecksumHeader(ownReserved)
	f.Add(ownReserved)

	ownZero := AppendSegmentHeader(nil, testSegmentHeader)
	clear(ownZero[24:32])
	rechecksumHeader(ownZero)
	f.Add(ownZero)

	f.Add([]byte{})
	f.Add(make([]byte, SegmentHeaderSize))

	f.Fuzz(func(t *testing.T, in []byte) {
		h, err := DecodeSegmentHeader(in, testIdentity)

		if err == nil {
			if len(in) != SegmentHeaderSize {
				t.Fatalf("decoded %d bytes, want %d", len(in), SegmentHeaderSize)
			}
			if got, want := binary.LittleEndian.Uint32(in[44:48]), wantCRC(in[0:44]); got != want {
				t.Fatalf("decoded bytes whose checksum is 0x%08x, want the CRC32C of [0,44): 0x%08x", got, want)
			}
			// A clean decode is by construction this journal's own: the
			// identity matched, and a header of no segment is damage.
			if h.Identity != testIdentity {
				t.Fatalf("decoded a header under identity % x, want the identity it was opened with % x", h.Identity, testIdentity)
			}
			if h.ID == 0 {
				t.Fatalf("decoded a header with a segment id of 0, want 1 or more")
			}
			if got := AppendSegmentHeader(nil, h); !slices.Equal(got, in) {
				t.Fatalf("AppendSegmentHeader =\n% x\nwant the input\n% x", got, in)
			}
			return
		}

		if errors.Is(err, ErrForeign) {
			// A foreign header is returned filled, so recovery can report
			// whose it is: every field must say what the bytes hold, and
			// the identity it names is never the one it was opened with.
			if h.Identity == testIdentity {
				t.Fatalf("a header reported foreign under the identity it was opened with")
			}
			if h.Identity != core.JournalID(in[8:24]) {
				t.Fatalf("a foreign header returned the identity % x, want the input's % x", h.Identity, in[8:24])
			}
			if h.ID != core.SegmentID(binary.LittleEndian.Uint64(in[24:32])) {
				t.Fatalf("a foreign header returned segment id %d, want the input's %d", h.ID, core.SegmentID(binary.LittleEndian.Uint64(in[24:32])))
			}
			if h.Predecessor != core.SegmentID(binary.LittleEndian.Uint64(in[32:40])) {
				t.Fatalf("a foreign header returned predecessor %d, want the input's %d", h.Predecessor, core.SegmentID(binary.LittleEndian.Uint64(in[32:40])))
			}
			if h.Stream != binary.LittleEndian.Uint32(in[40:44]) {
				t.Fatalf("a foreign header returned stream %d, want the input's %d", h.Stream, binary.LittleEndian.Uint32(in[40:44]))
			}
		}

		matched := 0
		for _, class := range []error{ErrDamaged, ErrUnknownVersion, ErrForeign} {
			if errors.Is(err, class) {
				matched++
			}
		}
		if matched != 1 {
			t.Fatalf("DecodeSegmentHeader = %v, want an error matching exactly one of ErrDamaged, ErrUnknownVersion and ErrForeign", err)
		}
	})
}

// FuzzSeal drives the decoder with any bytes read at any segment and offset.
// Whatever arrives, it either decodes, matches ErrWrongPlace — a marker that
// verifies but was written somewhere else, returned filled with the segment
// and offset its bytes hold — or matches exactly one of the outcome classes,
// never a panic; and whatever comes back is exactly what the bytes contain,
// so encoding it again reproduces the input.
func FuzzSeal(f *testing.F) {
	golden := readGolden(f, "seal")

	f.Add(golden, uint64(testSegment), testOffset)
	f.Add(golden, uint64(testSegment), testOffset+32)
	f.Add(golden, uint64(testSegment)+1, testOffset)

	f.Add(AppendSealMarker(nil, otherIdentity, testSealMarker), uint64(testSegment), testOffset)

	f.Add(slices.Clone(golden[:31]), uint64(testSegment), testOffset)
	f.Add(append(slices.Clone(golden), 0x00), uint64(testSegment), testOffset)
	f.Add(make([]byte, SealMarkerSize), uint64(testSegment), testOffset)
	f.Add([]byte(nil), uint64(testSegment), testOffset)

	// A marker whose reserved bytes are not zero but whose checksum was
	// recomputed for them: only the reserved rule can refuse it.
	reserved := slices.Clone(golden)
	reserved[4] = 0xff
	rechecksumSeal(reserved, testIdentity)
	f.Add(reserved, uint64(testSegment), testOffset)

	f.Fuzz(func(t *testing.T, in []byte, segment uint64, offset int64) {
		m, err := DecodeSealMarker(in, testIdentity, core.SegmentID(segment), offset)

		if err == nil {
			if len(in) != SealMarkerSize {
				t.Fatalf("decoded %d bytes, want %d", len(in), SealMarkerSize)
			}
			if got, want := binary.LittleEndian.Uint32(in[28:32]), wantSeededCRC(testIdentity, in[0:28]); got != want {
				t.Fatalf("decoded bytes whose checksum is 0x%08x, want the seeded CRC32C of [0,28): 0x%08x", got, want)
			}
			if m.Segment != core.SegmentID(segment) || m.Offset != offset {
				t.Fatalf("decoded %+v, want the segment %d and offset %d it was read at", m, segment, offset)
			}
			if got := AppendSealMarker(nil, testIdentity, m); !slices.Equal(got, in) {
				t.Fatalf("AppendSealMarker =\n% x\nwant the input\n% x", got, in)
			}
			return
		}

		if errors.Is(err, ErrWrongPlace) {
			// The marker is data even where it fails: re-encoding what
			// its bytes hold reproduces the input, so it yields only
			// what the bytes contain, and the place it names is never
			// the one it was read at.
			if len(in) != SealMarkerSize {
				t.Fatalf("read %d bytes, want %d", len(in), SealMarkerSize)
			}
			if got, want := binary.LittleEndian.Uint32(in[28:32]), wantSeededCRC(testIdentity, in[0:28]); got != want {
				t.Fatalf("a marker reported at the wrong place with checksum 0x%08x, want the seeded CRC32C of [0,28): 0x%08x", got, want)
			}
			if got := AppendSealMarker(nil, testIdentity, m); !slices.Equal(got, in) {
				t.Fatalf("a marker reported at the wrong place re-encodes as\n% x\nwant the input\n% x", got, in)
			}
			if m.Segment == core.SegmentID(segment) && m.Offset == offset {
				t.Fatalf("a marker reported at the wrong place names the segment %d and offset %d it was read at", segment, offset)
			}
		}

		damaged := errors.Is(err, ErrDamaged)
		wrongPlace := errors.Is(err, ErrWrongPlace)
		if damaged == wrongPlace {
			t.Fatalf("DecodeSealMarker = %v, want an error matching exactly one of ErrDamaged and ErrWrongPlace", err)
		}
		if errors.Is(err, ErrForeign) || errors.Is(err, ErrUnknownVersion) {
			t.Fatalf("DecodeSealMarker = %v, want no error matching ErrForeign or ErrUnknownVersion", err)
		}
	})
}

// FuzzRecord drives the record decoder with any bytes read at any segment
// and offset. Whatever arrives, it either decodes or matches exactly one
// outcome class, never a panic; a decode that fits the header but not the
// body verifies only the header; and whatever comes back is exactly what
// the bytes contain: a decoded header names the place it was read at, and
// a verified record re-encodes to exactly the bytes it was read from,
// padding included.
func FuzzRecord(f *testing.F) {
	var goldens []wantRecord
	// The 14 goldens at their own place, one at the wrong segment and one
	// at the wrong offset, then their foil bytes, each of which a real
	// journal could meet.
	for name, want := range recordGoldens(f) {
		goldens = append(goldens, want)

		b := readGolden(f, "record_"+name)
		f.Add(b, uint64(testSegment), want.h.Offset)
		f.Add(b, uint64(testSegment)+1, want.h.Offset)
		f.Add(b, uint64(testSegment), want.h.Offset+8)
	}

	// A record under another journal's identity: damaged under this one.
	f.Add(AppendRecord(nil, otherIdentity, recordGoldens(f)["release"].h, RecordBody{}), uint64(testSegment), recordGoldens(f)["release"].h.Offset)

	// Kinds this format version does not define: 0, the first reserved
	// value and the first extension's, rechecksummed so only the kind
	// rule can refuse them.
	for _, kind := range []byte{0, 15, 128} {
		b := slices.Clone(readGolden(f, "record_release"))
		b[4] = kind
		rechecksumRecord(b, testIdentity)
		f.Add(b, uint64(testSegment), recordGoldens(f)["release"].h.Offset)
	}

	// A non-zero epoch, a write with one modification-time byte flipped,
	// and a write whose padding is dirty: each reaches the rule its bytes
	// break, never a stale checksum.
	epochOne := slices.Clone(readGolden(f, "record_release"))
	binary.LittleEndian.PutUint64(epochOne[56:64], 1)
	rechecksumRecord(epochOne, testIdentity)
	f.Add(epochOne, uint64(testSegment), recordGoldens(f)["release"].h.Offset)

	mtimeFlipped := slices.Clone(readGolden(f, "record_write"))
	mtimeFlipped[96] ^= 0xff
	rechecksumRecordBody(mtimeFlipped, testIdentity, 21)
	f.Add(mtimeFlipped, uint64(testSegment), recordGoldens(f)["write"].h.Offset)

	dirtyPadding := slices.Clone(readGolden(f, "record_write"))
	for i := RecordHeaderSize + 21; i < len(dirtyPadding); i++ {
		dirtyPadding[i] = 0xff
	}
	f.Add(dirtyPadding, uint64(testSegment), recordGoldens(f)["write"].h.Offset)

	// A record whose stored own offset is not a multiple of 8, at the place
	// it names: it is refused as damage, never handed out as a header no
	// writer could have produced.
	misaligned := slices.Clone(readGolden(f, "record_release"))
	binary.LittleEndian.PutUint32(misaligned[88:92], 4)
	rechecksumRecord(misaligned, testIdentity)
	f.Add(misaligned, uint64(testSegment), int64(4))

	f.Add([]byte(nil), uint64(testSegment), testOffset)
	f.Add(make([]byte, RecordHeaderSize), uint64(testSegment), testOffset)

	f.Fuzz(func(t *testing.T, in []byte, segment uint64, offset int64) {
		if len(in) < RecordHeaderSize {
			return // the decode's length rule is covered by its tests
		}
		header := in[:RecordHeaderSize]

		h, err := DecodeRecordHeader(header, testIdentity, core.SegmentID(segment), offset)
		if err != nil {
			matched := 0
			for _, class := range []error{ErrDamaged, ErrWrongPlace, ErrUnknownKind, ErrNonZeroEpoch} {
				if errors.Is(err, class) {
					matched++
				}
			}
			if matched != 1 {
				t.Fatalf("DecodeRecordHeader = %v, want an error matching exactly one of the record classes", err)
			}
			if h != (RecordHeader{}) {
				t.Fatalf("DecodeRecordHeader = %v with the header %+v, want the zero header on refusal", err, h)
			}
			return
		}

		// A decoded header names the place it was read at and holds
		// readable extents.
		if h.Segment != core.SegmentID(segment) || h.Offset != offset {
			t.Fatalf("decoded %+v, want the segment %d and offset %d it was read at", h, segment, offset)
		}
		if h.Extent.Off < 0 || h.Extent.Len < 0 {
			t.Fatalf("decoded a negative extent %+v", h.Extent)
		}
		if h.Extent.Off+h.Extent.Len < 0 {
			t.Fatalf("decoded an overflowing extent %+v", h.Extent)
		}
		if h.Version.Epoch != 0 {
			t.Fatalf("decoded a non-zero epoch %d", h.Version.Epoch)
		}

		// The header names the body's size; if the input holds it, the
		// body verifies and re-encodes, to the input followed by its
		// own zero padding. The encoding of the body is the decoder's
		// inverse, so the record round trips.
		end := RecordHeaderSize + h.BodySize()
		if int64(len(in)) < end {
			return
		}
		body, err := VerifyRecordBody(in[RecordHeaderSize:end], testIdentity, h)
		if err != nil {
			matched := 0
			for _, class := range []error{ErrDamaged, ErrNonZeroEpoch} {
				if errors.Is(err, class) {
					matched++
				}
			}
			if matched != 1 {
				t.Fatalf("VerifyRecordBody = %v, want an error matching exactly one of ErrDamaged and ErrNonZeroEpoch", err)
			}
			if !recordBodiesEqual(body, RecordBody{}) {
				t.Fatalf("VerifyRecordBody = %v with body %+v, want the zero body on refusal", err, body)
			}
			return
		}
		// A verified payload is the input's own bytes, never a copy
		// the decoder made: re-encoding it reproduces the input.
		if h.Kind == KindWrite || h.Kind == KindFill {
			if !payloadWithin(body.Payload, in[RecordHeaderSize:end], h) {
				t.Fatalf("the verified payload is not the input's own bytes")
			}
		}

		encoded := AppendRecord(nil, testIdentity, h, body)
		want := append(slices.Clone(in[:end]), make([]byte, (8-end%8)%8)...)
		if !slices.Equal(encoded, want) {
			t.Fatalf("AppendRecord =\n% x\nwant the input's header and body with zero padding\n% x", encoded, want)
		}
	})
}

// payloadWithin reports whether the verified payload is a slice of the input
// the decoder was handed — the very bytes it checked, never a copy. The
// body's layout puts a write's payload after its 8-byte modification time
// and a fill's at the body's first byte, so the payload's first byte sits
// at that offset of the input, and its last within the input's end.
func payloadWithin(payload, in []byte, h RecordHeader) bool {
	start := 8
	if h.Kind == KindFill {
		start = 0
	}
	if len(in) < start+len(payload) {
		return false
	}
	if len(payload) == 0 {
		return true
	}
	return &payload[0] == &in[start]
}
