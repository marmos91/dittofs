package format

// The sizes of the on-disk structures, in bytes. Every structure is packed
// without padding, with integers little-endian, and puts its checksum last.
const (
	FormatFileSize    = 56 // the format file
	SegmentHeaderSize = 48 // a segment's header, at offset 0; the first record slot follows it
	SealMarkerSize    = 32 // a seal marker, at the record slot past a segment's last record
	RecordHeaderSize  = 96 // a record's header
	CatalogEntrySize  = 72 // one footer entry
	TrailerSize       = 32 // a sealed segment's trailer, its file's last 32 bytes

	// RecordAlign is the alignment of every record slot within a segment. A
	// record's header, modification time and payload are followed by zero
	// padding to the next multiple of RecordAlign, which no checksum covers
	// and a reader ignores.
	RecordAlign = 8
)

// The sizes of the fixed payloads that follow a record header, in bytes,
// before the padding to RecordAlign.
const (
	ModTimeSize          = 8  // a write record's modification time, before its content
	LossPayloadSize      = 16 // a loss record's payload
	CloneSpecPayloadSize = 56 // a clone target record's payload
	CutPayloadSize       = 8  // a hold, unhold or stamp record's payload
)

// The magic values that open each structure: four ASCII bytes. A scan tells
// a slot by its first four bytes: MagicRecord a record header, MagicSeal a
// seal marker, four zero bytes a zero slot, anything else damage.
const (
	MagicFormat  = "DJF1" // the format file
	MagicSegment = "DJH1" // a segment header
	MagicSeal    = "DJS1" // a seal marker
	MagicRecord  = "DJR1" // a record header
	MagicTrailer = "DJT1" // a trailer
)

// The versions this implementation writes and reads.
const (
	// FormatVersion is the format version of the format file and of every
	// segment header this implementation writes. A later version names what
	// it adds; an unrecognised one fails the open.
	FormatVersion = 1

	// CatalogVersion is the catalog layout version of every trailer this
	// implementation writes, independent of FormatVersion. An unrecognised
	// one is read as no footer, never as an error.
	CatalogVersion = 1
)

// Kind is a record's kind, the byte at offset 4 of its header. Values are
// stable: a value is never reassigned, nor reused once its kind is retired.
// Zero is never a kind, so a zero slot never reads as one.
type Kind uint8

// The record kinds of format version 1. The comment on each gives its header
// fields and what follows the header.
const (
	// KindWrite: the file, the extent written, the version written; then an
	// 8-byte modification time, then length bytes of content.
	KindWrite Kind = 1

	// KindFill: the file, the extent filled, the version given to the fill;
	// then length bytes of content.
	KindFill Kind = 2

	// KindRelease: the file, one extent released, the version released;
	// nothing after the header. One record per extent.
	KindRelease Kind = 3

	// KindTruncate: the file, the new size as the offset and 0 as the
	// length, its version; nothing after the header.
	KindTruncate Kind = 4

	// KindDeallocate: the file, the extent punched, its version; nothing
	// after the header.
	KindDeallocate Kind = 5

	// KindDelete: the file, offset and length 0, its version; nothing after
	// the header.
	KindDelete Kind = 6

	// KindCloneTarget: the destination file, its destination offset and
	// length, its version; then a 56-byte clone spec.
	KindCloneTarget Kind = 7

	// KindOffloaded: the file, one extent marked, the newest version marked;
	// nothing after the header. One record per extent.
	KindOffloaded Kind = 8

	// KindUnmark: the file, one extent unmarked, the version of the content
	// unmarked; nothing after the header. One record per extent.
	KindUnmark Kind = 9

	// KindLoss: the dropped record's file, the extent dropped, the version
	// dropped; then a 16-byte loss payload.
	KindLoss Kind = 10

	// KindHold: the file, offset and length 0, the hold mark as the version;
	// then an 8-byte cut number.
	KindHold Kind = 11

	// KindUnhold: a zero file, offset and length 0, a zero version; then an
	// 8-byte cut number.
	KindUnhold Kind = 12

	// KindStamp: the file, offset and length 0, the version stamped through;
	// then an 8-byte cut number.
	KindStamp Kind = 13

	// KindForget: a zero file, offset and length 0, a zero version; nothing
	// after the header. It names only the share tag.
	KindForget Kind = 14
)

// The reserved ranges of Kind. A value in either range is assigned only with
// the format version that introduces it; a verifying header carrying a kind
// its segment's format version does not define fails the open in every
// range, and is never skipped by its length.
const (
	KindCoreReservedFirst Kind = 15  // first value reserved for later core kinds
	KindCoreReservedLast  Kind = 127 // last value reserved for later core kinds
	KindExtensionFirst    Kind = 128 // first value reserved for extensions
	KindExtensionLast     Kind = 255 // last value reserved for extensions
)
