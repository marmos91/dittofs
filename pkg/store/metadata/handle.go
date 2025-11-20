package metadata

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// ============================================================================
// File Handle Utilities
// ============================================================================

// ============================================================================
// Share-Aware Handle Encoding/Decoding
// ============================================================================

// EncodeShareHandle encodes a share name and path into a FileHandle.
//
// Format: "<shareName>:<path>"
// Example: "export:/documents/report.pdf"
//
// This encoding allows NFS handlers to determine which share a file handle
// belongs to, even though NFS only provides the export path during MOUNT
// and subsequent operations only receive opaque file handles.
//
// Parameters:
//   - shareName: The name of the share (e.g., "/export", "documents")
//   - path: The path within the share (must start with "/")
//
// Returns:
//   - FileHandle: Encoded file handle containing both share and path
//
// Example:
//
//	handle := EncodeShareHandle("/export", "/path/to/file.txt")
//	// handle = "/export:/path/to/file.txt"
func EncodeShareHandle(shareName, path string) FileHandle {
	encoded := shareName + ":" + path
	return FileHandle([]byte(encoded))
}

// DecodeShareHandle decodes a FileHandle into its share name and path components.
//
// This is the inverse of EncodeShareHandle. It extracts the share name and path
// from a file handle so that handlers can:
//  1. Look up the correct share in the ShareRegistry
//  2. Access the share's metadata and content stores
//  3. Perform operations on the correct backend
//
// Parameters:
//   - handle: The file handle to decode
//
// Returns:
//   - shareName: The name of the share
//   - path: The path within the share
//   - error: ErrInvalidFileHandle if the handle format is invalid
//
// Example:
//
//	shareName, path, err := DecodeShareHandle(handle)
//	if err != nil {
//	    return NFS3ERR_STALE
//	}
//	share := registry.GetShare(shareName)
func DecodeShareHandle(handle FileHandle) (shareName, path string, err error) {
	handleStr := string(handle)

	// Find the first colon separator
	idx := strings.Index(handleStr, ":")
	if idx == -1 {
		return "", "", fmt.Errorf("invalid file handle format: missing ':' separator")
	}

	shareName = handleStr[:idx]
	path = handleStr[idx+1:]

	if shareName == "" {
		return "", "", fmt.Errorf("invalid file handle: empty share name")
	}

	if path == "" || path[0] != '/' {
		return "", "", fmt.Errorf("invalid file handle: path must start with '/'")
	}

	return shareName, path, nil
}

// HandleToINode converts a FileHandle to a uint64 file ID (inode number).
//
// This is the CANONICAL implementation for converting file handles to inode
// numbers. All MetadataStore implementations and protocol handlers MUST use
// this function to ensure consistent inode numbers across the system.
//
// IMPORTANT FOR CUSTOM MetadataStore IMPLEMENTATIONS:
//
// If you implement your own MetadataStore, you MUST use this function when:
//  1. Populating file IDs for directory entries (ReadDirectory)
//  2. Generating inode numbers for file attributes
//  3. Any other operation that requires converting handles to numeric IDs
//
// Failure to use this function will result in:
//   - Inconsistent inode numbers between operations
//   - Circular directory structures (rm -rf failures)
//   - Client cache coherency issues
//   - find/ls reporting duplicate inodes
//
// Algorithm:
//
// Uses SHA-256 hash of the entire handle, taking the first 8 bytes as a
// uint64 in big-endian format. SHA-256 provides:
//   - Excellent collision resistance (critical for inode uniqueness)
//   - Deterministic output (same handle always produces same inode)
//   - Standard, well-tested algorithm
//   - Fast computation
//
// Parameters:
//   - handle: The file handle to convert (typically a path-based handle)
//
// Returns:
//   - uint64: File ID (inode number), or 0 if handle is empty
//
// Example:
//
//	handle := FileHandle([]byte("/export:/path/to/file.txt"))
//	inode := HandleToINode(handle)
//	// Use inode in directory entries or file attributes
func HandleToINode(handle FileHandle) uint64 {
	if len(handle) == 0 {
		return 0
	}

	// Compute SHA-256 hash of entire handle
	hash := sha256.Sum256(handle)

	// Use first 8 bytes as uint64 inode number (big-endian)
	return binary.BigEndian.Uint64(hash[:8])
}
