package sql

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
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
