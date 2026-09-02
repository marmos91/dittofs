package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	// glebarez/go-sqlite is a pure-Go (no cgo) SQLite driver — a fork of
	// modernc.org/sqlite — that registers the database/sql driver name
	// "sqlite". The control-plane GORM layer uses the same package, so a single
	// registration serves both. Importing modernc.org/sqlite directly would
	// register "sqlite" a SECOND time and panic at init.
	_ "github.com/glebarez/go-sqlite"
	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sharecache"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// sqliteDriverName is the database/sql driver name registered by the imported
// pure-Go SQLite driver.
const sqliteDriverName = "sqlite"

// SQLiteMetadataStore implements the metadata.Store interface using an embedded
// SQLite database (modernc.org/sqlite, pure Go, no cgo). It is a near-verbatim
// port of the Postgres store: the schema, recursive-CTE path reconstruction,
// hard-link model (parent_child_map + nlink), and object_id dedup index are all
// preserved, with SQL adapted to the SQLite dialect.
type SQLiteMetadataStore struct {
	// Core carries the executor and dialect the shared SQL bodies run on, and
	// promotes those bodies onto this type so they exist once for both
	// backends. Embedded by pointer: the transaction embeds its own Core over
	// the open transaction, and nothing is shared between the two but the
	// dialect.
	*storesql.Core

	// db is the database/sql handle over the single SQLite file. SQLite is a
	// single-writer engine; the pool is bounded to keep contention predictable.
	db *sql.DB

	// config holds the store configuration.
	config *SQLiteMetadataStoreConfig

	// capabilities holds the filesystem capabilities.
	capabilities metadata.FilesystemCapabilities

	// logger for structured logging.
	logger *slog.Logger

	// ctx is the store context (for graceful shutdown).
	ctx context.Context

	// cancel cancels the store context.
	cancel context.CancelFunc

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

	// quota tracks per-identity usage (bytes + file count) for regular files,
	// keyed by owner uid / gid. Seeded from a GROUP BY query on startup and
	// updated from each committed transaction's deltas. Guarded by quotaMu.
	quotaMu sync.Mutex
	quota   *basestore.QuotaCache

	// shareCache caches decoded ShareOptions so the permission funnel every
	// read/write/create/setattr traverses does not re-run the options SELECT
	// and JSON decode per op. Every share-record write invalidates it after
	// commit — a stale entry is a wrong permission decision.
	shareCache sharecache.Cache

	// storeID is the engine-persistent identifier, backed by
	// server_config.store_id. Created on first open with a fresh ULID; read
	// thereafter. Immutable for the life of the instance.
	storeID string

	// Sub-stores for lock / client / durable-handle / NFSv4-recovery
	// persistence. Each wraps the shared *sql.DB executor.
	lockStore     *sqliteLockStore
	clientStore   *storesql.ClientStore
	durableStore  *sqliteDurableStore
	recoveryStore *storesql.RecoveryStore
}

// NewSQLiteMetadataStore creates a new SQLite-backed metadata store.
func NewSQLiteMetadataStore(
	ctx context.Context,
	cfg *SQLiteMetadataStoreConfig,
	capabilities metadata.FilesystemCapabilities,
) (*SQLiteMetadataStore, error) {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	log := logger.With("component", "sqlite_metadata_store")

	db, err := sql.Open(sqliteDriverName, cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// SQLite is single-writer. Bounding the pool to one connection serializes
	// access deterministically and keeps a warm connection alive — important
	// for ":memory:" + cache=shared, where the database lives only as long as
	// at least one connection is open.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	// Guard the schema before anything queries it. This is not a migration, so
	// it runs whether or not AutoMigrate is set.
	if err := checkFormatVersion(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	if cfg.AutoMigrate {
		log.Info("AutoMigrate is enabled, running migrations...")
		if err := runMigrations(ctx, db, log); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to run migrations: %w", err)
		}
	} else {
		log.Info("AutoMigrate is disabled, skipping migrations")
	}

	if err := initializeFilesystemCapabilities(ctx, db, capabilities); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize filesystem capabilities: %w", err)
	}

	storeCtx, cancel := context.WithCancel(context.Background())

	store := &SQLiteMetadataStore{
		db:           db,
		config:       cfg,
		capabilities: capabilities,
		logger:       log,
		ctx:          storeCtx,
		cancel:       cancel,
		quota:        basestore.NewQuotaCache(),
	}
	// The shared SQL bodies run on the pool for store-level calls.
	store.Core = &storesql.Core{X: store.conn(), D: sqliteDialect, Caps: store.currentCapabilities}

	// The substores derive only from db, which is never reassigned, so bind
	// them once here.
	store.lockStore = newSQLiteLockStore(store.conn())
	store.clientStore = &storesql.ClientStore{X: store.conn(), D: sqliteDialect}
	store.durableStore = newSQLiteDurableStore(store.conn())
	store.recoveryStore = &storesql.RecoveryStore{X: store.conn(), D: sqliteDialect}

	if err := store.initUsedBytesCounter(ctx); err != nil {
		_ = db.Close()
		cancel()
		return nil, fmt.Errorf("failed to initialize used bytes counter: %w", err)
	}

	sid, err := store.ensureStoreID(ctx)
	if err != nil {
		_ = db.Close()
		cancel()
		return nil, fmt.Errorf("ensure store_id: %w", err)
	}
	store.storeID = sid

	log.Info("SQLite metadata store initialized successfully", "path", cfg.Path)

	return store, nil
}

