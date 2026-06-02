package api

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	goruntime "runtime"
	"strconv"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// DefaultRestoreHTTPTimeout is the fallback context bound applied to the
// REST snapshot-restore handler when callers do not supply an explicit
// timeout. The dfs binary sources this from config.SnapshotConfig.
const DefaultRestoreHTTPTimeout = 30 * time.Minute

// Server provides an HTTP server for the REST API.
//
// The server exposes health check endpoints and authentication APIs.
//
// Endpoints:
//   - GET /health: Liveness probe
//   - GET /health/ready: Readiness probe
//   - POST /api/v1/auth/login: User authentication
//   - POST /api/v1/auth/refresh: Token refresh
//   - GET /api/v1/auth/me: Current user info
//   - /api/v1/users/*: User management (admin only)
//   - /api/v1/groups/*: Group management (admin only)
//
// The server supports graceful shutdown with configurable timeout.
type Server struct {
	server       *http.Server
	tlsConfig    *tls.Config // non-nil when TLS is configured; nil = plain HTTP
	runtime      *runtime.Runtime
	jwtService   *auth.JWTService
	cpStore      store.Store
	config       APIConfig
	shutdownOnce sync.Once
}

// NewServer creates a new API HTTP server.
//
// The server is created in a stopped state. Call Start() to begin serving requests.
//
// The JWT service is created internally from the config. The JWT secret must be
// configured via config.JWT.Secret or the DITTOFS_CONTROLPLANE_SECRET environment variable.
//
// Parameters:
//   - config: Server configuration (port, timeouts, JWT config)
//   - rt: Runtime for store health checks (may be nil for basic health only)
//   - cpStore: Control plane store for user/group management
//
// Returns a configured but not yet started Server, or an error if JWT configuration is invalid.
func NewServer(config APIConfig, rt *runtime.Runtime, cpStore store.Store, restoreHTTPTimeout time.Duration) (*Server, error) {
	config.ApplyDefaults()

	// Fail fast on internally inconsistent TLS settings (cert without key, etc.).
	if err := config.Validate(); err != nil {
		return nil, err
	}

	// Build the TLS config (loads + parses the cert files now, so a bad cert
	// path fails at startup rather than on the first handshake). nil when TLS
	// is not configured, in which case the server serves plain HTTP.
	tlsConfig, err := buildTLSConfig(config.TLS)
	if err != nil {
		return nil, fmt.Errorf("failed to configure TLS: %w", err)
	}

	// Get JWT secret from config (prefers env var)
	jwtSecret := config.GetJWTSecret()
	if len(jwtSecret) < 32 {
		return nil, fmt.Errorf("JWT secret must be at least 32 characters; set via %s env var or config", EnvControlPlaneSecret)
	}

	// Create JWT service internally
	jwtConfig := auth.JWTConfig{
		Secret:               jwtSecret,
		Issuer:               "dittofs",
		AccessTokenDuration:  config.JWT.AccessTokenDuration,
		RefreshTokenDuration: config.JWT.RefreshTokenDuration,
	}
	jwtService, err := auth.NewJWTService(jwtConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create JWT service: %w", err)
	}

	// Set mutex/block sampling so /debug/pprof/{mutex,block} return non-empty
	// profiles. The HTTP handlers only read the runtime's sampled data; without
	// these calls the sampling rate stays 0 and the profiles are header-only.
	// These are process-global runtime knobs, so NewServer is authoritative in
	// both directions: when Pprof is off we reset them to 0 rather than leaving
	// whatever a prior caller (another server, bench tooling) set — that keeps
	// "no sampling overhead when pprof is off" true regardless of call order.
	if config.Pprof {
		goruntime.SetMutexProfileFraction(config.PprofMutexRate)
		goruntime.SetBlockProfileRate(config.PprofBlockRateNs)
		logger.Info("pprof mutex/block sampling enabled",
			"mutex_rate", config.PprofMutexRate, "block_rate_ns", config.PprofBlockRateNs)
	} else {
		goruntime.SetMutexProfileFraction(0)
		goruntime.SetBlockProfileRate(0)
	}

	// cpStore implements both IdentityStore and Store
	router := NewRouter(rt, jwtService, cpStore, config.Pprof, restoreHTTPTimeout)

	writeTimeout := config.WriteTimeout
	if config.Pprof && writeTimeout < 120*time.Second {
		writeTimeout = 120 * time.Second
	}

	server := &http.Server{
		Addr:         net.JoinHostPort(config.Host, strconv.Itoa(config.Port)),
		Handler:      router,
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  config.IdleTimeout,
		TLSConfig:    tlsConfig,
	}

	return &Server{
		server:     server,
		tlsConfig:  tlsConfig,
		runtime:    rt,
		jwtService: jwtService,
		cpStore:    cpStore,
		config:     config,
	}, nil
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
		scheme := "http"
		if s.tlsConfig != nil {
			scheme = "https"
		}
		logger.Info("API server listening", "addr", s.server.Addr, "scheme", scheme, "mtls", s.mTLSEnabled())
		// Build a routable base for the example URLs: a wildcard bind
		// (0.0.0.0 / ::) is not a destination, so show localhost there.
		urlBase := displayAddr(s.config.Host, s.config.Port)
		logger.Debug("API endpoints available",
			"health", fmt.Sprintf("%s://%s/health", scheme, urlBase),
			"ready", fmt.Sprintf("%s://%s/health/ready", scheme, urlBase),
		)

		var err error
		if s.tlsConfig != nil {
			// Certificates are supplied via tlsConfig.GetCertificate, so the
			// cert/key path arguments are intentionally empty here.
			err = s.server.ListenAndServeTLS("", "")
		} else {
			err = s.server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
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

// Handler returns the underlying HTTP handler for the API server.
//
// This allows external consumers to mount the DittoFS API routes in their own
// HTTP servers rather than running a separate listener. The returned handler
// includes all configured middleware (auth, logging, recovery, etc.).
func (s *Server) Handler() http.Handler {
	return s.server.Handler
}

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int {
	return s.config.Port
}

// TLSEnabled reports whether the server is serving HTTPS.
func (s *Server) TLSEnabled() bool {
	return s.tlsConfig != nil
}

// mTLSEnabled reports whether the server requires verified client certificates.
func (s *Server) mTLSEnabled() bool {
	return s.tlsConfig != nil && s.tlsConfig.ClientAuth == tls.RequireAndVerifyClientCert
}

// displayAddr returns a host:port that is routable for example/log URLs. A
// wildcard bind (0.0.0.0, ::, or empty) is a listen address, not a
// destination, so it is shown as localhost.
func displayAddr(host string, port int) string {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
