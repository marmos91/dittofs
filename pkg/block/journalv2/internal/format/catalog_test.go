package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// noFooterCase is one refusal the footer reader makes: how the golden
// segment's bytes are edited, and a piece of the refusal's own message, so a
// case cannot pass on a refusal another rule made first.
type noFooterCase struct {
	name string
	set  func(seg, trailer []byte)
	say  string
}

// noFooterCases returns every refusal's case, all built over the golden
// segment: the two entries' footer, its seal marker at 240, the trailer at
// the segment's end. A trailer edit takes the trailer's 32 bytes; an entry
// edit takes the segment and writes at testFooterOffset plus its own field's
// span; a whole-segment edit — only the marker ones — rebuilds part of the
// segment.
func noFooterCases() []noFooterCase {
	return []noFooterCase{
		{"bad magic", func(seg, tr []byte) { copy(tr[0:4], "DJX1") }, `magic "DJX1", want "DJT1"`},
		{"layout version 2", func(seg, tr []byte) { binary.LittleEndian.PutUint16(tr[4:6], 2) }, "unrecognised catalog layout version"},
		{"non-zero flags", func(seg, tr []byte) { binary.LittleEndian.PutUint16(tr[6:8], 1) }, "flags"},
		{"a non-zero reserved byte in the trailer at 24", func(seg, tr []byte) { tr[24] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in the trailer at 25", func(seg, tr []byte) { tr[25] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in the trailer at 26", func(seg, tr []byte) { tr[26] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in the trailer at 27", func(seg, tr []byte) { tr[27] = 0xff }, "reserved bytes are not zero"},
		{"footerOffset below 80", func(seg, tr []byte) { binary.LittleEndian.PutUint64(tr[8:16], 48) }, "below the seal marker a footer needs"},
		{"footerOffset misaligned", func(seg, tr []byte) { binary.LittleEndian.PutUint64(tr[8:16], uint64(testFooterOffset)+4) }, "not a multiple of 8"},
		{"footerOffset above MaxInt64", func(seg, tr []byte) { binary.LittleEndian.PutUint64(tr[8:16], 0x8000000000000000) }, "above MaxInt64"},
		{"footerOffset past the trailer", func(seg, tr []byte) { binary.LittleEndian.PutUint64(tr[8:16], uint64(maxSegmentSize)) }, "is past the trailer at 416"},
		{"entryCount that does not fit", func(seg, tr []byte) { binary.LittleEndian.PutUint32(tr[16:20], 3) }, "entryCount 3 does not fit"},
		{"no verifying seal marker", func(seg, tr []byte) { clear(seg[testFooterOffset-32 : testFooterOffset]) }, "no seal marker verifies"},
		{"a non-zero reserved byte in an entry at 61", func(seg, tr []byte) { seg[testFooterOffset+61] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in an entry at 62", func(seg, tr []byte) { seg[testFooterOffset+62] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in an entry at 63", func(seg, tr []byte) { seg[testFooterOffset+63] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in an entry at 68", func(seg, tr []byte) { seg[testFooterOffset+68] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in an entry at 69", func(seg, tr []byte) { seg[testFooterOffset+69] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in an entry at 70", func(seg, tr []byte) { seg[testFooterOffset+70] = 0xff }, "reserved bytes are not zero"},
		{"a non-zero reserved byte in an entry at 71", func(seg, tr []byte) { seg[testFooterOffset+71] = 0xff }, "reserved bytes are not zero"},
		{"entry file offset above MaxInt64", func(seg, tr []byte) {
			binary.LittleEndian.PutUint64(seg[testFooterOffset+16:testFooterOffset+24], 0x8000000000000000)
		}, "file offset 9223372036854775808 is above MaxInt64"},
		{"entry length above MaxInt64", func(seg, tr []byte) {
			binary.LittleEndian.PutUint64(seg[testFooterOffset+28:testFooterOffset+36], 0x8000000000000000)
		}, "length 9223372036854775808 is above MaxInt64"},
		{"entry extent overflow", func(seg, tr []byte) {
			binary.LittleEndian.PutUint64(seg[testFooterOffset+16:testFooterOffset+24], 0x7fffffffffffffff)
			binary.LittleEndian.PutUint64(seg[testFooterOffset+28:testFooterOffset+36], 1)
		}, "overflows"},
		{"entry non-zero epoch", func(seg, tr []byte) {
			binary.LittleEndian.PutUint64(seg[testFooterOffset+44:testFooterOffset+52], 1)
		}, "epoch"},
		{"entry of an unknown kind", func(seg, tr []byte) { seg[testFooterOffset+60] = 15 }, "unknown kind"},
		{"entry recordOffset below the first record slot", func(seg, tr []byte) {
			binary.LittleEndian.PutUint32(seg[testFooterOffset+24:testFooterOffset+28], 8)
		}, "recordOffset 8 is below the first record slot 48"},
		{"entry recordOffset misaligned", func(seg, tr []byte) {
			binary.LittleEndian.PutUint32(seg[testFooterOffset+24:testFooterOffset+28], 50)
		}, "recordOffset 50 is not a multiple of 8"},
		{"entries equal in recordOffset", func(seg, tr []byte) {
			binary.LittleEndian.PutUint32(seg[testFooterOffset+24:testFooterOffset+28], 144)
		}, "recordOffset 144 is not above the one before it, 144"},
		{"entries descending in recordOffset", func(seg, tr []byte) {
			binary.LittleEndian.PutUint32(seg[testFooterOffset+24:testFooterOffset+28], 240)
		}, "recordOffset 144 is not above the one before it, 240"},
		{"records end short of the seal marker", func(seg, tr []byte) {
			binary.LittleEndian.PutUint32(seg[testFooterOffset+72+24:testFooterOffset+72+28], 136)
		}, "last record ends at 232, the seal marker is at 240"},
		{"records end past the seal marker", func(seg, tr []byte) {
			binary.LittleEndian.PutUint32(seg[testFooterOffset+72+24:testFooterOffset+72+28], 152)
		}, "last record ends at 248, the seal marker is at 240"},
		{"a last write whose length leaves its record no room", func(seg, tr []byte) {
			seg[testFooterOffset+72+60] = uint8(KindWrite)
			// The file offset is zeroed too: entry B's own offset plus a
			// length at the write band's top would overflow, and the
			// entry's own decoder checks pass before the sizes are asked
			// for.
			clear(seg[testFooterOffset+72+16 : testFooterOffset+72+24])
			binary.LittleEndian.PutUint64(seg[testFooterOffset+72+28:testFooterOffset+72+36], math.MaxInt64-RecordHeaderSize-8-7+1)
		}, "write length 9223372036854775697 leaves the record no room"},
		{"a last write at the band's padding edge", func(seg, tr []byte) {
			// MaxInt64-104 is the highest length the band's plain sum, 96+8
			// plus the length, still admits — but Size() adds 7 for the
			// padding before clearing them, and that sum wraps. The band
			// itself takes the padding's 7 in, so this length is refused
			// under the length rule's own message.
			seg[testFooterOffset+72+60] = uint8(KindWrite)
			clear(seg[testFooterOffset+72+16 : testFooterOffset+72+24])
			binary.LittleEndian.PutUint64(seg[testFooterOffset+72+28:testFooterOffset+72+36], math.MaxInt64-RecordHeaderSize-8)
		}, "write length 9223372036854775703 leaves the record no room"},
		{"a last fill whose length leaves its record no room", func(seg, tr []byte) {
			seg[testFooterOffset+72+60] = uint8(KindFill)
			clear(seg[testFooterOffset+72+16 : testFooterOffset+72+24])
			binary.LittleEndian.PutUint64(seg[testFooterOffset+72+28:testFooterOffset+72+36], math.MaxInt64-RecordHeaderSize-7+1)
		}, "fill length 9223372036854775705 leaves the record no room"},
		{"a last record whose offset leaves it no room", func(seg, tr []byte) {
			// A fill at the highest 8-multiple the recordOffset column
			// holds, with the length well under the fill band's top:
			// every entry check passes and so does the fill band, and
			// only the end sum, which even the recordOffset column's top
			// can drive past MaxInt64, refuses it.
			seg[testFooterOffset+72+60] = uint8(KindFill)
			clear(seg[testFooterOffset+72+16 : testFooterOffset+72+24])
			binary.LittleEndian.PutUint32(seg[testFooterOffset+72+24:testFooterOffset+72+28], math.MaxUint32-7)
			binary.LittleEndian.PutUint64(seg[testFooterOffset+72+28:testFooterOffset+72+36], math.MaxInt64-RecordHeaderSize-0x10000000-7)
		}, "recordOffset 4294967288 plus the record's size"},
	}
}

// maxSegmentSize stands in for any footerOffset a real cap could not hold:
// the segment is only 416 bytes, so an offset that large is past the trailer
// (the segment's end minus its own 32 bytes), not past any bound of the
// format's own.
const maxSegmentSize = 1 << 40

// TestReadFooterNoFooter pins that every failure the format's reading list
// names — and every stricter refusal this reader adds — reads as "no footer",
// never an error of its own: each returns an error wrapping ErrNoFooter, the
// zero footer, and a message naming the rule that failed it. Every case is the
// golden segment under an edit, rechecksummed after the edit so only the
// edited rule can refuse it, never a stale checksum from a chain the edit did
// not touch.
func TestReadFooterNoFooter(t *testing.T) {
	segment := sealedSegment(t, []CatalogEntry{testEntryA, testEntryB}, 0)

	// The unedited segment reads: a case built on an unreadable segment
	// proves nothing.
	golden, err := ReadFooter(bytes.NewReader(segment), int64(len(segment)), testIdentity, testSegment)
	if err != nil || golden.Offset != testFooterOffset || len(golden.Entries) != 2 {
		t.Fatalf("ReadFooter over the golden segment = (%+v, %v), want the two entries at %d", golden, err, testFooterOffset)
	}

	for _, c := range noFooterCases() {
		seg := slices.Clone(segment)
		c.set(seg, seg[len(seg)-TrailerSize:])
		rechecksumFooter(seg)

		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, c.name, f, err, c.say)
	}

	// The two marker cases rebuild part of the segment: the marker is
	// covered by none of the trailer's checksums, so an edit there needs no
	// rechecksum, and a verifying marker that names another journal or
	// another segment is refused by the marker rule alone.
	for _, c := range []struct {
		name     string
		identity core.JournalID
		segment  core.SegmentID
		say      string
	}{
		{"seal marker under another identity", otherIdentity, testSegment, "no seal marker verifies"},
		{"seal marker for the previous segment id", testIdentity, testSegment - 1, "no seal marker verifies"},
	} {
		marker := AppendSealMarker(nil, c.identity, SealMarker{Segment: c.segment, Offset: testFooterOffset - SealMarkerSize})
		seg := slices.Concat(segment[:testFooterOffset-SealMarkerSize], marker, segment[testFooterOffset:])

		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, c.name, f, err, c.say)
	}

	// A read at the wrong segment id: the marker verifies only for the
	// segment it names, so a reader handed another id reads no footer.
	f, err := ReadFooter(bytes.NewReader(segment), int64(len(segment)), testIdentity, testSegment+1)
	assertNoFooter(t, "a segment id the marker does not name", f, err, "no seal marker verifies")

	// A trailer checksum slot that holds another value: the bytes it
	// covers are the golden's own, so no rechecksum is owed, and
	// rechecksumTrailer would restore what the case corrupts.
	{
		seg := slices.Clone(segment)
		seg[len(seg)-TrailerSize+28] ^= 0xff
		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, "trailer CRC wrong", f, err, "trailer checksum")
	}

	// A footer byte flipped with every checksum left stale: the trailer's
	// own CRC covers the trailer's 28 bytes and passes, and the footer's
	// checksum must be the rule that refuses it. Not rechecksummed, that
	// edit would be refused by any rule a rechecksum chain could reach.
	{
		seg := slices.Clone(segment)
		seg[testFooterOffset] ^= 0xff
		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, "footer CRC wrong", f, err, "footer checksum 0x09e7d833, want the CRC32C of 144 entry bytes: 0xcadde441")
	}

	// A trailer-only file: a valid 32-byte trailer that names the footer
	// the smallest one takes, which no longer fits below the 32 bytes a
	// trailer alone leaves. The trailer's own checks all read and pass, and
	// the fit rule refuses.
	{
		seg := AppendTrailer(nil, SegmentHeaderSize+SealMarkerSize, nil)
		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, "a file of exactly a trailer", f, err, "past the trailer at 0")
	}

	// An entry count that fits the room a full seal leaves but not the
	// trailer's boundary: the gap ends 32 bytes above the trailer, so the
	// three entries' 216 bytes would run into it and the fit rule must
	// refuse them there — one byte of gap in, and the entries would read
	// the trailer's own bytes for a checksum that then blames the footer.
	{
		seg := sealedSegment(t, []CatalogEntry{testEntryA, testEntryB}, 40)
		seg[len(seg)-TrailerSize+16] = 3 // entryCount
		rechecksumFooter(seg)
		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, "an entry count that fits the seal's room but not below the trailer", f, err, "entryCount 3 does not fit")
	}

	// A short file: no trailer to read.
	f, err = ReadFooter(bytes.NewReader(segment[:31]), 31, testIdentity, testSegment)
	assertNoFooter(t, "a file of 31 bytes", f, err, "31 bytes, shorter than the 32 a trailer takes")

	// A footer with no entry whose marker sits past the first record slot:
	// the records end at 48 and the marker names 56, so the extent check
	// refuses — a segment cannot hold a marker its own records have already
	// passed.
	{
		seg := make([]byte, 56)
		seg = AppendSealMarker(seg, testIdentity, SealMarker{Segment: testSegment, Offset: 56})
		seg = AppendTrailer(seg, 88, nil)
		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, "no entry and a seal marker past the first record slot", f, err, "last record ends at 48, the seal marker is at 56")
	}
}