// GetUsedBytesForShare returns the logical bytes held by one share's regular
// files. O(1) read of the per-share bucket seeded by initUsedBytesCounter and
// maintained by the transaction delta pipeline — the same discipline that keeps
// the per-identity buckets fresh. It sits on the write path (the share-quota
// gate), so it must not issue a query per call.
func (s *SQLiteMetadataStore) GetUsedBytesForShare(ctx context.Context, shareName string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quota.Share(shareName).Bytes, nil
}

// initUsedBytesCounter seeds the usage cache — per share, and per owner
// identity within a share — from GROUP BY aggregates over the inodes table (the
// source of truth).
func (s *SQLiteMetadataStore) initUsedBytesCounter(ctx context.Context) error {
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
func (s *SQLiteMetadataStore) seedUsageByColumn(ctx context.Context, col string, scope metadata.QuotaScope, out map[basestore.QuotaKey]*metadata.UsageStat) error {
	query := fmt.Sprintf(
		`SELECT share_name, %s, COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE file_type = ?1 AND nlink > 0 GROUP BY share_name, %s`,
		col, col,
	)
	rows, err := s.db.QueryContext(ctx, query, int(metadata.FileTypeRegular))
	if err != nil {
		return fmt.Errorf("failed to seed %s usage: %w", col, err)
	}
	defer func() { _ = rows.Close() }()
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
func (s *SQLiteMetadataStore) GetQuotaUsage(shareName string, scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quota.Get(shareName, scope, id), nil
}

// applyQuotaDelta folds a per-identity usage delta into the in-memory usage
// cache. Called post-commit (matching usedBytes).
func (s *SQLiteMetadataStore) applyQuotaDelta(delta map[basestore.QuotaKey]metadata.UsageStat) {
	if len(delta) == 0 {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	s.quota.Apply(delta)
}

// ensureStoreID reads the engine-persistent store_id from server_config; if the
// row is missing or carries the empty sentinel, writes a fresh ULID atomically
// and returns it. Idempotent after bootstrap.
func (s *SQLiteMetadataStore) ensureStoreID(ctx context.Context) (string, error) {
	fresh := ulid.Make().String()
	var existing string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO server_config (id, store_id)
		VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE
		SET store_id = COALESCE(NULLIF(server_config.store_id, ''), excluded.store_id)
		RETURNING store_id
	`, fresh).Scan(&existing)
	if err != nil {
		return "", fmt.Errorf("upsert store_id row: %w", err)
	}
	return existing, nil
}

// GetStoreID returns the SQLite-persistent store identifier (stored in
// server_config.store_id). Stable across restarts.
func (s *SQLiteMetadataStore) GetStoreID() string { return s.storeID }

// Compile-time assertion: the SQLite engine exposes GetStoreID.
var _ interface{ GetStoreID() string } = (*SQLiteMetadataStore)(nil)

// Close records the clean-shutdown marker and closes the database handle.
func (s *SQLiteMetadataStore) Close() error {
	s.logger.Info("Closing SQLite metadata store...")

	// Record a clean-shutdown marker BEFORE closing so the lock-recovery boot
	// path can distinguish a graceful drain from a crash. A persist failure is
	// logged but does not block close.
	markCtx, markCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := s.SetCleanShutdown(markCtx, true); err != nil {
		s.logger.Error("failed to persist clean-shutdown marker on close", "error", err)
	}
	markCancel()

	s.cancel()

	if s.db != nil {
		if err := s.db.Close(); err != nil {
			return err
		}
	}

	s.logger.Info("SQLite metadata store closed")
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
		1, ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14
	)
	ON CONFLICT (id) DO UPDATE SET
		max_read_size = excluded.max_read_size,
		preferred_read_size = excluded.preferred_read_size,
		max_write_size = excluded.max_write_size,
		preferred_write_size = excluded.preferred_write_size,
		max_file_size = excluded.max_file_size,
		max_filename_len = excluded.max_filename_len,
		max_path_len = excluded.max_path_len,
		max_hard_link_count = excluded.max_hard_link_count,
		supports_hard_links = excluded.supports_hard_links,
		supports_symlinks = excluded.supports_symlinks,
		case_sensitive = excluded.case_sensitive,
		case_preserving = excluded.case_preserving,
		supports_acls = excluded.supports_acls,
		time_resolution = excluded.time_resolution
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

// initializeFilesystemCapabilities inserts or updates filesystem capabilities.
func initializeFilesystemCapabilities(ctx context.Context, db *sql.DB, caps metadata.FilesystemCapabilities) error {
	_, err := db.ExecContext(ctx, upsertCapabilitiesSQL, capabilityArgs(caps)...)
	return err
}
