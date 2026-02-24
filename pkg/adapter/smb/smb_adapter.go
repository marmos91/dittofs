package smb

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/session"
	"github.com/marmos91/dittofs/internal/protocol/smb/v2/handlers"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// SMBAdapter implements the adapter.Adapter interface for SMB2 protocol.
//
// This adapter provides an SMB2 server with:
//   - Graceful shutdown with configurable timeout
//   - Connection limiting and resource management
//   - Context-based request cancellation
//   - Configurable timeouts for read/write/idle operations
//   - Thread-safe operation with atomic counters
//
// Architecture:
// SMBAdapter manages the TCP listener and connection lifecycle. Each accepted
// connection is handled by an SMBConnection instance that manages SMB2 request/response
// cycles. The adapter coordinates graceful shutdown across all active connections
// using context cancellation and wait groups.
//
// Shutdown flow:
//  1. Context cancelled or Stop() called
//  2. Listener closed (no new connections)
//  3. shutdownCtx cancelled (signals in-flight requests to abort)
//  4. Wait for active connections to complete (up to ShutdownTimeout)
//  5. Force-close any remaining connections after timeout
//
// Thread safety:
// All methods are safe for concurrent use. The shutdown mechanism uses sync.Once
// to ensure idempotent behavior even if Stop() is called multiple times.
type SMBAdapter struct {
	// config holds the server configuration (ports, timeouts, limits)
	config SMBConfig

	// listener is the TCP listener for accepting SMB connections
	// Closed during shutdown to stop accepting new connections
	listener net.Listener

	// handler processes SMB2 protocol operations (CREATE, READ, WRITE, etc.)
	handler *handlers.Handler

	// registry provides access to all stores and shares
	registry *runtime.Runtime

	// activeConns tracks all currently active connections for graceful shutdown
	// Each connection calls Add(1) when starting and Done() when complete
	activeConns sync.WaitGroup

	// shutdownOnce ensures shutdown is only initiated once
	// Protects the shutdown channel close and listener cleanup
	shutdownOnce sync.Once

	// shutdown signals that graceful shutdown has been initiated
	// Closed by initiateShutdown(), monitored by Serve()
	shutdown chan struct{}

	// connCount tracks the current number of active connections
	// Used for metrics and shutdown logging
	connCount atomic.Int32

	// connSemaphore limits the number of concurrent connections if MaxConnections > 0
	// Connections must acquire a slot before being accepted
	// nil if MaxConnections is 0 (unlimited)
	connSemaphore chan struct{}

	// shutdownCtx is cancelled during shutdown to abort in-flight requests
	// This context is passed to all request handlers, allowing them to detect
	// shutdown and gracefully abort long-running operations
	shutdownCtx context.Context

	// cancelRequests cancels shutdownCtx during shutdown
	// This triggers request cancellation across all active connections
	cancelRequests context.CancelFunc

	// activeConnections tracks all active TCP connections for forced closure
	// Maps connection remote address (string) to net.Conn for forced shutdown
	// Uses sync.Map for concurrent-safe access optimized for high churn scenarios
	activeConnections sync.Map

	// listenerReady is closed when the listener is ready to accept connections
	// Used by tests to synchronize with server startup
	listenerReady chan struct{}

	// listenerMu protects access to the listener field
	listenerMu sync.RWMutex

	// sessionManager provides unified session and credit management
	sessionManager *session.Manager
}

