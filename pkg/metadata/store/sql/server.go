package sql

import (
	"context"
	"encoding/json"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// ============================================================================
// Server and filesystem operations
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

// GetServerConfig reads the store-wide server configuration.
//
// A missing row is an empty configuration rather than a not-found error, which
// is what the memory and badger backends report for a store that has never had
// one written.
func (c *Core) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	var raw []byte
	err := c.X.QueryRow(ctx, c.D.Server().GetServerConfig).Scan(&raw)
	if c.D.IsNoRows(err) {
		return metadata.MetadataServerConfig{CustomSettings: map[string]any{}}, nil
	}
	if err != nil {
		return metadata.MetadataServerConfig{}, c.D.MapError(err, "GetServerConfig", "")
	}

	settings := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return metadata.MetadataServerConfig{}, c.D.MapError(err, "GetServerConfig", "")
		}
	}
	return metadata.MetadataServerConfig{CustomSettings: settings}, nil
}

// SetServerConfig writes the store-wide server configuration, replacing
// whatever was there.
//
// Nil settings are stored as an empty object, so the column never holds SQL
// NULL and the read above never has to tell "no row" from "row holding null".
func (c *Core) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	settings := config.CustomSettings
	if settings == nil {
		settings = map[string]any{}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return c.D.MapError(err, "SetServerConfig", "")
	}

	if _, err := c.X.Exec(ctx, c.D.Server().SetServerConfig, raw); err != nil {
		return c.D.MapError(err, "SetServerConfig", "")
	}
	return nil
}
