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
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sharecache"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// PostgresMetadataStore implements the metadata.Store interface using PostgreSQL
type PostgresMetadataStore struct {
	// Core carries the executor and dialect the shared SQL bodies run on, and
	// promotes those bodies onto this type so they exist once for both
	// backends. Embedded by pointer: the transaction embeds its own Core over
	// the open pgx.Tx, and nothing is shared between the two but the dialect.
	*storesql.Core

	// pool is the PostgreSQL connection pool
	pool *pgxpool.Pool

	// config holds the store configuration
	config *PostgresMetadataStoreConfig

	// capabilities holds the filesystem capabilities
	capabilities metadata.FilesystemCapabilities

	// logger for structured logging
	logger *slog.Logger

	// manifestWrites counts how many times a write actually persisted the
	// file_block_refs manifest (i.e. came in through SetManifest). Test-only
	// observability: it lets the conformance suite prove an attr-only write
	// performed ZERO manifest writes (row-count alone cannot — a DELETE+INSERT
	// of the same M rows leaves the same count). Never read in production.
	manifestWrites atomic.Int64

	// manifestRowsScanned counts the stored file_block_refs rows the manifest
	// diff has had to read since open. Test-only observability: it is what
	// ManifestDirtyOffsets bounds, so a test can prove a scoped commit reads the
	// changed offsets rather than the whole file. Never read in production.
	manifestRowsScanned atomic.Int64

	// ctx is the store context (for graceful shutdown)
	ctx context.Context

	// cancel cancels the store context
	cancel context.CancelFunc

	// lockStore holds persisted lock data for NLM/SMB lock persistence.
	lockStore *postgresLockStore

	// clientStore holds NSM client registration persistence.
	clientStore *postgresClientStore

	// durableStore holds SMB3 durable handle persistence.
	durableStore *postgresDurableStore

	// recoveryStore holds NFSv4 client-recovery persistence.
	recoveryStore *postgresRecoveryStore

	// quota tracks per-identity usage (bytes + file count) for regular files,
	// keyed by owner uid / gid. In-memory cache mirroring usedBytes, seeded from
	// a SQL GROUP BY query on startup (the files table is the source of truth, so
	// it is always reconstructed correctly). Updated from a transaction's pending
	// per-identity deltas exactly once on successful commit. Guarded by quotaMu.
	quotaMu sync.Mutex
	quota   *basestore.QuotaCache

	// shareCache caches decoded ShareOptions so the permission funnel every
	// read/write/create/setattr traverses does not re-run the options SELECT
	// and JSON decode per op. Every share-record write invalidates it after
	// commit — a stale entry is a wrong permission decision.
	shareCache sharecache.Cache

	// storeID is the engine-persistent identifier for this store instance,
	// backed by the server_config.store_id column. Created on first open
	// (or after migration 000008 on an existing database) with a fresh
	// ULID; read thereafter. Immutable for the life of the instance.
	//
	// Persisting the ULID with the Postgres schema means a control-plane
	// DB reset (which rotates cfg.ID) does NOT cause the engine to report
	// a different identity.
	storeID string
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

	// Guard the schema before anything queries it. This is not a migration, so
	// it runs whether or not AutoMigrate is set.
	if err := checkFormatVersion(ctx, pool); err != nil {
		pool.Close()
		return nil, err
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
		logger:       log,
		ctx:          storeCtx,
		cancel:       cancel,
		quota:        basestore.NewQuotaCache(),
	}
	// The shared SQL bodies run on the pool for store-level calls.
	store.Core = &storesql.Core{X: poolExecer{s: store}, D: pgDialect}

	// The substores derive only from pool, which is never reassigned, so bind
	// them once here.
	store.lockStore = newPostgresLockStore(pool)
	store.clientStore = newPostgresClientStore(store)
	store.durableStore = newPostgresDurableStore(store)
	store.recoveryStore = newPostgresRecoveryStore(store)

	// Initialize the usedBytes counter from a SQL SUM query.
	if err := store.initUsedBytesCounter(ctx); err != nil {
		pool.Close()
		cancel()
		return nil, fmt.Errorf("failed to initialize used bytes counter: %w", err)
	}

	// Bootstrap the engine-persistent store_id after migrations have run
	// (migration 000008 adds the column). ensureStoreID is idempotent —
	// first call on a fresh schema writes a ULID, later calls read the
	// existing value.
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
		"prepared_statements", !cfg.DisablePreparedStatements,
	)

	return store, nil
}

