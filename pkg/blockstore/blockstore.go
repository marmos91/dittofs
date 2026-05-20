// Unified BlockStore contract — Phase 17.
//
// This file declares the single CAS-keyed surface that replaces the
// split LocalStore (22 methods) + RemoteStore (12 methods) of v0.15.
// BlockStoreAppend extends BlockStore with the random-write absorber
// tier (per-file append log + rollup) used by the fs backend only;
// s3 and memory backends implement only BlockStore.
//
// Sentinel-file conventions (.cas-migrated-v1) and the legacy-layout
// boot guard (ErrLegacyLayoutDetected) live in doc.go and errors.go.
//
// No implementer is wired against these types in this commit — the
// rest of Phase 17 (Waves 2-6) narrows LocalStore onto BlockStoreAppend,
// renames RemoteStore methods onto BlockStore, and lands the unified
// blockstoretest conformance suite. This file is the locked target.

package blockstore

import (
	"context"
	"time"
)

// Meta is the minimal per-object metadata returned by BlockStore.Head
// and BlockStore.Walk. Per Phase 17 D-08, the lookup key (ContentHash)
// is NEVER echoed inside Meta — it is the input, not output.
//
// The S3 backend continues to stamp x-amz-meta-content-hash on every
// PutObject as defense-in-depth (BSCAS-06: ReadBlockVerified compares
// the header against the recomputed BLAKE3 before returning bytes), but
// that header stays inside the s3 backend and is not surfaced through
// Meta. Callers that need integrity verification use BlockStore.Get
// (which performs the verification on backends that support it) rather
// than reading metadata.
type Meta struct {
	// Size is the object body length in bytes.
	Size int64

	// LastModified is the backend's last-modified timestamp. MUST be
	// non-zero for every object the backend reports — the GC sweep
	// fails closed on a zero LastModified (mirrors Phase 11 WR-4-02 /
	// INV-04). Backends that cannot natively report a timestamp MUST
	// stamp time.Now() at Put time and surface that value here.
	LastModified time.Time
}

// BlockStore is the unified content-addressed block storage contract.
// Every implementation is keyed by ContentHash (BLAKE3-256, 32 bytes);
// no opaque "block key" strings appear on this surface.
//
// Implementations:
//   - pkg/blockstore/local/fs.FSStore (also implements BlockStoreAppend)
//   - pkg/blockstore/remote/s3.Store
//   - pkg/blockstore/remote/memory.Store
//
// All methods take ctx context.Context as the first argument and MUST
// honor cancellation. All hash arguments are the full 32-byte
// ContentHash; backends translate to their on-disk / on-wire key shape
// (cas/{hh}/{hh}/{hex} for fs/s3) internally.
type BlockStore interface {
	// Put writes data under the key derived from hash. Put is
	// idempotent: a second Put with the same hash and identical bytes
	// is a no-op (or an integrity-verified rewrite, backend's choice);
	// a Put with the same hash but different bytes is undefined
	// behavior — callers MUST NOT rely on either outcome and the
	// conformance suite asserts only the same-bytes idempotent case.
	//
	// Returns an error if the backend is closed, the I/O fails, or the
	// hash is zero (callers must compute the hash before calling).
	Put(ctx context.Context, hash ContentHash, data []byte) error

	// Get returns the chunk bytes addressed by the given content hash.
	// The returned []byte is freshly allocated and owned by the caller
	// — matches the prior mmap-then-copy semantics from the engine
	// Cache's perspective (the Cache always copied bytes out of the
	// mmapped region into its LRU slot, so the allocation simply moves
	// earlier in the pipeline).
	//
	// Returns blockstore.ErrChunkNotFound if the chunk is absent from
	// the store. Implementations MUST NOT return a slice that aliases
	// internal storage; no read-buffer pool is used.
	//
	// Signature is byte-identical to the Phase 16 *fs.FSStore.Get and
	// LocalStore.Get methods that this contract supersedes — engine
	// call sites narrow the receiver type from *fs.FSStore (or
	// LocalStore) to BlockStore with zero rename churn.
	Get(ctx context.Context, hash ContentHash) ([]byte, error)

	// GetRange returns a byte sub-range [offset, offset+length) of the
	// chunk addressed by hash. The returned slice is freshly allocated
	// and owned by the caller (same no-aliasing rule as Get).
	//
	// Returns blockstore.ErrChunkNotFound if the chunk is absent.
	// Returns blockstore.ErrInvalidOffset if offset is negative or
	// beyond the chunk size; returns blockstore.ErrInvalidSize if
	// length is non-positive or offset+length overflows the chunk.
	// Backends MAY clamp length to the chunk's remaining bytes; the
	// conformance suite asserts both clamp and explicit-error
	// behaviors are acceptable as long as callers can detect short
	// reads via the returned slice length.
	GetRange(ctx context.Context, hash ContentHash, offset, length int64) ([]byte, error)

	// Has reports whether the store currently holds an object addressed
	// by hash. Backends MAY implement this as Head (S3 HEAD) or as
	// Get with a Range: bytes=0-0 probe (more portable, costlier) —
	// the choice is per-backend.
	//
	// Returns (false, nil) for a confirmed miss; (true, nil) for a hit;
	// (_, err) for backend errors that do not constitute a definitive
	// answer (network failures, permission errors, etc.). Callers MUST
	// distinguish (false, nil) from (false, err).
	Has(ctx context.Context, hash ContentHash) (bool, error)

	// Delete removes the object addressed by hash. Delete is idempotent:
	// deleting an absent hash returns nil (no ErrChunkNotFound). The
	// conformance suite asserts Delete-then-Get returns ErrChunkNotFound
	// and Delete-then-Delete returns nil.
	//
	// Backends MUST make Delete durable before returning nil; partial
	// state (e.g., a tombstone written but the object body still
	// present) is not acceptable.
	Delete(ctx context.Context, hash ContentHash) error

	// Head returns Meta for the object addressed by hash without
	// transferring the body. Returns blockstore.ErrChunkNotFound (or
	// blockstore.ErrBlockNotFound for remote backends — both are
	// acceptable; callers match via errors.Is on either) when the
	// object is absent.
	//
	// Meta.Size MUST equal the byte length of what Get would return for
	// the same hash. Meta.LastModified MUST be non-zero (see Meta
	// godoc).
	Head(ctx context.Context, hash ContentHash) (Meta, error)

	// Walk enumerates every object in the store. The callback receives
	// the content hash and Meta for each object; ordering is
	// unspecified (backends MAY parallelize internally; the conformance
	// suite does not pin a traversal order).
	//
	// Returning blockstore.ErrStopWalk from the callback exits cleanly
	// — Walk returns nil to the outer caller. Any other non-nil
	// callback error halts the walk and Walk returns it wrapped with
	//
	//   fmt.Errorf("walk halted at %s: %w", hash, err)
	//
	// Context cancellation aborts immediately; the callback is NOT
	// re-invoked after ctx.Err() != nil (Walk MUST surface ctx.Err()
	// without one final spurious callback). Contract mirrors
	// filepath.SkipDir / fs.SkipAll.
	//
	// See blockstore.ErrStopWalk for the sentinel doc.
	Walk(ctx context.Context, fn func(hash ContentHash, meta Meta) error) error
}

