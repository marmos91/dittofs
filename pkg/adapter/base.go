package adapter

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ConnectionHandler represents a protocol-specific connection that can serve
// requests. Each protocol adapter creates its own connection type implementing
// this interface. The Serve method blocks until the connection is closed or
// the context is cancelled.
type ConnectionHandler interface {
	Serve(ctx context.Context)
}

// ConnectionFactory creates protocol-specific connection handlers for accepted
// TCP connections. Protocol adapters implement this interface and pass themselves
// to BaseAdapter.ServeWithFactory().
type ConnectionFactory interface {
	NewConnection(conn net.Conn) ConnectionHandler
}

// BaseConfig holds configuration common to all protocol adapters.
// Protocol-specific adapters embed this alongside their own config.
type BaseConfig struct {
	// BindAddress is the IP address to bind to.
	// Empty string or "0.0.0.0" binds to all interfaces.
	BindAddress string

	// Port is the TCP port to listen on.
	Port int

	// MaxConnections limits the number of concurrent client connections.
	// 0 means unlimited.
	MaxConnections int

	// ShutdownTimeout is the maximum duration to wait for active connections
	// to complete during graceful shutdown.
	ShutdownTimeout time.Duration

	// MetricsLogInterval is the interval at which to log server metrics.
	// 0 disables periodic metrics logging.
	MetricsLogInterval time.Duration
}

// MetricsRecorder allows protocol adapters to record connection lifecycle
// metrics. NFS provides an implementation; SMB may provide nil (no metrics).
type MetricsRecorder interface {
	RecordConnectionAccepted()
	RecordConnectionClosed()
	RecordConnectionForceClosed()
	SetActiveConnections(count int32)
}

// OnConnectionClose is an optional callback invoked when a connection's serve
// goroutine completes (before WaitGroup.Done and semaphore release). Protocol
// adapters use this for protocol-specific cleanup (e.g., NFS backchannel
// unbinding). The callback receives the connection remote address and any
// protocol-specific data stored during accept.
type OnConnectionClose func(addr string)

// BaseAdapter provides shared TCP lifecycle management for protocol adapters.
//
// Both NFS and SMB adapters embed this struct and delegate listener management,
// graceful shutdown, connection tracking, and metrics logging to it. Protocol-
// specific behavior is injected via ConnectionFactory and PreAccept hooks.
//
// Thread safety:
// All exported methods are safe for concurrent use. The shutdown mechanism uses
// sync.Once to ensure idempotent behavior even if Stop() is called multiple times.
type BaseAdapter struct {
	// Config holds the shared configuration (bind address, port, limits, timeouts)
	Config BaseConfig

	// protocolName is the human-readable protocol name for logging (e.g., "NFS", "SMB")
	protocolName string

	// Metrics is an optional recorder for connection lifecycle metrics.
	// If nil, no metrics are collected (zero overhead).
	Metrics MetricsRecorder

	// listener is the TCP listener for accepting connections.
	// Closed during shutdown to stop accepting new connections.
	listener net.Listener

	// activeConns tracks all currently active connections for graceful shutdown.
	// Each connection calls Add(1) when starting and Done() when complete.
	activeConns sync.WaitGroup

	// shutdownOnce ensures shutdown is only initiated once.
	// Protects the shutdown channel close and listener cleanup.
	shutdownOnce sync.Once

	// Shutdown signals that graceful shutdown has been initiated.
	// Closed by initiateShutdown(), monitored by ServeWithFactory().
	Shutdown chan struct{}

	// ConnCount tracks the current number of active connections.
	// Used for metrics and shutdown logging.
	ConnCount atomic.Int32

	// connSemaphore limits the number of concurrent connections if MaxConnections > 0.
	// Connections must acquire a slot before being accepted.
	// nil if MaxConnections is 0 (unlimited).
	connSemaphore chan struct{}

	// ShutdownCtx is cancelled during shutdown to abort in-flight requests.
	// This context is passed to all request handlers, allowing them to detect
	// shutdown and gracefully abort long-running operations.
	ShutdownCtx context.Context

	// CancelRequests cancels ShutdownCtx during shutdown.
	// This triggers request cancellation across all active connections.
	CancelRequests context.CancelFunc

	// ActiveConnections tracks all active TCP connections for forced closure.
	// Maps connection remote address (string) to net.Conn for forced shutdown.
	// Uses sync.Map for concurrent-safe access optimized for high churn scenarios.
	ActiveConnections sync.Map

	// ListenerReady is closed when the listener is ready to accept connections.
	// Used by tests to synchronize with server startup.
	ListenerReady chan struct{}

	// listenerMu protects access to the listener field.
	listenerMu sync.RWMutex

	// Registry provides access to all stores and shares.
	Registry *runtime.Runtime
}

