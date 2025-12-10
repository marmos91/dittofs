package postgres

import (
	"context"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// GetFilesystemCapabilities returns the filesystem capabilities
func (s *PostgresMetadataStore) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	// Return cached capabilities (set during initialization)
	// Note: handle parameter not used as capabilities are share-level, not file-level
	return &s.capabilities, nil
}

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
		TotalBytes:     1 << 50,         // 1 PB (effectively unlimited)
		AvailableBytes: (1 << 50) - uint64(bytesUsed),
		UsedBytes:      uint64(bytesUsed),
		TotalFiles:     1 << 32,         // 4 billion files
		AvailableFiles: (1 << 32) - uint64(filesUsed),
		UsedFiles:      uint64(filesUsed),
	}

	// Update cache
	s.statsCache.set(stats)

	return &stats, nil
}

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

// Healthcheck verifies the PostgreSQL connection is healthy
func (s *PostgresMetadataStore) Healthcheck(ctx context.Context) error {
	// Simple ping to verify connection
	if err := s.pool.Ping(ctx); err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "PostgreSQL health check failed",
		}
	}

	return nil
}
