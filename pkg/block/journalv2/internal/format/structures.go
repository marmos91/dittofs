package format

import "github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"

// A content version is stored as 16 bytes, the epoch half then the counter
// half, each a little-endian 64-bit integer. Every offset and length below is
// a 64-bit integer stored as its little-endian two's complement; a decoder
// refuses a negative value as malformed. The fields a structure carries in
// its bytes but not in its Go value (magic, reserved bytes, checksum) are
// written by its encoder and checked by its decoder.

// FormatFile is the content of a journal's format file.
//
//	offset size field
//	     0    4 magic, MagicFormat
//	     4    2 format version
//	     6    2 reserved, zero
//	     8   16 journal identity
//	    24   16 installation ID
//	    40    8 generation
//	    48    4 reserved, zero
//	    52    4 CRC32C over bytes [0, 52), not seeded
type FormatFile struct {
	Version      uint16
	Identity     core.JournalID
	Installation core.InstallationID

	// Generation is zero in format version 1, and never read: only the
	// replication extension's format version raises it.
	Generation uint64
}

// SegmentHeader is a segment's header, written once at offset 0 when a stream
// takes the segment.
//
//	offset size field
//	     0    4 magic, MagicSegment
//	     4    2 format version of the journal that created the segment
//	     6    2 reserved, zero
//	     8   16 journal identity
//	    24    8 segment id, the one in the segment's file name
//	    32    8 predecessor's segment id, or 0 for none
//	    40    4 number of the append stream that opened it
//	    44    4 CRC32C over bytes [0, 44), not seeded
//
// The checksum is not seeded, so another journal's segment verifies and is
// recognised as foreign by the identity it names rather than reported as
// damage.
type SegmentHeader struct {
	Version     uint16
	Identity    core.JournalID
	Segment     core.SegmentID
	Predecessor core.SegmentID

	// Stream is for reporting only: the predecessor, not this number, links a
	// stream's segments.
	Stream uint32
}

// SealMarker ends a sealed segment's records. It is not a record: it carries
// no file, extent or version. Bytes after it belong to the footer.
//
//	offset size field
//	     0    4 magic, MagicSeal
//	     4    4 reserved, zero
//	     8    8 segment id
//	    16    8 the marker's own offset in the segment
//	    24    4 reserved, zero
//	    28    4 CRC32C over bytes [0, 28), seeded with the journal identity
type SealMarker struct {
	Segment core.SegmentID
	Offset  int64
}

// RecordHeader is a record's header. Both of a record's checksums are seeded
// with the journal identity; the header checksum covers every other header
// field, the payload checksum included, so the two cover the whole record.
//
//	offset size field
//	     0    4 magic, MagicRecord
//	     4    1 record kind
//	     5    3 reserved, zero
//	     8    8 segment id
//	    16   16 FileID
//	    32    8 file offset
//	    40    8 length
//	    48    8 sequence number
//	    56   16 content version, epoch then counter
//	    72    4 share tag
//	    76    8 synced-through offset
//	    84    4 payload checksum
//	    88    4 reserved, zero
//	    92    4 header checksum, over bytes [0, 92)
//
// The payload checksum covers the bytes between the header and the padding: a
// write record's modification time and content, a fill record's content, or
// a fixed payload. For a kind with nothing after the header it is the
// checksum of no bytes.
type RecordHeader struct {
	Kind    Kind
	Segment core.SegmentID
	File    core.FileID
	Offset  int64
	Length  int64
	Seq     core.Seq
	Version core.Version
	Tag     core.ShareTag

	// SyncedThrough is the offset in this segment up to which a sync had
	// completed when the record was written.
	SyncedThrough int64

	PayloadChecksum uint32
}

// CatalogEntry describes one record of a sealed segment, in its footer. It
// carries no payload and no checksum of its own: the trailer's footer
// checksum covers every entry.
//
//	offset size field
//	     0   16 FileID
//	    16    8 file offset of the record's first byte
//	    24    4 the record's offset within the segment
//	    28    8 length
//	    36    8 sequence number
//	    44   16 content version, epoch then counter
//	    60    1 record kind
//	    61    3 reserved, zero
//	    64    4 share tag
//	    68    4 reserved, zero
type CatalogEntry struct {
	File   core.FileID
	Offset int64

	// RecordOffset is 32 bits, which caps a segment below 4 GiB.
	RecordOffset uint32

	Length  int64
	Seq     core.Seq
	Version core.Version
	Kind    Kind
	Tag     core.ShareTag
}

// Trailer locates a sealed segment's footer. It is always the segment file's
// last 32 bytes, and is accepted only behind a seal marker that verifies for
// the segment at FooterOffset - SealMarkerSize.
//
//	offset size field
//	     0    4 magic, MagicTrailer
//	     4    2 catalog layout version
//	     6    2 flags; none defined
//	     8    8 footerOffset, the absolute offset of the first footer entry
//	    16    4 entryCount
//	    20    4 CRC32C over the footer entries, not seeded
//	    24    4 reserved, zero
//	    28    4 CRC32C over bytes [0, 28), not seeded
type Trailer struct {
	Version         uint16
	Flags           uint16
	FooterOffset    int64
	EntryCount      uint32
	EntriesChecksum uint32
}

// ModTime is a write record's modification time: nanoseconds since the Unix
// epoch, UTC, that the write sets as the file's modification time, or zero
// when the caller's open suspended modification-time updates. It precedes the
// write's content and the payload checksum covers it.
//
//	offset size field
//	     0    8 nanoseconds since the Unix epoch, signed
type ModTime int64

// LossReason is why a loss record dropped its record.
type LossReason uint8

// The loss reasons. Zero is not a reason.
const (
	LossCorrupt          LossReason = 1 // the record's checksum did not verify
	LossStale            LossReason = 2 // the record was refused as stale
	LossFailedSyncWindow LossReason = 3 // the record was written in a sync window that failed
)

// Loss is a loss record's payload: the record it drops.
//
//	offset size field
//	     0    8 the dropped record's sequence number
//	     8    8 flag word: bit 0 synced, bit 1 offloaded, bits 8-15 the reason;
//	            every other bit zero
type Loss struct {
	Dropped core.Seq

	// Synced reports whether the dropped record was synced.
	Synced bool

	// Offloaded reports whether the dropped record's offloaded bit was set.
	Offloaded bool

	Reason LossReason
}

// CloneSpec is a clone target record's payload: where the cloned bytes come
// from. The destination offset and length are the record header's file offset
// and length.
//
//	offset size field
//	     0   16 source ShareID
//	    16   16 source FileID
//	    32    8 source offset
//	    40   16 AsOf, a content version, epoch then counter
type CloneSpec struct {
	SourceShare  core.ShareID
	SourceFile   core.FileID
	SourceOffset int64
	AsOf         core.Version
}

// Cut is a hold, unhold or stamp record's payload: the snapshot cut the record
// is for.
//
//	offset size field
//	     0    8 the cut number
type Cut struct {
	Cut core.SnapshotCut
}
