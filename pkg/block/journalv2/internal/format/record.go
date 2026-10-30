package format

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// RecordHeaderSize is the size of a record header; the body and then the
// padding to the next multiple of 8 follow it.
const RecordHeaderSize = 96

// RecordHeader is a record's 96-byte header: what the record is, which file
// and extent of it, where in the journal it was written, and the two
// checksums that cover the whole record between them.
type RecordHeader struct {
	Kind    Kind
	Segment core.SegmentID // the segment the record was written into
	File    core.FileID    // the file the record belongs to
	Extent  core.Extent    // file offset and length, as the kind spells them
	Seq     core.Seq       // the journal's global append order
	Version core.Version   // the content version the record carries
	Tag     core.ShareTag  // the share whose limits the record counts against

	// SyncedThrough is the offset in this segment up to which a sync had
	// completed when the record was written.
	SyncedThrough int64

	// Offset is the record's own offset in its segment, 0 or more and a
	// multiple of 8, where no segment exceeds MaxInt64 bytes.
	Offset int64

	// PayloadCRC is the seeded checksum of the body. Set by
	// DecodeRecordHeader from the bytes; AppendRecord ignores it and
	// writes the checksum of the body it is given.
	PayloadCRC uint32
}

// BodySize returns the bytes the record's body takes: after the header,
// before the padding. It panics on an unknown kind: no length reaches a
// caller for a record it cannot name, so it can never be stepped over.
func (h RecordHeader) BodySize() int64 {
	return bodySize(h.Kind, h.Extent.Len)
}

// Size returns the bytes the record takes as a whole, the multiple of 8 its
// header, body and padding occupy. It panics on an unknown kind, as
// BodySize does.
func (h RecordHeader) Size() int64 {
	return (RecordHeaderSize + h.BodySize() + 7) &^ 7
}

