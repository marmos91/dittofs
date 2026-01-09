package metadata

import "time"

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

// BuildContentID constructs a ContentID from share name and full path.
//
// This creates a path-based ContentID suitable for S3 storage that:
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
//   - string: ContentID in format "shareName/path" (e.g., "export/docs/report.pdf")
//
// Examples:
//   - BuildContentID("/export", "/file.txt") -> "export/file.txt"
//   - BuildContentID("/export", "/docs/report.pdf") -> "export/docs/report.pdf"
func BuildContentID(shareName, fullPath string) string {
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
