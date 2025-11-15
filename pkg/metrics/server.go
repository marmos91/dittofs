package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server provides an HTTP server for exposing Prometheus metrics.
//
// The server exposes the following endpoints:
//   - GET /metrics: Prometheus metrics in text format
//   - GET /: Simple index page with link to /metrics
//
// The server supports graceful shutdown with configurable timeout.
type Server struct {
	server       *http.Server
	port         int
	shutdownOnce sync.Once
}

// ServerConfig configures the metrics HTTP server.
type ServerConfig struct {
	// Port to listen on for HTTP requests.
	// Default: 9090
	Port int
}

// applyDefaults fills in zero values with sensible defaults.
func (c *ServerConfig) applyDefaults() {
	if c.Port <= 0 {
		c.Port = 9090
	}
}

// NewServer creates a new metrics HTTP server.
//
// The server is created in a stopped state. Call Start() to begin serving requests.
//
// Parameters:
//   - config: Server configuration (port, timeouts)
//
// Returns a configured but not yet started Server.
func NewServer(config ServerConfig) *Server {
	config.applyDefaults()

	mux := http.NewServeMux()

	// Prometheus metrics endpoint
	if IsEnabled() {
		registry := GetRegistry()
		if registry != nil {
			mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
				EnableOpenMetrics: true,
			}))
			logger.Debug("Metrics endpoint registered at /metrics")
		}
	} else {
		// Metrics disabled - return helpful message
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "Metrics collection is disabled\n")
		})
		logger.Debug("Metrics collection disabled")
	}

	// Simple index page
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
    <title>DittoFS Metrics</title>
    <style>
        body { font-family: sans-serif; max-width: 800px; margin: 50px auto; padding: 20px; }
        h1 { color: #333; }
        a { color: #0066cc; text-decoration: none; }
        a:hover { text-decoration: underline; }
        .info { background: #f0f0f0; padding: 15px; border-radius: 5px; margin: 20px 0; }
    </style>
</head>
<body>
    <h1>DittoFS Metrics Server</h1>
    <div class="info">
        <p><strong>Metrics Endpoint:</strong> <a href="/metrics">/metrics</a></p>
        <p>Prometheus metrics in text format for scraping.</p>
    </div>
    <h2>About</h2>
    <p>This server exposes Prometheus metrics for DittoFS monitoring and observability.</p>
    <p>Configure your Prometheus server to scrape <code>http://&lt;host&gt;:%d/metrics</code></p>
</body>
</html>`, config.Port)
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", config.Port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return &Server{
		server: server,
		port:   config.Port,
	}
}

// Start starts the metrics HTTP server and blocks until the context is cancelled
// or an error occurs.
//
// The server listens on the configured port and serves metrics at /metrics.
//
// When the context is cancelled, Start initiates graceful shutdown and returns.
//
// Parameters:
//   - ctx: Controls the server lifecycle. Cancellation triggers graceful shutdown.
//
// Returns:
//   - nil on graceful shutdown
//   - http.ErrServerClosed on normal shutdown
//   - error if the server fails to start or shutdown encounters an error
func (s *Server) Start(ctx context.Context) error {
	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		logger.Info("Metrics server listening on port %d", s.port)
		logger.Debug("Metrics endpoint available at http://localhost:%d/metrics", s.port)

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
		logger.Info("Metrics server shutdown signal received")
		return s.Stop(ctx)
	case err := <-errChan:
		return fmt.Errorf("metrics server failed: %w", err)
	}
}

// Stop initiates graceful shutdown of the metrics server.
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
		logger.Debug("Metrics server shutdown initiated")

		if err := s.server.Shutdown(ctx); err != nil {
			shutdownErr = fmt.Errorf("metrics server shutdown error: %w", err)
			logger.Error("Metrics server shutdown error: %v", err)
		} else {
			logger.Info("Metrics server stopped gracefully")
		}
	})
	return shutdownErr
}

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int {
	return s.port
}
