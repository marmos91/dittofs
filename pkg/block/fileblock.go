package block

import (
	"context"
	"time"
)

// FileBlockStore defines content-addressed block CRUD for the engine.
//
// 7 methods: GetByHash, Put, Delete, IncrementRefCount,
// DecrementRefCount, ListPending, AddRef. Block identity is hash-keyed
// at the contract level; backends still use `id VARCHAR PRIMARY KEY +
// hash non-unique index` internally to preserve the
// multi-row-per-hash tolerance for legacy data.
//
// Contract: Put returns nil for any hash-already-present-on-another-
// row case. The upload path produces only one row per hash going
// forward; legacy data may still hold dual rows. The conformance test
// storetest.testPut_TwoIDsSameHash pins this contract for legacy
// tolerance.
//
// Enumeration of all FileBlocks across the store has moved up to
// MetadataStore.EnumerateFileBlocks.
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
	// backend implementations
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

	// DecrementRefCountAndReap atomically decrements RefCount for the FileBlock
	// id and, IF the new count is 0, deletes the row (and its hash index entry)
	// in the SAME critical section as the decrement — TOCTOU-free against a
	// concurrent AddRef the same way IncrementRefCount/AddRef are. Returns the
	// new count (0 when reaped or when the row was already absent).
	// ErrFileBlockNotFound is tolerated and reported as count 0 (a row already
	// swept is not a caller error). Used by the engine Delete/Truncate reclaim
	// path so that, once a hash has no live references, it leaves
	// EnumerateFileBlocks and the GC sweep can collect the remote chunk.
	DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error)

	// AddRef atomically increments RefCount on the FileBlock row
	// indexed by hash. Used by the in-memory hash dedup LRU hit path.
	//
	// On success, RefCount is incremented; BlockState is UNCHANGED
	// (no Pending→Syncing→Remote transition; no new row created).
	// This is the load-bearing contract: the LRU hit path references
	// an existing block — it never creates one.
	//
	// Returns ErrUnknownHash if no FileBlock row exists for the given
	// hash. Callers (see pkg/block/local/fs/rollup.go LRU hit
	// path) MUST fall back to the full Put path on this sentinel —
	// the LRU may be ahead of the metadata store after a crash, or
	// the hash may not be present yet.
	//
	// Atomicity matches IncrementRefCount's contract: the increment
	// is performed under the backend's native concurrency primitive
	// (mutex / Badger txn / Postgres conditional UPDATE) so AddRef
	// is TOCTOU-free against concurrent DecrementRefCount cascade
	// (the dedup hit path otherwise races engine.Delete).
	//
	// Multi-row-per-hash tolerance
	// AddRef MAY operate on any one matching row when more than one
	// row shares the hash (legacy data + dedup short-circuit). The
	// caller's BlockRef contract is satisfied either way — RefCount
	// is a per-row property, and any non-zero RefCount keeps the row
	// alive past GC.
	//
	// Invariant preserved: AddRef references an existing block; the
	// LRU hit path never creates a new block, so the "every block
	// must visit Pending" rule is not contradicted. No new block row
	// is materialized on success or on the ErrUnknownHash failure
	// path.
	//
	// payloadID and blockRef are passed for backend-side
	// observability (logging, tracing) and to allow future
	// multi-row-per-hash backends to choose which row to bump; they
	// are NOT part of the persisted state.
	AddRef(ctx context.Context, hash ContentHash, payloadID string, blockRef BlockRef) error

	// ListPending returns up-to-limit Pending FileBlocks older than
	// olderThan, for the syncer claim path. Replaces the legacy
	// ListLocalBlocks (narrowed local→pending semantics already; this
	// is just the rename).
	ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*FileBlock, error)
}

