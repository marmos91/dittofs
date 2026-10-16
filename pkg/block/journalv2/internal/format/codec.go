package format

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Every Decode function below accepts or rejects any input it is given, of any
// length and content: it never panics, and never returns a value its bytes do
// not contain. On error the returned value is the zero value. Every Encode
// function writes the structure's magic, zero reserved bytes and its
// checksums, and is the exact inverse of its Decode for every value Decode
// accepts.
//
// decision: a structure whose checksum is seeded with the journal identity
// takes the seed as an explicit parameter of its Encode and Decode, so no
// codec holds a journal's identity as hidden state. A structure the format
// does not seed takes no seed, so a seed cannot be passed where none applies.

// ErrNoFooter is returned for a trailer or footer entry that cannot be used.
// It is never an error to the caller: a segment without a usable footer is
// scanned instead, so its reader loses speed and nothing else.
var ErrNoFooter = errors.New("journal: segment has no usable footer")

// Checksum returns the CRC32C (Castagnoli, reflected, initial value and final
// XOR all ones) of parts, concatenated. With seed non-nil it is the CRC32C of
// the 16 bytes of *seed followed by parts: the checksum seeded with the
// journal identity. With seed nil it is the CRC32C of parts alone.
func Checksum(seed *core.JournalID, parts ...[]byte) uint32 {
	return 0
}

// EncodeFormatFile returns the bytes of f.
func EncodeFormatFile(f FormatFile) [FormatFileSize]byte {
	return [FormatFileSize]byte{}
}

// DecodeFormatFile decodes a format file's bytes, checked in this order:
//
//   - fewer than 6 bytes, or a magic other than MagicFormat: core.ErrCorrupt;
//   - a format version this implementation does not recognise:
//     core.ErrUnknownFormat;
//   - for format version 1, a length other than FormatFileSize:
//     core.ErrCorrupt;
//   - a checksum that does not verify: core.ErrCorrupt;
//   - a non-zero reserved byte under a checksum that verifies:
//     core.ErrUnknownFormat.
//
// The generation is decoded but never checked: format version 1 never reads
// it.
func DecodeFormatFile(b []byte) (FormatFile, error) {
	return FormatFile{}, core.ErrNotImplemented
}

// EncodeSegmentHeader returns the bytes of h.
func EncodeSegmentHeader(h SegmentHeader) [SegmentHeaderSize]byte {
	return [SegmentHeaderSize]byte{}
}

// DecodeSegmentHeader decodes the first SegmentHeaderSize bytes of b, checked
// in this order:
//
//   - fewer than SegmentHeaderSize bytes, a magic other than MagicSegment or
//     a checksum that does not verify: core.ErrCorrupt;
//   - a format version this implementation does not recognise, or a
//     non-zero reserved byte: core.ErrUnknownFormat;
//   - a segment id of zero: core.ErrCorrupt.
//
// It does not compare the identity with the journal's, nor the version with
// the format file's: a foreign or newer segment is the caller's to refuse.
func DecodeSegmentHeader(b []byte) (SegmentHeader, error) {
	return SegmentHeader{}, core.ErrNotImplemented
}

// EncodeSealMarker returns the bytes of m, its checksum seeded with seed.
func EncodeSealMarker(seed core.JournalID, m SealMarker) [SealMarkerSize]byte {
	return [SealMarkerSize]byte{}
}

// DecodeSealMarker decodes the first SealMarkerSize bytes of b, read at
// offset in the segment segment. It verifies only if its magic is MagicSeal,
// its checksum verifies seeded with seed, its reserved bytes are zero, and
// its segment id and own offset are segment and offset; anything else returns
// core.ErrCorrupt, as does a b shorter than SealMarkerSize. So a copy of a
// seal marker held inside a record's payload never ends a scan.
func DecodeSealMarker(seed core.JournalID, segment core.SegmentID, offset int64, b []byte) (SealMarker, error) {
	return SealMarker{}, core.ErrNotImplemented
}

// EncodeRecordHeader returns the bytes of h, its header checksum seeded with
// seed. h.PayloadChecksum is written as given: compute it with Checksum(&seed,
// ...) over the bytes that follow the header.
func EncodeRecordHeader(seed core.JournalID, h RecordHeader) [RecordHeaderSize]byte {
	return [RecordHeaderSize]byte{}
}

