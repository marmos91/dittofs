package nfs

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck overrides [adapter.BaseAdapter.Healthcheck] to layer the
// NFS-specific configured-on flag on top of the base derivation:
//
//   - If config.Enabled is false → [health.StatusDisabled]. The adapter
//     was deliberately turned off; nothing to probe.
//   - Otherwise → delegate to [adapter.BaseAdapter.Healthcheck], which
//     handles the started / shutdown / running cases.
//
// Phase U-C does not introduce new error instrumentation, so a running
// NFS adapter currently always reports healthy. A future phase can
// upgrade this method to return [health.StatusDegraded] when recent
// lease breaches, NLM lock timeouts, or RPC dispatch errors cross a
// per-window threshold.
func (a *NFSAdapter) Healthcheck(ctx context.Context) health.Report {
	if !a.config.Enabled {
		return health.Report{
			Status:    health.StatusDisabled,
			Message:   "NFS adapter is disabled in config",
			CheckedAt: time.Now().UTC(),
		}
	}
	return a.BaseAdapter.Healthcheck(ctx)
}
