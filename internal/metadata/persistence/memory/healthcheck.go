package memory

import "context"

// Healthcheck performs a health check on the in-memory repository.
//
// For the in-memory implementation, this always returns nil since there
// are no external dependencies to check. The repository is considered
// healthy as long as it's initialized.
//
// Context Cancellation:
// This operation checks the context immediately to respect cancellation.
// In a production implementation with external dependencies, context would
// be passed to all subsystem checks with appropriate timeouts.
//
// In a production implementation with external storage (database, object store),
// this method would:
//   - Verify database connectivity (with ctx timeout)
//   - Check storage backend availability (with ctx timeout)
//   - Validate resource availability (memory, disk space)
//   - Test critical subsystems (each respecting ctx cancellation)
//
// The NULL procedure uses this for optional health monitoring while maintaining
// RFC 1813 compliance (NULL always succeeds even if Healthcheck fails).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - error: Always returns nil for in-memory implementation, or
//     context.Canceled/context.DeadlineExceeded if context is cancelled
func (r *MemoryRepository) Healthcheck(ctx context.Context) error {
	// ========================================================================
	// Step 1: Check context cancellation
	// ========================================================================
	// Even though this is a no-op for in-memory, we should respect cancellation
	// for consistency with the rest of the codebase

	if err := ctx.Err(); err != nil {
		return err
	}

	// ========================================================================
	// Step 2: Perform health checks
	// ========================================================================
	// In-memory repository has no external dependencies
	// Always healthy if the repository object exists
	//
	// Production implementation would check:
	// - if err := r.checkDatabaseConnection(ctx); err != nil { return err }
	// - if err := r.checkStorageBackend(ctx); err != nil { return err }
	// - if err := r.checkResourceAvailability(ctx); err != nil { return err }

	return nil
}
