package format

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

const (
	// CatalogEntrySize is the size of one footer entry.
	CatalogEntrySize = 72

	// TrailerSize is the size of the trailer that ends a segment carrying
	// a footer: the file's last 32 bytes.
	TrailerSize = 32
)

// catalogLayoutVersion is the catalog layout this package writes and reads:
// independent of the format file's own version.
const catalogLayoutVersion uint16 = 1

// CatalogEntry is one footer entry: where a record sits in the segment and
// what its header says. It carries no payload of its own — a record whose
// payload matters is read at its RecordOffset and verified like any record —
// and no checksum of its own: the trailer's footer checksum covers it.
type CatalogEntry struct {
	File core.FileID

	// Extent is the file offset and length, as the record's header has
	// them.
	Extent core.Extent

	// RecordOffset is the record's offset in the segment. On disk it is a
	// 32-bit column, so AppendCatalogEntry refuses a value above
	// MaxUint32.
	RecordOffset int64

	Seq     core.Seq     // the journal's global append order
	Version core.Version // the content version the record carries
	Kind    Kind         // which record this is
	Tag     core.ShareTag
}

// Footer is a verified segment footer: where its entries start, and the
// entries, ascending by RecordOffset. The seal marker sits at Offset-32.
type Footer struct {
	Offset  int64
	Entries []CatalogEntry
}

// AppendCatalogEntry appends e's 72 bytes to dst and returns the result: the
// file, the extent, the record's offset in the segment, the sequence number,
// the version in its two halves, the kind, the tag, and reserved bytes
// written zero.
//
// It panics — caller bugs, refused at the call and never encoded onto disk,
// as the record encoder's conventions have it: what a writer could write and
// no reader could take back never reaches the disk — on an unknown kind; a
// non-zero epoch; a negative file offset or length, or an offset that
// overflows with the length; and a record offset below the first record
// slot, above MaxUint32, or off the multiple of 8 a record slot starts at.
func AppendCatalogEntry(dst []byte, e CatalogEntry) []byte {
	if !e.Kind.known() {
		panic(fmt.Sprintf("format: unknown kind %d", uint8(e.Kind)))
	}
	if e.Version.Epoch != 0 {
		panic(fmt.Sprintf("format: catalog entry version epoch %d, want 0", e.Version.Epoch))
	}
	if e.Extent.Off < 0 {
		panic(fmt.Sprintf("format: catalog entry file offset %d is below 0", e.Extent.Off))
	}
	if e.Extent.Len < 0 {
		panic(fmt.Sprintf("format: catalog entry length %d is below 0", e.Extent.Len))
	}
	// The bounds the reader refuses as no footer are panics here too: an
	// entry no reader of this format could take back never reaches the disk.
	if e.Extent.Off+e.Extent.Len < 0 {
		panic(fmt.Sprintf("format: catalog entry file offset %d plus the length %d overflows", e.Extent.Off, e.Extent.Len))
	}
	if e.RecordOffset < SegmentHeaderSize {
		panic(fmt.Sprintf("format: catalog entry record offset %d is below the first record slot %d", e.RecordOffset, SegmentHeaderSize))
	}
	if e.RecordOffset > math.MaxUint32 {
		panic(fmt.Sprintf("format: catalog entry record offset %d is above MaxUint32", e.RecordOffset))
	}
	if e.RecordOffset%8 != 0 {
		panic(fmt.Sprintf("format: catalog entry record offset %d is not a multiple of 8", e.RecordOffset))
	}

	file := e.File.Bytes()
	dst = append(dst, file[:]...)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(e.Extent.Off))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(e.RecordOffset))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(e.Extent.Len))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(e.Seq))
	dst = binary.LittleEndian.AppendUint64(dst, e.Version.Epoch)
	dst = binary.LittleEndian.AppendUint64(dst, e.Version.Counter)
	dst = append(dst, uint8(e.Kind))
	dst = append(dst, 0, 0, 0) // reserved
	dst = binary.LittleEndian.AppendUint32(dst, uint32(e.Tag))
	return binary.LittleEndian.AppendUint32(dst, 0) // reserved
}

