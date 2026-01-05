package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/registry"
)

// Server provides an HTTP server for the REST API.
//
// The server exposes health check endpoints and will be extended
// with management APIs in future phases.
//
// Endpoints:
//   - GET /health: Liveness probe
//   - GET /health/ready: Readiness probe
//   - GET /health/stores: Detailed store health
//
// The server supports graceful shutdown with configurable timeout.
type Server struct {
	server       *http.Server
	registry     *registry.Registry
	config       APIConfig
	shutdownOnce sync.Once
}

// NewServer creates a new API HTTP server.
//
// The server is created in a stopped state. Call Start() to begin serving requests.
//
// Defaults are applied here to ensure the server works correctly even when
// created directly (e.g., in tests). This is idempotent with the defaults
// applied during config loading.
//
// Parameters:
//   - config: Server configuration (port, timeouts)
//   - registry: Registry for store health checks (may be nil for basic health only)
//
// Returns a configured but not yet started Server.
func NewServer(config APIConfig, registry *registry.Registry) *Server {
	config.applyDefaults()

	router := NewRouter(registry)

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", config.Port),
		Handler:      router,
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
		IdleTimeout:  config.IdleTimeout,
	}

	return &Server{
		server:   server,
		registry: registry,
		config:   config,
	}
}

// Start starts the API HTTP server and blocks until the context is cancelled
// or an error occurs.
//
// The server listens on the configured port and serves API endpoints.
//
// When the context is cancelled, Start initiates graceful shutdown and returns.
//
// Parameters:
//   - ctx: Controls the server lifecycle. Cancellation triggers graceful shutdown.
//
// Returns:
//   - nil on graceful shutdown
//   - error if the server fails to start or shutdown encounters an error
func (s *Server) Start(ctx context.Context) error {
	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		logger.Info("API server listening", "port", s.config.Port)
		logger.Debug("API endpoints available",
			"health", fmt.Sprintf("http://localhost:%d/health", s.config.Port),
			"ready", fmt.Sprintf("http://localhost:%d/health/ready", s.config.Port),
			"stores", fmt.Sprintf("http://localhost:%d/health/stores", s.config.Port),
		)

		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			select {
			case errChan <- err:
			default:
				// Context was cancelled, error is not needed
			}
		}
	}()

	// Wait for context cancellation or server error
	select {
	case <-ctx.Done():
		logger.Info("API server shutdown signal received")
		// Create new context with timeout for graceful shutdown
		// Don't use the cancelled ctx as it would cause immediate shutdown
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.Stop(shutdownCtx)
	case err := <-errChan:
		return fmt.Errorf("API server failed: %w", err)
	}
}

// Stop initiates graceful shutdown of the API server.
//
// Stop is safe to call multiple times and safe to call concurrently with Start().
//
// Parameters:
//   - ctx: Controls the shutdown timeout. If cancelled, shutdown aborts immediately.
//
// Returns:
//   - nil on successful shutdown
//   - error if shutdown fails or times out
func (s *Server) Stop(ctx context.Context) error {
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		logger.Debug("API server shutdown initiated")

		if err := s.server.Shutdown(ctx); err != nil {
			shutdownErr = fmt.Errorf("API server shutdown error: %w", err)
			logger.Error("API server shutdown error", "error", err)
		} else {
			logger.Info("API server stopped gracefully")
		}
	})
	return shutdownErr
}

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int {
	return s.config.Port
}
