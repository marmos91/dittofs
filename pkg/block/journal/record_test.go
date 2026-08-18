package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// frameRecord builds a record's on-disk bytes exactly as appendRecord does,
// letting reader tests craft valid and deliberately torn segments.
func frameRecord(id string, offset int64, payload []byte, version uint64, flags uint8) []byte {
	fileID := []byte(id)
	hdr := encodeHeader(recordHeader{
		FileIDLen:  uint16(len(fileID)),
		FileOffset: uint64(offset),
		PayloadLen: uint32(len(payload)),
		Version:    version,
		Flags:      flags,
	}, fileID)
	var crcBuf [payloadCRCSize]byte
	binary.LittleEndian.PutUint32(crcBuf[:], recordCRC(fileID, payload))
	out := make([]byte, 0, len(hdr)+len(payload)+payloadCRCSize)
	out = append(out, hdr...)
	out = append(out, payload...)
	out = append(out, crcBuf[:]...)
	return out
}

// segmentBytes prefixes a header-sized region ahead of the framed record stream
// so offsets match a real segment (records begin at segHeaderSize).
func segmentBytes(records ...[]byte) []byte {
	seg := make([]byte, segHeaderSize)
	for _, r := range records {
		seg = append(seg, r...)
	}
	return seg
}

func TestReadRecordRoundTrip(t *testing.T) {
	rec := frameRecord("file-xyz", 4096, bytes.Repeat([]byte("payload"), 300), 7, flagSynced)
	seg := segmentBytes(rec)

	got, next, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, int64(len(seg)), nil)
	if err != nil {
		t.Fatalf("readRecordAt: %v", err)
	}
	if string(got.fileID) != "file-xyz" {
		t.Fatalf("fileID = %q", got.fileID)
	}
	if got.header.FileOffset != 4096 || got.header.Version != 7 || got.header.Flags&flagSynced == 0 {
		t.Fatalf("header mismatch: %+v", got.header)
	}
	if !bytes.Equal(got.payload, bytes.Repeat([]byte("payload"), 300)) {
		t.Fatalf("payload mismatch")
	}
	if got.segOff != segHeaderSize {
		t.Fatalf("segOff = %d", got.segOff)
	}
	if want := int64(len(seg)); next != want {
		t.Fatalf("next = %d want %d", next, want)
	}
}

func TestReadRecordTruncatedHeader(t *testing.T) {
	rec := frameRecord("f", 0, []byte("hello"), 1, 0)
	// Cut the record mid-header: only a few header bytes survive. A partial
	// header (n>0 then EOF) is a torn write, NOT a clean boundary.
	seg := segmentBytes(rec[:5])
	_, _, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, 1<<20, nil)
	if !errors.Is(err, errTornRecord) {
		t.Fatalf("expected errTornRecord on partial header, got %v", err)
	}
}

func TestReadRecordCleanEOF(t *testing.T) {
	rec := frameRecord("f", 0, []byte("hello"), 1, 0)
	seg := segmentBytes(rec)
	// Reading exactly at the end of the stream (zero bytes available) is a clean
	// boundary: io.EOF, distinct from a torn record.
	_, _, err := readRecordAt(bytes.NewReader(seg), int64(len(seg)), 1<<20, nil)
	if !errors.Is(err, io.EOF) || errors.Is(err, errTornRecord) {
		t.Fatalf("expected clean io.EOF at stream end, got %v", err)
	}
}

func TestReadRecordTruncatedPayload(t *testing.T) {
	rec := frameRecord("f", 0, bytes.Repeat([]byte("z"), 1000), 1, 0)
	// Drop the trailing payload + CRC: header is whole, body is torn.
	seg := segmentBytes(rec[:recordHeaderSize+1+100])
	_, _, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, 1<<20, nil)
	if !errors.Is(err, errTornRecord) {
		t.Fatalf("expected errTornRecord on truncated payload, got %v", err)
	}
}

func TestReadRecordCorruptPayload(t *testing.T) {
	rec := frameRecord("f", 0, bytes.Repeat([]byte("z"), 1000), 1, 0)
	seg := segmentBytes(rec)
	// Flip a payload byte in place: header still validates, payload CRC must not.
	seg[segHeaderSize+recordHeaderSize+1+10] ^= 0xFF
	_, _, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, 1<<20, nil)
	if !errors.Is(err, errTornRecord) {
		t.Fatalf("expected errTornRecord on corrupt payload, got %v", err)
	}
}

// TestReadRecordPayloadLenCeiling is the load-bearing torn-tail guard: a header
// whose CRC validates but whose PayloadLen is implausibly large must be rejected
// on the ceiling alone, before any allocation — CRC coincidence is not enough.
func TestReadRecordPayloadLenCeiling(t *testing.T) {
	fileID := []byte("f")
	// Frame a header claiming a 3 GiB payload; encodeHeader computes a VALID
	// header CRC for it, so only the ceiling can catch this.
	hdr := encodeHeader(recordHeader{
		FileIDLen:  uint16(len(fileID)),
		FileOffset: 0,
		PayloadLen: 3 << 30,
		Version:    1,
		Flags:      0,
	}, fileID)
	seg := segmentBytes(hdr) // no real payload follows

	_, _, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, 4<<20 /* 4 MiB ceiling */, nil)
	if !errors.Is(err, errTornRecord) {
		t.Fatalf("expected errTornRecord from PayloadLen ceiling, got %v", err)
	}
	// Confirm the header CRC itself was valid, i.e. the ceiling (not CRC) is what
	// rejected it.
	if _, derr := decodeHeader(hdr); derr != nil {
		t.Fatalf("crafted header should have a valid CRC, got %v", derr)
	}
}

