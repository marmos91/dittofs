package format

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

const (
	// FormatFileSize is the size of a version-1 format file.
	FormatFileSize = 56

	// FormatVersion is the format version this package writes and reads.
	FormatVersion uint16 = 1
)

// FormatFile is the content of the journal's format file: whose journal this
// is, and the installation that created it.
type FormatFile struct {
	Identity     core.JournalID
	Installation core.InstallationID
}

// AppendFormatFile appends f's 56 bytes to dst and returns the result: the
// magic and the format version, the identity, the installation, and the
// checksum of the bytes this call appends, never of a caller's prefix.
//
// The generation is written zero and the reserved bytes are written zero, as
// the format's table has them.
func AppendFormatFile(dst []byte, f FormatFile) []byte {
	start := len(dst)

	dst = append(dst, 'D', 'J', 'F', '1')
	dst = binary.LittleEndian.AppendUint16(dst, FormatVersion)
	dst = append(dst, 0, 0) // reserved
	dst = append(dst, f.Identity[:]...)
	dst = append(dst, f.Installation[:]...)
	dst = append(dst, 0, 0, 0, 0, 0, 0, 0, 0) // generation, never read below the replication extension's version
	dst = append(dst, 0, 0, 0, 0)             // reserved
	return binary.LittleEndian.AppendUint32(dst, crc32c(dst[start:]))
}

// DecodeFormatFile reads a format file's content. An error matching
// ErrDamaged means the bytes are not a version-1 format file; one matching
// ErrUnknownVersion means they name a version this binary does not read.
func DecodeFormatFile(b []byte) (FormatFile, error) {
	f := FormatFile{}

	// decision: the version is read before length and checksum, as the
	// format's rules order it, so a bit flipped in a version-1 file's
	// version field reports an unknown version rather than damage; both
	// fail the open. A later version may have another size, so its length
	// and checksum are never judged by version 1's rules.
	if len(b) < 6 {
		return f, fmt.Errorf("%w: %d bytes, no version to read", ErrDamaged, len(b))
	}
	if string(b[0:4]) != "DJF1" {
		return f, fmt.Errorf("%w: magic %q, want %q", ErrDamaged, b[0:4], "DJF1")
	}
	if version := binary.LittleEndian.Uint16(b[4:6]); version != FormatVersion {
		return f, fmt.Errorf("%w: version %d, want %d", ErrUnknownVersion, version, FormatVersion)
	}
	if len(b) != FormatFileSize {
		return f, fmt.Errorf("%w: %d bytes, want %d", ErrDamaged, len(b), FormatFileSize)
	}
	if got, want := binary.LittleEndian.Uint32(b[52:56]), crc32c(b[0:52]); got != want {
		return f, fmt.Errorf("%w: checksum 0x%08x, want the CRC32C of [0,52): 0x%08x", ErrDamaged, got, want)
	}

	// decision: non-zero reserved bytes are refused as damage, the
	// stricter reading — the table writes them as zero and says no more.
	// Accept them if the format's owner rules that a reader ignores them.
	if !zero(b[6:8]) || !zero(b[48:52]) {
		return f, fmt.Errorf("%w: reserved bytes are not zero", ErrDamaged)
	}

	// decision: the generation is never read below the replication
	// extension's version, which gives it its meaning. Read it when that
	// version is recognised.
	f.Identity = core.JournalID(b[8:24])
	f.Installation = core.InstallationID(b[24:40])
	return f, nil
}

// zero reports whether every byte of b is zero.
func zero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