// New creates a new SMBAdapter with the specified configuration.
//
// The adapter is created in a stopped state. Call SetRuntime() to inject
// the Runtime, then call Serve() to start accepting connections.
//
// Configuration:
//   - Zero values in config are replaced with sensible defaults
//   - Invalid configurations cause a panic (indicates programmer error)
//
// Parameters:
//   - config: Server configuration (ports, timeouts, limits)
//
// Returns a configured but not yet started SMBAdapter.
//
// Panics if config validation fails.
func New(config SMBConfig) *SMBAdapter {
	// Apply defaults for zero values
	config.applyDefaults()

	// Validate configuration
	if err := config.validate(); err != nil {
		panic(fmt.Sprintf("invalid SMB config: %v", err))
	}

	// Create connection semaphore if MaxConnections is set
	var connSemaphore chan struct{}
	if config.MaxConnections > 0 {
		connSemaphore = make(chan struct{}, config.MaxConnections)
		logger.Debug("SMB connection limit", "max_connections", config.MaxConnections)
	} else {
		logger.Debug("SMB connection limit", "max_connections", "unlimited")
	}

	// Create shutdown context for request cancellation
	shutdownCtx, cancelRequests := context.WithCancel(context.Background())

	// Create unified session manager with configured credit strategy
	creditConfig := config.Credits.ToSessionConfig()
	creditStrategy := config.Credits.GetStrategy()
	sessionManager := session.NewManagerWithStrategy(creditStrategy, creditConfig)

	logger.Debug("SMB credit configuration",
		"strategy", config.Credits.Strategy,
		"min_grant", creditConfig.MinGrant,
		"max_grant", creditConfig.MaxGrant,
		"initial_grant", creditConfig.InitialGrant,
		"max_session_credits", creditConfig.MaxSessionCredits)

	// Create handler with session manager
	handler := handlers.NewHandlerWithSessionManager(sessionManager)

	// Apply signing configuration to handler
	handler.SigningConfig = config.Signing.ToSigningConfig()
	logger.Debug("SMB signing configuration",
		"enabled", handler.SigningConfig.Enabled,
		"required", handler.SigningConfig.Required)

	return &SMBAdapter{
		config:         config,
		handler:        handler,
		shutdown:       make(chan struct{}),
		connSemaphore:  connSemaphore,
		shutdownCtx:    shutdownCtx,
		cancelRequests: cancelRequests,
		listenerReady:  make(chan struct{}),
		sessionManager: sessionManager,
	}
}

// SetRuntime injects the runtime containing all stores and shares.
//
// This method is called by Runtime before Serve() is called. The runtime
// provides access to all configured metadata stores, content stores, and shares.
//
// Parameters:
//   - rt: Runtime containing all stores and shares
//
// Thread safety:
// Called exactly once before Serve(), no synchronization needed.
func (s *SMBAdapter) SetRuntime(rt *runtime.Runtime) {
	s.registry = rt
	s.handler.Registry = rt

	// Register OplockManager with MetadataService for cross-protocol lease breaks.
	// This enables NFS handlers to break SMB leases before write/delete operations.
	// The registration uses the package-level SetOplockChecker function since
	// OplockChecker is a global singleton (one SMB adapter instance).
	if s.handler.OplockManager != nil {
		metadata.SetOplockChecker(s.handler.OplockManager)
		logger.Debug("SMB adapter registered OplockManager for cross-protocol lease breaks")
	}

	logger.Debug("SMB adapter configured with runtime", "shares", rt.CountShares())

	// Apply live SMB adapter settings from SettingsWatcher.
	// The SettingsWatcher polls DB every 10s and provides atomic pointer swap
	// for thread-safe reads. Settings are consumed here at startup and on
	// each new connection (grandfathering per locked decision).
	s.applySMBSettings(rt)
}

// applySMBSettings reads current SMB adapter settings from the runtime's
// SettingsWatcher and applies them. Called during SetRuntime (startup).
func (s *SMBAdapter) applySMBSettings(rt *runtime.Runtime) {
	settings := rt.GetSMBSettings()
	if settings == nil {
		logger.Debug("SMB adapter: no live settings available, using defaults")
		return
	}

	// Encryption stub: log warning when enabled per locked decision
	if settings.EnableEncryption {
		logger.Info("SMB encryption requested but not yet implemented -- connections will proceed without encryption")
	}

	// Dialect range: logged at startup for visibility
	logger.Debug("SMB adapter: dialect range from settings",
		"min_dialect", settings.MinDialect,
		"max_dialect", settings.MaxDialect)

	// Operation blocklist: log active blocks. SMB blocklist is a pass-through
	// that logs unsupported operation names (SMB doesn't have the same per-op
	// granularity as NFS COMPOUND).
	blockedOps := settings.GetBlockedOperations()
	if len(blockedOps) > 0 {
		logger.Info("SMB adapter: operation blocklist from settings (advisory only)",
			"blocked_ops", blockedOps)
	}
}