// assertNoFooter checks one refusal: the error wraps ErrNoFooter, the footer
// is the zero one — no offset, no partial entry list — and the error names
// its own rule.
//
// The refusal also carries none of the record decoder's classes. The
// error classes are the only contract recovery matches on, and a footer
// refusal is "no footer", never damage, a wrong place or a kind or epoch
// of its own — so a recovery matching the classes in any order still ends
// at the scan this format asks for.
func assertNoFooter(t *testing.T, name string, f Footer, err error, say string) {
	t.Helper()

	if err == nil {
		t.Errorf("%s: ReadFooter returned (%+v, nil), want an error", name, f)
		return
	}
	if !errors.Is(err, ErrNoFooter) {
		t.Errorf("%s: ReadFooter = %v, want an error wrapping ErrNoFooter", name, err)
	}
	for _, class := range []error{ErrDamaged, ErrWrongPlace, ErrUnknownKind, ErrNonZeroEpoch} {
		if errors.Is(err, class) {
			t.Errorf("%s: ReadFooter = %v, want no %v wrapped in it: a footer refusal is never that class", name, err, class)
		}
	}
	if f.Offset != 0 || len(f.Entries) != 0 {
		t.Errorf("%s: ReadFooter returned %+v on refusal, want the zero footer", name, f)
	}
	if !strings.Contains(err.Error(), say) {
		t.Errorf("%s: ReadFooter = %v, want it to say %q", name, err, say)
	}
}

