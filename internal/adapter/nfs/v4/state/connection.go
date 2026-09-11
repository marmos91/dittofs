package state

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// Connection Direction and Type
// ============================================================================

// ConnectionDirection represents the channel direction of a bound connection.
type ConnectionDirection uint8

const (
	// ConnDirFore indicates a fore-channel-only connection.
	ConnDirFore ConnectionDirection = iota + 1
	// ConnDirBack indicates a back-channel-only connection.
	ConnDirBack
	// ConnDirBoth indicates a connection used for both fore and back channels.
	ConnDirBoth
)

// String returns a human-readable representation of the connection direction.
func (d ConnectionDirection) String() string {
	switch d {
	case ConnDirFore:
		return "fore"
	case ConnDirBack:
		return "back"
	case ConnDirBoth:
		return "both"
	default:
		return fmt.Sprintf("unknown(%d)", d)
	}
}

// ConnectionType represents the transport type of a bound connection.
type ConnectionType uint8

const (
	// ConnTypeTCP indicates a TCP transport connection.
	ConnTypeTCP ConnectionType = iota
	// ConnTypeRDMA indicates an RDMA transport connection.
	ConnTypeRDMA
)

// String returns a human-readable representation of the connection type.
func (t ConnectionType) String() string {
	switch t {
	case ConnTypeTCP:
		return "TCP"
	case ConnTypeRDMA:
		return "RDMA"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}

// ============================================================================
// BoundConnection
// ============================================================================

// BoundConnection represents a TCP connection that has been bound to a session
// via BIND_CONN_TO_SESSION (RFC 8881 Section 18.34) or auto-bound during
// CREATE_SESSION.
type BoundConnection struct {
	// ConnectionID is the unique identifier assigned at TCP accept() time.
	ConnectionID uint64

	// SessionID is the session this connection is bound to.
	SessionID types.SessionId4

	// Direction is the channel direction for this binding.
	Direction ConnectionDirection

	// ConnType is the transport type (TCP or RDMA).
	ConnType ConnectionType

	// BoundAt is the time this connection was bound to the session.
	BoundAt time.Time

	// LastActivity is the time of the last request on this connection.
	LastActivity time.Time

	// Draining indicates the connection is being drained (no new requests).
	// When true, the server returns NFS4ERR_DELAY for new COMPOUND requests.
	Draining bool
}

// ============================================================================
// BindConnResult
// ============================================================================

// BindConnResult holds the result of a successful BindConnToSession operation.
type BindConnResult struct {
	// ServerDir is the server-assigned channel direction (CDFS4_FORE, CDFS4_BACK, CDFS4_BOTH).
	ServerDir uint32
}

// ============================================================================
// Direction Negotiation
// ============================================================================

// negotiateDirection implements the generous direction negotiation policy
// per RFC 8881 Section 18.34.
//
// The server grants the most permissive direction compatible with the
// client's request:
//   - CDFC4_FORE -> ConnDirFore, CDFS4_FORE
//   - CDFC4_BACK -> ConnDirBack, CDFS4_BACK
//   - CDFC4_FORE_OR_BOTH -> ConnDirBoth, CDFS4_BOTH
//   - CDFC4_BACK_OR_BOTH -> ConnDirBoth, CDFS4_BOTH
//   - default -> ConnDirFore, CDFS4_FORE
func negotiateDirection(clientDir uint32) (ConnectionDirection, uint32) {
	switch clientDir {
	case types.CDFC4_FORE:
		return ConnDirFore, types.CDFS4_FORE
	case types.CDFC4_BACK:
		return ConnDirBack, types.CDFS4_BACK
	case types.CDFC4_FORE_OR_BOTH:
		return ConnDirBoth, types.CDFS4_BOTH
	case types.CDFC4_BACK_OR_BOTH:
		return ConnDirBoth, types.CDFS4_BOTH
	default:
		return ConnDirFore, types.CDFS4_FORE
	}
}

func (sm *StateManager) BindConnToSession(connectionID uint64, sessionID types.SessionId4, clientDir uint32) (*BindConnResult, error) {
	// Validate session exists under sm.mu.RLock
	sm.mu.RLock()
	_, exists := sm.sessionsByID[sessionID]
	sm.mu.RUnlock()

	if !exists {
		return nil, ErrBadSession
	}

	// Negotiate direction
	direction, serverDir := negotiateDirection(clientDir)

	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	// If connection already bound to a different session, silently unbind
	if existing, ok := sm.connByID[connectionID]; ok && existing.SessionID != sessionID {
		sm.unbindConnectionLocked(connectionID)
	}

	// Check connection limit: count bindings for this session, allow rebind
	bindings := sm.connBySession[sessionID]
	isRebind := false
	for _, b := range bindings {
		if b.ConnectionID == connectionID {
			isRebind = true
			break
		}
	}
	if sm.maxConnsPerSession > 0 && !isRebind && len(bindings) >= sm.maxConnsPerSession {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_RESOURCE,
			Message: "per-session connection limit exceeded",
		}
	}

	// Fore-channel enforcement: if binding as back-only, ensure at least one
	// fore connection remains (excluding self in case of rebind)
	if direction == ConnDirBack {
		foreCount := 0
		for _, b := range bindings {
			if b.ConnectionID == connectionID {
				continue // skip self (rebind case)
			}
			if b.Direction == ConnDirFore || b.Direction == ConnDirBoth {
				foreCount++
			}
		}
		if foreCount == 0 {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_INVAL,
				Message: "cannot leave session with zero fore-channel connections",
			}
		}
	}

	now := time.Now()

	// Remove old binding for this connID from session list (rebind case)
	sm.removeConnFromSessionLocked(connectionID, sessionID)

	// Create or update binding
	binding := &BoundConnection{
		ConnectionID: connectionID,
		SessionID:    sessionID,
		Direction:    direction,
		ConnType:     ConnTypeTCP,
		BoundAt:      now,
		LastActivity: now,
	}

	sm.connByID[connectionID] = binding
	sm.connBySession[sessionID] = append(sm.connBySession[sessionID], binding)

	return &BindConnResult{ServerDir: serverDir}, nil
}

