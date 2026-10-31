package raw

import (
	"encoding/binary"
	"fmt"
)

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
// Identity, which is not written. The body is whichever one of the four body
// fields is set, and it is written whatever Kind says, so a check can plant a
// record whose body does not match its kind.
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

	// SyncedThrough is the offset in this segment a sync had completed
	// through when the record was written, at [76,84).
	SyncedThrough uint64

	// At is the record's own offset in its segment, at [88,92).
	At uint32

	Write *WriteBody
	Fill  *FillBody
	Loss  *LossBody
	Clone *CloneBody
	Cut   *CutBody
}

// WriteBody is a write record's body: the modification time, then the
// payload. The payload checksum covers both.
type WriteBody struct {
	MTime   int64 // nanoseconds since the Unix epoch, or 0 when suspended
	Payload []byte
}

// FillBody is a fill record's body: the payload alone, of Length bytes.
type FillBody struct{ Payload []byte }

// LossBody is a loss record's 16-byte body: the sequence number of the record
// it drops, then its flag word, both written as given so a check can plant a
// flag the reader must refuse.
type LossBody struct{ Dropped, Flags uint64 }

// CloneBody is a clone target record's 56-byte body: the source share, the
// source file, the source offset and the AsOf version.
type CloneBody struct {
	Share, File [16]byte
	Off         uint64
	AsOf        Version
}

// CutBody is the 8-byte body of a hold, unhold or stamp record: the cut
// number.
type CutBody struct{ Cut uint64 }

// writeBody returns the record's body, of whichever kind it holds, or nil for
// none. More than one body set is a caller's bug.
func (r Record) writeBody() []byte {
	set := 0
	var b []byte
	if r.Write != nil {
		set++
		b = binary.LittleEndian.AppendUint64(b, uint64(r.Write.MTime))
		b = append(b, r.Write.Payload...)
	}
	if r.Fill != nil {
		set++
		b = r.Fill.Payload
	}
	if r.Loss != nil {
		set++
		b = binary.LittleEndian.AppendUint64(b, r.Loss.Dropped)
		b = binary.LittleEndian.AppendUint64(b, r.Loss.Flags)
	}
	if r.Clone != nil {
		set++
		b = append(b, r.Clone.Share[:]...)
		b = append(b, r.Clone.File[:]...)
		b = binary.LittleEndian.AppendUint64(b, r.Clone.Off)
		b = binary.LittleEndian.AppendUint64(b, r.Clone.AsOf.Epoch)
		b = binary.LittleEndian.AppendUint64(b, r.Clone.AsOf.Counter)
	}
	if r.Cut != nil {
		set++
		b = binary.LittleEndian.AppendUint64(b, r.Cut.Cut)
	}
	if set > 1 {
		panic(fmt.Sprintf("raw: %d bodies set, want one at most", set))
	}
	return b
}

// Bytes returns the record's header, body and padding. A record with no body
// is its header alone.
func (r Record) Bytes() []byte {
	body := r.writeBody()

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
	binary.LittleEndian.PutUint64(b[76:84], r.SyncedThrough)
	binary.LittleEndian.PutUint32(b[84:88], seededCRC32C(r.Identity, body))
	binary.LittleEndian.PutUint32(b[88:92], r.At)
	binary.LittleEndian.PutUint32(b[92:96], seededCRC32C(r.Identity, b[0:92]))

	b = append(b, body...)

	return append(b, make([]byte, (8-len(b)%8)%8)...)
}
