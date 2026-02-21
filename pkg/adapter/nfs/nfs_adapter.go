package nfs

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	mount "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc/gss"
	v3 "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	v4handlers "github.com/marmos91/dittofs/internal/protocol/nfs/v4/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	v4state "github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/protocol/nlm/blocking"
	nlm_handlers "github.com/marmos91/dittofs/internal/protocol/nlm/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nsm"
	nsm_handlers "github.com/marmos91/dittofs/internal/protocol/nsm/handlers"
	"github.com/marmos91/dittofs/internal/protocol/portmap"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// NFSAdapter implements the adapter.Adapter interface for NFS protocol.
//
// This adapter provides a production-ready NFS server supporting both
// NFSv3 and NFSv4 simultaneously with:
//   - Graceful shutdown with configurable timeout
//   - Connection limiting and resource management
//   - Context-based request cancellation
//   - Configurable timeouts for read/write/idle operations
//   - Thread-safe operation with atomic counters
//
// Architecture:
// NFSAdapter manages the TCP listener and connection lifecycle. Each accepted
// connection is handled by a conn instance (defined elsewhere) that manages
// RPC request/response cycles. The adapter coordinates graceful shutdown across
// all active connections using context cancellation and wait groups.
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
type NFSAdapter struct {
	// config holds the server configuration (ports, timeouts, limits)
	config NFSConfig

	// listener is the TCP listener for accepting NFS connections
	// Closed during shutdown to stop accepting new connections
	listener net.Listener

	// nfsHandler processes NFSv3 protocol operations (LOOKUP, READ, WRITE, etc.)
	nfsHandler *v3.Handler

	// v4Handler processes NFSv4 COMPOUND operations
	v4Handler *v4handlers.Handler

	// pseudoFS is the NFSv4 pseudo-filesystem virtual namespace
	pseudoFS *pseudofs.PseudoFS

	// v3FirstUse and v4FirstUse log at INFO level on first use of each version
	v3FirstUse sync.Once
	v4FirstUse sync.Once

	// mountHandler processes MOUNT protocol operations (MNT, UMNT, EXPORT, etc.)
	mountHandler *mount.Handler

	// nlmHandler processes NLM (Network Lock Manager) operations (LOCK, UNLOCK, TEST, etc.)
	nlmHandler *nlm_handlers.Handler

	// nsmHandler processes NSM (Network Status Monitor) operations (MON, UNMON, NOTIFY, etc.)
	nsmHandler *nsm_handlers.Handler

	// nsmNotifier orchestrates SM_NOTIFY callbacks on server restart
	nsmNotifier *nsm.Notifier

	// nsmMetrics provides NSM-specific Prometheus metrics
	nsmMetrics *nsm.Metrics

	// gssProcessor handles RPCSEC_GSS context lifecycle (INIT/DATA/DESTROY).
	// nil when Kerberos is not enabled.
	gssProcessor *gss.GSSProcessor

	// kerberosProvider holds the Kerberos keytab/config provider.
	// Closed in Stop() to release the keytab hot-reload goroutine.
	// nil when Kerberos is not enabled.
	kerberosProvider *kerberos.Provider

	// kerberosConfig holds the Kerberos configuration for GSS initialization.
	// nil when Kerberos is not enabled.
	kerberosConfig *config.KerberosConfig

	// portmapServer is the embedded portmapper server (RFC 1057).
	// nil when portmapper is disabled.
	portmapServer *portmap.Server

	// portmapRegistry holds the portmap service registry.
	// nil when portmapper is disabled.
	portmapRegistry *portmap.Registry

	// nsmClientStore persists client registrations for crash recovery
	nsmClientStore lock.ClientRegistrationStore

	// blockingQueue manages pending NLM blocking lock requests
	blockingQueue *blocking.BlockingQueue

	// registry provides access to all stores and shares
	registry *runtime.Runtime

	// metrics provides optional Prometheus metrics collection
	// If nil, no metrics are collected (zero overhead)
	metrics metrics.NFSMetrics

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
	// shutdown and gracefully abort long-running operations (directory scans, etc.)
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

	// nextConnID is a global atomic counter for assigning unique connection IDs.
	// Incremented at TCP accept() time and passed to each NFSConnection.
	nextConnID atomic.Uint64
}

