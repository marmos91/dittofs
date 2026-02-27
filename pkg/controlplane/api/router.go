package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/internal/controlplane/api/handlers"
	apiMiddleware "github.com/marmos91/dittofs/internal/controlplane/api/middleware"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
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
//   - /api/v1/groups/* - Group management (admin only)
//   - /api/v1/shares/* - Share management (admin only)
//   - /api/v1/metadata-stores/* - Metadata store management (admin only)
//   - /api/v1/payload-stores/* - Payload store management (admin only)
//   - GET /api/v1/adapters - Adapter list (admin + operator)
//   - /api/v1/adapters/* - Adapter management (admin only)
//   - /api/v1/adapters/{type}/settings - Adapter settings (admin only)
//   - /api/v1/adapters/{type}/clients - NFS client management (admin only)
//   - /api/v1/adapters/{type}/clients/{id}/sessions - NFS client sessions (admin only)
//   - /api/v1/adapters/{type}/grace - NFS grace period management (admin only)
//   - /api/v1/adapters/{type}/netgroups - NFS netgroup management (admin only)
//   - /api/v1/adapters/{type}/identity-mappings - NFS identity mapping management (admin only)
//   - /api/v1/adapters/{type}/mounts - Protocol-specific mount listing (admin only)
//   - /api/v1/settings/* - System settings management (admin only)
//   - /api/v1/mounts - Unified mount listing (admin only)
func NewRouter(rt *runtime.Runtime, jwtService *auth.JWTService, cpStore store.Store) http.Handler {
	r := chi.NewRouter()

	// Middleware stack - order matters
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// Health check handlers
	healthHandler := handlers.NewHealthHandler(rt)

	// Health routes - unauthenticated
	r.Route("/health", func(r chi.Router) {
		r.Get("/", healthHandler.Liveness)
		r.Get("/ready", healthHandler.Readiness)
		r.Get("/stores", healthHandler.Stores)
	})

	// Grace period status - unauthenticated (like health probes)
	if graceHandler := newGraceHandler(rt); graceHandler != nil {
		r.Get("/api/v1/grace", graceHandler.Status)
	}

	// Root redirect to health for convenience
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/health", http.StatusTemporaryRedirect)
	})

	// API handlers - use cpStore directly since API handlers have request context
	authHandler := handlers.NewAuthHandler(cpStore, jwtService)
	userHandler, err := handlers.NewUserHandler(cpStore, jwtService)
	if err != nil {
		// This is a programming error - jwtService should always be provided
		panic("failed to create user handler: " + err.Error())
	}

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

			// User management
			r.Route("/users", func(r chi.Router) {
				// Self-access allowed - handler does its own authorization
				r.Get("/{username}", userHandler.Get)

				// Admin-only operations
				r.Group(func(r chi.Router) {
					r.Use(apiMiddleware.RequireAdmin())

					r.Post("/", userHandler.Create)
					r.Get("/", userHandler.List)
					r.Put("/{username}", userHandler.Update)
					r.Delete("/{username}", userHandler.Delete)
					r.Post("/{username}/password", userHandler.ResetPassword)
				})
			})

			// Group management (admin only)
			r.Route("/groups", func(r chi.Router) {
				r.Use(apiMiddleware.RequireAdmin())

				groupHandler := handlers.NewGroupHandler(cpStore)
				r.Post("/", groupHandler.Create)
				r.Get("/", groupHandler.List)
				r.Get("/{name}", groupHandler.Get)
				r.Put("/{name}", groupHandler.Update)
				r.Delete("/{name}", groupHandler.Delete)

				// Group members
				r.Get("/{name}/members", groupHandler.ListMembers)
				r.Post("/{name}/members", groupHandler.AddMember)
				r.Delete("/{name}/members/{username}", groupHandler.RemoveMember)
			})

			// Share management (admin only)
			r.Route("/shares", func(r chi.Router) {
				r.Use(apiMiddleware.RequireAdmin())

				shareHandler := handlers.NewShareHandler(cpStore, rt)
				r.Post("/", shareHandler.Create)
				r.Get("/", shareHandler.List)
				r.Get("/{name}", shareHandler.Get)
				r.Put("/{name}", shareHandler.Update)
				r.Delete("/{name}", shareHandler.Delete)

				// Share permissions
				r.Get("/{name}/permissions", shareHandler.ListPermissions)
				r.Put("/{name}/permissions/users/{username}", shareHandler.SetUserPermission)
				r.Delete("/{name}/permissions/users/{username}", shareHandler.DeleteUserPermission)
				r.Put("/{name}/permissions/groups/{groupname}", shareHandler.SetGroupPermission)
				r.Delete("/{name}/permissions/groups/{groupname}", shareHandler.DeleteGroupPermission)
			})

			// Metadata store management (admin only)
			r.Route("/metadata-stores", func(r chi.Router) {
				r.Use(apiMiddleware.RequireAdmin())

				metadataStoreHandler := handlers.NewMetadataStoreHandler(cpStore, rt)
				r.Post("/", metadataStoreHandler.Create)
				r.Get("/", metadataStoreHandler.List)
				r.Get("/{name}", metadataStoreHandler.Get)
				r.Put("/{name}", metadataStoreHandler.Update)
				r.Delete("/{name}", metadataStoreHandler.Delete)
			})

			// Payload store management (admin only)
			r.Route("/payload-stores", func(r chi.Router) {
				r.Use(apiMiddleware.RequireAdmin())

				payloadStoreHandler := handlers.NewPayloadStoreHandler(cpStore)
				r.Post("/", payloadStoreHandler.Create)
				r.Get("/", payloadStoreHandler.List)
				r.Get("/{name}", payloadStoreHandler.Get)
				r.Put("/{name}", payloadStoreHandler.Update)
				r.Delete("/{name}", payloadStoreHandler.Delete)
			})

			// Adapter configuration - split read/write access
			//
			// All adapter routes (generic CRUD, settings, and protocol-specific)
			// are nested under /adapters/{type} to avoid chi routing conflicts.
			// Previously, separate /adapters/nfs and /adapters/smb route groups
			// shadowed the /{type} parameterized routes for those adapter types.
			r.Route("/adapters", func(r chi.Router) {
				adapterHandler := handlers.NewAdapterHandler(rt)
				settingsHandler := handlers.NewAdapterSettingsHandler(cpStore, rt)
				mountHandler := handlers.NewMountHandler(rt)

				// Read endpoint: admin + operator (list only)
				r.Group(func(r chi.Router) {
					r.Use(apiMiddleware.RequireRole("admin", "operator"))
					r.Get("/", adapterHandler.List)
				})

				// Write endpoints + individual get: admin only
				r.Group(func(r chi.Router) {
					r.Use(apiMiddleware.RequireAdmin())
					r.Post("/", adapterHandler.Create)

					// All type-scoped routes under /{type}
					r.Route("/{type}", func(r chi.Router) {
						// Adapter CRUD
						r.Get("/", adapterHandler.Get)
						r.Put("/", adapterHandler.Update)
						r.Delete("/", adapterHandler.Delete)

						// Adapter settings
						r.Route("/settings", func(r chi.Router) {
							r.Get("/", settingsHandler.GetSettings)
							r.Put("/", settingsHandler.PutSettings)
							r.Patch("/", settingsHandler.PatchSettings)
							r.Get("/defaults", settingsHandler.GetDefaults)
							r.Post("/reset", settingsHandler.ResetSettings)
						})

						// Protocol-specific mount listing
						r.Get("/mounts", func(w http.ResponseWriter, req *http.Request) {
							adapterType := chi.URLParam(req, "type")
							mountHandler.ListByProtocol(adapterType)(w, req)
						})

						// NFS client management (handlers are nil if NFS state not available)
						if clientHandler := newClientHandler(rt); clientHandler != nil {
							r.Route("/clients", func(r chi.Router) {
								r.Get("/", clientHandler.List)
								r.Delete("/{id}", clientHandler.Evict)
								r.Route("/{id}/sessions", func(r chi.Router) {
									r.Get("/", clientHandler.ListSessions)
									r.Delete("/{sid}", clientHandler.ForceDestroySession)
								})
							})
						}

						// NFS grace period management
						if graceHandler := newGraceHandler(rt); graceHandler != nil {
							r.Route("/grace", func(r chi.Router) {
								r.Post("/end", graceHandler.ForceEnd)
							})
						}

						// NFS netgroup management - requires NetgroupStore capability
						if ns, ok := cpStore.(store.NetgroupStore); ok {
							r.Route("/netgroups", func(r chi.Router) {
								netgroupHandler := handlers.NewNetgroupHandler(ns)
								r.Post("/", netgroupHandler.Create)
								r.Get("/", netgroupHandler.List)
								r.Get("/{name}", netgroupHandler.Get)
								r.Delete("/{name}", netgroupHandler.Delete)
								r.Post("/{name}/members", netgroupHandler.AddMember)
								r.Delete("/{name}/members/{id}", netgroupHandler.RemoveMember)
							})
						}

						// NFS identity mapping management - requires IdentityMappingStore capability
						if ims, ok := cpStore.(store.IdentityMappingStore); ok {
							r.Route("/identity-mappings", func(r chi.Router) {
								idmapHandler := handlers.NewIdentityMappingHandler(ims)
								r.Get("/", idmapHandler.List)
								r.Post("/", idmapHandler.Create)
								r.Delete("/{principal}", idmapHandler.Delete)
							})
						}
					})
				})
			})

			// System settings (admin only)
			r.Route("/settings", func(r chi.Router) {
				r.Use(apiMiddleware.RequireAdmin())

				settingsHandler := handlers.NewSettingsHandler(cpStore)
				r.Get("/", settingsHandler.List)
				r.Get("/{key}", settingsHandler.Get)
				r.Put("/{key}", settingsHandler.Set)
				r.Delete("/{key}", settingsHandler.Delete)
			})

			// Unified mount listing (admin only) - all protocols
			r.Route("/mounts", func(r chi.Router) {
				r.Use(apiMiddleware.RequireAdmin())
				mountHandler := handlers.NewMountHandler(rt)
				r.Get("/", mountHandler.List)
			})
		})
	})

	return r
}

