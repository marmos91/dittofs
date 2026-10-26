package raw

import (
	"encoding/binary"
	"hash/crc32"
)

// Test values. Within one structure every field gets its own range of byte
// values, so a field written a byte off, big-endian or in another field's place
// reads back as bytes from the wrong range, never as the right value by
// accident. Separate structures never share a buffer, so ranges may repeat
// across them. Integers are chosen so their little-endian bytes count up:
// 0x0807060504030201 is stored as 01 02 03 04 05 06 07 08.
//
//	0x01-0x08  segment id         0x41-0x44  stream number
//	0x10-0x1f  journal identity   0x47-0x48  catalog layout version
//	0x21-0x28  marker offset      0x51-0x52  format version
//	0x31-0x38  predecessor        0x53-0x5a  footer offset
//	0x60-0x6f  installation ID    0x70-0xff  catalog entries
//
// Within the catalog entries' range, each field of a catalog entry has its own
// range:
//
//	0x70-0x7f  FileID             0xa1-0xa8  length
//	0x81-0x88  file offset        0xb1-0xb8  sequence number
//	0x91-0x94  recordOffset       0xc1       kind
//	                              0xd1-0xd4  share tag

var testIdentity = [16]byte{
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

const (
	testSegment uint64 = 0x0807060504030201
	testOffset  uint64 = 0x2827262524232221

	testPredecessor uint64 = 0x3837363534333231
	testStream      uint32 = 0x44434241

	testFormatVersion uint16 = 0x5251

	testLayoutVersion uint16 = 0x4847
	testFooterOffset  uint64 = 0x5a59585756555453
)

var testInstallation = [16]byte{
	0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
	0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
}

// testEntries are two catalog entries filled with the bytes 0x70, 0x71, ...
// 0xff in order: the first entry holds 0x70..0xb7, the second 0xb8..0xff.
var testEntries = func() [][CatalogEntrySize]byte {
	var e [2][CatalogEntrySize]byte
	v := byte(0x70)

	for i := range e {
		for j := range e[i] {
			e[i][j] = v
			v++
		}
	}

	return e[:]
}()

var testCatalogEntry = Entry{
	FileID: [16]byte{
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77,
		0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f,
	},
	Offset:       0x8887868584838281,
	RecordOffset: 0x94939291,
	Length:       0xa8a7a6a5a4a3a2a1,
	Seq:          0xb8b7b6b5b4b3b2b1,
	Kind:         0xc1,
	Tag:          0xd4d3d2d1,
}

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