// TestReadFooterEntryKinds pins that a footer whose one entry is of each kind
// in the kinds table is read, not refused, its seal marker at the literal
// offset the kind's record ends at — the offsets are hard-coded from the
// format's tables, never computed with Size(), so a wrong body size fails
// here on its own literal. Where its kind has a length, the entry's is
// non-zero.
func TestReadFooterEntryKinds(t *testing.T) {
	// A record that starts at 48, the first record slot: its kind's body
	// decides the literal offset its record ends at.
	cases := []struct {
		name string
		kind Kind
		len  int64
		end  int64
	}{
		{"write of length 5", KindWrite, 5, 160},
		{"fill of length 4096", KindFill, 4096, 4240},
		{"release of length 4096", KindRelease, 4096, 144},
		{"truncate of length 0", KindTruncate, 0, 144},
		{"deallocate of length 4096", KindDeallocate, 4096, 144},
		{"delete of length 0", KindDelete, 0, 144},
		{"clone target of length 4096", KindCloneTarget, 4096, 200},
		{"offloaded of length 4096", KindOffloaded, 4096, 144},
		{"unmark of length 4096", KindUnmark, 4096, 144},
		{"loss of length 4096", KindLoss, 4096, 160},
		{"hold of length 0", KindHold, 0, 152},
		{"unhold of length 0", KindUnhold, 0, 152},
		{"stamp of length 0", KindStamp, 0, 152},
		{"forget of length 0", KindForget, 0, 144},
	}

	for _, c := range cases {
		e := testEntryA
		e.Kind = c.kind
		e.Extent = core.Extent{Off: testRecordFileOffset, Len: c.len}

		seg := sealedSegment(t, []CatalogEntry{e}, 0)
		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Errorf("%s: ReadFooter = %v, want no error", c.name, err)
			continue
		}
		if f.Offset != c.end+SealMarkerSize {
			t.Errorf("%s: footer offset %d, want %d, the literal seal offset %d plus 32", c.name, f.Offset, c.end+SealMarkerSize, c.end)
		}
		if len(f.Entries) != 1 || f.Entries[0] != e {
			t.Errorf("%s: ReadFooter gave %+v, want the one entry %+v", c.name, f.Entries, e)
		}
	}

	// A footer with no entry, its seal marker at 48.
	seg := sealedSegment(t, nil, 0)
	f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
	if err != nil {
		t.Fatalf("no entry: ReadFooter = %v, want no error", err)
	}
	if f.Offset != SegmentHeaderSize+SealMarkerSize || len(f.Entries) != 0 {
		t.Errorf("no entry: ReadFooter = (%+v), want no entries and offset %d", f, SegmentHeaderSize+SealMarkerSize)
	}
}