// Serve starts the SMB server and blocks until the context is cancelled
// or an unrecoverable error occurs.
//
// Serve accepts incoming TCP connections on the configured port and spawns
// a goroutine to handle each connection. The connection handler processes
// SMB2 protocol requests.
//
// Graceful shutdown:
// When the context is cancelled, Serve initiates graceful shutdown:
//  1. Stops accepting new connections (listener closed)
//  2. Cancels all in-flight request contexts (shutdownCtx cancelled)
//  3. Waits for active connections to complete (up to ShutdownTimeout)
//  4. Forcibly closes any remaining connections after timeout
//
// Parameters:
//   - ctx: Controls the server lifecycle. Cancellation triggers graceful shutdown.
//
// Returns:
//   - nil on graceful shutdown
//   - context.Canceled if cancelled via context
//   - error if listener fails to start or shutdown is not graceful
//
// Thread safety:
// Serve() should only be called once per SMBAdapter instance.
func (s *SMBAdapter) Serve(ctx context.Context) error {
	// Create TCP listener
	listenAddr := fmt.Sprintf("%s:%d", s.config.BindAddress, s.config.Port)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to create SMB listener on port %d: %w", s.config.Port, err)
	}

	// Store listener with mutex protection and signal readiness
	s.listenerMu.Lock()
	s.listener = listener
	s.listenerMu.Unlock()
	close(s.listenerReady)

	logger.Info("SMB server listening", "port", s.config.Port)
	logger.Debug("SMB config", "max_connections", s.config.MaxConnections, "read_timeout", s.config.Timeouts.Read, "write_timeout", s.config.Timeouts.Write, "idle_timeout", s.config.Timeouts.Idle)

	// Monitor context cancellation in separate goroutine
	go func() {
		<-ctx.Done()
		logger.Info("SMB shutdown signal received", "error", ctx.Err())
		s.initiateShutdown()
	}()

	// Start metrics logging if enabled
	if s.config.MetricsLogInterval > 0 {
		go s.logMetrics(ctx)
	}

	// Accept connections until shutdown
	for {
		// Acquire connection semaphore if connection limiting is enabled
		if s.connSemaphore != nil {
			select {
			case s.connSemaphore <- struct{}{}:
				// Acquired semaphore slot, proceed with accept
			case <-s.shutdown:
				// Shutdown initiated while waiting for semaphore
				return s.gracefulShutdown()
			}
		}

		// Accept next connection (blocks until connection arrives or error)
		tcpConn, err := s.listener.Accept()
		if err != nil {
			// Release semaphore on accept error
			if s.connSemaphore != nil {
				<-s.connSemaphore
			}

			// Check if error is due to shutdown (expected) or network error (unexpected)
			select {
			case <-s.shutdown:
				// Expected error during shutdown (listener was closed)
				return s.gracefulShutdown()
			default:
				// Unexpected error - log but continue
				logger.Debug("Error accepting SMB connection", "error", err)
				continue
			}
		}

		// Enable TCP_NODELAY to disable Nagle's algorithm
		// This ensures SMB responses are sent immediately without waiting for more data
		if tcp, ok := tcpConn.(*net.TCPConn); ok {
			if err := tcp.SetNoDelay(true); err != nil {
				logger.Debug("Failed to set TCP_NODELAY", "error", err)
			}
		}

		// Check live settings for dynamic max_connections limit.
		// Per locked decision: existing connections are grandfathered; only new
		// connections are rejected when the live limit is exceeded.
		if s.registry != nil {
			if liveSettings := s.registry.GetSMBSettings(); liveSettings != nil {
				if liveSettings.MaxConnections > 0 {
					currentActive := s.connCount.Load()
					if int(currentActive) >= liveSettings.MaxConnections {
						logger.Warn("SMB connection rejected: live settings max_connections exceeded",
							"active", currentActive,
							"max_connections", liveSettings.MaxConnections,
							"client", tcpConn.RemoteAddr())
						_ = tcpConn.Close()
						if s.connSemaphore != nil {
							<-s.connSemaphore
						}
						continue
					}
				}
			}
		}

		// Track connection for graceful shutdown
		s.activeConns.Add(1)
		s.connCount.Add(1)

		// Register connection for forced closure capability
		connAddr := tcpConn.RemoteAddr().String()
		s.activeConnections.Store(connAddr, tcpConn)

		// Log new connection
		currentConns := s.connCount.Load()
		logger.Debug("SMB connection accepted", "address", tcpConn.RemoteAddr(), "active", currentConns)

		// Handle connection in separate goroutine
		conn := s.newConn(tcpConn)
		go func(addr string, tcp net.Conn) {
			defer func() {
				// Unregister connection from tracking map
				s.activeConnections.Delete(addr)

				// Cleanup on connection close
				s.activeConns.Done()
				s.connCount.Add(-1)
				if s.connSemaphore != nil {
					<-s.connSemaphore
				}

				logger.Debug("SMB connection closed", "address", tcp.RemoteAddr(), "active", s.connCount.Load())
			}()

			// Handle connection requests
			conn.Serve(s.shutdownCtx)
		}(connAddr, tcpConn)
	}
}

