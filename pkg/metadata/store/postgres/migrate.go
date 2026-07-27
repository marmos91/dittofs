package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for database/sql

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres/migrations"
)

// runMigrations executes database migrations using golang-migrate
// Uses advisory locks to ensure only one instance runs migrations at a time
func runMigrations(ctx context.Context, connString string, logger *slog.Logger) error {
	logger.Info("Running database migrations...")

	// Open database connection using database/sql (required by golang-migrate)
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("failed to open database connection: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Test the connection
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	// Create postgres driver instance for migrations
	driver, err := postgres.WithInstance(db, &postgres.Config{
		MigrationsTable: "schema_migrations",
		DatabaseName:    "dittofs",
	})
	if err != nil {
		return fmt.Errorf("failed to create postgres driver: %w", err)
	}

	// Create source driver from embedded filesystem
	sourceDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("failed to create source driver: %w", err)
	}

	// Create migrate instance
	m, err := migrate.NewWithInstance(
		"iofs",
		sourceDriver,
		"postgres",
		driver,
	)
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}

	// Run migrations
	// golang-migrate uses PostgreSQL advisory locks automatically to prevent
	// concurrent migrations from multiple instances
	logger.Info("Applying migrations...")
	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migration failed: %w", err)
	}

	if err == migrate.ErrNoChange {
		logger.Info("No migrations to apply (database is up to date)")
	} else {
		logger.Info("Migrations completed successfully")
	}

	// Get current version
	version, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return fmt.Errorf("failed to get migration version: %w", err)
	}

	if err == migrate.ErrNilVersion {
		logger.Info("No migrations applied yet")
	} else {
		logger.Info("Current schema version",
			"version", version,
			"dirty", dirty,
		)

		if dirty {
			logger.Warn("Database schema is in dirty state - manual intervention may be required")
		}
	}

	return nil
}

// newestEmbeddedVersion returns the highest version among the embedded
// `NNNNNN_*.up.sql` migrations — the newest schema this build ships.
func newestEmbeddedVersion() (int64, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return 0, fmt.Errorf("read migrations dir: %w", err)
	}
	var newest int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		idx := strings.IndexByte(name, '_')
		if idx <= 0 {
			continue
		}
		v, perr := strconv.ParseInt(name[:idx], 10, 64)
		if perr != nil {
			continue
		}
		if v > newest {
			newest = v
		}
	}
	return newest, nil
}

// checkFormatVersion refuses to open a database migrated past what this build
// knows how to run against.
//
// schema_migrations already records every applied version, so the newest
// applied row versus the newest embedded migration is the whole comparison — no
// second stamp is needed. A row above that ceiling means a newer release
// migrated this database and may have dropped or renamed columns this build's
// queries still name. golang-migrate only surfaces that halfway — m.Up() fails
// with "file does not exist", and only when AutoMigrate is on, which it is not
// by default — so the database otherwise opens cleanly and then fails query by
// query with raw SQL errors, long after startup.
func checkFormatVersion(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('schema_migrations') IS NOT NULL`,
	).Scan(&exists); err != nil {
		return fmt.Errorf("probe schema_migrations: %w", err)
	}
	if !exists {
		// A database no migration has ever touched; golang-migrate creates the
		// table and brings it to the embedded ceiling.
		return nil
	}

	var stored *int64
	if err := pool.QueryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&stored); err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	if stored == nil {
		return nil
	}

	newest, err := newestEmbeddedVersion()
	if err != nil {
		return err
	}
	if newest == 0 {
		return nil
	}

	if *stored > newest {
		return fmt.Errorf("%w: metadata database is at schema version %d, this build reads up to %d",
			block.ErrFutureFormat, *stored, newest)
	}
	return nil
}

// RunMigrations is a public wrapper for manual migration execution (e.g., from CLI)
func RunMigrations(ctx context.Context, cfg *PostgresMetadataStoreConfig) error {
	// Apply defaults and validate
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Create logger for migration using internal logger
	log := logger.With("component", "postgres_migration")

	// Run migrations
	return runMigrations(ctx, cfg.ConnectionString(), log)
}
