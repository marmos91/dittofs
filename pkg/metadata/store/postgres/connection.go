package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// createConnectionPool creates a new PostgreSQL connection pool with the given configuration
func createConnectionPool(ctx context.Context, cfg *PostgresMetadataStoreConfig, logger *slog.Logger) (*pgxpool.Pool, error) {
	cfg.ApplyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	poolConfig, err := pgxpool.ParseConfig(cfg.ConnectionString())
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolConfig.HealthCheckPeriod = cfg.HealthCheckPeriod

	// Set query timeout as statement timeout
	if cfg.QueryTimeout > 0 {
		poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%dms", cfg.QueryTimeout.Milliseconds())
	}

	// By default pgx keeps a per-connection prepared-statement cache, which
	// names statements on the server. A pooler in transaction-pooling mode hands
	// a different backend to each transaction, so those names resolve on the
	// wrong session and queries fail with "prepared statement does not exist".
	// Disabling the cache switches to unnamed extended-protocol statements,
	// which carry no server-side name while still binding parameters
	// server-side (unlike the simple protocol, which interpolates them
	// client-side).
	if cfg.DisablePreparedStatements {
		poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}

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
