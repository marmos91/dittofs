package format

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestCRC32CMatchesTheRFC3720Vectors pins the checksum against the published
// CRC32C test vectors, so the codec does not check itself against itself.
func TestCRC32CMatchesTheRFC3720Vectors(t *testing.T) {
	inc, dec := make([]byte, 32), make([]byte, 32)
	for i := range 32 {
		inc[i], dec[i] = byte(i), byte(31-i)
	}

	for _, c := range []struct {
		name string
		in   []byte
		want uint32
	}{
		{"zeros", make([]byte, 32), 0x8A9136AA},
		{"ones", bytes.Repeat([]byte{0xff}, 32), 0x62A8AB43},
		{"incrementing", inc, 0x46DD794E},
		{"decrementing", dec, 0x113FDB5C},
		{"scsi read(10) PDU", bytes.Join([][]byte{
			{0x01, 0xc0, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x14, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x04, 0x00},
			{0x00, 0x00, 0x00, 0x14},
			{0x00, 0x00, 0x00, 0x18},
			{0x28, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x02, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
		}, nil), 0xD9963A56},
	} {
		if got := crc32c(c.in); got != c.want {
			t.Errorf("crc32c(%s) = 0x%08x, want 0x%08x", c.name, got, c.want)
		}
	}

	// The stored form of the zeros vector is little-endian, as RFC 3720
	// prints it: 8a 36 91 8a is checked above as a value, here as bytes.
	le := make([]byte, 4)
	binary.LittleEndian.PutUint32(le, crc32c(make([]byte, 32)))
	if got, want := le, []byte{0xaa, 0x36, 0x91, 0x8a}; !bytes.Equal(got, want) {
		t.Errorf("the zeros vector stored little-endian is % x, want % x", got, want)
	}
}
