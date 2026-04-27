package engine

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// MetadataCoordinator abstracts the metadata-store operations the engine
// needs without importing pkg/metadata on hot paths (Phase 12 API-02).
// Implementations live in the per-share runtime service
// (pkg/controlplane/runtime/shares/) and bind to the concrete
// metadata-store backend at construction time.
//
// Transaction ownership rule (BLOCKER-1/2/3 resolution, 2026-04-26):
// the engine NEVER opens a metadata txn. The CALLER (per-share runtime
// wrapper for CopyPayload/WriteAt; common.WriteToBlockStore for the
// adapter path; syncer post-Flush wrapper for PersistFileBlocks) opens
// the txn around the engine call. The coordinator's IncrementRefCount /
// DecrementRefCount / PersistFileBlocks operations run inside that
// caller-owned txn.
//
// PayloadID is passed as a `string` rather than a strongly-typed
// blockstore.PayloadID because Phase 12 plan 07 deferred introducing a
// new typed alias to avoid type-system churn across every adapter caller
// (which all use metadata.PayloadID and cast to string at the engine
// boundary). The MetadataCoordinator interface keeps the engine seam
// clean; a future plan can promote the parameter to a typed alias when
// the adapter call sites are converged on a single PayloadID type.
type MetadataCoordinator interface {
	// IncrementRefCount atomically bumps the FileBlock RefCount for a
	// hash. Engine invokes this once per unique hash inside CopyPayload
	// (under the caller's metadata txn).
	IncrementRefCount(ctx context.Context, hash blockstore.ContentHash) error

	// DecrementRefCount atomically decrements; returns the new count.
	// Engine invokes this from Delete (per hash in the BlockRef list)
	// and Truncate (per hash dropped past the new size).
	DecrementRefCount(ctx context.Context, hash blockstore.ContentHash) (uint32, error)

	// PersistFileBlocks updates FileAttr.Blocks for a given file in a
	// single metadata txn. Engine invokes this from the syncer's
	// post-Flush path (where new BlockRefs are produced from a chunk
	// upload). The runtime wrapper resolves payloadID → fileHandle and
	// runs PutFile in one txn.
	PersistFileBlocks(ctx context.Context, payloadID string, blocks []blockstore.BlockRef) error
}

// ErrMetadataCoordinatorNotWired is returned when an engine method
// requiring metadata coordination is invoked on a BlockStore that was
// constructed with a nil coordinator. Production wiring (per-share
// runtime service in pkg/controlplane/runtime/shares/) MUST inject a
// real coordinator; unit tests tolerate nil for ReadAt-only fixtures
// and for CopyPayload over an empty BlockRef list (no work => no
// coordinator needed).
var ErrMetadataCoordinatorNotWired = errors.New("engine: metadata coordinator not wired")

// ErrPersistFileBlocksNotWired signals that PersistFileBlocks was invoked
// against a coordinator whose payloadID → fileHandle resolution chain has
// not yet been wired (Phase 12 plan 07 / WR-02). The Syncer's post-Flush
// hook recognises this sentinel and tolerates it (the dual-read shim keeps
// reads correct), but logs a warning so the silent-drop window is
// observable. Other callers should treat it as a hard error so a future
// plan flipping WriteAt to return real BlockRefs is forced to implement
// the method rather than silently succeed.
var ErrPersistFileBlocksNotWired = errors.New("engine: PersistFileBlocks not wired (Phase 12 deferred — dual-read shim covers reads)")
