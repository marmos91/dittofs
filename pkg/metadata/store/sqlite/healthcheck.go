package sqlite

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck verifies the database is operational and returns a
// structured [health.Report].
//
// The probe pings the database, which runs a trivial round-trip query.
// This catches a closed pool or an unreadable/locked database file —
// the failure modes a /status route operator would care about.
//
// Returns [health.StatusUnknown] when the caller's context is canceled
// (the probe was indeterminate, not the store), [health.StatusUnhealthy]
// when the ping itself returns an error, or [health.StatusHealthy] with
// the measured probe latency on success.
//
// Thread-safe; designed to be called concurrently from /status routes
// behind a [health.CachedChecker].
func (s *SQLiteMetadataStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	if err := s.db.PingContext(ctx); err != nil {
		return health.NewUnhealthyReport("sqlite ping: "+err.Error(), time.Since(start))
	}

	return health.NewHealthyReport(time.Since(start))
}
