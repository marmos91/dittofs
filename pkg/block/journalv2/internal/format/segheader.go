package format

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// SegmentHeaderSize is the size of a segment header; the first record slot
// follows it.
const SegmentHeaderSize = 48

// SegmentHeader is the header at offset 0 of every segment: whose journal
// the segment is, its id, and the segment its stream sealed before it.
type SegmentHeader struct {
	Identity    core.JournalID
	ID          core.SegmentID // the id in the segment's file name; never 0
	Predecessor core.SegmentID // 0 for none
	Stream      uint32         // the append stream that opened it, for reporting only
}

// AppendSegmentHeader appends h's 48 bytes to dst and returns the result:
// the magic, the format version, the identity, the id, the predecessor, the
// stream, and the checksum of the bytes this call appends, never of a
// caller's prefix. The checksum is not seeded with the identity, so another
// journal's segment verifies here and is told apart by the identity it
// names, not reported as damage.
func AppendSegmentHeader(dst []byte, h SegmentHeader) []byte {
	start := len(dst)

	dst = append(dst, 'D', 'J', 'H', '1')
	dst = binary.LittleEndian.AppendUint16(dst, FormatVersion)
	dst = append(dst, 0, 0) // reserved
	dst = append(dst, h.Identity[:]...)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.ID))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.Predecessor))
	dst = binary.LittleEndian.AppendUint32(dst, h.Stream)
	return binary.LittleEndian.AppendUint32(dst, crc32c(dst[start:]))
}

// DecodeSegmentHeader reads a segment header, opened under journal's
// identity. An error matching ErrDamaged means the bytes are not a
// version-1 segment header; one matching ErrUnknownVersion means they name
// a version this binary does not read; one matching ErrForeign means they
// verify but belong to another journal, and it comes with the decoded
// header, so the caller can report whose segment it is.
func DecodeSegmentHeader(b []byte, journal core.JournalID) (SegmentHeader, error) {
	h := SegmentHeader{}

	// decision: the version is read before length and checksum, as the
	// format's rules order it, so a bit flipped in a version-1 header's
	// version field reports an unknown version rather than damage; both
	// fail the open. A later version may have another size, so its length
	// and checksum are never judged by version 1's rules.
	if len(b) < 6 {
		return h, fmt.Errorf("%w: %d bytes, no version to read", ErrDamaged, len(b))
	}
	if string(b[0:4]) != "DJH1" {
		return h, fmt.Errorf("%w: magic %q, want %q", ErrDamaged, b[0:4], "DJH1")
	}
	if version := binary.LittleEndian.Uint16(b[4:6]); version != FormatVersion {
		return h, fmt.Errorf("%w: version %d, want %d", ErrUnknownVersion, version, FormatVersion)
	}
	if len(b) != SegmentHeaderSize {
		return h, fmt.Errorf("%w: %d bytes, want %d", ErrDamaged, len(b), SegmentHeaderSize)
	}
	if got, want := binary.LittleEndian.Uint32(b[44:48]), crc32c(b[0:44]); got != want {
		return h, fmt.Errorf("%w: checksum 0x%08x, want the CRC32C of [0,44): 0x%08x", ErrDamaged, got, want)
	}

	h.Identity = core.JournalID(b[8:24])
	h.ID = core.SegmentID(binary.LittleEndian.Uint64(b[24:32]))
	h.Predecessor = core.SegmentID(binary.LittleEndian.Uint64(b[32:40]))
	h.Stream = binary.LittleEndian.Uint32(b[40:44])

	// decision: a header that verifies under another identity is that
	// journal's, so its other fields are not this journal's to judge, and
	// recovery must be able to tell it from damage — the reason the
	// checksum is not seeded. Report it as foreign whatever its reserved
	// bytes or id hold; the header is returned filled.
	if h.Identity != journal {
		return h, fmt.Errorf("%w: identity % x, want % x", ErrForeign, h.Identity, journal)
	}

	// decision: non-zero reserved bytes and an id of zero are refused as
	// damage, the stricter reading — the table writes them as zero and
	// says no more. Accept them if the format's owner rules that a reader
	// ignores them.
	if !zero(b[6:8]) {
		return SegmentHeader{}, fmt.Errorf("%w: reserved bytes are not zero", ErrDamaged)
	}
	if h.ID == 0 {
		return SegmentHeader{}, fmt.Errorf("%w: segment id is 0, want 1 or more", ErrDamaged)
	}
	return h, nil
}
