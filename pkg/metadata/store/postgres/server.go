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
	err := s.queryRow(ctx, query).Scan(&customSettings)
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
	// Read usage from atomic counter (O(1), always fresh).
	bytesUsed := uint64(s.usedBytes.Load())

	// Check cache for file count.
	if cached, valid := s.statsCache.get(); valid {
		// Update UsedBytes from atomic counter (cache may be stale for bytes).
		cached.UsedBytes = bytesUsed
		if cached.TotalBytes > bytesUsed {
			cached.AvailableBytes = cached.TotalBytes - bytesUsed
		} else {
			cached.AvailableBytes = 0
		}
		return &cached, nil
	}

	// Cache miss - query file count only (usage comes from atomic counter).
	query := `SELECT COUNT(*) FROM files`

	var filesUsed int64
	err := s.queryRow(ctx, query).Scan(&filesUsed)
	if err != nil {
		return nil, mapPgError(err, "GetFilesystemStatistics", "")
	}

	totalBytes := uint64(1 << 50) // 1 PB (effectively unlimited)
	availableBytes := uint64(0)
	if totalBytes > bytesUsed {
		availableBytes = totalBytes - bytesUsed
	}

	totalFiles := uint64(1 << 32) // 4 billion files
	availableFiles := uint64(0)
	if totalFiles > uint64(filesUsed) {
		availableFiles = totalFiles - uint64(filesUsed)
	}

	stats := metadata.FilesystemStatistics{
		TotalBytes:     totalBytes,
		AvailableBytes: availableBytes,
		UsedBytes:      bytesUsed,
		TotalFiles:     totalFiles,
		AvailableFiles: availableFiles,
		UsedFiles:      uint64(filesUsed),
	}

	// Update cache.
	s.statsCache.set(stats)

	return &stats, nil
}
