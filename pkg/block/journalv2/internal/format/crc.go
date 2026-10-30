package format

import (
	"hash/crc32"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c returns the CRC32C of b, not seeded.
func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// crc32cSeeded returns the CRC32C of identity's 16 bytes followed by b: the
// checksum a structure seeded with the journal identity carries. The
// identity travels as a pointer because an array passed by value and sliced
// moves to the heap — one allocation on every seeded encode and decode.
func crc32cSeeded(identity *core.JournalID, b []byte) uint32 {
	return crc32.Update(crc32.Checksum(identity[:], castagnoli), castagnoli, b)
}
