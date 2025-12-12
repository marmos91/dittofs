package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// createConnectionPool creates a new PostgreSQL connection pool with the given configuration
func createConnectionPool(ctx context.Context, cfg *PostgresMetadataStoreConfig, logger *slog.Logger) (*pgxpool.Pool, error) {
	// Apply defaults before validation
	cfg.ApplyDefaults()

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Build pgxpool config
	poolConfig, err := pgxpool.ParseConfig(cfg.ConnectionString())
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	// Apply connection pool settings
	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolConfig.HealthCheckPeriod = cfg.HealthCheckPeriod

	// Set query timeout as statement timeout
	if cfg.QueryTimeout > 0 {
		poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%dms", cfg.QueryTimeout.Milliseconds())
	}

	// Configure logging (optional, can be adjusted)
	// pgxpool uses its own logging, but we can configure it to use our logger
	// For now, we'll keep it simple and let pgx use default logging

	// Create the connection pool
	logger.Info("Creating PostgreSQL connection pool",
		"host", cfg.Host,
		"port", cfg.Port,
		"database", cfg.Database,
		"user", cfg.User,
		"max_conns", cfg.MaxConns,
		"min_conns", cfg.MinConns,
		"ssl_mode", cfg.SSLMode,
	)

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Test the connection
	logger.Info("Testing PostgreSQL connection...")
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping PostgreSQL: %w", err)
	}

	logger.Info("PostgreSQL connection pool created successfully")

	return pool, nil
}

// closeConnectionPool closes the PostgreSQL connection pool gracefully
func closeConnectionPool(pool *pgxpool.Pool, logger *slog.Logger) {
	if pool == nil {
		return
	}

	logger.Info("Closing PostgreSQL connection pool...")
	pool.Close()
	logger.Info("PostgreSQL connection pool closed")
}
