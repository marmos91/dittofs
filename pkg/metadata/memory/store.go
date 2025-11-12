package memory

import (
	"encoding/binary"
	"sync"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
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
// File handles are generated using UUIDs (Universally Unique Identifiers).
// This approach ensures:
//   - Uniqueness: UUIDs provide statistically guaranteed uniqueness
//   - Unpredictability: Random generation makes handles non-guessable
//   - Fixed size: All handles are exactly 16 bytes (128 bits)
//   - Stability: Handles don't change once assigned to a file
//   - Stateless: No counters or seeds to maintain, serialize, or manage
//
// The UUID approach is standard, well-understood, and eliminates the need
// for any handle generation state in the store.
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
	serverConfig metadata.ServerConfig

	// capabilities stores static filesystem capabilities and limits.
	// These are set at creation time and define what the filesystem supports.
	capabilities metadata.FilesystemCapabilities

	// maxStorageBytes is the maximum total bytes that can be stored.
	// 0 means unlimited (constrained only by available memory).
	maxStorageBytes uint64

	// maxFiles is the maximum number of files (inodes) that can be created.
	// 0 means unlimited (constrained only by available memory).
	maxFiles uint64
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
	return &MemoryMetadataStore{
		shares:          make(map[string]*shareData),
		files:           make(map[string]*fileData),
		parents:         make(map[string]metadata.FileHandle),
		children:        make(map[string]map[string]metadata.FileHandle),
		linkCounts:      make(map[string]uint32),
		pendingWrites:   make(map[string]*metadata.WriteOperation),
		capabilities:    config.Capabilities,
		maxStorageBytes: config.MaxStorageBytes,
		maxFiles:        config.MaxFiles,
	}
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
// in Go. This function converts it to a string, which is safe because:
//   - Go strings can contain arbitrary byte sequences
//   - String conversion creates an immutable copy of the bytes
//   - The conversion is deterministic (same bytes → same string)
//
// This is an internal helper used throughout the implementation to index
// into the various maps (files, parents, children, etc.).
//
// Parameters:
//   - handle: The file handle to convert
//
// Returns:
//   - string: String representation suitable for map indexing
func handleToKey(handle metadata.FileHandle) string {
	return string(handle)
}

// generateFileHandle creates a unique file handle using UUIDs.
//
// The handle generation uses UUID v4 (random UUIDs) which provides:
//
// Uniqueness:
// UUIDs are statistically guaranteed to be unique. The probability of collision
// is so low (2^-128) that it's negligible for practical purposes. This eliminates
// the need for any counter or state management.
//
// Unpredictability:
// Random UUID generation makes it computationally infeasible to predict what
// handle will be generated next, or to guess handles for unauthorized access.
// This prevents security issues where clients might try to guess handles.
//
// Fixed Size:
// All handles are exactly 16 bytes (128 bits), which provides:
//   - Consistent memory usage
//   - Predictable network serialization
//   - Sufficient space for collision-free operation
//
// Stateless:
// No counters, seeds, or other state needs to be maintained, serialized, or
// synchronized. Each call independently generates a unique handle.
//
// Protocol Compatibility:
// The 16-byte size works well with file sharing protocols:
//   - NFS: Supports variable-length file handles up to 64 bytes
//   - SMB: Uses 8-byte file IDs (we extract from first 8 bytes)
//
// Parameters:
//   - seed: A string for debugging/logging context (e.g., file path or name).
//     This parameter is not used in handle generation but can be logged for
//     troubleshooting. Pass empty string if not needed.
//
// Returns:
//   - FileHandle: A unique 16-byte file handle
func (store *MemoryMetadataStore) generateFileHandle() metadata.FileHandle {
	id := uuid.New()
	return id[:]
}

// extractFileIDFromHandle derives a 64-bit file ID from a file handle.
//
// Some protocols (like NFS and SMB) require numeric file IDs in addition to
// or instead of opaque file handles. This function extracts a stable 64-bit
// identifier from the first 8 bytes of the handle.
//
// Properties:
//   - Stable: Same handle always produces the same file ID
//   - Unique: Different handles (almost certainly) produce different IDs
//   - Efficient: Simple byte extraction, no computation required
//
// The file ID is used for:
//   - Directory entry listings (NFS READDIR, SMB directory queries)
//   - Client-side caching and reference
//   - Debugging and logging
//
// Note: File IDs are not guaranteed to be sequential or have any particular
// ordering. They are simply unique identifiers derived from handles.
//
// Parameters:
//   - handle: The file handle to extract from
//
// Returns:
//   - uint64: The extracted file ID, or 0 if handle is too short
func extractFileIDFromHandle(handle metadata.FileHandle) uint64 {
	if len(handle) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(handle[:8])
}
