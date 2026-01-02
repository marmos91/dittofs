package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/api/handlers"
	"github.com/marmos91/dittofs/pkg/registry"
)

// NewRouter creates and configures the chi router with all middleware and routes.
//
// The router is configured with:
//   - Request ID middleware for request tracking
//   - Real IP extraction for proper client identification
//   - Custom request logging using the internal logger
//   - Panic recovery to prevent server crashes
//   - Request timeout to prevent hung requests
//
// Routes:
//   - GET /health - Liveness probe
//   - GET /health/ready - Readiness probe
//   - GET /health/stores - Detailed store health
func NewRouter(registry *registry.Registry) http.Handler {
	r := chi.NewRouter()

	// Middleware stack - order matters
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// Health check handlers
	healthHandler := handlers.NewHealthHandler(registry)

	// Health routes - unauthenticated
	r.Route("/health", func(r chi.Router) {
		r.Get("/", healthHandler.Liveness)
		r.Get("/ready", healthHandler.Readiness)
		r.Get("/stores", healthHandler.Stores)
	})

	// Root redirect to health for convenience
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/health", http.StatusTemporaryRedirect)
	})

	return r
}

// requestLogger is a custom middleware that logs requests using the internal logger.
//
// It logs:
//   - Request start (DEBUG level): method, path, remote addr
//   - Request completion (INFO level): method, path, status, duration
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := middleware.GetReqID(r.Context())

		logger.Debug("API request started",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
		)

		// Wrap response writer to capture status code
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		duration := time.Since(start)

		logger.Info("API request completed",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration", duration.String(),
		)
	})
}