// EngineFileBlockStore is the engine-internal extension of
// FileBlockStore. The engine + local/fs packages still need by-ID
// and per-file lookups for the dual-read read path, recovery,
// dedup-delete and stats fan-out (callers under
// pkg/block/{engine,local/fs}/).
//
// All three metadata backends (memory/badger/postgres) satisfy this
// interface — the methods are concrete on the backend struct, just
// not on the public FileBlockStore surface. Future work will
// eliminate the remaining call sites by routing reads through
// FileAttr.Blocks, and this interface will go away with them.
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
// read operations thread a caller-supplied
// []BlockRef snapshot of the file's FileAttr.Blocks. Empty/nil blocks
// triggers the dual-read shim — engine routes through
// the legacy {payloadID}/block-{N} resolver. Non-empty blocks routes
// through the CAS path: findBlocksForRange + cache.OnRead.
type Reader interface {
	// ReadAt reads data from storage at the given offset into dest.
	// Empty blocks => dual-read shim (path).
	ReadAt(ctx context.Context, payloadID string, blocks []BlockRef, dest []byte, offset uint64) (int, error)

	// GetSize returns the stored size of a payload.
	GetSize(ctx context.Context, payloadID string) (uint64, error)

	// Exists checks whether a payload exists.
	Exists(ctx context.Context, payloadID string) (bool, error)
}

// Writer defines write operations on the block store.
//
// write operations thread a caller-supplied
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
	// rolls back all increments on any error.
	CopyPayload(ctx context.Context, srcPayloadID, dstPayloadID string, srcBlocks []BlockRef) ([]BlockRef, error)
}

// Flusher defines flush/sync operations on the block store.
type Flusher interface {
	// Flush quiesces the payload's local-side state and (when a healthy
	// remote is configured) mirrors every locally stored CAS chunk to
	// the remote store. Return-value contract:
	//
	//   - (Finalized=true, nil)
	//     All locally-mirrored data for payloadID is durable on the
	//     configured remote. Callers may report COMMIT/Flush success
	//     to the client.
	//
	//   - (Finalized=false, nil)
	//     A NON-fatal soft condition prevented finalization THIS call:
	//     no remote is configured (local-only mode — local quiesce
	//     completed but no remote durability target exists), the remote
	//     is configured but currently unhealthy, or another in-flight
	//     mirror pass (periodic uploader or overlapping Flush) is
	//     already running. The dirty state is unchanged and will be
	//     re-attempted on the next Flush or the next periodic uploader
	//     tick.
	//
	//     Callers driving NFS COMMIT or SMB Flush loops MUST rate-limit
	//     their retries against this branch. A tight retry storm on
	//     Finalized=false starves the uploading goroutine (the
	//     CompareAndSwap gate in syncer.Flush makes the explicit caller
	//     LOSE every retry attempt against the periodic uploader's
	//     in-flight tick) and pegs the CPU without making progress.
	//     Recommended pattern: surface the soft-fail to the protocol
	//     adapter and let the client drive the next attempt on its own
	//     schedule (e.g. NFSv3 reports the WRITE's "committed" enum as
	//     UNSTABLE rather than DATASYNC/FILESYNC so the client reissues
	//     COMMIT later; SMB Flush returns success after a bounded
	//     attempt) rather than spin in-handler.
	//
	//   - (nil, err)
	//     Hard failure (I/O error, remote.Put rejection, MarkSynced
	//     metadata error). Do NOT retry until the underlying condition
	//     is addressed; the caller should surface a protocol-level
	//     error to the client.
	Flush(ctx context.Context, payloadID string) (*FlushResult, error)

	// DrainAllUploads waits for all pending uploads to complete.
	DrainAllUploads(ctx context.Context) error
}

// ComposedStore is the composed block store interface that combines all sub-interfaces
// with lifecycle and health operations.
type ComposedStore interface {
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

// FlushResult indicates the outcome of a flush operation. See the Flush
// method on Flusher for the full (Finalized, err) state-machine and
// caller-retry guidance.
type FlushResult struct {
	// Finalized indicates all blocks have been synced to the backend
	// store. When false alongside a nil error, the call hit a soft
	// non-fatal condition (remote unhealthy or another mirror pass
	// already in flight); the dirty state is unchanged and will be
	// re-attempted by the next Flush or the periodic uploader. Callers
	// MUST NOT spin-retry on Finalized=false — see Flusher.Flush godoc.
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
