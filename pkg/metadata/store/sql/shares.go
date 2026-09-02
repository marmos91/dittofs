package sql

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// ============================================================================
// Share reads
// ============================================================================
//
// As with the file reads, these are pure and so want the identical body on the
// pool path and the transaction path. GetShareOptions is the uncached backing
// read; the store shadows it with its share cache and calls back into this one
// for the miss.

// GetRootHandle returns the root handle for a share, reporting ErrNotFound when
// the share does not exist.
func (c *Core) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rootID uuid.UUID
	if err := c.X.QueryRow(ctx, c.D.Shares().GetRootHandle, shareName).Scan(&rootID); err != nil {
		return nil, c.D.MapError(err, "GetRootHandle", shareName)
	}

	return metadata.EncodeShareHandle(shareName, rootID)
}

// GetShareOptions reads a share's options straight from the backing store,
// reporting ErrNotFound when the share does not exist.
//
// The returned value is freshly decoded on every call and aliases nothing, so
// a caller may hold or mutate it. That is what lets the store layer cache it
// by handing out copies.
func (c *Core) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var optionsJSON []byte
	if err := c.X.QueryRow(ctx, c.D.Shares().GetShareOptions, shareName).Scan(&optionsJSON); err != nil {
		return nil, c.D.MapError(err, "GetShareOptions", shareName)
	}

	var options metadata.ShareOptions
	if len(optionsJSON) > 0 {
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			return nil, fmt.Errorf("failed to unmarshal share options: %w", err)
		}
	}

	return &options, nil
}

// ListShares returns the names of every share in the store.
func (c *Core) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := c.X.Query(ctx, c.D.Shares().ListShares)
	if err != nil {
		return nil, c.D.MapError(err, "ListShares", "")
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}

	// Surface an error that terminated the iteration early so a partial share
	// list is not returned as if it were complete.
	if err := rows.Err(); err != nil {
		return nil, c.D.MapError(err, "ListShares", "")
	}

	return names, nil
}

// GetFilesystemMeta returns a share's stored filesystem metadata, or the
// store's configured capabilities when the share has none.
//
// ponytail: every read failure reports the defaults — not just a missing row —
// because postgres has no filesystem_meta table at all and each call there
// fails with an undefined-relation error. The one exception is a context that
// has given out, cancelled or past its deadline alike, which is returned.
// Narrow this to the no-rows case alone once that table exists on both
// dialects; until then the strict version fails the conformance suite on
// postgres.
func (c *Core) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var data []byte
	if err := c.X.QueryRow(ctx, c.D.Shares().GetFilesystemMeta, shareName).Scan(&data); err != nil {
		// The context can give out while the query is in flight, and a
		// request that is cancelled or past its deadline must not read as a
		// share with default capabilities.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return &metadata.FilesystemMeta{Capabilities: c.Caps()}, nil
	}

	var meta metadata.FilesystemMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// PutFilesystemMeta stores a share's filesystem metadata, replacing whatever
// was there.
func (c *Core) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	if _, err := c.X.Exec(ctx, c.D.Shares().PutFilesystemMeta, shareName, data); err != nil {
		return c.D.MapError(err, "PutFilesystemMeta", shareName)
	}
	return nil
}

// UpdateShareOptions replaces an existing share's options, reporting
// metadata.ErrNotFound when the share does not exist.
func (c *Core) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(options)
	if err != nil {
		return err
	}

	result, err := c.X.Exec(ctx, c.D.Shares().SetShareOptions, data, shareName)
	if err != nil {
		return c.D.MapError(err, "UpdateShareOptions", shareName)
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

// DeleteShare removes a share and every inode belonging to it, reporting
// metadata.ErrNotFound when the share does not exist.
func (c *Core) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Aggregate the usage about to disappear before the rows go, so the
	// store's quota cache can be decremented once the transaction commits.
	if err := c.collectShareQuotaFreed(ctx, shareName, "uid", metadata.QuotaScopeUser); err != nil {
		return err
	}
	if err := c.collectShareQuotaFreed(ctx, shareName, "gid", metadata.QuotaScopeGroup); err != nil {
		return err
	}

	// Drop the share row first: shares.root_file_id references inodes(id)
	// WITHOUT ON DELETE CASCADE, so the inode rows cannot be removed while the
	// share still points at the root inode.
	result, err := c.X.Exec(ctx, c.D.Shares().DeleteShare, shareName)
	if err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}
	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	// The contract is "removes a share and all its metadata"; dropping only the
	// share row orphans every inodes/parent_child_map/file_block_refs row.
	// parent_child_map and file_block_refs both cascade from inodes(id).
	if _, err := c.X.Exec(ctx, c.D.Shares().DeleteShareInodes, shareName); err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}

	return nil
}

// collectShareQuotaFreed aggregates the regular-file usage of a share being
// deleted, grouped by uid or gid, and records the negative delta so the
// in-memory usage cache is decremented after the commit.
func (c *Core) collectShareQuotaFreed(ctx context.Context, shareName, col string, scope metadata.QuotaScope) error {
	query := fmt.Sprintf(c.D.Shares().ShareQuotaFreed, col, col)
	rows, err := c.X.Query(ctx, query, shareName, int(metadata.FileTypeRegular))
	if err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}
	defer rows.Close()

	for rows.Next() {
		var id, bytes, files int64
		if err := rows.Scan(&id, &bytes, &files); err != nil {
			return c.D.MapError(err, "DeleteShare", shareName)
		}
		c.Quota.AddKeyed(
			basestore.QuotaKey{Share: shareName, Scope: scope, ID: uint32(id)},
			metadata.UsageStat{Bytes: -bytes, Files: -files},
		)
	}
	if err := rows.Err(); err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}
	return nil
}
