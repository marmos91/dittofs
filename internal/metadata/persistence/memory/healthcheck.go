package memory

import "context"

// Ping performs a health check on the in-memory repository.
// For the in-memory implementation, this always returns nil since there
// are no external dependencies to check. The repository is considered
// healthy as long as it's initialized.
//
// In a production implementation with external storage (database, object store),
// this method would:
//   - Verify database connectivity
//   - Check storage backend availability
//   - Validate resource availability (memory, disk space)
//   - Test critical subsystems
//
// The NULL procedure uses this for optional health monitoring while maintaining
// RFC 1813 compliance (NULL always succeeds even if Ping fails).
//
// Returns:
//   - error: Always returns nil for in-memory implementation
func (r *MemoryRepository) Healthcheck(ctx context.Context) error {
	// In-memory repository has no external dependencies
	// Always healthy if the repository object exists
	return nil
}
