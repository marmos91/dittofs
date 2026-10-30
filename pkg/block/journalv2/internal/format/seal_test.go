package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// TestSealMarkerGolden pins the encoder's bytes against a hand-written
// golden file and against raw's account of the same fields — raw fixes no
// field of a seal marker, so all three must agree byte for byte — and pins
// the decoder against them.
func TestSealMarkerGolden(t *testing.T) {
	golden := readGolden(t, "seal")

	encoded := AppendSealMarker(nil, testIdentity, testSealMarker)
	if !bytes.Equal(encoded, golden) {
		t.Errorf("AppendSealMarker =\n% x\nwant the golden file\n% x", encoded, golden)
	}

	other := raw.SealMarker{Identity: testIdentity, Segment: uint64(testSegment), Offset: uint64(testOffset)}.Bytes()
	if !bytes.Equal(golden, other) {
		t.Errorf("golden file =\n% x\nwant raw's bytes\n% x", golden, other)
	}

	m, err := DecodeSealMarker(golden, testIdentity, testSegment, testOffset)
	if err != nil {
		t.Fatalf("DecodeSealMarker(golden) = %v, want no error", err)
	}
	if m != testSealMarker {
		t.Errorf("decoded %+v, want %+v", m, testSealMarker)
	}
}

// TestAppendSealMarkerAppendsToDst pins that the encoder appends to dst
// rather than overwriting it, and that the seeded CRC covers only the 32
// bytes it writes, never a caller's prefix.
func TestAppendSealMarkerAppendsToDst(t *testing.T) {
	prefix := []byte{0xee, 0xbe, 0xef}
	golden := readGolden(t, "seal")

	if got := AppendSealMarker(slices.Clone(prefix), testIdentity, testSealMarker); !bytes.Equal(got, append(slices.Clone(prefix), golden...)) {
		t.Errorf("AppendSealMarker with a prefix =\n% x\nwant the prefix then the golden file\n% x", got, append(prefix, golden...))
	}
}

// TestDecodeSealMarkerVerifiesOnlyWhereItWasWritten pins that a marker whose
// checksum verifies under the reader's identity still fails when it was
// written somewhere else — at another offset, or in another segment — and is
// then returned filled with the segment and offset its bytes name, so a copy
// inside a payload never ends a scan; and that a marker seeded with another
// journal's identity fails the checksum under this one's, reported as damage
// never as wrong place — which is what the seed is for: unlike a segment
// header's, no marker of another journal can be told apart as foreign.
func TestDecodeSealMarkerVerifiesOnlyWhereItWasWritten(t *testing.T) {
	golden := readGolden(t, "seal")

	for _, c := range []struct {
		name    string
		segment core.SegmentID
		offset  int64
	}{
		{"read at another offset", testSegment, testOffset + 32},
		{"read in another segment", testSegment + 1, testOffset},
	} {
		m, err := DecodeSealMarker(golden, testIdentity, c.segment, c.offset)
		if !errors.Is(err, ErrWrongPlace) || errors.Is(err, ErrDamaged) {
			t.Fatalf("%s: DecodeSealMarker = %v, want an error matching ErrWrongPlace and not ErrDamaged", c.name, err)
		}
		if m != testSealMarker {
			t.Errorf("%s: decoded %+v, want %+v, the marker its bytes hold", c.name, m, testSealMarker)
		}
	}

	for _, c := range []struct {
		name     string
		b        []byte
		identity core.JournalID
	}{
		{"read under another identity", golden, otherIdentity},
		{"a marker written under another identity", AppendSealMarker(nil, otherIdentity, testSealMarker), testIdentity},
	} {
		m, err := DecodeSealMarker(c.b, c.identity, testSegment, testOffset)
		if !errors.Is(err, ErrDamaged) || errors.Is(err, ErrWrongPlace) {
			t.Fatalf("%s: DecodeSealMarker = %v, want an error matching ErrDamaged and not ErrWrongPlace", c.name, err)
		}
		if m != (SealMarker{}) {
			t.Errorf("%s: decoded %+v on damage, want the zero marker", c.name, m)
		}
	}
}

