package raw

import "encoding/binary"

// Entry is one catalog entry of a sealed segment's footer: where a record
// sits in the segment and what its header says. It has no checksum of its
// own; the trailer's footer checksum covers it.
//
// ponytail: the content version at [44,60) is written as zeros and is not a
// field. Add it once the byte order of its epoch and counter halves is
// settled.
type Entry struct {
	FileID       [16]byte
	Offset       uint64 // file offset of the record's first byte
	RecordOffset uint32 // the record's offset within its segment
	Length       uint64
	Seq          uint64 // sequence number
	Kind         uint8
	Tag          uint32 // share tag
}

// Bytes returns the entry's 72 bytes.
func (e Entry) Bytes() [CatalogEntrySize]byte {
	var b [CatalogEntrySize]byte
	copy(b[0:16], e.FileID[:])
	binary.LittleEndian.PutUint64(b[16:24], e.Offset)
	binary.LittleEndian.PutUint32(b[24:28], e.RecordOffset)
	binary.LittleEndian.PutUint64(b[28:36], e.Length)
	binary.LittleEndian.PutUint64(b[36:44], e.Seq)
	b[60] = e.Kind
	binary.LittleEndian.PutUint32(b[64:68], e.Tag)
	return b
}
