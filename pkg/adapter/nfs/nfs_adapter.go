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
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
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
}

// NFSConfig holds configuration parameters for the NFS server.
//
// These values control server behavior including connection limits, timeouts,
// and resource management. All timeout values are optional - zero means no timeout.
//
// Default values (applied by New if zero):
//   - Port: 2049 (standard NFS port)
//   - MaxConnections: 0 (unlimited)
//   - ReadTimeout: 30s
//   - WriteTimeout: 30s
//   - IdleTimeout: 5m
//   - ShutdownTimeout: 30s
//   - MetricsLogInterval: 5m (0 disables)
//
// Production recommendations:
//   - MaxConnections: Set based on expected load (e.g., 1000 for busy servers)
//   - ReadTimeout: 30s prevents slow clients from holding connections
//   - WriteTimeout: 30s prevents slow networks from blocking responses
//   - IdleTimeout: 5m closes inactive connections to free resources
//   - ShutdownTimeout: 30s balances graceful shutdown with restart time
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

	// ReadTimeout is the maximum duration for reading a complete RPC request.
	// This prevents slow or malicious clients from holding connections indefinitely.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	ReadTimeout time.Duration `mapstructure:"read_timeout" validate:"min=0"`

	// WriteTimeout is the maximum duration for writing an RPC response.
	// This prevents slow networks or clients from blocking server resources.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	WriteTimeout time.Duration `mapstructure:"write_timeout" validate:"min=0"`

	// IdleTimeout is the maximum duration a connection can remain idle
	// between requests before being closed automatically.
	// This frees resources from abandoned connections.
	// 0 means no timeout (connections stay open indefinitely).
	// Recommended: 5m for production.
	IdleTimeout time.Duration `mapstructure:"idle_timeout" validate:"min=0"`

	// ShutdownTimeout is the maximum duration to wait for active connections
	// to complete during graceful shutdown.
	// After this timeout, remaining connections are forcibly closed.
	// Must be > 0 to ensure shutdown completes.
	// Recommended: 30s (balances graceful shutdown with restart time).
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" validate:"required,gt=0"`

	// MetricsLogInterval is the interval at which to log server metrics
	// (active connections, requests/sec, etc.).
	// 0 disables periodic metrics logging.
	// Recommended: 5m for production monitoring.
	MetricsLogInterval time.Duration `mapstructure:"metrics_log_interval" validate:"min=0"`
}

// applyDefaults fills in zero values with sensible defaults.
func (c *NFSConfig) applyDefaults() {
	// Enabled defaults to true if not explicitly set
	// (zero value for bool is false, but we want it enabled by default)
	// Note: This is handled in pkg/config/defaults.go for consistency

	if c.Port <= 0 {
		c.Port = 2049
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 5 * time.Minute
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 5 * time.Minute
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	if c.MetricsLogInterval == 0 {
		c.MetricsLogInterval = 5 * time.Minute
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
	if c.ReadTimeout < 0 {
		return fmt.Errorf("invalid ReadTimeout %v: must be >= 0", c.ReadTimeout)
	}
	if c.WriteTimeout < 0 {
		return fmt.Errorf("invalid WriteTimeout %v: must be >= 0", c.WriteTimeout)
	}
	if c.IdleTimeout < 0 {
		return fmt.Errorf("invalid IdleTimeout %v: must be >= 0", c.IdleTimeout)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("invalid ShutdownTimeout %v: must be > 0", c.ShutdownTimeout)
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
//
// Returns a configured but not yet started NFSAdapter.
//
// Panics if config validation fails.
func New(config NFSConfig) *NFSAdapter {
	// Apply defaults for zero values
	config.applyDefaults()

	// Validate configuration
	if err := config.validate(); err != nil {
		panic(fmt.Sprintf("invalid NFS config: %v", err))
	}

	// Create connection semaphore if MaxConnections is set
	var connSemaphore chan struct{}
	if config.MaxConnections > 0 {
		connSemaphore = make(chan struct{}, config.MaxConnections)
		logger.Debug("NFS connection limit: %d", config.MaxConnections)
	} else {
		logger.Debug("NFS connection limit: unlimited")
	}

	// Create shutdown context for request cancellation
	shutdownCtx, cancelRequests := context.WithCancel(context.Background())

	return &NFSAdapter{
		config:         config,
		nfsHandler:     &v3.DefaultNFSHandler{},
		mountHandler:   &mount.DefaultMountHandler{},
		shutdown:       make(chan struct{}),
		connSemaphore:  connSemaphore,
		shutdownCtx:    shutdownCtx,
		cancelRequests: cancelRequests,
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

	s.listener = listener
	logger.Info("NFS server listening on port %d", s.config.Port)
	logger.Debug("NFS config: max_connections=%d read_timeout=%v write_timeout=%v idle_timeout=%v",
		s.config.MaxConnections, s.config.ReadTimeout, s.config.WriteTimeout, s.config.IdleTimeout)

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
	for {
		// Check if shutdown has been initiated
		select {
		case <-s.shutdown:
			return s.gracefulShutdown()
		default:
		}

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

		// Log new connection (debug level to avoid log spam under load)
		logger.Debug("NFS connection accepted from %s (active: %d)",
			tcpConn.RemoteAddr(), s.connCount.Load())

		// Handle connection in separate goroutine
		conn := s.newConn(tcpConn)
		go func() {
			defer func() {
				// Cleanup on connection close
				s.activeConns.Done()
				s.connCount.Add(-1)
				if s.connSemaphore != nil {
					<-s.connSemaphore
				}
				logger.Debug("NFS connection closed from %s (active: %d)",
					tcpConn.RemoteAddr(), s.connCount.Load())
			}()

			// Handle connection requests
			// Pass shutdownCtx so requests can detect shutdown and abort
			conn.Serve(s.shutdownCtx)
		}()
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
// After timeout, any remaining connections are considered non-responsive
// and are abandoned (their goroutines will eventually clean up when they
// complete or detect the closed listener).
//
// Returns:
//   - nil if all connections completed gracefully
//   - error if shutdown timeout was exceeded
//
// Thread safety:
// Should only be called once, from the Serve() method.
func (s *NFSAdapter) gracefulShutdown() error {
	activeCount := s.connCount.Load()
	logger.Info("NFS graceful shutdown: waiting for %d active connection(s) (timeout: %v)",
		activeCount, s.config.ShutdownTimeout)

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

	case <-time.After(s.config.ShutdownTimeout):
		remaining := s.connCount.Load()
		logger.Warn("NFS shutdown timeout exceeded: %d connection(s) still active after %v",
			remaining, s.config.ShutdownTimeout)
		return fmt.Errorf("NFS shutdown timeout: %d connections still active", remaining)
	}
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
