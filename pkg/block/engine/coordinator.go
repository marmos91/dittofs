package engine

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block"
)

// MetadataCoordinator abstracts the metadata-store operations the engine
// needs without importing pkg/metadata on hot paths.
// Implementations live in the per-share runtime service
// (pkg/controlplane/runtime/shares/) and bind to the concrete
// metadata-store backend at construction time.
//
// Transaction ownership rule (BLOCKER-1/2/3 resolution, 2026-04-26)
// the engine NEVER opens a metadata txn. The CALLER (common.CloneWholeFile
// for the clone path; common.WriteToBlockStore for the adapter path) opens
// the txn around the engine call. The coordinator's IncrementRefCount,
// DecrementRefCountAndReap and DecrementRefCountAndReapMany operations run
// inside that caller-owned txn.
//
// PayloadID is passed as a `string` rather than a strongly-typed
// block.PayloadID to avoid type-system churn across every
// adapter caller (which all use metadata.PayloadID and cast to string
// at the engine boundary). The MetadataCoordinator interface keeps
// the engine seam clean; a future plan can promote the parameter to
// a typed alias when the adapter call sites converge on a single
// PayloadID type.
type MetadataCoordinator interface {
	// IncrementRefCount atomically bumps the FileChunk RefCount for a
	// hash. Engine invokes this once per unique hash inside CopyPayload
	// (under the caller's metadata txn).
	IncrementRefCount(ctx context.Context, hash block.ContentHash) error

	// DecrementRefCountAndReap atomically decrements and, when the new count
	// is 0, deletes the FileChunk index row identified by the exact block ID
	// "{payloadID}/{offset}" so its hash leaves EnumerateFileChunks once no
	// row anywhere references it and the GC mark-sweep can reclaim the remote
	// chunk. Engine invokes this from Delete (per ChunkRef in the list) and
	// Truncate (per ChunkRef dropped past the new size) — the reclaim path.
	//
	// Reaping by EXACT ID (not by hash) is load-bearing for correctness: the
	// engine rollup creates per-chunk rows keyed "{payloadID}/{offset}" in
	// BlockStatePending and never finalizes them, so RefCount stays 0 and
	// GetByHash (Remote-gated) returns nil for them. A hash-resolved reap
	// would pick an INDETERMINATE row when two files share the same content
	// hash (whichever the hash index yields first) and could
	// reap the wrong file's row — leaking the creator's row or over-reaping a
	// still-referenced one (data loss). Removing THIS file's own row by ID
	// is unambiguous; cross-file dedup keep-alive is provided by SIBLING rows
	// keeping the hash in EnumerateFileChunks (the GC live set), NOT by
	// RefCount, so GC sweeps the chunk only when no row anywhere references it.
	//
	// Returns the new count (0 when reaped or when the row was already
	// absent). ErrFileChunkNotFound is tolerated and reported as count 0 — a
	// row already reaped (or a dedup-miss fallback that never created one) is
	// not a caller error.
	DecrementRefCountAndReap(ctx context.Context, payloadID string, offset uint64) (uint32, error)

	// ReprojectBlocks re-materializes FileAttr.Blocks from the file's
	// surviving FileChunk manifest. Engine invokes it after the Truncate
	// and PunchHole reap loops, where dropping manifest rows leaves the
	// Blocks projection stale and snapshot/audit over-counting.
	//
	// Required, not optional: a coordinator that skips the reprojection
	// leaves that over-count in place, and making it part of the interface
	// is what turns "forgot to reproject" into a compile error rather than
	// a silently drifting projection.
	ReprojectBlocks(ctx context.Context, payloadID string) error

	// DecrementRefCountAndReapMany is DecrementRefCountAndReap over a whole set
	// of the payload's offsets, with identical per-row semantics (exact
	// "{payloadID}/{offset}" identity, reap at 0, absent row tolerated) and no
	// counts reported back — the reap paths never read them. Truncate /
	// PunchHole / Delete drop every manifest row of a range at once, tens of
	// thousands of rows for a large file, so the whole set resolves in a fixed
	// number of statements instead of one round trip per row.
	//
	// offsets MUST be distinct: each names one row, and reaping a row twice
	// would strip a reference the file does not hold.
	DecrementRefCountAndReapMany(ctx context.Context, payloadID string, offsets []uint64) error
}

// ErrMetadataCoordinatorNotWired is returned when an engine method
// requiring metadata coordination is invoked on a Store that was
// constructed with a nil coordinator. Production wiring (per-share
// runtime service in pkg/controlplane/runtime/shares/) MUST inject a
// real coordinator; unit tests tolerate nil for ReadAt-only fixtures
// and for CopyPayload over an empty ChunkRef list (no work => no
// coordinator needed).
var ErrMetadataCoordinatorNotWired = errors.New("engine: metadata coordinator not wired")
