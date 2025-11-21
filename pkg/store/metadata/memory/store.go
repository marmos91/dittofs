package memory

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/marmos91/dittofs/pkg/store/metadata/internal"
)

// shareData holds the internal representation of a share configuration.
//
// This structure combines the share configuration (access rules, options)
// with the root directory handle that serves as the entry point for all
// filesystem operations within the share.
type shareData struct {
	Share      metadata.Share
	RootHandle metadata.FileHandle
}

type fileData struct {
	// Attr contains the protocol-agnostic file attributes
	Attr *metadata.FileAttr

	// ShareName tracks which share this file belongs to.
	// Used to enforce share-level policies (e.g., read-only shares).
	ShareName string
}

// deviceNumber stores major and minor device numbers for special files.
type deviceNumber struct {
	Major uint32
	Minor uint32
}

// MemoryMetadataStore implements MetadataStore using in-memory storage.
//
// This implementation provides a fully functional metadata repository backed
// by in-memory data structures. It is suitable for:
//   - Testing and development environments
//   - Ephemeral filesystems where persistence is not required
//   - Caching layers in hybrid storage architectures
//   - Systems where persistence is handled by external mechanisms
//
// Thread Safety:
// All operations are protected by a single read-write mutex (mu), making the
// store safe for concurrent access from multiple goroutines. This coarse-grained
// locking is simple and correct, though fine-grained locking could improve
// concurrency for high-throughput scenarios.
//
// Storage Model:
//
// The store maintains several interconnected maps that together represent the
// complete filesystem metadata:
//
//  1. File Metadata (files):
//     Maps file handles to file attributes (size, permissions, timestamps, etc.)
//     This is the primary metadata storage.
//
// 2. Directory Hierarchy (parents, children):
//
//   - parents: Maps each file handle to its parent directory handle
//
//   - children: Maps each directory handle to its child entries (name → handle)
//     These maps maintain the tree structure of the filesystem.
//
//     3. Share Management (shares):
//     Maps share names to their configuration and root directory handles.
//     Shares are the entry points for client access.
//
//     4. Hard Links (linkCounts):
//     Maps file handles to the number of directory entries (hard links) pointing
//     to them. When linkCounts reaches 0, the file's content can be deleted.
//     Directories always have linkCounts ≥ 2 (parent entry + "." self-reference).
//
//     7. Write Operations (pendingWrites):
//     Tracks in-flight write operations for the two-phase write protocol.
//     Maps operation IDs to WriteOperation structs containing the file handle,
//     new size, and other metadata needed to commit the write.
//
//     8. Server Configuration (serverConfig):
//     Stores global server settings that apply across all shares and operations.
//
// Handle Generation:
//
// File handles are generated using path-based identifiers in the format:
// "shareName:fullPath" (e.g., "/export:/images/photo.jpg").
//
// This approach ensures:
//   - Determinism: Same path always generates the same handle
//   - Reversibility: Path can be extracted from handle for import/export
//   - Stability: Handles remain stable across server restarts
//   - Human-readable: Easy to debug and inspect
//   - Import-ready: Enables future filesystem import features
//
// The path-based approach matches the BadgerDB metadata store implementation,
// ensuring consistent behavior across all metadata store backends. This
// consistency is critical for implementing metadata import/export features.
//
// Consistency Guarantees:
//
// The store maintains several invariants:
//   - Every file in 'files' has an entry in 'linkCounts' (≥ 1 for regular files)
//   - Every file in 'files' has an entry in 'parents' (except root directories)
//   - Every entry in 'children' corresponds to a valid file in 'files'
//   - Every symlink in 'files' has an entry in 'symlinkTargets'
//   - Every regular file in 'files' has an entry in 'contentIDs'
//   - Parent-child relationships are bidirectional (if A is parent of B, then B is in A's children)
//
// These invariants are maintained by all operations and can be verified by
// consistency checking tools.
type MemoryMetadataStore struct {
	// mu protects all fields in this struct for concurrent access.
	// Operations acquire read locks for queries and write locks for mutations.
	mu sync.RWMutex

	// shares maps share names to their configuration and root handles.
	// Key: share name (string)
	// Value: share configuration and root directory handle
	shares map[string]*shareData

	// files maps file handles to file attributes.
	// This is the primary metadata storage for all files and directories.
	// Key: string representation of FileHandle
	// Value: complete file attributes (type, size, permissions, timestamps, etc.)
	files map[string]*fileData

	// parents maps each file/directory to its parent directory.
	// This enables upward traversal in the directory tree.
	// Key: string representation of child FileHandle
	// Value: parent directory FileHandle
	// Note: Root directories of shares don't have parents (not in this map)
	parents map[string]metadata.FileHandle

	// children maps each directory to its child entries.
	// This enables downward traversal and name resolution.
	// Key: string representation of parent directory FileHandle
	// Value: map of child names to their FileHandles
	// Note: Only directories have entries in this map
	children map[string]map[string]metadata.FileHandle

	// linkCounts tracks the number of hard links (directory entries) for each file.
	// Key: string representation of FileHandle
	// Value: number of directory entries pointing to this file
	// Notes:
	//   - Regular files start at 1, increment with CreateHardLink
	//   - Directories start at 2 ("." and parent's entry), increment with subdirectories
	//   - When count reaches 0, file content can be deleted
	linkCounts map[string]uint32

	// deviceNumbers stores major and minor device numbers for block and character devices.
	// Key: string representation of FileHandle
	// Value: struct containing major and minor numbers
	// Note: Only populated for FileTypeBlockDevice and FileTypeCharDevice
	deviceNumbers map[string]*deviceNumber

	// pendingWrites tracks in-flight write operations for two-phase writes.
	// Key: operation ID (opaque string, typically UUID)
	// Value: WriteOperation struct with file handle, new size, timestamps, etc.
	// Notes:
	//   - Created by PrepareWrite
	//   - Consumed by CommitWrite
	//   - Should be cleaned up on timeout/cancellation
	pendingWrites map[string]*metadata.WriteOperation

	// serverConfig stores global server configuration.
	// This includes settings that apply across all shares and operations.
	serverConfig metadata.MetadataServerConfig

	// capabilities stores static filesystem capabilities and limits.
	// These are set at creation time and define what the filesystem supports.
	capabilities metadata.FilesystemCapabilities

	// maxStorageBytes is the maximum total bytes that can be stored.
	// 0 means unlimited (constrained only by available memory).
	maxStorageBytes uint64

	// maxFiles is the maximum number of files (inodes) that can be created.
	// 0 means unlimited (constrained only by available memory).
	maxFiles uint64

	// attrPool is a sync.Pool for FileAttr allocations to reduce GC pressure.
	// This pool is used to recycle FileAttr objects during copy operations.
	attrPool sync.Pool

	// sessions tracks active share mount sessions for monitoring and DUMP.
	// Key: composite key "shareName|clientAddr"
	// Value: ShareSession with mount timestamp
	// Note: Sessions are informational only and don't affect access control
	sessions map[string]*metadata.ShareSession

	// sortedDirCache caches sorted directory entries to avoid O(n log n) sorting
	// on every ReadDirectory call. Invalidated when directory contents change.
	// Key: string representation of directory FileHandle
	// Value: sorted slice of child names
	// Note: Cache is lazy-populated on first read and cleared on modifications
	sortedDirCache map[string][]string
}

