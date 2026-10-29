package raw

import (
	"bytes"
	"testing"
)

// TestSeededCRC32CCoversTheIdentityThenTheBytes pins that a seeded checksum
// covers the journal identity and then the bytes, as one stream.
func TestSeededCRC32CCoversTheIdentityThenTheBytes(t *testing.T) {
	b := []byte("a journal's bytes")

	if got, want := seededCRC32C(testIdentity, b), wantSeededCRC(testIdentity, b); got != want {
		t.Errorf("seededCRC32C = 0x%08x, want the CRC32C of the identity then the bytes 0x%08x", got, want)
	}
}

// TestCRC32CMatchesTheRFC3720Vectors pins the checksum against the published
// CRC32C test vectors.
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
	} {
		if got := crc32c(c.in); got != c.want {
			t.Errorf("crc32c(%s) = 0x%08x, want 0x%08x", c.name, got, c.want)
		}
	}
}
