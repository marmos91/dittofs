package handlers

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/callback"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// notifyDispatcher sends a single SM_NOTIFY callback to one local monitor.
//
// It is satisfied by *callback.Client. The handler depends on this narrow
// interface (rather than the concrete client) so the relay can be exercised
// without real sockets in tests. A nil dispatcher disables relay: the security
// gates still run, but no callback is sent.
type notifyDispatcher interface {
	Send(
		ctx context.Context,
		addr string,
		status *types.Status,
		proc uint32,
		prog uint32,
		vers uint32,
	) error
}

// DefaultMaxClients is the default maximum number of monitored clients.
const DefaultMaxClients = 10000

// Handler handles NSM protocol requests.
//
// Handler coordinates NSM operations including:
//   - SM_NULL: Ping/health check
//   - SM_STAT: Query server state without registering
//   - SM_MON: Register client for crash notification
//   - SM_UNMON: Unregister a specific monitored host
//   - SM_UNMON_ALL: Unregister all hosts from a caller
//   - SM_NOTIFY: Receive crash notification from another NSM
//
// Thread Safety:
// Handler is safe for concurrent use by multiple goroutines.
// The underlying tracker and client store handle synchronization.
type Handler struct {
	// tracker is the connection tracker for client registration.
	// Used to track active clients and their NSM callback info.
	tracker *lock.ConnectionTracker

	// clientStore persists client registrations across server restarts.
	// If nil, registrations are not persisted.
	clientStore lock.ClientRegistrationStore

	// serverState is the current NSM state counter.
	// Odd values indicate the server is up, even values indicate it went down.
	// Incremented on each server restart.
	serverState atomic.Int32

	// serverName is this server's hostname for SM_NOTIFY.
	// Sent in notifications to identify which server restarted.
	serverName string

	// maxClients is the maximum number of monitored clients.
	// SM_MON returns STAT_FAIL if this limit is reached.
	maxClients int

	// dispatcher relays inbound SM_NOTIFY to local monitors. If nil, the
	// relay is disabled (security gates still run) — useful for tests and for
	// deployments that do not consume inbound notifications.
	dispatcher notifyDispatcher

	// peerStateMu guards peerState.
	peerStateMu sync.Mutex

	// peerState records the last-seen NSM state number for each monitored
	// host (keyed by mon_name). NSM state numbers are monotonically
	// increasing per the protocol, so an SM_NOTIFY is acted on only when its
	// state strictly exceeds the stored value; equal-or-lower states are
	// replays/stale and are ignored. This is the H17 monotonicity gate.
	//
	// Note: this is the last-seen state of the *peer* (the host that
	// rebooted), distinct from the server's own serverState counter.
	peerState map[string]int32
}

// HandlerConfig configures the NSM handler.
type HandlerConfig struct {
	// Tracker is the connection tracker (required).
	// Used to track active clients and their NSM callback info.
	Tracker *lock.ConnectionTracker

	// ClientStore persists registrations (optional).
	// If nil, registrations are not persisted across restarts.
	ClientStore lock.ClientRegistrationStore

	// ServerName is this server's hostname (required).
	// Used in SM_NOTIFY callbacks to identify this server.
	ServerName string

	// InitialState is the starting NSM state (default: 1 = up).
	// Typically loaded from persistent storage on startup.
	InitialState int32

	// MaxClients limits monitored clients (default: 10000).
	// SM_MON returns STAT_FAIL when this limit is exceeded.
	MaxClients int

	// Dispatcher relays inbound SM_NOTIFY callbacks to local monitors
	// (optional). If nil, inbound NOTIFY is validated but not relayed.
	// Normally a *callback.Client.
	Dispatcher notifyDispatcher
}

// NewHandler creates a new NSM handler.
//
// Parameters:
//   - config: Handler configuration
//
// Returns a configured Handler ready to process NSM requests.
//
// Panics if config.Tracker is nil.
func NewHandler(config HandlerConfig) *Handler {
	if config.Tracker == nil {
		panic("NSM handler requires a connection tracker")
	}

	if config.MaxClients == 0 {
		config.MaxClients = DefaultMaxClients
	}
	if config.InitialState == 0 {
		config.InitialState = 1 // Default: odd = up
	}
	if config.Dispatcher == nil {
		// Default to a real callback client so the inbound NOTIFY relay works
		// out of the box. Tests inject a fake dispatcher via config.
		config.Dispatcher = callback.NewClient(0)
	}

	h := &Handler{
		tracker:     config.Tracker,
		clientStore: config.ClientStore,
		serverName:  config.ServerName,
		maxClients:  config.MaxClients,
		dispatcher:  config.Dispatcher,
		peerState:   make(map[string]int32),
	}
	h.serverState.Store(config.InitialState)
	return h
}

// GetServerState returns the current NSM state counter.
//
// The state counter follows these conventions:
//   - Odd values: Server is up (after restart)
//   - Even values: Server went down (crash detected)
func (h *Handler) GetServerState() int32 {
	return h.serverState.Load()
}

// IncrementServerState increments the state counter.
//
// This should be called on server startup to indicate the server
// has restarted. The new state will be odd (up) if the previous
// state was even (down).
//
// Returns the new state value.
func (h *Handler) IncrementServerState() int32 {
	return h.serverState.Add(1)
}

// SetServerState sets the state counter to a specific value.
//
// This is primarily used for recovery from persistent storage
// or for testing. In normal operation, use IncrementServerState.
func (h *Handler) SetServerState(state int32) {
	h.serverState.Store(state)
}

// GetTracker returns the connection tracker for external access.
//
// This allows the NFS adapter to access client registration info
// for operations like grace period management.
func (h *Handler) GetTracker() *lock.ConnectionTracker {
	return h.tracker
}

// GetClientStore returns the client registration store.
//
// This allows the NSM service to access persisted registrations
// for crash recovery operations.
func (h *Handler) GetClientStore() lock.ClientRegistrationStore {
	return h.clientStore
}

// GetServerName returns the configured server hostname.
func (h *Handler) GetServerName() string {
	return h.serverName
}

// admitPeerState enforces SM_NOTIFY state-number monotonicity for a monitored
// host (H17). It returns true and records incoming as the new last-seen state
// only when incoming is a forward advance over the previously stored state for
// monName, using RFC 1982 serial-number arithmetic (the state is conceptually a
// uint32 that wraps). A replay (equal) or retrograde state returns false without
// mutating stored state.
//
// The first NOTIFY ever seen for a monName is admitted (its difference from the
// zero-value default is a small positive forward advance), then recorded.
func (h *Handler) admitPeerState(monName string, incoming int32) bool {
	h.peerStateMu.Lock()
	defer h.peerStateMu.Unlock()

	// Serial-number arithmetic per RFC 1982: NSM state numbers are conceptually
	// uint32 and increment monotonically, so they wrap from MaxInt32 to MinInt32
	// without going backwards. Admit if (incoming - stored) mod 2^32 is in
	// (0, 2^31): the values differ AND the high bit of the wrapped difference is
	// clear ("went forwards"). A signed comparison would wrongly reject a
	// legitimate reboot once the counter wraps past math.MaxInt32.
	stored := h.peerState[monName]
	if incoming == stored {
		return false // exact replay
	}
	diff := uint32(incoming) - uint32(stored)
	if diff >= 0x80000000 {
		return false // stale / retrograde (high bit set means "went backwards")
	}
	h.peerState[monName] = incoming
	return true
}
