package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	goruntime "runtime"
	"strconv"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/tlsconfig"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/netutil"
)

// DefaultRestoreHTTPTimeout is the fallback context bound applied to the
// REST snapshot-restore handler when callers do not supply an explicit
// timeout. The dfs binary sources this from config.SnapshotConfig.
const DefaultRestoreHTTPTimeout = 30 * time.Minute

// defaultDrainTimeout bounds how long Stop waits for in-flight API handlers
// after http.Server.Shutdown has returned. Shutdown returns when its own
// deadline expires whether or not the handlers behind it have finished, and the
// caller closes the control-plane store next, so this wait is the only thing
// that orders that close after a request still using the store.
const defaultDrainTimeout = 5 * time.Second

// Timeouts bundles the per-handler budgets for long-running control-plane
// operations that must outlive the global HTTP request timeout
// (middleware.Timeout / write_timeout, ~30s). They are assembled by the caller
// from the config sections that own each one (see cmd/dfs start), keeping the
// constructor signatures stable as more such operations are added.
type Timeouts struct {
	// Restore is the total wall-clock budget for a snapshot restore. Zero
	// falls back to the snapshot handler's default.
	Restore time.Duration
	// DrainStall is the inactivity budget for POST /system/drain-uploads — an
	// idle timeout, not a wall-clock cap. Zero falls back to the system
	// handler's DefaultDrainStallTimeout.
	DrainStall time.Duration
}

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

	// inflight counts requests currently inside the handler chain. Stop waits
	// on it (bounded by drainTimeout) after Shutdown returns, so a request that
	// outlived the server's own deadline is joined before the caller closes the
	// control-plane store the handlers read directly.
	inflight sync.WaitGroup
	// drainMu guards draining and makes admission atomic with the counter: a
	// request takes it for read, checks draining, and only then Adds. Drain takes
	// it for write to set draining before Waiting, so no Add can start after Wait
	// begins (a prohibited WaitGroup use) and no request admitted after that
	// point reaches a handler.
	drainMu  sync.RWMutex
	draining bool
	// drainTimeout overrides defaultDrainTimeout; tests shorten it.
	drainTimeout time.Duration
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
func NewServer(config APIConfig, rt *runtime.Runtime, cpStore store.Store, timeouts Timeouts) (*Server, error) {
	config.ApplyDefaults()

	// Fail fast on internally inconsistent TLS settings (cert without key, etc.)
	// and on a missing or too-short JWT signing secret. `dfs start` runs the
	// same check before it opens the control-plane database, so in the daemon
	// this is a backstop rather than the first line of defence.
	if err := config.ValidateStartup(); err != nil {
		return nil, err
	}

	// Build the TLS config (loads + parses the cert files now, so a bad cert
	// path fails at startup rather than on the first handshake). nil when TLS
	// is not configured, in which case the server serves plain HTTP.
	tlsConfig, err := tlsconfig.ServerConfig(config.TLS.shared())
	if err != nil {
		return nil, fmt.Errorf("failed to configure TLS: %w", err)
	}

	// Default-posture nudge: a non-loopback bind with no native TLS means
	// login credentials and JWTs cross the network in cleartext unless an edge
	// terminator (ingress / mesh / reverse proxy) wraps them. We do NOT
	// hard-require TLS (back-compat: many deployments terminate at the edge),
	// so this is a startup WARN pointing at the TLS / external-termination docs
	// rather than a fatal error.
	if tlsConfig == nil && netutil.IsNonLoopbackHost(config.Host) {
		logger.Warn("control plane API is bound to a non-loopback address with TLS disabled; "+
			"login credentials and tokens will traverse the network in CLEARTEXT. "+
			"Configure controlplane.tls.{cert_file,key_file} for native TLS, or terminate TLS at an ingress/mesh. "+
			"See docs/SECURITY.md.",
			"host", config.Host)
	}

	// Get JWT secret from config (prefers env var). Length already checked above.
	jwtSecret := config.GetJWTSecret()

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
	router := NewRouter(rt, jwtService, cpStore, config.Pprof, timeouts)

	srv := &Server{
		tlsConfig:    tlsConfig,
		runtime:      rt,
		jwtService:   jwtService,
		cpStore:      cpStore,
		config:       config,
		drainTimeout: defaultDrainTimeout,
	}

	writeTimeout := config.WriteTimeout
	if config.Pprof && writeTimeout < 120*time.Second {
		writeTimeout = 120 * time.Second
	}

	server := &http.Server{
		Addr:         net.JoinHostPort(config.Host, strconv.Itoa(config.Port)),
		Handler:      srv.trackInflight(router),
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  config.IdleTimeout,
		TLSConfig:    tlsConfig,
	}
	srv.server = server

	return srv, nil
}

// trackInflight wraps a handler so every request in flight is counted for the
// duration of the handler chain, and refuses requests once draining has begun.
// It is the outermost wrapper, so a request is counted for as long as anything
// downstream (middleware included) is still running.
//
// The admission check and the counter increment are one critical section: a
// request that saw draining=false has already Added before Drain can set
// draining and Wait, so the WaitGroup contract holds and the request is joined.
// A request that arrives after draining is set is refused here rather than
// dispatched into a handler that would touch the closing store.
func (s *Server) trackInflight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.drainMu.RLock()
		if s.draining {
			s.drainMu.RUnlock()
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		s.inflight.Add(1)
		s.drainMu.RUnlock()
		defer s.inflight.Done()
		next.ServeHTTP(w, r)
	})
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

		// Stop admitting before Shutdown, so a request the still-open listener
		// accepts during Shutdown's own window is refused rather than counted —
		// the same gate the caller's Drain relies on.
		s.drainMu.Lock()
		s.draining = true
		s.drainMu.Unlock()

		if err := s.server.Shutdown(ctx); err != nil {
			shutdownErr = fmt.Errorf("API server shutdown error: %w", err)
			logger.Error("API server shutdown error", "error", err)
		} else {
			logger.Info("API server stopped gracefully")
		}

		// Shutdown returns on its own deadline whether or not the handlers
		// behind it returned. The caller closes the control-plane store after
		// this returns, and handlers read that store directly, so wait (bounded)
		// for the stragglers before reporting. Losing this bound means the store
		// closes under a request still using it — the same shape as before, but
		// now it takes a handler that outlasted both deadlines rather than any
		// slow one.
		drainCtx, cancel := context.WithTimeout(context.Background(), s.drainTimeout)
		defer cancel()
		if !s.Drain(drainCtx) {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("in-flight API request did not finish within %s", s.drainTimeout))
			logger.Error("API server drain did not complete", "timeout", s.drainTimeout)
		}
	})
	return shutdownErr
}

// Drain stops admitting new requests and waits for the ones already counted to
// finish, returning true when they all have and false when ctx expires first.
// It is the fence that lets the caller close a resource the handlers read
// directly (the control-plane store) only after they are done with it, and it
// is separate from Stop so the caller can apply it on a path where Stop never
// ran — the forced-exit branch of the daemon's shutdown wait returns while
// Serve, and with it Stop, may still be in flight.
//
// Setting draining first is what makes the wait safe on that path: without it a
// request accepted by the still-open listener could Add after Wait began, which
// both breaks the WaitGroup contract and lets that request reach the store after
// this returned. Once draining is set the listener refuses such a request, so a
// false result means only that a request already in flight outlasted ctx.
func (s *Server) Drain(ctx context.Context) bool {
	s.drainMu.Lock()
	s.draining = true
	s.drainMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.inflight.Wait()
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
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