// AppendTrailer appends the trailer's 32 bytes to dst and returns the
// result: the magic, the catalog layout version, the flags (none defined),
// the footerOffset, the entry count, the plain CRC32C of the footer's entry
// bytes, and the plain CRC32C of the trailer's own first 28 bytes — over the
// bytes this call appends, never over a caller's prefix. footer holds the
// concatenated entries the trailer names.
//
// It panics — caller bugs, as AppendCatalogEntry's — on a footer that is not
// a whole number of entries or whose count tops the 32-bit entryCount
// column, and on a footerOffset below the first seal marker slot (80: a
// marker cannot precede the first record slot at 48) or off the multiple of
// 8 a slot starts at.
func AppendTrailer(dst []byte, footerOffset int64, footer []byte) []byte {
	if len(footer)%CatalogEntrySize != 0 {
		panic(fmt.Sprintf("format: trailer footer of %d bytes, want a whole number of %d-byte entries", len(footer), CatalogEntrySize))
	}
	// The count column is 32 bits wide, and a footer large enough to
	// overflow it is at least 72 * MaxUint32 bytes: refusing here keeps
	// the count honest for any footer that could ever be built.
	if int64(len(footer)/CatalogEntrySize) > math.MaxUint32 {
		panic(fmt.Sprintf("format: trailer footer of %d entries, above MaxUint32", len(footer)/CatalogEntrySize))
	}
	if footerOffset < SegmentHeaderSize+SealMarkerSize {
		panic(fmt.Sprintf("format: trailer footerOffset %d, below the first seal marker slot %d", footerOffset, SegmentHeaderSize+SealMarkerSize))
	}
	if footerOffset%8 != 0 {
		panic(fmt.Sprintf("format: trailer footerOffset %d is not a multiple of 8", footerOffset))
	}

	start := len(dst)

	dst = append(dst, 'D', 'J', 'T', '1')
	dst = binary.LittleEndian.AppendUint16(dst, catalogLayoutVersion)
	dst = append(dst, 0, 0) // flags; none defined
	dst = binary.LittleEndian.AppendUint64(dst, uint64(footerOffset))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(footer)/CatalogEntrySize))
	dst = binary.LittleEndian.AppendUint32(dst, crc32c(footer))
	dst = append(dst, 0, 0, 0, 0) // reserved
	return binary.LittleEndian.AppendUint32(dst, crc32c(dst[start:]))
}

