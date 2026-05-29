package engine

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// MetadataCoordinator abstracts the metadata-store operations the engine
// needs without importing pkg/metadata on hot paths.
// Implementations live in the per-share runtime service
// (pkg/controlplane/runtime/shares/) and bind to the concrete
// metadata-store backend at construction time.
//
// Transaction ownership rule (BLOCKER-1/2/3 resolution, 2026-04-26)
// the engine NEVER opens a metadata txn. The CALLER (per-share runtime
// wrapper for CopyPayload/WriteAt; common.WriteToBlockStore for the
// adapter path; syncer post-Flush wrapper for PersistFileBlocks) opens
// the txn around the engine call. The coordinator's IncrementRefCount /
// DecrementRefCount / PersistFileBlocks operations run inside that
// caller-owned txn.
//
// PayloadID is passed as a `string` rather than a strongly-typed
// blockstore.PayloadID to avoid type-system churn across every
// adapter caller (which all use metadata.PayloadID and cast to string
// at the engine boundary). The MetadataCoordinator interface keeps
// the engine seam clean; a future plan can promote the parameter to
// a typed alias when the adapter call sites converge on a single
// PayloadID type.
type MetadataCoordinator interface {
	// IncrementRefCount atomically bumps the FileBlock RefCount for a
	// hash. Engine invokes this once per unique hash inside CopyPayload
	// (under the caller's metadata txn).
	IncrementRefCount(ctx context.Context, hash blockstore.ContentHash) error

	// DecrementRefCount atomically decrements; returns the new count.
	// Engine invokes this from Delete (per hash in the BlockRef list)
	// and Truncate (per hash dropped past the new size).
	DecrementRefCount(ctx context.Context, hash blockstore.ContentHash) (uint32, error)

	// PersistFileBlocks updates FileAttr.Blocks AND FileAttr.ObjectID
	// for a given file in a single metadata txn. Engine invokes this
	// from the local store's rollup-completion callback (the
	// ObjectIDPersister wired in engine.New). The runtime wrapper
	// resolves payloadID → fileHandle and runs PutFile in one txn.
	//
	// ObjectID is the BLAKE3 Merkle root over blocks (computed by
	// blockstore.ComputeObjectID at rollup commit time). Pass an
	// all-zero ObjectID to mean "do not update ObjectID" (e.g.
	// partial flushes — but those currently never reach this hook per
	// Flush semantics).
	PersistFileBlocks(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, objectID blockstore.ObjectID) error

	// GetPersistedBlocks returns the file's currently-persisted
	// FileAttr.Blocks (with content hashes) for payloadID, or an empty
	// slice when the file has no blocks yet / does not exist. The
	// rollup-completion callback reads this to merge a partial rollup
	// pass into the already-committed block list before calling
	// PersistFileBlocks, so a multi-pass rollup keeps FileAttr.Blocks
	// complete instead of replacing it with only the latest pass (#789).
	//
	// Reads the manifest source (file_block_refs on Postgres, encoded
	// FileAttr.Blocks on Badger/Memory) — NOT the per-file FileBlock
	// index, whose Pending rows carry a NULL hash on Postgres.
	GetPersistedBlocks(ctx context.Context, payloadID string) ([]blockstore.BlockRef, error)

	// FindByObjectID looks up a previously-quiesced file in the
	// metadata store by Merkle-root ObjectID. Returns (nil, nil) on
	// miss. Used by the file-level dedup short-circuit
	// . Per-metadata-store scope, not per-share.
	//
	// Implementations short-circuit on zero-valued ObjectID and return
	// (nil, nil) without touching the metadata store.
	FindByObjectID(ctx context.Context, objectID blockstore.ObjectID) ([]blockstore.BlockRef, error)

	// GetFileObjectID returns the current FileAttr.ObjectID for
	// payloadID, or the all-zero sentinel when the file has never
	// quiesced (or does not exist). Used by Syncer.Flush to evaluate the
	// trigger condition for the file-level dedup short-circuit BEFORE
	// running the per-block upload pump.
	//
	// Implementations MUST NOT open a metadata transaction — this is a
	// single-row read on the public metadata-store surface. The
	// runtime coordinator routes through metadataStore.GetFileByPayloadID
	// directly (no caller-owned txn binding required for the trigger
	// check).
	//
	// Returning the all-zero ObjectID + nil for "no row" is the
	// authoritative pattern: callers treat zero as "skip short-circuit"
	// and fall through to the per-block path. Real backend errors
	// (storage I/O, corrupt row) propagate.
	GetFileObjectID(ctx context.Context, payloadID string) (blockstore.ObjectID, error)
}

// ErrMetadataCoordinatorNotWired is returned when an engine method
// requiring metadata coordination is invoked on a Store that was
// constructed with a nil coordinator. Production wiring (per-share
// runtime service in pkg/controlplane/runtime/shares/) MUST inject a
// real coordinator; unit tests tolerate nil for ReadAt-only fixtures
// and for CopyPayload over an empty BlockRef list (no work => no
// coordinator needed).
var ErrMetadataCoordinatorNotWired = errors.New("engine: metadata coordinator not wired")

// ErrPersistFileBlocksNotWired signals that PersistFileBlocks was invoked
// against a coordinator whose payloadID → fileHandle resolution chain has
// not yet been wired. The Syncer's post-Flush
// hook recognises this sentinel and tolerates it (the dual-read shim keeps
// reads correct), but logs a warning so the silent-drop window is
// observable. Other callers should treat it as a hard error so a future
// plan flipping WriteAt to return real BlockRefs is forced to implement
// the method rather than silently succeed.
var ErrPersistFileBlocksNotWired = errors.New("engine: PersistFileBlocks not wired (dual-read shim covers reads)")

// ErrObjectIDConflict signals that PersistFileBlocks rejected a write
// because another file already holds the same FileAttr.ObjectID
// (first-committer-wins). The short-circuit
// caller (applyFileLevelDedupHit) catches this, rolls back the
// just-incremented refcounts on the original target, re-fetches the
// now-canonical target via FindByObjectID, and retries once.
//
// Wrapping: the runtime coordinator wraps three sources into this
// sentinel via errors.Join
//  1. Postgres pgconn.PgError with Code "23505" AND ConstraintName
//     "files_object_id_idx".
//  2. Postgres pgconn.PgError with Code "23505" AND empty
//     ConstraintName whose Message text mentions "object_id"
//     (defensive fallback — some pg drivers strip ConstraintName under
//
// certain configurations; detection MUST NOT rely solely on
//
//	   the constraint label).
//	3. metadata.errors.StoreError with Code == errors.ErrConflict
//
// (Memory and Badger surface this from maintenance).
var ErrObjectIDConflict = errors.New("engine: object_id already mapped to another file")
