package raw

import (
	"encoding/binary"
	"hash/crc32"
)

// Field values with no byte repeated next to another, and none shared between
// fields, so a field written a byte off, in the wrong order or in another
// field's place cannot read back right by accident.
var testIdentity = [16]byte{
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

const (
	testSegment uint64 = 0x0807060504030201
	testOffset  uint64 = 0x2827262524232221

	testPredecessor uint64 = 0x3837363534333231
	testStream      uint32 = 0x44434241
)

// wantCRC is the standard library's CRC32C of b, computed here and not by
// the package, so a test never checks raw against itself.
func wantCRC(b []byte) uint32 {
	return crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
}

// wantSeededCRC is wantCRC of the identity followed by b.
func wantSeededCRC(identity [16]byte, b []byte) uint32 {
	return wantCRC(append(identity[:], b...))
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// zero reports whether every byte of b is zero.
func zero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
