package raw

import (
	"encoding/binary"
	"testing"
)

// Offsets and sizes below are the record header table's, written out here and
// never taken from the package's constants.

// testFullRecord is a record holding every field a header can carry: the
// synced-through offset, the own offset and a write body. Its byte ranges are
// the payload's and the catalog entry's own, so a field written a byte off
// reads back as bytes from the wrong range, never as the right value by
// accident.
//
//	0x41-0x48  synced-through offset   0xd9-0xdc  own offset
//	0x31-0x38  clone source offset     0x8f-0x98  loss body
//	0xb0-0xbf  clone source share      0xef-0xf6  cut number
//	0xc0-0xcf  clone source file       0xe0-0xf4  fill payload
var testFullRecord = Record{
	Identity: testIdentity,
	Kind:     0xc5,
	Segment:  testSegment,
	FileID: [16]byte{
		0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
		0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
	},
	Offset:        0x4847464544434241,
	Length:        0x5857565554535251,
	Seq:           0x6867666564636261,
	Version:       Version{Epoch: 0xa8a7a6a5a4a3a2a1, Counter: 0xb8b7b6b5b4b3b2b1},
	Tag:           0x84838281,
	SyncedThrough: 0x4142434445464748,
	At:            0x484a4b4c,
	Write:         &WriteBody{MTime: testMTime, Payload: []byte{0x91, 0x92, 0x93}},
	// Write:         nil, // set by the tests that need a kind's body
}

// TestFullRecordCarriesTheSyncedThroughOffsetAndOwnOffset pins that a record
// header carries the stream's synced-through offset at [76,84) and the
// record's own offset in its segment at [88,92), both as stored.
func TestFullRecordCarriesTheSyncedThroughOffsetAndOwnOffset(t *testing.T) {
	b := testFullRecord.Bytes()

	if got := le64(b[76:84]); got != testFullRecord.SyncedThrough {
		t.Errorf("synced-through offset at [76,84) is %#x, want %#x", got, testFullRecord.SyncedThrough)
	}
	if got := le32(b[88:92]); got != testFullRecord.At {
		t.Errorf("own offset at [88,92) is %#x, want %#x", got, testFullRecord.At)
	}
}

// TestWriteBodyOffsetsPinsFieldLayout pins that the write body's modification
// time lands at the header's end and the payload after it, under the payload
// checksum at [84,88).
func TestWriteBodyOffsetsPinsFieldLayout(t *testing.T) {
	b := testFullRecord.Bytes()

	body := binary.LittleEndian.AppendUint64(nil, uint64(testMTime))
	body = append(body, testFullRecord.Write.Payload...)
	if got := le32(b[84:88]); got != wantSeededCRC(testIdentity, body) {
		t.Errorf("payload checksum at [84,88) is 0x%08x, want the CRC32C of the identity then the body: 0x%08x", got, wantSeededCRC(testIdentity, body))
	}
	if got := le64(b[96:104]); got != uint64(testMTime) {
		t.Errorf("modification time at [96,104) is %#x, want %#x", got, uint64(testMTime))
	}
	if got := b[104:107]; string(got) != string(testFullRecord.Write.Payload) {
		t.Errorf("payload at [104,107) is % x, want % x", got, testFullRecord.Write.Payload)
	}
}
