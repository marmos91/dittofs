package raw

import "testing"

// Offsets and sizes below are the trailer table's, written out here and never
// taken from the package's constants.

func testTrailer() []byte {
	return Trailer(testLayoutVersion, testFooterOffset, testEntries...)
}

// TestTrailerLayout pins every field of a trailer at its offset and size.
func TestTrailerLayout(t *testing.T) {
	b := testTrailer()

	if len(b) != 32 {
		t.Fatalf("a trailer is %d bytes, want 32", len(b))
	}

	if got := string(b[0:4]); got != "DJT1" {
		t.Errorf("magic at [0,4) is %q, want %q", got, "DJT1")
	}

	if got := le16(b[4:6]); got != testLayoutVersion {
		t.Errorf("catalog layout version at [4,6) is %#x, want %#x", got, testLayoutVersion)
	}

	if !zero(b[6:8]) {
		t.Errorf("flags at [6,8) are % x, want zeros", b[6:8])
	}

	if got := le64(b[8:16]); got != testFooterOffset {
		t.Errorf("footerOffset at [8,16) is %#x, want %#x", got, testFooterOffset)
	}

	if got := le32(b[16:20]); got != 2 {
		t.Errorf("entryCount at [16,20) is %d, want 2 for 144 entry bytes", got)
	}

	if !zero(b[24:28]) {
		t.Errorf("reserved [24,28) is % x, want zeros", b[24:28])
	}
}

// TestTrailerFooterChecksumIsUnseededOverTheEntries pins that a trailer's
// footer checksum is the plain CRC32C of the entries it describes.
func TestTrailerFooterChecksumIsUnseededOverTheEntries(t *testing.T) {
	b := testTrailer()

	if len(b) != 32 {
		t.Fatalf("a trailer is %d bytes, want 32", len(b))
	}

	footer := append(testEntries[0][:], testEntries[1][:]...)
	if got, want := le32(b[20:24]), wantCRC(footer); got != want {
		t.Errorf("footer checksum at [20,24) is 0x%08x, want the CRC32C of the 144 entry bytes: 0x%08x", got, want)
	}
}

// TestTrailerChecksumIsUnseededOverItsFirst28Bytes pins that a trailer's
// checksum is the plain CRC32C of its first 28 bytes.
func TestTrailerChecksumIsUnseededOverItsFirst28Bytes(t *testing.T) {
	b := testTrailer()

	if len(b) != 32 {
		t.Fatalf("a trailer is %d bytes, want 32", len(b))
	}

	if got, want := le32(b[28:32]), wantCRC(b[0:28]); got != want {
		t.Errorf("checksum at [28,32) is 0x%08x, want the CRC32C of [0,28): 0x%08x", got, want)
	}
}
