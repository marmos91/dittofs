package metadata

import (
	"time"

	"github.com/google/uuid"
)

// File represents a file's complete identity and attributes.
//
// This structure combines file identity (ID, ShareName, Path) with attributes
// (permissions, size, timestamps, etc.) for efficient handling across the system.
//
// The File struct embeds FileAttr for convenient access to attributes directly
// on the File object (e.g., file.Mode instead of file.Attr.Mode).
//
// Protocol Handle Format:
//
//	Handle = "shareName:uuid" (e.g., "/export:550e8400-e29b-41d4-a716-446655440000")
//	Maximum size: 45 bytes (well under NFS RFC 1813's 64-byte limit)
type File struct {
	// ID is a unique identifier for this file.
	ID uuid.UUID `json:"id"`

	// ShareName is the share this file belongs to (e.g., "/export").
	ShareName string `json:"share_name"`

	// Path is the full path within the share (e.g., "/documents/report.pdf").
	Path string `json:"path"`

	// FileAttr is embedded for convenient access to attributes.
	FileAttr
}

// FileAttr contains the complete metadata for a file or directory.
//
// Time Semantics:
//   - Atime (access time): Updated when file is read
//   - Mtime (modification time): Updated when file content changes
//   - Ctime (change time): Updated when metadata changes (size, permissions, etc.)
type FileAttr struct {
	// Type is the file type (regular, directory, symlink, etc.)
	Type FileType `json:"type"`

	// Mode contains permission bits (0o7777 max)
	Mode uint32 `json:"mode"`

	// UID is the owner user ID
	UID uint32 `json:"uid"`

	// GID is the owner group ID
	GID uint32 `json:"gid"`

	// Nlink is the number of hard links referencing this file.
	Nlink uint32 `json:"nlink"`

	// Size is the file size in bytes
	Size uint64 `json:"size"`

	// Atime is the last access time
	Atime time.Time `json:"atime"`

	// Mtime is the last modification time (content changes)
	Mtime time.Time `json:"mtime"`

	// Ctime is the last change time (metadata changes)
	Ctime time.Time `json:"ctime"`

	// CreationTime is the file creation time (birth time).
	CreationTime time.Time `json:"creation_time"`

	// ContentID is the identifier for retrieving file content
	ContentID ContentID `json:"content_id"`

	// LinkTarget is the target path for symbolic links
	LinkTarget string `json:"link_target,omitempty"`

	// Rdev contains device major and minor numbers for device files.
	Rdev uint64 `json:"rdev,omitempty"`

	// Hidden indicates if the file should be hidden from directory listings.
	Hidden bool `json:"hidden,omitempty"`

	// IdempotencyToken for detecting duplicate creation requests.
	IdempotencyToken uint64 `json:"idempotency_token,omitempty"`
}

// SetAttrs specifies which attributes to update in a SetFileAttributes call.
// Each field is a pointer. A nil pointer means "do not change this attribute".
type SetAttrs struct {
	Mode         *uint32
	UID          *uint32
	GID          *uint32
	Size         *uint64
	Atime        *time.Time
	Mtime        *time.Time
	AtimeNow     bool
	MtimeNow     bool
	CreationTime *time.Time
	Hidden       *bool
}

// FileType represents the type of a filesystem object.
type FileType int

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
	FileTypeBlockDevice
	FileTypeCharDevice
	FileTypeSocket
	FileTypeFIFO
)

// ContentID is an identifier for retrieving file content from the content repository.
type ContentID string

// ============================================================================
// Device Number Helpers
// ============================================================================

// MakeRdev encodes major and minor device numbers into a single Rdev value.
func MakeRdev(major, minor uint32) uint64 {
	return (uint64(major) << 20) | uint64(minor&0xFFFFF)
}

// RdevMajor extracts the major device number from an Rdev value.
func RdevMajor(rdev uint64) uint32 {
	return uint32(rdev >> 20)
}

// RdevMinor extracts the minor device number from an Rdev value.
func RdevMinor(rdev uint64) uint32 {
	return uint32(rdev & 0xFFFFF)
}

// GetInitialLinkCount returns the initial link count for a new file.
func GetInitialLinkCount(fileType FileType) uint32 {
	if fileType == FileTypeDirectory {
		return 2 // . and parent entry
	}
	return 1
}
