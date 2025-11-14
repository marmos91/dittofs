package badger

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/internal"
)

// File Handle Generation Strategy
// ================================
//
// DittoFS uses path-based file handles to enable filesystem import/export and
// metadata reconstruction from content stores. The handle format is designed to
// be deterministic (same path = same handle), reversible (can extract path from
// handle), and NFS-compatible (under 64 bytes for most paths).
//
// Handle Formats:
//
// 1. Path-Based (Primary) - For paths that fit in NFS limit
//    Format: "shareName:fullPath"
//    Example: "/export:/images/photo.jpg" → []byte("/export:/images/photo.jpg")
//    Size: Variable, typically 20-50 bytes
//    Used when: len(shareName + ":" + fullPath) <= 64 bytes
//
// 2. Hash-Based (Fallback) - For very long paths exceeding NFS limit
//    Format: "H:" + sha256(shareName:fullPath)[:30]
//    Example: "/export:/very/long/path..." → []byte("H:" + hash[:30])
//    Size: Fixed 32 bytes (2 byte prefix + 30 bytes hash)
//    Used when: len(shareName + ":" + fullPath) > 64 bytes
//
// The hybrid approach provides:
// - Deterministic generation (reproducible across restarts)
// - Path extraction for import/export (when using path-based)
// - NFS compatibility (max 64 bytes per RFC 1813)
// - Graceful handling of edge cases (very long paths)
//
// For hash-based handles, we maintain a reverse mapping in the database:
//   Key: "hmap:" + string(handle)
//   Value: shareName + ":" + fullPath

const (
	// maxNFSHandleSize is the maximum file handle size supported by NFSv3 (RFC 1813)
	maxNFSHandleSize = 64

	// hashPrefixSize is the size of the "H:" prefix for hash-based handles
	hashPrefixSize = 2

	// hashHandleDataSize is the number of hash bytes included in hash-based handles
	// This leaves room for: 2 (prefix) + 30 (hash) = 32 bytes total
	hashHandleDataSize = 30

	// hashHandlePrefix identifies hash-based handles
	hashHandlePrefix = "H:"
)

// generateFileHandle creates a deterministic file handle from a share name and path.
//
// This function implements the hybrid handle generation strategy, choosing between
// path-based and hash-based formats depending on the total length.
//
// Thread Safety: Safe for concurrent use (pure function).
//
// Parameters:
//   - shareName: The name of the share (e.g., "/export")
//   - fullPath: The full path within the share (e.g., "/images/photo.jpg")
//
// Returns:
//   - metadata.FileHandle: Deterministic handle for the file
//   - bool: True if path-based, false if hash-based
//
// Example:
//
//	handle, isPathBased := generateFileHandle("/export", "/images/photo.jpg")
//	// Returns: []byte("/export:/images/photo.jpg"), true
func generateFileHandle(shareName, fullPath string) (metadata.FileHandle, bool) {
	// Construct the path-based handle format
	pathBased := shareName + ":" + fullPath

	// If it fits in the NFS limit, use path-based format (preferred)
	if len(pathBased) <= maxNFSHandleSize {
		return metadata.FileHandle([]byte(pathBased)), true
	}

	// For long paths, use hash-based format
	// Format: "H:" + sha256(pathBased)[:30]
	h := sha256.Sum256([]byte(pathBased))
	handle := make([]byte, hashPrefixSize+hashHandleDataSize)
	copy(handle, []byte(hashHandlePrefix))
	copy(handle[hashPrefixSize:], h[:hashHandleDataSize])

	return metadata.FileHandle(handle), false
}

// decodeFileHandle extracts the share name and full path from a file handle.
//
// This function handles both path-based and hash-based handle formats:
//   - Path-based: Direct extraction by splitting on ":"
//   - Hash-based: Returns an error indicating lookup required
//
// For hash-based handles, the caller must perform a database lookup using
// the handle mapping key to retrieve the original path.
//
// Thread Safety: Safe for concurrent use (pure function).
//
// Parameters:
//   - handle: The file handle to decode
//
// Returns:
//   - shareName: The share name (empty if hash-based)
//   - fullPath: The full path (empty if hash-based)
//   - isHashBased: True if this is a hash-based handle requiring lookup
//   - error: Error if handle format is invalid
//
// Example:
//
//	// Path-based handle
//	share, path, isHash, err := decodeFileHandle([]byte("/export:/file.txt"))
//	// Returns: "/export", "/file.txt", false, nil
//
//	// Hash-based handle
//	share, path, isHash, err := decodeFileHandle([]byte("H:abc123..."))
//	// Returns: "", "", true, nil (caller must look up in database)
func decodeFileHandle(handle metadata.FileHandle) (shareName, fullPath string, isHashBased bool, err error) {
	if len(handle) == 0 {
		return "", "", false, fmt.Errorf("empty file handle")
	}

	// Check if this is a hash-based handle
	if len(handle) >= hashPrefixSize && string(handle[:hashPrefixSize]) == hashHandlePrefix {
		// Hash-based handle - cannot decode directly
		// Caller must look up the mapping in the database
		return "", "", true, nil
	}

	// Path-based handle - decode directly
	s := string(handle)
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", false, fmt.Errorf("invalid handle format: missing separator")
	}

	shareName = s[:idx]
	fullPath = s[idx+1:]

	// Validate the components
	if shareName == "" {
		return "", "", false, fmt.Errorf("invalid handle: empty share name")
	}
	if fullPath == "" {
		return "", "", false, fmt.Errorf("invalid handle: empty path")
	}

	return shareName, fullPath, false, nil
}

