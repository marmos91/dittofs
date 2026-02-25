package nfs

import (
	"time"

	v4attrs "github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/logger"
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
		s.v4Handler.StateManager.SetLeaseTime(time.Duration(settings.LeaseTime) * time.Second)
		v4attrs.SetLeaseTime(uint32(settings.LeaseTime))
		logger.Debug("NFS adapter: applied lease time from settings",
			"lease_time_seconds", settings.LeaseTime)
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
	// 0 explicitly resets to default (50ms fallback in resetBatchTimer).
	// Negative values are invalid and ignored.
	if settings.DirDelegBatchWindowMs >= 0 {
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

	// Portmapper settings -> adapter config
	// The DB model uses plain bool; the adapter config uses *bool pointer.
	// We always set the pointer from the DB value so it's never nil.
	enabled := settings.PortmapperEnabled
	s.config.Portmapper.Enabled = &enabled
	if settings.PortmapperPort > 0 {
		s.config.Portmapper.Port = settings.PortmapperPort
	}
	logger.Debug("NFS adapter: applied portmapper settings from DB",
		"enabled", settings.PortmapperEnabled, "port", s.config.Portmapper.Port)
}
