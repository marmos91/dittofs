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
// No error instrumentation is tracked yet, so a running NFS adapter
// always reports healthy. Healthcheck could return
// [health.StatusDegraded] if recent lease breaches, NLM lock timeouts,
// or RPC dispatch errors were counted against a per-window threshold.
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
