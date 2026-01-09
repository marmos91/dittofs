package postgres

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Healthcheck verifies the PostgreSQL connection is healthy.
//
// This performs a health check to ensure PostgreSQL is accessible and can serve
// requests. The check:
//   - Attempts to ping the connection pool
//   - Verifies the database connection is alive
//   - Checks context cancellation
//
// For PostgreSQL, this uses the connection pool's Ping method which:
//   - Acquires a connection from the pool
//   - Executes a simple query to verify the connection
//   - Returns the connection to the pool
//
// Use Cases:
//   - Liveness probes in container orchestration
//   - Load balancer health checks
//   - Monitoring and alerting systems
//   - Protocol NULL/ping procedures
//
// Thread Safety: Safe for concurrent use.
//
// Returns:
//   - error: Returns error if repository is unhealthy, nil if healthy
func (s *PostgresMetadataStore) Healthcheck(ctx context.Context) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	// Simple ping to verify connection pool is healthy
	if err := s.pool.Ping(ctx); err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "PostgreSQL health check failed",
		}
	}

	return nil
}
