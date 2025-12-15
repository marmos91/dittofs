package metadata

import (
	"fmt"
	"time"
)

// ValidateName validates a filename for creation/move operations.
// Returns ErrInvalidArgument if name is empty, ".", or "..".
func ValidateName(name string) error {
	if name == "" || name == "." || name == ".." {
		return &StoreError{
			Code:    ErrInvalidArgument,
			Message: "invalid name",
			Path:    name,
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
// Returns ErrAccessDenied if the user is not root (UID 0).
func RequiresRoot(ctx *AuthContext) error {
	if ctx.Identity == nil || ctx.Identity.UID == nil || *ctx.Identity.UID != 0 {
		return &StoreError{
			Code:    ErrAccessDenied,
			Message: "only root can create device files",
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

// ApplyCreateDefaults applies all default values to FileAttr for create operations.
// This is a convenience function that combines mode, owner, and timestamp defaults.
//
// Modifies attr in place:
//   - Sets Mode to default if 0, and masks to valid bits
//   - Sets UID from ctx.Identity if 0
//   - Sets GID from ctx.Identity if 0
//   - Sets Atime, Mtime, Ctime to current time
//   - Sets Size to 0 (or len(linkTarget) for symlinks)
func ApplyCreateDefaults(attr *FileAttr, ctx *AuthContext, linkTarget string) {
	now := time.Now()

	// Default mode based on type and mask to valid bits
	attr.Mode = ApplyModeDefault(attr.Mode, attr.Type)

	// Default UID/GID from auth context
	ApplyOwnerDefaults(attr, ctx)

	// Set timestamps
	attr.Atime = now
	attr.Mtime = now
	attr.Ctime = now

	// Set size based on type
	if attr.Type == FileTypeSymlink {
		attr.Size = uint64(len(linkTarget))
	} else {
		attr.Size = 0
	}
}

// NowTimestamp returns the current time for use in store operations.
// This centralizes timestamp generation for consistency and potential future
// customization (e.g., testing with mock time).
func NowTimestamp() time.Time {
	return time.Now()
}