// TestReadFooterRefusesEveryEntryOfEveryUnknownKindInAnyPlace pins that an
// entry of a kind this format version does not define — the last entry or a
// middle one, of 0, the first reserved kind and the first extension's — makes
// the whole footer "no footer", never a partial entry list.
func TestReadFooterRefusesEveryEntryOfEveryUnknownKindInAnyPlace(t *testing.T) {
	for _, c := range []struct {
		name  string
		kind  uint8
		entry int
	}{
		{"the last entry of kind 0", 0, 1},
		{"the last entry of kind 15", 15, 1},
		{"the last entry of kind 128", 128, 1},
		{"the last entry of kind 255", 255, 1},
		{"a middle entry of kind 15", 15, 0},
	} {
		seg := sealedSegment(t, []CatalogEntry{testEntryA, testEntryB}, 0)
		seg[testFooterOffset+int64(c.entry)*CatalogEntrySize+60] = c.kind
		rechecksumFooter(seg)

		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		assertNoFooter(t, c.name, f, err, "unknown kind")
	}
}

// TestReadFooterFullSeal pins that the bytes between the footer and the
// trailer belong to neither: a full seal's gap — zeros, or a recycled
// segment's old life still on the device — never fails a footer read, and
// footerOffset still names the footer just past the marker, not the trailer.
func TestReadFooterFullSeal(t *testing.T) {
	for _, c := range []struct {
		name string
		gap  []byte
	}{
		{"zeros", make([]byte, 4000)},
		{"the old life's non-zero bytes", bytes.Repeat([]byte{0xa5}, 4000)},
	} {
		seg := sealedSegment(t, []CatalogEntry{testEntryA, testEntryB}, len(c.gap))
		copy(seg[testFooterOffset+144:], c.gap)

		f, err := ReadFooter(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if err != nil {
			t.Errorf("%s: ReadFooter = %v, want no error", c.name, err)
			continue
		}
		if f.Offset != testFooterOffset || len(f.Entries) != 2 {
			t.Errorf("%s: ReadFooter = (%d, %d entries), want the two entries at %d", c.name, f.Offset, len(f.Entries), testFooterOffset)
		}
	}
}

// readAtFailer is an io.ReaderAt whose read at one chosen offset fails with
// an injected error, and whose other reads serve the bytes.
type readAtFailer struct {
	b   []byte
	off int64
	err error
}

func (r readAtFailer) ReadAt(p []byte, off int64) (int, error) {
	if off == r.off {
		return 0, r.err
	}
	return copy(p, r.b[off:]), nil
}

// readAtEOFer is an io.ReaderAt behaving as the interface's contract allows
// at its last bytes: a full read that reaches the end returns both the whole
// buffer and io.EOF. bytes.Reader never operates that way, so no test feeding
// one can reach the branch readFull holds for such a reader.
type readAtEOFer struct{ b []byte }

func (r readAtEOFer) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, r.b[off:])
	if off+int64(n) == int64(len(r.b)) {
		return n, io.EOF
	}
	return n, nil
}

