package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck reports the in-memory local store's status. The only
// failure modes a pure in-memory store can have are: the store has been
// closed (which means it can no longer accept reads or writes), or the
// caller's context is canceled (which surfaces as
// [health.StatusUnknown] — the probe was indeterminate, not the store).
//
// Cheap, lock-protected, safe for concurrent calls.
func (s *MemoryStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()

	if closed {
		return health.NewUnhealthyReport("memory block store is closed", time.Since(start))
	}

	return health.NewHealthyReport(time.Since(start))
}
