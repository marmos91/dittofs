package badger

import (
	"context"
	"fmt"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck verifies the BadgerDB-backed store is operational and
// returns a structured [health.Report].
//
// The probe attempts a no-op read transaction (db.View) which forces
// BadgerDB to verify the database handle is open and the underlying
// LSM tree is accessible. This is the cheapest possible probe that
// still catches "DB closed" and "DB corrupted" failure modes.
//
// Returns [health.StatusUnhealthy] when the context is canceled or the
// View call returns an error; otherwise [health.StatusHealthy] with
// the measured probe latency.
//
// Thread-safe; designed to be called concurrently from /status routes
// behind a [health.CachedChecker].
func (s *BadgerMetadataStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return health.Report{
			Status:    health.StatusUnhealthy,
			Message:   err.Error(),
			CheckedAt: time.Now().UTC(),
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}

	// A no-op View transaction is enough to verify the DB is open.
	// BadgerDB returns an error if the handle is closed or the storage
	// engine has reported corruption.
	if err := s.db.View(func(txn *badgerdb.Txn) error { return nil }); err != nil {
		return health.Report{
			Status:    health.StatusUnhealthy,
			Message:   fmt.Sprintf("badger view: %v", err),
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