// AppendRecord appends h's header, body and padding to dst, seeded with the
// journal identity, and returns the result: a 96-byte header with its two
// checksum slots and reserved bytes written zero, the body laid out by the
// kind, and zero padding to the next multiple of 8. Both checksums are
// computed over slices of dst this call appends, never over a caller's
// prefix. It allocates nothing beyond the append when dst has capacity.
//
// The body is laid out by the kind: for a write the modification time then
// the payload; a fill its payload; a loss the dropped sequence number then
// the flag word of its synced bit, its offloaded bit and its reason; a clone
// target the source share, the source file, the source offset and AsOf; a
// hold, unhold or stamp the cut number. The payload checksum at [84,88)
// covers the body alone and the header checksum at [92,96) every other
// header byte, the payload checksum included.
//
// It panics — caller bugs, refused at the call and never encoded onto disk —
// on: an unknown kind; a negative own offset, one above MaxUint32 or one not
// a multiple of 8; a negative file offset, length or synced-through offset;
// a write or fill whose payload is not the length in bytes; another kind
// carrying a payload; a non-zero epoch in the version or in the clone's
// AsOf; a loss reason outside 1–4. It does not enforce the kinds table's
// zero-field rules, which no writer of this package can express: a record
// that breaks them is refused at the API before AppendRecord is reached.
func AppendRecord(dst []byte, identity core.JournalID, h RecordHeader, body RecordBody) []byte {
	if !h.Kind.known() {
		panic(fmt.Sprintf("format: unknown kind %d", uint8(h.Kind)))
	}
	if h.Offset < 0 {
		panic(fmt.Sprintf("format: record offset %d is below 0", h.Offset))
	}
	if h.Offset > math.MaxUint32 {
		panic(fmt.Sprintf("format: record offset %d is above MaxUint32", h.Offset))
	}
	if h.Offset%8 != 0 {
		panic(fmt.Sprintf("format: record offset %d is not a multiple of 8", h.Offset))
	}
	if h.Extent.Off < 0 {
		panic(fmt.Sprintf("format: file offset %d is below 0", h.Extent.Off))
	}
	if h.Extent.Len < 0 {
		panic(fmt.Sprintf("format: length %d is below 0", h.Extent.Len))
	}
	if h.SyncedThrough < 0 {
		panic(fmt.Sprintf("format: synced-through offset %d is below 0", h.SyncedThrough))
	}
	if h.Version.Epoch != 0 {
		panic(fmt.Sprintf("format: version epoch %d is above 0", h.Version.Epoch))
	}

	switch h.Kind {
	case KindWrite, KindFill:
		if int64(len(body.Payload)) != h.Extent.Len {
			panic(fmt.Sprintf("format: payload of %d bytes, want the length %d", len(body.Payload), h.Extent.Len))
		}
	default:
		if len(body.Payload) != 0 {
			panic(fmt.Sprintf("format: kind %d carries a payload, want none", uint8(h.Kind)))
		}
		if body.Clone.AsOf.Epoch != 0 {
			panic(fmt.Sprintf("format: clone AsOf epoch %d is above 0", body.Clone.AsOf.Epoch))
		}
		if !body.Loss.Reason.known() {
			if h.Kind != KindLoss && body.Loss.Reason != 0 {
				panic(fmt.Sprintf("format: kind %d carries a loss reason, want none", uint8(h.Kind)))
			}
			if h.Kind == KindLoss {
				panic(fmt.Sprintf("format: loss reason %d is outside 1-4", uint8(body.Loss.Reason)))
			}
		}
	}

	start := len(dst)
	seed := identitySeed(identity)

	dst = append(dst, 'D', 'J', 'R', '1')
	dst = append(dst, uint8(h.Kind))
	dst = append(dst, 0, 0, 0) // reserved
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.Segment))
	file := h.File.Bytes()
	dst = append(dst, file[:]...)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.Extent.Off))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.Extent.Len))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.Seq))
	dst = binary.LittleEndian.AppendUint64(dst, h.Version.Epoch)
	dst = binary.LittleEndian.AppendUint64(dst, h.Version.Counter)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(h.Tag))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.SyncedThrough))

	// The payload checksum's slot at [84,88) is written zero first, then
	// redone from the body once appended, so the header checksum covers
	// what the finished header names.
	dst = binary.LittleEndian.AppendUint32(dst, 0)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(h.Offset))
	dst = binary.LittleEndian.AppendUint32(dst, 0) // the header checksum's slot

	bodyStart := len(dst)

	switch h.Kind {
	case KindWrite:
		dst = binary.LittleEndian.AppendUint64(dst, uint64(body.MTime))
		dst = append(dst, body.Payload...)
	case KindFill:
		dst = append(dst, body.Payload...)
	case KindLoss:
		dst = binary.LittleEndian.AppendUint64(dst, uint64(body.Loss.Dropped))
		flags := uint64(0)
		if body.Loss.Synced {
			flags |= 1 << 0
		}
		if body.Loss.Offloaded {
			flags |= 1 << 1
		}
		flags |= uint64(body.Loss.Reason) << 8
		dst = binary.LittleEndian.AppendUint64(dst, flags)
	case KindCloneTarget:
		dst = append(dst, body.Clone.Share[:]...)
		cloneFile := body.Clone.File.Bytes()
		dst = append(dst, cloneFile[:]...)
		dst = binary.LittleEndian.AppendUint64(dst, uint64(body.Clone.Off))
		dst = binary.LittleEndian.AppendUint64(dst, body.Clone.AsOf.Epoch)
		dst = binary.LittleEndian.AppendUint64(dst, body.Clone.AsOf.Counter)
	case KindHold, KindUnhold, KindStamp:
		dst = binary.LittleEndian.AppendUint64(dst, uint64(body.Cut))
	}

	// The checksum slots redone, so the whole record is covered. Both are
	// over slices of dst from start, never over a caller's prefix, and
	// derived from the seed, never a slice of the identity.
	binary.LittleEndian.PutUint32(dst[start+84:start+88], crc32cSeeded(seed, dst[bodyStart:]))
	binary.LittleEndian.PutUint32(dst[start+92:start+96], crc32cSeeded(seed, dst[start:start+92]))

	// The padding lines the record up within its segment, not within dst:
	// the caller's prefix is never a part of it, so it is counted from
	// the record's own start.
	for (len(dst)-start)%8 != 0 {
		dst = append(dst, 0)
	}
	return dst
}

