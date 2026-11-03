package raw

import (
	"encoding/binary"
	"testing"
)

// Offsets and sizes below are the format file table's, written out here and
// never taken from the package's constants.

func testFormat() []byte {
	return Format{Version: testFormatVersion, Identity: testIdentity, Installation: testInstallation}.Bytes()
}

// rechecksumFormat stores the CRC32C of [0,52) at [52,56), so an edited file
// fails a parse only for the edit, not for its stale checksum.
func rechecksumFormat(b []byte) {
	binary.LittleEndian.PutUint32(b[52:56], wantCRC(b[0:52]))
}

// TestFormatLayout pins every field of the format file at its offset and size.
func TestFormatLayout(t *testing.T) {
	b := testFormat()

	if len(b) != 56 {
		t.Fatalf("a format file is %d bytes, want 56", len(b))
	}

	if got := string(b[0:4]); got != "DJF1" {
		t.Errorf("magic at [0,4) is %q, want %q", got, "DJF1")
	}

	if got := le16(b[4:6]); got != testFormatVersion {
		t.Errorf("format version at [4,6) is %#x, want %#x", got, testFormatVersion)
	}

	if !zero(b[6:8]) {
		t.Errorf("reserved [6,8) is % x, want zeros", b[6:8])
	}

	if got := [16]byte(b[8:24]); got != testIdentity {
		t.Errorf("identity at [8,24) is % x, want % x", got, testIdentity)
	}

	if got := [16]byte(b[24:40]); got != testInstallation {
		t.Errorf("installation ID at [24,40) is % x, want % x", got, testInstallation)
	}

	if got := le64(b[40:48]); got != 0 {
		t.Errorf("generation at [40,48) is %#x, want 0", got)
	}

	if !zero(b[48:52]) {
		t.Errorf("reserved [48,52) is % x, want zeros", b[48:52])
	}
}

// TestFormatChecksumIsUnseededOverItsFirst52Bytes pins that the format file's
// checksum is the plain CRC32C of its first 52 bytes.
func TestFormatChecksumIsUnseededOverItsFirst52Bytes(t *testing.T) {
	b := testFormat()

	if len(b) != 56 {
		t.Fatalf("a format file is %d bytes, want 56", len(b))
	}

	if got, want := le32(b[52:56]), wantCRC(b[0:52]); got != want {
		t.Errorf("checksum at [52,56) is 0x%08x, want the CRC32C of [0,52): 0x%08x", got, want)
	}
}

// The built file's version is not 1, so a parser that required 1 fails here.
func TestParseFormatReadsBackWhatWasBuilt(t *testing.T) {
	f, err := ParseFormat(testFormat())
	if err != nil {
		t.Errorf("parsing a built format file: %v", err)
	}

	if f.Version != testFormatVersion {
		t.Errorf("parsed version is %#x, want %#x", f.Version, testFormatVersion)
	}

	if f.Identity != testIdentity {
		t.Errorf("parsed identity is % x, want % x", f.Identity, testIdentity)
	}

	if f.Installation != testInstallation {
		t.Errorf("parsed installation ID is % x, want % x", f.Installation, testInstallation)
	}
}

// TestParseFormatRefusesAWrongMagic pins that a format file with another magic
// does not parse.
func TestParseFormatRefusesAWrongMagic(t *testing.T) {
	b := testFormat()
	if len(b) != 56 {
		t.Fatalf("a format file is %d bytes, want 56", len(b))
	}
	copy(b[0:4], "DJH1")
	rechecksumFormat(b)

	if _, err := ParseFormat(b); err == nil {
		t.Errorf("a format file with magic %q and a valid checksum parsed", b[0:4])
	}
}

// TestParseFormatRefusesAWrongLength pins that a format file of any length but
// 56 bytes does not parse.
func TestParseFormatRefusesAWrongLength(t *testing.T) {
	b := testFormat()

	if _, err := ParseFormat(b[:55]); err == nil {
		t.Errorf("a 55-byte format file parsed")
	}

	if _, err := ParseFormat(append(b, 0)); err == nil {
		t.Errorf("a 57-byte format file parsed")
	}
}

// TestParseFormatRefusesAFlippedBit pins that a format file with one bit
// flipped in any field does not parse.
func TestParseFormatRefusesAFlippedBit(t *testing.T) {
	for _, f := range []struct {
		name string
		at   int
	}{
		{"magic", 0},
		{"format version", 4},
		{"reserved [6,8)", 6},
		{"identity", 8},
		{"installation ID", 24},
		{"generation", 40},
		{"reserved [48,52)", 48},
		{"checksum", 52},
	} {
		b := testFormat()
		if len(b) != 56 {
			t.Fatalf("a format file is %d bytes, want 56", len(b))
		}
		b[f.at] ^= 0x01

		if _, err := ParseFormat(b); err == nil {
			t.Errorf("a format file with a bit of its %s flipped at byte %d parsed", f.name, f.at)
		}
	}
}
