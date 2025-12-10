package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for database/sql

	"github.com/marmos91/dittofs/pkg/store/metadata/postgres/migrations"
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
	defer db.Close()

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

// RunMigrations is a public wrapper for manual migration execution (e.g., from CLI)
func RunMigrations(ctx context.Context, cfg *PostgresMetadataStoreConfig) error {
	// Apply defaults and validate
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Create logger for migration
	logger := slog.Default()

	// Run migrations
	return runMigrations(ctx, cfg.ConnectionString(), logger)
}

// getMigrationVersion returns the current migration version
func getMigrationVersion(connString string) (uint, bool, error) {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return 0, false, fmt.Errorf("failed to open database connection: %w", err)
	}
	defer db.Close()

	driver, err := postgres.WithInstance(db, &postgres.Config{
		MigrationsTable: "schema_migrations",
	})
	if err != nil {
		return 0, false, fmt.Errorf("failed to create postgres driver: %w", err)
	}

	sourceDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return 0, false, fmt.Errorf("failed to create source driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", sourceDriver, "postgres", driver)
	if err != nil {
		return 0, false, fmt.Errorf("failed to create migrate instance: %w", err)
	}

	version, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return 0, false, err
	}

	if err == migrate.ErrNilVersion {
		return 0, false, nil
	}

	return version, dirty, nil
}
