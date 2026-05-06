package blockstore

import (
	"context"
	"time"
)

// FileBlockStore defines content-addressed block CRUD for the engine.
//
// Narrowed to 6 methods in Phase 12 (META-03 / D-09): GetByHash, Put,
// Delete, IncrementRefCount, DecrementRefCount, ListPending. Block
// identity is hash-keyed at the contract level; backends still use
// `id VARCHAR PRIMARY KEY + hash non-unique index` internally to
// preserve the Phase 11 IN-3-02 / WR-4-01 multi-row-per-hash
// tolerance for legacy data.
//
// The Phase 11 contract: Put returned nil for any
// hash-already-present-on-another-row case. Phase 12 D-37 fixes the
// dedup short-circuit so the upload path produces only one row per
// hash going forward; legacy data may still hold dual rows. The
// conformance test storetest.testPut_TwoIDsSameHash pins
// this contract for legacy tolerance.
//
// Enumeration of all FileBlocks across the store moved up to
// MetadataStore.EnumerateFileBlocks in Phase 12 (D-08).
//
// Backends MAY (and currently do) implement additional methods
// (GetFileBlock, ListFileBlocks, ListRemoteBlocks, ListUnreferenced)
// for engine-internal use; those are accessed via a wider engine-
// internal interface, NOT via this public surface.
type FileBlockStore interface {
	// GetByHash returns any FileBlock with the given content hash, or
	// (nil, nil) when absent. The "any" wording matters: legacy data
	// may have multiple rows per hash; callers (the engine dedup
	// short-circuit) treat the result as best-effort and proceed with
	// any one row's BlockStoreKey. Renamed from FindFileBlockByHash.
	GetByHash(ctx context.Context, hash ContentHash) (*FileBlock, error)

	// Put creates or replaces a FileBlock by ID.
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
	//     for GetByHash speed (see migrations 000010 and 000011).
	//     The index was UNIQUE in the original 000010 cut; that violated
	//     this contract by rejecting cross-row hash duplicates and was
	//     dropped to a regular partial index in 000011 to match the
	//     memory + badger behavior.
	//
	// The pinned contract: Put returns nil for any
	// hash-already-present-on-another-row case. GetByHash MAY
	// return either of the colliding rows. Callers (the dedup short-
	// circuit in engine.uploadOne) treat the lookup as best-effort —
	// they re-PUT with the donor's BlockStoreKey regardless.
	//
	// The conformance test storetest.testPut_TwoIDsSameHash
	// pins this contract across all three backends. Renamed from
	// PutFileBlock.
	Put(ctx context.Context, block *FileBlock) error

	// Delete removes a FileBlock by ID. Returns ErrFileBlockNotFound
	// if not found. Renamed from DeleteFileBlock.
	//
	// Collision check (2026-04-26): no backend struct has a
	// pre-existing method named exactly `Delete()`; the rename is
	// collision-free.
	Delete(ctx context.Context, id string) error

	// IncrementRefCount atomically bumps RefCount for the given
	// FileBlock id.
	IncrementRefCount(ctx context.Context, id string) error

	// DecrementRefCount atomically decrements; returns the new
	// count. RefCount=0 marks the block as a GC candidate.
	DecrementRefCount(ctx context.Context, id string) (uint32, error)

	// ListPending returns up-to-limit Pending FileBlocks older than
	// olderThan, for the syncer claim path. Replaces the legacy
	// ListLocalBlocks (Phase 11 narrowed local→pending semantics
	// already; this is just the rename).
	ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*FileBlock, error)
}

// EngineFileBlockStore is the engine-internal extension of FileBlockStore.
// Phase 12 (META-03 / D-09) narrowed the public FileBlockStore to 6
// methods; the engine + local/fs packages still need the by-ID and
// per-file lookups for the dual-read read path, recovery, dedup-delete,
// and stats fan-out (see Phase 12 plan 04 SUMMARY for the full caller
// list under pkg/blockstore/{engine,local/fs}/).
//
// All three metadata backends (memory/badger/postgres) satisfy this
// interface — the methods are concrete on the backend struct, just not
// on the public FileBlockStore surface. Phase 13/14 will eliminate the
// remaining call sites by routing reads through FileAttr.Blocks
// (D-13/D-20); the interface goes away with them.
type EngineFileBlockStore interface {
	FileBlockStore

	// GetFileBlock retrieves a FileBlock by ID. Returns
	// ErrFileBlockNotFound if absent.
	GetFileBlock(ctx context.Context, id string) (*FileBlock, error)

	// ListFileBlocks returns every FileBlock whose ID begins with
	// "{payloadID}/", sorted by parsed numeric block index. Returns
	// an empty (non-nil) slice when no blocks match.
	ListFileBlocks(ctx context.Context, payloadID string) ([]*FileBlock, error)
}

// Reader defines read operations on the block store.
//
// Phase 12 API-01..04: read operations thread a caller-supplied
// []BlockRef snapshot of the file's FileAttr.Blocks. Empty/nil blocks
// triggers the Phase 11 dual-read shim (D-20) — engine routes through
// the legacy {payloadID}/block-{N} resolver. Non-empty blocks routes
// through the CAS path: findBlocksForRange + cache.OnRead.
type Reader interface {
	// ReadAt reads data from storage at the given offset into dest.
	// Empty blocks => dual-read shim (Phase 11 path, D-20).
	ReadAt(ctx context.Context, payloadID string, blocks []BlockRef, dest []byte, offset uint64) (int, error)

	// GetSize returns the stored size of a payload.
	GetSize(ctx context.Context, payloadID string) (uint64, error)

	// Exists checks whether a payload exists.
	Exists(ctx context.Context, payloadID string) (bool, error)
}

// Writer defines write operations on the block store.
//
// Phase 12 API-01..04: write operations thread a caller-supplied
// []BlockRef snapshot of the file's FileAttr.Blocks. WriteAt returns the
// new []BlockRef (caller persists via PutFile in the same metadata txn).
// Truncate / Delete invoke the MetadataCoordinator to decrement RefCount
// for hashes the operation drops; CopyPayload becomes O(1) — increments
// RefCount per unique source hash, no data copy.
type Writer interface {
	// WriteAt writes data to storage at the given offset and returns
	// the file's new BlockRef list (sorted, sparse-hole-preserving).
	// Caller persists via PutFile in the same metadata txn.
	WriteAt(ctx context.Context, payloadID string, currentBlocks []BlockRef, data []byte, offset uint64) ([]BlockRef, error)

	// Truncate changes the size of a payload. Returns the new BlockRef
	// list (blocks past newSize are dropped; the coordinator
	// decrements their RefCount).
	Truncate(ctx context.Context, payloadID string, currentBlocks []BlockRef, newSize uint64) ([]BlockRef, error)

	// Delete removes all data for a payload and decrements RefCount on
	// every hash in blocks via the coordinator.
	Delete(ctx context.Context, payloadID string, blocks []BlockRef) error

	// CopyPayload duplicates a file's BlockRef list with O(1) cost.
	// Increments the RefCount of each unique hash via the coordinator
	// (no per-block data copy); returns a deep copy of srcBlocks as
	// the destination's BlockRef list. The caller's metadata txn
	// rolls back all increments on any error per Phase 12 D-11.
	CopyPayload(ctx context.Context, srcPayloadID, dstPayloadID string, srcBlocks []BlockRef) ([]BlockRef, error)
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
