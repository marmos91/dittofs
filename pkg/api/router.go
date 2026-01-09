package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/api/auth"
	"github.com/marmos91/dittofs/pkg/api/handlers"
	apiMiddleware "github.com/marmos91/dittofs/pkg/api/middleware"
	"github.com/marmos91/dittofs/pkg/identity"
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
//   - POST /api/v1/auth/login - User authentication
//   - POST /api/v1/auth/refresh - Token refresh
//   - GET /api/v1/auth/me - Current user info
//   - POST /api/v1/users/me/password - Change own password
//   - /api/v1/users/* - User management (admin only)
func NewRouter(reg *registry.Registry, jwtService *auth.JWTService, identityStore identity.IdentityStore) http.Handler {
	r := chi.NewRouter()

	// Middleware stack - order matters
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// Health check handlers
	healthHandler := handlers.NewHealthHandler(reg)

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

	// API handlers
	authHandler := handlers.NewAuthHandler(identityStore, jwtService)
	userHandler := handlers.NewUserHandler(identityStore)
	mappingHandler := handlers.NewShareMappingHandler(identityStore)

	// API v1 routes
	r.Route("/api/v1", func(r chi.Router) {
		// Auth routes - mostly unauthenticated
		r.Route("/auth", func(r chi.Router) {
			// Public endpoints
			r.Post("/login", authHandler.Login)
			r.Post("/refresh", authHandler.Refresh)

			// Authenticated endpoint
			r.Group(func(r chi.Router) {
				r.Use(apiMiddleware.JWTAuth(jwtService))
				r.Get("/me", authHandler.Me)
			})
		})

		// Password change - authenticated but exempt from MustChangePassword check
		// This allows users who must change their password to actually change it
		r.Route("/users/me/password", func(r chi.Router) {
			r.Use(apiMiddleware.JWTAuth(jwtService))
			r.Post("/", userHandler.ChangeOwnPassword)
		})

		// Protected routes - require authentication and password change complete
		r.Group(func(r chi.Router) {
			r.Use(apiMiddleware.JWTAuth(jwtService))
			r.Use(apiMiddleware.RequirePasswordChange("/api/v1/users/me/password"))

			// User management (admin only)
			r.Route("/users", func(r chi.Router) {
				r.Use(apiMiddleware.RequireAdmin())

				r.Post("/", userHandler.Create)
				r.Get("/", userHandler.List)
				r.Get("/{username}", userHandler.Get)
				r.Put("/{username}", userHandler.Update)
				r.Delete("/{username}", userHandler.Delete)
				r.Post("/{username}/password", userHandler.ResetPassword)

				// Share identity mappings
				r.Get("/{username}/shares", mappingHandler.List)
				r.Get("/{username}/shares/{share}", mappingHandler.Get)
				r.Put("/{username}/shares/{share}", mappingHandler.Set)
				r.Delete("/{username}/shares/{share}", mappingHandler.Delete)
			})
		})
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
