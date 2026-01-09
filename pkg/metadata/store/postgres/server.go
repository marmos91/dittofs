package postgres

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Server Configuration
// ============================================================================

// GetServerConfig retrieves server-wide configuration
func (s *PostgresMetadataStore) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	query := `SELECT config FROM server_config WHERE id = 1`

	var customSettings map[string]any
	err := s.pool.QueryRow(ctx, query).Scan(&customSettings)
	if err != nil {
		return metadata.MetadataServerConfig{}, mapPgError(err, "GetServerConfig", "")
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

	_, err := s.pool.Exec(ctx, query, config.CustomSettings)
	if err != nil {
		return mapPgError(err, "SetServerConfig", "")
	}

	return nil
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

	_, err := s.pool.Exec(ctx, query,
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

// GetFilesystemStatistics returns filesystem statistics with caching
func (s *PostgresMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	// Check cache first
	if stats, valid := s.statsCache.get(); valid {
		return &stats, nil
	}

	// Cache miss - query database
	query := `
		SELECT
			COALESCE(SUM(size), 0) AS total_bytes_used,
			COUNT(*) AS total_files_used
		FROM files
	`

	var bytesUsed, filesUsed int64
	err := s.pool.QueryRow(ctx, query).Scan(&bytesUsed, &filesUsed)
	if err != nil {
		return nil, mapPgError(err, "GetFilesystemStatistics", "")
	}

	// For PostgreSQL, we don't have hard limits on storage
	// Return very large values to indicate "unlimited"
	// In production, you might want to configure these based on your PostgreSQL instance
	stats := metadata.FilesystemStatistics{
		TotalBytes:     1 << 50, // 1 PB (effectively unlimited)
		AvailableBytes: (1 << 50) - uint64(bytesUsed),
		UsedBytes:      uint64(bytesUsed),
		TotalFiles:     1 << 32, // 4 billion files
		AvailableFiles: (1 << 32) - uint64(filesUsed),
		UsedFiles:      uint64(filesUsed),
	}

	// Update cache
	s.statsCache.set(stats)

	return &stats, nil
}

