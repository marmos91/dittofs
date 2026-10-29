package raw

import "hash/crc32"

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c returns the CRC32C of b.
func crc32c(b []byte) uint32 {
	return crc32.Checksum(b, castagnoli)
}

// seededCRC32C returns the CRC32C of identity followed by b.
func seededCRC32C(identity [16]byte, b []byte) uint32 {
	return crc32.Update(crc32c(identity[:]), castagnoli, b)
}