// DecodeRecordHeader decodes the first RecordHeaderSize bytes of b, read from
// the segment segment. It does not read or check the payload. Checked in this
// order:
//
//   - fewer than RecordHeaderSize bytes, a magic other than MagicRecord, a
//     header checksum that does not verify seeded with seed, or a segment id
//     other than segment: core.ErrCorrupt. Such a header does not verify,
//     and a scan treats its slot as damage;
//   - a kind this format version does not define, in any range, a content
//     version with a non-zero epoch, or a non-zero reserved byte:
//     core.ErrUnknownFormat. The header verifies, so the open fails; the
//     record is never skipped by its length;
//   - a negative file offset, length or synced-through offset:
//     core.ErrCorrupt.
//
// The fields a kind gives as zero are not checked here.
func DecodeRecordHeader(seed core.JournalID, segment core.SegmentID, b []byte) (RecordHeader, error) {
	return RecordHeader{}, core.ErrNotImplemented
}

// EncodeCatalogEntry returns the bytes of e.
func EncodeCatalogEntry(e CatalogEntry) [CatalogEntrySize]byte {
	return [CatalogEntrySize]byte{}
}

// DecodeCatalogEntry decodes the first CatalogEntrySize bytes of b. It
// returns ErrNoFooter for fewer than CatalogEntrySize bytes, a kind this
// format version does not define, a non-zero reserved byte, or a negative
// file offset or length. It checks no checksum: the trailer's footer checksum
// covers the entries, and every entry is checked again against the record
// header it leads to.
func DecodeCatalogEntry(b []byte) (CatalogEntry, error) {
	return CatalogEntry{}, core.ErrNotImplemented
}

// EncodeTrailer returns the bytes of t.
func EncodeTrailer(t Trailer) [TrailerSize]byte {
	return [TrailerSize]byte{}
}

// DecodeTrailer decodes the first TrailerSize bytes of b. It returns
// ErrNoFooter for fewer than TrailerSize bytes, a magic other than
// MagicTrailer, a checksum that does not verify, a catalog layout version
// other than CatalogVersion, non-zero flags, a non-zero reserved byte, or a
// negative footer offset. An unrecognised layout version is no footer, never
// an error.
//
// It does not check that the footer fits in the segment or that a seal marker
// precedes it: those need the segment, and are the reader's.
func DecodeTrailer(b []byte) (Trailer, error) {
	return Trailer{}, core.ErrNotImplemented
}

// EncodeModTime returns the bytes of t.
func EncodeModTime(t ModTime) [ModTimeSize]byte {
	return [ModTimeSize]byte{}
}

// DecodeModTime decodes the first ModTimeSize bytes of b. It returns
// core.ErrCorrupt for fewer than ModTimeSize bytes. Any value is accepted,
// zero included.
func DecodeModTime(b []byte) (ModTime, error) {
	return 0, core.ErrNotImplemented
}

// EncodeLoss returns the bytes of l.
func EncodeLoss(l Loss) [LossPayloadSize]byte {
	return [LossPayloadSize]byte{}
}

// DecodeLoss decodes a loss record's payload. It returns core.ErrCorrupt for
// a b of any length but LossPayloadSize, and core.ErrUnknownFormat for a flag
// word with a bit set that this format version does not define, or a reason
// other than the three LossReason values. The payload checksum has verified
// before DecodeLoss is called, so a bit it does not know was written by a
// newer format, not by damage.
func DecodeLoss(b []byte) (Loss, error) {
	return Loss{}, core.ErrNotImplemented
}

// EncodeCloneSpec returns the bytes of s.
func EncodeCloneSpec(s CloneSpec) [CloneSpecPayloadSize]byte {
	return [CloneSpecPayloadSize]byte{}
}

// DecodeCloneSpec decodes a clone target record's payload. It returns
// core.ErrCorrupt for a b of any length but CloneSpecPayloadSize or a negative
// source offset, and core.ErrUnknownFormat for an AsOf with a non-zero epoch.
func DecodeCloneSpec(b []byte) (CloneSpec, error) {
	return CloneSpec{}, core.ErrNotImplemented
}

// EncodeCut returns the bytes of c.
func EncodeCut(c Cut) [CutPayloadSize]byte {
	return [CutPayloadSize]byte{}
}

// DecodeCut decodes a hold, unhold or stamp record's payload. It returns
// core.ErrCorrupt for a b of any length but CutPayloadSize. Any cut number is
// accepted.
func DecodeCut(b []byte) (Cut, error) {
	return Cut{}, core.ErrNotImplemented
}
