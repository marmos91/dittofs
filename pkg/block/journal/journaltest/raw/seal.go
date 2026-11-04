package raw

import "encoding/binary"

// SealMarkerSize is the size of a seal marker.
const SealMarkerSize = 32

// SealMarker is the marker sealing writes at the record slot just past a
// segment's last record. It verifies only under the journal identity its
// checksum is seeded with, and only where it was written: its segment id and
// offset must be those of the segment and offset it is read at, so a copy
// inside a payload never ends a scan.
type SealMarker struct {
	Identity [16]byte
	Segment  uint64 // the segment's id
	Offset   uint64 // the marker's own offset in the segment
}

// Bytes returns the marker's 32 bytes.
func (m SealMarker) Bytes() []byte {
	b := make([]byte, SealMarkerSize)
	copy(b[0:4], "DJS1")
	binary.LittleEndian.PutUint64(b[8:16], m.Segment)
	binary.LittleEndian.PutUint64(b[16:24], m.Offset)
	binary.LittleEndian.PutUint32(b[28:32], seededCRC32C(m.Identity, b[0:28]))
	return b
}