// TestDecodeSealMarkerRefusesDamage pins that bytes that are not this place's
// seal marker — a wrong length, a slot of zeros, another structure's magic,
// reserved bytes the table writes as zero, or any single bit flipped — are
// refused as damage, never as a marker written somewhere else: a flip in the
// marker's own segment or offset breaks the checksum first.
func TestDecodeSealMarkerRefusesDamage(t *testing.T) {
	golden := readGolden(t, "seal")

	// DJR1 is a record header's magic: a record header is not a seal
	// marker, whatever its checksum verifies as.
	recordMagic := slices.Clone(golden)
	copy(recordMagic[0:4], "DJR1")
	rechecksumSeal(recordMagic, testIdentity)

	// Every reserved byte is its own case, rechecksummed, so only the
	// reserved rule can refuse each — never a stale checksum — and a
	// check that skips any of the eight cannot pass unnoticed.
	for _, i := range []int{4, 5, 6, 7, 24, 25, 26, 27} {
		reserved := slices.Clone(golden)
		reserved[i] = 0xff
		rechecksumSeal(reserved, testIdentity)

		m, err := DecodeSealMarker(reserved, testIdentity, testSegment, testOffset)
		if !errors.Is(err, ErrDamaged) || errors.Is(err, ErrWrongPlace) {
			t.Errorf("non-zero reserved byte at %d: DecodeSealMarker = %v, want an error matching ErrDamaged and not ErrWrongPlace", i, err)
		}
		if m != (SealMarker{}) {
			t.Errorf("non-zero reserved byte at %d: decoded %+v on damage, want the zero marker", i, m)
		}
	}

	cases := []struct {
		name string
		b    []byte
	}{
		{"31 bytes", golden[:31]},
		{"33 bytes", append(slices.Clone(golden), 0x00)},
		{"empty", nil},
		{"a slot of zeros", make([]byte, SealMarkerSize)},
		{"a record header's magic", recordMagic},
	}
	for _, c := range cases {
		m, err := DecodeSealMarker(c.b, testIdentity, testSegment, testOffset)
		if !errors.Is(err, ErrDamaged) || errors.Is(err, ErrWrongPlace) {
			t.Errorf("%s: DecodeSealMarker = %v, want an error matching ErrDamaged and not ErrWrongPlace", c.name, err)
		}
		if m != (SealMarker{}) {
			t.Errorf("%s: decoded %+v on damage, want the zero marker", c.name, m)
		}
	}

	// Every single-bit flip of all 256 bits is damage: a flip in the
	// checksum fails the comparison, and one in the covered bytes fails
	// the seeded checksum before the segment and offset it names are ever
	// compared, so a damaged marker never reads as one written elsewhere.
	for i := range len(golden) {
		for bit := range 8 {
			b := slices.Clone(golden)
			b[i] ^= 1 << bit
			if _, err := DecodeSealMarker(b, testIdentity, testSegment, testOffset); !errors.Is(err, ErrDamaged) {
				t.Errorf("golden with byte %d bit %d flipped: DecodeSealMarker = %v, want an error matching ErrDamaged", i, bit, err)
			}
		}
	}
}

// TestDecodeSealMarkerRefusesAnOwnOffsetAboveMaxInt64 pins that a marker
// whose own offset, as stored, does not fit an int64 is refused as damage —
// at any read offset, including the negative one its bytes decode as — and
// never accepted as a marker of this place. The offset column of the table
// is 64 bits wide while core's offsets are int64, so a marker that did hold
// such an offset could not name a place this journal reads at.
func TestDecodeSealMarkerRefusesAnOwnOffsetAboveMaxInt64(t *testing.T) {
	// 0xffffffffffffffff at [16,24) is the least stored offset above
	// MaxInt64. The encoder refuses to write one, so the field is set by
	// hand and rechecksummed.
	b := slices.Clone(readGolden(t, "seal"))
	binary.LittleEndian.PutUint64(b[16:24], 0xffffffffffffffff)
	rechecksumSeal(b, testIdentity)

	for _, c := range []struct {
		name   string
		offset int64
	}{
		{"read at a real offset", testOffset},
		{"read at the negative offset it decodes as", -1},
	} {
		m, err := DecodeSealMarker(b, testIdentity, testSegment, c.offset)
		if !errors.Is(err, ErrDamaged) || errors.Is(err, ErrWrongPlace) {
			t.Errorf("%s: DecodeSealMarker = %v, want an error matching ErrDamaged and not ErrWrongPlace", c.name, err)
		}
		if m != (SealMarker{}) {
			t.Errorf("%s: decoded %+v on damage, want the zero marker", c.name, m)
		}
	}
}

// TestAppendSealMarkerPanicsOnANegativeOffset pins that an encoder handed an
// offset no read could ever pass crashes rather than writing a marker its
// own decoder refuses: the bound is a programming invariant of the callers,
// not something the bytes can carry, so it is refused at the call, not
// encoded onto disk.
func TestAppendSealMarkerPanicsOnANegativeOffset(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("AppendSealMarker with a negative offset returned; want a panic")
		}
	}()
	AppendSealMarker(nil, testIdentity, SealMarker{Segment: testSegment, Offset: -1})
}