// readAtShort is an io.ReaderAt that breaks the interface's contract, serving
// one byte fewer than asked for and reporting no error: readFull must not
// mistake the short read for a whole one.
type readAtShort struct{ b []byte }

func (r readAtShort) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return copy(p, r.b[off:len(r.b)-1]), nil
}

// TestReadFooterReturnsReadErrorsAsThemselves pins that a read that fails is
// returned as itself, never as "no footer": an unreadable device is not
// evidence of damage and unreadable never counts as empty. The error reaches
// the caller wrapped but unwashed wherever it happens — at the trailer, at
// the seal marker or at the entries.
func TestReadFooterReturnsReadErrorsAsThemselves(t *testing.T) {
	segment := sealedSegment(t, []CatalogEntry{testEntryA, testEntryB}, 0)

	for _, c := range []struct {
		name string
		off  int64
	}{
		{"the trailer read", int64(len(segment)) - TrailerSize},
		{"the seal marker read", testFooterOffset - SealMarkerSize},
		{"the entries read", testFooterOffset},
	} {
		injected := io.ErrClosedPipe
		r := readAtFailer{b: segment, off: c.off, err: injected}

		f, err := ReadFooter(r, int64(len(segment)), testIdentity, testSegment)
		if !errors.Is(err, injected) {
			t.Errorf("%s: ReadFooter = %v, want the injected error", c.name, err)
		}
		if errors.Is(err, ErrNoFooter) {
			t.Errorf("%s: ReadFooter = %v, want no ErrNoFooter: an unreadable device is not evidence of damage", c.name, err)
		}
		if !strings.Contains(err.Error(), "read ") {
			t.Errorf("%s: ReadFooter = %v, want it to name the failed read", c.name, err)
		}
		if f.Offset != 0 || len(f.Entries) != 0 {
			t.Errorf("%s: ReadFooter returned %+v, want the zero footer", c.name, f)
		}
	}

	// A reader may return io.EOF with a full read at its last bytes — the
	// io.ReaderAt contract allows it — and the footer it verifies must
	// still read: the whole buffer reached the reader either way.
	{
		f, err := ReadFooter(readAtEOFer{segment}, int64(len(segment)), testIdentity, testSegment)
		if err != nil {
			t.Errorf("a reader returning io.EOF with a full read: ReadFooter = %v, want no error", err)
		}
		if f.Offset != testFooterOffset || len(f.Entries) != 2 {
			t.Errorf("a reader returning io.EOF with a full read: ReadFooter = (%+v), want the two entries at %d", f, testFooterOffset)
		}
	}

	// A reader serving too few bytes with no error breaks the interface's
	// contract; readFull still refuses to call the short read whole,
	// because a footer read from bytes that are not there is no footer at
	// all, and the caller needs to be told the bytes fell short rather
	// than handed a wrong verdict.
	{
		f, err := ReadFooter(readAtShort{segment}, int64(len(segment)), testIdentity, testSegment)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("a short quiet read: ReadFooter = %v, want io.ErrUnexpectedEOF", err)
		}
		if errors.Is(err, ErrNoFooter) {
			t.Errorf("a short quiet read: ReadFooter = %v, want no ErrNoFooter: bytes missing is not evidence of damage", err)
		}
		if f.Offset != 0 || len(f.Entries) != 0 {
			t.Errorf("a short quiet read: ReadFooter returned %+v, want the zero footer", f)
		}
	}
}

