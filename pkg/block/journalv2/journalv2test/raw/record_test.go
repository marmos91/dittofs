package raw

import (
	"encoding/binary"
	"testing"
)

// Offsets and sizes below are the record header table's, written out here and
// never taken from the package's constants.

func TestRecordHeaderLayout(t *testing.T) {
	b := testRecord.Bytes()

	if got := string(b[0:4]); got != "DJR1" {
		t.Errorf("magic at [0,4) is %q, want %q", got, "DJR1")
	}

	if got := b[4]; got != testRecord.Kind {
		t.Errorf("kind at [4] is %#x, want %#x", got, testRecord.Kind)
	}

	if !zero(b[5:8]) {
		t.Errorf("reserved [5,8) is % x, want zeros", b[5:8])
	}

	if got := le64(b[8:16]); got != testRecord.Segment {
		t.Errorf("segment id at [8,16) is %#x, want %#x", got, testRecord.Segment)
	}

	if got := [16]byte(b[16:32]); got != testRecord.FileID {
		t.Errorf("FileID at [16,32) is % x, want % x", got, testRecord.FileID)
	}

	if got := le64(b[32:40]); got != testRecord.Offset {
		t.Errorf("file offset at [32,40) is %#x, want %#x", got, testRecord.Offset)
	}

	if got := le64(b[40:48]); got != testRecord.Length {
		t.Errorf("length at [40,48) is %#x, want %#x", got, testRecord.Length)
	}

	if got := le64(b[48:56]); got != testRecord.Seq {
		t.Errorf("sequence number at [48,56) is %#x, want %#x", got, testRecord.Seq)
	}

	if got := le64(b[56:64]); got != testRecord.Version.Epoch {
		t.Errorf("version epoch at [56,64) is %#x, want %#x", got, testRecord.Version.Epoch)
	}

	if got := le64(b[64:72]); got != testRecord.Version.Counter {
		t.Errorf("version counter at [64,72) is %#x, want %#x", got, testRecord.Version.Counter)
	}

	if got := le32(b[72:76]); got != testRecord.Tag {
		t.Errorf("share tag at [72,76) is 0x%08x, want 0x%08x", got, testRecord.Tag)
	}

	if got := le64(b[76:84]); got != 0 {
		t.Errorf("synced-through offset at [76,84) is %#x, want 0", got)
	}

	if !zero(b[88:92]) {
		t.Errorf("reserved [88,92) is % x, want zeros", b[88:92])
	}
}

func TestRecordHeaderChecksumIsSeededOverItsFirst92Bytes(t *testing.T) {
	b := testRecord.Bytes()

	if got, want := le32(b[92:96]), wantSeededCRC(testIdentity, b[0:92]); got != want {
		t.Errorf("header checksum at [92,96) is 0x%08x, want the CRC32C of the identity then [0,92): 0x%08x", got, want)
	}
}

func TestWritePayloadChecksumIsSeededOverTheTimeThenThePayload(t *testing.T) {
	r, p := testWrite(13)
	b := r.Bytes()

	body := binary.LittleEndian.AppendUint64(nil, uint64(testMTime))
	body = append(body, p...)
	if got, want := le32(b[84:88]), wantSeededCRC(testIdentity, body); got != want {
		t.Errorf("payload checksum at [84,88) is 0x%08x, want the CRC32C of the identity, the time, then the payload: 0x%08x", got, want)
	}
}

// Seeded is the identity followed by the bytes covered, and a record with no
// body covers none.
func TestBodilessPayloadChecksumIsTheCRC32COfTheIdentity(t *testing.T) {
	b := testRecord.Bytes()

	if got, want := le32(b[84:88]), wantCRC(testIdentity[:]); got != want {
		t.Errorf("payload checksum at [84,88) is 0x%08x, want the CRC32C of the identity alone: 0x%08x", got, want)
	}
}

// 96 header bytes, 8 of time and 13 of payload make 117, padded to 120.
func TestWriteBodyFollowsTheHeaderAndIsPaddedTo8(t *testing.T) {
	r, p := testWrite(13)
	b := r.Bytes()

	if len(b) != 120 {
		t.Fatalf("a write of 13 payload bytes is %d bytes, want 120", len(b))
	}

	if got := le64(b[96:104]); got != uint64(testMTime) {
		t.Errorf("modification time at [96,104) is %#x, want %#x", got, testMTime)
	}

	if got := b[104:117]; string(got) != string(p) {
		t.Errorf("payload at [104,117) is % x, want % x", got, p)
	}

	if !zero(b[117:120]) {
		t.Errorf("padding [117,120) is % x, want zeros", b[117:120])
	}
}

// 96 header bytes, 8 of time and 16 of payload make 120, already a multiple
// of 8.
func TestWriteOfWholeWordsGetsNoPadding(t *testing.T) {
	r, _ := testWrite(16)

	if got := len(r.Bytes()); got != 120 {
		t.Errorf("a write of 16 payload bytes is %d bytes, want 120", got)
	}
}

// The exported offsets are the tables' values, so a check that damages one
// field through them damages that field.
func TestRecordOffsets(t *testing.T) {
	if RecordLengthOffset != 40 {
		t.Errorf("RecordLengthOffset is %d, want 40", RecordLengthOffset)
	}
}
