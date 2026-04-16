package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// DatabaseType defines the supported database backends.
type DatabaseType string

const (
	// DatabaseTypeSQLite uses SQLite (single-node, default).
	DatabaseTypeSQLite DatabaseType = "sqlite"

	// DatabaseTypePostgres uses PostgreSQL (HA-capable).
	DatabaseTypePostgres DatabaseType = "postgres"
)

// SQLiteConfig contains SQLite-specific configuration.
type SQLiteConfig struct {
	// Path is the path to the SQLite database file.
	// Default: $XDG_CONFIG_HOME/dittofs/controlplane.db
	Path string
}

// PostgresConfig contains PostgreSQL-specific configuration.
type PostgresConfig struct {
	Host         string
	Port         int
	Database     string
	User         string
	Password     string
	SSLMode      string // disable, require, verify-ca, verify-full
	SSLRootCert  string
	MaxOpenConns int
	MaxIdleConns int
}

// DSN returns the PostgreSQL connection string.
func (c *PostgresConfig) DSN() string {
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s",
		c.Host, c.Port, c.User, c.Password, c.Database)

	if c.SSLMode != "" {
		dsn += fmt.Sprintf(" sslmode=%s", c.SSLMode)
	}
	if c.SSLRootCert != "" {
		dsn += fmt.Sprintf(" sslrootcert=%s", c.SSLRootCert)
	}

	return dsn
}

// Config contains database configuration.
type Config struct {
	Type     DatabaseType
	SQLite   SQLiteConfig
	Postgres PostgresConfig
}

// ApplyDefaults fills in missing configuration with default values.
func (c *Config) ApplyDefaults() {
	if c.Type == "" {
		c.Type = DatabaseTypeSQLite
	}

	if c.Type == DatabaseTypeSQLite && c.SQLite.Path == "" {
		// Resolve default config directory based on platform
		var configDir string
		if runtime.GOOS == "windows" {
			// On Windows, use %APPDATA% (matching internal/cli/credentials/store.go pattern)
			configDir = os.Getenv("APPDATA")
			if configDir == "" {
				homeDir, _ := os.UserHomeDir()
				configDir = filepath.Join(homeDir, "AppData", "Roaming")
			}
		} else {
			// Unix: XDG_CONFIG_HOME or ~/.config
			configDir = os.Getenv("XDG_CONFIG_HOME")
			if configDir == "" {
				homeDir, _ := os.UserHomeDir()
				configDir = filepath.Join(homeDir, ".config")
			}
		}
		c.SQLite.Path = filepath.Join(configDir, "dittofs", "controlplane.db")
	}

	if c.Type == DatabaseTypePostgres {
		if c.Postgres.Port == 0 {
			c.Postgres.Port = 5432
		}
		if c.Postgres.SSLMode == "" {
			c.Postgres.SSLMode = "disable"
		}
		if c.Postgres.MaxOpenConns == 0 {
			c.Postgres.MaxOpenConns = 25
		}
		if c.Postgres.MaxIdleConns == 0 {
			c.Postgres.MaxIdleConns = 5
		}
	}
}

// Validate checks if the configuration is valid.
func (c *Config) Validate() error {
	switch c.Type {
	case DatabaseTypeSQLite:
		if c.SQLite.Path == "" {
			return fmt.Errorf("sqlite path is required")
		}
	case DatabaseTypePostgres:
		if c.Postgres.Host == "" {
			return fmt.Errorf("postgres host is required")
		}
		if c.Postgres.Database == "" {
			return fmt.Errorf("postgres database is required")
		}
		if c.Postgres.User == "" {
			return fmt.Errorf("postgres user is required")
		}
	default:
		return fmt.Errorf("unsupported database type: %s", c.Type)
	}
	return nil
}

// GORMStore implements the Store interface using GORM.
// It supports both SQLite and PostgreSQL backends via the same codebase.
type GORMStore struct {
	db     *gorm.DB
	config *Config
}

