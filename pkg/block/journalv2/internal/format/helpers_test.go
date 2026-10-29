package format

import (
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"os"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Test values. Within one structure every field gets its own range of byte
// values, so a field written a byte off, big-endian or in another field's
// place reads back as bytes from the wrong range, never as the right value by
// accident. The identity and the installation use the byte ranges raw's tests
// use, so the golden file and raw's bytes agree field for field.
var testIdentity = core.JournalID{
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

var testInstallation = core.InstallationID{
	0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
	0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
}

// testFormatFile is the format file the golden file holds.
var testFormatFile = FormatFile{Identity: testIdentity, Installation: testInstallation}

// readGolden reads testdata/golden/<name>.hex: # to end of line a comment,
// whitespace ignored, the rest hex. It takes the interface of both testing.T
// and testing.F, so a fuzz test's seeds read it too.
func readGolden(t testing.TB, name string) []byte {
	t.Helper()

	data, err := os.ReadFile("testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}

	// A comment is cut line by line, so a # never hides digits after it,
	// and whitespace is dropped so the digits of a line join.
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, r := range line {
			if !strings.ContainsRune(" \t\r", r) {
				digits.WriteRune(r)
			}
		}
	}
	if digits.Len()%2 != 0 {
		t.Fatalf("golden %s holds an odd number of hex digits", name)
	}
	b := make([]byte, digits.Len()/2)
	if _, err := hex.Decode(b, []byte(digits.String())); err != nil {
		t.Fatalf("decode golden %s: %v", name, err)
	}
	return b
}

// wantCRC is the standard library's CRC32C of b, computed here and not by
// the package, so a test never checks the codec against itself.
func wantCRC(b []byte) uint32 {
	return crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
}

// rechecksum stores the CRC32C of [0,52) at [52,56) of a format file, so an
// edit fails a decode only for the edit, not for its stale checksum.
func rechecksum(b []byte) {
	binary.LittleEndian.PutUint32(b[52:56], wantCRC(b[0:52]))
}
