package sql

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// ============================================================================
// Server and filesystem reads
// ============================================================================

// GenerateHandle mints a handle for a new file in a share. The path plays no
// part: handles are opaque and share-scoped, and the path is passed only
// because the interface carries it.
func (c *Core) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	return basestore.GenerateHandle(ctx, shareName)
}

// GetFilesystemCapabilities reports the store's configured capabilities. They
// are per-store rather than per-file, so the handle is unused.
//
// The result is a copy. The pool path used to hand back a pointer to the
// store's own field, which let a caller edit the capabilities every other
// caller reads.
func (c *Core) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	caps := c.Caps()
	return &caps, nil
}

// GetFilesystemStatistics reports a share's used bytes and file count, which is
// what statfs is built from.
func (c *Core) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}

	var bytesUsed, filesUsed int64
	err = c.X.QueryRow(ctx, c.D.Shares().Statfs, shareName, int(metadata.FileTypeRegular)).
		Scan(&bytesUsed, &filesUsed)
	if err != nil {
		return nil, c.D.MapError(err, "GetFilesystemStatistics", "")
	}

	return basestore.BuildStatistics(bytesUsed, filesUsed), nil
}
