package lock

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ============================================================================
// Connection Tracking for Lock Management
// ============================================================================

// ClientRegistration represents a registered client connection.
type ClientRegistration struct {
	// ClientID is the unique client identifier.
	ClientID string

	// AdapterType identifies which protocol adapter (e.g., "nfs", "smb").
	AdapterType string

	// TTL is how long to keep locks after disconnect (0 = immediate release).
	TTL time.Duration

	// RegisteredAt is when the client registered.
	RegisteredAt time.Time

	// LastSeen is the last activity timestamp.
	LastSeen time.Time

	// RemoteAddr is the client IP address (for logging/metrics).
	RemoteAddr string

	// LockCount is the number of locks held by this client.
	LockCount int

	// ============================================================================
	// NSM-specific fields (Phase 3)
	// ============================================================================

	// MonName is the monitored hostname (mon_id.mon_name from SM_MON).
	// This is typically the server's hostname that the client is monitoring.
	MonName string

	// Priv is the 16-byte private data returned in SM_NOTIFY callbacks.
	// Stored from SM_MON and sent back to the client when state changes.
	Priv [16]byte

	// SMState is the client's NSM state counter at registration time.
	// Used to detect stale registrations after client restarts.
	SMState int32

	// CallbackInfo contains RPC callback details from SM_MON my_id field.
	// Used to send SM_NOTIFY callbacks when server restarts or client crashes.
	CallbackInfo *NSMCallback
}

// NSMCallback holds callback RPC details from SM_MON my_id field.
// Used to send SM_NOTIFY callbacks when server restarts or client crashes.
type NSMCallback struct {
	// Hostname is the callback target (my_id.my_name).
	// This is where the SM_NOTIFY RPC will be sent.
	Hostname string

	// Program is the RPC program number (usually NLM 100021).
	Program uint32

	// Version is the program version.
	Version uint32

	// Proc is the procedure number for the callback.
	// NLM uses NLM_FREE_ALL (procedure 23) to release locks.
	Proc uint32
}

// ConnectionTrackerConfig configures the connection tracker.
type ConnectionTrackerConfig struct {
	// MaxConnectionsPerAdapter limits connections by adapter type.
	MaxConnectionsPerAdapter map[string]int

	// DefaultMaxConnections is the fallback limit (default: 10000).
	DefaultMaxConnections int

	// StaleCheckInterval is how often to check for stale clients (default: 30s).
	StaleCheckInterval time.Duration

	// OnClientDisconnect is called when a client is fully disconnected.
	OnClientDisconnect func(clientID string)
}

// DefaultConnectionTrackerConfig returns a config with sensible defaults.
func DefaultConnectionTrackerConfig() ConnectionTrackerConfig {
	return ConnectionTrackerConfig{
		MaxConnectionsPerAdapter: make(map[string]int),
		DefaultMaxConnections:    10000,
		StaleCheckInterval:       30 * time.Second,
	}
}

// ConnectionTracker manages client connections for lock lifecycle.
//
// Thread Safety:
// All operations are protected by a read-write mutex.
type ConnectionTracker struct {
	mu sync.RWMutex

	// clients maps ClientID to registration.
	clients map[string]*ClientRegistration

	// config holds the tracker configuration.
	config ConnectionTrackerConfig

	// disconnectTimers holds pending disconnect timers (for TTL > 0).
	disconnectTimers map[string]*time.Timer

	// adapterCounts tracks connections per adapter for limits.
	adapterCounts map[string]int
}

// NewConnectionTracker creates a new connection tracker.
func NewConnectionTracker(config ConnectionTrackerConfig) *ConnectionTracker {
	if config.DefaultMaxConnections == 0 {
		config.DefaultMaxConnections = 10000
	}
	if config.MaxConnectionsPerAdapter == nil {
		config.MaxConnectionsPerAdapter = make(map[string]int)
	}

	return &ConnectionTracker{
		clients:          make(map[string]*ClientRegistration),
		config:           config,
		disconnectTimers: make(map[string]*time.Timer),
		adapterCounts:    make(map[string]int),
	}
}

// RegisterClient registers a new client or updates an existing one.
// Returns ErrConnectionLimitReached if the limit is exceeded.
func (ct *ConnectionTracker) RegisterClient(clientID, adapterType, remoteAddr string, ttl time.Duration) error {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Check if client already exists (idempotent update)
	if existing, exists := ct.clients[clientID]; exists {
		existing.LastSeen = time.Now()
		existing.RemoteAddr = remoteAddr
		// Cancel any pending disconnect timer
		if timer, hasTimer := ct.disconnectTimers[clientID]; hasTimer {
			timer.Stop()
			delete(ct.disconnectTimers, clientID)
		}
		return nil
	}

	// Check connection limit for this adapter
	limit := ct.config.DefaultMaxConnections
	if adapterLimit, hasLimit := ct.config.MaxConnectionsPerAdapter[adapterType]; hasLimit {
		limit = adapterLimit
	}

	currentCount := ct.adapterCounts[adapterType]
	if currentCount >= limit {
		return &errors.StoreError{
			Code:    errors.ErrConnectionLimitReached,
			Message: "connection limit reached for adapter",
		}
	}

	// Create new registration
	now := time.Now()
	ct.clients[clientID] = &ClientRegistration{
		ClientID:     clientID,
		AdapterType:  adapterType,
		TTL:          ttl,
		RegisteredAt: now,
		LastSeen:     now,
		RemoteAddr:   remoteAddr,
		LockCount:    0,
	}

	ct.adapterCounts[adapterType]++
	return nil
}

