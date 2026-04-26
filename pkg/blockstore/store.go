package blockstore

import (
	"context"
	"time"
)

// FileBlockStore defines operations for content-addressed file block management.
//
// FileBlock is the single block entity in DittoFS. Each block is content-addressed
// by its SHA-256 hash and reference-counted for dedup and GC.
type FileBlockStore interface {
	// GetFileBlock retrieves a file block by its ID.
	// Returns ErrFileBlockNotFound if not found.
	GetFileBlock(ctx context.Context, id string) (*FileBlock, error)

	// PutFileBlock stores or updates a file block.
	//
	// Upsert semantics are by ID: an INSERT for a new ID, or an UPDATE
	// when the ID already exists. The Hash column is NOT a uniqueness
	// constraint at the contract level — engines like the dedup
	// short-circuit (engine.uploadOne) WILL produce two distinct
	// FileBlock IDs sharing the same ContentHash when two file regions
	// hash-match. Backends MUST tolerate this without erroring.
	//
	// Phase 11 IN-3-02 / WR-4-01: backend implementations:
	//
	//   - memory + badger maintain hash→id maps that silently overwrite
	//     on collision (the most recent writer wins the hash index).
	//   - postgres has a non-UNIQUE partial index on (hash WHERE NOT NULL)
	//     for FindFileBlockByHash speed (see migrations 000010 and 000011).
	//     The index was UNIQUE in the original 000010 cut; that violated
	//     this contract by rejecting cross-row hash duplicates and was
	//     dropped to a regular partial index in 000011 to match the
	//     memory + badger behavior.
	//
	// The pinned contract: PutFileBlock returns nil for any
	// hash-already-present-on-another-row case. FindFileBlockByHash MAY
	// return either of the colliding rows. Callers (the dedup short-
	// circuit in engine.uploadOne) treat the lookup as best-effort —
	// they re-PUT with the donor's BlockStoreKey regardless.
	//
	// The conformance test storetest.testPutFileBlock_TwoIDsSameHash
	// pins this contract across all three backends.
	PutFileBlock(ctx context.Context, block *FileBlock) error

	// DeleteFileBlock removes a file block by its ID.
	// Returns ErrFileBlockNotFound if not found.
	DeleteFileBlock(ctx context.Context, id string) error

	// IncrementRefCount atomically increments a block's RefCount.
	IncrementRefCount(ctx context.Context, id string) error

	// DecrementRefCount atomically decrements a block's RefCount.
	// Returns the new count. When 0, the block is a GC candidate.
	DecrementRefCount(ctx context.Context, id string) (uint32, error)

	// FindFileBlockByHash looks up a finalized block by its content hash.
	// Returns nil without error if not found (used for dedup checks).
	FindFileBlockByHash(ctx context.Context, hash ContentHash) (*FileBlock, error)

	// ListLocalBlocks returns blocks that are in Local state (complete, on disk,
	// not yet synced to remote) and older than the given duration.
	// If limit > 0, at most limit blocks are returned. If limit <= 0, all are returned.
	ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*FileBlock, error)

	// ListRemoteBlocks returns blocks that are both stored locally and confirmed
	// in remote store, ordered by LRU (oldest LastAccess first), up to limit.
	ListRemoteBlocks(ctx context.Context, limit int) ([]*FileBlock, error)

	// ListUnreferenced returns blocks with RefCount=0, up to limit.
	// These are candidates for garbage collection.
	ListUnreferenced(ctx context.Context, limit int) ([]*FileBlock, error)

	// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
	// Block IDs follow the format "{payloadID}/{blockIdx}", so this method returns
	// all blocks whose ID starts with "{payloadID}/".
	// Returns empty slice (not nil) if no blocks found.
	ListFileBlocks(ctx context.Context, payloadID string) ([]*FileBlock, error)

	// EnumerateFileBlocks streams every FileBlock's ContentHash through fn in
	// implementation-defined order. Returns the first non-nil error from fn or
	// from the underlying store iterator. Implementations MUST NOT load the
	// full set into application memory — use server-side cursors or prefix
	// iterators.
	//
	// Used by the GC mark phase (Phase 11). Callers respect ctx.Done() to
	// bound iteration time. Implementations SHOULD check ctx every batch.
	//
	// Zero-valued ContentHashes (legacy rows pre-CAS) are emitted; callers
	// skip them as needed. See GC-01 / D-02.
	EnumerateFileBlocks(ctx context.Context, fn func(ContentHash) error) error
}

