package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// TestSegmentHeaderGolden pins the encoder's bytes against a hand-written
// golden file and against raw's account of the same fields, and pins the
// decoder against them. The stream number is checked against the golden file
// alone: raw hard-codes stream 0.
func TestSegmentHeaderGolden(t *testing.T) {
	for _, c := range []struct {
		name   string
		golden string
		h      SegmentHeader
	}{
		{"with a predecessor", "segheader_pred", testSegmentHeader},
		{"with no predecessor", "segheader_nopred", SegmentHeader{Identity: testIdentity, ID: testSegment, Predecessor: 0, Stream: testStream}},
	} {
		golden := readGolden(t, c.golden)

		encoded := AppendSegmentHeader(nil, c.h)
		if !bytes.Equal(encoded, golden) {
			t.Errorf("%s: AppendSegmentHeader =\n% x\nwant the golden file\n% x", c.name, encoded, golden)
		}

		// raw hard-codes version 1 and stream 0, so the golden with its
		// stream zeroed and rechecksummed is what raw writes: the stream,
		// which raw cannot set, is checked against the golden file alone.
		zeroed := slices.Clone(golden)
		clear(zeroed[40:44])
		rechecksumHeader(zeroed)
		other := raw.SegmentHeader{Identity: testIdentity, ID: uint64(c.h.ID), Predecessor: uint64(c.h.Predecessor)}.Bytes()
		if !bytes.Equal(zeroed, other) {
			t.Errorf("%s: golden with a zero stream =\n% x\nwant raw's bytes\n% x", c.name, zeroed, other)
		}

		h, err := DecodeSegmentHeader(golden, testIdentity)
		if err != nil {
			t.Fatalf("%s: DecodeSegmentHeader(golden) = %v, want no error", c.name, err)
		}
		if h != c.h {
			t.Errorf("%s: decoded %+v, want %+v", c.name, h, c.h)
		}
	}
}

// TestAppendSegmentHeaderAppendsToDst pins that the encoder appends to dst
// rather than overwriting it, and that the CRC covers only the 48 bytes it
// writes.
func TestAppendSegmentHeaderAppendsToDst(t *testing.T) {
	prefix := []byte{0xee, 0xbe, 0xef}
	golden := readGolden(t, "segheader_pred")

	if got := AppendSegmentHeader(slices.Clone(prefix), testSegmentHeader); !bytes.Equal(got, append(slices.Clone(prefix), golden...)) {
		t.Errorf("AppendSegmentHeader with a prefix =\n% x\nwant the prefix then the golden file\n% x", got, append(prefix, golden...))
	}
}

// TestDecodeSegmentHeaderTellsAForeignSegment pins that a header that
// verifies under another identity is that journal's, not damage: the
// unseeded checksum lets it verify, the identity tells it apart, and whatever
// else the header holds is not this journal's to judge.
func TestDecodeSegmentHeaderTellsAForeignSegment(t *testing.T) {
	other := testSegmentHeader
	other.Identity = otherIdentity

	b := AppendSegmentHeader(nil, other)

	h, err := DecodeSegmentHeader(b, testIdentity)
	if !errors.Is(err, ErrForeign) || errors.Is(err, ErrDamaged) || errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("DecodeSegmentHeader(a foreign header) = %v, want an error matching ErrForeign and not ErrDamaged or ErrUnknownVersion", err)
	}
	if h != other {
		t.Errorf("decoded %+v, want %+v", h, other)
	}
	if _, err := DecodeSegmentHeader(b, otherIdentity); err != nil {
		t.Errorf("DecodeSegmentHeader under the identity it names = %v, want no error", err)
	}

	// A foreign header with non-zero reserved bytes is still foreign: the
	// reserved bytes are judged only after the identity says whose header
	// this is.
	copy(b[6:8], []byte{0xff, 0xff})
	rechecksumHeader(b)
	if _, err := DecodeSegmentHeader(b, testIdentity); !errors.Is(err, ErrForeign) {
		t.Errorf("DecodeSegmentHeader(a foreign header with non-zero reserved bytes) = %v, want an error matching ErrForeign", err)
	}

	// So is a foreign header holding an id of zero: the id is judged after
	// the identity too, and the header is returned with the id its bytes
	// hold, so recovery reports whose segment it is whatever the header
	// says of itself.
	b = AppendSegmentHeader(nil, other)
	clear(b[24:32])
	rechecksumHeader(b)
	h, err = DecodeSegmentHeader(b, testIdentity)
	if !errors.Is(err, ErrForeign) {
		t.Fatalf("DecodeSegmentHeader(a foreign header with a zero id) = %v, want an error matching ErrForeign", err)
	}
	if h.ID != 0 || h.Identity != otherIdentity {
		t.Errorf("decoded id %d under identity % x, want id 0 under the other identity % x", h.ID, h.Identity, otherIdentity)
	}
}

