package metadata

import (
	"fmt"
	"time"
)

// ============================================================================
// Directory Entry Type
// ============================================================================

// DirEntry represents a single entry in a directory listing.
//
// This is a minimal structure containing only the information needed for
// directory iteration. For full attributes, clients use Lookup or GetFile
// on the entry's ID.
type DirEntry struct {
	// ID is the unique identifier for the file/directory
	// This typically maps to an inode number in Unix systems
	ID uint64

	// Name is the filename
	// Does not include the parent path
	Name string

	// Cookie is the NFS/SMB pagination cookie for this entry
	// Used to resume directory listing from this position
	// Set by MetadataService.ReadDirectory, not by store implementations
	Cookie uint64

	// Handle is the file handle for this entry
	// This avoids expensive Lookup() calls in READDIRPLUS
	// Implementations MUST populate this field for performance
	Handle FileHandle

	// Attr contains the file attributes (optional, for READDIRPLUS optimization)
	// If nil, READDIRPLUS will call GetFile() to retrieve attributes
	// If populated, READDIRPLUS can avoid per-entry GetFile() calls
	Attr *FileAttr
}

// ============================================================================
// Pointer Helper Functions
// ============================================================================

// Uint32Ptr returns a pointer to a uint32 value.
func Uint32Ptr(v uint32) *uint32 { return &v }

// Uint64Ptr returns a pointer to a uint64 value.
func Uint64Ptr(v uint64) *uint64 { return &v }

// TimePtr returns a pointer to a time.Time value.
func TimePtr(v time.Time) *time.Time { return &v }

// BoolPtr returns a pointer to a bool value.
func BoolPtr(v bool) *bool { return &v }

// ============================================================================
// Content ID Helpers
// ============================================================================

// BuildPayloadID constructs a PayloadID from share name and full path.
//
// This creates a path-based PayloadID suitable for S3 storage that:
//   - Removes leading "/" from both shareName and path
//   - Results in keys like "export/docs/report.pdf"
//
// This format enables:
//   - Easy S3 bucket inspection (human-readable)
//   - Metadata reconstruction from S3 (disaster recovery)
//   - Simple migrations and backups
//
// Parameters:
//   - shareName: The share/export name (e.g., "/export" or "export")
//   - fullPath: Full path with leading "/" (e.g., "/docs/report.pdf")
//
// Returns:
//   - string: PayloadID in format "shareName/path" (e.g., "export/docs/report.pdf")
//
// Examples:
//   - BuildPayloadID("/export", "/file.txt") -> "export/file.txt"
//   - BuildPayloadID("/export", "/docs/report.pdf") -> "export/docs/report.pdf"
func BuildPayloadID(shareName, fullPath string) string {
	// Remove leading "/" from shareName
	share := shareName
	if len(share) > 0 && share[0] == '/' {
		share = share[1:]
	}

	// Remove leading "/" from fullPath
	path := fullPath
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	// Handle edge cases
	if len(share) == 0 {
		return path
	}

	if len(path) == 0 {
		return share
	}

	return share + "/" + path
}

// Mode bits for special permission checks
const (
	// ModeSticky is the sticky bit (01000 in octal).
	// When set on a directory, only the file owner, directory owner, or root
	// can delete or rename files within that directory.
	ModeSticky = 0o1000
)

// Filesystem path limits (POSIX standard values)
const (
	// MaxNameLen is the maximum length of a filename component (NAME_MAX)
	MaxNameLen = 255
	// MaxPathLen is the maximum length of a full path (PATH_MAX)
	MaxPathLen = 4096
)

// ValidateName validates a filename for creation/move operations.
// Returns ErrInvalidArgument if name is empty, ".", or "..".
// Returns ErrNameTooLong if name exceeds MaxNameLen (255 bytes).
func ValidateName(name string) error {
	if name == "" || name == "." || name == ".." {
		return &StoreError{
			Code:    ErrInvalidArgument,
			Message: "invalid name",
			Path:    name,
		}
	}
	if len(name) > MaxNameLen {
		return &StoreError{
			Code:    ErrNameTooLong,
			Message: "filename too long",
			Path:    name,
		}
	}
	return nil
}

// ValidatePath validates a full path for POSIX compliance.
// Returns ErrNameTooLong if path exceeds MaxPathLen (4096 bytes).
//
// Internal paths start with "/" (e.g., "/dir/file"), while client paths are
// relative to the mount point (e.g., "dir/file"). POSIX PATH_MAX (4096) includes
// the null terminator, so client paths can be up to 4095 characters. However,
// our internal paths add a leading "/" making them up to 4096 characters.
// This function validates the internal path length using > MaxPathLen.
func ValidatePath(path string) error {
	if len(path) > MaxPathLen {
		return &StoreError{
			Code:    ErrNameTooLong,
			Message: "path too long",
			Path:    path,
		}
	}
	return nil
}

// ValidateCreateType validates that the file type is valid for Create().
// Only FileTypeRegular and FileTypeDirectory are allowed.
func ValidateCreateType(fileType FileType) error {
	if fileType != FileTypeRegular && fileType != FileTypeDirectory {
		return &StoreError{
			Code:    ErrInvalidArgument,
			Message: "Create only supports regular files and directories",
		}
	}
	return nil
}

// ValidateSpecialFileType validates that the file type is a valid special file type.
// Valid types: FileTypeBlockDevice, FileTypeCharDevice, FileTypeSocket, FileTypeFIFO.
func ValidateSpecialFileType(fileType FileType) error {
	switch fileType {
	case FileTypeBlockDevice, FileTypeCharDevice, FileTypeSocket, FileTypeFIFO:
		return nil
	default:
		return &StoreError{
			Code:    ErrInvalidArgument,
			Message: fmt.Sprintf("invalid special file type: %d", fileType),
		}
	}
}

