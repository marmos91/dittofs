package raw

import "testing"

// Offsets and sizes below are the seal marker table's, written out here and
// never taken from the package's constants.

func testSealMarker() []byte {
	return SealMarker{Identity: testIdentity, Segment: testSegment, Offset: testOffset}.Bytes()
}

// TestSealMarkerLayout pins every field of a seal marker at its offset and
// size.
func TestSealMarkerLayout(t *testing.T) {
	b := testSealMarker()

	if len(b) != 32 {
		t.Fatalf("a seal marker is %d bytes, want 32", len(b))
	}

	if got := string(b[0:4]); got != "DJS1" {
		t.Errorf("magic at [0,4) is %q, want %q", got, "DJS1")
	}

	if !zero(b[4:8]) {
		t.Errorf("reserved [4,8) is % x, want zeros", b[4:8])
	}

	if got := le64(b[8:16]); got != testSegment {
		t.Errorf("segment id at [8,16) is %#x, want %#x", got, testSegment)
	}

	if got := le64(b[16:24]); got != testOffset {
		t.Errorf("own offset at [16,24) is %#x, want %#x", got, testOffset)
	}

	if !zero(b[24:28]) {
		t.Errorf("reserved [24,28) is % x, want zeros", b[24:28])
	}
}

// TestSealMarkerChecksumIsSeededOverItsFirst28Bytes pins that a seal marker's
// checksum is the seeded CRC32C of its first 28 bytes.
func TestSealMarkerChecksumIsSeededOverItsFirst28Bytes(t *testing.T) {
	b := testSealMarker()

	if len(b) != 32 {
		t.Fatalf("a seal marker is %d bytes, want 32", len(b))
	}

	if got, want := le32(b[28:32]), wantSeededCRC(testIdentity, b[0:28]); got != want {
		t.Errorf("checksum at [28,32) is 0x%08x, want the CRC32C of the identity then [0,28): 0x%08x", got, want)
	}
}