// newClientHandler returns a ClientHandler if an NFS adapter with state management is configured.
func newClientHandler(rt *runtime.Runtime) *handlers.ClientHandler {
	if rt == nil {
		return nil
	}
	return handlers.NewClientHandlerFromProvider(rt.NFSClientProvider())
}

// newGraceHandler returns a GraceHandler if an NFS adapter with state management is configured.
func newGraceHandler(rt *runtime.Runtime) *handlers.GraceHandler {
	if rt == nil {
		return nil
	}
	return handlers.NewGraceHandlerFromProvider(rt.NFSClientProvider())
}

// isHealthPath returns true if the request path is a healthcheck endpoint.
func isHealthPath(path string) bool {
	return path == "/health" || strings.HasPrefix(path, "/health/")
}

// requestLogger is a custom middleware that logs requests using the internal logger.
//
// It logs:
//   - Request start (DEBUG level): method, path, remote addr
//   - Request completion (INFO level): method, path, status, duration
//   - Healthcheck requests are logged at DEBUG level to reduce noise
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

		logArgs := []any{
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration", duration.String(),
		}

		// Log healthcheck requests at DEBUG to avoid polluting logs in k8s
		if isHealthPath(r.URL.Path) {
			logger.Debug("API request completed", logArgs...)
		} else {
			logger.Info("API request completed", logArgs...)
		}
	})
}
