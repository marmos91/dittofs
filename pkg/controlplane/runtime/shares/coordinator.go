package shares

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// metadataCoordinator is the per-share implementation of
// engine.MetadataCoordinator. It binds the engine's metadata-coordination
// surface (RefCount mutations, FileAttr.Blocks persistence) to a concrete
// metadata.Store so the engine package itself can satisfy the
// strict-grep boundary (zero pkg/metadata imports under
// pkg/block/engine/*.go).
//
// Transaction ownership rule (BLOCKER-1/2/3 resolution): the engine
// NEVER opens a metadata txn. The CALLER (common.CloneWholeFile for the
// clone path; common.WriteToBlockStore for the adapter path) opens the
// txn around the engine call. This implementation calls into metadataStore at the
// public surface; callers that need atomicity across multiple
// IncrementRefCount calls (e.g. the clone manifest replacement) MUST drive
// this from inside a metadataStore.WithTransaction wrapper.
type metadataCoordinator struct {
	metadataStore metadata.Store
}

// Compile-time assertion: metadataCoordinator satisfies engine.MetadataCoordinator.
var _ engine.MetadataCoordinator = (*metadataCoordinator)(nil)

// newMetadataCoordinator wires a per-share metadata store into the
// engine.MetadataCoordinator contract. Returns nil when metadataStore is
// nil so the caller can skip injection cleanly in degenerate test
// fixtures (production callers always pass a non-nil store).
func newMetadataCoordinator(metadataStore metadata.Store) engine.MetadataCoordinator {
	if metadataStore == nil {
		return nil
	}
	return &metadataCoordinator{metadataStore: metadataStore}
}

// resolveStore picks between a context-bound metadata.Transaction (when
// the caller used metadata.WithTx, e.g. common.CloneWholeFile) and the
// public metadata.Store surface (Truncate/Delete which do not
// run inside a metadata txn). The Transaction interface embeds the
// FileChunkStore, so GetByHash / IncrementRefCount / DecrementRefCount
// are available on both surfaces with identical signatures.
//
// Without this, every coordinator mutation would open and commit its own
// store transaction — defeating the BLOCKER-2 atomic rollback contract documented in
// clone.go and engine.go. The returned
// block.FileChunkStore-shaped surface is the narrow set of methods
// the coordinator needs.
func (c *metadataCoordinator) resolveStore(ctx context.Context) block.FileChunkStore {
	if tx := metadata.TxFromContext(ctx); tx != nil {
		return tx
	}
	return c.metadataStore
}

// IncrementRefCount looks up the FileChunk by hash and bumps its
// RefCount. If no FileChunk with the hash exists, returns
// ErrFileChunkNotFound (the caller — typically the clone helper — surfaces
// this so the metadata txn can roll back).
//
// When the caller has bound an active metadata.Transaction into ctx
// via metadata.WithTx, both GetByHash and IncrementRefCount route
// through that tx — keeping the per-row UPDATE inside the caller's
// txn so a downstream UpdateAttrs failure rolls back BOTH the file attrs
// AND every increment.
func (c *metadataCoordinator) IncrementRefCount(ctx context.Context, hash block.ContentHash) error {
	store := c.resolveStore(ctx)
	fb, err := store.GetByHash(ctx, hash)
	if err != nil {
		return fmt.Errorf("coordinator: GetByHash(%s): %w", hash.String(), err)
	}
	if fb == nil {
		return fmt.Errorf("coordinator: no FileChunk with hash %s: %w", hash.String(), metadata.ErrFileChunkNotFound)
	}
	if err := store.IncrementRefCount(ctx, fb.ID); err != nil {
		return fmt.Errorf("coordinator: IncrementRefCount(%s): %w", fb.ID, err)
	}
	return nil
}

// DecrementRefCountAndReap decrements the FileChunk row identified by the EXACT
// ID "{payloadID}/{offset}" and reaps it (row + hash index entry) atomically
// when the new count reaches 0. The hash then leaves EnumerateFileChunks once no
// row anywhere references it, so the GC sweep can reclaim the remote chunk.
// ErrFileChunkNotFound on a missing row is tolerated (returns count=0, nil) — a
// Truncate/Delete on an already-reaped row, or a row a dedup-miss fallback never
// created, is not a caller error.
//
// By-ID, NOT by-hash: the engine rollup creates per-chunk rows keyed
// "{payloadID}/{offset}" in BlockStatePending and never finalizes them, so
// RefCount stays 0 and GetByHash (Remote-gated) returns nil for them. Resolving
// the row by hash would pick an INDETERMINATE row when two files share the same
// content hash and could reap the wrong file's row (leak or over-reap → data
// loss). Removing THIS file's own row by ID is unambiguous; cross-file dedup
// keep-alive is provided by SIBLING rows keeping the hash in EnumerateFileChunks
// (the GC live set), not by RefCount.
//
// Same txn-binding rule as DecrementRefCount: when ctx carries an active
// metadata.Transaction via metadata.WithTx the reap routes through that tx; the
// engine Truncate/Delete reclaim path does not bind a tx.
func (c *metadataCoordinator) DecrementRefCountAndReap(ctx context.Context, payloadID string, offset uint64) (uint32, error) {
	store := c.resolveStore(ctx)
	id := fmt.Sprintf("%s/%d", payloadID, offset)
	count, err := store.DecrementRefCountAndReap(ctx, id)
	if err != nil {
		if errors.Is(err, metadata.ErrFileChunkNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("coordinator: DecrementRefCountAndReap(%s): %w", id, err)
	}
	return count, nil
}

// DecrementRefCountAndReapMany reaps the payload's rows for every offset in one
// metadata transaction. Same txn-binding and ErrFileChunkNotFound tolerance as
// the single-offset form: a caller-bound metadata.Transaction on ctx carries the
// reap so the caller's rollback undoes it, and the engine's reclaim path binds
// none and gets a transaction of its own — one for the whole set, where the
// single-offset form opened one per row.
func (c *metadataCoordinator) DecrementRefCountAndReapMany(ctx context.Context, payloadID string, offsets []uint64) error {
	if len(offsets) == 0 {
		return nil
	}
	ids := make([]string, len(offsets))
	for i, offset := range offsets {
		ids[i] = fmt.Sprintf("%s/%d", payloadID, offset)
	}
	reap := func(tx metadata.Transaction) error { return tx.DecrementRefCountAndReapMany(ctx, ids) }

	var err error
	if tx := metadata.TxFromContext(ctx); tx != nil {
		err = reap(tx)
	} else {
		err = c.metadataStore.WithTransaction(ctx, reap)
	}
	if err != nil && !errors.Is(err, metadata.ErrFileChunkNotFound) {
		return fmt.Errorf("coordinator: DecrementRefCountAndReapMany(%s, %d offsets): %w", payloadID, len(offsets), err)
	}
	return nil
}

// ReprojectBlocks re-materializes FileAttr.Blocks from the surviving FileChunk
// manifest for payloadID, in one txn. The engine calls it after a Truncate /
// PunchHole reap loop so the R-materialized Blocks projection stays == the
// manifest once the reaped tail rows are gone (a stale Blocks list otherwise
// makes snapshot/audit over-count). Satisfies engine's optional blocksReprojector.
func (c *metadataCoordinator) ReprojectBlocks(ctx context.Context, payloadID string) error {
	return c.metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.ProjectManifestToBlocks(ctx, tx, payloadID)
	})
}
