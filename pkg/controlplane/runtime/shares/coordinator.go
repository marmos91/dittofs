package shares

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// metadataCoordinator is the per-share implementation of
// engine.MetadataCoordinator. It binds the engine's metadata-coordination
// surface (RefCount mutations, FileAttr.Blocks persistence) to a concrete
// metadata.MetadataStore so the engine package itself can satisfy the
// Phase 12 API-02 strict-grep gate (zero pkg/metadata imports under
// pkg/blockstore/engine/*.go).
//
// Transaction ownership rule (BLOCKER-1/2/3 resolution): the engine
// NEVER opens a metadata txn. The CALLER (per-share runtime wrapper for
// CopyPayload/WriteAt; common.WriteToBlockStore for the adapter path;
// syncer post-Flush wrapper for PersistFileBlocks) opens the txn around
// the engine call. This implementation calls into metadataStore at the
// public surface; callers that need atomicity across multiple
// IncrementRefCount calls (e.g. CopyPayload) MUST drive this from inside
// a metadataStore.WithTransaction wrapper.
type metadataCoordinator struct {
	metadataStore metadata.MetadataStore
}

// Compile-time assertion: metadataCoordinator satisfies engine.MetadataCoordinator.
var _ engine.MetadataCoordinator = (*metadataCoordinator)(nil)

// newMetadataCoordinator wires a per-share metadata store into the
// engine.MetadataCoordinator contract. Returns nil when metadataStore is
// nil so the caller can skip injection cleanly in degenerate test
// fixtures (production callers always pass a non-nil store).
func newMetadataCoordinator(metadataStore metadata.MetadataStore) engine.MetadataCoordinator {
	if metadataStore == nil {
		return nil
	}
	return &metadataCoordinator{metadataStore: metadataStore}
}

// IncrementRefCount looks up the FileBlock by hash and bumps its
// RefCount. If no FileBlock with the hash exists, returns
// ErrFileBlockNotFound (the caller — typically CopyPayload — surfaces
// this so the metadata txn can roll back).
func (c *metadataCoordinator) IncrementRefCount(ctx context.Context, hash blockstore.ContentHash) error {
	fb, err := c.metadataStore.GetByHash(ctx, hash)
	if err != nil {
		return fmt.Errorf("coordinator: GetByHash(%s): %w", hash.String(), err)
	}
	if fb == nil {
		return fmt.Errorf("coordinator: no FileBlock with hash %s: %w", hash.String(), metadata.ErrFileBlockNotFound)
	}
	if err := c.metadataStore.IncrementRefCount(ctx, fb.ID); err != nil {
		return fmt.Errorf("coordinator: IncrementRefCount(%s): %w", fb.ID, err)
	}
	return nil
}

// DecrementRefCount looks up the FileBlock by hash and decrements its
// RefCount, returning the new count. ErrFileBlockNotFound on a hash
// that has no row is tolerated (returns count=0, nil) — a Truncate or
// Delete on a hash that has already been swept by GC is not a caller
// error.
func (c *metadataCoordinator) DecrementRefCount(ctx context.Context, hash blockstore.ContentHash) (uint32, error) {
	fb, err := c.metadataStore.GetByHash(ctx, hash)
	if err != nil {
		return 0, fmt.Errorf("coordinator: GetByHash(%s): %w", hash.String(), err)
	}
	if fb == nil {
		// Already swept / never existed — caller's metadata is stale but
		// the requested decrement effectively succeeded (count is zero).
		return 0, nil
	}
	count, err := c.metadataStore.DecrementRefCount(ctx, fb.ID)
	if err != nil {
		if errors.Is(err, metadata.ErrFileBlockNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("coordinator: DecrementRefCount(%s): %w", fb.ID, err)
	}
	return count, nil
}

// PersistFileBlocks updates the FileAttr.Blocks slice for the file
// identified by payloadID in a single PutFile call. The runtime wrapper
// resolves payloadID → fileHandle → file via the metadata store's
// existing helpers; if the lookup chain is unavailable on the active
// backend, returns ErrNotImplemented so the caller can fall back to the
// legacy path.
//
// Phase 12 D-37 / D-20: this is the post-Flush seam — the syncer
// invokes this once per uploaded chunk so the canonical FileAttr.Blocks
// list reflects every Remote BlockRef the engine has produced.
//
// The current implementation is a placeholder: routing payloadID →
// FileHandle requires either a reverse-index in the metadata store or a
// caller-supplied handle. Phase 12 plans 08+ wire the call site through
// the per-share runtime service which has the file handle in context;
// for now this method documents the contract and returns nil so plans
// 08+ can drop in the real implementation without churning the engine.
func (c *metadataCoordinator) PersistFileBlocks(ctx context.Context, payloadID string, blocks []blockstore.BlockRef) error {
	// Phase 12 plan 07: the engine seam exists; production wiring of the
	// payloadID → fileHandle → PutFile chain lands in plan 08 alongside
	// the adapter common helper refactor (which has the file handle).
	// Until then, accept the call and persist nothing — the dual-read
	// shim (D-20) keeps reads correct, and uploads still complete via
	// the existing FileBlock.State=Remote path. This is intentionally a
	// no-op and is the only "pending wiring" surface in plan 07.
	_ = ctx
	_ = payloadID
	_ = blocks
	return nil
}