// UnregisterClient removes a client, potentially with delayed lock cleanup.
func (ct *ConnectionTracker) UnregisterClient(clientID string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	client, exists := ct.clients[clientID]
	if !exists {
		return
	}

	adapterType := client.AdapterType
	ttl := client.TTL

	// Remove from clients map
	delete(ct.clients, clientID)
	if ct.adapterCounts[adapterType] > 0 {
		ct.adapterCounts[adapterType]--
	}

	// Handle disconnect callback
	if ttl == 0 {
		// Immediate release
		if ct.config.OnClientDisconnect != nil {
			go ct.config.OnClientDisconnect(clientID)
		}
	} else {
		// Deferred release - schedule timer
		timer := time.AfterFunc(ttl, func() {
			ct.mu.Lock()
			delete(ct.disconnectTimers, clientID)
			ct.mu.Unlock()

			if ct.config.OnClientDisconnect != nil {
				ct.config.OnClientDisconnect(clientID)
			}
		})
		ct.disconnectTimers[clientID] = timer
	}
}

// CancelDisconnect cancels a pending disconnect timer (client reconnected).
func (ct *ConnectionTracker) CancelDisconnect(clientID string) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if timer, exists := ct.disconnectTimers[clientID]; exists {
		timer.Stop()
		delete(ct.disconnectTimers, clientID)
		return true
	}
	return false
}

// UpdateLastSeen updates the last activity timestamp for a client.
func (ct *ConnectionTracker) UpdateLastSeen(clientID string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if client, exists := ct.clients[clientID]; exists {
		client.LastSeen = time.Now()
	}
}

// GetClient returns the registration for a client if it exists.
func (ct *ConnectionTracker) GetClient(clientID string) (*ClientRegistration, bool) {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	client, exists := ct.clients[clientID]
	if !exists {
		return nil, false
	}

	// Return a copy to prevent modification
	clientCopy := *client
	return &clientCopy, true
}

// ListClients returns all clients, optionally filtered by adapter type.
// Pass empty string for adapterType to get all clients.
func (ct *ConnectionTracker) ListClients(adapterType string) []*ClientRegistration {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	var result []*ClientRegistration
	for _, client := range ct.clients {
		if adapterType == "" || client.AdapterType == adapterType {
			clientCopy := *client
			result = append(result, &clientCopy)
		}
	}
	return result
}

// GetClientCount returns the number of clients, optionally filtered by adapter.
// Pass empty string for adapterType to get total count.
func (ct *ConnectionTracker) GetClientCount(adapterType string) int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	if adapterType == "" {
		return len(ct.clients)
	}
	return ct.adapterCounts[adapterType]
}

// IncrementLockCount increments the lock count for a client.
func (ct *ConnectionTracker) IncrementLockCount(clientID string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if client, exists := ct.clients[clientID]; exists {
		client.LockCount++
	}
}

// DecrementLockCount decrements the lock count for a client.
func (ct *ConnectionTracker) DecrementLockCount(clientID string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if client, exists := ct.clients[clientID]; exists && client.LockCount > 0 {
		client.LockCount--
	}
}

// GetPendingDisconnectCount returns the number of pending disconnect timers.
func (ct *ConnectionTracker) GetPendingDisconnectCount() int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return len(ct.disconnectTimers)
}

// ============================================================================
// NSM-specific methods (Phase 3)
// ============================================================================

// UpdateNSMInfo updates NSM-specific fields for a client.
// Called after SM_MON to store monitoring callback details.
func (ct *ConnectionTracker) UpdateNSMInfo(clientID, monName string, priv [16]byte, callback *NSMCallback) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if client, exists := ct.clients[clientID]; exists {
		client.MonName = monName
		client.Priv = priv
		client.CallbackInfo = callback
	}
}

// UpdateSMState updates the NSM state counter for a client.
func (ct *ConnectionTracker) UpdateSMState(clientID string, state int32) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if client, exists := ct.clients[clientID]; exists {
		client.SMState = state
	}
}

// GetNSMClients returns all clients with NSM callback info (for SM_NOTIFY).
// Returns copies to prevent modification of internal state.
func (ct *ConnectionTracker) GetNSMClients() []*ClientRegistration {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	var result []*ClientRegistration
	for _, client := range ct.clients {
		if client.CallbackInfo != nil {
			clientCopy := *client
			// Deep copy the callback info
			callbackCopy := *client.CallbackInfo
			clientCopy.CallbackInfo = &callbackCopy
			result = append(result, &clientCopy)
		}
	}
	return result
}

// ClearNSMInfo removes NSM-specific fields for a client.
// Called after SM_UNMON to clear monitoring registration.
func (ct *ConnectionTracker) ClearNSMInfo(clientID string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if client, exists := ct.clients[clientID]; exists {
		client.MonName = ""
		client.Priv = [16]byte{}
		client.CallbackInfo = nil
	}
}

// Close cancels all pending disconnect timers and clears state.
func (ct *ConnectionTracker) Close() {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Cancel all pending disconnect timers
	for clientID, timer := range ct.disconnectTimers {
		timer.Stop()
		delete(ct.disconnectTimers, clientID)
	}

	// Clear all state
	ct.clients = make(map[string]*ClientRegistration)
	ct.adapterCounts = make(map[string]int)
}