// validateFileHandle performs basic validation on a file handle.
//
// This checks:
//   - Non-empty handle
//   - Size within NFS limits (≤ 64 bytes)
//   - Valid format (can be decoded or is hash-based)
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - handle: The file handle to validate
//
// Returns:
//   - error: Error if handle is invalid, nil otherwise
func validateFileHandle(handle metadata.FileHandle) error {
	if len(handle) == 0 {
		return fmt.Errorf("empty file handle")
	}

	if len(handle) > maxNFSHandleSize {
		return fmt.Errorf("file handle too large: %d bytes (max %d)", len(handle), maxNFSHandleSize)
	}

	// Try to decode to ensure it's valid
	_, _, _, err := decodeFileHandle(handle)
	return err
}

// handleToKey converts a FileHandle to a string key for database indexing.
//
// This is a helper for using FileHandles as database keys. It converts the
// byte slice to a string using a simple cast.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - handle: The file handle to convert
//
// Returns:
//   - string: String representation for database indexing
func handleToKey(handle metadata.FileHandle) string {
	return string(handle)
}

// buildFullPath constructs the full path for a file by walking up the parent chain.
//
// This is used during file creation to determine the complete path from the root
// to the file being created. It's necessary for generating deterministic handles.
//
// Thread Safety: Must be called with appropriate locking on the store.
//
// Parameters:
//   - store: The BadgerDB store (for parent lookups)
//   - parentHandle: The parent directory handle
//   - childName: The name of the child being created
//
// Returns:
//   - string: The full path (e.g., "/images/photo.jpg")
//   - error: Error if parent chain is broken or lookup fails
//
// Example:
//
//	path, err := buildFullPath(store, dirHandle, "photo.jpg")
//	// Returns: "/images/photo.jpg" if dirHandle points to "/images"
func buildFullPath(store pathBuilder, parentHandle metadata.FileHandle, childName string) (string, error) {
	// If parent is empty, this is a root
	if len(parentHandle) == 0 {
		return "/" + childName, nil
	}

	// Try to decode the parent handle to get its path
	_, parentPath, isHash, err := decodeFileHandle(parentHandle)
	if err != nil {
		return "", fmt.Errorf("failed to decode parent handle: %w", err)
	}

	// If parent uses hash-based handle, we need to look it up
	if isHash {
		parentPath, err = store.lookupHashedHandlePath(parentHandle)
		if err != nil {
			return "", fmt.Errorf("failed to lookup hashed parent handle: %w", err)
		}
	}

	// Construct the full path
	if parentPath == "/" {
		return "/" + childName, nil
	}

	return parentPath + "/" + childName, nil
}

// buildContentID is a convenience wrapper around internal.BuildContentID.
// See internal.BuildContentID for full documentation.
func buildContentID(shareName, fullPath string) string {
	return internal.BuildContentID(shareName, fullPath)
}

// pathBuilder is an interface for looking up paths from hashed handles.
// This allows buildFullPath to work without circular dependencies.
type pathBuilder interface {
	lookupHashedHandlePath(handle metadata.FileHandle) (string, error)
}

// fileHandleToID converts a FileHandle ([]byte) to a uint64 file ID.
//
// This is used for directory entries where NFS requires a uint64 file ID.
// We compute a simple hash of the handle bytes.
//
// Note: This is not cryptographically secure, but provides a reasonable
// mapping for file IDs used in directory listings.
func fileHandleToID(handle metadata.FileHandle) uint64 {
	if len(handle) == 0 {
		return 0
	}

	// Simple FNV-1a hash
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211

	hash := uint64(offset64)
	for _, b := range handle {
		hash ^= uint64(b)
		hash *= prime64
	}

	return hash
}