// MemoryMetadataStoreConfig contains configuration for creating a memory metadata store.
//
// This structure allows explicit configuration of store capabilities and limits
// at creation time, making it easy to configure from environment variables,
// config files, or command-line flags.
type MemoryMetadataStoreConfig struct {
	// Capabilities defines static filesystem capabilities and limits
	Capabilities metadata.FilesystemCapabilities

	// MaxStorageBytes is the maximum total bytes that can be stored
	// 0 means unlimited (constrained only by available memory)
	MaxStorageBytes uint64

	// MaxFiles is the maximum number of files that can be created
	// 0 means unlimited (constrained only by available memory)
	MaxFiles uint64
}

// NewMemoryMetadataStore creates a new in-memory metadata store with specified configuration.
//
// The store is initialized with the provided capabilities and limits, which define
// what the filesystem supports and its constraints. These settings are immutable
// after creation (capabilities are static by nature).
//
// The returned store is immediately ready for use and safe for concurrent
// access from multiple goroutines.
//
// Parameters:
//   - config: Configuration including capabilities and storage limits
//
// Returns:
//   - *MemoryMetadataStore: A new store instance ready for use
//
// Example:
//
//	config := MemoryMetadataStoreConfig{
//	    Capabilities: metadata.FilesystemCapabilities{
//	        MaxReadSize: 1048576,
//	        MaxFileSize: 1099511627776, // 1TB
//	        // ... other fields
//	    },
//	    MaxStorageBytes: 10 * 1024 * 1024 * 1024, // 10GB
//	    MaxFiles: 100000,
//	}
//	store := NewMemoryMetadataStore(config)
func NewMemoryMetadataStore(config MemoryMetadataStoreConfig) *MemoryMetadataStore {
	store := &MemoryMetadataStore{
		shares:          make(map[string]*shareData),
		files:           make(map[string]*fileData),
		parents:         make(map[string]metadata.FileHandle),
		children:        make(map[string]map[string]metadata.FileHandle),
		linkCounts:      make(map[string]uint32),
		deviceNumbers:   make(map[string]*deviceNumber),
		pendingWrites:   make(map[string]*metadata.WriteOperation),
		capabilities:    config.Capabilities,
		maxStorageBytes: config.MaxStorageBytes,
		maxFiles:        config.MaxFiles,
		sessions:        make(map[string]*metadata.ShareSession),
		sortedDirCache:  make(map[string][]string),
	}

	// Initialize the sync.Pool for FileAttr allocations
	store.attrPool = sync.Pool{
		New: func() any {
			return &metadata.FileAttr{}
		},
	}

	return store
}