// TestDecodeSegmentHeaderRefusesAnUnknownVersion pins that a header of
// version 1's size whose checksum verifies but names a version this binary
// does not read is refused; and a header of another length is judged for its
// version first, never by version 1's rules — a later version may change the
// size, so its length is never judged by version 1's.
func TestDecodeSegmentHeaderRefusesAnUnknownVersion(t *testing.T) {
	for _, version := range []uint16{0, 2} {
		b := slices.Clone(readGolden(t, "segheader_pred"))
		binary.LittleEndian.PutUint16(b[4:6], version)
		rechecksumHeader(b)
		if _, err := DecodeSegmentHeader(b, testIdentity); !errors.Is(err, ErrUnknownVersion) {
			t.Errorf("DecodeSegmentHeader(version %d) = %v, want an error matching ErrUnknownVersion", version, err)
		}
	}

	long := append(slices.Clone(readGolden(t, "segheader_pred")), 0)
	binary.LittleEndian.PutUint16(long[4:6], 2)
	if _, err := DecodeSegmentHeader(long, testIdentity); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("DecodeSegmentHeader(a 49-byte version-2 header) = %v, want an error matching ErrUnknownVersion", err)
	}

	stale := slices.Clone(readGolden(t, "segheader_pred"))
	binary.LittleEndian.PutUint16(stale[4:6], 2)
	stale[47] ^= 0xff // a later version's checksum is still wrong
	if _, err := DecodeSegmentHeader(stale, testIdentity); !errors.Is(err, ErrDamaged) || errors.Is(err, ErrUnknownVersion) {
		t.Errorf("DecodeSegmentHeader(a version-2 header with a wrong checksum) = %v, want an error matching ErrDamaged and not ErrUnknownVersion", err)
	}
}

// TestDecodeSegmentHeaderRefusesAFlippedBit flips every bit of every byte of
// the golden file and checks the one outcome there is: every flip breaks the
// checksum, so every flip is damage. That pins the checksum before the
// identity, so a damaged header is never taken for another journal's, and
// before the version field, so a flipped version bit reports the segment as
// damaged with its records attached rather than failing the open.
func TestDecodeSegmentHeaderRefusesAFlippedBit(t *testing.T) {
	golden := readGolden(t, "segheader_pred")

	for off := range SegmentHeaderSize {
		for bit := range 8 {
			b := slices.Clone(golden)
			b[off] ^= 1 << bit

			_, err := DecodeSegmentHeader(b, testIdentity)
			if !errors.Is(err, ErrDamaged) || errors.Is(err, ErrUnknownVersion) || errors.Is(err, ErrForeign) {
				t.Errorf("byte %d bit %d flipped: DecodeSegmentHeader = %v, want an error matching ErrDamaged and no other class", off, bit, err)
			}
		}
	}
}

// TestDecodeSegmentHeaderRefusesAWrongLength pins that a version-1 header of
// any other length fails the decode like a bad checksum, whatever the length.
func TestDecodeSegmentHeaderRefusesAWrongLength(t *testing.T) {
	golden := readGolden(t, "segheader_pred")

	// A truncated input is built by copy into a slice of exactly its
	// length, never a re-slice of the golden file: a re-slice keeps the
	// 48-byte capacity, so a decode that read past len would read the
	// golden's bytes and pass rather than panic.
	trunc := func(n int) []byte {
		b := make([]byte, n)
		copy(b, golden)
		return b
	}

	for name, b := range map[string][]byte{
		"empty":    {},
		"5 bytes":  trunc(5),
		"47 bytes": trunc(47),
		"49 bytes": append(slices.Clone(golden), 0),
	} {
		_, err := DecodeSegmentHeader(b, testIdentity)
		if !errors.Is(err, ErrDamaged) || errors.Is(err, ErrUnknownVersion) {
			t.Errorf("DecodeSegmentHeader(%s) = %v, want an error matching ErrDamaged and not ErrUnknownVersion", name, err)
		}
	}
}

// TestDecodeSegmentHeaderRefusesAWrongMagic pins that the magic is judged: a
// header of another format is never read as this one however it verifies,
// and neither is a page of zeros.
func TestDecodeSegmentHeaderRefusesAWrongMagic(t *testing.T) {
	b := readGolden(t, "segheader_pred")
	copy(b[0:4], "DJH2")
	rechecksumHeader(b)

	if _, err := DecodeSegmentHeader(b, testIdentity); !errors.Is(err, ErrDamaged) {
		t.Errorf("DecodeSegmentHeader(DJH2) = %v, want an error matching ErrDamaged", err)
	}
	if _, err := DecodeSegmentHeader(make([]byte, SegmentHeaderSize), testIdentity); !errors.Is(err, ErrDamaged) {
		t.Errorf("DecodeSegmentHeader(48 zero bytes) = %v, want an error matching ErrDamaged", err)
	}
}

// TestDecodeSegmentHeaderRefusesNonZeroReservedBytes pins the stricter
// reading of the reserved bytes: written as zero, refused when not.
func TestDecodeSegmentHeaderRefusesNonZeroReservedBytes(t *testing.T) {
	b := readGolden(t, "segheader_pred")
	copy(b[6:8], []byte{0x01, 0x00})
	rechecksumHeader(b)

	if _, err := DecodeSegmentHeader(b, testIdentity); !errors.Is(err, ErrDamaged) || errors.Is(err, ErrForeign) || errors.Is(err, ErrUnknownVersion) {
		t.Errorf("DecodeSegmentHeader(reserved [6,8) non-zero) = %v, want an error matching ErrDamaged and not ErrForeign or ErrUnknownVersion", err)
	}
}

// TestDecodeSegmentHeaderRefusesAZeroSegmentId pins that a header naming no
// segment — a zero id, the id the format reserves for "none" — is damage: a
// segment's name and header must name a segment.
func TestDecodeSegmentHeaderRefusesAZeroSegmentId(t *testing.T) {
	b := readGolden(t, "segheader_pred")
	clear(b[24:32])
	rechecksumHeader(b)

	if _, err := DecodeSegmentHeader(b, testIdentity); !errors.Is(err, ErrDamaged) || errors.Is(err, ErrForeign) || errors.Is(err, ErrUnknownVersion) {
		t.Errorf("DecodeSegmentHeader(segment id 0) = %v, want an error matching ErrDamaged and not ErrForeign or ErrUnknownVersion", err)
	}
}
