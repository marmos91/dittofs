package raw

import (
	"encoding/binary"
	"testing"
)

// Offsets and sizes below are the record table's, written out here and never
// taken from the package's constants. Every test record carries its body's
// fields in their own byte ranges, so a field written a byte off never reads
// back as the right value by accident.

// testFill is a record with a fill body of 5 payload bytes 0xe0..0xe4.
var testFill = fillRecord()

func fillRecord() Record {
	r := testFullRecord
	r.Write = nil
	r.Kind = 2
	r.Length = 5
	r.Fill = &FillBody{Payload: []byte{0xe0, 0xe1, 0xe2, 0xe3, 0xe4}}
	return r
}

// TestFillBodyIsItsPayloadPinnedAndChecksummed pins that a fill's body is its
// payload alone at [96,...), with the modification-time bytes a write would
// carry nowhere in it, and that the payload checksum at [84,88) covers the
// payload and nothing else.
func TestFillBodyIsItsPayloadPinnedAndChecksummed(t *testing.T) {
	b := testFill.Bytes()

	if len(b) != 96+8 {
		t.Fatalf("a fill of 5 payload bytes is %d bytes, want 104", len(b))
	}
	if got := b[96:101]; string(got) != string(testFill.Fill.Payload) {
		t.Errorf("payload at [96,101) is % x, want % x", got, testFill.Fill.Payload)
	}
	if !zero(b[101:104]) {
		t.Errorf("padding [101,104) is % x, want zeros", b[101:104])
	}
	if got, want := le32(b[84:88]), wantSeededCRC(testIdentity, testFill.Fill.Payload); got != want {
		t.Errorf("payload checksum at [84,88) is 0x%08x, want the CRC32C of the identity then the payload: 0x%08x", got, want)
	}
}

// testLoss is a record with a loss body: dropped sequence number and flags,
// each in its own byte range.
var testLoss = lossRecord()

func lossRecord() Record {
	r := testFullRecord
	r.Write = nil
	r.Kind = 10
	r.Loss = &LossBody{Dropped: 0x8f8e8d8c8b8a8988, Flags: 0x0303}
	return r
}

// TestLossBodyIsDroppedThenFlags pins that a loss's 16-byte body is the
// dropped sequence number at [96,104) then the flag word at [104,112), both
// little-endian, and that the payload checksum covers both and nothing else.
func TestLossBodyIsDroppedThenFlags(t *testing.T) {
	b := testLoss.Bytes()

	if len(b) != 96+16 {
		t.Fatalf("a loss record is %d bytes, want 112", len(b))
	}
	if got := le64(b[96:104]); got != testLoss.Loss.Dropped {
		t.Errorf("dropped sequence number at [96,104) is %#x, want %#x", got, testLoss.Loss.Dropped)
	}
	if got := le64(b[104:112]); got != testLoss.Loss.Flags {
		t.Errorf("flag word at [104,112) is %#x, want %#x", got, testLoss.Loss.Flags)
	}

	body := binary.LittleEndian.AppendUint64(nil, testLoss.Loss.Dropped)
	body = binary.LittleEndian.AppendUint64(body, testLoss.Loss.Flags)
	if got, want := le32(b[84:88]), wantSeededCRC(testIdentity, body); got != want {
		t.Errorf("payload checksum at [84,88) is 0x%08x, want the CRC32C of the identity then the 16 body bytes: 0x%08x", got, want)
	}
}

// testClone is a record with a clone target body: source share, source file,
// source offset and AsOf, each in its own byte range.
var testClone = cloneRecord()

func cloneRecord() Record {
	r := testFullRecord
	r.Write = nil
	r.Kind = 7
	r.Clone = &CloneBody{
		Share: [16]byte{
			0xb0, 0xb1, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7,
			0xb8, 0xb9, 0xba, 0xbb, 0xbc, 0xbd, 0xbe, 0xbf,
		},
		File: [16]byte{
			0xc0, 0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7,
			0xc8, 0xc9, 0xca, 0xcb, 0xcc, 0xcd, 0xce, 0xcf,
		},
		Off:  0x2726252423222120,
		AsOf: Version{Epoch: 0, Counter: 0x9f9e9d9c9b9a9998},
	}
	return r
}

