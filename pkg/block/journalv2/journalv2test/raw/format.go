package raw

import (
	"encoding/binary"
	"fmt"
)

// FormatSize is the size of the format file.
const FormatSize = 56

// Format is the file that names a journal: its format version, its identity
// and the installation that created it. Its checksum is not seeded.
//
// decision: Bytes writes generation 0, which is never read below the
// replication extension's version. Make it a field when a check needs that
// version.
type Format struct {
	Version      uint16
	Identity     [16]byte
	Installation [16]byte
}

// Bytes returns the file's 56 bytes.
func (f Format) Bytes() []byte {
	b := make([]byte, FormatSize)
	copy(b[0:4], "DJF1")

	binary.LittleEndian.PutUint16(b[4:6], f.Version)
	copy(b[8:24], f.Identity[:])

	copy(b[24:40], f.Installation[:])
	binary.LittleEndian.PutUint32(b[52:56], crc32c(b[0:52]))

	return b
}

// ParseFormat reads the version, identity and installation ID of a format
// file. A wrong size, magic or checksum is an error. The version is read, not
// required to be 1, so a test can rebuild a journal's file under another
// version.
//
// ponytail: every version must be 56 bytes, though the size is stated for
// version 1 only. Read the version first and size the file by it when a check
// plants a later version of another size.
func ParseFormat(b []byte) (Format, error) {
	if len(b) != FormatSize {
		return Format{}, fmt.Errorf("format file is %d bytes, want %d", len(b), FormatSize)
	}
	if string(b[0:4]) != "DJF1" {
		return Format{}, fmt.Errorf("format file magic is %q, want %q", b[0:4], "DJF1")
	}
	if got, want := binary.LittleEndian.Uint32(b[52:56]), crc32c(b[0:52]); got != want {
		return Format{}, fmt.Errorf("format file checksum is 0x%08x, want 0x%08x", got, want)
	}
	return Format{
		Version:      binary.LittleEndian.Uint16(b[4:6]),
		Identity:     [16]byte(b[8:24]),
		Installation: [16]byte(b[24:40]),
	}, nil
}
