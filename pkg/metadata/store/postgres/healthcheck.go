package postgres

import (
	"context"
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
// Returns [health.StatusUnknown] when the caller's context is canceled
// (the probe was indeterminate, not the store), [health.StatusUnhealthy]
// when the ping itself returns an error, or [health.StatusHealthy] with
// the measured probe latency on success.
//
// Thread-safe; designed to be called concurrently from /status routes
// behind a [health.CachedChecker].
func (s *PostgresMetadataStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	if err := s.pool.Ping(ctx); err != nil {
		return health.NewUnhealthyReport("postgres ping: "+err.Error(), time.Since(start))
	}

	return health.NewHealthyReport(time.Since(start))
}
