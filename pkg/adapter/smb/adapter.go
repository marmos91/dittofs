package smb

import (
	"context"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// Adapter implements the adapter.Adapter interface for SMB2 protocol.
//
// This adapter provides an SMB2 server with:
//   - Graceful shutdown with configurable timeout
//   - Connection limiting and resource management
//   - Context-based request cancellation
//   - Configurable timeouts for read/write/idle operations
//   - Thread-safe operation with atomic counters
//
// Architecture:
// Adapter embeds BaseAdapter for shared TCP lifecycle management (listener,
// shutdown, connection tracking, semaphore, metrics logging). Protocol-specific
// behavior (handler registry, session management, signing) stays on the outer
// struct. The ConnectionFactory pattern enables BaseAdapter to create SMB-
// specific connections via NewConnection.
//
// Shutdown flow:
//  1. Context cancelled or Stop() called
//  2. Listener closed (no new connections) [BaseAdapter]
//  3. shutdownCtx cancelled (signals in-flight requests to abort) [BaseAdapter]
//  4. Wait for active connections to complete (up to ShutdownTimeout) [BaseAdapter]
//  5. Force-close any remaining connections after timeout [BaseAdapter]
//
// Thread safety:
// All methods are safe for concurrent use. The shutdown mechanism uses sync.Once
// to ensure idempotent behavior even if Stop() is called multiple times.
type Adapter struct {
	*adapter.BaseAdapter

	// config holds the SMB-specific server configuration (ports, timeouts, limits, credits, signing)
	config Config

	// handler processes SMB2 protocol operations (CREATE, READ, WRITE, etc.)
	handler *handlers.Handler

	// sessionManager provides unified session and credit management
	sessionManager *session.Manager
}

// New creates a new Adapter with the specified configuration.
//
// The adapter is created in a stopped state. Call SetRuntime() to inject
// the Runtime, then call Serve() to start accepting connections.
//
// Configuration:
//   - Zero values in config are replaced with sensible defaults
//   - Invalid configurations cause a panic (indicates programmer error)
//
// Parameters:
//   - config: Server configuration (ports, timeouts, limits)
//
// Returns a configured but not yet started Adapter.
//
// Panics if config validation fails.
func New(config Config) *Adapter {
	// Apply defaults for zero values
	config.applyDefaults()

	// Validate configuration
	if err := config.validate(); err != nil {
		panic(fmt.Sprintf("invalid SMB config: %v", err))
	}

	// Create unified session manager with configured credit strategy
	creditConfig := config.Credits.ToSessionConfig()
	creditStrategy := config.Credits.GetStrategy()
	sessionManager := session.NewManagerWithStrategy(creditStrategy, creditConfig)

	logger.Debug("SMB credit configuration",
		"strategy", config.Credits.Strategy,
		"min_grant", creditConfig.MinGrant,
		"max_grant", creditConfig.MaxGrant,
		"initial_grant", creditConfig.InitialGrant,
		"max_session_credits", creditConfig.MaxSessionCredits)

	// Create handler with session manager
	handler := handlers.NewHandlerWithSessionManager(sessionManager)

	// Apply signing configuration to handler
	handler.SigningConfig = config.Signing.ToSigningConfig()
	logger.Debug("SMB signing configuration",
		"enabled", handler.SigningConfig.Enabled,
		"required", handler.SigningConfig.Required)

	// Build BaseAdapter config from SMB config
	baseConfig := adapter.BaseConfig{
		BindAddress:     config.BindAddress,
		Port:            config.Port,
		MaxConnections:  config.MaxConnections,
		ShutdownTimeout: config.Timeouts.Shutdown,
	}

	return &Adapter{
		BaseAdapter:    adapter.NewBaseAdapter(baseConfig, "SMB"),
		config:         config,
		handler:        handler,
		sessionManager: sessionManager,
	}
}

// SetRuntime injects the runtime containing all stores and shares.
//
// This method is called by Runtime before Serve() is called. The runtime
// provides access to all configured metadata stores, content stores, and shares.
//
// Parameters:
//   - rt: Runtime containing all stores and shares
//
// Thread safety:
// Called exactly once before Serve(), no synchronization needed.
func (s *Adapter) SetRuntime(rtAny any) {
	s.BaseAdapter.SetRuntime(rtAny)
	rt := rtAny.(*runtime.Runtime)
	s.handler.Registry = rt

	// Register OplockManager as the cross-protocol oplock breaker.
	// This allows NFS handlers to trigger oplock breaks on SMB clients
	// without importing the SMB handler package.
	if s.handler != nil && s.handler.OplockManager != nil {
		rt.SetAdapterProvider(adapter.OplockBreakerProviderKey, s.handler.OplockManager)
	}

	// Register share change callback for cache invalidation
	s.handler.RegisterShareChangeCallback()

	// Wire the machine SID mapper for security descriptor operations and
	// lsarpc SID-to-name resolution. The mapper is initialized by the
	// lifecycle service before adapters are started, so it is guaranteed
	// to be available here.
	if mapper := rt.SIDMapper(); mapper != nil {
		handlers.SetSIDMapper(mapper)
		if s.handler != nil && s.handler.PipeManager != nil {
			s.handler.PipeManager.SetSIDMapper(mapper)
		}
		logger.Debug("SMB adapter: machine SID mapper configured",
			"sid", mapper.MachineSIDString())
	}

	logger.Debug("SMB adapter configured with runtime", "shares", rt.CountShares())

	// Apply live SMB adapter settings from SettingsWatcher.
	// The SettingsWatcher polls DB every 10s and provides atomic pointer swap
	// for thread-safe reads. Settings are consumed here at startup and on
	// each new connection (grandfathering per locked decision).
	s.applySMBSettings(rt)
}

// applySMBSettings reads current SMB adapter settings from the runtime's
// SettingsWatcher and applies them. Called during SetRuntime (startup).
func (s *Adapter) applySMBSettings(rt *runtime.Runtime) {
	settings := rt.GetSMBSettings()
	if settings == nil {
		logger.Debug("SMB adapter: no live settings available, using defaults")
		return
	}

	// Encryption stub: log warning when enabled per locked decision
	if settings.EnableEncryption {
		logger.Info("SMB encryption requested but not yet implemented -- connections will proceed without encryption")
	}

	// Dialect range: apply from settings to handler
	if minD, ok := types.ParseSMBDialect(settings.MinDialect); ok {
		s.handler.MinDialect = minD
	} else if settings.MinDialect != "" {
		logger.Warn("SMB adapter: unrecognized MinDialect, using default",
			"value", settings.MinDialect)
	}
	if maxD, ok := types.ParseSMBDialect(settings.MaxDialect); ok {
		s.handler.MaxDialect = maxD
	} else if settings.MaxDialect != "" {
		logger.Warn("SMB adapter: unrecognized MaxDialect, using default",
			"value", settings.MaxDialect)
	}

	// Directory leasing: apply from settings
	s.handler.DirectoryLeasingEnabled = settings.DirectoryLeasingEnabled
	s.handler.EncryptionEnabled = settings.EnableEncryption

	logger.Debug("SMB adapter: dialect range from settings",
		"min_dialect", settings.MinDialect,
		"max_dialect", settings.MaxDialect,
		"encryption", settings.EnableEncryption,
		"directory_leasing", settings.DirectoryLeasingEnabled)

	// Operation blocklist: log active blocks. SMB blocklist is a pass-through
	// that logs unsupported operation names (SMB doesn't have the same per-op
	// granularity as NFS COMPOUND).
	blockedOps := settings.GetBlockedOperations()
	if len(blockedOps) > 0 {
		logger.Info("SMB adapter: operation blocklist from settings (advisory only)",
			"blocked_ops", blockedOps)
	}
}

// Serve starts the SMB server and blocks until the context is cancelled
// or an unrecoverable error occurs.
//
// Serve delegates to BaseAdapter.ServeWithFactory() for the shared TCP accept
// loop, providing SMB-specific connection creation via the ConnectionFactory
// interface and a preAccept hook for live settings max_connections checking.
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
// Serve() should only be called once per Adapter instance.
func (s *Adapter) Serve(ctx context.Context) error {
	return s.ServeWithFactory(ctx, s, s.preAcceptCheck, nil)
}

// preAcceptCheck checks live settings for dynamic max_connections limit.
// Per locked decision: existing connections are grandfathered; only new
// connections are rejected when the live limit is exceeded.
func (s *Adapter) preAcceptCheck(conn net.Conn) bool {
	if s.Registry == nil {
		return true
	}

	liveSettings := s.Registry.GetSMBSettings()
	if liveSettings == nil || liveSettings.MaxConnections <= 0 {
		return true
	}

	currentActive := s.ConnCount.Load()
	if int(currentActive) >= liveSettings.MaxConnections {
		logger.Warn("SMB connection rejected: live settings max_connections exceeded",
			"active", currentActive,
			"max_connections", liveSettings.MaxConnections,
			"client", conn.RemoteAddr())
		return false
	}

	return true
}

// NewConnection creates a protocol-specific connection handler for an accepted
// TCP connection. This implements the adapter.ConnectionFactory interface.
func (s *Adapter) NewConnection(conn net.Conn) adapter.ConnectionHandler {
	return NewConnection(s, conn)
}

// SetKerberosProvider injects the shared Kerberos provider into the SMB handler.
// This enables Kerberos authentication via SPNEGO in SESSION_SETUP.
// Must be called before Serve(). When not called, Kerberos auth is disabled
// and only NTLM/guest authentication is available.
func (s *Adapter) SetKerberosProvider(provider *kerberos.Provider) {
	if provider == nil {
		return
	}
	s.handler.KerberosProvider = provider
	logger.Debug("SMB adapter Kerberos provider configured",
		"principal", provider.ServicePrincipal())
}

// ============================================================================
// Session Reconnection for Grace Period Recovery
// ============================================================================

// OnReconnect is called when an SMB session reconnects after server restart.
//
// During the grace period, this method triggers lease reclaim for all leases
// the client previously held. This allows SMB clients to restore their caching
// state after server restart, maintaining cache consistency.
//
// Parameters:
//   - ctx: Context for cancellation
//   - sessionID: The reconnecting session ID
//   - clientID: The connection tracker client ID
//
// Implementation note: This is a minimal implementation for gap closure.
// Full implementation would enumerate persisted leases for the session and
// call HandleLeaseReclaim for each one. Currently, the reclaim happens
// implicitly when the client requests the same lease key during grace period.
func (s *Adapter) OnReconnect(ctx context.Context, sessionID uint64, clientID string) {
	logger.Info("SMB session reconnected",
		"sessionID", sessionID,
		"clientID", clientID)

	// During grace period, leases will be reclaimed when the client
	// makes a CREATE request with its known lease key.
	// The RequestLeaseWithReclaim method handles this transparently.
	//
	// A full implementation would:
	// 1. Query LockStore for all leases owned by this clientID
	// 2. Prepare them for reclaim on first access
	// 3. Notify client of available leases to reclaim
	//
	// For this gap closure, we rely on implicit reclaim in RequestLeaseWithReclaim.
}