// TestAppendCatalogEntryPanics pins every condition the entry encoder refuses
// at the call — a caller bug, as the record encoder's conventions have it:
// what a writer could write and no reader could take back never reaches the
// disk. One case per rule, each over a known-good entry and each matched by
// its own message, so a case can never pass on an unrelated condition it
// triggers by accident.
func TestAppendCatalogEntryPanics(t *testing.T) {
	for _, c := range []struct {
		name string
		e    CatalogEntry
		want string
	}{
		{"an unknown kind", func() CatalogEntry {
			e := testEntryA
			e.Kind = Kind(15)
			return e
		}(), "unknown kind"},
		{"kind 0", func() CatalogEntry {
			e := testEntryA
			e.Kind = Kind(0)
			return e
		}(), "unknown kind"},
		{"kind 128", func() CatalogEntry {
			e := testEntryA
			e.Kind = Kind(128)
			return e
		}(), "unknown kind"},
		{"a non-zero version epoch", func() CatalogEntry {
			e := testEntryA
			e.Version = core.Version{Epoch: 1, Counter: e.Version.Counter}
			return e
		}(), "epoch"},
		{"a negative file offset", func() CatalogEntry {
			e := testEntryA
			e.Extent.Off = -1
			return e
		}(), "file offset -1"},
		{"a negative length", func() CatalogEntry {
			e := testEntryA
			e.Extent.Len = -1
			return e
		}(), "length -1"},
		{"a file offset that overflows with the length", func() CatalogEntry {
			e := testEntryA
			e.Extent.Off = math.MaxInt64
			e.Extent.Len = 1
			return e
		}(), "overflows"},
		{"a record offset below the first record slot", func() CatalogEntry {
			e := testEntryA
			e.RecordOffset = 40
			return e
		}(), "the first record slot"},
		{"a record offset not a multiple of 8", func() CatalogEntry {
			e := testEntryA
			e.RecordOffset++
			return e
		}(), "not a multiple of 8"},
		{"a record offset above MaxUint32", func() CatalogEntry {
			e := testEntryA
			e.RecordOffset = 1 << 32
			return e
		}(), "above MaxUint32"},
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("%s: AppendCatalogEntry returned; want a panic matching %q", c.name, c.want)
					return
				}
				if got := fmt.Sprint(r); !strings.Contains(got, c.want) {
					t.Errorf("%s: AppendCatalogEntry panicked with %q, want it to mention %q", c.name, got, c.want)
				}
			}()
			AppendCatalogEntry(nil, c.e)
		}()
	}
}

