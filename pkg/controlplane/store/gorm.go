package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

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
		// Use XDG config home or fallback
		configDir := os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			homeDir, _ := os.UserHomeDir()
			configDir = filepath.Join(homeDir, ".config")
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

	// Run auto-migration
	if err := db.AutoMigrate(models.AllModels()...); err != nil {
		return nil, fmt.Errorf("failed to run database migration: %w", err)
	}

	store := &GORMStore{
		db:     db,
		config: config,
	}

	// Post-migration: populate default settings for existing adapters
	if err := store.EnsureAdapterSettings(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ensure adapter settings: %w", err)
	}

	// Post-migration: set correct defaults for existing shares that got zero-values
	// from column addition. GORM's default tag only applies on INSERT, not ALTER TABLE.
	// Existing shares would get allow_auth_sys=false (zero value) instead of true.
	// Only update shares where all security fields are zero-valued (untouched by migration).
	if err := db.Exec(
		"UPDATE shares SET allow_auth_sys = ? WHERE allow_auth_sys = ? AND require_kerberos = ? AND blocked_operations IS NULL",
		true, false, false,
	).Error; err != nil {
		return nil, fmt.Errorf("failed to apply post-migration defaults: %w", err)
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
