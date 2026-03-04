package smb

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	smblease "github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	authkerberos "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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

	// Apply encryption configuration to handler
	handler.EncryptionConfig = handlers.EncryptionConfig{
		Mode:           config.Encryption.Mode,
		AllowedCiphers: config.Encryption.AllowedCiphers,
	}
	logger.Debug("SMB encryption configuration",
		"mode", handler.EncryptionConfig.Mode,
		"allowed_ciphers", handler.EncryptionConfig.AllowedCiphers)

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

	// Wire LeaseManager: create a thin wrapper over the shared LockManagers.
	// The LockManagers are per-share, obtained from MetadataService. The
	// LeaseManager uses a resolver pattern to find the correct LockManager
	// at request time.
	if metaSvc := rt.GetMetadataService(); metaSvc != nil {
		resolver := &metadataServiceResolver{metaSvc: metaSvc}
		// TODO(lease-breaks): Wire a concrete LeaseBreakNotifier from the SMB
		// session/connection layer so break notifications are delivered over the
		// wire. Without this, breaks are initiated in LockManager but never
		// sent to clients. Server-side lease state is still correct (conflicts
		// are detected, breaking state is tracked, timeouts will revoke), but
		// clients won't flush dirty caches proactively. This should be wired
		// before leases are used in production.
		leaseMgr := smblease.NewLeaseManager(resolver, nil)
		s.handler.LeaseManager = leaseMgr

		// Register SMBOplockBreaker as the cross-protocol oplock breaker.
		// This allows NFS handlers to trigger lease breaks on SMB clients
		// without importing the SMB handler package.
		oplockBreaker := smblease.NewSMBOplockBreaker(resolver)
		rt.SetAdapterProvider(adapter.OplockBreakerProviderKey, oplockBreaker)

		// Register SMBBreakHandler as BreakCallbacks on each share's LockManager.
		// The notifier is nil until the transport layer is wired (see TODO above).
		breakHandler := smblease.NewSMBBreakHandler(leaseMgr, nil)
		registeredBreakLMs := make(map[lock.LockManager]struct{})
		var breakRegMu sync.Mutex

		// registerBreakCallbacks registers break callbacks for any shares whose
		// LockManagers have not been seen yet. Deduplicate: multiple shares may
		// reference the same LockManager instance.
		registerBreakCallbacks := func(shares []string) {
			breakRegMu.Lock()
			defer breakRegMu.Unlock()
			for _, shareName := range shares {
				if lockMgr := metaSvc.GetLockManagerForShare(shareName); lockMgr != nil {
					if _, already := registeredBreakLMs[lockMgr]; already {
						continue
					}
					lockMgr.RegisterBreakCallbacks(breakHandler)
					registeredBreakLMs[lockMgr] = struct{}{}
				}
			}
		}

		// Register for existing shares at startup.
		registerBreakCallbacks(rt.ListShares())

		// Register for shares added dynamically after startup.
		rt.OnShareChange(registerBreakCallbacks)

		logger.Debug("SMB adapter: LeaseManager wired with per-share LockManagers")
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

	// Apply encryption setting from live settings.
	// When EnableEncryption is true, set handler encryption mode to "preferred"
	// (live settings can enable/disable; the config-level mode takes precedence
	// if already "required").
	if settings.EnableEncryption && s.handler.EncryptionConfig.Mode == "disabled" {
		s.handler.EncryptionConfig.Mode = "preferred"
		logger.Info("SMB encryption enabled via live settings",
			"mode", s.handler.EncryptionConfig.Mode)
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

	// Derive EncryptionEnabled from EncryptionConfig.Mode to keep the two in sync.
	// EncryptionEnabled controls CapEncryption in NEGOTIATE for SMB 3.0/3.0.2.
	s.handler.EncryptionEnabled = (s.handler.EncryptionConfig.Mode != "disabled")

	logger.Debug("SMB adapter: dialect range from settings",
		"min_dialect", settings.MinDialect,
		"max_dialect", settings.MaxDialect,
		"encryption_mode", s.handler.EncryptionConfig.Mode,
		"directory_leasing", settings.DirectoryLeasingEnabled)

	// Authentication settings: NtlmEnabled, GuestEnabled, SMBServicePrincipal
	s.handler.NtlmEnabled = settings.NtlmEnabled
	s.handler.GuestEnabled = settings.GuestEnabled

	s.handler.SMBServicePrincipal = settings.SMBServicePrincipal
	if settings.SMBServicePrincipal != "" {
		logger.Info("SMB adapter: custom SPN override from settings",
			"smb_service_principal", settings.SMBServicePrincipal)
	}

	logger.Debug("SMB adapter: authentication settings from live config",
		"ntlm_enabled", settings.NtlmEnabled,
		"guest_enabled", settings.GuestEnabled,
		"smb_service_principal", settings.SMBServicePrincipal)

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
	// Start durable handle scavenger if a DurableHandleStore is available.
	// The scavenger runs in the background and stops when ctx is cancelled.
	if durableStore := s.findDurableHandleStore(); durableStore != nil {
		s.handler.DurableStore = durableStore

		durableTimeout := uint32(DefaultDurableHandleTimeout)
		if s.handler.DurableTimeoutMs != 0 {
			durableTimeout = s.handler.DurableTimeoutMs
		}

		scavenger := handlers.NewDurableHandleScavenger(
			durableStore,
			s.handler,
			DefaultDurableScavengerInterval,
			durableTimeout,
			s.handler.StartTime,
		)
		go scavenger.Run(ctx)
		logger.Info("SMB adapter: durable handle scavenger started",
			"interval", DefaultDurableScavengerInterval,
			"timeout_ms", durableTimeout)
	}

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
// Also creates the KerberosService for AP-REQ verification and sets the
// default IdentityConfig (strip realm).
// Must be called before Serve(). When not called, Kerberos auth is disabled
// and only NTLM/guest authentication is available.
func (s *Adapter) SetKerberosProvider(provider *kerberos.Provider) {
	if provider == nil {
		return
	}
	s.handler.KerberosProvider = provider

	// Create KerberosService from the provider for AP-REQ verification,
	// replay detection, and mutual auth token construction.
	s.handler.KerberosService = authkerberos.NewKerberosService(provider)

	// Set default IdentityConfig: strip realm ("alice@REALM" -> "alice").
	if s.handler.IdentityConfig == nil {
		s.handler.IdentityConfig = kerberos.DefaultIdentityConfig()
	}

	logger.Debug("SMB adapter Kerberos provider configured",
		"principal", provider.ServicePrincipal(),
		"stripRealm", s.handler.IdentityConfig.StripRealm)
}

const (
	// DefaultDurableScavengerInterval is how often the scavenger checks for expired handles.
	DefaultDurableScavengerInterval = 10 * time.Second

	// DefaultDurableHandleTimeout is the default durable handle timeout in milliseconds.
	DefaultDurableHandleTimeout = 60000
)

// durableHandleStoreProvider is a local interface for metadata stores that
// provide a DurableHandleStore accessor. Mirrors storetest.DurableHandleStoreProvider
// to avoid importing the test package from production code.
type durableHandleStoreProvider interface {
	DurableHandleStore() lock.DurableHandleStore
}

// findDurableHandleStore returns the DurableHandleStore from the single
// registered metadata store that implements durableHandleStoreProvider.
// To avoid persisting durable handles into the wrong store, this helper
// only resolves a store when exactly one metadata store is registered.
// Returns nil if no suitable metadata store exists or if multiple stores
// are registered (ambiguous).
func (s *Adapter) findDurableHandleStore() lock.DurableHandleStore {
	if s.Registry == nil {
		return nil
	}

	names := s.Registry.ListMetadataStores()
	if len(names) == 0 {
		return nil
	}

	// When multiple metadata stores exist, we cannot safely pick one because
	// durable handles for different shares could end up in the wrong store.
	// TODO: resolve DurableHandleStore per-share for multi-store topologies.
	if len(names) > 1 {
		logger.Warn("findDurableHandleStore: multiple metadata stores registered, "+
			"durable handles disabled to avoid cross-store mixing",
			"stores", len(names))
		return nil
	}

	metaStore, err := s.Registry.GetMetadataStore(names[0])
	if err != nil {
		return nil
	}
	if provider, ok := metaStore.(durableHandleStoreProvider); ok {
		return provider.DurableHandleStore()
	}

	return nil
}

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

// ============================================================================
// LockManager Resolver for per-share routing
// ============================================================================

// metadataServiceResolver implements smblease.LockManagerResolver by wrapping
// the MetadataService's per-share LockManagers. This adapter bridges the gap
// between the MetadataService (which owns per-share LockManagers) and the
// lease package (which needs to resolve them by share name).
type metadataServiceResolver struct {
	metaSvc *metadata.MetadataService
}

// GetLockManagerForShare returns the LockManager for the given share.
// Returns nil if no LockManager exists for the share.
func (r *metadataServiceResolver) GetLockManagerForShare(shareName string) lock.LockManager {
	if r.metaSvc == nil {
		return nil
	}
	return r.metaSvc.GetLockManagerForShare(shareName)
}

// GetLockManagerForHandle attempts to resolve the LockManager from a file handle.
// File handles in DittoFS encode the share name as a prefix ("shareName:uuid"),
// so we decode the handle to extract the share and delegate to GetLockManagerForShare.
//
// This implements smblease.AllSharesResolver for cross-protocol oplock breaking.
func (r *metadataServiceResolver) GetLockManagerForHandle(handleKey string) lock.LockManager {
	if r.metaSvc == nil {
		return nil
	}
	// File handles are formatted as "shareName:uuid". Use DecodeFileHandle
	// to extract the share name for LockManager routing.
	shareName, _, err := metadata.DecodeFileHandle(metadata.FileHandle(handleKey))
	if err != nil || shareName == "" {
		return nil
	}
	return r.metaSvc.GetLockManagerForShare(shareName)
}
