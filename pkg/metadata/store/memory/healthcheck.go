package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck verifies the in-memory store is operational and returns a
// structured [health.Report].
//
// The in-memory implementation has no external dependencies — there is
// nothing that can be unhealthy in the traditional sense. The only
// failure mode is the caller's context being canceled or timed out,
// which surfaces as [health.StatusUnknown] (the probe was indeterminate)
// rather than [health.StatusUnhealthy] (which would falsely suggest the
// store itself is broken).
//
// This method does not acquire any locks; it is designed to be
// non-blocking so the cache wrapper can call it from /status routes
// without contention.
func (store *MemoryMetadataStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}
	return health.NewHealthyReport(time.Since(start))
}
