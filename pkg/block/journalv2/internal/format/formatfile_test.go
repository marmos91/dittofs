package format

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// TestFormatFileGolden pins the encoder's bytes against a hand-written golden
// file and against raw's account of the same fields, and pins the decoder
// against them.
func TestFormatFileGolden(t *testing.T) {
	golden := readGolden(t, "format_v1")

	encoded := AppendFormatFile(nil, testFormatFile)
	if !bytes.Equal(encoded, golden) {
		t.Errorf("AppendFormatFile =\n% x\nwant the golden file\n% x", encoded, golden)
	}

	other := raw.Format{Version: 1, Identity: testIdentity, Installation: testInstallation}.Bytes()
	if !bytes.Equal(golden, other) {
		t.Errorf("golden =\n% x\nwant raw's bytes\n% x", golden, other)
	}

	f, err := DecodeFormatFile(golden)
	if err != nil {
		t.Fatalf("DecodeFormatFile(golden) = %v, want no error", err)
	}
	if f.Identity != testIdentity {
		t.Errorf("identity = % x, want % x", f.Identity, testIdentity)
	}
	if f.Installation != testInstallation {
		t.Errorf("installation = % x, want % x", f.Installation, testInstallation)
	}
}

// TestAppendFormatFileAppendsToDst pins that the encoder appends to dst rather
// than overwriting it, and that the CRC covers only the 56 bytes it writes.
func TestAppendFormatFileAppendsToDst(t *testing.T) {
	prefix := []byte{0xee, 0xbe, 0xef}
	golden := readGolden(t, "format_v1")

	if got := AppendFormatFile(slices.Clone(prefix), testFormatFile); !bytes.Equal(got, append(slices.Clone(prefix), golden...)) {
		t.Errorf("AppendFormatFile with a prefix =\n% x\nwant the prefix then the golden file\n% x", got, append(prefix, golden...))
	}
}

// TestDecodeFormatFileRefusesAWrongLength pins that a version-1 file of any
// other length fails the decode like a bad checksum, whatever the length.
func TestDecodeFormatFileRefusesAWrongLength(t *testing.T) {
	golden := readGolden(t, "format_v1")

	// A truncated input is built by copy into a slice of exactly its
	// length, never a re-slice of the golden file: a re-slice keeps the
	// 56-byte capacity, so a decode that read past len would read the
	// golden's bytes and pass rather than panic.
	trunc := func(n int) []byte {
		b := make([]byte, n)
		copy(b, golden)
		return b
	}

	for name, b := range map[string][]byte{
		"empty":    {},
		"5 bytes":  trunc(5),
		"55 bytes": trunc(55),
		"57 bytes": append(slices.Clone(golden), 0),
	} {
		_, err := DecodeFormatFile(b)
		if !errors.Is(err, ErrDamaged) || errors.Is(err, ErrUnknownVersion) {
			t.Errorf("DecodeFormatFile(%s) = %v, want an error matching ErrDamaged and not ErrUnknownVersion", name, err)
		}
	}
}

// TestDecodeFormatFileRefusesAFlippedBit flips every bit of every byte of the
// golden file and checks the two outcomes there are: a flip in the version
// names an unknown version, a flip anywhere else damages the file.
func TestDecodeFormatFileRefusesAFlippedBit(t *testing.T) {
	golden := readGolden(t, "format_v1")

	for off := range FormatFileSize {
		for bit := range 8 {
			b := slices.Clone(golden)
			b[off] ^= 1 << bit

			_, err := DecodeFormatFile(b)
			var want error
			switch off {
			case 4, 5:
				want = ErrUnknownVersion
			default:
				want = ErrDamaged
			}
			if !errors.Is(err, want) {
				t.Errorf("byte %d bit %d flipped: DecodeFormatFile = %v, want an error matching %v", off, bit, err, want)
			}
		}
	}
}

// TestDecodeFormatFileRefusesAnUnknownVersion pins that a version this binary
// does not recognise is refused, whatever the file's length or checksum: a
// later version may have another size, so version 1's rules never judge its
// bytes.
func TestDecodeFormatFileRefusesAnUnknownVersion(t *testing.T) {
	for name, c := range map[string]struct {
		version uint16
	}{
		"version 0": {0},
		"version 2": {2},
	} {
		b := raw.Format{Version: c.version, Identity: testIdentity, Installation: testInstallation}.Bytes()
		if _, err := DecodeFormatFile(b); !errors.Is(err, ErrUnknownVersion) {
			t.Errorf("DecodeFormatFile(%s) = %v, want an error matching ErrUnknownVersion", name, err)
		}
	}

	v2 := raw.Format{Version: 2, Identity: testIdentity, Installation: testInstallation}.Bytes()
	v2Long := append(slices.Clone(v2), 0)
	if _, err := DecodeFormatFile(v2Long); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("DecodeFormatFile(a 57-byte version-2 file) = %v, want an error matching ErrUnknownVersion", err)
	}

	v2[len(v2)-1] ^= 0xff // a stale checksum is never judged, either
	if _, err := DecodeFormatFile(v2); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("DecodeFormatFile(a version-2 file with a stale checksum) = %v, want an error matching ErrUnknownVersion", err)
	}
}

// TestDecodeFormatFileReadsANonZeroGenerationAsVersion1 pins that the
// generation is never read below the replication extension's version: bytes a
// version-1 writer leaves zero do not fail the open when anything else set
// them.
func TestDecodeFormatFileReadsANonZeroGenerationAsVersion1(t *testing.T) {
	b := readGolden(t, "format_v1")
	copy(b[40:48], []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48})
	rechecksum(b)

	f, err := DecodeFormatFile(b)
	if err != nil {
		t.Fatalf("DecodeFormatFile with a non-zero generation = %v, want no error", err)
	}
	if f.Identity != testIdentity {
		t.Errorf("identity = % x, want % x", f.Identity, testIdentity)
	}
	if f.Installation != testInstallation {
		t.Errorf("installation = % x, want % x", f.Installation, testInstallation)
	}
}

// TestDecodeFormatFileRefusesNonZeroReservedBytes pins the stricter reading
// of the reserved bytes: written as zero, refused when not.
func TestDecodeFormatFileRefusesNonZeroReservedBytes(t *testing.T) {
	golden := readGolden(t, "format_v1")

	for _, c := range []struct {
		name   string
		extent [2]int
	}{
		{"reserved [6,8)", [2]int{6, 8}},
		{"reserved [48,52)", [2]int{48, 52}},
	} {
		b := slices.Clone(golden)
		copy(b[c.extent[0]:c.extent[1]], bytes.Repeat([]byte{0xff}, c.extent[1]-c.extent[0]))
		rechecksum(b)

		if _, err := DecodeFormatFile(b); !errors.Is(err, ErrDamaged) {
			t.Errorf("DecodeFormatFile(%s non-zero) = %v, want an error matching ErrDamaged", c.name, err)
		}
	}
}

// TestDecodeFormatFileRefusesAWrongMagic pins that the magic is judged, a
// file of another format never read as this one however it verifies.
func TestDecodeFormatFileRefusesAWrongMagic(t *testing.T) {
	b := readGolden(t, "format_v1")
	copy(b[0:4], "DJF2")
	rechecksum(b)

	if _, err := DecodeFormatFile(b); !errors.Is(err, ErrDamaged) {
		t.Errorf("DecodeFormatFile(DJF2) = %v, want an error matching ErrDamaged", err)
	}
}
