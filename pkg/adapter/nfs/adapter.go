package nfs

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/tlsconfig"

	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	nlm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm"
	nsm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nsm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	v3 "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	v4handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/logger"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Compile-time assertion that NFSAdapter satisfies the adapter.Adapter
// interface. Guards against method-signature drift (e.g. SetRuntime regaining
// a concrete *runtime.Runtime parameter).
var _ adapter.Adapter = (*NFSAdapter)(nil)

// DefaultMaxConnections is the connection cap applied when MaxConnections is
// left at its zero value. A non-zero default ensures the accept-loop semaphore
// is always built, preventing unauthenticated connection-exhaustion DoS.
// Operators can raise it for high-load deployments.
const DefaultMaxConnections = 1024

// NFSAdapter implements the adapter.Adapter interface for NFS protocol.
//
// This adapter provides a production-ready NFS server supporting both
// NFSv3 and NFSv4 simultaneously with:
//   - Graceful shutdown with configurable timeout
//   - Connection limiting and resource management
//   - Context-based request cancellation
//   - Configurable timeouts for read/write/idle operations
//   - Thread-safe operation with atomic counters
//
// Architecture:
// NFSAdapter embeds BaseAdapter for shared TCP lifecycle management (listener,
// shutdown, connection tracking, semaphore, metrics logging). Protocol-specific
// behavior (handlers, portmapper, Kerberos, NSM) stays on the outer struct.
// The ConnectionFactory pattern enables BaseAdapter to create NFS-specific
// connections via NewConnection.
//
// Shutdown flow:
//  1. Context cancelled or Stop() called
//  2. NFS-specific cleanup (portmapper, GSS, Kerberos, NSM) [NFSAdapter.Stop]
//  3. Listener closed (no new connections) [BaseAdapter]
//  4. shutdownCtx cancelled (signals in-flight requests to abort) [BaseAdapter]
//  5. Wait for active connections to complete (up to ShutdownTimeout) [BaseAdapter]
//  6. Force-close any remaining connections after timeout [BaseAdapter]
//
// Thread safety:
// All methods are safe for concurrent use. The shutdown mechanism uses sync.Once
// to ensure idempotent behavior even if Stop() is called multiple times.
type NFSAdapter struct {
	*adapter.BaseAdapter

	// config holds the NFS-specific server configuration (ports, timeouts, limits, portmapper)
	config NFSConfig

	// nfsHandler processes NFSv3 protocol operations (LOOKUP, READ, WRITE, etc.)
	nfsHandler *v3.Handler

	// v4Handler processes NFSv4 COMPOUND operations
	v4Handler *v4handlers.Handler

	// pseudoFS is the NFSv4 pseudo-filesystem virtual namespace
	pseudoFS *pseudofs.PseudoFS

	// v3FirstUse and v4FirstUse log at INFO level on first use of each version
	v3FirstUse sync.Once
	v4FirstUse sync.Once

	// mountHandler processes MOUNT protocol operations (MNT, UMNT, EXPORT, etc.)
	mountHandler *mount.Handler

	// nlmHandler processes NLM (Network Lock Manager) operations (LOCK, UNLOCK, TEST, etc.)
	nlmHandler *nlm_handlers.Handler

	// nsmHandler processes NSM (Network Status Monitor) operations (MON, UNMON, NOTIFY, etc.)
	nsmHandler *nsm_handlers.Handler

	// nsmNotifier orchestrates SM_NOTIFY callbacks on server restart
	nsmNotifier *nsm.Notifier

	// gssProcessor handles RPCSEC_GSS context lifecycle (INIT/DATA/DESTROY).
	// nil when Kerberos is not enabled.
	gssProcessor *gss.GSSProcessor

	// kerberosProvider holds the Kerberos keytab/config provider.
	// Closed in Stop() to release the keytab hot-reload goroutine.
	// nil when Kerberos is not enabled.
	kerberosProvider *kerberos.Provider

	// kerberosConfig holds the Kerberos configuration for GSS initialization.
	// nil when Kerberos is not enabled.
	kerberosConfig *config.KerberosConfig

	// portmapServer is the embedded portmapper server (RFC 1057).
	// nil when portmapper is disabled.
	portmapServer *portmap.Server

	// portmapRegistry holds the portmap service registry.
	// nil when portmapper is disabled.
	portmapRegistry *portmap.Registry

	// nsmClientStore persists client registrations for crash recovery
	nsmClientStore lock.ClientRegistrationStore

	// blockingQueue manages pending NLM blocking lock requests
	blockingQueue *blocking.BlockingQueue

	// nextConnID is a global atomic counter for assigning unique connection IDs.
	// Incremented at TCP accept() time and passed to each NFSConnection.
	nextConnID atomic.Uint64

	// shareUnsubscribers holds unsubscribe functions returned by rt.OnShareChange.
	// Called during Stop to prevent stale callbacks from accumulating across restarts.
	shareUnsubscribers []func()

	// drc is the server-wide duplicate-request cache for non-idempotent NFSv3
	// procedures. It replays the recorded reply of a completed op when a client
	// retransmits the same request (RPC-timeout retry on a hard mount), so the
	// retry does not re-execute and return a spurious EEXIST/ENOENT/NOT_SYNC.
	drc *duplicateRequestCache

	// blockedOpsMu protects v3BlockedOps from concurrent read/write access.
	blockedOpsMu sync.RWMutex

	// v3BlockedOps is the parsed set of NFSv3 procedure names blocked at the
	// adapter level, keyed by procedure name (e.g. "WRITE", "REMOVE"). It is
	// parsed once from NFSAdapterSettings.BlockedOperations in applyNFSSettings
	// (at startup and on each settings-change event) so the hot v3 dispatch path
	// (isOperationBlocked) does a single map lookup instead of a per-RPC JSON
	// unmarshal + linear scan.
	v3BlockedOps map[string]bool

	// tlsConfig is the server TLS config used to upgrade connections that send
	// an AUTH_TLS STARTTLS probe (RFC 9289). nil when TLS is not configured, in
	// which case connections stay plaintext. Built once in Serve.
	tlsConfig *tls.Config

	// tlsRequire mirrors NFSTLSConfig.Mode == "require": when true, a connection
	// must upgrade via AUTH_TLS before any other RPC is served.
	tlsRequire bool
}

