package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck reports the in-memory local store's status. The only
// failure modes a pure in-memory store can have are: the caller's
// context is canceled, or the store has been closed (which means it
// can no longer accept reads or writes).
//
// Cheap, lock-protected, safe for concurrent calls.
func (s *MemoryStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	if err := ctx.Err(); err != nil {
		return health.Report{
			Status:    health.StatusUnhealthy,
			Message:   err.Error(),
			CheckedAt: time.Now().UTC(),
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}

	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()

	if closed {
		return health.Report{
			Status:    health.StatusUnhealthy,
			Message:   "memory block store is closed",
			CheckedAt: time.Now().UTC(),
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}

	return health.Report{
		Status:    health.StatusHealthy,
		CheckedAt: time.Now().UTC(),
		LatencyMs: time.Since(start).Milliseconds(),
	}
}
