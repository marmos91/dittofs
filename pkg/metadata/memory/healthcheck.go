package memory

import (
	"context"
)

// Healthcheck verifies the repository is operational.
//
// For the in-memory implementation, this is a simple check that always succeeds
// (unless the context is cancelled). Since there are no external dependencies
// like databases, network storage, or other services, there's nothing that can
// be "unhealthy" in the traditional sense.
//
// Thread Safety:
// This method does not acquire any locks as it only checks context status.
// It's designed to be extremely fast and non-blocking.
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - error: Returns nil if healthy, context error if cancelled/timed out
func (store *MemoryMetadataStore) Healthcheck(ctx context.Context) error {
	// For in-memory store, just check if context is valid
	// This ensures we respect timeouts and cancellation
	return ctx.Err()
}