// UnbindConnection removes a connection binding from all tracking maps.
// Called on TCP disconnect cleanup.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) UnbindConnection(connectionID uint64) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	sm.unbindConnectionLocked(connectionID)
}

// unbindConnectionLocked removes a connection binding. Caller must hold sm.connMu.

func (sm *StateManager) unbindConnectionLocked(connectionID uint64) {
	binding, ok := sm.connByID[connectionID]
	if !ok {
		return
	}
	sessionID := binding.SessionID
	delete(sm.connByID, connectionID)
	sm.removeConnFromSessionLocked(connectionID, sessionID)

	// Clean up backchannel state for this connection
	delete(sm.connWriters, connectionID)
	delete(sm.cbRepliesByConn, connectionID)
}

// removeConnFromSessionLocked removes a connection from the session binding list.
// Caller must hold sm.connMu.

func (sm *StateManager) removeConnFromSessionLocked(connectionID uint64, sessionID types.SessionId4) {
	bindings := sm.connBySession[sessionID]
	for i, b := range bindings {
		if b.ConnectionID == connectionID {
			sm.connBySession[sessionID] = append(bindings[:i], bindings[i+1:]...)
			break
		}
	}
	// Clean up empty slice
	if len(sm.connBySession[sessionID]) == 0 {
		delete(sm.connBySession, sessionID)
	}
}

// GetConnectionBindings returns a copy of all connection bindings for a session.
//
// Thread-safe: acquires sm.connMu.RLock.

func (sm *StateManager) GetConnectionBindings(sessionID types.SessionId4) []*BoundConnection {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	bindings := sm.connBySession[sessionID]
	if len(bindings) == 0 {
		return nil
	}

	result := make([]*BoundConnection, len(bindings))
	for i, b := range bindings {
		copied := *b
		result[i] = &copied
	}
	return result
}

// GetConnectionBinding returns a copy of the binding for a specific connection.
//
// Thread-safe: acquires sm.connMu.RLock.

func (sm *StateManager) GetConnectionBinding(connectionID uint64) *BoundConnection {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	binding, ok := sm.connByID[connectionID]
	if !ok {
		return nil
	}
	copied := *binding
	return &copied
}

// UpdateConnectionActivity updates the LastActivity timestamp for a connection.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) UpdateConnectionActivity(connectionID uint64) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	if binding, ok := sm.connByID[connectionID]; ok {
		binding.LastActivity = time.Now()
	}
}

// SetConnectionDraining sets the draining flag on a connection.
// When draining, the server returns NFS4ERR_DELAY for new requests.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) SetConnectionDraining(connectionID uint64, draining bool) error {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	binding, ok := sm.connByID[connectionID]
	if !ok {
		return fmt.Errorf("connection %d not found", connectionID)
	}
	binding.Draining = draining
	return nil
}

// IsConnectionDraining returns true if the connection is being drained.
//
// Thread-safe: acquires sm.connMu.RLock.

func (sm *StateManager) IsConnectionDraining(connectionID uint64) bool {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	if binding, ok := sm.connByID[connectionID]; ok {
		return binding.Draining
	}
	return false
}

// SetMaxConnectionsPerSession sets the maximum number of connections per session.
// A value of 0 means unlimited (no limit enforced).
