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

// resolveStore picks between a context-bound metadata.Transaction (when
// the caller used metadata.WithTx, e.g. common.CopyPayload) and the
// public metadata.MetadataStore surface (Truncate/Delete which do not
// run inside a metadata txn). The Transaction interface embeds the
// FileBlockStore, so GetByHash / IncrementRefCount / DecrementRefCount
// are available on both surfaces with identical signatures.
//
// CR-01 (Phase 12 review iteration 1): without this, every coordinator
// mutation routes through the Postgres connection pool and commits
// immediately on its own connection — defeating the BLOCKER-2 atomic
// rollback contract documented in copy_payload.go and engine.go. The
// returned blockstore.FileBlockStore-shaped surface is the narrow set
// of methods the coordinator needs.
func (c *metadataCoordinator) resolveStore(ctx context.Context) blockstore.FileBlockStore {
	if tx := metadata.TxFromContext(ctx); tx != nil {
		return tx
	}
	return c.metadataStore
}

// IncrementRefCount looks up the FileBlock by hash and bumps its
// RefCount. If no FileBlock with the hash exists, returns
// ErrFileBlockNotFound (the caller — typically CopyPayload — surfaces
// this so the metadata txn can roll back).
//
// CR-01 (Phase 12 review iteration 1): when the caller has bound an
// active metadata.Transaction into ctx via metadata.WithTx, both
// GetByHash and IncrementRefCount route through that tx — keeping the
// per-row UPDATE inside the caller's txn so a downstream PutFile
// failure rolls back BOTH the file attrs AND every increment.
func (c *metadataCoordinator) IncrementRefCount(ctx context.Context, hash blockstore.ContentHash) error {
	store := c.resolveStore(ctx)
	fb, err := store.GetByHash(ctx, hash)
	if err != nil {
		return fmt.Errorf("coordinator: GetByHash(%s): %w", hash.String(), err)
	}
	if fb == nil {
		return fmt.Errorf("coordinator: no FileBlock with hash %s: %w", hash.String(), metadata.ErrFileBlockNotFound)
	}
	if err := store.IncrementRefCount(ctx, fb.ID); err != nil {
		return fmt.Errorf("coordinator: IncrementRefCount(%s): %w", fb.ID, err)
	}
	return nil
}

// DecrementRefCount looks up the FileBlock by hash and decrements its
// RefCount, returning the new count. ErrFileBlockNotFound on a hash
// that has no row is tolerated (returns count=0, nil) — a Truncate or
// Delete on a hash that has already been swept by GC is not a caller
// error.
//
// CR-01 (Phase 12 review iteration 1): when the caller has bound an
// active metadata.Transaction into ctx via metadata.WithTx, both
// GetByHash and DecrementRefCount route through that tx. Truncate and
// Delete from the engine path do NOT currently bind a tx (no wrapping
// WithTransaction), so those callers route through the public store —
// the documented Phase 12 stance is that Truncate/Delete are non-atomic
// at the cross-store level and the INV-02 audit reconciles drift.
func (c *metadataCoordinator) DecrementRefCount(ctx context.Context, hash blockstore.ContentHash) (uint32, error) {
	store := c.resolveStore(ctx)
	fb, err := store.GetByHash(ctx, hash)
	if err != nil {
		return 0, fmt.Errorf("coordinator: GetByHash(%s): %w", hash.String(), err)
	}
	if fb == nil {
		// Already swept / never existed — caller's metadata is stale but
		// the requested decrement effectively succeeded (count is zero).
		return 0, nil
	}
	count, err := store.DecrementRefCount(ctx, fb.ID)
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
// Phase 13 D-05/D-06: the syncer also computes the BLAKE3 Merkle-root
// ObjectID over `blocks` and threads it through this hook so the
// metadata write atomically updates both Blocks AND ObjectID in the same
// PutFile/transaction. Until the payloadID → fileHandle → PutFile chain
// is wired (Phase 12 plan 08+), this method returns
// engine.ErrPersistFileBlocksNotWired so the syncer's post-Flush hook
// recognizes the deferred-wiring case and logs rather than silently
// dropping the BlockRef list.
func (c *metadataCoordinator) PersistFileBlocks(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, objectID blockstore.ObjectID) error {
	// Phase 12 plan 07: the engine seam exists; production wiring of the
	// payloadID → fileHandle → PutFile chain lands in plan 08 alongside
	// the adapter common helper refactor (which has the file handle).
	// The dual-read shim (D-20) keeps reads correct in the meantime; this
	// path is wire-but-not-implemented.
	//
	// WR-02 (Phase 12 review iteration 1): the previous "return nil" silently
	// swallowed every call. That is dangerous once WriteAt starts producing
	// non-empty BlockRef lists in a future plan: the syncer would observe
	// success and never retry the persist, leaving FileAttr.Blocks empty.
	// Surface the gap by returning engine.ErrPersistFileBlocksNotWired so
	// callers (today: only Syncer.persistFileBlocksAfterFlush) can recognise
	// the deferred-wiring case explicitly. The Syncer's post-Flush hook
	// tolerates this sentinel (dual-read shim covers reads) but logs a
	// warning so the silent-drop window is observable; a future plan that
	// flips WriteAt to return real BlockRefs is forced to implement this
	// method, not silently succeed.
	//
	// Phase 13 wiring template (Plan 04): once the payloadID → file
	// resolution chain lands, the body becomes:
	//   return c.metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
	//     file, err := tx.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
	//     if err != nil { return err }
	//     file.FileAttr.Blocks = blocks
	//     file.FileAttr.ObjectID = objectID  // Phase 13 D-05/D-06 — same txn.
	//     return tx.PutFile(ctx, file)
	//   })
	_ = ctx
	_ = payloadID
	_ = blocks
	_ = objectID
	return engine.ErrPersistFileBlocksNotWired
}

// FindByObjectID forwards to the underlying metadata store's
// FindByObjectID lookup. Returns (nil, nil) on zero-valued ObjectID
// (defense-in-depth — backends also short-circuit) and on cache miss.
//
// Phase 13 BSCAS-05 / D-12: callers (Plan 07 short-circuit) use this
// to detect whether a provisional ObjectID matches a previously-quiesced
// file's BlockRef list, enabling file-level dedup at write time.
func (c *metadataCoordinator) FindByObjectID(ctx context.Context, objectID blockstore.ObjectID) ([]blockstore.BlockRef, error) {
	if objectID.IsZero() {
		return nil, nil
	}
	return c.metadataStore.FindByObjectID(ctx, objectID)
}
