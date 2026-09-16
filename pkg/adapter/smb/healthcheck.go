package smb

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck overrides [adapter.BaseAdapter.Healthcheck] to layer the
// SMB-specific configured-on flag on top of the base derivation:
//
//   - If config.Enabled is false → [health.StatusDisabled]. The adapter
//     was deliberately turned off; nothing to probe.
//   - Otherwise → delegate to [adapter.BaseAdapter.Healthcheck], which
//     handles the started / shutdown / running cases.
//
// No error instrumentation is tracked yet, so a running SMB adapter
// always reports healthy. Healthcheck could return
// [health.StatusDegraded] if recent decrypt failures, signing
// verification failures, or session-setup rejections were counted
// against a per-window threshold — the building blocks
// (e.g. the per-connection DecryptFailures atomic counter) are already
// in place for that follow-up.
func (a *Adapter) Healthcheck(ctx context.Context) health.Report {
	if !a.config.Enabled {
		return health.Report{
			Status:    health.StatusDisabled,
			Message:   "SMB adapter is disabled in config",
			CheckedAt: time.Now().UTC(),
		}
	}
	return a.BaseAdapter.Healthcheck(ctx)
}