// NewBaseAdapter creates a new BaseAdapter with the specified configuration.
// The adapter is created in a stopped state. Call ServeWithFactory() to start.
//
// Returns a pointer to avoid copying sync primitives (WaitGroup, Once, Map, RWMutex).
func NewBaseAdapter(config BaseConfig, protocol string) *BaseAdapter {
	var connSemaphore chan struct{}
	if config.MaxConnections > 0 {
		connSemaphore = make(chan struct{}, config.MaxConnections)
		logger.Debug(protocol+" connection limit", "max_connections", config.MaxConnections)
	} else {
		logger.Debug(protocol+" connection limit", "max_connections", "unlimited")
	}

	shutdownCtx, cancelRequests := context.WithCancel(context.Background())

	return &BaseAdapter{
		Config:         config,
		protocolName:   protocol,
		Shutdown:       make(chan struct{}),
		connSemaphore:  connSemaphore,
		ShutdownCtx:    shutdownCtx,
		CancelRequests: cancelRequests,
		ListenerReady:  make(chan struct{}),
	}
}

// SetRuntime stores the runtime reference. Protocol adapters should call
// b.BaseAdapter.SetRuntime(rt) first, then perform protocol-specific setup.
func (b *BaseAdapter) SetRuntime(rt *runtime.Runtime) {
	b.Registry = rt
}

// ServeWithFactory runs the shared TCP accept loop, delegating to factory for
// protocol-specific connection creation.
//
// Parameters:
//   - ctx: Controls the server lifecycle. Cancellation triggers graceful shutdown.
//   - factory: Creates protocol-specific connection handlers for each accepted connection.
//   - preAccept: Optional hook called after TCP accept but before connection tracking.
//     Return true to accept the connection, false to reject it. If nil, all connections
//     are accepted.
//   - onClose: Optional callback invoked when a connection's goroutine exits, before
//     WaitGroup.Done and semaphore release. Used for protocol-specific cleanup.
//     If nil, no callback is invoked.
//
// Returns:
//   - nil on graceful shutdown
//   - error if listener fails to start or shutdown is not graceful
func (b *BaseAdapter) ServeWithFactory(
	ctx context.Context,
	factory ConnectionFactory,
	preAccept func(net.Conn) bool,
	onClose OnConnectionClose,
) error {
	// Create TCP listener
	listenAddr := fmt.Sprintf("%s:%d", b.Config.BindAddress, b.Config.Port)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to create %s listener on port %d: %w", b.protocolName, b.Config.Port, err)
	}

	// Store listener with mutex protection and signal readiness
	b.listenerMu.Lock()
	b.listener = listener
	b.listenerMu.Unlock()
	close(b.ListenerReady)

	logger.Info(b.protocolName+" server listening", "port", b.Config.Port)

	// Monitor context cancellation in separate goroutine
	go func() {
		<-ctx.Done()
		logger.Info(b.protocolName+" shutdown signal received", "error", ctx.Err())
		b.initiateShutdown()
	}()

	// Start metrics logging if enabled
	if b.Config.MetricsLogInterval > 0 {
		go b.logMetrics(ctx)
	}

	// Accept connections until shutdown
	for {
		// Acquire connection semaphore if connection limiting is enabled
		if b.connSemaphore != nil {
			select {
			case b.connSemaphore <- struct{}{}:
				// Acquired semaphore slot, proceed with accept
			case <-b.Shutdown:
				// Shutdown initiated while waiting for semaphore
				return b.gracefulShutdown()
			}
		}

		// Accept next connection (blocks until connection arrives or error)
		tcpConn, err := b.listener.Accept()
		if err != nil {
			// Release semaphore on accept error
			if b.connSemaphore != nil {
				<-b.connSemaphore
			}

			// Check if error is due to shutdown (expected) or network error (unexpected)
			select {
			case <-b.Shutdown:
				// Expected error during shutdown (listener was closed)
				return b.gracefulShutdown()
			default:
				// Unexpected error - log but continue
				logger.Debug("Error accepting "+b.protocolName+" connection", "error", err)
				continue
			}
		}

		// Enable TCP_NODELAY to disable Nagle's algorithm
		if tcp, ok := tcpConn.(*net.TCPConn); ok {
			if err := tcp.SetNoDelay(true); err != nil {
				logger.Debug("Failed to set TCP_NODELAY", "error", err)
			}
		}

		// Protocol-specific pre-accept check (e.g., live settings max_connections)
		if preAccept != nil && !preAccept(tcpConn) {
			_ = tcpConn.Close()
			if b.connSemaphore != nil {
				<-b.connSemaphore
			}
			continue
		}

		// Track connection for graceful shutdown
		b.activeConns.Add(1)
		b.ConnCount.Add(1)

		// Register connection for forced closure capability
		connAddr := tcpConn.RemoteAddr().String()
		b.ActiveConnections.Store(connAddr, tcpConn)

		// Record metrics for connection accepted
		currentConns := b.ConnCount.Load()
		if b.Metrics != nil {
			b.Metrics.RecordConnectionAccepted()
			b.Metrics.SetActiveConnections(currentConns)
		}

		// Log new connection
		logger.Debug(b.protocolName+" connection accepted", "address", tcpConn.RemoteAddr(), "active", currentConns)

		// Create protocol-specific connection handler
		conn := factory.NewConnection(tcpConn)

		// Handle connection in separate goroutine
		go func(addr string, tcp net.Conn) {
			defer func() {
				// Protocol-specific cleanup callback
				if onClose != nil {
					onClose(addr)
				}

				// Unregister connection from tracking map
				b.ActiveConnections.Delete(addr)

				// Cleanup on connection close
				b.activeConns.Done()
				b.ConnCount.Add(-1)
				if b.connSemaphore != nil {
					<-b.connSemaphore
				}

				// Record metrics for connection closed
				if b.Metrics != nil {
					b.Metrics.RecordConnectionClosed()
					b.Metrics.SetActiveConnections(b.ConnCount.Load())
				}

				logger.Debug(b.protocolName+" connection closed", "address", tcp.RemoteAddr(), "active", b.ConnCount.Load())
			}()

			// Handle connection requests
			conn.Serve(b.ShutdownCtx)
		}(connAddr, tcpConn)
	}
}

