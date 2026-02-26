package metadata

import (
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// File represents a file's complete identity and attributes.
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

	// PayloadID is the identifier for retrieving file content.
	// This is the legacy path-based content identifier (e.g., "{shareName}/{path}").
	// When deduplication is enabled, ObjectID is the primary identifier.
	PayloadID PayloadID `json:"content_id"`

	// ObjectID is the content-addressed identifier for the file's content.
	// This is the SHA-256 hash of the file's content (or Merkle root of chunk hashes).
	// Used for deduplication: files with the same ObjectID share the same content.
	// Zero value indicates the object is not finalized or deduplication is disabled.
	ObjectID ContentHash `json:"object_id,omitempty"`

	// COWSourcePayloadID is the source PayloadID for copy-on-write semantics.
	// When a hard-linked file with finalized content is written to, it gets a new
	// PayloadID and this field tracks where to lazily copy unmodified blocks from.
	// Empty means no COW source (normal file or blocks already copied).
	COWSourcePayloadID PayloadID `json:"cow_source,omitempty"`

	// LinkTarget is the target path for symbolic links
	LinkTarget string `json:"link_target,omitempty"`

	// Rdev contains device major and minor numbers for device files.
	Rdev uint64 `json:"rdev,omitempty"`

	// Hidden indicates if the file should be hidden from directory listings.
	Hidden bool `json:"hidden,omitempty"`

	// ACL is the NFSv4 Access Control List for this file.
	// nil means no ACL is set -- use classic Unix permission check.
	// Non-nil with empty ACEs means an explicit empty ACL (denies all access).
	ACL *acl.ACL `json:"acl,omitempty"`

	// IdempotencyToken for detecting duplicate creation requests.
	IdempotencyToken uint64 `json:"idempotency_token,omitempty"`
}

// SetAttrs specifies which attributes to update in a SetFileAttributes call.
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

	// ACL sets the NFSv4 ACL on the file.
	// When non-nil, the ACL is validated (canonical ordering, max ACEs) before applying.
	ACL *acl.ACL
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

// PayloadID is an identifier for retrieving file content from the content repository.
type PayloadID string