// NFSTimeoutsConfig groups all timeout-related configuration.
type NFSTimeoutsConfig struct {
	// Read is the maximum duration for reading a complete RPC request.
	// This prevents slow or malicious clients from holding connections indefinitely.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	Read time.Duration `mapstructure:"read" validate:"min=0"`

	// Write is the maximum duration for writing an RPC response.
	// This prevents slow networks or clients from blocking server resources.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	Write time.Duration `mapstructure:"write" validate:"min=0"`

	// Idle is the maximum duration a connection can remain idle
	// between requests before being closed automatically.
	// This frees resources from abandoned connections.
	// 0 means no timeout (connections stay open indefinitely).
	// Recommended: 5m for production.
	Idle time.Duration `mapstructure:"idle" validate:"min=0"`

	// Shutdown is the maximum duration to wait for active connections
	// to complete during graceful shutdown.
	// After this timeout, remaining connections are forcibly closed.
	// Must be > 0 to ensure shutdown completes.
	// Recommended: 30s (balances graceful shutdown with restart time).
	Shutdown time.Duration `mapstructure:"shutdown" validate:"required,gt=0"`
}

// NFSConfig holds configuration parameters for the NFS server.
//
// These values control server behavior including connection limits, timeouts,
// and resource management.
//
// Default values (applied by New if zero):
//   - Port: 2049 (standard NFS port)
//   - MaxConnections: 0 (unlimited)
//   - Timeouts.Read: 5m
//   - Timeouts.Write: 30s
//   - Timeouts.Idle: 5m
//   - Timeouts.Shutdown: 30s
//   - MetricsLogInterval: 5m (0 disables)
//
// Production recommendations:
//   - MaxConnections: Set based on expected load (e.g., 1000 for busy servers)
//   - Timeouts.Read: 30s prevents slow clients from holding connections
//   - Timeouts.Write: 30s prevents slow networks from blocking responses
//   - Timeouts.Idle: 5m closes inactive connections to free resources
//   - Timeouts.Shutdown: 30s balances graceful shutdown with restart time
type NFSConfig struct {
	// Enabled controls whether the NFS adapter is active.
	// When false, the NFS adapter will not be started.
	Enabled bool `mapstructure:"enabled"`

	// Port is the TCP port to listen on for NFS connections.
	// Standard NFS port is 2049. Must be > 0.
	// If 0, defaults to 2049.
	Port int `mapstructure:"port" validate:"min=0,max=65535"`

	// MaxConnections limits the number of concurrent client connections.
	// When reached, new connections are rejected until existing ones close.
	// 0 means unlimited (not recommended for production).
	// Recommended: 1000-5000 for production servers.
	MaxConnections int `mapstructure:"max_connections" validate:"min=0"`

	// MaxRequestsPerConnection limits the number of concurrent RPC requests
	// that can be processed simultaneously on a single connection.
	// This enables parallel handling of multiple COMMITs, WRITEs, and READs.
	// 0 means unlimited (will default to 100).
	// Recommended: 50-200 for high-throughput servers.
	MaxRequestsPerConnection int `mapstructure:"max_requests_per_connection" validate:"min=0"`

	// Timeouts groups all timeout-related configuration
	Timeouts NFSTimeoutsConfig `mapstructure:"timeouts"`

	// MetricsLogInterval is the interval at which to log server metrics
	// (active connections, requests/sec, etc.).
	// 0 disables periodic metrics logging.
	// Recommended: 5m for production monitoring.
	MetricsLogInterval time.Duration `mapstructure:"metrics_log_interval" validate:"min=0"`

	// Portmapper configures the embedded portmapper (RFC 1057).
	// The portmapper allows NFS clients to discover DittoFS services
	// via rpcinfo/showmount without requiring a system-level rpcbind daemon.
	// Default: enabled on port 10111.
	Portmapper PortmapConfig `mapstructure:"portmapper"`
}

// PortmapConfig holds configuration for the embedded portmapper.
//
// The portmapper enables NFS clients to discover DittoFS services without
// needing a system-level rpcbind/portmap daemon. It runs on a configurable
// port (default 10111, an unprivileged port to avoid requiring root).
//
// Configuration path: adapters.nfs.portmapper.enabled / adapters.nfs.portmapper.port
//
// The Enabled field uses a *bool pointer type to distinguish between
// "not set in config" (nil, defaults to true) and "explicitly set to false".
type PortmapConfig struct {
	// Enabled controls whether the portmapper is active.
	// When nil (not specified in config), defaults to true.
	// Set to false to explicitly disable the portmapper.
	Enabled *bool `mapstructure:"enabled"`

	// Port is the port to listen on for portmapper requests.
	// Default: 10111 (unprivileged port; standard portmapper uses 111 but requires root).
	Port int `mapstructure:"port" validate:"min=0,max=65535"`
}