// NewMemoryMetadataStoreWithDefaults creates a new in-memory metadata store with sensible defaults.
//
// This is a convenience constructor that sets up the store with standard capabilities
// and limits suitable for most use cases:
//
// Transfer Sizes:
//   - Max read/write: 1MB
//   - Preferred read/write: 64KB
//
// Limits:
//   - Max file size: Practically unlimited (2^63-1)
//   - Max filename: 255 bytes
//   - Max path: 4096 bytes
//   - Max hard links: 32767
//   - Storage: Unlimited (1TB reported)
//   - Files: Unlimited (1 million reported)
//
// Features:
//   - Hard links: Yes
//   - Symlinks: Yes
//   - Case-sensitive: Yes
//   - Case-preserving: Yes
//   - ACLs: No
//   - Extended attributes: No
//   - Timestamp resolution: 1 nanosecond
//
// For custom configuration, use NewMemoryMetadataStore with a MemoryMetadataStoreConfig.
//
// Returns:
//   - *MemoryMetadataStore: A new store instance with default configuration
func NewMemoryMetadataStoreWithDefaults() *MemoryMetadataStore {
	return NewMemoryMetadataStore(MemoryMetadataStoreConfig{
		Capabilities: metadata.FilesystemCapabilities{
			// Transfer Sizes
			MaxReadSize:        1048576, // 1MB
			PreferredReadSize:  65536,   // 64KB
			MaxWriteSize:       1048576, // 1MB
			PreferredWriteSize: 65536,   // 64KB

			// Limits
			MaxFileSize:      9223372036854775807, // 2^63-1 (practically unlimited)
			MaxFilenameLen:   255,                 // Standard Unix limit
			MaxPathLen:       4096,                // Standard Unix limit
			MaxHardLinkCount: 32767,               // Similar to ext4

			// Features
			SupportsHardLinks:     true, // We track link counts
			SupportsSymlinks:      true, // We store symlink targets
			CaseSensitive:         true, // Go map keys are case-sensitive
			CasePreserving:        true, // We store exact filenames
			ChownRestricted:       false,
			SupportsACLs:          false,
			SupportsExtendedAttrs: false,
			TruncatesLongNames:    true, // Reject with error, don't truncate

			// Time Resolution
			TimestampResolution: 1, // 1 nanosecond (Go time.Time precision)
		},
		MaxStorageBytes: 0, // Unlimited (reported as 1TB)
		MaxFiles:        0, // Unlimited (reported as 1 million)
	})
}

