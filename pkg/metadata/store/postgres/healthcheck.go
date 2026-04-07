package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck verifies the PostgreSQL connection pool is operational
// and returns a structured [health.Report].
//
// The probe pings the pool, which acquires a connection, runs a
// trivial round-trip query, and releases it back. This catches a
// closed/exhausted pool, broken network paths, or a server that has
// stopped accepting new connections — all the failure modes a
// /status route operator would care about.
//
// Returns [health.StatusUnhealthy] when the context is canceled or
// the ping returns an error; otherwise [health.StatusHealthy] with
// the measured probe latency.
//
// Thread-safe; designed to be called concurrently from /status routes
// behind a [health.CachedChecker].
func (s *PostgresMetadataStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return health.Report{
			Status:    health.StatusUnhealthy,
			Message:   err.Error(),
			CheckedAt: time.Now().UTC(),
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}

	if err := s.pool.Ping(ctx); err != nil {
		return health.Report{
			Status:    health.StatusUnhealthy,
			Message:   fmt.Sprintf("postgres ping: %v", err),
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
