package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/metadata"
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

// currentCapabilities reports the capabilities the store is configured with
// right now. SetFilesystemCapabilities replaces them, so the shared reads take
// this rather than a copy captured at construction.
func (s *PostgresMetadataStore) currentCapabilities() metadata.FilesystemCapabilities {
	return s.capabilities
}
