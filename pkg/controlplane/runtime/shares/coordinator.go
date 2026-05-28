package shares

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// metadataCoordinator is the per-share implementation of
// engine.MetadataCoordinator. It binds the engine's metadata-coordination
// surface (RefCount mutations, FileAttr.Blocks persistence) to a concrete
// metadata.MetadataStore so the engine package itself can satisfy the
// strict-grep boundary (zero pkg/metadata imports under
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
// Without this, every coordinator mutation routes through the Postgres
// connection pool and commits immediately on its own connection —
// defeating the BLOCKER-2 atomic rollback contract documented in
// copy_payload.go and engine.go. The returned
// blockstore.FileBlockStore-shaped surface is the narrow set of methods
// the coordinator needs.
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
// When the caller has bound an active metadata.Transaction into ctx
// via metadata.WithTx, both GetByHash and IncrementRefCount route
// through that tx — keeping the per-row UPDATE inside the caller's
// txn so a downstream PutFile failure rolls back BOTH the file attrs
// AND every increment.
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
// When the caller has bound an active metadata.Transaction into ctx
// via metadata.WithTx, both GetByHash and DecrementRefCount route
// through that tx. Truncate and Delete from the engine path do NOT
// currently bind a tx (no wrapping WithTransaction), so those callers
// route through the public store — Truncate/Delete are non-atomic at
// the cross-store level by design and the refcount audit reconciles
// drift.
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

// PersistFileBlocks atomically updates FileAttr.Blocks AND
// FileAttr.ObjectID for the file identified by payloadID in a single
// metadata transaction. The runtime wrapper resolves
// payloadID → fileHandle → file via tx.GetFileByPayloadID and persists
// the updated FileAttr via tx.PutFile.
//
// This is the post-Flush seam — the syncer invokes this after every
// successful Flush so the canonical FileAttr.Blocks list reflects every
// Remote BlockRef the engine has produced.
//
// The syncer computes the BLAKE3 Merkle-root ObjectID over `blocks`
// (via blockstore.ComputeObjectID) and threads it through this hook so
// the metadata write atomically updates both Blocks AND ObjectID in the
// same PutFile transaction.
//
// Conflict mapping: a Postgres unique-violation on files_object_id_idx
// (first-committer-wins) — or the equivalent mderrors.ErrConflict from
// Memory/Badger — is wrapped into engine.ErrObjectIDConflict by
// mapObjectIDConflict so the file-level dedup short-circuit retry path
// in Syncer.applyFileLevelDedupHit detects the race uniformly across
// backends.
func (c *metadataCoordinator) PersistFileBlocks(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, objectID blockstore.ObjectID) error {
	return c.metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		file, err := tx.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
		if err != nil {
			return fmt.Errorf("coordinator: GetFileByPayloadID(%s): %w", payloadID, err)
		}
		if file == nil {
			return fmt.Errorf("coordinator: GetFileByPayloadID(%s): nil file (no row)", payloadID)
		}
		// FileAttr is embedded on metadata.File (not a pointer).
		file.Blocks = blocks
		// Same-txn write of Blocks AND ObjectID.
		file.ObjectID = objectID
		if err := tx.PutFile(ctx, file); err != nil {
			return mapObjectIDConflict(err)
		}
		return nil
	})
}