// applyDefaults fills in zero values with sensible defaults.
func (c *NFSConfig) applyDefaults() {
	// Note: Enabled field defaults are handled in pkg/config/defaults.go
	// to allow explicit false values from configuration files.

	if c.Port <= 0 {
		c.Port = 2049
	}
	if c.MaxRequestsPerConnection == 0 {
		c.MaxRequestsPerConnection = 100
	}
	if c.Timeouts.Read == 0 {
		c.Timeouts.Read = 5 * time.Minute
	}
	if c.Timeouts.Write == 0 {
		c.Timeouts.Write = 30 * time.Second
	}
	if c.Timeouts.Idle == 0 {
		c.Timeouts.Idle = 5 * time.Minute
	}
	if c.Timeouts.Shutdown == 0 {
		c.Timeouts.Shutdown = 30 * time.Second
	}
	if c.MetricsLogInterval == 0 {
		c.MetricsLogInterval = 5 * time.Minute
	}
	// Portmapper port defaults to 10111 (unprivileged port).
	// Note: Portmapper.Enabled is NOT set here -- it uses a *bool pointer where
	// nil means "default to true" and explicit false means "disabled".
	// This is handled by isPortmapperEnabled().
	if c.Portmapper.Port == 0 {
		c.Portmapper.Port = 10111
	}
}

// validate checks that the configuration is valid for production use.
func (c *NFSConfig) validate() error {
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be 0-65535", c.Port)
	}
	if c.MaxConnections < 0 {
		return fmt.Errorf("invalid MaxConnections %d: must be >= 0", c.MaxConnections)
	}
	if c.Timeouts.Read < 0 {
		return fmt.Errorf("invalid timeouts.read %v: must be >= 0", c.Timeouts.Read)
	}
	if c.Timeouts.Write < 0 {
		return fmt.Errorf("invalid timeouts.write %v: must be >= 0", c.Timeouts.Write)
	}
	if c.Timeouts.Idle < 0 {
		return fmt.Errorf("invalid timeouts.idle %v: must be >= 0", c.Timeouts.Idle)
	}
	if c.Timeouts.Shutdown <= 0 {
		return fmt.Errorf("invalid timeouts.shutdown %v: must be > 0", c.Timeouts.Shutdown)
	}
	return nil
}

// New creates a new NFSAdapter with the specified configuration.
//
// The adapter is created in a stopped state. Call SetStores() to inject
// the backend repositories, then call Serve() to start accepting connections.
//
// Configuration:
//   - Zero values in config are replaced with sensible defaults
//   - Invalid configurations cause a panic (indicates programmer error)
//
// Parameters:
//   - config: Server configuration (ports, timeouts, limits)
//   - nfsMetrics: Optional metrics collector (nil for no metrics)
//
// Returns a configured but not yet started NFSAdapter.
//
// Panics if config validation fails.
func New(
	nfsConfig NFSConfig,
	nfsMetrics metrics.NFSMetrics,
) *NFSAdapter {
	// Apply defaults for zero values
	nfsConfig.applyDefaults()

	// Validate configuration
	if err := nfsConfig.validate(); err != nil {
		panic(fmt.Sprintf("invalid NFS config: %v", err))
	}

	// Create connection semaphore if MaxConnections is set
	var connSemaphore chan struct{}
	if nfsConfig.MaxConnections > 0 {
		connSemaphore = make(chan struct{}, nfsConfig.MaxConnections)
		logger.Debug("NFS connection limit", "max_connections", nfsConfig.MaxConnections)
	} else {
		logger.Debug("NFS connection limit", "max_connections", "unlimited")
	}

	// Create shutdown context for request cancellation
	shutdownCtx, cancelRequests := context.WithCancel(context.Background())

	// nfsMetrics can be nil for zero-overhead disabled metrics

	return &NFSAdapter{
		config:         nfsConfig,
		nfsHandler:     &v3.Handler{Metrics: nfsMetrics},
		mountHandler:   &mount.Handler{},
		metrics:        nfsMetrics,
		shutdown:       make(chan struct{}),
		connSemaphore:  connSemaphore,
		shutdownCtx:    shutdownCtx,
		cancelRequests: cancelRequests,
		listenerReady:  make(chan struct{}),
	}
}