// DecodeRecordHeader reads a record header, opened under journal's identity,
// at the given segment and offset. An error matching ErrDamaged means the
// bytes are not a record of this journal: a wrong length, magic or checksum
// against this identity's seed, reserved bytes that are not zero, or a field
// outside the bounds or inside the kinds table's zero-field rules — another
// journal's record lands here too, indistinguishable from damage, as with a
// seal marker, which is what the seed is for. An error matching ErrWrongPlace
// means the header verifies but was written somewhere else — at another
// segment or another offset — and it is never returned filled: its other
// fields are unvalidated and nothing it could report is needed by a scan, so
// the zero header comes back. An error matching ErrUnknownKind means the
// header verifies at its place but names a kind this format version does not
// define — 0 or 15 and above — and it too comes back zero, so no length
// reaches the caller and the record can never be stepped over by one. An
// error matching ErrNonZeroEpoch means the record verifies but a version in
// it carries a non-zero epoch half, which this format version forbids.
func DecodeRecordHeader(b []byte, identity core.JournalID, segment core.SegmentID, offset int64) (RecordHeader, error) {
	if len(b) != RecordHeaderSize {
		return RecordHeader{}, fmt.Errorf("%w: %d bytes, want %d", ErrDamaged, len(b), RecordHeaderSize)
	}
	if string(b[0:4]) != "DJR1" {
		return RecordHeader{}, fmt.Errorf("%w: magic %q, want %q", ErrDamaged, b[0:4], "DJR1")
	}
	// A header seeded with another journal's identity fails here and is
	// indistinguishable from damage — that is what the seed is for. Unlike
	// a segment header, whose checksum is unseeded so a foreign one can be
	// told apart, no foreign record is ever reported as anything but
	// damage.
	if got, want := binary.LittleEndian.Uint32(b[92:96]), crc32cSeeded(identitySeed(identity), b[0:92]); got != want {
		return RecordHeader{}, fmt.Errorf("%w: checksum 0x%08x, want the CRC32C of the identity then [0,92): 0x%08x", ErrDamaged, got, want)
	}

	storedSegment := core.SegmentID(binary.LittleEndian.Uint64(b[8:16]))
	storedOffset := int64(binary.LittleEndian.Uint32(b[88:92]))
	if storedSegment != segment || storedOffset != offset {
		return RecordHeader{}, fmt.Errorf("%w: segment %d, own offset %d, want the segment %d and offset %d it was read at", ErrWrongPlace, storedSegment, storedOffset, segment, offset)
	}

	kind := Kind(b[4])
	if !kind.known() {
		return RecordHeader{}, fmt.Errorf("%w: kind %d, want one of 1-14", ErrUnknownKind, uint8(kind))
	}

	epoch := binary.LittleEndian.Uint64(b[56:64])
	if epoch != 0 {
		return RecordHeader{}, fmt.Errorf("%w: version epoch %d, want 0", ErrNonZeroEpoch, epoch)
	}

	// decision: non-zero reserved bytes are refused as damage, the
	// stricter reading — the table writes them as zero and says no more.
	// Accept them if the format's owner rules that a reader ignores them.
	if !zero(b[5:8]) {
		return RecordHeader{}, fmt.Errorf("%w: reserved bytes are not zero", ErrDamaged)
	}

	h := RecordHeader{
		Kind:          kind,
		Segment:       storedSegment,
		File:          core.NewFileID([16]byte(b[16:32])),
		Extent:        core.Extent{Off: int64(binary.LittleEndian.Uint64(b[32:40])), Len: int64(binary.LittleEndian.Uint64(b[40:48]))},
		Seq:           core.Seq(binary.LittleEndian.Uint64(b[48:56])),
		Version:       core.Version{Epoch: epoch, Counter: binary.LittleEndian.Uint64(b[64:72])},
		Tag:           core.ShareTag(binary.LittleEndian.Uint32(b[72:76])),
		SyncedThrough: int64(binary.LittleEndian.Uint64(b[76:84])),
		PayloadCRC:    binary.LittleEndian.Uint32(b[84:88]),
		Offset:        storedOffset,
	}

	// decision: the D9 bounds are stricter than the byte table, which
	// bounds no field. They hold while a segment cannot exceed MaxInt64
	// bytes — the day one could, an in-range extent a core.Extent cannot
	// hold exists and the refusal must go; until then no record of this
	// format can carry one.
	if h.Extent.Off < 0 {
		return RecordHeader{}, fmt.Errorf("%w: file offset %d is above MaxInt64", ErrDamaged, uint64(h.Extent.Off))
	}
	if h.Extent.Len < 0 {
		return RecordHeader{}, fmt.Errorf("%w: length %d is above MaxInt64", ErrDamaged, uint64(h.Extent.Len))
	}
	if h.SyncedThrough < 0 {
		return RecordHeader{}, fmt.Errorf("%w: synced-through offset %d is above MaxInt64", ErrDamaged, uint64(h.SyncedThrough))
	}
	if h.Extent.Off+h.Extent.Len < 0 {
		return RecordHeader{}, fmt.Errorf("%w: file offset %d plus length %d overflows", ErrDamaged, uint64(h.Extent.Off), uint64(h.Extent.Len))
	}
	if offset+h.Size() < 0 {
		return RecordHeader{}, fmt.Errorf("%w: own offset %d plus the record's size %d overflows", ErrDamaged, offset, h.Size())
	}

	if err := checkFields(h); err != nil {
		return RecordHeader{}, err
	}
	return h, nil
}

