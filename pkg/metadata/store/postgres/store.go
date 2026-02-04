package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// PostgresMetadataStore implements the metadata.MetadataStore interface using PostgreSQL
type PostgresMetadataStore struct {
	// pool is the PostgreSQL connection pool
	pool *pgxpool.Pool

	// config holds the store configuration
	config *PostgresMetadataStoreConfig

	// capabilities holds the filesystem capabilities
	capabilities metadata.FilesystemCapabilities

	// statsCache caches filesystem statistics
	statsCache *statsCache

	// logger for structured logging
	logger *slog.Logger

	// ctx is the store context (for graceful shutdown)
	ctx context.Context

	// cancel cancels the store context
	cancel context.CancelFunc

	// lockStore holds persisted lock data for NLM/SMB lock persistence.
	// Initialized lazily via initLockStore().
	lockStore   *postgresLockStore
	lockStoreMu sync.Mutex
}

// statsCache holds cached filesystem statistics
type statsCache struct {
	stats     metadata.FilesystemStatistics
	hasStats  bool
	timestamp time.Time
	ttl       time.Duration
	mu        sync.RWMutex
}

// NewPostgresMetadataStore creates a new PostgreSQL-backed metadata store
func NewPostgresMetadataStore(
	ctx context.Context,
	cfg *PostgresMetadataStoreConfig,
	capabilities metadata.FilesystemCapabilities,
) (*PostgresMetadataStore, error) {
	// Apply defaults
	cfg.ApplyDefaults()

	// Create logger using internal logger
	log := logger.With("component", "postgres_metadata_store")

	// Create connection pool
	pool, err := createConnectionPool(ctx, cfg, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Run migrations if AutoMigrate is enabled
	if cfg.AutoMigrate {
		log.Info("AutoMigrate is enabled, running migrations...")
		if err := runMigrations(ctx, cfg.ConnectionString(), log); err != nil {
			pool.Close()
			return nil, fmt.Errorf("failed to run migrations: %w", err)
		}
	} else {
		log.Info("AutoMigrate is disabled, skipping migrations")
		log.Info("Run 'dittofs migrate' to apply migrations manually")
	}

	// Initialize filesystem capabilities in database
	if err := initializeFilesystemCapabilities(ctx, pool, capabilities); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to initialize filesystem capabilities: %w", err)
	}

	// Create store context
	storeCtx, cancel := context.WithCancel(context.Background())

	store := &PostgresMetadataStore{
		pool:         pool,
		config:       cfg,
		capabilities: capabilities,
		statsCache: &statsCache{
			ttl: cfg.StatsCacheTTL,
		},
		logger: log,
		ctx:    storeCtx,
		cancel: cancel,
	}

	log.Info("PostgreSQL metadata store initialized successfully",
		"host", cfg.Host,
		"database", cfg.Database,
		"max_conns", cfg.MaxConns,
		"stats_cache_ttl", cfg.StatsCacheTTL,
		"prepare_statements", cfg.PrepareStatements,
	)

	return store, nil
}

// Close closes the PostgreSQL connection pool and releases resources
func (s *PostgresMetadataStore) Close() error {
	s.logger.Info("Closing PostgreSQL metadata store...")

	// Cancel context
	s.cancel()

	// Close connection pool
	closeConnectionPool(s.pool, s.logger)

	s.logger.Info("PostgreSQL metadata store closed")
	return nil
}

// initializeFilesystemCapabilities inserts or updates filesystem capabilities in the database
func initializeFilesystemCapabilities(ctx context.Context, pool *pgxpool.Pool, caps metadata.FilesystemCapabilities) error {
	query := `
		INSERT INTO filesystem_capabilities (
			id,
			max_read_size,
			preferred_read_size,
			max_write_size,
			preferred_write_size,
			max_file_size,
			max_filename_len,
			max_path_len,
			max_hard_link_count,
			supports_hard_links,
			supports_symlinks,
			case_sensitive,
			case_preserving,
			supports_acls,
			time_resolution
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

	_, err := pool.Exec(ctx, query,
		caps.MaxReadSize,
		caps.PreferredReadSize,
		caps.MaxWriteSize,
		caps.PreferredWriteSize,
		caps.MaxFileSize,
		caps.MaxFilenameLen,
		caps.MaxPathLen,
		caps.MaxHardLinkCount,
		caps.SupportsHardLinks,
		caps.SupportsSymlinks,
		caps.CaseSensitive,
		caps.CasePreserving,
		caps.SupportsACLs,
		caps.TimestampResolution,
	)

	return err
}

// Helper method to get cached stats
func (sc *statsCache) get() (metadata.FilesystemStatistics, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	if !sc.hasStats || time.Since(sc.timestamp) >= sc.ttl {
		return metadata.FilesystemStatistics{}, false
	}

	return sc.stats, true
}

// Helper method to set cached stats
func (sc *statsCache) set(stats metadata.FilesystemStatistics) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.stats = stats
	sc.hasStats = true
	sc.timestamp = time.Now()
}
