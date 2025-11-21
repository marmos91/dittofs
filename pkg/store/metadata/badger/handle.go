package badger

import (
	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/marmos91/dittofs/pkg/store/metadata/internal"
)

// File Handle Generation Strategy
// ================================
//
// DittoFS uses UUID-based file handles to comply with NFS RFC 1813's 64-byte limit
// while supporting arbitrarily deep directory hierarchies. The handle format is:
//
// Handle Format:
//    Format: "shareName:uuid"
//    Example: "/export:550e8400-e29b-41d4-a716-446655440000"
//    Size: Typically 45 bytes (well under 64-byte NFS limit)
//
// Benefits:
//   - Always under 64 bytes (NFS RFC 1813 compliant)
//   - No path length limitations
//   - Stable across file renames (UUID doesn't change)
//   - Future-proof for multi-protocol support (SMB can use UUIDs directly)
//
// The UUID is generated when a file is created and never changes, even if the
// file is renamed or moved.

// buildFullPath constructs the full path for a file from its parent's path and child name.
//
// This is used during file creation to determine the complete path from the root.
// With UUID-based handles, we simply append the child name to the parent's stored path.
//
// Thread Safety: Safe for concurrent use (pure function).
//
// Parameters:
//   - parentPath: The parent directory's path (from the parent File object)
//   - childName: The name of the child being created
//
// Returns:
//   - string: The full path (e.g., "/images/photo.jpg")
//
// Example:
//
//	path := buildFullPath("/images", "photo.jpg")
//	// Returns: "/images/photo.jpg"
func buildFullPath(parentPath, childName string) string {
	// Ensure we don't create double slashes
	if parentPath == "/" {
		return "/" + childName
	}
	return parentPath + "/" + childName
}

// buildContentID is a convenience wrapper around internal.BuildContentID.
// See internal.BuildContentID for full documentation.
func buildContentID(shareName, fullPath string) string {
	return internal.BuildContentID(shareName, fullPath)
}

// fileHandleToID converts a FileHandle ([]byte) to a uint64 file ID.
//
// This is a thin wrapper around metadata.HandleToFileID() which provides the
// canonical implementation for converting file handles to inode numbers.
//
// This is used for directory entries where NFS requires a uint64 file ID.
//
// IMPORTANT: All MetadataStore implementations should use metadata.HandleToFileID()
// directly to ensure consistent inode generation across the system.
//
// See: metadata.HandleToFileID() for implementation details
func fileHandleToID(handle metadata.FileHandle) uint64 {
	return metadata.HandleToINode(handle)
}
