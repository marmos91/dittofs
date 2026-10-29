package raw

import "encoding/binary"

// RecordHeaderSize is the size of a record header.
const RecordHeaderSize = 96

// RecordLengthOffset is the offset of the length field in a record header,
// for a check that damages that field alone.
const RecordLengthOffset = 40

// WriteMTimeOffset is the offset of a write record's modification time from
// the record's first byte: the 8 bytes just past the header, before the
// payload.
const WriteMTimeOffset = RecordHeaderSize

// Record is one record: its 96-byte header, its body and the zero padding
// that brings it to a multiple of 8 bytes. Both checksums are seeded with
// Identity, which is not written.
//
// decision: the synced-through offset is written as 0, because no check
// plants a record that must prove an earlier one synced. Make it a field when
// one does.
type Record struct {
	Identity [16]byte
	Kind     uint8
	Segment  uint64 // the segment the record is written into
	FileID   [16]byte
	Offset   uint64 // file offset
	Length   uint64 // written as given, for every kind
	Seq      uint64 // sequence number
	Version  Version
	Tag      uint32 // share tag
	Write    *WriteBody
}

// WriteBody is a write record's body: the modification time, then the
// payload. The payload checksum covers both.
type WriteBody struct {
	MTime   int64 // nanoseconds since the Unix epoch, or 0 when suspended
	Payload []byte
}

// Bytes returns the record's header, body and padding. A record with no body
// is its header alone.
func (r Record) Bytes() []byte {
	var body []byte
	if r.Write != nil {
		body = binary.LittleEndian.AppendUint64(body, uint64(r.Write.MTime))
		body = append(body, r.Write.Payload...)
	}

	b := make([]byte, RecordHeaderSize)
	copy(b[0:4], "DJR1")

	b[4] = r.Kind
	binary.LittleEndian.PutUint64(b[8:16], r.Segment)
	copy(b[16:32], r.FileID[:])

	binary.LittleEndian.PutUint64(b[32:40], r.Offset)
	binary.LittleEndian.PutUint64(b[40:48], r.Length)
	binary.LittleEndian.PutUint64(b[48:56], r.Seq)
	binary.LittleEndian.PutUint64(b[56:64], r.Version.Epoch)
	binary.LittleEndian.PutUint64(b[64:72], r.Version.Counter)
	binary.LittleEndian.PutUint32(b[72:76], r.Tag)
	binary.LittleEndian.PutUint32(b[84:88], seededCRC32C(r.Identity, body))
	binary.LittleEndian.PutUint32(b[92:96], seededCRC32C(r.Identity, b[0:92]))

	b = append(b, body...)

	return append(b, make([]byte, (8-len(b)%8)%8)...)
}
