package memory

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/pkg/block"
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

	// Path stores the full path within the share (e.g., "/documents/report.pdf").
	// Required for directory rename path propagation.
	Path string
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
//   - Every regular file in 'files' has an entry in 'payloadIDs'
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

	// sessions tracks active share mount sessions for monitoring and DUMP.
	// Key: composite key "shareName|clientAddr"
	// Value: ShareSession with mount timestamp
	// Note: Sessions are informational only and don't affect access control
	sessions map[string]*metadata.ShareSession

	// fileBlockData holds content-addressed file block tracking data.
	// Initialized lazily on first use.
	fileBlockData *fileBlockStoreData

	// lockStore holds persisted lock data for NLM/SMB lock persistence.
	// Initialized lazily on first use.
	lockStore *memoryLockStore

	// clientStore holds NSM client registrations for crash recovery.
	// Initialized lazily on first use.
	clientStore *memoryClientStore

	// durableStore holds SMB3 durable handle state for reconnection.
	// Initialized lazily on first use.
	durableStore *memoryDurableStore

	// recoveryStore holds NFSv4 client-recovery records for reboot/grace
	// recovery. Initialized lazily on first use.
	recoveryStore *memoryRecoveryStore

	// usedBytes tracks the total logical bytes used by regular files.
	// Updated atomically on every size-changing operation (create, update, truncate, delete).
	// Only regular files count toward usage; directories, symlinks, etc. do not.
	usedBytes atomic.Int64

	// userUsage / groupUsage track per-identity usage (bytes + file count) for
	// regular files, keyed by owner uid / gid. Mirror of usedBytes but keyed by
	// owner identity for per-user/per-group quota enforcement and reporting.
	// Guarded by quotaMu (separate from s.mu so the GetQuotaUsage read path and
	// the transaction commit-apply do not contend with unrelated metadata ops).
	// Applied from a transaction's pending per-identity deltas exactly once on
	// successful commit, identical to the usedBytes discipline.
	quotaMu    sync.Mutex
	userUsage  map[uint32]*metadata.UsageStat
	groupUsage map[uint32]*metadata.UsageStat

	// storeID is the engine-persistent identifier for this store instance.
	// Assigned on construction with a fresh ULID and immutable for the life
	// of the instance.
	//
	// The memory engine is ephemeral by nature — "persistence across restart"
	// is not a meaningful clause for it — but the contract still applies at
	// the API surface: the ID must be non-empty on construction and stable
	// across calls on the same instance.
	storeID string

	// rollupMu guards rollupOffsets for rollup_offset persistence
	// Kept separate from s.mu so rollup_offset read/compare/write
	// does not contend with unrelated metadata operations. is enforced
	// here: the read+compare+write all happen under rollupMu.
	rollupMu sync.RWMutex
	// rollupOffsets maps payloadID -> persisted rollup_offset. Lazily
	// initialized on first Set; Get treats absence as zero.
	rollupOffsets map[string]uint64

	// syncedMu guards `synced` for SyncedHashStore. Kept separate from
	// s.mu so per-hash sync markers do not contend with unrelated
	// metadata operations. All three SyncedHashStore methods serialize
	// here (write-lock for Mark/Delete, read-lock for IsSynced).
	syncedMu sync.RWMutex
	// synced records "has this CAS hash been mirrored to remote?".
	// Presence-of-key == synced; the time.Time value reserves capacity
	// for future observability without a schema change. Lazily
	// initialized on first Mark; reads treat absence as not-synced.
	synced map[block.ContentHash]time.Time

	// objectIndex maps FileAttr.ObjectID -> handle key (the same string
	// used as the key in `files`) for the dedup short-circuit
	// lookup. Populated only for non-zero ObjectIDs
	// (post-quiesce); zero entries skipped.
	//
	// Maintained inside PutFile/DeleteFile under the same store-level lock
	// (mu) that guards `files`, mirroring the fileBlockData.hashIndex
	// discipline (objects.go).
	//
	// NOTE: `fileData` carries no separate UUID field; the canonical
	// identifier in this package is the handle string (`handleToKey`
	// output). FindByObjectID resolves through this map -> files lookup
	// chain (added in).
	objectIndex map[block.ContentHash]string
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
		// Assign a fresh ULID on construction so every live instance
		// advertises its own non-empty identity at the API surface. Even
		// though memory-backed stores do not survive restart, the
		// identifier is stable for the lifetime of the instance.
		storeID: ulid.Make().String(),
		// rollup_offset persistence (see rollup.go).
		rollupOffsets: make(map[string]uint64),
		// ObjectID -> handle-key secondary index.
		objectIndex: make(map[block.ContentHash]string),
		// per-identity quota usage counters.
		userUsage:  make(map[uint32]*metadata.UsageStat),
		groupUsage: make(map[uint32]*metadata.UsageStat),
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
			PreferredReadSize:  1048576, // 1MB — matches Linux knfsd default; reduces NFS round-trips per block
			MaxWriteSize:       1048576, // 1MB
			PreferredWriteSize: 1048576, // 1MB

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

