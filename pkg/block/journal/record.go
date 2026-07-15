package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Record framing. Many files pack into one shared segment, so each record
// carries its own FileID, logical FileOffset and a store-wide Version (LSN) so
// newest-wins stays well-defined after GC physically relocates records.
//
// On-disk header layout (recordHeaderSize bytes), little-endian:
//
//	off  size  field
//	0    1     MagicByte     0xD5, torn-write scan anchor
//	1    1     HeaderLen     currently recordHeaderSize
//	2    2     FileIDLen
//	4    8     FileOffset
//	12   4     PayloadLen
//	16   8     Version
//	24   1     Flags         bit0=synced, bit1=tombstone
//	25   4     HeaderCRC32   covers bytes [0,24) — deliberately EXCLUDES Flags
//
// then FileIDLen bytes of FileID, PayloadLen bytes of payload, then a trailing
// PayloadCRC32 (4 bytes) over the payload only.
//
// HeaderCRC32 excluding Flags is load-bearing: carve completion flips the
// synced bit in place with a single one-byte pwrite without invalidating the
// header CRC or rewriting the record.
const (
	recordMagic      = 0xD5
	recordHeaderSize = 29 // Magic..HeaderCRC32 inclusive
	headerCRCCovers  = 24 // Magic..Version, excludes Flags
	payloadCRCSize   = 4

	// recordFlagsOffset is the byte offset of the Flags field within a record
	// header. Carve flips the synced bit here with a single one-byte pwrite; it
	// equals headerCRCCovers because the CRC deliberately covers only the bytes
	// before Flags, so the flip never invalidates the header CRC.
	recordFlagsOffset = headerCRCCovers

	flagSynced    = 1 << 0
	flagTombstone = 1 << 1
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func crc(b []byte) uint32 { return crc32.Checksum(b, crcTable) }

// recordHeader is the decoded fixed-size portion of a record.
type recordHeader struct {
	FileIDLen  uint16
	FileOffset uint64
	PayloadLen uint32
	Version    uint64
	Flags      uint8
}

// recordLen is the total on-disk size of a record with the given ID and
// payload lengths.
func recordLen(fileIDLen, payloadLen int) int64 {
	return int64(recordHeaderSize) + int64(fileIDLen) + int64(payloadLen) + payloadCRCSize
}

// encodeHeader serializes the fixed header plus the FileID bytes into a single
// small buffer. The payload and its trailing CRC are written separately so a
// large payload is never copied.
func encodeHeader(h recordHeader, fileID []byte) []byte {
	buf := make([]byte, recordHeaderSize+len(fileID))
	buf[0] = recordMagic
	buf[1] = recordHeaderSize
	binary.LittleEndian.PutUint16(buf[2:4], h.FileIDLen)
	binary.LittleEndian.PutUint64(buf[4:12], h.FileOffset)
	binary.LittleEndian.PutUint32(buf[12:16], h.PayloadLen)
	binary.LittleEndian.PutUint64(buf[16:24], h.Version)
	buf[24] = h.Flags
	binary.LittleEndian.PutUint32(buf[25:29], crc(buf[:headerCRCCovers]))
	copy(buf[recordHeaderSize:], fileID)
	return buf
}

// decodeHeader parses and validates a fixed record header. It checks the magic
// byte, header length and header CRC (which excludes Flags). It does not read
// the FileID or payload.
func decodeHeader(buf []byte) (recordHeader, error) {
	if len(buf) < recordHeaderSize {
		return recordHeader{}, fmt.Errorf("journal: short record header: %d bytes", len(buf))
	}
	if buf[0] != recordMagic {
		return recordHeader{}, fmt.Errorf("journal: bad record magic 0x%02x", buf[0])
	}
	if buf[1] != recordHeaderSize {
		return recordHeader{}, fmt.Errorf("journal: unsupported header len %d", buf[1])
	}
	want := binary.LittleEndian.Uint32(buf[25:29])
	if got := crc(buf[:headerCRCCovers]); got != want {
		return recordHeader{}, fmt.Errorf("journal: header CRC mismatch: got %08x want %08x", got, want)
	}
	return recordHeader{
		FileIDLen:  binary.LittleEndian.Uint16(buf[2:4]),
		FileOffset: binary.LittleEndian.Uint64(buf[4:12]),
		PayloadLen: binary.LittleEndian.Uint32(buf[12:16]),
		Version:    binary.LittleEndian.Uint64(buf[16:24]),
		Flags:      buf[24],
	}, nil
}

// errTornRecord marks a record that fails structural validation: bad magic,
// header CRC, an implausible PayloadLen, a payload that runs past the written
// bytes, or a payload CRC mismatch. A recovery tail-scan stops at the first
// torn record and truncates there. A clean end-of-data (nothing left to read)
// surfaces as io.EOF, distinct from corruption.
var errTornRecord = errors.New("journal: torn record")

// record is a fully decoded and CRC-verified record read back from a segment.
type record struct {
	header  recordHeader
	fileID  []byte
	payload []byte
	segOff  int64 // record start offset within the segment
}

// readRecordAt reads and fully validates the record at segment offset off.
//
// It is the single point that decides a record is trustworthy: it validates the
// header CRC, then rejects any PayloadLen above maxPayload BEFORE allocating so
// a header CRC that validates by coincidence on a torn tail can never make the
// reader trust a bogus length and blow up memory, then verifies the payload CRC.
// A torn or corrupt record returns errTornRecord; a clean boundary (no bytes
// left) returns io.EOF. On success it also returns the offset of the next
// record so a scan can advance.
func readRecordAt(r io.ReaderAt, off, maxPayload int64) (record, int64, error) {
	var hdrBuf [recordHeaderSize]byte
	n, err := r.ReadAt(hdrBuf[:], off)
	if err != nil {
		// Only a zero-byte read at off is a clean end of the record stream. A
		// partial header (n>0) is a torn write at the tail, not a boundary.
		if errors.Is(err, io.EOF) && n == 0 {
			return record{}, 0, io.EOF
		}
		return record{}, 0, fmt.Errorf("%w: header read: %v", errTornRecord, err)
	}
	h, err := decodeHeader(hdrBuf[:])
	if err != nil {
		return record{}, 0, fmt.Errorf("%w: %v", errTornRecord, err)
	}
	if int64(h.PayloadLen) > maxPayload {
		return record{}, 0, fmt.Errorf("%w: payload len %d exceeds ceiling %d", errTornRecord, h.PayloadLen, maxPayload)
	}
	body := int64(h.FileIDLen) + int64(h.PayloadLen) + payloadCRCSize
	// Reject a length that would not survive the int64->int narrowing make does
	// on 32-bit platforms, so a corrupt header can never overflow or panic here.
	if int64(int(body)) != body {
		return record{}, 0, fmt.Errorf("%w: record length %d out of range", errTornRecord, body)
	}
	buf := make([]byte, body)
	if _, err := r.ReadAt(buf, off+recordHeaderSize); err != nil {
		// A truncated payload (torn write) shows up as EOF here, mid-record —
		// corruption, not a clean boundary.
		return record{}, 0, fmt.Errorf("%w: body read: %v", errTornRecord, err)
	}
	payloadEnd := int64(h.FileIDLen) + int64(h.PayloadLen)
	payload := buf[h.FileIDLen:payloadEnd]
	wantCRC := binary.LittleEndian.Uint32(buf[payloadEnd:])
	if got := crc(payload); got != wantCRC {
		return record{}, 0, fmt.Errorf("%w: payload CRC mismatch", errTornRecord)
	}
	return record{
		header:  h,
		fileID:  buf[:h.FileIDLen],
		payload: payload,
		segOff:  off,
	}, off + recordHeaderSize + body, nil
}

// scanValidRecords walks a segment's record stream from the first record to the
// first torn record or clean end, returning the valid records and the offset up
// to which the segment is intact. Recovery replays the records and truncates the
// segment at validUpTo. maxPayload is the per-record sanity ceiling.
func scanValidRecords(r io.ReaderAt, segSize, maxPayload int64) (recs []record, validUpTo int64) {
	off := int64(segHeaderSize)
	for off < segSize {
		rec, next, err := readRecordAt(r, off, maxPayload)
		if err != nil {
			break
		}
		recs = append(recs, rec)
		off = next
	}
	return recs, off
}
