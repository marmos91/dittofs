// Package clients provides a thread-safe registry for tracking connected
// protocol clients (NFS, SMB) with automatic TTL-based stale cleanup.
package clients

import (
	"context"
	"slices"
	"sync"
	"time"
)

// DefaultTTL is the default time after which inactive client records are
// removed by the background sweeper.
const DefaultTTL = 5 * time.Minute

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

// Registry provides thread-safe client tracking with TTL-based stale cleanup.
// It follows the same sub-service pattern as mounts.Tracker.
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*ClientRecord // keyed by ClientID
	ttl     time.Duration
	stopCh  chan struct{}
}

// NewRegistry creates a new client registry. If ttl is zero, DefaultTTL is used.
func NewRegistry(ttl time.Duration) *Registry {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	return &Registry{
		clients: make(map[string]*ClientRecord),
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

	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[record.ClientID] = record
}

// Deregister removes and returns a client record. Returns nil if not found.
func (r *Registry) Deregister(clientID string) *ClientRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.clients[clientID]
	if !ok {
		return nil
	}
	delete(r.clients, clientID)
	return rec
}

// Get returns a deep copy of the client record, or nil if not found.
func (r *Registry) Get(clientID string) *ClientRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rec, ok := r.clients[clientID]
	if !ok {
		return nil
	}
	return copyRecord(rec)
}

// UpdateActivity updates the LastActivity timestamp for a client.
// No-op if the client does not exist.
func (r *Registry) UpdateActivity(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if rec, ok := r.clients[clientID]; ok {
		rec.LastActivity = time.Now()
	}
}

// AddShare adds a share to the client's shares list if not already present.
// No-op if the client does not exist.
func (r *Registry) AddShare(clientID, share string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.clients[clientID]
	if !ok {
		return
	}
	if !slices.Contains(rec.Shares, share) {
		rec.Shares = append(rec.Shares, share)
	}
}

// RemoveShare removes a share from the client's shares list.
// No-op if the client or share does not exist.
func (r *Registry) RemoveShare(clientID, share string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.clients[clientID]
	if !ok {
		return
	}
	for i, s := range rec.Shares {
		if s == share {
			rec.Shares = append(rec.Shares[:i], rec.Shares[i+1:]...)
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
	for _, rec := range r.clients {
		if filter != nil && !filter(rec) {
			continue
		}
		result = append(result, copyRecord(rec))
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

	now := time.Now()
	for id, rec := range r.clients {
		if now.Sub(rec.LastActivity) > r.ttl {
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

// copyRecord creates a deep copy of a ClientRecord, including the Shares
// slice and protocol detail structs.
func copyRecord(src *ClientRecord) *ClientRecord {
	dst := *src
	// Deep copy Shares slice.
	if src.Shares != nil {
		dst.Shares = append([]string(nil), src.Shares...)
	}
	// Deep copy protocol details.
	if src.NFS != nil {
		nfsCopy := *src.NFS
		dst.NFS = &nfsCopy
	}
	if src.SMB != nil {
		smbCopy := *src.SMB
		dst.SMB = &smbCopy
	}
	return &dst
}