// NFSTimeoutsConfig groups all timeout-related configuration.
type NFSTimeoutsConfig struct {
	// Read is the maximum duration for reading a complete RPC request.
	// This prevents slow or malicious clients from holding connections indefinitely.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	Read time.Duration `mapstructure:"read" validate:"min=0"`

	// Write is the maximum duration for writing an RPC response.
	// This prevents slow networks or clients from blocking server resources.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	Write time.Duration `mapstructure:"write" validate:"min=0"`

	// Idle is the maximum duration a connection can remain idle
	// between requests before being closed automatically.
	// This frees resources from abandoned connections.
	// 0 means no timeout (connections stay open indefinitely).
	// Recommended: 5m for production.
	Idle time.Duration `mapstructure:"idle" validate:"min=0"`

	// Shutdown is the maximum duration to wait for active connections
	// to complete during graceful shutdown.
	// After this timeout, remaining connections are forcibly closed.
	// Must be > 0 to ensure shutdown completes.
	// Recommended: 30s (balances graceful shutdown with restart time).
	Shutdown time.Duration `mapstructure:"shutdown" validate:"required,gt=0"`
}

// NFSConfig holds configuration parameters for the NFS server.
//
// These values control server behavior including connection limits, timeouts,
// and resource management.
//
// Default values (applied by New if zero):
//   - Port: 2049 (standard NFS port)
//   - MaxConnections: 1024 (DefaultMaxConnections)
//   - Timeouts.Read: 5m
//   - Timeouts.Write: 30s
//   - Timeouts.Idle: 5m
//   - Timeouts.Shutdown: 30s
//
// Production recommendations:
//   - MaxConnections: Set based on expected load (e.g., 1000 for busy servers)
//   - Timeouts.Read: 30s prevents slow clients from holding connections
//   - Timeouts.Write: 30s prevents slow networks from blocking responses
//   - Timeouts.Idle: 5m closes inactive connections to free resources
//   - Timeouts.Shutdown: 30s balances graceful shutdown with restart time
type NFSConfig struct {
	// Enabled controls whether the NFS adapter is active.
	// When false, the NFS adapter will not be started.
	Enabled bool `mapstructure:"enabled"`

	// Port is the TCP port to listen on for NFS connections.
	// Standard NFS port is 2049. Must be > 0.
	// If 0, defaults to 2049.
	Port int `mapstructure:"port" validate:"min=0,max=65535"`

	// MaxConnections limits the number of concurrent client connections.
	// When reached, new connections are rejected until existing ones close.
	// If 0, defaults to DefaultMaxConnections (1024) so the accept loop is
	// always bounded. Raise to 1000-5000 for busy production servers.
	MaxConnections int `mapstructure:"max_connections" validate:"min=0"`

	// MaxRequestsPerConnection limits the number of concurrent RPC requests
	// that can be processed simultaneously on a single connection.
	// This enables parallel handling of multiple COMMITs, WRITEs, and READs.
	// 0 means unlimited (will default to 100).
	// Recommended: 50-200 for high-throughput servers.
	MaxRequestsPerConnection int `mapstructure:"max_requests_per_connection" validate:"min=0"`

	// Timeouts groups all timeout-related configuration
	Timeouts NFSTimeoutsConfig `mapstructure:"timeouts"`

	// Portmapper configures the embedded portmapper (RFC 1057).
	// The portmapper allows NFS clients to discover DittoFS services
	// via rpcinfo/showmount without requiring a system-level rpcbind daemon.
	// Default: enabled on port 10111.
	Portmapper PortmapConfig `mapstructure:"portmapper"`

	// TLS configures opportunistic NFS-over-TLS (RFC 9289). When the cert/key
	// files are set, the server answers an AUTH_TLS STARTTLS probe and upgrades
	// the connection to TLS 1.3. Unset = plaintext only (back-compat).
	TLS NFSTLSConfig `mapstructure:"tls"`
}

