package raw

import (
	"encoding/binary"
	"fmt"
)

// SegmentHeaderSize is the size of a segment header.
const SegmentHeaderSize = 48

// SegmentHeader is the header at offset 0 of every segment. Its checksum is
// not seeded, so another journal's segment verifies and is told apart by the
// identity it names rather than reported as damage.
//
// decision: Bytes writes format version 1 and stream number 0, because no check
// plants a segment that needs another value. Make either a field when one does.
type SegmentHeader struct {
	Identity    [16]byte
	ID          uint64 // the segment's id, the one in its file name
	Predecessor uint64 // the id of the segment its stream sealed before it, or 0 for none
}

// Bytes returns the header's 48 bytes.
func (h SegmentHeader) Bytes() []byte {
	b := make([]byte, SegmentHeaderSize)
	copy(b[0:4], "DJH1")
	binary.LittleEndian.PutUint16(b[4:6], 1)
	copy(b[8:24], h.Identity[:])
	binary.LittleEndian.PutUint64(b[24:32], h.ID)
	binary.LittleEndian.PutUint64(b[32:40], h.Predecessor)
	binary.LittleEndian.PutUint32(b[44:48], crc32c(b[0:44]))
	return b
}

// ParseSegmentHeader reads the identity, id and predecessor of a segment
// header. A wrong size, magic or checksum is an error. The format version and
// stream number are read past, so a header the journal wrote parses whatever
// stream opened it.
func ParseSegmentHeader(b []byte) (SegmentHeader, error) {
	if len(b) != SegmentHeaderSize {
		return SegmentHeader{}, fmt.Errorf("segment header is %d bytes, want %d", len(b), SegmentHeaderSize)
	}

	if string(b[0:4]) != "DJH1" {
		return SegmentHeader{}, fmt.Errorf("segment header magic is %q, want %q", b[0:4], "DJH1")
	}

	if got, want := binary.LittleEndian.Uint32(b[44:48]), crc32c(b[0:44]); got != want {
		return SegmentHeader{}, fmt.Errorf("segment header checksum is 0x%08x, want 0x%08x", got, want)
	}

	return SegmentHeader{
		Identity:    [16]byte(b[8:24]),
		ID:          binary.LittleEndian.Uint64(b[24:32]),
		Predecessor: binary.LittleEndian.Uint64(b[32:40]),
	}, nil
}