// TestAppendTrailerPanics pins every condition the trailer encoder refuses at
// the call, one case per rule, each matched by its own message.
//
// The count rule — len(footer)/72 must not top the 32-bit entryCount column —
// has no case here: reaching it takes a footer of 2^32 entries, over 309 GB,
// which no test builds.
func TestAppendTrailerPanics(t *testing.T) {
	footer := append(AppendCatalogEntry(nil, testEntryA), AppendCatalogEntry(nil, testEntryB)...)

	for _, c := range []struct {
		name         string
		footer       []byte
		footerOffset int64
		want         string
	}{
		{"a footer not a whole number of entries", footer[:71], testFooterOffset, "of 71 bytes, want a whole number of 72-byte entries"},
		{"footerOffset below the seal marker it needs", footer, 48, "footerOffset 48, below the first seal marker slot 80"},
		{"footerOffset not a multiple of 8", footer, testFooterOffset + 4, "is not a multiple of 8"},
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("%s: AppendTrailer returned; want a panic matching %q", c.name, c.want)
					return
				}
				if got := fmt.Sprint(r); !strings.Contains(got, c.want) {
					t.Errorf("%s: AppendTrailer panicked with %q, want it to mention %q", c.name, got, c.want)
				}
			}()
			AppendTrailer(nil, c.footerOffset, c.footer)
		}()
	}
}
