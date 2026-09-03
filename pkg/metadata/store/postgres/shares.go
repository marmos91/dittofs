package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sharecache"
)

// ============================================================================
// Handle/Share Operations
// ============================================================================

// GetShareOptions returns the share configuration options, reporting
// ErrNotFound if the share does not exist.
//
// This shadows the promoted Core.GetShareOptions to put the share cache in
// front of it: every permission check funnels through this read, so the SELECT
// and decode are worth skipping. The returned value is always a deep copy, so
// a caller can never reach the shared cache entry.
func (s *PostgresMetadataStore) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	// Ahead of the cache lookup, not just inside the backing read: a hit must
	// not report success for a request whose context has already given out.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if cached, ok := s.shareCache.Get(shareName); ok {
		return sharecache.Clone(cached), nil
	}

	// Snapshot the invalidation generation BEFORE the backing read so a write
	// that races this read cannot leave a stale value cached (Store checks it).
	gen := s.shareCache.Generation()

	options, err := s.Core.GetShareOptions(ctx, shareName)
	if err != nil {
		return nil, err
	}

	s.shareCache.Store(shareName, options, gen)
	return sharecache.Clone(options), nil
}

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

// UpdateShareOptions updates the share configuration options.
func (s *PostgresMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	optionsData, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("failed to marshal share options: %w", err)
	}

	query := `UPDATE shares SET options = $1 WHERE share_name = $2`
	result, err := s.exec(ctx, query, optionsData, shareName)
	// Drop the cached options AFTER the write lands, whatever it reported: an
	// extra invalidation costs a re-read, a missed one is a stale permission.
	s.shareCache.InvalidateAll()
	if err != nil {
		return err
	}

	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	return nil
}

// DeleteShare removes a share and all its metadata. Runs inside a
// transaction so the share row and its file rows are dropped atomically
// (see the tx-path for the cascade rationale).
func (s *PostgresMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteShare(ctx, shareName)
	})
}

// ============================================================================
// Root Directory Operations
// ============================================================================

// CreateRootDirectory creates the root directory for a share.
//
// The store path runs the shared body through a transaction rather than on the
// pool: the probe and the create must not have a commit between them, or a
// concurrent caller slips in and leaves an orphaned root inode behind. Going
// through the transaction method is also what marks the share cache dirty.
func (s *PostgresMetadataStore) CreateRootDirectory(
	ctx context.Context,
	shareName string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	var root *metadata.File
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var txErr error
		root, txErr = tx.CreateRootDirectory(ctx, shareName, attr)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return root, nil
}