// initiateShutdown signals the server to begin graceful shutdown.
//
// Shutdown sequence:
//  1. Close shutdown channel (signals accept loop to stop)
//  2. Close listener (stops accepting new connections)
//  3. Interrupt blocking reads on all active connections
//  4. Cancel shutdownCtx (signals in-flight requests to abort)
//
// Thread safety:
// Safe to call multiple times and from multiple goroutines.
func (b *BaseAdapter) initiateShutdown() {
	b.shutdownOnce.Do(func() {
		logger.Debug(b.protocolName + " shutdown initiated")

		// Close shutdown channel (signals accept loop)
		close(b.Shutdown)

		// Close listener (stops accepting new connections)
		b.listenerMu.Lock()
		if b.listener != nil {
			if err := b.listener.Close(); err != nil {
				logger.Debug("Error closing "+b.protocolName+" listener", "error", err)
			}
		}
		b.listenerMu.Unlock()

		// Set a short deadline on all connections to unblock any pending reads
		b.interruptBlockingReads()

		// Cancel all in-flight request contexts
		b.CancelRequests()
		logger.Debug(b.protocolName + " request cancellation signal sent to all in-flight operations")
	})
}

// interruptBlockingReads sets a short deadline on all active connections
// to interrupt any blocking read operations during shutdown.
func (b *BaseAdapter) interruptBlockingReads() {
	deadline := time.Now().Add(100 * time.Millisecond)

	b.ActiveConnections.Range(func(key, value any) bool {
		if conn, ok := value.(net.Conn); ok {
			if err := conn.SetReadDeadline(deadline); err != nil {
				logger.Debug("Error setting shutdown deadline on connection",
					"address", key, "error", err)
			}
		}
		return true
	})
	logger.Debug(b.protocolName + " shutdown: interrupted blocking reads on all connections")
}

