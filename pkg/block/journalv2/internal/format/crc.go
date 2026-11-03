package format

import (
	"hash/crc32"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c returns the CRC32C of b, not seeded.
func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// identitySeed is the seed a seeded checksum derives from the journal's
// identity: the CRC32C of its own 16 bytes, from which the CRC32C of the
// identity then the covered bytes continues. It walks the table itself
// rather than slicing the identity and handing the slice to hash/crc32,
// which moves the array to the heap — one allocation on every seeded
// encode and decode.
func identitySeed(identity core.JournalID) uint32 {
	crc := ^uint32(0)
	for _, v := range identity {
		crc = castagnoli[byte(crc)^v] ^ crc>>8
	}
	return ^crc
}

// crc32cSeeded returns the CRC32C continued from seed over b. With the
// identity's seed this is the checksum a structure seeded with the journal
// identity carries: the identity's bytes then the covered bytes. The seed,
// not the identity, is the parameter, so no copy of the identity must
// outlive the call that hashes it.
func crc32cSeeded(seed uint32, b []byte) uint32 {
	return crc32.Update(seed, castagnoli, b)
}
