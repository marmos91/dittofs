package core

import (
	"bytes"
	"testing"
)

// TestVersionBytesOrder checks the contract other components store versions
// under: two encodings compare byte by byte as their versions compare, and an
// encoding decodes back to its version.
func TestVersionBytesOrder(t *testing.T) {
	for _, c := range [][2]Version{
		{{0, 1}, {0, 2}},          // low byte
		{{0, 0xff}, {0, 0x100}},   // carry: fails if Bytes is little-endian
		{{0, ^uint64(0)}, {1, 0}}, // epoch boundary
	} {
		a, b := c[0].Bytes(), c[1].Bytes()
		if c[0].Compare(c[1]) != -1 || bytes.Compare(a[:], b[:]) != -1 || VersionFromBytes(a) != c[0] {
			t.Errorf("%v / %v: Compare, byte order or round trip disagree", c[0], c[1])
		}
	}
}
