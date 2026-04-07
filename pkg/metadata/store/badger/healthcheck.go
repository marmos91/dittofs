package badger

import (
	"context"
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
// Returns [health.StatusUnknown] when the caller's context is canceled
// (the probe was indeterminate, not the store), [health.StatusUnhealthy]
// when the View call itself returns an error, or [health.StatusHealthy]
// with the measured probe latency on success.
//
// Thread-safe; designed to be called concurrently from /status routes
// behind a [health.CachedChecker].
func (s *BadgerMetadataStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	// A no-op View transaction is enough to verify the DB is open.
	// BadgerDB returns an error if the handle is closed or the storage
	// engine has reported corruption.
	if err := s.db.View(func(txn *badgerdb.Txn) error { return nil }); err != nil {
		return health.NewUnhealthyReport("badger view: "+err.Error(), time.Since(start))
	}

	return health.NewHealthyReport(time.Since(start))
}
