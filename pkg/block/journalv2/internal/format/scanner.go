package format

import (
	"errors"
	"io"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// ErrForeignSegment is returned by a Scanner whose segment header verifies but
// names a journal identity other than its seed. The segment belongs to
// another journal: it is reported and left alone, and none of its records is
// attached.
var ErrForeignSegment = errors.New("journal: segment belongs to another journal")

// SlotKind is what a Scanner found at a record slot.
type SlotKind uint8

// The slot kinds.
const (
	// SlotRecord is a record header that verifies. Its payload is not read.
	SlotRecord SlotKind = iota + 1

	// SlotSeal is a seal marker that verifies for this segment and offset.
	// It is the last slot: the bytes after it belong to the footer.
	SlotSeal

	// SlotZero is a header slot of zeros followed by a verifying header: the
	// slot of a write that failed and was never acknowledged. It is neither
	// damage nor a torn tail.
	SlotZero

	// SlotDamage is a slot that is neither a verifying header, a verifying
	// seal marker nor zeros. Whether it is a torn tail or corruption is
	// decided by the caller, from the synced-through offsets of the records
	// after it.
	SlotDamage
)

// Slot is one slot a Scanner found. For SlotZero and SlotDamage, the slot runs
// from Offset to the Offset of the next slot Next returns, or to the end of
// the segment when Next returns io.EOF.
type Slot struct {
	Kind   SlotKind
	Offset int64

	// Header is set for SlotRecord.
	Header RecordHeader

	// Seal is set for SlotSeal.
	Seal SealMarker
}

// Scanner reads one segment's record slots in order, without a catalog. It
// trusts a verifying header's length to find the next slot and does not read
// payloads. Past a zero slot or damage it searches forward, at each
// RecordAlign boundary, for the next header that verifies, and resumes there.
//
// A Scanner is not safe for concurrent use.
type Scanner struct{}

// NewScanner returns a scanner over the size bytes of the segment r, whose
// records are checked under seed. It reads nothing until Next is called.
func NewScanner(r io.ReaderAt, size int64, seed core.JournalID) *Scanner {
	return &Scanner{}
}

// Next returns the next slot. The first call reads the segment header at
// offset 0, and returns:
//
//   - io.EOF, and no slot, for a header slot of zeros: a spare no stream has
//     taken holds no record;
//   - core.ErrCorrupt for a header that does not verify, and
//     core.ErrUnknownFormat for one whose format version is not recognised;
//   - ErrForeignSegment for a header naming another journal identity.
//
// After that, Next returns slots in ascending offset until it returns io.EOF:
// after a SlotSeal, or when no verifying header follows a zero slot, which
// marks the end of what was written. It returns core.ErrUnknownFormat for a
// verifying header of an unknown kind, which fails the open, and an error
// from r unchanged. After any error, every later call returns the same error.
//
// decision: the scanner reads the segment's id and identity from its own
// header, so a caller cannot scan a segment under the wrong id.
func (s *Scanner) Next() (Slot, error) {
	return Slot{}, core.ErrNotImplemented
}

// Header returns the segment header the first Next read, and false until
// then or if it did not verify.
func (s *Scanner) Header() (SegmentHeader, bool) {
	return SegmentHeader{}, false
}
