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
// Identity Fields:
//   - ID: Unique UUID identifier for this file, used in protocol handles
//   - ShareName: The share this file belongs to (e.g., "/export")
//   - Path: Full path within the share (e.g., "/documents/report.pdf")
//
// Attributes (via embedded FileAttr):
//   - Type, Mode, UID, GID: Standard Unix attributes
//   - Size, Atime, Mtime, Ctime: Size and timestamps
//   - ContentID: Identifier for content operations
//   - LinkTarget: Symlink target path
//
// Protocol Handle Format:
//
//	Handle = "shareName:uuid" (e.g., "/export:550e8400-e29b-41d4-a716-446655440000")
//	Maximum size: 45 bytes (well under NFS RFC 1813's 64-byte limit)
//
// Storage Backend Considerations:
//   - Metadata stores use ID as the primary key
//   - Content stores may use ID, Path, or custom schemes
//   - Path is preserved for human-readable storage (S3, filesystem)
type File struct {
	// ID is a unique identifier for this file.
	// Used in protocol handles: "/export:550e8400-e29b-41d4-a716-446655440000"
	// Generated using UUID v4 (random) for collision resistance.
	ID uuid.UUID `json:"id"`

	// ShareName is the share this file belongs to (e.g., "/export").
	// Used for routing operations to the correct metadata/content stores.
	ShareName string `json:"share_name"`

	// Path is the full path within the share (e.g., "/documents/report.pdf").
	// Used by storage backends that need paths (S3, human-readable filesystems).
	// Always starts with "/" and is relative to the share root.
	Path string `json:"path"`

	// FileAttr is embedded for convenient access to attributes.
	// Access via: file.Mode, file.Size, file.Mtime (not file.Attr.Mode)
	FileAttr
}

// FileAttr contains the complete metadata for a file or directory.
//
// This structure represents the core attributes common across different
// filesystem protocols. Protocol handlers translate between FileAttr and
// protocol-specific attribute structures.
//
// Time Semantics:
//   - Atime (access time): Updated when file is read
//   - Mtime (modification time): Updated when file content changes
//   - Ctime (change time): Updated when metadata changes (size, permissions, etc.)
//
// Note: Ctime is not "creation time" in Unix semantics - it's the last metadata
// change time. Some protocols (like Windows) have separate creation times which
// may need to be tracked separately.
type FileAttr struct {
	// Type is the file type (regular, directory, symlink, etc.)
	Type FileType `json:"type"`

	// Mode contains permission bits and file type
	// Format: Unix permission bits (0o7777 max)
	//   - Bits 0-2: Other permissions (rwx)
	//   - Bits 3-5: Group permissions (rwx)
	//   - Bits 6-8: Owner permissions (rwx)
	//   - Bits 9-11: Special bits (setuid, setgid, sticky)
	Mode uint32 `json:"mode"`

	// UID is the owner user ID
	UID uint32 `json:"uid"`

	// GID is the owner group ID
	GID uint32 `json:"gid"`

	// Nlink is the number of hard links (directory entries) referencing this file.
	//
	// For regular files:
	//   - Starts at 1 when created
	//   - Incremented when hard links are created
	//   - Decremented when any link (including the original) is removed
	//   - File content is deleted when Nlink reaches 0
	//
	// For directories:
	//   - Starts at 2 when created (. and parent's entry)
	//   - Incremented when subdirectories are created (each subdir adds ..)
	//   - Decremented when subdirectories are removed
	//
	// For other types (symlinks, devices, sockets, FIFOs):
	//   - Always 1 (hard links to these types are typically not supported)
	//
	// This field is populated by metadata stores from their internal link tracking.
	// If 0, protocol layers should default to 1 for backwards compatibility.
	Nlink uint32 `json:"nlink"`

	// TODO: Extended attributes for protocol-specific data. Useful, for example, to map Windows ACLs
	// Extended map[string][]byte

	// Size is the file size in bytes
	// For directories: implementation-specific (typically block size)
	// For symlinks: length of target path
	// For special files: 0
	Size uint64 `json:"size"`

	// Atime is the last access time
	Atime time.Time `json:"atime"`

	// Mtime is the last modification time (content changes)
	Mtime time.Time `json:"mtime"`

	// Ctime is the last change time (metadata changes)
	Ctime time.Time `json:"ctime"`

	// CreationTime is the file creation time (birth time).
	// This is the actual time the file was created, distinct from Ctime which tracks
	// metadata changes. Used by:
	//   - SMB2 protocol (CreationTime field)
	//   - NFSv4 protocol (time_create attribute)
	// Set when file is first created and never updated afterwards.
	CreationTime time.Time `json:"creation_time"`

	// ContentID is the identifier for retrieving file content from the content repository
	// Empty for directories, symlinks, and special files
	ContentID ContentID `json:"content_id"`

	// LinkTarget is the target path for symbolic links
	// Only valid when Type == FileTypeSymlink
	LinkTarget string `json:"link_target,omitempty"`

	// Rdev contains the device major and minor numbers for device files.
	// Only valid when Type == FileTypeBlockDevice or FileTypeCharDevice.
	// For other file types, this should be 0.
	// The value is encoded as: (major << 8) | minor (for compatibility)
	// or stored as separate fields depending on implementation.
	Rdev uint64 `json:"rdev,omitempty"`

	// Hidden indicates if the file should be hidden from directory listings.
	// This is primarily used for Windows/SMB compatibility where hidden files
	// are marked with an attribute rather than a dot prefix.
	// When set via SMB, this persists the FILE_ATTRIBUTE_HIDDEN flag.
	// Unix dot-prefix files are also treated as hidden in SMB listings.
	Hidden bool `json:"hidden,omitempty"`

	// IdempotencyToken stores a token for detecting duplicate creation requests.
	// Used by protocols that support idempotent file creation:
	//   - NFS: CREATE EXCLUSIVE mode verifier (8-byte client-provided value)
	//   - SMB: Could be used for durable handle reconnection (future)
	// Empty/0 for normal file creation. Set only on initial exclusive creation.
	// This enables clients to safely retry file creation after network failures.
	IdempotencyToken uint64 `json:"idempotency_token,omitempty"`
}

