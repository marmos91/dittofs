package badger

import (
	"context"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"
)

// Healthcheck verifies the repository is operational.
//
// This performs a health check to ensure BadgerDB is accessible and can serve
// requests. The check:
//   - Attempts to start a read transaction
//   - Verifies the database is not closed
//   - Checks context cancellation
//
// For BadgerDB, this is a lightweight operation that doesn't perform extensive
// checks, as BadgerDB handles most error conditions internally.
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
func (s *BadgerMetadataStore) Healthcheck(ctx context.Context) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	// Attempt a simple read transaction to verify database is accessible
	err := s.db.View(func(txn *badgerdb.Txn) error {
		// Just verify we can start a transaction
		// BadgerDB will return an error if it's closed or corrupted
		return nil
	})

	if err != nil {
		return fmt.Errorf("healthcheck failed: %w", err)
	}

	return nil
}
