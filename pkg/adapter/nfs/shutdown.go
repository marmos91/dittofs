package nfs

import (
	"context"
)

// Stop initiates graceful shutdown of the NFS server.
//
// Stop performs NFS-specific cleanup (portmapper, GSS, Kerberos) first,
// then delegates to BaseAdapter.Stop() for the shared shutdown sequence
// (close listener, cancel context, wait for connections, force-close).
//
// Stop is safe to call multiple times and safe to call concurrently with Serve().
//
// Parameters:
//   - ctx: Controls the shutdown timeout. If cancelled, Stop returns immediately
//     with context error after initiating shutdown.
//
// Returns:
//   - nil on successful graceful shutdown
//   - error if shutdown timeout exceeded or context cancelled
//
// Thread safety:
// Safe to call concurrently from multiple goroutines.
func (s *NFSAdapter) Stop(ctx context.Context) error {
	// Stop portmapper first (stops accepting new queries before NFS stops)
	s.stopPortmapper()

	// Stop GSS processor if running (releases background cleanup goroutine)
	if s.gssProcessor != nil {
		s.gssProcessor.Stop()
	}

	// Close Kerberos provider (stops keytab hot-reload goroutine)
	if s.kerberosProvider != nil {
		_ = s.kerberosProvider.Close()
	}

	// Delegate to BaseAdapter for shared shutdown (listener close, context cancel,
	// connection wait, force-close)
	return s.BaseAdapter.Stop(ctx)
}