// GetUsedBytes returns the current total logical bytes used by regular files.
// This is an O(1) atomic read, safe for concurrent access without locks.
func (store *MemoryMetadataStore) GetUsedBytes() int64 {
	return store.usedBytes.Load()
}

// GetQuotaUsage returns per-identity usage for the given scope and id.
// O(1) map read under quotaMu. A missing key returns a zero UsageStat.
func (store *MemoryMetadataStore) GetQuotaUsage(scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	m := store.userUsage
	if scope == metadata.QuotaScopeGroup {
		m = store.groupUsage
	}
	if u, ok := m[id]; ok {
		return *u, nil
	}
	return metadata.UsageStat{}, nil
}

// GetStoreID returns the engine-persistent store identifier. Assigned on
// construction with a fresh ULID and immutable for the life of the instance.
//
// The memory engine is exempt from the "persistence across restart" clause
// of the GetStoreID contract, since the whole store is ephemeral. The
// instance-lifetime-stability guarantee still holds.
func (store *MemoryMetadataStore) GetStoreID() string { return store.storeID }

// Compile-time assertion: the memory engine exposes GetStoreID.
var _ interface{ GetStoreID() string } = (*MemoryMetadataStore)(nil)

// handleToKey converts a FileHandle to a string key for map indexing.
//
// FileHandle is a []byte type, which cannot be used directly as a map key
// in Go. Converts it to a string using unsafe.String to avoid
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

// buildFileWithNlink creates a File struct with the Nlink field populated from linkCounts.
// This helper ensures all returned File objects have accurate link count information.
// Thread Safety: Must be called with lock held (read or write).
func (store *MemoryMetadataStore) buildFileWithNlink(
	handle metadata.FileHandle,
	fileData *fileData,
) (*metadata.File, error) {
	// Decode handle to get ID
	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode file handle",
		}
	}

	// Get link count from internal tracking
	key := handleToKey(handle)
	nlink, exists := store.linkCounts[key]
	if !exists {
		// Default to 1 if not tracked (shouldn't happen normally)
		nlink = 1
	}
	// Note: nlink=0 is valid for files that have been unlinked but are still open
	// (NFS "silly rename" pattern where files are renamed to .nfs* instead of deleted)

	// Copy attributes and set Nlink
	attr := *fileData.Attr
	attr.Nlink = nlink
	// Deep-copy reference-bearing fields (Blocks, ACL) so a caller-side
	// in-place mutation of the returned value cannot leak into the
	// stored view.
	attr.Blocks = cloneBlocks(fileData.Attr.Blocks)
	attr.ACL = cloneACL(fileData.Attr.ACL)
	attr.EAs = cloneEAs(fileData.Attr.EAs)

	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      fileData.Path,
		FileAttr:  attr,
	}, nil
}

// generateFileHandle creates a UUID-based file handle from a share name.
//
// Generates a new UUID for each file and encodes it with the
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

// sortedChildNames returns the child names of a directory in sorted order.
//
// Thread Safety: Must be called with at least a read lock held.
func sortedChildNames(childrenMap map[string]metadata.FileHandle) []string {
	sorted := make([]string, 0, len(childrenMap))
	for name := range childrenMap {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return sorted
}

// childPageStart returns the index into sortedNames where a READDIR page should
// begin given the previous page's cursor.
//
// It uses binary search so the page starts after the cursor's lexicographic
// position even when the cursor entry itself was deleted between pages — a
// linear scan would fail to find the deleted name, reset to 0, and replay
// already-returned entries.
func childPageStart(sortedNames []string, cursor string) int {
	if cursor == "" {
		return 0
	}
	idx := sort.SearchStrings(sortedNames, cursor)
	if idx < len(sortedNames) && sortedNames[idx] == cursor {
		idx++
	}
	return idx
}