// initiateShutdown signals the server to begin graceful shutdown.
//
// Thread safety:
// Safe to call multiple times and from multiple goroutines.
func (s *SMBAdapter) initiateShutdown() {
	s.shutdownOnce.Do(func() {
		logger.Debug("SMB shutdown initiated")

		// Close shutdown channel (signals accept loop)
		close(s.shutdown)

		// Close listener (stops accepting new connections)
		s.listenerMu.Lock()
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logger.Debug("Error closing SMB listener", "error", err)
			}
		}
		s.listenerMu.Unlock()

		// Set a short deadline on all connections to unblock any pending reads
		// This allows connection loops to notice shutdown quickly instead of
		// waiting for the full read timeout (which could be minutes)
		s.interruptBlockingReads()

		// Cancel all in-flight request contexts
		s.cancelRequests()
		logger.Debug("SMB request cancellation signal sent to all in-flight operations")
	})
}

// interruptBlockingReads sets a short deadline on all active connections
// to interrupt any blocking read operations during shutdown.
// This allows connections to notice the shutdown signal quickly.
func (s *SMBAdapter) interruptBlockingReads() {
	// Set deadline to 100ms from now - enough time for any in-flight reads to complete
	// but short enough for quick shutdown
	deadline := time.Now().Add(100 * time.Millisecond)

	s.activeConnections.Range(func(key, value any) bool {
		if conn, ok := value.(net.Conn); ok {
			// Setting deadline will cause any blocked Read() to return with timeout error
			if err := conn.SetReadDeadline(deadline); err != nil {
				logger.Debug("Error setting shutdown deadline on connection",
					"address", key, "error", err)
			}
		}
		return true
	})
	logger.Debug("SMB shutdown: interrupted blocking reads on all connections")
}

// gracefulShutdown waits for active connections to complete or timeout.
func (s *SMBAdapter) gracefulShutdown() error {
	activeCount := s.connCount.Load()
	logger.Info("SMB graceful shutdown: waiting for active connections", "active", activeCount, "timeout", s.config.Timeouts.Shutdown)

	// Create channel that closes when all connections are done
	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	// Wait for completion or timeout
	select {
	case <-done:
		logger.Info("SMB graceful shutdown complete: all connections closed")
		return nil

	case <-time.After(s.config.Timeouts.Shutdown):
		remaining := s.connCount.Load()
		logger.Warn("SMB shutdown timeout exceeded - forcing closure", "active", remaining, "timeout", s.config.Timeouts.Shutdown)

		// Force-close all remaining connections
		s.forceCloseConnections()

		return fmt.Errorf("SMB shutdown timeout: %d connections force-closed", remaining)
	}
}

// forceCloseConnections closes all active TCP connections to accelerate shutdown.
func (s *SMBAdapter) forceCloseConnections() {
	logger.Info("Force-closing active SMB connections")

	closedCount := 0
	s.activeConnections.Range(func(key, value any) bool {
		addr := key.(string)
		conn := value.(net.Conn)

		if err := conn.Close(); err != nil {
			logger.Debug("Error force-closing connection", "address", addr, "error", err)
		} else {
			closedCount++
			logger.Debug("Force-closed connection", "address", addr)
		}

		return true
	})

	if closedCount == 0 {
		logger.Debug("No connections to force-close")
	} else {
		logger.Info("Force-closed connections", "count", closedCount)
	}
}

