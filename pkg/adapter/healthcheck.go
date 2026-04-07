package adapter

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck is the default implementation for the [Adapter] interface.
// Concrete adapters typically embed [BaseAdapter] and override Healthcheck
// to layer protocol-specific concerns (such as the configured-on flag) on
// top of this baseline.
//
// Derivation rules using only signals BaseAdapter already tracks:
//
//   - [health.StatusUnknown] when Serve() has not yet been called and the
//     listener has therefore not been bound. The adapter exists in the
//     runtime registry but hasn't had a chance to start.
//   - [health.StatusUnhealthy] when shutdown has been initiated but
//     Serve() has not yet returned (the adapter is mid-shutdown), or
//     when the listener has been torn down out of band.
//   - [health.StatusHealthy] otherwise — the adapter has bound its
//     listener and is not currently shutting down.
//
// This default does not return [health.StatusDegraded] or
// [health.StatusDisabled]. Concrete adapters that track recent errors
// or have an enabled flag should override and add those branches before
// delegating to BaseAdapter.Healthcheck.
//
// The method is intentionally cheap — it inspects atomic flags and
// channel state, never reaches network or disk. The API layer wraps
// it with a [health.CachedChecker] anyway.
func (b *BaseAdapter) Healthcheck(_ context.Context) health.Report {
	now := time.Now().UTC()

	if !b.started.Load() {
		return health.Report{
			Status:    health.StatusUnknown,
			Message:   b.protocolName + " adapter has not started yet",
			CheckedAt: now,
		}
	}

	// Once started, peek at the shutdown channel: if it's closed the
	// adapter is mid-stop and not currently servicing connections.
	select {
	case <-b.Shutdown:
		return health.Report{
			Status:    health.StatusUnhealthy,
			Message:   b.protocolName + " adapter is shutting down",
			CheckedAt: now,
		}
	default:
	}

	// Listener was bound and shutdown hasn't been initiated. Healthy.
	return health.Report{
		Status:    health.StatusHealthy,
		CheckedAt: now,
	}
}