// NFSTLSConfig configures NFS-over-TLS (RFC 9289). It mirrors the control
// plane's file-based TLS config and reuses internal/tlsconfig for cert loading
// and hot-reload. DittoFS only consumes cert files; lifecycle (issuance,
// renewal, rotation) is the platform's responsibility.
type NFSTLSConfig struct {
	// CertFile / KeyFile are the PEM server certificate and private key. Both
	// must be set to enable TLS; setting one without the other is an error.
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`

	// ClientCA is a PEM CA bundle. When set, the server requires and verifies a
	// client certificate (mutual TLS).
	ClientCA string `mapstructure:"client_ca"`

	// MinVersion is the configured minimum TLS version: "1.2" or "1.3". RFC 9289
	// mandates TLS 1.3, so the effective floor is always raised to 1.3 even when
	// "1.2" is set; the knob exists only for forward-compatibility symmetry with
	// the control-plane TLS config.
	MinVersion string `mapstructure:"min_version"`

	// Mode selects the upgrade policy:
	//   - "opportunistic" (default): non-TLS clients are still served in
	//     plaintext; only clients that send the AUTH_TLS probe are upgraded.
	//   - "require": every connection must upgrade via AUTH_TLS before any
	//     other RPC; plaintext requests are rejected.
	Mode string `mapstructure:"mode"`
}

// NFS TLS Mode values. An empty Mode is treated as opportunistic.
const (
	// NFSTLSModeOpportunistic upgrades clients that send the AUTH_TLS probe but
	// still serves non-TLS clients in plaintext.
	NFSTLSModeOpportunistic = "opportunistic"
	// NFSTLSModeRequire rejects plaintext RPC until a connection has upgraded
	// via AUTH_TLS.
	NFSTLSModeRequire = "require"
)

// validateMode rejects an unknown TLS Mode so a typo (e.g. "required") does not
// silently fall back to opportunistic. An empty Mode is allowed (opportunistic).
func (t NFSTLSConfig) validateMode() error {
	switch t.Mode {
	case "", NFSTLSModeOpportunistic, NFSTLSModeRequire:
		return nil
	default:
		return fmt.Errorf("invalid mode %q (use %q or %q)", t.Mode, NFSTLSModeOpportunistic, NFSTLSModeRequire)
	}
}

// shared converts the wire config into the backend-agnostic tlsconfig.Config.
func (t NFSTLSConfig) shared() tlsconfig.Config {
	return tlsconfig.Config{
		CertFile:   t.CertFile,
		KeyFile:    t.KeyFile,
		ClientCA:   t.ClientCA,
		MinVersion: t.MinVersion,
	}
}

// Enabled reports whether TLS is configured (both cert and key set).
func (t NFSTLSConfig) Enabled() bool {
	return t.shared().Enabled()
}

// PortmapConfig holds configuration for the embedded portmapper.
//
// The portmapper enables NFS clients to discover DittoFS services without
// needing a system-level rpcbind/portmap daemon. It runs on a configurable
// port (default 10111, an unprivileged port to avoid requiring root).
//
// Configuration path: adapters.nfs.portmapper.enabled / adapters.nfs.portmapper.port
//
// The Enabled field uses a *bool pointer type to distinguish between
// "not set in config" (nil, defaults to true) and "explicitly set to false".
type PortmapConfig struct {
	// Enabled controls whether the portmapper is active.
	// When nil (not specified in config), defaults to true.
	// Set to false to explicitly disable the portmapper.
	Enabled *bool `mapstructure:"enabled"`

	// Port is the port to listen on for portmapper requests.
	// Default: 10111 (unprivileged port; standard portmapper uses 111 but requires root).
	Port int `mapstructure:"port" validate:"min=0,max=65535"`
}

// applyDefaults fills in zero values with sensible defaults.
func (c *NFSConfig) applyDefaults() {
	// Note: Enabled field defaults are handled in pkg/config/defaults.go
	// to allow explicit false values from configuration files.

	if c.Port <= 0 {
		c.Port = 2049
	}
	if c.MaxRequestsPerConnection == 0 {
		c.MaxRequestsPerConnection = 100
	}
	if c.MaxConnections == 0 {
		// A sane default cap so the accept loop's connSemaphore is always
		// built. Leaving this at 0 (unlimited) lets unauthenticated clients
		// exhaust memory/goroutines. Operators can raise it for busy servers.
		c.MaxConnections = DefaultMaxConnections
	}
	if c.Timeouts.Read == 0 {
		c.Timeouts.Read = 5 * time.Minute
	}
	if c.Timeouts.Write == 0 {
		c.Timeouts.Write = 30 * time.Second
	}
	if c.Timeouts.Idle == 0 {
		c.Timeouts.Idle = 5 * time.Minute
	}
	if c.Timeouts.Shutdown == 0 {
		c.Timeouts.Shutdown = 30 * time.Second
	}
	// Portmapper port defaults to 10111 (unprivileged port).
	// Note: Portmapper.Enabled is NOT set here -- it uses a *bool pointer where
	// nil means "default to true" and explicit false means "disabled".
	// This is handled by isPortmapperEnabled().
	if c.Portmapper.Port == 0 {
		c.Portmapper.Port = 10111
	}
}

// validate checks that the configuration is valid for production use.
func (c *NFSConfig) validate() error {
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be 0-65535", c.Port)
	}
	if c.MaxConnections < 0 {
		return fmt.Errorf("invalid MaxConnections %d: must be >= 0", c.MaxConnections)
	}
	if c.Timeouts.Read < 0 {
		return fmt.Errorf("invalid timeouts.read %v: must be >= 0", c.Timeouts.Read)
	}
	if c.Timeouts.Write < 0 {
		return fmt.Errorf("invalid timeouts.write %v: must be >= 0", c.Timeouts.Write)
	}
	if c.Timeouts.Idle < 0 {
		return fmt.Errorf("invalid timeouts.idle %v: must be >= 0", c.Timeouts.Idle)
	}
	if c.Timeouts.Shutdown <= 0 {
		return fmt.Errorf("invalid timeouts.shutdown %v: must be > 0", c.Timeouts.Shutdown)
	}
	return nil
}

// New creates a new NFSAdapter with the specified configuration.
//
// The adapter is created in a stopped state. Call SetStores() to inject
// the backend repositories, then call Serve() to start accepting connections.
//
// Configuration:
//   - Zero values in config are replaced with sensible defaults
//   - Invalid configurations cause a panic (indicates programmer error)
//
// Parameters:
//   - config: Server configuration (ports, timeouts, limits)
//
// Returns a configured but not yet started NFSAdapter.
//
// Panics if config validation fails.
func New(nfsConfig NFSConfig) *NFSAdapter {
	// Apply defaults for zero values
	nfsConfig.applyDefaults()

	// Validate configuration
	if err := nfsConfig.validate(); err != nil {
		panic(fmt.Sprintf("invalid NFS config: %v", err))
	}

	// Build BaseAdapter config from NFS config
	baseConfig := adapter.BaseConfig{
		Port:            nfsConfig.Port,
		MaxConnections:  nfsConfig.MaxConnections,
		ShutdownTimeout: nfsConfig.Timeouts.Shutdown,
	}

	base := adapter.NewBaseAdapter(baseConfig, "NFS")

	return &NFSAdapter{
		BaseAdapter:  base,
		config:       nfsConfig,
		nfsHandler:   &v3.Handler{},
		mountHandler: &mount.Handler{},
		drc:          newDuplicateRequestCache(),
	}
}

// SetKerberosConfig sets the Kerberos configuration for RPCSEC_GSS support.
//
// This must be called before SetRuntime() if Kerberos authentication is desired.
// When set, the GSSProcessor will be initialized during SetRuntime().
//
// Parameters:
//   - cfg: Kerberos configuration. If nil or Enabled is false, Kerberos is disabled.
//
// Thread safety:
// Called exactly once before Serve(), no synchronization needed.
func (s *NFSAdapter) SetKerberosConfig(cfg *config.KerberosConfig) {
	if cfg != nil && cfg.Enabled {
		s.kerberosConfig = cfg
	}
}

// SetRuntime injects the runtime containing all stores and shares.
//
// Called by Runtime before Serve() is called. The runtime
// provides access to all configured metadata stores, content stores, and shares.
//
// The NFS adapter stores the runtime and injects it into both the NFS and Mount
// handlers so they can access stores based on share names.
//
// Parameters:
//   - rt: Runtime containing all stores and shares
//
// Thread safety:
// Called exactly once before Serve(), no synchronization needed.
func (s *NFSAdapter) SetRuntime(rtAny any) {
	s.BaseAdapter.SetRuntime(rtAny)
	rt := rtAny.(*runtime.Runtime)

	// Inject runtime into handlers
	s.nfsHandler.Registry = rt
	s.mountHandler.Registry = rt

	// Initialize NFSv4 pseudo-filesystem and handler
	s.pseudoFS = pseudofs.New()
	shares := rt.ListShares()
	s.pseudoFS.Rebuild(shares)
	v4StateManager := v4state.NewStateManager(v4state.DefaultLeaseDuration)
	s.v4Handler = v4handlers.NewHandler(rt, s.pseudoFS, v4StateManager)
	s.v4Handler.KerberosEnabled = s.kerberosConfig != nil

	// Expose StateManager to REST API via runtime (for /clients endpoint and /health server info)
	rt.SetNFSClientProvider(v4StateManager)

	// Designate a server-global durable client-recovery store (area-4 H8): the
	// first share's metadata store implementing lock.ClientRecoveryStore,
	// mirroring the NSM ClientRegistrationStore designation. NFSv4 clientids are
	// not owned by any one share, so the chosen store backs the whole server.
	var recoveryStore lock.ClientRecoveryStore
	for _, shareName := range rt.ListShares() {
		store, err := rt.GetMetadataStoreForShare(shareName)
		if err != nil {
			continue
		}
		if crs, ok := store.(lock.ClientRecoveryStore); ok {
			recoveryStore = crs
			break
		}
	}
	if recoveryStore != nil {
		v4StateManager.SetClientRecoveryStore(recoveryStore, uint64(v4StateManager.BootEpoch()))
		// On boot, load the durable prior-client set and seed the v4 grace
		// expected-reclaim roster (records already reclaim-complete are excluded).
		// This is what gives the previously-no-op v4 grace a real expected set
		// after an ungraceful restart. With an empty/fresh store the roster is
		// empty and grace is skipped, exactly as on develop today.
		n := v4StateManager.LoadClientRecovery(context.Background())
		logger.Debug("NFSv4 client-recovery store designated", "boot_loaded_clients", n)
	}

	// Register callback to rebuild pseudo-fs when shares change (add/remove).
	// Registered before the initial share loop so no share created between the
	// loop and subscription is missed.
	unsubPseudoFS := rt.OnShareChange(func(shares []string) {
		s.pseudoFS.Rebuild(shares)
		logger.Info("NFSv4 pseudo-fs rebuilt", "shares", len(shares))
	})
	s.shareUnsubscribers = append(s.shareUnsubscribers, unsubPseudoFS)

	// Create blocking queue for NLM lock operations
	s.blockingQueue = blocking.NewBlockingQueue(nlm_handlers.DefaultBlockingQueueSize)

	// Initialize NLM handler with routingNLMService (uses LockManager directly, not MetadataService)
	metadataService := rt.GetMetadataService()
	nlmSvc := s.createRoutingNLMService(metadataService)
	s.nlmHandler = nlm_handlers.NewHandler(nlmSvc, s.blockingQueue)

	// Cross-protocol blocked-waiter wakeup: a byte-range UNLOCK on ANY protocol
	// (notably an SMB UNLOCK) must re-drive blocked NLM waiters on the freed
	// range, because NLM uses a server-driven NLM_GRANTED callback rather than
	// poll-retry. Register the hook on the service for shares added later AND
	// stamp it onto lock managers already created at boot (shares loaded before
	// this adapter existed), mirroring the grace-coordinator catch-up below.
	byteRangeReleaseHook := func(handleKey string) {
		go s.processNLMWaiters(metadata.FileHandle(handleKey))
	}
	metadataService.SetByteRangeReleaseHook(byteRangeReleaseHook)
	for _, shareName := range rt.ListShares() {
		if lm := metadataService.GetLockManagerForShare(shareName); lm != nil {
			lm.SetByteRangeReleaseCallback(byteRangeReleaseHook)
		}
	}

	// Couple the NFSv4 StateManager grace machine to the per-share lock-manager
	// grace machine so both enter/exit the post-restart window together. Shares
	// loaded at boot are registered BEFORE this adapter exists, so the lock
	// manager may already be in grace; we both (a) register the coordinator for
	// any share added later and (b) catch up the v4 machine for shares already
	// in grace at startup.
	graceCoord := &nfsGraceCoordinator{sm: v4StateManager}
	metadataService.SetGraceCoordinator(graceCoord)
	for _, shareName := range rt.ListShares() {
		if lm := metadataService.GetLockManagerForShare(shareName); lm != nil && lm.IsInGracePeriod() {
			graceCoord.OnLockGraceStart(lm.GetExpectedClients())
		}
	}

	// Initialize NSM handler for crash recovery
	// NSM uses the ConnectionTracker from the MetadataService and ClientRegistrationStore
	s.initNSMHandler(rt, metadataService)

	// Initialize RPCSEC_GSS processor if Kerberos is enabled
	s.initGSSProcessor(rt)

	logger.Debug("NFS adapter configured with runtime", "shares", rt.CountShares())

	// Register NFSBreakHandler on each share's LockManager.
	// When the shared LockManager recalls a delegation (e.g., due to an SMB
	// write conflicting with an NFS delegation), the handler translates the
	// recall into a CB_RECALL sent via the NFS backchannel.
	breakHandler := v4state.NewNFSBreakHandler(v4StateManager)
	registeredLockManagers := make(map[lock.LockManager]struct{})
	var breakRegMu sync.Mutex

	// registerBreakCallbacks registers break callbacks for any shares whose
	// LockManagers have not been seen yet. Multiple shares may reference the
	// same LockManager instance, so we deduplicate.
	registerBreakCallbacks := func(shares []string) {
		breakRegMu.Lock()
		defer breakRegMu.Unlock()
		for _, shareName := range shares {
			if lockMgr := metadataService.GetLockManagerForShare(shareName); lockMgr != nil {
				if _, already := registeredLockManagers[lockMgr]; already {
					continue
				}
				lockMgr.RegisterBreakCallbacks(breakHandler)
				registeredLockManagers[lockMgr] = struct{}{}
			}
		}
	}

	// Register for shares added dynamically after startup. Done before the
	// initial loop so no share created between the two is missed.
	unsubBreak := rt.OnShareChange(registerBreakCallbacks)
	s.shareUnsubscribers = append(s.shareUnsubscribers, unsubBreak)

	// Register for existing shares at startup.
	registerBreakCallbacks(rt.ListShares())

	// Apply live NFS adapter settings from SettingsWatcher.
	// The SettingsWatcher polls DB every 10s and provides atomic pointer swap
	// for thread-safe reads. Settings are consumed here at startup and on
	// each new connection (grandfathering per locked decision).
	s.applyNFSSettings(rt)

	// Populate filesystem capabilities (FATTR4_MAXFILESIZE/MAXREAD/MAXWRITE) once
	// at startup. These are store-level config consumed by GETATTR, so they live
	// in process-global atomics in the attrs package and are NOT re-read per
	// request or per connection — only here and on settings-change events.
	s.applyFilesystemCapabilities(rt)

	// Refresh cached state (blocked-ops set, lease/grace, filesystem
	// capabilities) whenever settings change on the 10s poll, so long-lived
	// connections pick up changes without re-reading live settings per request.
	if sw := rt.GetSettingsWatcher(); sw != nil {
		sw.OnNFSSettingsChange(func(_ *models.NFSAdapterSettings) {
			s.applyNFSSettings(rt)
			s.applyFilesystemCapabilities(rt)
		})
	}

	// Boot/initial-recovery wiring is complete: boot shares already in
	// lock-manager grace have been caught up above and the durable v4 reclaim
	// roster (LoadClientRecovery) has been armed. Latch the coordinator into the
	// serving phase so a subsequent RUNTIME AddShare does not arm server-wide
	// NFSv4 reboot grace (round-2 #7 H-1): a runtime-added share has no
	// pre-existing v4 clients to reclaim, and arming grace with the LIVE client
	// set would freeze every connected client's OPEN/LOCK for the grace window.
	graceCoord.MarkServing()
}

// Serve starts the NFS server and blocks until the context is cancelled
// or an unrecoverable error occurs.
//
// Serve performs NFS-specific startup (portmapper, NSM, v4.1 session reaper)
// then delegates to BaseAdapter.ServeWithFactory() for the shared TCP accept
// loop. NFS-specific connection cleanup (v4 backchannel unbinding) is handled
// via the onClose callback.
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
// Serve() should only be called once per NFSAdapter instance.
func (s *NFSAdapter) Serve(ctx context.Context) error {
	logger.Debug("NFS config", "max_connections", s.config.MaxConnections, "read_timeout", s.config.Timeouts.Read, "write_timeout", s.config.Timeouts.Write, "idle_timeout", s.config.Timeouts.Idle)

	// Build the server TLS config (loads + parses the cert files now, so a bad
	// cert path fails at startup rather than on the first STARTTLS probe). nil
	// when TLS is not configured, in which case connections stay plaintext.
	if err := s.config.TLS.shared().Validate(); err != nil {
		return fmt.Errorf("adapters.nfs.tls.%w", err)
	}
	if err := s.config.TLS.validateMode(); err != nil {
		return fmt.Errorf("adapters.nfs.tls.%w", err)
	}
	tlsCfg, err := tlsconfig.ServerConfig(s.config.TLS.shared())
	if err != nil {
		return fmt.Errorf("configure NFS TLS: %w", err)
	}
	s.tlsConfig = tlsCfg
	s.tlsRequire = s.config.TLS.Enabled() && s.config.TLS.Mode == NFSTLSModeRequire
	if tlsCfg != nil {
		// TLS 1.3 is the RFC 9289 floor; raise the configured minimum if needed.
		if tlsCfg.MinVersion < tls.VersionTLS13 {
			tlsCfg.MinVersion = tls.VersionTLS13
		}
		logger.Info("NFS-over-TLS enabled (RFC 9289)",
			"mode", map[bool]string{true: NFSTLSModeRequire, false: NFSTLSModeOpportunistic}[s.tlsRequire],
			"mtls", s.config.TLS.ClientCA != "")
	}

	// Start embedded portmapper (RFC 1057) for NFS service discovery.
	// This allows clients to query rpcinfo/showmount without needing
	// a system-level rpcbind daemon. Portmapper failure is non-fatal
	// (privileged ports like 111 may require root privileges).
	if err := s.startPortmapper(ctx); err != nil {
		logger.Warn("Portmapper failed to start (NFS will continue without it)", "error", err)
	}

	// NSM startup: Load persisted registrations and notify all clients
	// Per CONTEXT.md: Parallel notification for fastest recovery
	s.performNSMStartup(ctx)

	// Start NFSv4.1 session reaper for expired/unconfirmed client cleanup
	if s.v4Handler != nil && s.v4Handler.StateManager != nil {
		s.v4Handler.StateManager.StartSessionReaper(ctx)
	}

	// Note: onClose is nil because v4 backchannel cleanup is handled per-connection
	// in NewNFSConnection's defer, not at the adapter level.
	return s.ServeWithFactory(ctx, s, s.preAcceptCheck, nil)
}

// preAcceptCheck checks live settings for dynamic max_connections limit and
// re-applies NFS settings on each new connection.
func (s *NFSAdapter) preAcceptCheck(conn net.Conn) bool {
	if s.Registry == nil {
		return true
	}

	// Check live max_connections limit
	if liveSettings := s.Registry.GetNFSSettings(); liveSettings != nil && liveSettings.MaxConnections > 0 {
		currentActive := s.ConnCount.Load()
		if int(currentActive) >= liveSettings.MaxConnections {
			logger.Warn("NFS connection rejected: live settings max_connections exceeded",
				"active", currentActive,
				"max_connections", liveSettings.MaxConnections,
				"client", conn.RemoteAddr())
			return false
		}
	}

	// Re-apply live NFS settings on each new connection.
	// This ensures dynamic settings changes (e.g., delegations-enabled)
	// propagate from the SettingsWatcher to the StateManager.
	s.applyNFSSettings(s.Registry)

	return true
}

// NewConnection creates a protocol-specific connection handler for an accepted
// TCP connection. This implements the adapter.ConnectionFactory interface.
//
// NFS connections need a unique connection ID for backchannel binding,
// so we assign one here at accept time.
func (s *NFSAdapter) NewConnection(conn net.Conn) adapter.ConnectionHandler {
	connID := s.nextConnID.Add(1)
	return NewNFSConnection(s, conn, connID)
}

// logV3FirstUse logs at INFO level the first time a client uses NFSv3.
// Subsequent calls are no-ops (uses sync.Once for one-time logging).
func (s *NFSAdapter) logV3FirstUse() {
	s.v3FirstUse.Do(func() {
		logger.Info("First NFSv3 request received")
	})
}

// logV4FirstUse logs at INFO level the first time a client uses NFSv4.
// Subsequent calls are no-ops (uses sync.Once for one-time logging).
func (s *NFSAdapter) logV4FirstUse() {
	s.v4FirstUse.Do(func() {
		logger.Info("First NFSv4 request received")
	})
}
