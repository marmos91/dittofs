package nfs

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/portmap"
)

// isPortmapperEnabled returns whether the embedded portmapper should be started.
//
// The portmapper is enabled by default. Users can explicitly disable it by
// setting adapters.nfs.portmapper.enabled to false in the configuration file.
//
// The *bool pointer approach allows distinguishing between:
//   - nil (not set in config) -> default to true (portmapper enabled)
//   - false (explicitly disabled) -> portmapper disabled
//   - true (explicitly enabled) -> portmapper enabled
func (s *NFSAdapter) isPortmapperEnabled() bool {
	if s.config.Portmapper.Enabled == nil {
		return true // Default: enabled
	}
	return *s.config.Portmapper.Enabled
}

// startPortmapper creates and starts the embedded portmapper server.
//
// The portmapper (RFC 1057) enables NFS clients to discover DittoFS services
// via standard tools like rpcinfo and showmount. It registers all DittoFS
// services (NFS, MOUNT, NLM, NSM) on both TCP and UDP.
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
	registry.RegisterDittoFSServices(s.config.Port)
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
