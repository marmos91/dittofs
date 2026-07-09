package nfs

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
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
	// Unsubscribe from share change notifications to prevent stale callbacks
	// from accumulating across adapter restarts.
	for _, unsub := range s.shareUnsubscribers {
		unsub()
	}
	s.shareUnsubscribers = nil

	// Unsubscribe identity resolver callbacks registered by wireIdentityResolver.
	if s.identityUnsub != nil {
		s.identityUnsub()
		s.identityUnsub = nil
	}
	if s.identityProviderUnsub != nil {
		s.identityProviderUnsub()
		s.identityProviderUnsub = nil
	}

	// Stop the auxiliary/companion services (portmapper, system rpcbind
	// registration, UDP transport, NSM) before tearing down the main listener.
	// The group stops them in reverse start order, which preserves the ordering
	// that matters: unregister from the system rpcbind before the embedded
	// portmapper closes, so a client never resolves a stale NLM port to a port
	// we no longer serve.
	if err := s.sidecars.StopAll(ctx); err != nil {
		logger.Debug("NFS auxiliary service shutdown reported an error", "error", err)
	}

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