// TestCloneBodyIsShareFileOffsetThenAsOf pins that a clone target's 56-byte
// body is the source share at [96,112), the source file at [112,128), the
// source offset at [128,136) and AsOf as the epoch at [136,144) then the
// counter at [144,152) — two little-endian 64-bit halves, never one 128-bit
// integer — and that the payload checksum covers all 56 bytes.
func TestCloneBodyIsShareFileOffsetThenAsOf(t *testing.T) {
	b := testClone.Bytes()

	if len(b) != 96+56 {
		t.Fatalf("a clone target record is %d bytes, want 152", len(b))
	}
	if got := [16]byte(b[96:112]); got != testClone.Clone.Share {
		t.Errorf("source share at [96,112) is % x, want % x", got, testClone.Clone.Share)
	}
	if got := [16]byte(b[112:128]); got != testClone.Clone.File {
		t.Errorf("source file at [112,128) is % x, want % x", got, testClone.Clone.File)
	}
	if got := le64(b[128:136]); got != testClone.Clone.Off {
		t.Errorf("source offset at [128,136) is %#x, want %#x", got, testClone.Clone.Off)
	}
	if got := le64(b[136:144]); got != testClone.Clone.AsOf.Epoch {
		t.Errorf("AsOf epoch at [136,144) is %#x, want %#x", got, testClone.Clone.AsOf.Epoch)
	}
	if got := le64(b[144:152]); got != testClone.Clone.AsOf.Counter {
		t.Errorf("AsOf counter at [144,152) is %#x, want %#x", got, testClone.Clone.AsOf.Counter)
	}

	body := append(testClone.Clone.Share[:], testClone.Clone.File[:]...)
	body = binary.LittleEndian.AppendUint64(body, testClone.Clone.Off)
	body = binary.LittleEndian.AppendUint64(body, testClone.Clone.AsOf.Epoch)
	body = binary.LittleEndian.AppendUint64(body, testClone.Clone.AsOf.Counter)
	if got, want := le32(b[84:88]), wantSeededCRC(testIdentity, body); got != want {
		t.Errorf("payload checksum at [84,88) is 0x%08x, want the CRC32C of the identity then the 56 body bytes: 0x%08x", got, want)
	}
}

// testStamp is a record with the 8-byte body a hold, unhold and stamp record
// share: the cut number.
var testStamp = stampRecord()

func stampRecord() Record {
	r := testFullRecord
	r.Write = nil
	r.Kind = 13
	r.Cut = &CutBody{Cut: 0xefeeedecebeae9e8}
	return r
}

// TestCutBodyIsTheCutNumber pins that a stamp record's body is the cut number
// alone at [96,104), that a record whose body is 8 bytes needs no padding, and
// that the payload checksum covers the cut alone.
func TestCutBodyIsTheCutNumber(t *testing.T) {
	b := testStamp.Bytes()

	if len(b) != 96+8 {
		t.Fatalf("a stamp record is %d bytes, want 104", len(b))
	}
	if got := le64(b[96:104]); got != testStamp.Cut.Cut {
		t.Errorf("cut number at [96,104) is %#x, want %#x", got, testStamp.Cut.Cut)
	}

	body := binary.LittleEndian.AppendUint64(nil, testStamp.Cut.Cut)
	if got, want := le32(b[84:88]), wantSeededCRC(testIdentity, body); got != want {
		t.Errorf("payload checksum at [84,88) is 0x%08x, want the CRC32C of the identity then the cut: 0x%08x", got, want)
	}
}

// TestTwoBodiesSetPanics pins that a record holding two bodies is a caller's
// bug, so Bytes panics rather than writing a body out of a guess.
func TestTwoBodiesSetPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("Bytes with two bodies set returned; want a panic")
		}
	}()
	r := testLoss
	r.Cut = &CutBody{Cut: 1}
	r.Bytes()
}