// ReadFooter reads a segment's footer: the final TrailerSize bytes tell it
// where the entries start, past a seal marker that must verify for this
// journal at this segment and offset, and the entries follow their own
// checksum there. A footer that does not verify — or that this format
// version's layout does not name — yields an error wrapping ErrNoFooter,
// never one of its own: the footer is an accelerator, so a reader that
// cannot use it loses speed and nothing else, and the caller scans.
//
// Any check the trailer, the marker or an entry fails refuses the whole
// footer, with the zero Footer — never a partial entry list — and the
// refusal's own message, so the caller's log names which rule failed.
//
// decision: a read that fails is returned as itself, never as "no footer".
// A failed read says nothing about the bytes it was asked for, so it is
// neither evidence of damage nor evidence of absence — and unreadable never
// counts as empty, so a segment the device cannot read must fail its reader
// rather than fall back to a scan that reads it just as little. Overturned
// only if the format rules a failed read is damage.
func ReadFooter(r io.ReaderAt, size int64, identity core.JournalID, segment core.SegmentID) (Footer, error) {
	if size < TrailerSize {
		return Footer{}, noFooter("the file is %d bytes, shorter than the %d a trailer takes", size, TrailerSize)
	}

	// The trailer, the file's last 32 bytes.
	trailer := make([]byte, TrailerSize)
	if err := readFull(r, trailer, size-TrailerSize); err != nil {
		return Footer{}, fmt.Errorf("format: read trailer at %d: %w", size-TrailerSize, err)
	}
	if got, want := binary.LittleEndian.Uint32(trailer[28:32]), crc32c(trailer[0:28]); got != want {
		return Footer{}, noFooter("trailer checksum 0x%08x, want the CRC32C of [0,28): 0x%08x", got, want)
	}
	if string(trailer[0:4]) != "DJT1" {
		return Footer{}, noFooter("magic %q, want %q", trailer[0:4], "DJT1")
	}
	if version := binary.LittleEndian.Uint16(trailer[4:6]); version != catalogLayoutVersion {
		return Footer{}, noFooter("unrecognised catalog layout version %d, want %d", version, catalogLayoutVersion)
	}
	if flags := binary.LittleEndian.Uint16(trailer[6:8]); flags != 0 {
		return Footer{}, noFooter("flags 0x%04x, read as an unrecognised version: none are defined", flags)
	}

	// decision: non-zero reserved bytes are refused as no footer, the
	// stricter reading — the table writes them as zero and says no more.
	// Accept them if the format's owner rules that a reader ignores them.
	if !zero(trailer[24:28]) {
		return Footer{}, noFooter("reserved bytes are not zero")
	}

	// The footerOffset checks, ordered so none reads a value it has not
	// bounded: the column is 64 bits, a read offset is an int64, and the
	// arithmetic below stays inside the one that has passed.
	footerOffset := binary.LittleEndian.Uint64(trailer[8:16])
	if footerOffset > math.MaxInt64 {
		return Footer{}, noFooter("footerOffset %d is above MaxInt64", footerOffset)
	}
	if footerOffset < SegmentHeaderSize+SealMarkerSize {
		return Footer{}, noFooter("footerOffset %d, below the seal marker a footer needs, %d", footerOffset, SegmentHeaderSize+SealMarkerSize)
	}
	if footerOffset%8 != 0 {
		return Footer{}, noFooter("footerOffset %d is not a multiple of 8", footerOffset)
	}
	if int64(footerOffset) > size-TrailerSize {
		return Footer{}, noFooter("footerOffset %d is past the trailer at %d", footerOffset, size-TrailerSize)
	}

	entryCount := int64(binary.LittleEndian.Uint32(trailer[16:20]))
	footerLen := entryCount * CatalogEntrySize
	if footerLen > size-TrailerSize-int64(footerOffset) {
		return Footer{}, noFooter("entryCount %d does not fit between footerOffset %d and the trailer", entryCount, footerOffset)
	}

	// The seal marker the footer starts just past: it must verify for
	// this journal, for this segment, at that offset — so a recycled
	// segment's old trailer, whose marker names the segment's previous
	// life, is never read as this one's.
	marker := make([]byte, SealMarkerSize)
	markerOff := int64(footerOffset) - SealMarkerSize
	if err := readFull(r, marker, markerOff); err != nil {
		return Footer{}, fmt.Errorf("format: read seal marker at %d: %w", markerOff, err)
	}
	if _, err := DecodeSealMarker(marker, identity, segment, markerOff); err != nil {
		return Footer{}, noFooter("no seal marker verifies at %d: %v", markerOff, err)
	}

	// The entries, covered by their own checksum in the trailer.
	footer := make([]byte, int(footerLen))
	if err := readFull(r, footer, int64(footerOffset)); err != nil {
		return Footer{}, fmt.Errorf("format: read footer entries at %d: %w", footerOffset, err)
	}
	if got, want := binary.LittleEndian.Uint32(trailer[20:24]), crc32c(footer); got != want {
		return Footer{}, noFooter("footer checksum 0x%08x, want the CRC32C of %d entry bytes: 0x%08x", got, len(footer), want)
	}

	entries := make([]CatalogEntry, 0, entryCount)
	prev := int64(0)
	for i := range entryCount {
		e, err := decodeCatalogEntry(footer[i*CatalogEntrySize : (i+1)*CatalogEntrySize])
		if err != nil {
			return Footer{}, err
		}
		if e.RecordOffset <= prev { // the first entry's prev is 0, which no slot's offset is at
			return Footer{}, noFooter("recordOffset %d is not above the one before it, %d: entries ascend by recordOffset", e.RecordOffset, prev)
		}
		prev = e.RecordOffset
		entries = append(entries, e)
	}

	// The extent check: the last record must end where the seal marker
	// sits, so a footer describing fewer records than the segment holds
	// is detected rather than silently truncating. With no entry the
	// records end at the first record slot.
	//
	// decision: the layout itself asks for this, beyond the reading
	// list's "identify the extent": the footerOffset row says a footer
	// starts "just past the seal marker", the marker "at a record slot
	// just past the last record", and a seal holds "a footer entry for
	// every record" — together, the last entry's record ends at
	// footerOffset-32. A footer ending short of the marker describes
	// fewer records than the segment holds, and one ending past it
	// describes a record the marker has already cut off — both are no
	// footer, and the scan then reads every record the segment does hold.
	// Overturned if the format's owner rules the layout places the seal
	// or the footer some other way.
	end := int64(SegmentHeaderSize)
	if len(entries) > 0 {
		last := entries[len(entries)-1]

		// Only a write's or a fill's body grows with the entry's length,
		// so only their length can drive the record's whole size past what
		// an int64 holds. Size() — the one account of a record's sizes — is
		// guarded rather than duplicated: its padding step adds 7 after the
		// constants, so each band takes the 7 in, and a length above it
		// would wrap Size()'s own arithmetic and is refused before the
		// call. Every other kind's body is a constant, bounded with the
		// header far below MaxInt64, and no segmented column reaches near
		// it either.
		if last.Kind == KindWrite && last.Extent.Len > math.MaxInt64-RecordHeaderSize-8-7 {
			return Footer{}, noFooter("write length %d leaves the record no room to end before MaxInt64", last.Extent.Len)
		}
		if last.Kind == KindFill && last.Extent.Len > math.MaxInt64-RecordHeaderSize-7 {
			return Footer{}, noFooter("fill length %d leaves the record no room to end before MaxInt64", last.Extent.Len)
		}
		whole := RecordHeader{Kind: last.Kind, Extent: core.Extent{Len: last.Extent.Len}}.Size()
		// The bands hold Size() itself inside an int64, but the record's
		// own offset is added to it and the recordOffset column still tops
		// at MaxUint32: the end sum is guarded where it is taken.
		if whole > math.MaxInt64-last.RecordOffset {
			return Footer{}, noFooter("recordOffset %d plus the record's size %d leaves the record no room to end before MaxInt64", last.RecordOffset, whole)
		}
		end = last.RecordOffset + whole
	}
	if end != markerOff {
		return Footer{}, noFooter("last record ends at %d, the seal marker is at %d", end, markerOff)
	}

	return Footer{Offset: int64(footerOffset), Entries: entries}, nil
}