// SetAttrs specifies which attributes to update in a SetFileAttributes call.
//
// Each field is a pointer. A nil pointer means "do not change this attribute".
// A non-nil pointer means "set this attribute to the pointed value".
//
// This allows atomic multi-attribute updates with selective modification without
// requiring separate boolean flags for each field.
//
// The server automatically updates Ctime when any attribute changes, so there
// is no Ctime field - clients cannot set Ctime directly.
//
// Example:
//
//	// Change only mode and size
//	attrs := &metadata.SetAttrs{
//	    Mode: metadata.Uint32Ptr(0644),
//	    Size: metadata.Uint64Ptr(1024),
//	    // Other fields are nil = unchanged
//	}
//	err := repo.SetFileAttributes(ctx, handle, attrs)
type SetAttrs struct {
	// Mode is the new permission bits (only lower 12 bits are used)
	// Format: Unix permission bits (0o7777 max)
	// nil = do not change
	Mode *uint32

	// UID is the new owner user ID
	// Only root can change file ownership
	// nil = do not change
	UID *uint32

	// GID is the new owner group ID
	// Only root or owner (if member of target group) can change group
	// nil = do not change
	GID *uint32

	// Size is the new file size in bytes
	// If less than current: truncate (remove trailing content)
	// If greater than current: extend (pad with zeros)
	// If zero: delete all content
	// Cannot be used on directories or special files
	// nil = do not change
	Size *uint64

	// Atime is the new access time
	// nil = do not change
	Atime *time.Time

	// Mtime is the new modification time
	// nil = do not change
	Mtime *time.Time

	// AtimeNow indicates that Atime should be set to current time (UTIME_NOW semantics).
	// When true, users with write permission can set atime.
	// When false (arbitrary time), only owner or root can set atime.
	// This field is only meaningful when Atime is non-nil.
	AtimeNow bool

	// MtimeNow indicates that Mtime should be set to current time (UTIME_NOW semantics).
	// When true, users with write permission can set mtime.
	// When false (arbitrary time), only owner or root can set mtime.
	// This field is only meaningful when Mtime is non-nil.
	MtimeNow bool

	// CreationTime is the new creation time
	// Only used by Windows/SMB clients that support setting creation time
	// nil = do not change
	CreationTime *time.Time

	// Hidden is the new hidden state
	// Used by Windows/SMB clients to mark files as hidden
	// nil = do not change
	Hidden *bool
}

// FileType represents the type of a filesystem object.
//
// These are generic types that map to Unix file types. Protocol handlers
// translate between FileType and protocol-specific type values.
type FileType int

const (
	// FileTypeRegular is a regular file containing data
	FileTypeRegular FileType = iota

	// FileTypeDirectory is a directory (container for other files)
	FileTypeDirectory

	// FileTypeSymlink is a symbolic link (contains a path to another file)
	FileTypeSymlink

	// FileTypeBlockDevice is a block device (disk, partition, etc.)
	FileTypeBlockDevice

	// FileTypeCharDevice is a character device (terminal, serial port, etc.)
	FileTypeCharDevice

	// FileTypeSocket is a Unix domain socket (IPC endpoint)
	FileTypeSocket

	// FileTypeFIFO is a named pipe (FIFO for IPC)
	FileTypeFIFO
)

// ContentID is an identifier for retrieving file content from the content repository.
//
// This is an opaque identifier used to coordinate between metadata and content
// repositories. The format and generation of ContentIDs is implementation-specific.
//
// Example formats:
//   - UUID: "550e8400-e29b-41d4-a716-446655440000"
//   - Hash: "sha256:abcdef123456..."
//   - Path-based: "content/2024/01/15/abc123"
type ContentID string

// MakeRdev encodes major and minor device numbers into a single Rdev value.
// This follows the Linux convention: MKDEV(major, minor) = (major << 20) | minor
// for the extended device number format supporting 12-bit major and 20-bit minor.
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
