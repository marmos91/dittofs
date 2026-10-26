package raw

import (
	"encoding/binary"
	"testing"
)

// Offsets and sizes below are the segment header table's, written out here
// and never taken from the package's constants.

func testSegmentHeader() []byte {
	return SegmentHeader{Identity: testIdentity, ID: testSegment, Predecessor: testPredecessor}.Bytes()
}

// rechecksum stores the CRC32C of [0,44) at [44,48), so an edited header
// fails a parse only for the edit, not for its stale checksum.
func rechecksum(b []byte) {
	binary.LittleEndian.PutUint32(b[44:48], wantCRC(b[0:44]))
}

func TestSegmentHeaderLayout(t *testing.T) {
	b := testSegmentHeader()

	if len(b) != 48 {
		t.Fatalf("a segment header is %d bytes, want 48", len(b))
	}

	if got := string(b[0:4]); got != "DJH1" {
		t.Errorf("magic at [0,4) is %q, want %q", got, "DJH1")
	}

	if got := le16(b[4:6]); got != 1 {
		t.Errorf("format version at [4,6) is %d, want 1", got)
	}

	if !zero(b[6:8]) {
		t.Errorf("reserved [6,8) is % x, want zeros", b[6:8])
	}

	if got := [16]byte(b[8:24]); got != testIdentity {
		t.Errorf("identity at [8,24) is % x, want % x", got, testIdentity)
	}

	if got := le64(b[24:32]); got != testSegment {
		t.Errorf("segment id at [24,32) is %#x, want %#x", got, testSegment)
	}

	if got := le64(b[32:40]); got != testPredecessor {
		t.Errorf("predecessor at [32,40) is %#x, want %#x", got, testPredecessor)
	}

	if got := le32(b[40:44]); got != 0 {
		t.Errorf("stream number at [40,44) is 0x%08x, want 0", got)
	}
}

func TestSegmentHeaderChecksumIsUnseededOverItsFirst44Bytes(t *testing.T) {
	b := testSegmentHeader()

	if len(b) != 48 {
		t.Fatalf("a segment header is %d bytes, want 48", len(b))
	}

	if got, want := le32(b[44:48]), wantCRC(b[0:44]); got != want {
		t.Errorf("checksum at [44,48) is 0x%08x, want the CRC32C of [0,44): 0x%08x", got, want)
	}
}

func TestParseSegmentHeaderReadsBackWhatWasBuilt(t *testing.T) {
	h, err := ParseSegmentHeader(testSegmentHeader())
	if err != nil {
		t.Errorf("parsing a built header: %v", err)
	}

	if h.Identity != testIdentity {
		t.Errorf("parsed identity is % x, want % x", h.Identity, testIdentity)
	}

	if h.ID != testSegment {
		t.Errorf("parsed segment id is %#x, want %#x", h.ID, testSegment)
	}

	if h.Predecessor != testPredecessor {
		t.Errorf("parsed predecessor is %#x, want %#x", h.Predecessor, testPredecessor)
	}
}

// A header the journal wrote carries the number of the stream that opened it,
// and the parser must read it all the same.
func TestParseSegmentHeaderReadsAHeaderWithAStreamNumber(t *testing.T) {
	b := testSegmentHeader()
	if len(b) != 48 {
		t.Fatalf("a segment header is %d bytes, want 48", len(b))
	}

	binary.LittleEndian.PutUint32(b[40:44], testStream)
	rechecksum(b)

	h, err := ParseSegmentHeader(b)
	if err != nil {
		t.Errorf("parsing a header with stream number 0x%08x: %v", testStream, err)
	}

	if h.ID != testSegment {
		t.Errorf("parsed segment id is %#x, want %#x", h.ID, testSegment)
	}
}

func TestParseSegmentHeaderRefusesAWrongMagic(t *testing.T) {
	b := testSegmentHeader()
	if len(b) != 48 {
		t.Fatalf("a segment header is %d bytes, want 48", len(b))
	}

	copy(b[0:4], "DJS1")
	rechecksum(b)

	if _, err := ParseSegmentHeader(b); err == nil {
		t.Errorf("a header with magic %q and a valid checksum parsed", b[0:4])
	}
}

func TestParseSegmentHeaderRefusesAWrongLength(t *testing.T) {
	b := testSegmentHeader()

	if _, err := ParseSegmentHeader(b[:47]); err == nil {
		t.Errorf("a 47-byte header parsed")
	}

	if _, err := ParseSegmentHeader(append(b, 0)); err == nil {
		t.Errorf("a 49-byte header parsed")
	}
}

// A header that does not verify is damage, not a foreign segment, so a parse
// that skipped the checksum would misreport it.
func TestParseSegmentHeaderRefusesAFlippedBit(t *testing.T) {
	for _, f := range []struct {
		name string
		at   int
	}{
		{"magic", 0},
		{"format version", 4},
		{"reserved", 6},
		{"identity", 8},
		{"segment id", 24},
		{"predecessor", 32},
		{"stream number", 40},
		{"checksum", 44},
	} {
		b := testSegmentHeader()
		if len(b) != 48 {
			t.Fatalf("a segment header is %d bytes, want 48", len(b))
		}
		b[f.at] ^= 0x01

		if _, err := ParseSegmentHeader(b); err == nil {
			t.Errorf("a header with a bit of its %s flipped at byte %d parsed", f.name, f.at)
		}
	}
}
