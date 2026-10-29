package raw

import "encoding/binary"

// TrailerSize is the size of a segment's trailer.
const TrailerSize = 32

// CatalogEntrySize is the size of a catalog entry.
const CatalogEntrySize = 72

// Trailer returns the 32 bytes that end a sealed segment's file, for a footer
// of entries that starts at footerOffset. The entry count is the number of
// entries and the footer checksum covers them in order. Both checksums are not
// seeded.
//
// decision: flags are written as 0, because a check that needs the trailer
// read as an unrecognised version gets it from layoutVersion. Make flags an
// input when a check needs a nonzero value of its own.
func Trailer(layoutVersion uint16, footerOffset uint64, entries ...[CatalogEntrySize]byte) []byte {
	var footer []byte
	for _, e := range entries {
		footer = append(footer, e[:]...)
	}

	b := make([]byte, TrailerSize)
	copy(b[0:4], "DJT1")

	binary.LittleEndian.PutUint16(b[4:6], layoutVersion)
	binary.LittleEndian.PutUint64(b[8:16], footerOffset)
	binary.LittleEndian.PutUint32(b[16:20], uint32(len(entries)))
	binary.LittleEndian.PutUint32(b[20:24], crc32c(footer))
	binary.LittleEndian.PutUint32(b[28:32], crc32c(b[0:28]))

	return b
}