// decodeCatalogEntry reads one 72-byte entry, refusing everything the
// trailer's reading rules do not spell but this reader holds a footer to:
// reserved bytes, an unknown kind, bounds the entry's int64 columns cannot
// hold. An error wraps ErrNoFooter, naming its own rule.
func decodeCatalogEntry(b []byte) (CatalogEntry, error) {
	e := CatalogEntry{
		File:         core.NewFileID([16]byte(b[0:16])),
		Extent:       core.Extent{Off: int64(binary.LittleEndian.Uint64(b[16:24])), Len: int64(binary.LittleEndian.Uint64(b[28:36]))},
		RecordOffset: int64(binary.LittleEndian.Uint32(b[24:28])),
		Seq:          core.Seq(binary.LittleEndian.Uint64(b[36:44])),
		Version:      core.Version{Epoch: binary.LittleEndian.Uint64(b[44:52]), Counter: binary.LittleEndian.Uint64(b[52:60])},
		Kind:         Kind(b[60]),
		Tag:          core.ShareTag(binary.LittleEndian.Uint32(b[64:68])),
	}

	// The trailer's reserved-bytes decision — recorded at its own check in
	// ReadFooter — holds for an entry's two ranges too: non-zero bytes are
	// refused as no footer rather than ignored.
	if !zero(b[61:64]) || !zero(b[68:72]) {
		return CatalogEntry{}, noFooter("reserved bytes are not zero")
	}

	// decision: the kinds table's zero-field rules — the fields a kind
	// spells as 0, which the record decoder refuses as damage — are not
	// applied to entries, here or at the writer. An entry is a summary a
	// writer derives from a record's header, and the record's own decode
	// polices its shape where the rule costs a record; nothing in the
	// footer's reading list reaches past the entry's own columns. Any
	// reader wanting the kinds' shapes in the entries takes back a
	// header-only summary and loses what the record's decode already
	// holds. Overturned if the format's owner rules the footer must
	// refuse an entry that breaks the kinds table's spellings.
	//
	// The kind itself must still be one this format version defines:
	if !e.Kind.known() {
		return CatalogEntry{}, noFooter("entry of unknown kind %d, want one of 1-14", uint8(e.Kind))
	}

	// decision: a non-zero epoch in an entry is read as no footer, not an
	// error of its own or a failed open. The footer is never authoritative
	// — the segments always win — so the scan reads the record the entry
	// names and refuses its epoch there, where the rule costs a record and
	// not the open. Overturned if the format's owner rules otherwise.
	if e.Version.Epoch != 0 {
		return CatalogEntry{}, noFooter("version epoch %d, want 0", e.Version.Epoch)
	}

	// decision: these bounds — the entry's file offset and length against
	// the int64 a core.Extent holds — are stricter than the byte table,
	// which bounds no field. They hold while a file cannot exceed MaxInt64
	// bytes: the day one could, an in-range extent a core.Extent cannot
	// hold exists and the refusal must go; until then no entry of this
	// format can carry one.
	if e.Extent.Off < 0 {
		return CatalogEntry{}, noFooter("file offset %d is above MaxInt64", uint64(e.Extent.Off))
	}
	if e.Extent.Len < 0 {
		return CatalogEntry{}, noFooter("length %d is above MaxInt64", uint64(e.Extent.Len))
	}
	if e.Extent.Off+e.Extent.Len < 0 {
		return CatalogEntry{}, noFooter("file offset %d plus length %d overflows", uint64(e.Extent.Off), uint64(e.Extent.Len))
	}

	if e.RecordOffset < SegmentHeaderSize {
		return CatalogEntry{}, noFooter("recordOffset %d is below the first record slot %d", e.RecordOffset, SegmentHeaderSize)
	}
	if e.RecordOffset%8 != 0 {
		return CatalogEntry{}, noFooter("recordOffset %d is not a multiple of 8", e.RecordOffset)
	}
	return e, nil
}

// readFull reads len(p) bytes at off, whole or not at all: a read that
// returns fewer bytes, quietly or reporting io.EOF — the end of the reader's
// bytes, which a caller reading a named-out extent must be told apart from a
// clean end — is io.ErrUnexpectedEOF, as io.ReadAtLeast reads the case, and
// any other error is returned as itself.
func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		// io.ReaderAt may return io.EOF with the full read, at the end
		// of its bytes; the whole buffer reached the caller either way.
		return nil
	}
	if err == nil || err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// noFooter builds one refusal: ErrNoFooter wrapped in a message naming the
// rule that failed it, so a log names which check refused.
func noFooter(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrNoFooter}, args...)...)
}