// Reader defines read operations on the block store.
type Reader interface {
	// ReadAt reads data from storage at the given offset into dest.
	ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error)

	// GetSize returns the stored size of a payload.
	GetSize(ctx context.Context, payloadID string) (uint64, error)

	// Exists checks whether a payload exists.
	Exists(ctx context.Context, payloadID string) (bool, error)
}

// Writer defines write operations on the block store.
type Writer interface {
	// WriteAt writes data to storage at the given offset.
	WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// Truncate changes the size of a payload.
	Truncate(ctx context.Context, payloadID string, newSize uint64) error

	// Delete removes all data for a payload.
	Delete(ctx context.Context, payloadID string) error

	// CopyPayload duplicates all blocks from srcPayloadID to dstPayloadID.
	// For remote-backed stores this leverages server-side copy (e.g., S3 CopyObject)
	// to avoid data transfer. Local blocks are copied via read+write.
	// Returns the number of blocks copied and any error encountered.
	CopyPayload(ctx context.Context, srcPayloadID, dstPayloadID string) (int, error)
}

// Flusher defines flush/sync operations on the block store.
type Flusher interface {
	// Flush ensures all dirty data for a payload is persisted.
	Flush(ctx context.Context, payloadID string) (*FlushResult, error)

	// DrainAllUploads waits for all pending uploads to complete.
	DrainAllUploads(ctx context.Context) error
}

// Store is the composed block store interface that combines all sub-interfaces
// with lifecycle and health operations.
type Store interface {
	Reader
	Writer
	Flusher

	// Stats returns storage statistics.
	Stats() (*Stats, error)

	// HealthCheck verifies the store is operational.
	HealthCheck(ctx context.Context) error

	// Start initializes the store and starts background goroutines.
	Start(ctx context.Context) error

	// Close releases resources held by the store.
	Close() error
}

// FlushResult indicates the outcome of a flush operation.
type FlushResult struct {
	// Finalized indicates all blocks have been synced to the backend store.
	Finalized bool
}

// Stats contains storage statistics.
type Stats struct {
	TotalSize     uint64 // Total storage capacity in bytes
	UsedSize      uint64 // Space consumed by content in bytes
	AvailableSize uint64 // Remaining available space in bytes
	ContentCount  uint64 // Total number of content items
	AverageSize   uint64 // Average size of content items in bytes
}

// RemoteObjectInfo describes a single remote object listed by the GC sweep
// phase via the RemoteStore.ListByPrefixWithMeta cursor (D-05). The full
// interface declaration lives in pkg/blockstore/remote/remote.go alongside
// the rest of the RemoteStore surface; this re-export documents the shape
// at the package root for readers tracing GC plumbing.
type RemoteObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// RemoteStoreSweepSurface is a documentation-only interface that captures
// the subset of remote.RemoteStore methods consumed by the GC sweep phase
// (Phase 11 Plan 06, D-05/D-07). The real interface is remote.RemoteStore
// in pkg/blockstore/remote/remote.go — this declaration exists at the
// blockstore package root so callers tracing the sweep plumbing can find
// the shape without crossing package boundaries.
//
// Implementations (memory, s3) live alongside the real interface; this
// type is never used as a parameter — the production code uses
// remote.RemoteStore directly.
type RemoteStoreSweepSurface interface {
	Delete(ctx context.Context, key string) error
	ListByPrefixWithMeta(ctx context.Context, prefix string) ([]RemoteObjectInfo, error)
}
