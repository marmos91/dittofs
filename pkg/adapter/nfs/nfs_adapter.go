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
	v3 "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	"github.com/marmos91/dittofs/internal/ratelimiter"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// NFSAdapter implements the adapter.Adapter interface for NFSv3 protocol.
//
// This adapter provides a production-ready NFSv3 server with:
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
	nfsHandler v3.NFSHandler

	// mountHandler processes MOUNT protocol operations (MNT, UMNT, EXPORT, etc.)
	mountHandler mount.MountHandler

	// store provides access to file system metadata operations
	metadataStore metadata.MetadataStore

	// content provides access to file content (data blocks)
	content content.ContentStore

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

	// rateLimiter enforces request rate limiting when enabled
	// nil if rate limiting is disabled
	rateLimiter *ratelimiter.RateLimiter
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

	// Timeouts groups all timeout-related configuration
	Timeouts NFSTimeoutsConfig `mapstructure:"timeouts"`

	// MetricsLogInterval is the interval at which to log server metrics
	// (active connections, requests/sec, etc.).
	// 0 disables periodic metrics logging.
	// Recommended: 5m for production monitoring.
	MetricsLogInterval time.Duration `mapstructure:"metrics_log_interval" validate:"min=0"`

	// RateLimiting contains adapter-specific rate limiting configuration.
	// If not specified, inherits from server.rate_limiting.
	// If specified, overrides server-level configuration for this adapter.
	RateLimiting *RateLimitingConfig `mapstructure:"rate_limiting"`
}

// RateLimitingConfig controls request rate limiting for the NFS adapter.
// This type mirrors config.RateLimitingConfig to avoid import cycles.
type RateLimitingConfig struct {
	Enabled           bool `mapstructure:"enabled"`
	RequestsPerSecond uint `mapstructure:"requests_per_second"`
	Burst             uint `mapstructure:"burst"`
}

// applyDefaults fills in zero values with sensible defaults.
func (c *NFSConfig) applyDefaults() {
	// Note: Enabled field defaults are handled in pkg/config/defaults.go
	// to allow explicit false values from configuration files.

	if c.Port <= 0 {
		c.Port = 2049
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
	// Rate limiting defaults are handled at server level in pkg/config/defaults.go
	// Adapter-level rate limiting is optional and inherits from server config
	if c.RateLimiting != nil {
		if c.RateLimiting.Enabled && c.RateLimiting.RequestsPerSecond == 0 {
			c.RateLimiting.RequestsPerSecond = 5000 // Default: 5000 req/s
		}
		if c.RateLimiting.Enabled && c.RateLimiting.Burst == 0 {
			// Default burst is 2x the sustained rate for handling traffic spikes
			c.RateLimiting.Burst = c.RateLimiting.RequestsPerSecond * 2
		}
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
	if c.RateLimiting != nil && c.RateLimiting.Enabled && c.RateLimiting.RequestsPerSecond > 0 && c.RateLimiting.Burst < c.RateLimiting.RequestsPerSecond {
		return fmt.Errorf("invalid rate_limiting.burst %d: should be >= rate_limiting.requests_per_second %d",
			c.RateLimiting.Burst, c.RateLimiting.RequestsPerSecond)
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
//   - serverRateLimit: Server-level rate limiting config (can be nil)
//   - nfsMetrics: Optional metrics collector (nil for no metrics)
//
// Returns a configured but not yet started NFSAdapter.
//
// Panics if config validation fails.
func New(nfsConfig NFSConfig, serverRateLimit *RateLimitingConfig, nfsMetrics metrics.NFSMetrics) *NFSAdapter {
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
		logger.Debug("NFS connection limit: %d", nfsConfig.MaxConnections)
	} else {
		logger.Debug("NFS connection limit: unlimited")
	}

	// Create shutdown context for request cancellation
	shutdownCtx, cancelRequests := context.WithCancel(context.Background())

	// Use no-op metrics if none provided
	if nfsMetrics == nil {
		nfsMetrics = metrics.NewNoopNFSMetrics()
	}

	// Determine effective rate limiting config
	// Adapter-level config overrides server-level config
	var effectiveRateLimit *RateLimitingConfig
	if nfsConfig.RateLimiting != nil {
		effectiveRateLimit = nfsConfig.RateLimiting
		logger.Debug("Using adapter-level rate limiting configuration")
	} else if serverRateLimit != nil {
		effectiveRateLimit = serverRateLimit
		logger.Debug("Using server-level rate limiting configuration")
	}

	// Create rate limiter if enabled
	var rateLimiter *ratelimiter.RateLimiter
	if effectiveRateLimit != nil && effectiveRateLimit.Enabled {
		rateLimiter = ratelimiter.New(effectiveRateLimit.RequestsPerSecond, effectiveRateLimit.Burst)
		logger.Info("NFS rate limiting enabled: %d req/s (burst: %d)",
			effectiveRateLimit.RequestsPerSecond, effectiveRateLimit.Burst)
	} else {
		logger.Debug("NFS rate limiting disabled")
	}

	return &NFSAdapter{
		config:         nfsConfig,
		nfsHandler:     &v3.DefaultNFSHandler{},
		mountHandler:   &mount.DefaultMountHandler{},
		metrics:        nfsMetrics,
		shutdown:       make(chan struct{}),
		connSemaphore:  connSemaphore,
		shutdownCtx:    shutdownCtx,
		cancelRequests: cancelRequests,
		listenerReady:  make(chan struct{}),
		rateLimiter:    rateLimiter,
	}
}

// SetStores injects the shared metadata and content stores.
//
// This method is called by DittoServer before Serve() is called. The stores
// are shared across all protocol adapters.
//
// Parameters:
//   - metadataStore: store for file system metadata operations
//   - contentRepo: Repository for file content operations
//
// Thread safety:
// Called exactly once before Serve(), no synchronization needed.
func (s *NFSAdapter) SetStores(metadataStore metadata.MetadataStore, contentRepo content.ContentStore) {
	s.metadataStore = metadataStore
	s.content = contentRepo
	logger.Debug("NFS repositories configured")
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

	logger.Info("NFS server listening on port %d", s.config.Port)
	logger.Debug("NFS config: max_connections=%d read_timeout=%v write_timeout=%v idle_timeout=%v",
		s.config.MaxConnections, s.config.Timeouts.Read, s.config.Timeouts.Write, s.config.Timeouts.Idle)

	// Monitor context cancellation in separate goroutine
	// This allows the main accept loop to focus on accepting connections
	go func() {
		<-ctx.Done()
		logger.Info("NFS shutdown signal received: %v", ctx.Err())
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
				logger.Debug("Error accepting NFS connection: %v", err)
				continue
			}
		}

		// Track connection for graceful shutdown
		s.activeConns.Add(1)
		s.connCount.Add(1)

		// Register connection for forced closure capability
		connAddr := tcpConn.RemoteAddr().String()
		s.activeConnections.Store(connAddr, tcpConn)

		// Record metrics for connection accepted
		s.metrics.RecordConnectionAccepted()
		currentConns := s.connCount.Load()
		s.metrics.SetActiveConnections(currentConns)

		// Log new connection (debug level to avoid log spam under load)
		logger.Debug("NFS connection accepted from %s (active: %d)",
			tcpConn.RemoteAddr(), currentConns)

		// Handle connection in separate goroutine
		// Capture connAddr and tcpConn in closure to avoid races
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

				// Record metrics for connection closed
				s.metrics.RecordConnectionClosed()
				currentConns := s.connCount.Load()
				s.metrics.SetActiveConnections(currentConns)

				logger.Debug("NFS connection closed from %s (active: %d)",
					tcp.RemoteAddr(), currentConns)
			}()

			// Handle connection requests
			// Pass shutdownCtx so requests can detect shutdown and abort
			conn.Serve(s.shutdownCtx)
		}(connAddr, tcpConn)
	}
}