// BlockStoreAppend extends BlockStore with the random-write absorber
// tier used by the local fs backend only. The append log absorbs
// adapter writes that arrive out-of-order or below the FastCDC chunk
// boundary; a background rollup loop chunks the log into CAS objects
// via Put and then trims the log via DeleteLog (or implicitly during
// the rollup, backend's choice).
//
// Remote backends (s3, memory) do NOT implement this interface — they
// only see the rolled-up Put calls.
//
// Note: the narrowed LocalStore interface that Plan 04 lands keeps
// additional lifecycle / admin methods (Truncate, EvictMemory,
// SetRetentionPolicy, SetEvictionEnabled, Stats, ListFiles,
// GetStoredFileSize, Healthcheck, SyncFileBlocks, SyncFileBlocksForFile,
// Flush, Start, Close) as a strict admin-superset of BlockStoreAppend.
// Those methods belong on LocalStore (boot-path / observability /
// retention), NOT on BlockStoreAppend — the append surface here is
// strictly the byte-level write-absorber contract.
type BlockStoreAppend interface {
	BlockStore

	// AppendWrite stages random-offset bytes for payloadID into the
	// per-file append log. Subsequent FastCDC rollup consumes the log
	// after the stabilization window and emits CAS chunks via Put.
	//
	// The interval [offset, offset+len(data)) is tracked so the rollup
	// can later compute a deterministic chunking over the consolidated
	// stream. Implementations MUST be safe under concurrent calls for
	// the same payloadID (per-file mutex; see *fs.FSStore.AppendWrite
	// godoc for the full pressure / tombstone / mutex semantics).
	//
	// Empty data is a no-op (returns nil). Context cancellation while
	// waiting on log-bytes pressure surfaces as ctx.Err().
	AppendWrite(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// DeleteLog removes the per-file append log and its tracked
	// intervals for payloadID (formerly named DeleteAppendLog on
	// *fs.FSStore — renamed here to match the conformance-suite test
	// `testDeleteLog`). BSCAS-05 invokes this on a file-level dedup
	// hit to discard speculative chunks the syncer was about to
	// upload.
	//
	// Implementations MUST be safe to call when no log exists for the
	// payload (no-op return nil). After DeleteLog returns, the payload
	// is tombstoned: subsequent AppendWrite for the same payloadID
	// returns ErrDeleted (or the backend-specific equivalent).
	//
	// Orphan content-addressed chunks already emitted by a prior
	// rollup are NOT removed here — they are swept by the mark-sweep
	// GC.
	DeleteLog(ctx context.Context, payloadID string) error
}