// GetUsedBytesForShare returns the logical bytes held by one share's regular
// files. O(1) read of the per-share bucket seeded by initUsedBytesCounter and
// maintained by the transaction delta pipeline — the same discipline that keeps
// the per-identity buckets fresh. It sits on the write path (the share-quota
// gate), so it must not issue a query per call.
func (s *PostgresMetadataStore) GetUsedBytesForShare(ctx context.Context, shareName string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quota.Share(shareName).Bytes, nil
}

// initUsedBytesCounter initializes the store-wide atomic counter from a SQL SUM
// query and seeds the per-identity usage cache from GROUP BY aggregates. Both
// are reconstructed from the inodes table (the source
// of truth), so a store opened against an existing database is always seeded
// correctly. The inodes(uid) / inodes(gid) indexes (migration 000033) keep the
// GROUP BY scans cheap.
func (s *PostgresMetadataStore) initUsedBytesCounter(ctx context.Context) error {
	byIdentity := make(map[basestore.QuotaKey]*metadata.UsageStat)
	if err := s.seedUsageByColumn(ctx, "uid", metadata.QuotaScopeUser, byIdentity); err != nil {
		return err
	}
	if err := s.seedUsageByColumn(ctx, "gid", metadata.QuotaScopeGroup, byIdentity); err != nil {
		return err
	}
	s.quotaMu.Lock()
	s.quota.Seed(byIdentity, nil)
	s.quotaMu.Unlock()
	return nil
}

// seedUsageByColumn aggregates usage (bytes + count) for regular files grouped
// by share and by the given owner column ("uid" or "gid"), accumulating into
// out under the matching scope. The column name is a fixed internal constant,
// never user input.
func (s *PostgresMetadataStore) seedUsageByColumn(ctx context.Context, col string, scope metadata.QuotaScope, out map[basestore.QuotaKey]*metadata.UsageStat) error {
	query := fmt.Sprintf(
		`SELECT share_name, %s, COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE file_type = $1 GROUP BY share_name, %s`,
		col, col,
	)
	rows, err := s.pool.Query(ctx, query, int(metadata.FileTypeRegular))
	if err != nil {
		return fmt.Errorf("failed to seed %s usage: %w", col, err)
	}
	defer rows.Close()
	for rows.Next() {
		var share string
		var id int64
		var bytes, files int64
		if err := rows.Scan(&share, &id, &bytes, &files); err != nil {
			return fmt.Errorf("failed to scan %s usage: %w", col, err)
		}
		out[basestore.QuotaKey{Share: share, Scope: scope, ID: uint32(id)}] = &metadata.UsageStat{Bytes: bytes, Files: files}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed iterating %s usage: %w", col, err)
	}
	return nil
}

// GetQuotaUsage returns per-identity usage within one share. O(1) cache read
// under quotaMu. A missing key returns a zero UsageStat.
func (s *PostgresMetadataStore) GetQuotaUsage(shareName string, scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quota.Get(shareName, scope, id), nil
}