// initiateShutdown signals the server to begin graceful shutdown.
//
// This method is called automatically when the context is cancelled or
// when Stop() is called. It's safe to call multiple times.
//
// Shutdown sequence:
//  1. Close shutdown channel (signals accept loop to stop)
//  2. Close listener (stops accepting new connections)
//  3. Cancel shutdownCtx (signals in-flight requests to abort)
//
// The context cancellation propagates through the entire request stack:
//   - Connection handlers detect ctx.Done() and finish current request
//   - RPC dispatchers check ctx.Done() before processing
//   - NFS procedure handlers check ctx.Done() during long operations
//   - store operations can detect ctx.Done() for early abort
//
// This enables graceful abort of long-running operations like:
//   - Large directory scans (READDIR/READDIRPLUS check context in loop)
//   - Large file reads/writes (can abort between chunks)
//   - Metadata tree traversal (can abort at each level)
//
// Thread safety:
// Safe to call multiple times and from multiple goroutines.
// Uses sync.Once to ensure shutdown logic only runs once.
func (s *NFSAdapter) initiateShutdown() {
	s.shutdownOnce.Do(func() {
		logger.Debug("NFS shutdown initiated")

		// Close shutdown channel (signals accept loop)
		close(s.shutdown)

		// Close listener (stops accepting new connections)
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logger.Debug("Error closing NFS listener: %v", err)
			}
		}

		// Cancel all in-flight request contexts
		// This is the key to graceful shutdown: NFS procedure handlers
		// check ctx.Done() during long operations and abort cleanly
		s.cancelRequests()
		logger.Debug("NFS request cancellation signal sent to all in-flight operations")
	})
}