// Stop initiates graceful shutdown of the SMB server.
//
// Stop is safe to call multiple times and safe to call concurrently with Serve().
func (s *SMBAdapter) Stop(ctx context.Context) error {
	// Always initiate shutdown first
	s.initiateShutdown()

	// If no context provided, use gracefulShutdown with configured timeout
	if ctx == nil {
		return s.gracefulShutdown()
	}

	// Wait for graceful shutdown with context timeout
	activeCount := s.connCount.Load()
	logger.Info("SMB graceful shutdown: waiting for active connections (context timeout)", "active", activeCount)

	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Info("SMB graceful shutdown complete: all connections closed")
		return nil

	case <-ctx.Done():
		remaining := s.connCount.Load()
		logger.Warn("SMB shutdown context cancelled", "active", remaining, "error", ctx.Err())
		return ctx.Err()
	}
}

// logMetrics periodically logs server metrics for monitoring.
func (s *SMBAdapter) logMetrics(ctx context.Context) {
	ticker := time.NewTicker(s.config.MetricsLogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			activeConns := s.connCount.Load()
			logger.Info("SMB metrics", "active_connections", activeConns)
		}
	}
}

// GetActiveConnections returns the current number of active connections.
func (s *SMBAdapter) GetActiveConnections() int32 {
	return s.connCount.Load()
}

// GetListenerAddr returns the address the server is listening on.
func (s *SMBAdapter) GetListenerAddr() string {
	<-s.listenerReady

	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()

	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// newConn creates a new connection wrapper for a TCP connection.
func (s *SMBAdapter) newConn(tcpConn net.Conn) *SMBConnection {
	return NewSMBConnection(s, tcpConn)
}

// SetKerberosProvider injects the shared Kerberos provider into the SMB handler.
// This enables Kerberos authentication via SPNEGO in SESSION_SETUP.
// Must be called before Serve(). When not called, Kerberos auth is disabled
// and only NTLM/guest authentication is available.
func (s *SMBAdapter) SetKerberosProvider(provider *kerberos.Provider) {
	s.handler.KerberosProvider = provider
	logger.Debug("SMB adapter Kerberos provider configured",
		"principal", provider.ServicePrincipal())
}

// Port returns the TCP port the SMB server is listening on.
func (s *SMBAdapter) Port() int {
	return s.config.Port
}

// Protocol returns "SMB" as the protocol identifier.
func (s *SMBAdapter) Protocol() string {
	return "SMB"
}

// ============================================================================
// Session Reconnection for Grace Period Recovery
// ============================================================================

// OnReconnect is called when an SMB session reconnects after server restart.
//
// During the grace period, this method triggers lease reclaim for all leases
// the client previously held. This allows SMB clients to restore their caching
// state after server restart, maintaining cache consistency.
//
// Parameters:
//   - ctx: Context for cancellation
//   - sessionID: The reconnecting session ID
//   - clientID: The connection tracker client ID
//
// Implementation note: This is a minimal implementation for gap closure.
// Full implementation would enumerate persisted leases for the session and
// call HandleLeaseReclaim for each one. Currently, the reclaim happens
// implicitly when the client requests the same lease key during grace period.
func (s *SMBAdapter) OnReconnect(ctx context.Context, sessionID uint64, clientID string) {
	logger.Info("SMB session reconnected",
		"sessionID", sessionID,
		"clientID", clientID)

	// During grace period, leases will be reclaimed when the client
	// makes a CREATE request with its known lease key.
	// The RequestLeaseWithReclaim method handles this transparently.
	//
	// A full implementation would:
	// 1. Query LockStore for all leases owned by this clientID
	// 2. Prepare them for reclaim on first access
	// 3. Notify client of available leases to reclaim
	//
	// For this gap closure, we rely on implicit reclaim in RequestLeaseWithReclaim.
}