// mapObjectIDConflict wraps backend conflict errors into
// engine.ErrObjectIDConflict so the file-level dedup short-circuit can
// detect concurrent-quiesce races uniformly across Postgres / Badger /
// Memory. Returns nil when err is nil; returns the unwrapped error
// untouched when no conflict signal is present so other failure modes
// propagate without false positives.
//
// Detection rules:
//
//  1. Postgres pgconn.PgError with Code "23505" AND ConstraintName
//     "files_object_id_idx" — strong signal, wrap into
//     ErrObjectIDConflict.
//  2. Postgres pgconn.PgError with Code "23505" AND empty
//     ConstraintName whose Message text mentions "object_id" —
//     defensive fallback for drivers that strip ConstraintName under
//     certain configurations. Other 23505 errors (e.g., file path
//     uniqueness violations) propagate untouched.
//  3. metadata.errors.StoreError with Code == ErrConflict — Memory and
//     Badger surface this from their maintenance paths.
//
// Wrapping uses errors.Join (Go 1.20+) so callers can both
// `errors.Is(err, engine.ErrObjectIDConflict)` AND see the underlying
// driver/store error in logs.
func mapObjectIDConflict(err error) error {
	if err == nil {
		return nil
	}

	// Postgres path: SQLSTATE 23505 (unique_violation).
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Strong signal: matching constraint name.
		if pgErr.ConstraintName == "files_object_id_idx" {
			return errors.Join(engine.ErrObjectIDConflict, err)
		}
		// Defensive fallback: empty ConstraintName + "object_id" in
		// message text. Other 23505 errors (e.g., duplicate file path)
		// propagate without ErrObjectIDConflict wrapping.
		if pgErr.ConstraintName == "" && strings.Contains(pgErr.Message, "object_id") {
			return errors.Join(engine.ErrObjectIDConflict, err)
		}
	}

	// Memory / Badger path: errors.ErrConflict on the StoreError.
	var storeErr *mderrors.StoreError
	if errors.As(err, &storeErr) && storeErr.Code == mderrors.ErrConflict {
		return errors.Join(engine.ErrObjectIDConflict, err)
	}

	return err
}

// FindByObjectID forwards to the underlying metadata store's
// FindByObjectID lookup. Returns (nil, nil) on zero-valued ObjectID
// (defense-in-depth — backends also short-circuit) and on cache miss.
//
// Callers use this to detect whether a provisional ObjectID matches a
// previously-quiesced file's BlockRef list, enabling file-level dedup
// at write time.
func (c *metadataCoordinator) FindByObjectID(ctx context.Context, objectID blockstore.ObjectID) ([]blockstore.BlockRef, error) {
	if objectID.IsZero() {
		return nil, nil
	}
	return c.metadataStore.FindByObjectID(ctx, objectID)
}

// GetFileObjectID reads the current FileAttr.ObjectID for payloadID
// from the metadata store. Returns the all-zero ObjectID + nil when
// the file does not exist or has never quiesced — callers (Syncer.Flush)
// treat zero as "evaluate short-circuit" / "skip short-circuit".
//
// Callers use this to evaluate the trigger condition for file-level
// dedup BEFORE running the per-block upload pump. Reads on the public
// metadataStore surface (not bound to a caller-owned txn) — the read
// is a single-row lookup, not part of any per-flow transaction.
//
// Backend NotFound semantics: the Memory and Badger backends return a
// StoreError with ErrNotFound code when the payloadID has no row; the
// Postgres backend may return a wrapped sql.ErrNoRows. Both are mapped
// to (zero ObjectID, nil) here so the caller's trigger evaluation does
// not see a transient error during the very first quiesce of a fresh
// file.
func (c *metadataCoordinator) GetFileObjectID(ctx context.Context, payloadID string) (blockstore.ObjectID, error) {
	file, err := c.metadataStore.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
	if err != nil {
		// NotFound is the steady state for a never-quiesced file. The
		// caller treats a zero ObjectID as "trigger condition holds"
		// (the file has no prior Merkle root), so this is the correct
		// disposition — not a real backend error.
		if metadata.IsNotFoundError(err) {
			return blockstore.ObjectID{}, nil
		}
		return blockstore.ObjectID{}, fmt.Errorf("coordinator: GetFileObjectID(%s): %w", payloadID, err)
	}
	if file == nil {
		// Defense-in-depth: some backends return (nil, nil) for "no row"
		// rather than an error. Treat the same as NotFound.
		return blockstore.ObjectID{}, nil
	}
	return file.ObjectID, nil
}