// VerifyRecordBody checks a record's whole body, of the length the header
// names, against the header's payload checksum, and decodes it. It is the
// only verify function: every path that hands a record's bytes to anyone
// goes through it and hands out no part of a record that does not verify. On
// any failure it returns the zero body — no payload slice, never part of
// one — and an error matching ErrDamaged, or ErrNonZeroEpoch for a clone
// target whose AsOf carries a non-zero epoch half.
//
// ponytail: a read of 4 KiB from a 1 MiB record reads and checks the whole
// megabyte, because the one checksum covers the whole payload and nothing
// may be handed out that was not checked whole. Upgrade to a payload checksum
// per 64 KiB of payload, each covering its own piece, when warm small-read
// rates over large records miss their target.
func VerifyRecordBody(body []byte, identity core.JournalID, h RecordHeader) (RecordBody, error) {
	if int64(len(body)) != h.BodySize() {
		return RecordBody{}, fmt.Errorf("%w: body of %d bytes, want the %d the header names", ErrDamaged, len(body), h.BodySize())
	}
	// A header of a kind with no body passes an empty body here, whose
	// checksum is the identity seed continued over no bytes — the checksum
	// its header carries.
	if got, want := crc32cSeeded(identitySeed(identity), body), h.PayloadCRC; got != want {
		return RecordBody{}, fmt.Errorf("%w: payload checksum 0x%08x, want the CRC32C of the identity then the body: 0x%08x", ErrDamaged, got, want)
	}

	var out RecordBody
	switch h.Kind {
	case KindWrite:
		out.MTime = int64(binary.LittleEndian.Uint64(body[0:8]))
		out.Payload = body[8:] // aliases the input, as a slice of the caller's buffer may
	case KindFill:
		out.Payload = body
	case KindLoss:
		flags := binary.LittleEndian.Uint64(body[8:16])
		if flags & ^uint64(0x000000000000ff03) != 0 {
			return RecordBody{}, fmt.Errorf("%w: flag word %#x holds bits outside 0, 1 and 8-15", ErrDamaged, flags)
		}
		reason := LossReason(flags >> 8)
		if !reason.known() {
			return RecordBody{}, fmt.Errorf("%w: loss reason %d, want one of 1-4", ErrDamaged, uint8(reason))
		}
		out.Loss = Loss{
			Dropped:   core.Seq(binary.LittleEndian.Uint64(body[0:8])),
			Synced:    flags&(1<<0) != 0,
			Offloaded: flags&(1<<1) != 0,
			Reason:    reason,
		}
	case KindCloneTarget:
		if epoch := binary.LittleEndian.Uint64(body[40:48]); epoch != 0 {
			return RecordBody{}, fmt.Errorf("%w: AsOf epoch %d, want 0", ErrNonZeroEpoch, epoch)
		}
		off := int64(binary.LittleEndian.Uint64(body[32:40]))
		if off < 0 {
			return RecordBody{}, fmt.Errorf("%w: source offset %d is above MaxInt64", ErrDamaged, uint64(off))
		}
		if off+h.Extent.Len < 0 {
			return RecordBody{}, fmt.Errorf("%w: source offset %d plus length %d overflows", ErrDamaged, uint64(off), uint64(h.Extent.Len))
		}
		out.Clone = CloneSource{
			Share: core.ShareID(body[0:16]),
			File:  core.NewFileID([16]byte(body[16:32])),
			Off:   off,
			AsOf:  core.Version{Counter: binary.LittleEndian.Uint64(body[48:56])},
		}
	case KindHold, KindUnhold, KindStamp:
		out.Cut = core.SnapshotCut(binary.LittleEndian.Uint64(body[0:8]))
	}
	return out, nil
}
