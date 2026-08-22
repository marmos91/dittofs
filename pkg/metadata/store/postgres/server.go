package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// ============================================================================
// Server Configuration
// ============================================================================

// GetServerConfig retrieves server-wide configuration
func (s *PostgresMetadataStore) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	query := `SELECT config FROM server_config WHERE id = 1`

	var customSettings map[string]any
	err := s.queryRow(ctx, query).Scan(&customSettings)
	if errors.Is(err, pgx.ErrNoRows) {
		// A fresh store has no persisted config row. Match the memory and
		// badger backends: report an empty (non-nil) config, not a
		// not-found error, so callers can write to CustomSettings.
		return metadata.MetadataServerConfig{CustomSettings: map[string]any{}}, nil
	}
	if err != nil {
		return metadata.MetadataServerConfig{}, mapPgError(err, "GetServerConfig", "")
	}

	// A JSON null/empty column scans to a nil map; hand back a non-nil map so
	// callers can index/write it without a panic (badger parity).
	if customSettings == nil {
		customSettings = map[string]any{}
	}
	return metadata.MetadataServerConfig{
		CustomSettings: customSettings,
	}, nil
}

// SetServerConfig updates server-wide configuration
func (s *PostgresMetadataStore) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	query := `
		INSERT INTO server_config (id, config)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE
		SET config = EXCLUDED.config, updated_at = NOW()
	`

	_, err := s.exec(ctx, query, config.CustomSettings)
	return err
}

// ============================================================================
// Filesystem Capabilities
// ============================================================================

// GetFilesystemCapabilities returns the filesystem capabilities
func (s *PostgresMetadataStore) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	// Return cached capabilities (set during initialization)
	// Note: handle parameter not used as capabilities are share-level, not file-level
	return &s.capabilities, nil
}

// SetFilesystemCapabilities updates the filesystem capabilities
func (s *PostgresMetadataStore) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	// Update cached capabilities
	s.capabilities = capabilities

	// Update database (best effort - don't fail if it errors)
	// This is called during initialization, so database updates are non-critical
	ctx := context.Background()
	_, err := s.exec(ctx, upsertCapabilitiesSQL, capabilityArgs(capabilities)...)

	// Log error but don't fail - capabilities are already cached
	if err != nil {
		s.logger.Warn("Failed to persist capabilities to database", "error", err)
	}
}

// ============================================================================
// Filesystem Statistics
// ============================================================================

// GetFilesystemStatistics returns filesystem statistics scoped to the share
// encoded in the handle. The store-wide atomic usedBytes counter cannot answer
// a per-share query, so a scoped SQL aggregate (statfsQuery) is used instead.
// A handle that does not decode names no share and is rejected.
func (s *PostgresMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}
	sql, args := statfsQuery(shareName)
	var bytesUsed, filesUsed int64
	if err := s.queryRow(ctx, sql, args...).Scan(&bytesUsed, &filesUsed); err != nil {
		return nil, mapPgError(err, "GetFilesystemStatistics", "")
	}
	return basestore.BuildStatistics(bytesUsed, filesUsed), nil
}