func TestScanValidRecordsStopsAtTorn(t *testing.T) {
	r1 := frameRecord("a", 0, []byte("one"), 1, 0)
	r2 := frameRecord("b", 10, []byte("two"), 2, 0)
	r3 := frameRecord("c", 20, []byte("three"), 3, 0)
	torn := frameRecord("d", 30, bytes.Repeat([]byte("x"), 500), 4, 0)
	torn[recordHeaderSize+1+50] ^= 0xFF // corrupt r4's payload

	seg := segmentBytes(r1, r2, r3, torn)
	recs, validUpTo := scanValidRecords(bytes.NewReader(seg), int64(len(seg)), 1<<20)

	if len(recs) != 3 {
		t.Fatalf("expected 3 valid records before the torn one, got %d", len(recs))
	}
	wantUpTo := int64(segHeaderSize + len(r1) + len(r2) + len(r3))
	if validUpTo != wantUpTo {
		t.Fatalf("validUpTo = %d want %d (must be the start of the torn record)", validUpTo, wantUpTo)
	}
	for i, id := range []string{"a", "b", "c"} {
		if string(recs[i].fileID) != id {
			t.Fatalf("record %d fileID = %q want %q", i, recs[i].fileID, id)
		}
	}
}

func TestScanValidRecordsCleanEnd(t *testing.T) {
	r1 := frameRecord("a", 0, []byte("one"), 1, 0)
	r2 := frameRecord("b", 10, []byte("two"), 2, 0)
	seg := segmentBytes(r1, r2)
	recs, validUpTo := scanValidRecords(bytes.NewReader(seg), int64(len(seg)), 1<<20)
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if validUpTo != int64(len(seg)) {
		t.Fatalf("validUpTo = %d want %d", validUpTo, len(seg))
	}
}

// io.ReaderAt sanity: the reader must not mutate the source slice.
var _ io.ReaderAt = (*bytes.Reader)(nil)

// frameRecordV1 builds a record in the original framing: magic 0xD5 and a
// trailing CRC over the payload alone. Journals written before the FileID
// joined the CRC domain still hold records shaped like this.
func frameRecordV1(id string, offset int64, payload []byte, version uint64, flags uint8) []byte {
	fileID := []byte(id)
	out := frameRecord(id, offset, payload, version, flags)
	out[0] = recordMagicV1
	binary.LittleEndian.PutUint32(out[25:29], crc(out[:headerCRCCovers]))
	binary.LittleEndian.PutUint32(out[recordHeaderSize+len(fileID)+len(payload):], crc(payload))
	return out
}

func TestReadRecordAcceptsV1Framing(t *testing.T) {
	payload := bytes.Repeat([]byte("v1"), 64)
	seg := segmentBytes(frameRecordV1("legacy-file", 8192, payload, 3, flagSynced))

	got, next, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, int64(len(seg)), nil)
	if err != nil {
		t.Fatalf("readRecordAt on V1 record: %v", err)
	}
	if string(got.fileID) != "legacy-file" || got.header.FileOffset != 8192 || got.header.Version != 3 {
		t.Fatalf("V1 record decoded wrong: %+v id=%q", got.header, got.fileID)
	}
	if !bytes.Equal(got.payload, payload) {
		t.Fatalf("V1 payload mismatch")
	}
	if want := int64(len(seg)); next != want {
		t.Fatalf("next = %d want %d", next, want)
	}
}

func TestReadRecordCorruptFileID(t *testing.T) {
	rec := frameRecord("file-abc", 0, bytes.Repeat([]byte("z"), 64), 1, 0)
	// Flip a bit inside the framed FileID. The header CRC does not cover these
	// bytes, so only the body CRC can catch it.
	rec[recordHeaderSize+2] ^= 0x01
	seg := segmentBytes(rec)

	if _, _, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, 1<<20, nil); !errors.Is(err, errTornRecord) {
		t.Fatalf("expected errTornRecord on flipped FileID, got %v", err)
	}
}

func TestWrittenRecordsUseV2Framing(t *testing.T) {
	rec := frameRecord("f", 0, []byte("hello"), 1, 0)
	if rec[0] != recordMagicV2 {
		t.Fatalf("encodeHeader wrote magic 0x%02x, want 0x%02x", rec[0], recordMagicV2)
	}
}

// TestReadRecordV1CorruptFileIDUndetected pins the residual exposure the V2
// framing was introduced to close: in a V1 record no CRC covers the FileID, so
// a flipped byte there still verifies and the payload comes back attributed to
// whatever file the damaged bytes now spell. Records already on disk cannot be
// retroactively covered; repack re-frames them as V2 as it copies them forward.
func TestReadRecordV1CorruptFileIDUndetected(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 64)
	rec := frameRecordV1("file-abc", 0, payload, 1, 0)
	rec[recordHeaderSize+2] ^= 0x01
	seg := segmentBytes(rec)

	got, _, err := readRecordAt(bytes.NewReader(seg), segHeaderSize, 1<<20, nil)
	if err != nil {
		t.Fatalf("V1 framing unexpectedly rejected a flipped FileID: %v", err)
	}
	if string(got.fileID) == "file-abc" {
		t.Fatalf("FileID was not actually corrupted by the test")
	}
}
