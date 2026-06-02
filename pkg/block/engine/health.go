package engine

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/health"
)

// HealthCheck verifies the store is operational by checking the syncer health
// (which in turn checks the remote store).
//
// Deprecated: use [Store.Healthcheck] (lowercase 'c'), which returns a
// structured [health.Report] derived from both the local and remote stores
// and satisfies [health.Checker]. This method only collapses the structured
// state into a single error and is retained for backward compatibility.
func (bs *Store) HealthCheck(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	return bs.syncer.HealthCheck(ctx)
}

// Healthcheck returns the engine's overall health, computed as the
// worst-of of its underlying local and remote stores. The result
// satisfies [health.Checker] so the API layer can wrap the engine in
// a [health.CachedChecker] for /status routes.
//
// Derivation rules (worst-of)
//
//   - If the local store reports unhealthy → engine is unhealthy
//     (we can't even serve cached blocks).
//   - If a remote store is configured and reports unhealthy → engine
//     is degraded (local reads still work, but new uploads will queue
//     and the system is operating in offline-write mode).
//   - Otherwise → healthy.
//
// The combined message preserves the worst-status component's message
// so operators can see exactly which subsystem is at fault.
func (bs *Store) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	// Pin against Close teardown. This method has no error return, so a
	// closed store reports unhealthy rather than racing the local/remote
	// teardown that Close performs under closeMu.Lock.
	bs.closeMu.RLock()
	defer bs.closeMu.RUnlock()
	if bs.closed {
		return health.NewUnhealthyReport(ErrStoreClosed.Error(), time.Since(start))
	}

	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	localRep := bs.local.Healthcheck(ctx)
	if localRep.Status == health.StatusUnhealthy {
		return health.NewUnhealthyReport("local: "+localRep.Message, time.Since(start))
	}

	if bs.remote != nil {
		remoteRep := bs.remote.Healthcheck(ctx)
		if remoteRep.Status == health.StatusUnhealthy {
			// Local works, remote is unreachable: degraded — reads
			// still served from local cache, writes will queue.
			return health.Report{
				Status:    health.StatusDegraded,
				Message:   "remote unreachable: " + remoteRep.Message,
				CheckedAt: time.Now().UTC(),
				LatencyMs: time.Since(start).Milliseconds(),
			}
		}
	}

	return health.NewHealthyReport(time.Since(start))
}

// HasRemoteStore returns true if this Store has a remote store configured.
func (bs *Store) HasRemoteStore() bool {
	return bs.remote != nil
}

// SetRetentionPolicy updates the retention policy on the underlying local store.
// Delegates to the local store's SetRetentionPolicy method.
func (bs *Store) SetRetentionPolicy(policy block.RetentionPolicy, ttl time.Duration) {
	bs.local.SetRetentionPolicy(policy, ttl)
}

// SetEvictionEnabled controls whether the local store can evict blocks to free disk space.
// Delegates to the local store's SetEvictionEnabled method.
func (bs *Store) SetEvictionEnabled(enabled bool) {
	bs.local.SetEvictionEnabled(enabled)
}
