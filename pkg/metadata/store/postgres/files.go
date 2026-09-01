package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// CRUD Operations
// ============================================================================
//
// Write operations open a transaction and delegate to its implementation, so
// each exists once. The handle-addressed reads are shared further still: they
// live on store/sql.Core, whose methods are promoted onto both this store and
// its transaction. What is left here is what could not be either — the reads
// that need store state, and the ones with no transaction-level counterpart.
//
// Several delegates below shadow a promoted Core method of the same name. That
// is what keeps a store-level write inside a transaction: deleting one would
// not break the build, since the promoted method satisfies the interface, but
// the write would go straight to the pool and a contended failure would reach
// the caller instead of being retried.

// UpdateAttrs stores or updates file metadata.
// Creates the entry if it doesn't exist.
func (s *PostgresMetadataStore) UpdateAttrs(ctx context.Context, file *metadata.File) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.UpdateAttrs(ctx, file)
	})
}

// SetManifest stores or updates file metadata and rewrites the stored block
// manifest from file.Blocks.
func (s *PostgresMetadataStore) SetManifest(ctx context.Context, file *metadata.File) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetManifest(ctx, file)
	})
}

// DeleteFile removes file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (s *PostgresMetadataStore) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteFile(ctx, handle)
	})
}

// SetChild adds or updates a child entry in a directory.
func (s *PostgresMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetChild(ctx, dirHandle, name, childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
func (s *PostgresMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteChild(ctx, dirHandle, name)
	})
}

// SetLinkCount sets the hard link count for a file.
func (s *PostgresMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (s *PostgresMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// ============================================================================
// Payload ID Operations
// ============================================================================

// FindByObjectID looks up a file by its Merkle-root ObjectID and returns the
// canonical ChunkRef list of the matching row. Returns (nil, nil) on miss
// (zero-valued input or no matching row).
//
// Uses the partial UNIQUE index files_object_id_idx; the LIMIT 1 is defensive
// (the partial UNIQUE constraint already enforces single-row matches for
// non-NULL object_id values).
func (s *PostgresMetadataStore) FindByObjectID(ctx context.Context, objectID block.ObjectID) ([]block.ChunkRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if objectID.IsZero() {
		return nil, nil
	}

	var fileID uuid.UUID
	err := s.queryRow(ctx,
		`SELECT id FROM inodes WHERE object_id = $1 LIMIT 1`,
		objectID[:],
	).Scan(&fileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPgError(err, "FindByObjectID", objectID.String())
	}

	return s.loadFileChunkRefs(ctx, fileID)
}

// CountObjectIDIndexRows implements the storetest.ObjectIDIndexAccessor
// optional capability. Returns the number of files indexed under the
// given objectID via the partial UNIQUE index files_object_id_idx.
//
// Test-only — never call from production code. Used by the
// ConcurrentQuiesceRace scenario to assert exactly one row
// survives the first-committer-wins resolution.
//
// Zero-valued objectID inputs short-circuit to (0, nil) without backend
// access, mirroring FindByObjectID's partial/skip-zero discipline.
func (s *PostgresMetadataStore) CountObjectIDIndexRows(ctx context.Context, objectID block.ObjectID) (int, error) {
	if objectID.IsZero() {
		return 0, nil
	}
	var n int
	err := s.queryRow(ctx,
		`SELECT count(*) FROM inodes WHERE object_id = $1`,
		objectID[:],
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count files.object_id: %w", err)
	}
	return n, nil
}
