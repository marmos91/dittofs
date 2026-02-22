package nfs

import (
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	v4attrs "github.com/marmos91/dittofs/internal/protocol/nfs/v4/attrs"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// applyNFSSettings reads current NFS adapter settings from the runtime's
// SettingsWatcher and applies them to the StateManager, v4Handler, and
// attrs package. Called during SetRuntime (startup) and can be called
// periodically or on new connections to pick up changed settings.
func (s *NFSAdapter) applyNFSSettings(rt *runtime.Runtime) {
	settings := rt.GetNFSSettings()
	if settings == nil {
		logger.Debug("NFS adapter: no live settings available, using defaults")
		return
	}

	// Lease time -> StateManager + attrs package (FATTR4_LEASE_TIME)
	if settings.LeaseTime > 0 {
		leaseSeconds := settings.LeaseTime
		s.v4Handler.StateManager.SetLeaseTime(time.Duration(leaseSeconds) * time.Second)
		v4attrs.SetLeaseTime(uint32(leaseSeconds))
		logger.Debug("NFS adapter: applied lease time from settings",
			"lease_time_seconds", leaseSeconds)
	}

	// Grace period -> StateManager
	if settings.GracePeriod > 0 {
		s.v4Handler.StateManager.SetGracePeriodDuration(
			time.Duration(settings.GracePeriod) * time.Second,
		)
	}

	// Delegation policy
	s.v4Handler.StateManager.SetDelegationsEnabled(settings.DelegationsEnabled)

	// Max delegations -> StateManager
	s.v4Handler.StateManager.SetMaxDelegations(settings.MaxDelegations)

	// Directory delegation batch window -> StateManager.
	// Only propagate positive values; 0 means "use default" (50ms fallback
	// in resetBatchTimer), negative values are invalid and ignored.
	if settings.DirDelegBatchWindowMs > 0 {
		s.v4Handler.StateManager.SetDirDelegBatchWindow(
			time.Duration(settings.DirDelegBatchWindowMs) * time.Millisecond,
		)
	}

	// Operation blocklist -> v4 Handler
	blockedOps := settings.GetBlockedOperations()
	s.v4Handler.SetBlockedOps(blockedOps)
	if len(blockedOps) > 0 {
		logger.Info("NFS adapter: operation blocklist active",
			"blocked_ops", blockedOps)
	}
}