// applyQuotaDelta folds a per-identity usage delta into the in-memory usage
// cache. Called post-commit (matching usedBytes). Buckets that drop to zero or
// below are removed.
func (s *PostgresMetadataStore) applyQuotaDelta(delta map[basestore.QuotaKey]metadata.UsageStat) {
	if len(delta) == 0 {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	s.quota.Apply(delta)
}

// ensureStoreID reads the engine-persistent store_id from server_config; if
// the row is missing or carries the empty sentinel, writes a fresh ULID
// atomically and returns it. Safe to call on every open — idempotent after
// bootstrap.
//
// The INSERT ... ON CONFLICT ... RETURNING form performs the check-and-set
// in a single round-trip AND recreates the singleton row when it is absent.
// The row can be absent legitimately: truncateAllTables (Reset and the
// pre-COPY phase of Restore) clears server_config, and AutoMigrate does not
// re-seed it on a reopen (migration 000001 already recorded). An UPDATE-only
// form matched zero rows in that state and failed store open with
// "no rows in result set" — bricking the store after a Reset/restore crash,
// which in turn prevents the boot-time restore-marker recovery from ever
// running. ON CONFLICT preserves any existing non-empty store_id (COALESCE +
// NULLIF treat an empty string as NULL); config and updated_at fall back to
// their column defaults on the insert path and are left untouched on
// conflict. Concurrency-safe: two racing opens converge on one row.
func (s *PostgresMetadataStore) ensureStoreID(ctx context.Context) (string, error) {
	fresh := ulid.Make().String()
	var existing string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO server_config (id, store_id)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE
		SET store_id = COALESCE(NULLIF(server_config.store_id, ''), EXCLUDED.store_id)
		RETURNING store_id
	`, fresh).Scan(&existing)
	if err != nil {
		return "", fmt.Errorf("upsert store_id row: %w", err)
	}
	return existing, nil
}

// GetStoreID returns the Postgres-persistent store identifier (stored in
// server_config.store_id). Stable across restarts — the ULID is written
// once on first open of a freshly-migrated schema and read on every
// subsequent open. Immutable for the life of the instance.
func (s *PostgresMetadataStore) GetStoreID() string { return s.storeID }

// Compile-time assertion: the Postgres engine exposes GetStoreID.
var _ interface{ GetStoreID() string } = (*PostgresMetadataStore)(nil)

// Close closes the PostgreSQL connection pool and releases resources
func (s *PostgresMetadataStore) Close() error {
	s.logger.Info("Closing PostgreSQL metadata store...")

	// Record a clean-shutdown marker BEFORE cancelling the store context and
	// closing the pool, so the lock-recovery boot path can distinguish a
	// graceful drain from a kill -9 / crash. Close is the single graceful
	// teardown site for the store. A dedicated short-lived context is used
	// because s.cancel() below tears down s.ctx. A persist failure is logged but
	// does not block close — the next boot then conservatively treats the start
	// as unclean and enters grace, which is the fail-safe direction.
	markCtx, markCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := s.SetCleanShutdown(markCtx, true); err != nil {
		s.logger.Error("failed to persist clean-shutdown marker on close", "error", err)
	}
	markCancel()

	// Cancel context
	s.cancel()

	// Close connection pool
	if s.pool != nil {
		s.logger.Info("Closing PostgreSQL connection pool")
		s.pool.Close()
	}

	s.logger.Info("PostgreSQL metadata store closed")
	return nil
}

// upsertCapabilitiesSQL writes the single filesystem_capabilities row. Shared
// by store construction and SetFilesystemCapabilities so the two can never
// persist a different column set.
const upsertCapabilitiesSQL = `
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

// capabilityArgs binds a capability set to upsertCapabilitiesSQL's placeholders.
func capabilityArgs(caps metadata.FilesystemCapabilities) []any {
	return []any{
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
	}
}

// initializeFilesystemCapabilities inserts or updates filesystem capabilities in the database
func initializeFilesystemCapabilities(ctx context.Context, pool *pgxpool.Pool, caps metadata.FilesystemCapabilities) error {
	_, err := pool.Exec(ctx, upsertCapabilitiesSQL, capabilityArgs(caps)...)
	return err
}
