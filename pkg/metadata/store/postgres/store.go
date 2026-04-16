package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

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

	// clientStore holds NSM client registration persistence.
	// Initialized lazily via getClientStore().
	clientStore   *postgresClientStore
	clientStoreMu sync.Mutex

	// durableStore holds SMB3 durable handle persistence.
	// Initialized lazily via getDurableStore().
	durableStore   *postgresDurableStore
	durableStoreMu sync.Mutex

	// usedBytes tracks the total logical bytes used by regular files.
	// Updated atomically on every size-changing operation (create, update, truncate, delete).
	// Initialized from a SQL SUM query on startup.
	usedBytes atomic.Int64

	// storeID is the engine-persistent identifier for this store instance,
	// backed by the server_config.store_id column. Created on first open
	// (or after migration 000008 on an existing database) with a fresh
	// ULID; read thereafter. Immutable for the life of the instance.
	//
	// Used by Phase 5 restore's D-06 store-identity gate (Pitfall #4) so a
	// control-plane DB reset (which rotates cfg.ID) does NOT cause the
	// engine to report a different identity — the ULID persists with the
	// Postgres schema itself.
	storeID string
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

	// Initialize the usedBytes counter from a SQL SUM query.
	if err := store.initUsedBytesCounter(ctx); err != nil {
		pool.Close()
		cancel()
		return nil, fmt.Errorf("failed to initialize used bytes counter: %w", err)
	}

	// Phase 5 D-06: bootstrap the engine-persistent store_id after
	// migrations have run (migration 000008 adds the column). ensureStoreID
	// is idempotent — first call on a fresh schema writes a ULID, later
	// calls read the existing value.
	sid, err := store.ensureStoreID(ctx)
	if err != nil {
		pool.Close()
		cancel()
		return nil, fmt.Errorf("ensure store_id: %w", err)
	}
	store.storeID = sid

	log.Info("PostgreSQL metadata store initialized successfully",
		"host", cfg.Host,
		"database", cfg.Database,
		"max_conns", cfg.MaxConns,
		"stats_cache_ttl", cfg.StatsCacheTTL,
		"prepare_statements", cfg.PrepareStatements,
	)

	return store, nil
}

// GetUsedBytes returns the current total logical bytes used by regular files.
// This is an O(1) atomic read, safe for concurrent access without locks.
func (s *PostgresMetadataStore) GetUsedBytes() int64 {
	return s.usedBytes.Load()
}

// initUsedBytesCounter initializes the atomic counter from a SQL SUM query.
func (s *PostgresMetadataStore) initUsedBytesCounter(ctx context.Context) error {
	query := `SELECT COALESCE(SUM(size), 0) FROM files WHERE file_type = $1`
	var totalUsed int64
	err := s.pool.QueryRow(ctx, query, int(metadata.FileTypeRegular)).Scan(&totalUsed)
	if err != nil {
		return fmt.Errorf("failed to query used bytes: %w", err)
	}
	s.usedBytes.Store(totalUsed)
	return nil
}

// ensureStoreID reads the engine-persistent store_id from server_config; if
// empty (migration 000008 default or a hand-cleared row), writes a fresh
// ULID atomically and returns it. Safe to call on every open — idempotent
// after bootstrap.
//
// The UPDATE ... RETURNING form performs the check-and-set in a single
// round-trip: COALESCE(NULLIF(store_id, ”), $1) keeps any existing
// non-empty value and substitutes the fresh ULID when empty.
//
// Used by Phase 5 D-06 store-identity gate (Pitfall #4).
func (s *PostgresMetadataStore) ensureStoreID(ctx context.Context) (string, error) {
	fresh := ulid.Make().String()
	var existing string
	err := s.pool.QueryRow(ctx, `
		UPDATE server_config
		SET store_id = COALESCE(NULLIF(store_id, ''), $1)
		WHERE id = 1
		RETURNING store_id
	`, fresh).Scan(&existing)
	if err != nil {
		return "", fmt.Errorf("ensure store_id: %w", err)
	}
	return existing, nil
}

// GetStoreID returns the Postgres-persistent store identifier (stored in
// server_config.store_id). Stable across restarts — the ULID is written
// once on first open of a freshly-migrated schema and read on every
// subsequent open. Immutable for the life of the instance.
//
// Used by Phase 5 restore's D-06 store-identity gate (Pitfall #4).
func (s *PostgresMetadataStore) GetStoreID() string { return s.storeID }

// Compile-time assertion: the Postgres engine exposes GetStoreID so the
// Phase 5 restore orchestrator can fetch the engine-persistent ID via a
// type assertion (see pkg/controlplane/runtime/storebackups/target.go).
var _ interface{ GetStoreID() string } = (*PostgresMetadataStore)(nil)

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
