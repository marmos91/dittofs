package format

import (
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"os"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Test values. Within one structure every field gets its own range of byte
// values, so a field written a byte off, big-endian or in another field's
// place reads back as bytes from the wrong range, never as the right value by
// accident. The identity and the installation use the byte ranges raw's tests
// use, so the golden file and raw's bytes agree field for field.
var testIdentity = core.JournalID{
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

var testInstallation = core.InstallationID{
	0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
	0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
}

// testFormatFile is the format file the golden file holds.
var testFormatFile = FormatFile{Identity: testIdentity, Installation: testInstallation}

// otherIdentity is a second journal's identity: a segment naming it is that
// journal's, not damaged. Its bytes are in no other header field's range.
var otherIdentity = core.JournalID{
	0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97,
	0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f,
}

// The segment header's test values, one range of bytes per field, raw's.
var (
	testSegment            = core.SegmentID(0x0807060504030201)
	testPredecessor        = core.SegmentID(0x3837363534333231)
	testStream      uint32 = 0x44434241
)

// testSegmentHeader is the header the pred golden holds; with a zero
// predecessor it is the nopred golden's. The identity is testIdentity in
// both, so a decode with testIdentity succeeds and one with otherIdentity
// reports foreign.
var testSegmentHeader = SegmentHeader{
	Identity:    testIdentity,
	ID:          testSegment,
	Predecessor: testPredecessor,
	Stream:      testStream,
}

// testOffset is the seal marker's own offset in the segment: raw's test
// value, a byte range no other field's.
const testOffset int64 = 0x2827262524232221

// testSealMarker is the marker the seal golden holds: it seals testSegment
// at testOffset, written under testIdentity, so a decode under that
// identity, at that segment and offset, succeeds and one under any other
// does not.
var testSealMarker = SealMarker{Segment: testSegment, Offset: testOffset}

// The record's test values, one byte-value pattern per field, raw's own
// where the two share a field: every 8-byte integer counts down from a
// distinctive hex pattern and every array holds one run of bytes, so a field
// written a byte off, big-endian or in another field's place reads back as
// bytes from the wrong pattern, never as the right value by accident. The
// clone source file and the write payload share 0xc0.., the only two fields
// that can: they never meet in one record, and neither is ever compared with
// the other.
var (
	// testRecordFile is the FileID every record golden but the unhold
	// and forget ones carries.
	testRecordFile = core.NewFileID([16]byte{
		0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
		0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf,
	})

	// testRecordOffset is the record's own offset in its segment: a
	// multiple of 8, as a record slot is.
	testRecordOffset int64 = 0xdbdad9d8

	// testRecordFileOffset is the file offset every kind with an offset
	// carries in its golden.
	testRecordFileOffset int64 = 0x3736353433323130

	// testRecordLength is the length of the release, deallocate, clone
	// target, offloaded, unmark and loss goldens.
	testRecordLength int64 = 0x4746454443424140

	// testRecordSeq is the sequence number every record golden carries.
	testRecordSeq = core.Seq(0x5756555453525150)

	// testRecordVersion is the content version every record golden carries,
	// with the epoch half zero as this format version requires.
	testRecordVersion = core.Version{Epoch: 0, Counter: 0x6766656463626160}

	// testRecordTag is the share tag every record golden carries.
	testRecordTag = core.ShareTag(0x73727170)

	// testRecordSyncedThrough is the synced-through offset every record
	// golden carries.
	testRecordSyncedThrough int64 = 0x4f4e4d4c4b4a4948

	// testWriteMTime is the modification time of the write golden.
	testWriteMTime int64 = 0x6f6e6d6c6b6a6968

	// testWritePayload is the 13 payload bytes of the write golden.
	testWritePayload = []byte{
		0xc0, 0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7,
		0xc8, 0xc9, 0xca, 0xcb, 0xcc,
	}

	// testFillPayload is the 21 payload bytes of the fill golden.
	testFillPayload = []byte{
		0xe0, 0xe1, 0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7,
		0xe8, 0xe9, 0xea, 0xeb, 0xec, 0xed, 0xee, 0xef,
		0xf0, 0xf1, 0xf2, 0xf3, 0xf4,
	}

	// testLoss is the loss body of the loss golden: a dropped sequence
	// number, and flags naming a synced drop of an offloaded record for
	// reason 3.
	testLoss = Loss{
		Dropped:   core.Seq(0x8f8e8d8c8b8a8988),
		Synced:    true,
		Offloaded: true,
		Reason:    LossFailedSync,
	}

	// testClone is the clone source of the clone target golden.
	testClone = CloneSource{
		Share: core.ShareID{
			0xb0, 0xb1, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7,
			0xb8, 0xb9, 0xba, 0xbb, 0xbc, 0xbd, 0xbe, 0xbf,
		},
		File: core.NewFileID([16]byte{
			0xc0, 0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7,
			0xc8, 0xc9, 0xca, 0xcb, 0xcc, 0xcd, 0xce, 0xcf,
		}),
		Off:  0x2726252423222120,
		AsOf: core.Version{Epoch: 0, Counter: 0x9f9e9d9c9b9a9998},
	}

	// testCut is the cut number of the hold, unhold and stamp goldens.
	testCut = core.SnapshotCut(0xefeeedecebeae9e8)
)

// wantWriteBody is the write body the write golden holds: the bytes its
// checksum covers, the modification time then the payload. The other bodies'
// byte strings live with the tests that build them.
func wantWriteBody() []byte {
	b := binary.LittleEndian.AppendUint64(nil, uint64(testWriteMTime))
	return append(b, testWritePayload...)
}

// rechecksumRecord stores the seeded CRC32C of identity then [92,96)'s
// covered bytes at [92,96) of a record header, so an edit fails a decode only
// for the edit, not for its stale checksum.
func rechecksumRecord(b []byte, identity core.JournalID) {
	binary.LittleEndian.PutUint32(b[92:96], wantSeededCRC(identity, b[0:92]))
}

// rechecksumRecordBody stores the seeded CRC32C of identity then the body's
// bytes at [84,88) of a record header and then redoes the header checksum,
// so an edit to a body, its length or its checksum fails a verify only for
// the edit, not for a stale checksum.
func rechecksumRecordBody(b []byte, identity core.JournalID, bodyLen int) {
	binary.LittleEndian.PutUint32(b[84:88], wantSeededCRC(identity, b[96:96+bodyLen]))
	rechecksumRecord(b, identity)
}

// readGolden reads testdata/golden/<name>.hex: # to end of line a comment,
// whitespace ignored, the rest hex. It takes the interface of both testing.T
// and testing.F, so a fuzz test's seeds read it too.
func readGolden(t testing.TB, name string) []byte {
	t.Helper()

	data, err := os.ReadFile("testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}

	// A comment is cut line by line, so a # never hides digits after it,
	// and whitespace is dropped so the digits of a line join.
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, r := range line {
			if !strings.ContainsRune(" \t\r", r) {
				digits.WriteRune(r)
			}
		}
	}
	if digits.Len()%2 != 0 {
		t.Fatalf("golden %s holds an odd number of hex digits", name)
	}
	b := make([]byte, digits.Len()/2)
	if _, err := hex.Decode(b, []byte(digits.String())); err != nil {
		t.Fatalf("decode golden %s: %v", name, err)
	}
	return b
}

// wantCRC is the standard library's CRC32C of b, computed here and not by
// the package, so a test never checks the codec against itself.
func wantCRC(b []byte) uint32 {
	return crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
}

// rechecksum stores the CRC32C of [0,52) at [52,56) of a format file, so an
// edit fails a decode only for the edit, not for its stale checksum.
func rechecksum(b []byte) {
	binary.LittleEndian.PutUint32(b[52:56], wantCRC(b[0:52]))
}

// rechecksumHeader stores the CRC32C of [0,44) at [44,48) of a segment
// header, so an edit fails a decode only for the edit, not for its stale
// checksum.
func rechecksumHeader(b []byte) {
	binary.LittleEndian.PutUint32(b[44:48], wantCRC(b[0:44]))
}

// wantSeededCRC is the standard library's CRC32C of identity followed by b,
// computed here and not by the package, on a fresh slice, so a test never
// checks the codec against itself.
func wantSeededCRC(identity core.JournalID, b []byte) uint32 {
	return crc32.Checksum(append(identity[:], b...), crc32.MakeTable(crc32.Castagnoli))
}

// rechecksumSeal stores the CRC32C of identity then [0,28) at [28,32) of a
// seal marker, so an edit fails a decode only for the edit, not for its
// stale checksum.
func rechecksumSeal(b []byte, identity core.JournalID) {
	binary.LittleEndian.PutUint32(b[28:32], wantSeededCRC(identity, b[0:28]))
}