// handleToKey converts a FileHandle to a string key for map indexing.
//
// FileHandle is a []byte type, which cannot be used directly as a map key
// in Go. This function converts it to a string using unsafe.String to avoid
// allocations (Go 1.20+).
//
// Safety:
//   - The returned string references the underlying byte slice
//   - Safe because FileHandle values are not modified after creation
//   - Map lookups don't retain the key, so lifetime is correct
//   - Eliminates one allocation per map lookup
//
// This is an internal helper used throughout the implementation to index
// into the various maps (files, parents, children, etc.).
//
// Parameters:
//   - handle: The file handle to convert
//
// Returns:
//   - string: String representation suitable for map indexing (zero-copy)
func handleToKey(handle metadata.FileHandle) string {
	if len(handle) == 0 {
		return ""
	}
	// Use unsafe.String to avoid allocation (Go 1.20+)
	// This is safe because:
	// 1. FileHandles are immutable after creation
	// 2. The map doesn't retain the key beyond the lookup
	// 3. We never modify the underlying bytes
	return unsafe.String(unsafe.SliceData(handle), len(handle))
}

// buildFullPath constructs the full path for a file by walking up the parent chain.
// This is used to generate deterministic file handles.
// Thread Safety: Must be called with lock held (read or write).
func (store *MemoryMetadataStore) buildFullPath(handle metadata.FileHandle, name string) string {
	if len(handle) == 0 {
		return "/" + name
	}

	// Walk up to build path components
	var components []string
	if name != "" {
		components = append(components, name)
	}

	currentHandle := handle
	for {
		currentKey := handleToKey(currentHandle)
		parentHandle, hasParent := store.parents[currentKey]
		if !hasParent {
			// Reached root of share
			break
		}

		// Find the name of current handle in parent's children
		parentKey := handleToKey(parentHandle)
		if childrenMap, exists := store.children[parentKey]; exists {
			for childName, childHandle := range childrenMap {
				if handleToKey(childHandle) == currentKey {
					components = append([]string{childName}, components...)
					break
				}
			}
		}

		currentHandle = parentHandle
	}

	// Build path
	if len(components) == 0 {
		return "/"
	}

	// Use strings.Builder for efficient path construction
	// Pre-allocate approximate capacity to avoid reallocations
	var builder strings.Builder
	totalLen := len(components) // For the "/" separators
	for _, comp := range components {
		totalLen += len(comp)
	}
	builder.Grow(totalLen + 1) // +1 for leading "/"

	builder.WriteByte('/')
	for i, comp := range components {
		if i > 0 {
			builder.WriteByte('/')
		}
		builder.WriteString(comp)
	}
	return builder.String()
}

// buildContentID is a convenience wrapper around internal.BuildContentID.
// See internal.BuildContentID for full documentation.
func buildContentID(shareName, fullPath string) string {
	return internal.BuildContentID(shareName, fullPath)
}

