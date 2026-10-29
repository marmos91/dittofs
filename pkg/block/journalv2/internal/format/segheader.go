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
// version-1 segment header; one matching ErrUnknownVersion means it verifies
// but names a version this binary does not read; one matching ErrForeign
// means it verifies and belongs to another journal, and it comes with the
// decoded header, so the caller can report whose segment it is.
//
// The returned header's ID is not compared with the id in the segment's
// file name: the caller must, and a mismatch is what an interrupted recycle
// leaves — a file whose old header still names the segment it used to be.
func DecodeSegmentHeader(b []byte, journal core.JournalID) (SegmentHeader, error) {
	h := SegmentHeader{}

	if len(b) < 6 {
		return h, fmt.Errorf("%w: %d bytes, no version to read", ErrDamaged, len(b))
	}
	if string(b[0:4]) != "DJH1" {
		return h, fmt.Errorf("%w: magic %q, want %q", ErrDamaged, b[0:4], "DJH1")
	}
	if len(b) != SegmentHeaderSize {
		// decision: a header of another length is judged for its version
		// first, as the format's rules order it: a later version may change
		// the size, so its length is never judged by version 1's rules.
		version := binary.LittleEndian.Uint16(b[4:6])
		if version != FormatVersion {
			return h, fmt.Errorf("%w: version %d, want %d", ErrUnknownVersion, version, FormatVersion)
		}
		return h, fmt.Errorf("%w: %d bytes, want %d", ErrDamaged, len(b), SegmentHeaderSize)
	}
	// decision: a header of version 1's size is judged by its checksum
	// before its version field: a header whose checksum does not verify is
	// damage whatever its version field says, because a damaged segment is
	// reported and its records still attached while an unknown version
	// fails the open; only a header that verifies can name a version this
	// binary does not know.
	if got, want := binary.LittleEndian.Uint32(b[44:48]), crc32c(b[0:44]); got != want {
		return h, fmt.Errorf("%w: checksum 0x%08x, want the CRC32C of [0,44): 0x%08x", ErrDamaged, got, want)
	}
	// The comparison is against the one version this package reads, the
	// same as "not newer than the format file's" while only version 1
	// exists: the format file has named version 1 since the journal was
	// created, so a header that verifies and reads version 1 is in step
	// with any format file a version-1 binary could have written.
	if version := binary.LittleEndian.Uint16(b[4:6]); version != FormatVersion {
		return h, fmt.Errorf("%w: version %d, want %d", ErrUnknownVersion, version, FormatVersion)
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
