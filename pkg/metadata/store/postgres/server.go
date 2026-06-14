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
	query := `
		INSERT INTO filesystem_capabilities (
			id, max_read_size, preferred_read_size, max_write_size, preferred_write_size,
			max_file_size, max_filename_len, max_path_len, max_hard_link_count,
			supports_hard_links, supports_symlinks, case_sensitive, case_preserving,
			supports_acls, time_resolution
		) VALUES (
			1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
		)
		ON CONFLICT (id) DO UPDATE SET
			max_read_size = EXCLUDED.max_read_size,
			preferred_read_size = EXCLUDED.preferred_read_size,
			max_write_size = EXCLUDED.max_write_size,
			preferred_write_size = EXCLUDED.preferred_write_size,
			max_file_size = EXCLUDED.max_file_size,
			max_filename_len = EXCLUDED.max_filename_len,
			max_path_len = EXCLUDED.max_path_len,
			max_hard_link_count = EXCLUDED.max_hard_link_count,
			supports_hard_links = EXCLUDED.supports_hard_links,
			supports_symlinks = EXCLUDED.supports_symlinks,
			case_sensitive = EXCLUDED.case_sensitive,
			case_preserving = EXCLUDED.case_preserving,
			supports_acls = EXCLUDED.supports_acls,
			time_resolution = EXCLUDED.time_resolution
	`

	_, err := s.exec(ctx, query,
		capabilities.MaxReadSize,
		capabilities.PreferredReadSize,
		capabilities.MaxWriteSize,
		capabilities.PreferredWriteSize,
		capabilities.MaxFileSize,
		capabilities.MaxFilenameLen,
		capabilities.MaxPathLen,
		capabilities.MaxHardLinkCount,
		capabilities.SupportsHardLinks,
		capabilities.SupportsSymlinks,
		capabilities.CaseSensitive,
		capabilities.CasePreserving,
		capabilities.SupportsACLs,
		capabilities.TimestampResolution,
	)

	// Log error but don't fail - capabilities are already cached
	if err != nil {
		s.logger.Warn("Failed to persist capabilities to database", "error", err)
	}
}

// ============================================================================
// Filesystem Statistics
// ============================================================================

// GetFilesystemStatistics returns filesystem statistics with caching.
// UsedBytes is read from the atomic counter (O(1), always fresh).
// File count uses the stats cache for efficiency.
func (s *PostgresMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	// Scope the aggregate to the share encoded in the handle. The atomic
	// usedBytes counter and statsCache are store-wide (sum across all shares),
	// so they cannot answer a per-share query; both are bypassed here in favour
	// of a scoped SQL aggregate (statfsQuery). An invalid handle falls back to
	// the store-wide totals (single-share compatible).
	sql, args := statfsQuery(handle)
	var bytesUsed, filesUsed int64
	if err := s.queryRow(ctx, sql, args...).Scan(&bytesUsed, &filesUsed); err != nil {
		return nil, mapPgError(err, "GetFilesystemStatistics", "")
	}

	// Do NOT populate statsCache here: the cache is store-wide and would be
	// polluted by a per-share result. The cache remains valid only for the
	// store-wide path elsewhere.
	return buildFilesystemStatistics(bytesUsed, filesUsed), nil
}