// New creates a new control plane store based on the configuration.
// It automatically creates the database schema via GORM AutoMigrate.
func New(config *Config) (*GORMStore, error) {
	if config == nil {
		config = &Config{}
	}

	// Apply defaults if not set
	config.ApplyDefaults()

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid database configuration: %w", err)
	}

	// Create the appropriate database connection
	var dialector gorm.Dialector
	switch config.Type {
	case DatabaseTypeSQLite:
		expanded, err := pathutil.ExpandPath(config.SQLite.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to expand database path %q: %w", config.SQLite.Path, err)
		}
		config.SQLite.Path = expanded
		// Ensure parent directory exists for SQLite
		if err := os.MkdirAll(filepath.Dir(config.SQLite.Path), 0755); err != nil {
			return nil, fmt.Errorf("failed to create database directory: %w", err)
		}
		// SQLite pragmas for better concurrent access:
		// - journal_mode(WAL): Write-Ahead Logging for concurrent readers/single writer
		// - busy_timeout(5000): Wait up to 5 seconds when database is locked
		dsn := config.SQLite.Path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
		dialector = sqlite.Open(dsn)

	case DatabaseTypePostgres:
		dialector = postgres.Open(config.Postgres.DSN())

	default:
		return nil, fmt.Errorf("unsupported database type: %s", config.Type)
	}

	// Configure GORM
	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // Suppress GORM logs by default
	}

	// Open database connection
	db, err := gorm.Open(dialector, gormConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// Configure connection pool for PostgreSQL
	if config.Type == DatabaseTypePostgres {
		sqlDB, err := db.DB()
		if err != nil {
			return nil, fmt.Errorf("failed to get underlying database: %w", err)
		}
		sqlDB.SetMaxOpenConns(config.Postgres.MaxOpenConns)
		sqlDB.SetMaxIdleConns(config.Postgres.MaxIdleConns)
	}

	// --- Pre-AutoMigrate migrations ---
	// Step 1: Migrate legacy payload_stores table to block_store_configs if needed.
	// This must happen before AutoMigrate so GORM sees the correct table name.
	preMigrator := db.Migrator()
	if preMigrator.HasTable("payload_stores") && !preMigrator.HasTable("block_store_configs") {
		if err := db.Exec("ALTER TABLE payload_stores RENAME TO block_store_configs").Error; err != nil {
			return nil, fmt.Errorf("failed to rename payload_stores table: %w", err)
		}
		if err := db.Exec("ALTER TABLE block_store_configs ADD COLUMN kind VARCHAR(10) NOT NULL DEFAULT 'remote'").Error; err != nil {
			return nil, fmt.Errorf("failed to add kind column: %w", err)
		}
		if err := db.Exec("UPDATE block_store_configs SET kind = 'remote'").Error; err != nil {
			return nil, fmt.Errorf("failed to set default kind: %w", err)
		}
	}

	// Pre-migration: drop legacy single-column unique index on block_store_configs.name.
	// AutoMigrate creates the new composite index idx_block_store_name_kind on (name, kind)
	// but does not drop pre-existing ones, which would prevent having a local and remote
	// store with the same name. Idempotent via IF EXISTS; safe to run on fresh installs.
	// Errors here (permissions, dialect mismatch) would silently re-introduce the original
	// 409 conflict on bootstrap, so surface them.
	if err := db.Exec("DROP INDEX IF EXISTS idx_payload_stores_name").Error; err != nil {
		return nil, fmt.Errorf("failed to drop legacy idx_payload_stores_name: %w", err)
	}
	if err := db.Exec("DROP INDEX IF EXISTS idx_block_store_configs_name").Error; err != nil {
		return nil, fmt.Errorf("failed to drop legacy idx_block_store_configs_name: %w", err)
	}

	// Pre-migration: drop legacy single-column unique index on identity_mappings.principal.
	// The new composite index idx_provider_principal (provider_name, principal) replaces it.
	if db.Migrator().HasTable(&models.IdentityMapping{}) {
		_ = db.Exec("DROP INDEX IF EXISTS idx_identity_mappings_principal")
	}

	// Pre-migration: rename read_cache_size column to read_buffer_size if it exists.
	if db.Migrator().HasColumn(&models.Share{}, "read_cache_size") {
		if err := db.Migrator().RenameColumn(&models.Share{}, "read_cache_size", "read_buffer_size"); err != nil {
			return nil, fmt.Errorf("failed to rename read_cache_size column: %w", err)
		}
	}

	// Pre-migration (D-26 step 1): rename backup_repos.metadata_store_id → target_id.
	// AutoMigrate does not handle renames; mirrors the Share.read_cache_size pattern
	// above. Idempotent via HasColumn guard so re-running on an already-migrated DB
	// is a no-op. Errors here abort boot rather than silently leaving the schema in
	// a half-migrated state (T-04-01-02).
	if db.Migrator().HasColumn(&models.BackupRepo{}, "metadata_store_id") {
		if err := db.Migrator().RenameColumn(&models.BackupRepo{}, "metadata_store_id", "target_id"); err != nil {
			return nil, fmt.Errorf("failed to rename backup_repos.metadata_store_id: %w", err)
		}
	}

	// Run auto-migration
	if err := db.AutoMigrate(models.AllModels()...); err != nil {
		return nil, fmt.Errorf("failed to run database migration: %w", err)
	}

	// Post-migration (D-26 step 3): backfill target_kind for rows that existed
	// before the column was added. Some dialects (SQLite ADD COLUMN with default)
	// leave NULL on legacy rows — mirrors the portmapper_port backfill below.
	// Scoped to '' OR NULL so subsequent boots do not rewrite operator-set values
	// (T-04-01-01).
	if err := db.Exec(
		"UPDATE backup_repos SET target_kind = ? WHERE target_kind = '' OR target_kind IS NULL",
		"metadata",
	).Error; err != nil {
		return nil, fmt.Errorf("failed to backfill backup_repos.target_kind: %w", err)
	}

	// Post-migration (D-25): backfill shares.enabled for rows that predate
	// the column. Mirrors the Phase-4 backup_repos.target_kind backfill
	// above. SQLite dialects may leave NULL on ALTER TABLE ADD COLUMN even
	// with DEFAULT; explicit backfill keeps the invariant "every share has
	// a non-NULL enabled value" across both SQLite and PostgreSQL.
	if err := db.Exec(
		"UPDATE shares SET enabled = ? WHERE enabled IS NULL",
		true,
	).Error; err != nil {
		return nil, fmt.Errorf("failed to backfill shares.enabled: %w", err)
	}

	// --- Post-AutoMigrate migrations ---
	// Step 2: Migrate legacy Share payload_store_id column to local/remote block store IDs.
	postMigrator := db.Migrator()
	if postMigrator.HasColumn(&models.Share{}, "payload_store_id") {
		// Create default-local block store for existing shares
		defaultLocalID := uuid.New().String()
		if err := db.Exec(
			"INSERT INTO block_store_configs (id, name, kind, type, config, created_at) VALUES (?, 'default-local', 'local', 'fs', '{}', ?)",
			defaultLocalID, time.Now(),
		).Error; err != nil {
			return nil, fmt.Errorf("failed to create default-local block store: %w", err)
		}
		// Populate new columns from legacy payload_store_id column
		if err := db.Exec("UPDATE shares SET local_block_store_id = ?", defaultLocalID).Error; err != nil {
			return nil, fmt.Errorf("failed to populate local_block_store_id: %w", err)
		}
		if err := db.Exec("UPDATE shares SET remote_block_store_id = payload_store_id WHERE payload_store_id IS NOT NULL AND payload_store_id != ''").Error; err != nil {
			return nil, fmt.Errorf("failed to populate remote_block_store_id: %w", err)
		}
		// Drop old column
		if err := postMigrator.DropColumn(&models.Share{}, "payload_store_id"); err != nil {
			return nil, fmt.Errorf("failed to drop payload_store_id column: %w", err)
		}
	}

	// Post-migration: drop protocol-specific columns from shares table.
	// These have been moved to share_adapter_configs.
	migrator := db.Migrator()
	for _, col := range []string{"squash", "anonymous_uid", "anonymous_gid", "allow_auth_sys", "require_kerberos", "min_kerberos_level", "netgroup_id", "disable_readdirplus"} {
		if migrator.HasColumn(&models.Share{}, col) {
			_ = migrator.DropColumn(&models.Share{}, col)
		}
	}
	for _, col := range []string{"guest_enabled", "guest_uid", "guest_gid"} {
		if migrator.HasColumn(&models.Share{}, col) {
			_ = migrator.DropColumn(&models.Share{}, col)
		}
	}

	store := &GORMStore{
		db:     db,
		config: config,
	}

	// Post-migration: populate default settings for existing adapters
	if err := store.EnsureAdapterSettings(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ensure adapter settings: %w", err)
	}

	// Post-migration: fix portmapper_port for existing NFS adapter settings.
	// ALTER TABLE ADD COLUMN sets int to 0, not the default 10111.
	if err := db.Exec(
		"UPDATE nfs_adapter_settings SET portmapper_port = ? WHERE portmapper_port = ?",
		10111, 0,
	).Error; err != nil {
		return nil, fmt.Errorf("failed to apply portmapper defaults: %w", err)
	}

	return store, nil
}

// DB returns the underlying GORM database connection.
// This is useful for advanced queries or testing.
func (s *GORMStore) DB() *gorm.DB {
	return s.db
}

// isUniqueConstraintError checks if the error is a unique constraint violation.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// SQLite or PostgreSQL unique constraint errors
	return strings.Contains(errStr, "UNIQUE constraint failed") ||
		strings.Contains(errStr, "duplicate key value violates unique constraint")
}

// convertNotFoundError converts gorm.ErrRecordNotFound to the appropriate domain error.
func convertNotFoundError(err error, notFoundErr error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return notFoundErr
	}
	return err
}
