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
