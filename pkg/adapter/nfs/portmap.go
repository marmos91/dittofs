package nfs

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/sysreg"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
)

// isPortmapperEnabled returns whether the embedded portmapper should be started.
//
// The portmapper is disabled by default. Users can explicitly enable it by
// setting adapters.nfs.portmapper.enabled to true in the configuration file.
//
// The *bool pointer approach allows distinguishing between:
//   - nil (not set in config) -> default to false (portmapper disabled)
//   - false (explicitly disabled) -> portmapper disabled
//   - true (explicitly enabled) -> portmapper enabled
func (s *NFSAdapter) isPortmapperEnabled() bool {
	if s.config.Portmapper.Enabled == nil {
		return false // Default: disabled
	}
	return *s.config.Portmapper.Enabled
}

// startPortmapper creates and starts the embedded portmapper server.
//
// The portmapper (RFC 1057) enables NFS clients to discover DittoFS services
// via standard tools like rpcinfo and showmount. It registers all DittoFS
// services (NFS, MOUNT, NLM, NSM) on TCP.
//
// The portmapper runs in a background goroutine. If it fails to start
// (e.g., port in use), the error is returned but should be treated as
// non-fatal -- NFS operates normally without it, clients just need explicit
// port options (e.g., mount -o port=12049).
//
// Parameters:
//   - ctx: Controls the portmapper lifecycle. Cancellation triggers shutdown.
//
// Returns nil if the portmapper is disabled or starts successfully.
// Returns an error if the server fails to bind its port.
func (s *NFSAdapter) startPortmapper(ctx context.Context) error {
	if !s.isPortmapperEnabled() {
		logger.Info("Portmapper disabled by configuration")
		return nil
	}

	// Create registry and register all DittoFS services
	registry := portmap.NewRegistry()
	registry.RegisterDittoFSServices(s.config.Port, s.isUDPEnabled())
	registry.RegisterPortmapper(s.config.Portmapper.Port)

	// Create portmapper server
	server := portmap.NewServer(portmap.ServerConfig{
		Port:      s.config.Portmapper.Port,
		EnableTCP: true,
		EnableUDP: true,
		Registry:  registry,
	})

	// Store references for shutdown
	s.portmapRegistry = registry
	s.portmapServer = server

	// Start in background goroutine
	errCh := make(chan error, 1)
	go func() {
		if err := server.Serve(ctx); err != nil {
			errCh <- err
			logger.Error("Portmapper server error", "error", err)
		}
	}()

	// Wait for listeners to be ready (or fail) with a timeout
	select {
	case <-server.WaitReady():
		logger.Info("Portmapper started", "port", s.config.Portmapper.Port, "services", registry.Count())
		return nil
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		return nil // Timeout waiting for ready, but non-fatal
	}
}

// registerWithSystemEnabled reports whether DittoFS should register its
// services with the host's system rpcbind on port 111. Defaults to false when
// unset (same *bool convention as isPortmapperEnabled).
func (s *NFSAdapter) registerWithSystemEnabled() bool {
	if s.config.Portmapper.RegisterWithSystem == nil {
		return false
	}
	return *s.config.Portmapper.RegisterWithSystem
}

// systemPortmapAddr is the dial address of the host's system rpcbind.
func systemPortmapAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", sysreg.SystemPortmapPort)
}

// startSystemPortmapRegistration registers DittoFS's services with the host's
// system rpcbind (port 111) when adapters.nfs.portmapper.register_with_system is
// enabled. Best effort: a missing/unreachable rpcbind is logged and skipped —
// NFS still serves, only kernel NFSv3 (NLM) locking without `nolock` stays
// unavailable. On success it sets sysregActive so shutdown unregisters.
// systemRegMappings returns the service mappings to register with the system
// rpcbind. It deliberately EXCLUDES NSM (prog 100024): on a host that runs
// rpc.statd, NSM is already owned by the host's status monitor, which is shared
// infrastructure — claiming it would redirect every host SM_NOTIFY to DittoFS.
// The kernel only needs NLM (and MOUNT/NFS) discovery to take v3 byte-range
// locks; status monitoring continues via the host statd.
func (s *NFSAdapter) systemRegMappings() []*xdr.Mapping {
	all := portmap.DittoFSServiceMappings(s.config.Port, s.isUDPEnabled())
	out := all[:0:0]
	for _, m := range all {
		if m.Prog == rpc.ProgramNSM {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *NFSAdapter) startSystemPortmapRegistration(ctx context.Context) {
	if !s.registerWithSystemEnabled() {
		return
	}

	addr := systemPortmapAddr()
	if err := sysreg.Ping(ctx, addr); err != nil {
		logger.Warn("No system portmapper reachable; NFSv3 locking needs `nolock`",
			"addr", addr, "error", err)
		return
	}

	// Best effort: Register claims each tuple (UNSET+SET) and continues past
	// conflicts, so a tuple the host already owns (e.g. MOUNT/NFS on a host that
	// also runs kernel NFS) does not stop the critical NLM registration. We mark
	// the registration active and unregister on shutdown regardless of partial
	// failures, since whatever landed must be cleaned up.
	mappings := s.systemRegMappings()
	s.sysregActive.Store(true)
	if err := sysreg.Register(ctx, addr, mappings); err != nil {
		logger.Warn("Some services failed to register with system portmapper",
			"addr", addr, "error", err)
		return
	}
	logger.Info("Registered NFS services with system portmapper",
		"addr", addr, "services", len(mappings))
}

// stopSystemPortmapRegistration unregisters DittoFS's services from the system
// rpcbind. No-op when system registration was never active.
func (s *NFSAdapter) stopSystemPortmapRegistration() {
	if !s.sysregActive.Load() {
		return
	}
	s.sysregActive.Store(false)

	// Use a fresh bounded context: the adapter's lifecycle ctx is already
	// cancelled during shutdown, but unregistering stale NLM/NSM mappings is
	// important enough to spend a few seconds on.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mappings := s.systemRegMappings()
	if err := sysreg.Unregister(ctx, systemPortmapAddr(), mappings); err != nil {
		logger.Warn("Failed to unregister services from system portmapper", "error", err)
		return
	}
	logger.Info("Unregistered NFS services from system portmapper")
}

// stopPortmapper gracefully shuts down the embedded portmapper server.
//
// This is called during NFS adapter shutdown to ensure the portmapper
// stops accepting queries and releases its TCP/UDP ports. Stop() blocks
// until all portmapper goroutines have completed.
//
// Safe to call when portmapper is nil (disabled or never started).
func (s *NFSAdapter) stopPortmapper() {
	if s.portmapServer == nil {
		return
	}
	s.portmapServer.Stop()
	logger.Info("Portmapper stopped")
}