// ValidateSymlinkTarget validates that the symlink target is not empty.
func ValidateSymlinkTarget(target string) error {
	if target == "" {
		return &StoreError{
			Code:    ErrInvalidArgument,
			Message: "symlink target cannot be empty",
		}
	}
	return nil
}

// RequiresRoot checks if the operation requires root privileges.
// Returns ErrPrivilegeRequired if the user is not root (UID 0).
// This maps to NFS3ErrPerm (EPERM) per RFC 1813 for privilege violations.
func RequiresRoot(ctx *AuthContext) error {
	if ctx.Identity == nil || ctx.Identity.UID == nil || *ctx.Identity.UID != 0 {
		return &StoreError{
			Code:    ErrPrivilegeRequired,
			Message: "operation requires root privileges",
		}
	}
	return nil
}

// DefaultMode returns the default mode for a given file type.
// Returns:
//   - 0755 for directories
//   - 0777 for symlinks
//   - 0644 for regular files, special files, and others
func DefaultMode(fileType FileType) uint32 {
	switch fileType {
	case FileTypeDirectory:
		return 0755
	case FileTypeSymlink:
		return 0777
	default:
		return 0644 // Regular files, special files
	}
}

// ApplyModeDefault applies the default mode if the provided mode is 0.
// Also masks the mode to valid permission bits (0o7777).
func ApplyModeDefault(mode uint32, fileType FileType) uint32 {
	if mode == 0 {
		mode = DefaultMode(fileType)
	}
	return mode & 0o7777
}

// ApplyOwnerDefaults applies default UID/GID from the auth context if not already set.
// If attr.UID is 0 and ctx has a valid UID, uses the context UID.
// If attr.GID is 0 and ctx has a valid GID, uses the context GID.
func ApplyOwnerDefaults(attr *FileAttr, ctx *AuthContext) {
	if ctx.Identity != nil && ctx.Identity.UID != nil {
		if attr.UID == 0 {
			attr.UID = *ctx.Identity.UID
		}
		if attr.GID == 0 && ctx.Identity.GID != nil {
			attr.GID = *ctx.Identity.GID
		}
	}
}

// ApplyCreateDefaults applies default values to FileAttr for create operations.
//
// IMPORTANT: This function does NOT set UID/GID defaults. Callers are responsible
// for setting UID/GID before calling this function. This is because UID=0 and GID=0
// are valid values (root), so we cannot use 0 as a sentinel for "not set".
// The XDR layer (ConvertSetAttrsToMetadata) handles UID/GID defaulting correctly
// using pointer semantics.
//
// Modifies attr in place:
//   - Sets Mode to default if 0, and masks to valid bits
//   - Sets Atime, Mtime, Ctime to current time
//   - Sets Size to 0 (or len(linkTarget) for symlinks)
//
// Does NOT modify:
//   - UID (caller must set)
//   - GID (caller must set)
func ApplyCreateDefaults(attr *FileAttr, ctx *AuthContext, linkTarget string) {
	now := time.Now()

	// Default mode based on type and mask to valid bits
	attr.Mode = ApplyModeDefault(attr.Mode, attr.Type)

	// NOTE: UID/GID defaults are NOT applied here.
	// The XDR layer handles this correctly using pointer semantics.
	// See ConvertSetAttrsToMetadata in internal/protocol/nfs/xdr/attributes.go

	// Set timestamps
	attr.Atime = now
	attr.Mtime = now
	attr.Ctime = now
	attr.CreationTime = now

	// Set size based on type
	if attr.Type == FileTypeSymlink {
		attr.Size = uint64(len(linkTarget))
	} else {
		attr.Size = 0
	}
}

// CheckStickyBitRestriction checks if an operation is allowed under sticky bit semantics.
//
// When a directory has the sticky bit set (mode & 01000), only certain users can
// delete or rename files in that directory:
//   - Root (UID 0) can always delete/rename
//   - The owner of the file/directory being deleted/renamed
//   - The owner of the sticky directory
//
// Parameters:
//   - ctx: Authentication context with the user's identity
//   - dirAttr: Attributes of the directory containing the file
//   - fileAttr: Attributes of the file being deleted/renamed
//
// Returns:
//   - nil if the operation is allowed
//   - ErrAccessDenied if the sticky bit restriction blocks the operation
func CheckStickyBitRestriction(ctx *AuthContext, dirAttr *FileAttr, fileAttr *FileAttr) error {
	// Check if the directory has the sticky bit set
	if dirAttr.Mode&ModeSticky == 0 {
		// No sticky bit, no restriction
		return nil
	}

	// Get the effective UID of the caller
	callerUID := ^uint32(0) // Default to max (invalid)
	if ctx.Identity != nil && ctx.Identity.UID != nil {
		callerUID = *ctx.Identity.UID
	}

	// Root (UID 0) can always delete/rename in sticky directories
	if callerUID == 0 {
		return nil
	}

	// Check if the caller owns the file being deleted/renamed
	if fileAttr.UID == callerUID {
		return nil
	}

	// Check if the caller owns the sticky directory
	if dirAttr.UID == callerUID {
		return nil
	}

	// Sticky bit restriction applies - deny the operation
	return &StoreError{
		Code:    ErrAccessDenied,
		Message: "sticky bit set: operation not permitted",
	}
}
