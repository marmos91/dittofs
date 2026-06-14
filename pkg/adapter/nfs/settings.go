package nfs

import (
	"context"
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

	// NFSv4.1 session limits
	if settings.V4MaxSessionSlots > 0 {
		s.v4Handler.StateManager.SetMaxSessionSlots(settings.V4MaxSessionSlots)
	}
	if settings.V4MaxSessionsPerClient > 0 {
		s.v4Handler.StateManager.SetMaxSessionsPerClient(settings.V4MaxSessionsPerClient)
	}
	s.v4Handler.StateManager.SetMaxConnectionsPerSession(settings.V4MaxConnectionsPerSession)

	// Operation blocklist -> v4 Handler.
	// SetBlockedOps parses the names into a uint32 set once here so the hot
	// COMPOUND dispatch path (Handler.IsOperationBlocked) only does a map lookup
	// instead of a per-op JSON unmarshal + linear scan.
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

// fsCapabilitiesProbeTimeout bounds the metadata read issued when refreshing
// the cached filesystem capabilities, so a slow/hung backend cannot stall
// adapter startup or the SettingsWatcher poll goroutine.
const fsCapabilitiesProbeTimeout = 5 * time.Second

// applyFilesystemCapabilities resolves the filesystem capabilities
// (FATTR4_MAXFILESIZE/MAXREAD/MAXWRITE) from the metadata store and stores them
// in the attrs package's process-global atomics. GETATTR then reads the cached
// globals instead of issuing a read transaction per request.
//
// The advertised capabilities are intentionally a single process-global value
// (matching the attrs-package atomic design). They model server-wide limits;
// the metadata stores return these as static configuration, so a single share's
// root handle is representative. If a future deployment backs different shares
// with stores configured for materially different read/write/file-size limits,
// this would advertise one share's limits for all — at that point capability
// caching should move to a per-store keyed cache or into the service layer.
func (s *NFSAdapter) applyFilesystemCapabilities(rt *runtime.Runtime) {
	metaSvc := rt.GetMetadataService()
	if metaSvc == nil {
		return
	}
	shares := rt.ListShares()
	if len(shares) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fsCapabilitiesProbeTimeout)
	defer cancel()

	for _, shareName := range shares {
		root, err := rt.GetRootHandle(shareName)
		if err != nil {
			continue
		}
		caps, err := metaSvc.GetFilesystemCapabilities(ctx, root)
		if err != nil || caps == nil {
			continue
		}
		v4attrs.SetFilesystemCapabilities(caps.MaxFileSize, caps.MaxReadSize, caps.MaxWriteSize)
		logger.Debug("NFS adapter: applied filesystem capabilities",
			"share", shareName,
			"max_file_size", caps.MaxFileSize,
			"max_read", caps.MaxReadSize,
			"max_write", caps.MaxWriteSize)
		return
	}
}