// gracefulShutdown waits for active connections to complete or timeout.
//
// Returns:
//   - nil if all connections completed gracefully
//   - error if shutdown timeout exceeded (connections were force-closed)
func (b *BaseAdapter) gracefulShutdown() error {
	activeCount := b.ConnCount.Load()
	logger.Info(b.protocolName+" graceful shutdown: waiting for active connections",
		"active", activeCount, "timeout", b.Config.ShutdownTimeout)

	// Create channel that closes when all connections are done
	done := make(chan struct{})
	go func() {
		b.activeConns.Wait()
		close(done)
	}()

	// Wait for completion or timeout
	select {
	case <-done:
		logger.Info(b.protocolName + " graceful shutdown complete: all connections closed")
		return nil

	case <-time.After(b.Config.ShutdownTimeout):
		remaining := b.ConnCount.Load()
		logger.Warn(b.protocolName+" shutdown timeout exceeded - forcing closure",
			"active", remaining, "timeout", b.Config.ShutdownTimeout)

		// Force-close all remaining connections
		b.forceCloseConnections()

		return fmt.Errorf("%s shutdown timeout: %d connections force-closed", b.protocolName, remaining)
	}
}

// forceCloseConnections closes all active TCP connections to accelerate shutdown.
func (b *BaseAdapter) forceCloseConnections() {
	logger.Info("Force-closing active " + b.protocolName + " connections")

	closedCount := 0
	b.ActiveConnections.Range(func(key, value any) bool {
		addr := key.(string)
		conn := value.(net.Conn)

		if err := conn.Close(); err != nil {
			logger.Debug("Error force-closing connection", "address", addr, "error", err)
		} else {
			closedCount++
			logger.Debug("Force-closed connection", "address", addr)
			if b.Metrics != nil {
				b.Metrics.RecordConnectionForceClosed()
			}
		}

		return true
	})

	if closedCount == 0 {
		logger.Debug("No connections to force-close")
	} else {
		logger.Info("Force-closed connections", "count", closedCount)
	}
}

// Stop initiates graceful shutdown of the server.
//
// Stop is safe to call multiple times and safe to call concurrently with
// ServeWithFactory(). It signals the server to begin shutdown and waits for
// active connections to complete up to ShutdownTimeout.
//
// Parameters:
//   - ctx: Controls the shutdown timeout. If cancelled, Stop returns immediately
//     with context error after initiating shutdown.
//
// Returns:
//   - nil on successful graceful shutdown
//   - error if shutdown timeout exceeded or context cancelled
func (b *BaseAdapter) Stop(ctx context.Context) error {
	// Always initiate shutdown first
	b.initiateShutdown()

	// If no context provided, use gracefulShutdown with configured timeout
	if ctx == nil {
		return b.gracefulShutdown()
	}

	// Wait for graceful shutdown with context timeout
	activeCount := b.ConnCount.Load()
	logger.Info(b.protocolName+" graceful shutdown: waiting for active connections (context timeout)",
		"active", activeCount)

	// Create channel that closes when all connections are done
	done := make(chan struct{})
	go func() {
		b.activeConns.Wait()
		close(done)
	}()

	// Wait for completion or context cancellation
	select {
	case <-done:
		logger.Info(b.protocolName + " graceful shutdown complete: all connections closed")
		return nil

	case <-ctx.Done():
		remaining := b.ConnCount.Load()
		logger.Warn(b.protocolName+" shutdown context cancelled",
			"active", remaining, "error", ctx.Err())
		return ctx.Err()
	}
}

// logMetrics periodically logs server metrics for monitoring.
func (b *BaseAdapter) logMetrics(ctx context.Context) {
	ticker := time.NewTicker(b.Config.MetricsLogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			activeConns := b.ConnCount.Load()
			logger.Info(b.protocolName+" metrics", "active_connections", activeConns)
		}
	}
}

// GetActiveConnections returns the current number of active connections.
func (b *BaseAdapter) GetActiveConnections() int32 {
	return b.ConnCount.Load()
}

// GetListenerAddr returns the address the server is listening on.
// This method blocks until the listener is ready, making it safe for tests.
func (b *BaseAdapter) GetListenerAddr() string {
	<-b.ListenerReady

	b.listenerMu.RLock()
	defer b.listenerMu.RUnlock()

	if b.listener == nil {
		return ""
	}
	return b.listener.Addr().String()
}

// Port returns the configured TCP port.
func (b *BaseAdapter) Port() int {
	return b.Config.Port
}

// Protocol returns the human-readable protocol name (e.g., "NFS", "SMB").
func (b *BaseAdapter) Protocol() string {
	return b.protocolName
}

// MapError is a default stub implementation that returns nil.
// Protocol-specific adapters should override this method to translate domain
// errors into protocol-specific ProtocolError values with appropriate status codes.
func (b *BaseAdapter) MapError(_ error) ProtocolError {
	return nil
}