// SetKerberosConfig sets the Kerberos configuration for RPCSEC_GSS support.
//
// This must be called before SetRuntime() if Kerberos authentication is desired.
// When set, the GSSProcessor will be initialized during SetRuntime().
//
// Parameters:
//   - cfg: Kerberos configuration. If nil or Enabled is false, Kerberos is disabled.
//
// Thread safety:
// Called exactly once before Serve(), no synchronization needed.
func (s *NFSAdapter) SetKerberosConfig(cfg *config.KerberosConfig) {
	if cfg != nil && cfg.Enabled {
		s.kerberosConfig = cfg
	}
}

// SetRuntime injects the runtime containing all stores and shares.
//
// This method is called by Runtime before Serve() is called. The runtime
// provides access to all configured metadata stores, content stores, and shares.
//
// The NFS adapter stores the runtime and injects it into both the NFS and Mount
// handlers so they can access stores based on share names.
//
// Parameters:
//   - rt: Runtime containing all stores and shares
//
// Thread safety:
// Called exactly once before Serve(), no synchronization needed.
func (s *NFSAdapter) SetRuntime(rt *runtime.Runtime) {
	s.registry = rt

	// Inject runtime into handlers
	s.nfsHandler.Registry = rt
	s.mountHandler.Registry = rt

	// Initialize NFSv4 pseudo-filesystem and handler
	s.pseudoFS = pseudofs.New()
	shares := rt.ListShares()
	s.pseudoFS.Rebuild(shares)
	v4StateManager := v4state.NewStateManager(v4state.DefaultLeaseDuration)
	s.v4Handler = v4handlers.NewHandler(rt, s.pseudoFS, v4StateManager)
	s.v4Handler.KerberosEnabled = s.kerberosConfig != nil

	// Expose StateManager to REST API via runtime (for /clients endpoint and /health server info)
	rt.SetNFSClientProvider(v4StateManager)

	// Register callback to rebuild pseudo-fs when shares change (add/remove)
	rt.OnShareChange(func(shares []string) {
		s.pseudoFS.Rebuild(shares)
		logger.Info("NFSv4 pseudo-fs rebuilt", "shares", len(shares))
	})

	// Create blocking queue for NLM lock operations
	s.blockingQueue = blocking.NewBlockingQueue(nlm_handlers.DefaultBlockingQueueSize)

	// Initialize NLM handler with MetadataService and blocking queue
	metadataService := rt.GetMetadataService()
	s.nlmHandler = nlm_handlers.NewHandler(metadataService, s.blockingQueue)

	// Set unlock callback to process waiting locks when a lock is released
	metadataService.SetNLMUnlockCallback(func(handle metadata.FileHandle) {
		// Process waiters in a goroutine to avoid blocking unlock path
		go s.processNLMWaiters(handle)
	})

	// Initialize NSM handler for crash recovery
	// NSM uses the ConnectionTracker from the MetadataService and ClientRegistrationStore
	s.initNSMHandler(rt, metadataService)

	// Initialize RPCSEC_GSS processor if Kerberos is enabled
	s.initGSSProcessor()

	logger.Debug("NFS adapter configured with runtime", "shares", rt.CountShares())

	// Apply live NFS adapter settings from SettingsWatcher.
	// The SettingsWatcher polls DB every 10s and provides atomic pointer swap
	// for thread-safe reads. Settings are consumed here at startup and on
	// each new connection (grandfathering per locked decision).
	s.applyNFSSettings(rt)
}

