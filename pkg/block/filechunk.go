package block

import (
	"context"
)

// FileChunkStore defines content-addressed block CRUD for the engine.
//
// methods: GetByHash, Put, Delete, IncrementRefCount,
// DecrementRefCount, DecrementRefCountAndReap, AddRef. Rows are keyed by ID,
// not by hash: backends use `id VARCHAR PRIMARY KEY + hash non-unique index`,
// and hash is a lossy secondary index over an ID-keyed table.
//
// decision: two rows may share one ContentHash, and this is the live steady
// state rather than a tolerance for old data. A row's ID is
// "{payloadID}/{fileOffset}" (see ParseChunkOffset), so two files whose content
// hash-matches produce two rows by construction, and a carve whose chunks are
// all deduped still commits their manifest rows — the bytes are already
// remote-durable, but without the rows the range has no manifest coverage.
// Reclaim depends on it: DecrementRefCountAndReap removes strictly this file's
// own row by exact ID, and a sibling row is what keeps a shared hash in the GC
// live set. Withdraw this only if row IDs ever become hash-derived; restoring a
// UNIQUE constraint on hash would reject the second writer of any cross-file
// dedup. The conformance test storetest.testPut_TwoIDsSameHash pins it for
// every backend.
//
// Enumeration of all FileChunks across the store has moved up to
// MetadataStore.EnumerateFileChunks.
//
// Backends MAY (and currently do) implement additional methods
// (GetFileChunk, ListFileChunks, ListRemoteBlocks, ListUnreferenced)
// for engine-internal use; those are accessed via a wider engine-
// internal interface, NOT via this public surface.
type FileChunkStore interface {
	// GetByHash returns any FileChunk with the given content hash, or
	// (nil, nil) when absent. The "any" wording matters: multiple rows
	// routinely share one hash, so which row comes back is indeterminate.
	// Callers treat the result as best-effort and proceed with any one
	// row's chunk.
	GetByHash(ctx context.Context, hash ContentHash) (*FileChunk, error)

	// Put creates or replaces a FileChunk by ID.
	//
	// Upsert semantics are by ID: an INSERT for a new ID, or an UPDATE
	// when the ID already exists. The Hash column is NOT a uniqueness
	// constraint at the contract level — the carve commit path
	// (engineBlockSink.CommitBlock, via manifestRows) WILL produce two
	// distinct FileChunk IDs sharing the same ContentHash when two file
	// regions hash-match. Backends MUST tolerate this without erroring.
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
	// return either of the colliding rows. The dedup short-circuit
	// treats the lookup as best-effort — a miss just means the caller
	// re-PUTs the chunk, never data loss.
	//
	// The conformance test storetest.testPut_TwoIDsSameHash
	// pins this contract across every backend.
	Put(ctx context.Context, block *FileChunk) error

	// Delete removes a FileChunk by ID. Returns ErrFileChunkNotFound
	// if not found.
	Delete(ctx context.Context, id string) error

	// IncrementRefCount atomically bumps RefCount for the given
	// FileChunk id.
	IncrementRefCount(ctx context.Context, id string) error

	// DecrementRefCount atomically decrements; returns the new
	// count. RefCount=0 marks the block as a GC candidate.
	DecrementRefCount(ctx context.Context, id string) (uint32, error)

	// DecrementRefCountAndReap atomically decrements RefCount for the FileChunk
	// id and, IF the new count is 0, deletes the row (and its hash index entry)
	// in the SAME critical section as the decrement — TOCTOU-free against a
	// concurrent AddRef the same way IncrementRefCount/AddRef are. Returns the
	// new count (0 when reaped or when the row was already absent).
	// ErrFileChunkNotFound is tolerated and reported as count 0 (a row already
	// swept is not a caller error). Used by the engine Delete/Truncate reclaim
	// path so that, once a hash has no live references, it leaves
	// EnumerateFileChunks and the GC sweep can collect the remote chunk.
	DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error)

	// AddRef atomically increments RefCount on the FileChunk row
	// indexed by hash. Used by the in-memory hash dedup LRU hit path.
	//
	// On success, RefCount is incremented; BlockState is UNCHANGED
	// (no Pending→Syncing→Remote transition; no new row created).
	// This is the load-bearing contract: the LRU hit path references
	// an existing block — it never creates one.
	//
	// Returns ErrUnknownHash if no FileChunk row exists for the given
	// hash. Callers (see pkg/block/engine fetch's LRU hit
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
	// Multi-row-per-hash
	// AddRef MAY operate on any one matching row when more than one
	// row shares the hash, which is the normal case. The
	// caller's ChunkRef contract is satisfied either way — RefCount
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
	AddRef(ctx context.Context, hash ContentHash, payloadID string, blockRef ChunkRef) error
}

// EngineFileChunkStore is the engine-internal extension of
// FileChunkStore. The engine still needs by-ID
// and per-file lookups for the dual-read read path, recovery,
// dedup-delete and stats fan-out (callers under
// pkg/block/engine/).
//
// All three metadata backends (memory/badger/postgres) satisfy this
// interface — the methods are concrete on the backend struct, just
// not on the public FileChunkStore surface. Future work will
// eliminate the remaining call sites by routing reads through
// FileAttr.Blocks, and this interface will go away with them.
type EngineFileChunkStore interface {
	FileChunkStore

	// GetFileChunk retrieves a FileChunk by ID. Returns
	// ErrFileChunkNotFound if absent.
	GetFileChunk(ctx context.Context, id string) (*FileChunk, error)

	// ListFileChunks returns every FileChunk whose ID begins with
	// "{payloadID}/", sorted by parsed numeric block index. Returns
	// an empty (non-nil) slice when no blocks match.
	ListFileChunks(ctx context.Context, payloadID string) ([]*FileChunk, error)

	// EnumeratePayloads streams every distinct payloadID that has at
	// least one FileChunk row in this (share-scoped) store through fn,
	// in no guaranteed order. It is the authoritative enumeration of a
	// share's files: unlike the local block store's ListFiles (which
	// tracks only payloads with a live append-log and goes empty once a
	// payload rolls up), the FileChunk rows persist across rollup, so
	// this also surfaces rolled-up and remote-only payloads. warm and
	// stats drive off this rather than local.ListFiles. A non-nil error
	// from fn aborts the enumeration and is returned.
	EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error
}

// FlushResult indicates the outcome of a flush operation. The full
// (Finalized, err) state machine and the caller-retry guidance live on
// engine.Store.Flush in pkg/block/engine, which produces every value of this
// type. It cannot be linked from here: that package imports this one.
type FlushResult struct {
	// Finalized indicates all blocks have been synced to the backend
	// store. When false alongside a nil error, the call hit a soft
	// non-fatal condition (remote unhealthy or another mirror pass
	// already in flight); the dirty state is unchanged and will be
	// re-attempted by the next Flush or the periodic uploader. Callers
	// MUST NOT spin-retry on Finalized=false — see engine.Store.Flush in
	// pkg/block/engine.
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
