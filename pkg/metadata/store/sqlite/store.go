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

	// usedBytes tracks the total logical bytes used by regular files. Updated
	// atomically on every size-changing operation. Initialized from a SQL SUM
	// query on startup.
	usedBytes atomic.Int64

	// userUsage / groupUsage track per-identity usage (bytes + file count) for
	// regular files, keyed by owner uid / gid. Seeded from a GROUP BY query on
	// startup and updated from each committed transaction's deltas. Guarded by
	// quotaMu.
	quotaMu    sync.Mutex
	userUsage  map[uint32]*metadata.UsageStat
	groupUsage map[uint32]*metadata.UsageStat

	// storeID is the engine-persistent identifier, backed by
	// server_config.store_id. Created on first open with a fresh ULID; read
	// thereafter. Immutable for the life of the instance.
	storeID string

	// Lazily-initialized sub-stores for lock / client / durable-handle /
	// NFSv4-recovery persistence. Each wraps the shared *sql.DB executor.
	lockStore   *sqliteLockStore
	lockStoreMu sync.Mutex

	clientStore   *sqliteClientStore
	clientStoreMu sync.Mutex

	durableStore   *sqliteDurableStore
	durableStoreMu sync.Mutex

	recoveryStore   *sqliteRecoveryStore
	recoveryStoreMu sync.Mutex
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
	}

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

// GetUsedBytes returns the current total logical bytes used by regular files.
func (s *SQLiteMetadataStore) GetUsedBytes() int64 {
	return s.usedBytes.Load()
}

// initUsedBytesCounter initializes the store-wide atomic counter from a SQL SUM
// query and seeds the per-identity usage cache from GROUP BY aggregates. Both
// are reconstructed from the inodes table (the source of truth).
func (s *SQLiteMetadataStore) initUsedBytesCounter(ctx context.Context) error {
	query := `SELECT COALESCE(SUM(size), 0) FROM inodes WHERE file_type = ?`
	var totalUsed int64
	if err := s.db.QueryRowContext(ctx, query, int(metadata.FileTypeRegular)).Scan(&totalUsed); err != nil {
		return fmt.Errorf("failed to query used bytes: %w", err)
	}
	s.usedBytes.Store(totalUsed)

	userUsage, err := s.seedUsageByColumn(ctx, "uid")
	if err != nil {
		return err
	}
	groupUsage, err := s.seedUsageByColumn(ctx, "gid")
	if err != nil {
		return err
	}
	s.quotaMu.Lock()
	s.userUsage = userUsage
	s.groupUsage = groupUsage
	s.quotaMu.Unlock()
	return nil
}

// seedUsageByColumn aggregates per-identity usage (bytes + count) for regular
// files grouped by the given owner column ("uid" or "gid"). The column name is
// a fixed internal constant, never user input.
func (s *SQLiteMetadataStore) seedUsageByColumn(ctx context.Context, col string) (map[uint32]*metadata.UsageStat, error) {
	query := fmt.Sprintf(
		`SELECT %s, COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE file_type = ? GROUP BY %s`,
		col, col,
	)
	rows, err := s.db.QueryContext(ctx, query, int(metadata.FileTypeRegular))
	if err != nil {
		return nil, fmt.Errorf("failed to seed %s usage: %w", col, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[uint32]*metadata.UsageStat)
	for rows.Next() {
		var id int64
		var bytes, files int64
		if err := rows.Scan(&id, &bytes, &files); err != nil {
			return nil, fmt.Errorf("failed to scan %s usage: %w", col, err)
		}
		out[uint32(id)] = &metadata.UsageStat{Bytes: bytes, Files: files}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed iterating %s usage: %w", col, err)
	}
	return out, nil
}

// GetQuotaUsage returns per-identity usage for the given scope and id.
func (s *SQLiteMetadataStore) GetQuotaUsage(scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	m := s.userUsage
	if scope == metadata.QuotaScopeGroup {
		m = s.groupUsage
	}
	if u, ok := m[id]; ok {
		return *u, nil
	}
	return metadata.UsageStat{}, nil
}

// applyQuotaDelta folds a per-identity usage delta into the in-memory usage
// cache. Called post-commit (matching usedBytes).
func (s *SQLiteMetadataStore) applyQuotaDelta(delta map[sqQuotaKey]metadata.UsageStat) {
	if len(delta) == 0 {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	for k, d := range delta {
		m := s.userUsage
		if k.scope == metadata.QuotaScopeGroup {
			m = s.groupUsage
		}
		cur := m[k.id]
		if cur == nil {
			cur = &metadata.UsageStat{}
			m[k.id] = cur
		}
		cur.Bytes += d.Bytes
		cur.Files += d.Files
		if cur.Bytes < 0 {
			cur.Bytes = 0
		}
		if cur.Files < 0 {
			cur.Files = 0
		}
		if cur.Bytes == 0 && cur.Files == 0 {
			delete(m, k.id)
		}
	}
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

// SyncDurable forces WAL-buffered commits to disk — the durability barrier the
// Service's commit leader coalesces (#1573). Under journal_mode=WAL +
// synchronous=NORMAL (config.go) a COMMIT does not fsync the WAL, so a PASSIVE
// checkpoint is the barrier: it fsyncs the WAL (making committed transactions
// durable) and folds ready frames into the main DB. PASSIVE never blocks on
// concurrent readers/writers — it syncs what is committed and returns.
//
// Correctness depends on this store's single-connection pool (SetMaxOpenConns(1)
// in Open): a PASSIVE checkpoint syncs the WAL only for frames up to the oldest
// reader's mark, so with concurrent readers on stale snapshots it could skip
// just-committed frames. With exactly one serialized connection there is no
// second reader pinning an older snapshot when this runs, so PASSIVE fsyncs all
// committed frames — equivalent to FULL here. A future move to a pooled/multi-
// connection SQLite setup would reintroduce that gap and must revisit this.
func (s *SQLiteMetadataStore) SyncDurable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
	return err
}

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

// initializeFilesystemCapabilities inserts or updates filesystem capabilities.
func initializeFilesystemCapabilities(ctx context.Context, db *sql.DB, caps metadata.FilesystemCapabilities) error {
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
			1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
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

	_, err := db.ExecContext(ctx, query,
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