// Serve starts the NFS server and blocks until the context is cancelled
// or an unrecoverable error occurs.
//
// Serve accepts incoming TCP connections on the configured port and spawns
// a goroutine to handle each connection. The connection handler processes
// RPC requests for both NFS and MOUNT protocols.
//
// Graceful shutdown:
// When the context is cancelled, Serve initiates graceful shutdown:
//  1. Stops accepting new connections (listener closed)
//  2. Cancels all in-flight request contexts (shutdownCtx cancelled)
//  3. Waits for active connections to complete (up to ShutdownTimeout)
//  4. Forcibly closes any remaining connections after timeout
//
// Context cancellation propagation:
// The shutdownCtx is passed to all connection handlers and flows through
// the entire request stack:
//   - Connection handlers receive shutdownCtx
//   - RPC dispatchers receive shutdownCtx
//   - NFS procedure handlers receive shutdownCtx
//   - store operations can detect cancellation via ctx.Done()
//
// This enables graceful abort of long-running operations like:
//   - Large directory scans (READDIR/READDIRPLUS)
//   - Large file reads/writes
//   - Metadata operations on deep directory trees
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
// Serve() should only be called once per NFSAdapter instance.
func (s *NFSAdapter) Serve(ctx context.Context) error {
	// Create TCP listener
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.config.Port))
	if err != nil {
		return fmt.Errorf("failed to create NFS listener on port %d: %w", s.config.Port, err)
	}

	// Store listener with mutex protection and signal readiness
	s.listenerMu.Lock()
	s.listener = listener
	s.listenerMu.Unlock()
	close(s.listenerReady)

	logger.Info("NFS server listening", "port", s.config.Port)
	logger.Debug("NFS config", "max_connections", s.config.MaxConnections, "read_timeout", s.config.Timeouts.Read, "write_timeout", s.config.Timeouts.Write, "idle_timeout", s.config.Timeouts.Idle)

	// Start embedded portmapper (RFC 1057) for NFS service discovery.
	// This allows clients to query rpcinfo/showmount without needing
	// a system-level rpcbind daemon. Portmapper failure is non-fatal
	// (privileged ports like 111 may require root privileges).
	if err := s.startPortmapper(ctx); err != nil {
		logger.Warn("Portmapper failed to start (NFS will continue without it)", "error", err)
	}

	// NSM startup: Load persisted registrations and notify all clients
	// Per CONTEXT.md: Parallel notification for fastest recovery
	s.performNSMStartup(ctx)

	// Start NFSv4.1 session reaper for expired/unconfirmed client cleanup
	if s.v4Handler != nil && s.v4Handler.StateManager != nil {
		s.v4Handler.StateManager.SetSessionMetrics(v4state.NewSessionMetrics(prometheus.DefaultRegisterer))
		s.v4Handler.StateManager.StartSessionReaper(ctx)
	}

	// Monitor context cancellation in separate goroutine
	// This allows the main accept loop to focus on accepting connections
	go func() {
		<-ctx.Done()
		logger.Info("NFS shutdown signal received", "error", ctx.Err())
		s.initiateShutdown()
	}()

	// Start metrics logging if enabled
	if s.config.MetricsLogInterval > 0 {
		go s.logMetrics(ctx)
	}

	// Accept connections until shutdown
	// Note: We don't check s.shutdown at the top of the loop because:
	// 1. listener.Accept() will fail immediately after shutdown (listener closed)
	// 2. We check s.shutdown in error handling path
	// 3. This reduces redundant select overhead in the hot path
	for {
		// Acquire connection semaphore if connection limiting is enabled
		// This blocks if we're at MaxConnections until a connection closes
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
				// Common causes: resource exhaustion, network issues
				logger.Debug("Error accepting NFS connection", "error", err)
				continue
			}
		}

		// Check live settings for dynamic max_connections limit.
		// This supplements the static connSemaphore from config.
		// Per locked decision: existing connections are grandfathered; only new
		// connections are rejected when the live limit is exceeded.
		if s.registry != nil {
			if liveSettings := s.registry.GetNFSSettings(); liveSettings != nil {
				if liveSettings.MaxConnections > 0 {
					currentActive := s.connCount.Load()
					if int(currentActive) >= liveSettings.MaxConnections {
						logger.Warn("NFS connection rejected: live settings max_connections exceeded",
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

		// Re-apply live NFS settings on each new connection.
		// This ensures dynamic settings changes (e.g., delegations-enabled)
		// propagate from the SettingsWatcher to the StateManager.
		if s.registry != nil {
			s.applyNFSSettings(s.registry)
		}

		// Track connection for graceful shutdown
		s.activeConns.Add(1)
		s.connCount.Add(1)

		// Register connection for forced closure capability
		connAddr := tcpConn.RemoteAddr().String()
		s.activeConnections.Store(connAddr, tcpConn)

		// Record metrics for connection accepted
		currentConns := s.connCount.Load()
		if s.metrics != nil {
			s.metrics.RecordConnectionAccepted()
			s.metrics.SetActiveConnections(currentConns)
		}

		// Log new connection (debug level to avoid log spam under load)
		logger.Debug("NFS connection accepted", "address", tcpConn.RemoteAddr(), "active", currentConns)

		// Assign unique connection ID at accept() time
		connID := s.nextConnID.Add(1)

		// Handle connection in separate goroutine
		// Capture connAddr and tcpConn in closure to avoid races
		conn := s.newConn(tcpConn, connID)
		go func(addr string, tcp net.Conn, cid uint64) {
			defer func() {
				// Unregister connection from tracking map
				s.activeConnections.Delete(addr)

				// Unbind connection from any NFSv4.1 session on disconnect
				if s.v4Handler != nil && s.v4Handler.StateManager != nil {
					s.v4Handler.StateManager.UnbindConnection(cid)
				}

				// Cleanup on connection close
				s.activeConns.Done()
				s.connCount.Add(-1)
				if s.connSemaphore != nil {
					<-s.connSemaphore
				}

				// Record metrics for connection closed
				if s.metrics != nil {
					s.metrics.RecordConnectionClosed()
					currentConns := s.connCount.Load()
					s.metrics.SetActiveConnections(currentConns)
				}

				logger.Debug("NFS connection closed", "address", tcp.RemoteAddr(), "active", s.connCount.Load())
			}()

			// Handle connection requests
			// Pass shutdownCtx so requests can detect shutdown and abort
			conn.Serve(s.shutdownCtx)
		}(connAddr, tcpConn, connID)
	}
}

// logMetrics periodically logs server metrics for monitoring.
//
// This goroutine logs active connection count at regular intervals
// (MetricsLogInterval) to help operators monitor server load.
//
// The goroutine exits when the context is cancelled.
func (s *NFSAdapter) logMetrics(ctx context.Context) {
	ticker := time.NewTicker(s.config.MetricsLogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			activeConns := s.connCount.Load()
			logger.Info("NFS metrics", "active_connections", activeConns)
		}
	}
}

// GetActiveConnections returns the current number of active connections.
//
// This method is primarily used for testing and monitoring.
//
// Returns the count of connections currently being processed.
//
// Thread safety:
// Safe to call concurrently. Uses atomic operations.
func (s *NFSAdapter) GetActiveConnections() int32 {
	return s.connCount.Load()
}

// GetListenerAddr returns the address the server is listening on.
// This method blocks until the listener is ready, making it safe for tests
// to use without race conditions.
//
// Returns:
//   - The listener address as a string (e.g., "127.0.0.1:2049")
//   - Empty string if the server failed to start
//
// Thread safety:
// Safe to call concurrently. Waits for listener to be ready before accessing.
func (s *NFSAdapter) GetListenerAddr() string {
	// Wait for listener to be ready
	<-s.listenerReady

	// Read listener with mutex protection
	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()

	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// newConn creates a new connection wrapper for a TCP connection.
//
// The conn type (defined elsewhere) handles the RPC request/response cycle
// for a single client connection. It processes both NFS and MOUNT protocol
// requests.
//
// Parameters:
//   - tcpConn: The accepted TCP connection
//
// Returns a conn instance ready to serve requests.
func (s *NFSAdapter) newConn(tcpConn net.Conn, connectionID uint64) *NFSConnection {
	return NewNFSConnection(s, tcpConn, connectionID)
}

// Port returns the TCP port the NFS server is listening on.
//
// This implements the adapter.Adapter interface.
//
// Returns the configured port number.
func (s *NFSAdapter) Port() int {
	return s.config.Port
}

// Protocol returns "NFS" as the protocol identifier.
//
// This implements the adapter.Adapter interface.
//
// Returns "NFS" for logging and metrics.
func (s *NFSAdapter) Protocol() string {
	return "NFS"
}

// logV3FirstUse logs at INFO level the first time a client uses NFSv3.
// Subsequent calls are no-ops (uses sync.Once for one-time logging).
func (s *NFSAdapter) logV3FirstUse() {
	s.v3FirstUse.Do(func() {
		logger.Info("First NFSv3 request received")
	})
}

// logV4FirstUse logs at INFO level the first time a client uses NFSv4.
// Subsequent calls are no-ops (uses sync.Once for one-time logging).
func (s *NFSAdapter) logV4FirstUse() {
	s.v4FirstUse.Do(func() {
		logger.Info("First NFSv4 request received")
	})
}
