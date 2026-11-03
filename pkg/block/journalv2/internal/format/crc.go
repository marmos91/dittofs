package format

import "hash/crc32"

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c returns the CRC32C of b, not seeded.
func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }
