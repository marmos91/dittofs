package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
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

// ShareSession represents an active client session on a share. Sessions are
// informational only (monitoring and DUMP) and do not affect access control;
// the memory store is the only backend that tracks them.
type ShareSession struct {
	// ShareName is the name of the mounted share
	ShareName string

	// ClientAddr is the network address of the client
	ClientAddr string

	// MountedAt is the time when the share was mounted
	MountedAt time.Time
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
// The store is safe for concurrent use from multiple goroutines. A store-wide
// read-write mutex (mu) covers the metadata maps below, taken for read on
// queries and for write on mutations. It is not the only lock: quota usage
// (quotaMu), the SyncedHashStore markers (syncedMu) and each lazily built
// sub-store (lock, client, durable, recovery) carry their own, and usedBytes is
// atomic — all kept off mu so they do not contend with unrelated metadata ops.
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
//     5. Server Configuration (serverConfig):
//     Stores global server settings that apply across all shares and operations.
//
// Handle Generation:
//
// Handles encode a fresh UUID against the share name, so a handle is
// independent of the name a file is reachable under and survives renames.
// The badger backend mints handles the same way. A share name too long to
// fit the handle budget is reported as an error, never a panic; share
// creation rejects such names up front.
//
// Consistency Guarantees:
//
// The store maintains several invariants:
//   - Every file in 'files' has an entry in 'linkCounts' (≥ 1 for regular files)
//   - Every file in 'files' has an entry in 'parents' (except root directories)
//   - Every entry in 'children' corresponds to a valid file in 'files'
//   - Parent-child relationships are bidirectional (if A is parent of B, then B is in A's children)
//
// These invariants are maintained by all operations and can be verified by
// consistency checking tools.
type MemoryMetadataStore struct {
	// mu guards the metadata maps in this struct; fields carrying their own lock
	// (quotaMu, syncedMu, the lazy sub-stores) and the atomic counters are not
	// covered. Read locks are taken for queries, write locks for mutations.
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
	sessions map[string]*ShareSession

	// fileChunkData holds content-addressed file chunk tracking data.
	// Initialized lazily on first use.
	fileChunkData *fileChunkStoreData

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

	// quota tracks per-identity usage (bytes + file count) for regular files,
	// keyed by owner uid / gid. Mirror of usedBytes but keyed by owner identity
	// for per-user/per-group quota enforcement and reporting. Guarded by quotaMu
	// (separate from s.mu so the GetQuotaUsage read path and the transaction
	// commit-apply do not contend with unrelated metadata ops). Applied from a
	// transaction's pending per-identity deltas exactly once on successful
	// commit, identical to the usedBytes discipline.
	quotaMu sync.Mutex
	quota   *basestore.QuotaCache

	// storeID is the engine-persistent identifier for this store instance.
	// Assigned on construction with a fresh ULID and immutable for the life
	// of the instance.
	//
	// The memory engine is ephemeral by nature — "persistence across restart"
	// is not a meaningful clause for it — but the contract still applies at
	// the API surface: the ID must be non-empty on construction and stable
	// across calls on the same instance.
	storeID string

	// syncedMu guards `synced` for SyncedHashStore. Kept separate from
	// s.mu so per-hash sync markers do not contend with unrelated
	// metadata operations. All three SyncedHashStore methods serialize
	// here (write-lock for Mark/Delete, read-lock for IsSynced).
	syncedMu sync.RWMutex
	// synced records "has this CAS hash been mirrored to remote?".
	// Presence-of-key == synced; the time.Time value is the first-mirror
	// time. Lazily initialized on first Mark; reads treat absence as
	// not-synced.
	synced map[block.ContentHash]time.Time
	// syncedLocators records the remote block locator for synced hashes that
	// live inside a block (#1414). Standalone chunks (ChunkLocator.BlockID == "")
	// are NOT stored here — their absence resolves to the zero (standalone)
	// locator, mirroring the SQL backends' NULL block columns and badger's
	// no-suffix marker, so the common case stays free. Guarded by syncedMu.
	syncedLocators map[block.ContentHash]block.ChunkLocator

	// objectIndex maps FileAttr.ObjectID -> handle key (the same string
	// used as the key in `files`) for the dedup short-circuit
	// lookup. Populated only for non-zero ObjectIDs
	// (post-quiesce); zero entries skipped.
	//
	// Maintained inside UpdateAttrs/DeleteFile under the same store-level lock
	// (mu) that guards `files`, mirroring the fileChunkData.hashIndex
	// discipline (objects.go).
	//
	// NOTE: `fileData` carries no separate UUID field; the canonical
	// identifier in this package is the handle string (`handleToKey`
	// output). FindByObjectID resolves through this map -> files lookup
	// chain (added in).
	objectIndex map[block.ContentHash]string

	// blockRecords maps BlockID → BlockRecord. Protected by mu.
	blockRecords map[string]*block.BlockRecord
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
		capabilities:    config.Capabilities,
		maxStorageBytes: config.MaxStorageBytes,
		maxFiles:        config.MaxFiles,
		sessions:        make(map[string]*ShareSession),
		// Assign a fresh ULID on construction so every live instance
		// advertises its own non-empty identity at the API surface. Even
		// though memory-backed stores do not survive restart, the
		// identifier is stable for the lifetime of the instance.
		storeID: ulid.Make().String(),
		// ObjectID -> handle-key secondary index.
		objectIndex: make(map[block.ContentHash]string),
		// per-identity quota usage counters.
		quota: basestore.NewQuotaCache(),
		// Block packing record store.
		blockRecords: make(map[string]*block.BlockRecord),
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
		Capabilities:    basestore.DefaultCapabilities(),
		MaxStorageBytes: 0, // Unlimited (reported as 1TB)
		MaxFiles:        0, // Unlimited (reported as 1 million)
	})
}

// GetUsedBytesForShare returns the logical bytes held by one share's regular
// files. O(1) read of the per-share bucket maintained by the transaction delta
// pipeline.
func (store *MemoryMetadataStore) GetUsedBytesForShare(ctx context.Context, shareName string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	return store.quota.Share(shareName).Bytes, nil
}

// GetQuotaUsage returns per-identity usage within one share. O(1) map read
// under quotaMu. A missing key returns a zero UsageStat.
func (store *MemoryMetadataStore) GetQuotaUsage(shareName string, scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	return store.quota.Get(shareName, scope, id), nil
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
		Path:      store.derivePathLocked(handle),
		FileAttr:  attr,
	}, nil
}

// derivePathLocked reconstructs an inode's logical path by walking parent
// edges up to the share root (#1166). parents/children are the sole source of
// truth for the namespace, so the returned File.Path always reflects the
// inode's current location and can never go stale the way a stored path could
// after a hard-linked name is renamed or unlinked.
//
// For a hard-linked inode (reachable under N names) the walk is deterministic
// but arbitrary: at each level it follows the lexicographically smallest child
// name, yielding ONE valid reachable path. POSIX does not guarantee which path
// stat() reflects for a multiply-linked file, so any reachable path is correct.
// An inode with no parent edge (a share root, or an orphaned/unlinked-but-open
// inode) resolves to "/".
//
// Caller MUST hold the lock (read or write). The walk follows parent edges
// directly — at each level it scans only the parent's direct children (small)
// to recover the edge name, never the whole store. The depth guard turns a
// corrupt parent cycle (which no filesystem op can create) into a bounded
// result instead of an infinite loop.
func (store *MemoryMetadataStore) derivePathLocked(handle metadata.FileHandle) string {
	const maxDepth = 4096

	var components []string
	current := handle
	for depth := 0; depth < maxDepth; depth++ {
		key := handleToKey(current)
		parent, ok := store.parents[key]
		if !ok {
			break // reached a share root or an orphaned inode
		}

		name := store.childNameLocked(parent, current)
		if name == "" {
			// Parent edge exists but no matching child entry (inconsistent
			// state); stop rather than loop or emit an empty component.
			break
		}
		components = append(components, name)
		current = parent
	}

	if len(components) == 0 {
		return "/"
	}

	// components are leaf-first; reverse to root-first.
	var b strings.Builder
	for i := len(components) - 1; i >= 0; i-- {
		b.WriteByte('/')
		b.WriteString(components[i])
	}
	return b.String()
}

// childNameLocked returns the lexicographically smallest name under which
// child is linked into parent, or "" if no such edge exists. Caller MUST hold
// the lock.
//
// For a same-directory hard link this matches postgres' ORDER BY child_name
// edge choice. For a cross-directory hard link the backends can legitimately
// differ: postgres orders by (parent_id, child_name) across UUID-string parent
// IDs, whereas memory and badger resolve the single canonical parent edge — so
// the chosen name may differ. All choices yield a valid reachable path, which
// is all POSIX requires (cross-backend tests accept either with ||).
func (store *MemoryMetadataStore) childNameLocked(
	parent metadata.FileHandle,
	child metadata.FileHandle,
) string {
	childKey := handleToKey(child)
	childrenMap, ok := store.children[handleToKey(parent)]
	if !ok {
		return ""
	}
	best := ""
	for name, h := range childrenMap {
		if handleToKey(h) != childKey {
			continue
		}
		if best == "" || name < best {
			best = name
		}
	}
	return best
}

// rootHandleLocked returns the handle a share's root directory must be keyed
// under: the share's pre-assigned RootHandle when one exists, otherwise a
// fresh one.
//
// CreateShare generates a root handle up front so that GetRootHandle can
// succeed immediately after share creation; the root directory MUST reuse it
// so the file tree and the share's root pointer stay consistent. Without the
// reuse, CreateShare and CreateRootDirectory produce two distinct UUIDs and
// GetRootHandle ends up pointing at an empty subtree while the real tree lives
// elsewhere. Minting fails for a share name too long to encode a handle; share
// creation rejects such names up front.
//
// Thread Safety: Must be called with the write lock held.
func (store *MemoryMetadataStore) rootHandleLocked(shareName string) (metadata.FileHandle, error) {
	if sd, ok := store.shares[shareName]; ok && len(sd.RootHandle) > 0 {
		return sd.RootHandle, nil
	}
	return metadata.GenerateNewHandle(shareName)
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

// RecomputeUsage rebuilds the usage counters from the stored file records,
// discarding whatever the buckets hold. The memory store has no durable rows
// behind its counters — the records ARE the source of truth — so this is a
// walk of them under the store lock.
func (store *MemoryMetadataStore) RecomputeUsage(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// The walk drops store.mu before the seed takes quotaMu, so arm the cache to
	// record what commits in between — otherwise that transaction's delta is
	// applied and then overwritten. The two locks are never held at once.
	store.quotaMu.Lock()
	store.quota.BeginRebuild()
	store.quotaMu.Unlock()

	byIdentity := make(map[basestore.QuotaKey]*metadata.UsageStat)
	addUsage := func(k basestore.QuotaKey, bytes int64) {
		u := byIdentity[k]
		if u == nil {
			u = &metadata.UsageStat{}
			byIdentity[k] = u
		}
		u.Bytes += bytes
		u.Files++
	}

	store.mu.RLock()
	for key, fd := range store.files {
		if fd.Attr == nil || !store.chargedLocked(key, fd.Attr.Type) {
			continue
		}
		size := int64(fd.Attr.Size)
		addUsage(basestore.QuotaKey{Share: fd.ShareName, Scope: metadata.QuotaScopeUser, ID: fd.Attr.UID}, size)
		addUsage(basestore.QuotaKey{Share: fd.ShareName, Scope: metadata.QuotaScopeGroup, ID: fd.Attr.GID}, size)
	}
	store.mu.RUnlock()

	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	store.quota.Seed(byIdentity, nil)
	return nil
}

// chargedLocked reports whether the inode stored under key currently holds
// share bytes: a regular file with at least one name still reaching it. Must
// be called with store.mu held.
//
// An absent linkCounts entry means no count was ever written, which mirrors
// GetFile's default-by-type rather than "unlinked" — only an explicit
// SetLinkCount(0) puts a zero there, and that is what marks an inode as
// unlinked-but-open.
func (store *MemoryMetadataStore) chargedLocked(key string, fileType metadata.FileType) bool {
	count, ok := store.linkCounts[key]
	if !ok {
		count = 1
		if fileType == metadata.FileTypeDirectory {
			count = 2
		}
	}
	return basestore.Charged(fileType, count)
}
