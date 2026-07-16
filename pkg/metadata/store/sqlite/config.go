package sqlite

import (
	"fmt"
	"strings"
	"time"
)

// SQLiteMetadataStoreConfig holds the configuration for the SQLite metadata
// store. SQLite is an embedded, single-file engine, so there is no host/port/
// credential surface — the only required parameter is the on-disk database
// path.
type SQLiteMetadataStoreConfig struct {
	// Path is the on-disk location of the SQLite database file. The special
	// value ":memory:" opens an ephemeral in-memory database (used by tests).
	Path string `mapstructure:"path" validate:"required"`

	// BusyTimeout bounds how long a statement waits for the write lock held by
	// another connection before returning SQLITE_BUSY. SQLite serializes
	// writers; a generous busy timeout lets concurrent operations queue rather
	// than fail. Default: 5s.
	BusyTimeout time.Duration `mapstructure:"busy_timeout"`

	// QueryTimeout bounds an individual statement. Default: 30s.
	QueryTimeout time.Duration `mapstructure:"query_timeout"`

	// AutoMigrate runs embedded migrations on open when true. Default: false
	// (the factory enables it explicitly, matching the Postgres backend).
	AutoMigrate bool `mapstructure:"auto_migrate"`

	// StatsCacheTTL is retained for parity with the Postgres config surface.
	// Default: 5s.
	StatsCacheTTL time.Duration `mapstructure:"stats_cache_ttl"`
}

// ApplyDefaults sets default values for unspecified configuration fields.
func (c *SQLiteMetadataStoreConfig) ApplyDefaults() {
	if c.BusyTimeout == 0 {
		c.BusyTimeout = 5 * time.Second
	}
	if c.QueryTimeout == 0 {
		c.QueryTimeout = 30 * time.Second
	}
	if c.StatsCacheTTL == 0 {
		c.StatsCacheTTL = 5 * time.Second
	}
}

// Validate checks the configuration for required fields and sane values.
func (c *SQLiteMetadataStoreConfig) Validate() error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("sqlite metadata store requires a non-empty path")
	}
	return nil
}

// DSN builds the database/sql data-source string for modernc.org/sqlite.
//
// Foreign-key enforcement is OFF by default in SQLite; the schema relies on
// ON DELETE CASCADE (parent_child_map, file_block_refs), so it MUST be enabled
// per connection. busy_timeout (milliseconds) lets writers queue behind the
// single-writer lock instead of failing immediately. journal_mode=WAL improves
// read/write concurrency for the embedded single-binary workload this store
// targets. synchronous=NORMAL is the documented WAL pairing: it drops the
// per-commit fsync to a checkpoint-only fsync, which is what makes a large
// write-heavy flush (e.g. a multi-GiB buffered write draining thousands of
// block-record commits) sustainable instead of fsync-bound — modernc.org/sqlite
// otherwise defaults to FULL. It stays durable across app crashes; only an OS
// crash between checkpoints can lose the last commits, acceptable for this store.
// The pragmas are passed via the `_pragma` query parameters that
// modernc.org/sqlite recognises, so they apply to every pooled connection.
//
// #1687 note: this store has no strict/relaxed durability split — WAL+NORMAL
// applies unconditionally and it does not implement RelaxedTransactor, so
// withRelaxedTransaction falls back to WithTransaction here. A durable=true flush
// (FILE_SYNC WRITE, SMB CLOSE/FLUSH, shutdown) is therefore only best-effort on
// sqlite (NORMAL fsyncs at checkpoint, not per commit). This is a pre-existing
// property of the sqlite backend, documented here — not changed by #1687.
func (c *SQLiteMetadataStoreConfig) DSN() string {
	// An in-memory database must be shared across the connection pool, otherwise
	// each connection would see its own empty database. The "file::memory:" +
	// cache=shared form gives all connections in the pool one shared in-memory
	// database for the process lifetime.
	path := c.Path
	if path == ":memory:" {
		path = "file::memory:?cache=shared"
	}

	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	busyMS := c.BusyTimeout.Milliseconds()
	if busyMS <= 0 {
		busyMS = 5000
	}
	return fmt.Sprintf(
		"%s%s_pragma=foreign_keys(1)&_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		path, sep, busyMS,
	)
}
