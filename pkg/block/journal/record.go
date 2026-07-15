package journal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
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