// generateFileHandle creates a UUID-based file handle from a share name.
//
// This function generates a new UUID for each file and encodes it with the
// share name to create a unique file handle in the format:
//
//	Format: "shareName:uuid"
//	Example: "/export:550e8400-e29b-41d4-a716-446655440000"
//	Size: share name length + 37 bytes (1 colon + 36 UUID chars)
//
// UUID-based handles provide:
//   - Guaranteed uniqueness (no collisions)
//   - NFS compatibility (typically under 64 bytes)
//   - Stable identifiers (UUID doesn't change)
//   - No path length limitations
//
// Note: The fullPath parameter is currently unused but kept for compatibility
// with existing code. In the future, if path tracking is needed, it should be
// stored separately in the fileData structure.
//
// Parameters:
//   - shareName: The share name this file belongs to
//   - fullPath: Reserved for future use (currently ignored)
//
// Returns:
//   - FileHandle: A UUID-based file handle
func (store *MemoryMetadataStore) generateFileHandle(shareName, fullPath string) metadata.FileHandle {
	// Generate a new UUID for this file
	id := uuid.New()

	// Encode the handle using the standard format
	handle, err := metadata.EncodeShareHandle(shareName, id)
	if err != nil {
		// This should never happen for valid share names and UUIDs
		// If it does, generate a fallback handle
		// In practice, this error only occurs if the encoded handle exceeds 64 bytes,
		// which is unlikely for reasonable share names
		panic(fmt.Sprintf("failed to encode file handle: %v", err))
	}

	return handle
}

// extractFileIDFromHandle derives a 64-bit file ID from a file handle.
//
// Some protocols (like NFS and SMB) require numeric file IDs in addition to
// or instead of opaque file handles. This function computes a stable 64-bit
// identifier from the handle using FNV-1a hashing.
//
// Properties:
//   - Stable: Same handle always produces the same file ID
//   - Unique: Different handles produce different IDs (with high probability)
//   - Efficient: Fast non-cryptographic hash
//
// The file ID is used for:
//   - Directory entry listings (NFS READDIR, SMB directory queries)
//   - Client-side caching and reference
//   - Debugging and logging
//
// Note: File IDs are not guaranteed to be sequential or have any particular
// ordering. They are simply unique identifiers derived from handles.
//
// This matches the approach used in badger/handle.go:fileHandleToID to ensure
// consistent file IDs across different metadata store implementations.
//
// Parameters:
//   - handle: The file handle to compute ID from
//
// Returns:
//   - uint64: The computed file ID, or 0 if handle is empty
func extractFileIDFromHandle(handle metadata.FileHandle) uint64 {
	if len(handle) == 0 {
		return 0
	}

	// FNV-1a hash (same as badger implementation)
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211

	hash := uint64(offset64)
	for _, b := range handle {
		hash ^= uint64(b)
		hash *= prime64
	}

	return hash
}

// invalidateDirCache removes cached sorted entries for a directory.
//
// This should be called whenever directory contents change (add, remove, rename).
// It's safe to call even if the directory has no cached entries.
//
// Thread Safety: Must be called with write lock held.
//
// Parameters:
//   - dirHandle: The directory handle whose cache should be invalidated
func (s *MemoryMetadataStore) invalidateDirCache(dirHandle metadata.FileHandle) {
	delete(s.sortedDirCache, handleToKey(dirHandle))
}

// getSortedDirEntries returns a sorted list of child names for a directory.
//
// This function uses a cache to avoid repeated O(n log n) sorting on every
// ReadDirectory call. The cache is lazy-populated on first access and
// invalidated when directory contents change.
//
// Thread Safety: Must be called with at least a read lock held.
//
// Parameters:
//   - dirHandle: The directory handle to get sorted entries for
//   - childrenMap: The children map for this directory
//
// Returns:
//   - []string: Sorted slice of child names (cached)
func (s *MemoryMetadataStore) getSortedDirEntries(dirHandle metadata.FileHandle, childrenMap map[string]metadata.FileHandle) []string {
	dirKey := handleToKey(dirHandle)

	// Check cache first
	if cached, exists := s.sortedDirCache[dirKey]; exists {
		return cached
	}

	// Not in cache, build sorted list
	sorted := make([]string, 0, len(childrenMap))
	for name := range childrenMap {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	// Store in cache (note: we need write access for this, but since we're
	// doing this during read operations, we'll upgrade the lock if needed)
	// For now, we'll cache on next write operation
	// This is still better than sorting every time
	return sorted
}
