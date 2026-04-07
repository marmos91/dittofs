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
// Derivation rules using only the signals BaseAdapter currently tracks
// (started flag + Shutdown channel):
//
//   - [health.StatusUnknown] when the caller's context is canceled or
//     deadlined out (the probe was indeterminate, not the adapter), or
//     when the started flag is still false. The latter covers the
//     pre-Serve startup window AND a hypothetical "Serve called but
//     net.Listen failed" case — BaseAdapter has no way to distinguish
//     them today, so both surface as Unknown rather than Unhealthy.
//     A follow-up phase can add a failed-start state and lift that
//     limitation; see the [Adapter.Healthcheck] interface doc.
//   - [health.StatusUnhealthy] when shutdown has been initiated but
//     Serve() has not yet returned (the adapter is mid-shutdown).
//   - [health.StatusHealthy] otherwise — the adapter has started and
//     is not currently shutting down.
//
// This default does not return [health.StatusDegraded] or
// [health.StatusDisabled]. Concrete adapters that track recent errors
// or have an enabled flag should override and add those branches before
// delegating to BaseAdapter.Healthcheck.
//
// The method is intentionally cheap — it inspects atomic flags and
// channel state, never reaches network or disk. The API layer wraps
// it with a [health.CachedChecker] anyway.
func (b *BaseAdapter) Healthcheck(ctx context.Context) health.Report {
	now := time.Now().UTC()

	if err := ctx.Err(); err != nil {
		return health.Report{
			Status:    health.StatusUnknown,
			Message:   err.Error(),
			CheckedAt: now,
		}
	}

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
