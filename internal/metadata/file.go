package metadata

import (
	"time"

	"github.com/marmos91/dittofs/internal/content"
)

// FileHandle represents a unique identifier for a file or directory
type FileHandle []byte

// FileAttr contains the metadata for a file or directory
type FileAttr struct {
	Type          FileType
	Mode          uint32
	UID           uint32
	GID           uint32
	Size          uint64
	Atime         time.Time
	Mtime         time.Time
	Ctime         time.Time
	ContentID     content.ContentID // ID referencing the actual file content
	SymlinkTarget string            // For symlinks, the target path
}

// FileType represents the type of file
type FileType uint32

const (
	FileTypeRegular   FileType = 1
	FileTypeDirectory FileType = 2
	FileTypeBlock     FileType = 3
	FileTypeChar      FileType = 4
	FileTypeSymlink   FileType = 5
	FileTypeSocket    FileType = 6
	FileTypeFifo      FileType = 7
)

// FSInfo contains static filesystem information and capabilities.
// This structure is returned by the repository to inform NFS clients
// about server limits, preferences, and supported features.
type FSInfo struct {
	// RtMax is the maximum size in bytes of a READ request
	// Clients should not exceed this value in READ operations
	RtMax uint32

	// RtPref is the preferred size in bytes of a READ request
	// Optimal performance is achieved using this size
	RtPref uint32

	// RtMult is the suggested multiple for READ request sizes
	// READ sizes should ideally be multiples of this value
	RtMult uint32

	// WtMax is the maximum size in bytes of a WRITE request
	// Clients should not exceed this value in WRITE operations
	WtMax uint32

	// WtPref is the preferred size in bytes of a WRITE request
	// Optimal performance is achieved using this size
	WtPref uint32

	// WtMult is the suggested multiple for WRITE request sizes
	// WRITE sizes should ideally be multiples of this value
	WtMult uint32

	// DtPref is the preferred size in bytes of a READDIR request
	// Optimal performance is achieved using this size
	DtPref uint32

	// MaxFileSize is the maximum file size in bytes supported by the server
	MaxFileSize uint64

	// TimeDelta represents the server's time resolution
	// Indicates the granularity of timestamps
	TimeDelta TimeDelta

	// Properties is a bitmask of filesystem properties
	// Indicates supported features (hard links, symlinks, etc.)
	Properties uint32
}

// TimeDelta represents time resolution with seconds and nanoseconds.
type TimeDelta struct {
	// Seconds component of time resolution
	Seconds uint32

	// Nseconds component of time resolution (0-999999999)
	Nseconds uint32
}
