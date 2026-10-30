package format

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// SealMarkerSize is the size of a seal marker.
const SealMarkerSize = 32

// SealMarker is the marker sealing writes at the record slot just past a
// segment's last record. It is not a record and has no kind.
type SealMarker struct {
	Segment core.SegmentID // the segment it seals
	Offset  int64          // its own offset in the segment
}

// AppendSealMarker appends m's 32 bytes to dst, seeded with the journal's
// identity, and returns the result: the magic, the segment, the marker's own
// offset, and the checksum of the bytes this call appends, never of a
// caller's prefix. The checksum is seeded with the identity, so the marker
// verifies only under the journal that wrote it. The offset is a slot's, so
// it is 0 or more; a negative one is a caller's bug and panics rather than
// writing a marker its own decoder would refuse.
func AppendSealMarker(dst []byte, identity core.JournalID, m SealMarker) []byte {
	if m.Offset < 0 {
		panic(fmt.Sprintf("format: seal marker offset %d is below 0", m.Offset))
	}

	start := len(dst)

	dst = append(dst, 'D', 'J', 'S', '1')
	dst = append(dst, 0, 0, 0, 0) // reserved
	dst = binary.LittleEndian.AppendUint64(dst, uint64(m.Segment))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(m.Offset))
	dst = append(dst, 0, 0, 0, 0) // reserved
	return binary.LittleEndian.AppendUint32(dst, crc32cSeeded(identitySeed(identity), dst[start:]))
}

// DecodeSealMarker reads a seal marker, opened under journal's identity, at
// the given segment and offset. An error matching ErrDamaged means the bytes
// are not a seal marker of this journal: a wrong length, magic or checksum
// against this identity's seed, reserved bytes that are not zero, or an own
// offset too large for the int64 a read offset is. An error matching
// ErrWrongPlace means the marker verifies but names another segment or
// another offset than the ones it was read at — it was written somewhere
// else, so a copy inside a payload never ends a scan — and it comes with the
// marker filled, which holds its bytes' segment and offset. A slot of zeros
// is ErrDamaged here; the scanner tells an untaken slot from a marker before
// decoding.
func DecodeSealMarker(b []byte, identity core.JournalID, segment core.SegmentID, offset int64) (SealMarker, error) {
	if len(b) != SealMarkerSize {
		return SealMarker{}, fmt.Errorf("%w: %d bytes, want %d", ErrDamaged, len(b), SealMarkerSize)
	}
	if string(b[0:4]) != "DJS1" {
		return SealMarker{}, fmt.Errorf("%w: magic %q, want %q", ErrDamaged, b[0:4], "DJS1")
	}
	// A marker seeded with another journal's identity fails here and is
	// indistinguishable from damage — that is what the seed is for. Unlike
	// a segment header, whose checksum is unseeded so a foreign one can be
	// told apart, no foreign marker is ever reported as anything but
	// damage.
	if got, want := binary.LittleEndian.Uint32(b[28:32]), crc32cSeeded(identitySeed(identity), b[0:28]); got != want {
		return SealMarker{}, fmt.Errorf("%w: checksum 0x%08x, want the CRC32C of the identity then [0,28): 0x%08x", ErrDamaged, got, want)
	}

	// decision: non-zero reserved bytes are refused as damage, the
	// stricter reading — the table writes them as zero and says no more.
	// Accept them if the format's owner rules that a reader ignores them.
	if !zero(b[4:8]) || !zero(b[24:28]) {
		return SealMarker{}, fmt.Errorf("%w: reserved bytes are not zero", ErrDamaged)
	}

	m := SealMarker{
		Segment: core.SegmentID(binary.LittleEndian.Uint64(b[8:16])),
		Offset:  int64(binary.LittleEndian.Uint64(b[16:24])),
	}

	// The offset column is 64 bits wide while a read offset is an int64:
	// a stored offset with its top bit set decodes negative and can name
	// no place this journal reads at, so it is refused as damage here,
	// before the place comparison — never reported as a marker written
	// somewhere else.
	//
	// decision: the refusal is stricter than the byte table, which bounds
	// no field. It holds while a segment cannot exceed MaxInt64 bytes —
	// the day one could, an in-range recorded offset exists and the
	// refusal must go; until then no marker of this format can carry one.
	if m.Offset < 0 {
		return SealMarker{}, fmt.Errorf("%w: own offset %d is above MaxInt64", ErrDamaged, uint64(m.Offset))
	}
	if m.Segment != segment || m.Offset != offset {
		return m, fmt.Errorf("%w: segment %d, offset %d, want the segment %d and offset %d it was read at", ErrWrongPlace, m.Segment, m.Offset, segment, offset)
	}
	return m, nil
}