// gracefulShutdown waits for active connections to complete or timeout.
//
// This method blocks until either:
//   - All active connections complete naturally
//   - ShutdownTimeout expires
//
// Shutdown Flow:
//  1. Wait for all connections to complete naturally (up to ShutdownTimeout)
//  2. If timeout expires, force-close all remaining TCP connections
//  3. Context cancellation (already done in initiateShutdown) triggers handlers to abort
//  4. TCP close causes connection reads/writes to fail, accelerating cleanup
//
// Force Closure Strategy:
// After timeout, we actively close TCP connections to trigger immediate cleanup.
// This is safer than leaving goroutines running because:
//   - Closes TCP socket (releases OS resources)
//   - Triggers immediate error in ongoing reads/writes
//   - Connection handlers detect errors and exit
//   - Context cancellation prevents starting new work
//
// Returns:
//   - nil if all connections completed gracefully
//   - error if shutdown timeout exceeded (connections were force-closed)
//
// Thread safety:
// Should only be called once, from the Serve() method.
func (s *NFSAdapter) gracefulShutdown() error {
	activeCount := s.connCount.Load()
	logger.Info("NFS graceful shutdown: waiting for %d active connection(s) (timeout: %v)",
		activeCount, s.config.Timeouts.Shutdown)

	// Create channel that closes when all connections are done
	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	// Wait for completion or timeout
	select {
	case <-done:
		logger.Info("NFS graceful shutdown complete: all connections closed")
		return nil

	case <-time.After(s.config.Timeouts.Shutdown):
		remaining := s.connCount.Load()
		logger.Warn("NFS shutdown timeout exceeded: %d connection(s) still active after %v - forcing closure",
			remaining, s.config.Timeouts.Shutdown)

		// Force-close all remaining connections
		s.forceCloseConnections()

		return fmt.Errorf("NFS shutdown timeout: %d connections force-closed", remaining)
	}
}

// forceCloseConnections closes all active TCP connections to accelerate shutdown.
//
// This method is called after the graceful shutdown timeout expires. It iterates
// through all active connections and closes their underlying TCP sockets.
//
// Why Force Close:
//  1. Context cancellation (shutdownCtx) signals handlers to stop gracefully
//  2. TCP close forces immediate failure of any ongoing I/O operations
//  3. This combination ensures connections exit quickly even if stuck in I/O
//
// Effect on Clients:
//   - Clients receive TCP RST or FIN, depending on connection state
//   - NFS clients will see connection errors and reconnect/retry
//   - No data loss (in-flight requests were already cancelled by context)
//
// Thread safety:
// Safe to call once during shutdown. Uses sync.Map for concurrent-safe iteration.
func (s *NFSAdapter) forceCloseConnections() {
	logger.Info("Force-closing active NFS connections")

	// Close all tracked connections
	// sync.Map iteration (Range) is safe to call concurrently with Store/Delete operations,
	// though concurrent modifications may or may not be visible during iteration
	closedCount := 0
	s.activeConnections.Range(func(key, value any) bool {
		addr := key.(string)
		conn := value.(net.Conn)

		if err := conn.Close(); err != nil {
			logger.Debug("Error force-closing connection to %s: %v", addr, err)
		} else {
			closedCount++
			logger.Debug("Force-closed connection to %s", addr)
			// Record metric for each force-closed connection
			s.metrics.RecordConnectionForceClosed()
		}

		// Continue iteration
		return true
	})

	if closedCount == 0 {
		logger.Debug("No connections to force-close")
	} else {
		logger.Info("Force-closed %d connection(s)", closedCount)
	}

	// Note: sync.Map entries are automatically deleted by deferred cleanup in Serve()
	// No need to manually clear the map
}

// Stop initiates graceful shutdown of the NFS server.
//
// Stop is safe to call multiple times and safe to call concurrently with Serve().
// It signals the server to begin shutdown and waits for active connections to
// complete up to ShutdownTimeout.
//
// The context parameter allows the caller to set a custom shutdown timeout,
// overriding the configured ShutdownTimeout. If ctx is cancelled before
// connections complete, Stop returns with the context error.
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
	// Always initiate shutdown first
	s.initiateShutdown()

	// If no context provided, use gracefulShutdown with configured timeout
	if ctx == nil {
		return s.gracefulShutdown()
	}

	// Wait for graceful shutdown with context timeout
	activeCount := s.connCount.Load()
	logger.Info("NFS graceful shutdown: waiting for %d active connection(s) (context timeout)",
		activeCount)

	// Create channel that closes when all connections are done
	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	// Wait for completion or context cancellation
	select {
	case <-done:
		logger.Info("NFS graceful shutdown complete: all connections closed")
		return nil

	case <-ctx.Done():
		remaining := s.connCount.Load()
		logger.Warn("NFS shutdown context cancelled: %d connection(s) still active: %v",
			remaining, ctx.Err())
		return ctx.Err()
	}
}

// logMetrics periodically logs server metrics for monitoring.
//
// This goroutine logs active connection count at regular intervals
// (MetricsLogInterval) to help operators monitor server load.
//
// Future enhancements could include:
//   - Requests per second
//   - Average request latency
//   - Error rates
//   - Memory usage
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
			logger.Info("NFS metrics: active_connections=%d", activeConns)
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
func (s *NFSAdapter) newConn(tcpConn net.Conn) *NFSConnection {
	return NewNFSConnection(s, tcpConn)
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
