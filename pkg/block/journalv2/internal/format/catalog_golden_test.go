package format

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/raw"
)

// rawEntry is e as raw builds it: the same fields laid out by raw's own
// account of the entry table.
func rawEntry(e CatalogEntry) (out [raw.CatalogEntrySize]byte) {
	return raw.Entry{
		FileID:       e.File.Bytes(),
		Offset:       uint64(e.Extent.Off),
		RecordOffset: uint32(e.RecordOffset),
		Length:       uint64(e.Extent.Len),
		Seq:          uint64(e.Seq),
		Version:      raw.Version{Epoch: e.Version.Epoch, Counter: e.Version.Counter},
		Kind:         uint8(e.Kind),
		Tag:          uint32(e.Tag),
	}.Bytes()
}

// TestTrailerGolden pins the trailer's golden three ways, as the format's
// conformance rows ask: the encoders' bytes equal the hand-written hex file,
// which equals raw's account — and the reader, over the golden embedded where
// its segment would hold it, returns exactly the entries and footer offset the
// file writes. The golden holds two entries, so both the 72-byte entry layout
// and the 32-byte trailer layout are pinned, field for field.
func TestTrailerGolden(t *testing.T) {
	golden := readGolden(t, "trailer_two_entries")

	entryA := testEntryA
	entryA.RecordOffset = 48

	// The encoders' concatenated bytes: the two entries, then the trailer
	// over them.
	built := AppendCatalogEntry(nil, entryA)
	built = AppendCatalogEntry(built, testEntryB)
	footer := built
	built = AppendTrailer(built, testFooterOffset, footer)
	if !bytes.Equal(built, golden) {
		t.Errorf("the encoders' bytes =\n% x\nwant the golden file\n% x", built, golden)
	}

	// raw's bytes for the same fields: raw fixes no field of a catalog
	// entry, so all three accounts must agree byte for byte.
	rawA := rawEntry(entryA)
	rawB := rawEntry(testEntryB)
	rawBytes := raw.Trailer(1, uint64(testFooterOffset), rawA, rawB)
	if !bytes.Equal(append(append(rawA[:], rawB[:]...), rawBytes...), golden) {
		t.Errorf("raw's bytes =\n% x\nwant the golden file\n% x", append(append(rawA[:], rawB[:]...), rawBytes...), golden)
	}

	// The trailer the golden's last 32 bytes spell, per the conformance
	// row: magic DJT1, layout version 1, footerOffset the seal marker's
	// offset plus 32. The seal marker sits at footerOffset - 32 = 240.
	trailer := golden[len(golden)-TrailerSize:]
	if got := string(trailer[0:4]); got != "DJT1" {
		t.Errorf("the golden's magic is %q, want %q", got, "DJT1")
	}
	if got := binary.LittleEndian.Uint16(trailer[4:6]); got != 1 {
		t.Errorf("the golden's layout version is %d, want 1", got)
	}
	if got := int64(binary.LittleEndian.Uint64(trailer[8:16])); got != testFooterOffset {
		t.Errorf("the golden's footerOffset is %d, want %d", got, testFooterOffset)
	}

	// The reader, over the golden embedded where its segment holds it: 240
	// zero bytes, the marker at 240, then the 176 golden bytes. A segment is
	// the same bytes at a bigger cap, so this layout reads at any cap that
	// still fits.
	seg := make([]byte, 240)
	seg = AppendSealMarker(seg, testIdentity, SealMarker{Segment: testSegment, Offset: 240})
	seg = append(seg, golden...)
	f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ReadFooter over the golden segment = %v, want no error", err)
	}
	if f.Offset != testFooterOffset {
		t.Errorf("ReadFooter gave offset %d, want %d", f.Offset, testFooterOffset)
	}
	if len(f.Entries) != 2 || f.Entries[0] != testEntryA || f.Entries[1] != testEntryB {
		t.Errorf("ReadFooter gave entries %+v, want the golden's %+v and %+v", f.Entries, testEntryA, testEntryB)
	}

	// The golden's entry bytes are the ones its entries name: re-encoding
	// what the reader returned reproduces them.
	again := []byte{}
	for _, e := range f.Entries {
		again = AppendCatalogEntry(again, e)
	}
	if !slices.Equal(again, golden[:len(again)]) {
		t.Errorf("re-encoded entries =\n% x\nwant the golden's\n% x", again, golden[:len(again)])
	}
}

// TestAppendCatalogEntryAppendsToDst pins that the entry encoder appends to
// dst rather than overwriting it.
func TestAppendCatalogEntryAppendsToDst(t *testing.T) {
	prefix := []byte{0xee, 0xbe, 0xef}

	want := AppendCatalogEntry(nil, testEntryA)
	got := AppendCatalogEntry(slices.Clone(prefix), testEntryA)
	if !bytes.Equal(got, append(slices.Clone(prefix), want...)) {
		t.Errorf("AppendCatalogEntry with a prefix =\n% x\nwant the prefix then the entry\n% x", got, append(prefix, want...))
	}
}

// TestAppendTrailerAppendsToDst pins that the trailer encoder appends to dst
// rather than overwriting it: a caller's prefix is never part of the trailer
// and never covered by its CRC properly.
func TestAppendTrailerAppendsToDst(t *testing.T) {
	prefix := []byte{0xee, 0xbe, 0xef}

	footer := append(AppendCatalogEntry(nil, testEntryA), AppendCatalogEntry(nil, testEntryB)...)
	want := AppendTrailer(nil, testFooterOffset, footer)
	got := AppendTrailer(slices.Clone(prefix), testFooterOffset, footer)
	if !bytes.Equal(got, append(slices.Clone(prefix), want...)) {
		t.Errorf("AppendTrailer with a prefix =\n% x\nwant the prefix then the trailer\n% x", got, append(prefix, want...))
	}
}
