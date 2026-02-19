package nfs

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

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
		s.listenerMu.Lock()
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logger.Debug("Error closing NFS listener", "error", err)
			}
		}
		s.listenerMu.Unlock()

		// Set a short deadline on all connections to unblock any pending reads
		// This allows connection loops to notice shutdown quickly instead of
		// waiting for the full read timeout (which could be minutes)
		s.interruptBlockingReads()

		// Cancel all in-flight request contexts
		// This is the key to graceful shutdown: NFS procedure handlers
		// check ctx.Done() during long operations and abort cleanly
		s.cancelRequests()
		logger.Debug("NFS request cancellation signal sent to all in-flight operations")
	})
}

// interruptBlockingReads sets a short deadline on all active connections
// to interrupt any blocking read operations during shutdown.
// This allows connections to notice the shutdown signal quickly.
func (s *NFSAdapter) interruptBlockingReads() {
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
	logger.Debug("NFS shutdown: interrupted blocking reads on all connections")
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
	logger.Info("NFS graceful shutdown: waiting for active connections", "active", activeCount, "timeout", s.config.Timeouts.Shutdown)

	// Create channel that closes when all connections are done
	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	// Wait for completion or timeout
	var err error
	select {
	case <-done:
		logger.Info("NFS graceful shutdown complete: all connections closed")

	case <-time.After(s.config.Timeouts.Shutdown):
		remaining := s.connCount.Load()
		logger.Warn("NFS shutdown timeout exceeded - forcing closure", "active", remaining, "timeout", s.config.Timeouts.Shutdown)

		// Force-close all remaining connections
		s.forceCloseConnections()

		err = fmt.Errorf("NFS shutdown timeout: %d connections force-closed", remaining)
	}

	return err
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
			logger.Debug("Error force-closing connection", "address", addr, "error", err)
		} else {
			closedCount++
			logger.Debug("Force-closed connection", "address", addr)
			// Record metric for each force-closed connection
			if s.metrics != nil {
				s.metrics.RecordConnectionForceClosed()
			}
		}

		// Continue iteration
		return true
	})

	if closedCount == 0 {
		logger.Debug("No connections to force-close")
	} else {
		logger.Info("Force-closed connections", "count", closedCount)
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
	// Stop GSS processor if running (releases background cleanup goroutine)
	if s.gssProcessor != nil {
		s.gssProcessor.Stop()
	}

	// Close Kerberos provider (stops keytab hot-reload goroutine)
	if s.kerberosProvider != nil {
		_ = s.kerberosProvider.Close()
	}

	// Always initiate shutdown first
	s.initiateShutdown()

	// If no context provided, use gracefulShutdown with configured timeout
	if ctx == nil {
		return s.gracefulShutdown()
	}

	// Wait for graceful shutdown with context timeout
	activeCount := s.connCount.Load()
	logger.Info("NFS graceful shutdown: waiting for active connections (context timeout)",
		"active", activeCount)

	// Create channel that closes when all connections are done
	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	// Wait for completion or context cancellation
	var err error
	select {
	case <-done:
		logger.Info("NFS graceful shutdown complete: all connections closed")

	case <-ctx.Done():
		remaining := s.connCount.Load()
		logger.Warn("NFS shutdown context cancelled", "active", remaining, "error", ctx.Err())
		err = ctx.Err()
	}

	return err
}
