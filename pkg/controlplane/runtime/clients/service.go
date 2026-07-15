// Package clients provides a thread-safe registry for tracking connected
// protocol clients (NFS, SMB) with automatic TTL-based stale cleanup.
package clients

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTTL is the default time after which inactive client records are
// removed by the background sweeper.
const DefaultTTL = 30 * time.Minute

// ClientRecord represents a connected protocol client.
type ClientRecord struct {
	ClientID     string      `json:"client_id"`
	Protocol     string      `json:"protocol"` // "nfs" or "smb"
	Address      string      `json:"address"`
	User         string      `json:"user"`
	ConnectedAt  time.Time   `json:"connected_at"`
	LastActivity time.Time   `json:"last_activity"`
	Shares       []string    `json:"shares"`
	NFS          *NfsDetails `json:"nfs,omitempty"`
	SMB          *SmbDetails `json:"smb,omitempty"`
}

// NfsDetails holds NFS-specific client information.
type NfsDetails struct {
	Version    string `json:"version"`     // "3", "4.0", "4.1"
	AuthFlavor string `json:"auth_flavor"` // "AUTH_UNIX", "AUTH_NULL", "RPCSEC_GSS"
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
}

// SmbDetails holds SMB-specific client information.
type SmbDetails struct {
	SessionID uint64 `json:"session_id"`
	Dialect   string `json:"dialect"` // "3.1.1", "3.0.2", "3.0", "2.1", "2.0.2"
	Domain    string `json:"domain,omitempty"`
	Signed    bool   `json:"signed"`
	Encrypted bool   `json:"encrypted"`
}

// clientEntry is the map value. It holds the record's structural fields plus a
// lock-free last-activity timestamp. The timestamp is bumped on every request,
// so it lives in an atomic instead of under the registry write lock.
type clientEntry struct {
	ClientRecord
	lastActivity atomic.Int64 // Unix nanos; hot path, lock-free
}

// Registry provides thread-safe client tracking with TTL-based stale cleanup.
// It follows the same sub-service pattern as mounts.Tracker.
//
// mu guards the map structure and each entry's non-atomic fields (Shares).
// The per-request activity bump does not take mu — it stores into the entry's
// atomic timestamp, so the highest-fan-in call in the system never contends.
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*clientEntry // keyed by ClientID
	ttl     time.Duration
	stopCh  chan struct{}
}

// NewRegistry creates a new client registry. If ttl is zero, DefaultTTL is used.
func NewRegistry(ttl time.Duration) *Registry {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	return &Registry{
		clients: make(map[string]*clientEntry),
		ttl:     ttl,
		stopCh:  make(chan struct{}),
	}
}

// Register stores a client record. If ConnectedAt or LastActivity are zero,
// they are set to the current time.
func (r *Registry) Register(record *ClientRecord) {
	now := time.Now()
	if record.ConnectedAt.IsZero() {
		record.ConnectedAt = now
	}
	if record.LastActivity.IsZero() {
		record.LastActivity = now
	}

	e := &clientEntry{ClientRecord: *record}
	e.lastActivity.Store(record.LastActivity.UnixNano())

	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[record.ClientID] = e
}

// Deregister removes and returns a client record. Returns nil if not found.
func (r *Registry) Deregister(clientID string) *ClientRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.clients[clientID]
	if !ok {
		return nil
	}
	delete(r.clients, clientID)
	return copyRecord(e)
}

// Get returns a deep copy of the client record, or nil if not found.
func (r *Registry) Get(clientID string) *ClientRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.clients[clientID]
	if !ok {
		return nil
	}
	return copyRecord(e)
}

// UpdateActivity bumps the LastActivity timestamp for a client. Called on every
// NFS/SMB request, so it takes only a read lock (map safety) and stores into the
// per-client atomic — no write-lock convoy. No-op if the client does not exist.
func (r *Registry) UpdateActivity(clientID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if e, ok := r.clients[clientID]; ok {
		e.lastActivity.Store(time.Now().UnixNano())
	}
}

// AddShare adds a share to the client's shares list if not already present.
// No-op if the client does not exist.
func (r *Registry) AddShare(clientID, share string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.clients[clientID]
	if !ok {
		return
	}
	if !slices.Contains(e.Shares, share) {
		e.Shares = append(e.Shares, share)
	}
}

// RemoveShare removes a share from the client's shares list.
// No-op if the client or share does not exist.
func (r *Registry) RemoveShare(clientID, share string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.clients[clientID]
	if !ok {
		return
	}
	for i, s := range e.Shares {
		if s == share {
			e.Shares = append(e.Shares[:i], e.Shares[i+1:]...)
			return
		}
	}
}

// List returns deep copies of all client records.
func (r *Registry) List() []*ClientRecord {
	return r.collect(nil)
}

// ListByProtocol returns deep copies of client records filtered by protocol.
func (r *Registry) ListByProtocol(protocol string) []*ClientRecord {
	return r.collect(func(c *ClientRecord) bool {
		return c.Protocol == protocol
	})
}

// ListByShare returns deep copies of client records that are connected to the
// given share.
func (r *Registry) ListByShare(share string) []*ClientRecord {
	return r.collect(func(c *ClientRecord) bool {
		return slices.Contains(c.Shares, share)
	})
}

// Count returns the number of registered clients.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// collect returns deep copies of records matching the filter.
// If filter is nil, all records are returned.
func (r *Registry) collect(filter func(*ClientRecord) bool) []*ClientRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*ClientRecord, 0, len(r.clients))
	for _, e := range r.clients {
		if filter != nil && !filter(&e.ClientRecord) {
			continue
		}
		result = append(result, copyRecord(e))
	}
	return result
}

// StartSweeper starts a background goroutine that periodically removes stale
// client records. The sweep interval is ttl/2. The goroutine stops when ctx
// is cancelled or Stop() is called.
func (r *Registry) StartSweeper(ctx context.Context) {
	interval := max(r.ttl/2, time.Millisecond)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			case <-ticker.C:
				r.sweep()
			}
		}
	}()
}

// sweep removes client records whose LastActivity exceeds the TTL.
func (r *Registry) sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UnixNano()
	for id, e := range r.clients {
		if now-e.lastActivity.Load() > int64(r.ttl) {
			delete(r.clients, id)
		}
	}
}

// Stop signals the sweeper goroutine to stop.
func (r *Registry) Stop() {
	select {
	case <-r.stopCh:
		// Already closed.
	default:
		close(r.stopCh)
	}
}

// copyRecord creates a deep copy of an entry's ClientRecord, filling
// LastActivity from the atomic timestamp and deep-copying the Shares slice and
// protocol detail structs.
func copyRecord(e *clientEntry) *ClientRecord {
	dst := e.ClientRecord
	dst.LastActivity = time.Unix(0, e.lastActivity.Load())
	if e.Shares != nil {
		dst.Shares = append([]string(nil), e.Shares...)
	}
	if e.NFS != nil {
		nfsCopy := *e.NFS
		dst.NFS = &nfsCopy
	}
	if e.SMB != nil {
		smbCopy := *e.SMB
		dst.SMB = &smbCopy
	}
	return &dst
}
