package raw

import (
	"encoding/binary"
	"testing"
)

// Offsets and sizes below are the record header table's, written out here and
// never taken from the package's constants.

// testFullRecord is a record holding every field a header can carry: the
// synced-through offset, the own offset and a write body. Every field of it,
// header and body, reads back from its own range of byte values out of the
// record's table with the test values — the synced-through offset's 0xd1-0xd8
// and the own offset's 0xf0-0xf3 among them, which only a record carrying the
// two has — so a field written a byte off, or in another field's place, reads
// back as bytes from the wrong range, never as the right value by accident.
// The body kinds' ranges live with the records that carry them, in
// record_bodies_test.
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
	SyncedThrough: 0xd8d7d6d5d4d3d2d1,
	At:            0xf3f2f1f0,
	Write:         &WriteBody{MTime: testMTime, Payload: []byte{0x91, 0x92, 0x93}},
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
