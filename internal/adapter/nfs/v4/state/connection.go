package state

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// Connection Direction and Type
// ============================================================================

// carriesBackChannel reports whether a binding with this direction can carry
// the back channel (RFC 8881 Section 2.10.3): CDFC4_BACK and CDFC4_FORE_OR_BOTH
// negotiate to ConnDirBack and ConnDirBoth respectively.
func carriesBackChannel(d ConnectionDirection) bool {
	return d == ConnDirBack || d == ConnDirBoth
}

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

// BindConnToSession associates a TCP connection with a session.
//
// Per RFC 8881 Section 18.34, the server:
//   - Validates the session exists
//   - Negotiates the channel direction (generous policy)
//   - Rebinds in place when the connection is already bound to this session
//   - Enforces a per-session connection limit (NFS4ERR_DELAY)
//   - Ensures at least one fore-channel connection remains (NFS4ERR_INVAL)
//
// Binding leaves the connection's other session bindings alone: RFC 8881
// Section 2.10.3.1 states a connection's association with a session is not
// exclusive, and a client that runs several sessions over one connection
// would otherwise lose the back channel of every session but the newest.
//
// Thread-safe: acquires sm.mu.RLock then sm.connMu.Lock, then sm.mu.Lock.
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

	result, err := sm.bindConnToSessionLocked(connectionID, sessionID, direction, serverDir)
	if err != nil {
		return nil, err
	}

	// A rebind to a fore-only direction retires a callback route as surely as a
	// closing socket does, and it may have been the client's last. The client
	// record lives under sm.mu, which is taken before connMu and never after it,
	// so the verdict is re-derived here rather than inside the bind. It is a
	// no-op whenever a back-bound connection remains, which is every bind that
	// adds a route.
	sm.clearCBPathWithoutBackBinding([]types.SessionId4{sessionID})

	return result, nil
}

// bindConnToSessionLocked installs the binding. Caller must hold neither
// sm.connMu nor sm.mu; it takes sm.connMu for the whole of the update.
func (sm *StateManager) bindConnToSessionLocked(
	connectionID uint64,
	sessionID types.SessionId4,
	direction ConnectionDirection,
	serverDir uint32,
) (*BindConnResult, error) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

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
		// NFS4ERR_RESOURCE is an NFSv4.0-only error (RFC 7530 Section 13.1.3.4);
		// it is absent from the NFSv4.1 error registry and from
		// BIND_CONN_TO_SESSION's valid-error list in RFC 8881 Section 15.2. The
		// limit clears as soon as one of the session's existing connections
		// drops, so the answer is the retryable NFS4ERR_DELAY rather than
		// NFS4ERR_SERVERFAULT, which clients translate to a fatal EIO.
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_DELAY,
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

	// A rebind away from the back channel leaves this session no callback
	// route over this connection, so the waiters it registered on the shared
	// table would otherwise stay armed with nothing able to answer them. This
	// path removes the old binding directly rather than through
	// dropConnBindingLocked — which would release the writer it is about to
	// re-register and leave the rebound connection mute — so the cancellation
	// has to be done here. Both back-capable directions keep their route and
	// their waiters.
	if direction != ConnDirBack && direction != ConnDirBoth {
		if pending := sm.cbRepliesByConn[connectionID]; pending != nil {
			pending.CancelSession(sessionID)
		}
	}

	// Remove the old binding for this (connection, session) pair (rebind case)
	sm.removeConnFromSessionLocked(connectionID, sessionID)
	sm.removeConnBindingLocked(connectionID, sessionID)

	// Create or update binding
	binding := &BoundConnection{
		ConnectionID: connectionID,
		SessionID:    sessionID,
		Direction:    direction,
		ConnType:     ConnTypeTCP,
		BoundAt:      now,
		LastActivity: now,
	}

	sm.connByID[connectionID] = append(sm.connByID[connectionID], binding)
	sm.connBySession[sessionID] = append(sm.connBySession[sessionID], binding)

	// A rebind that leaves no back-capable binding strands the callback state
	// the same way dropping the last one does, and the connection's other
	// sessions are part of the question: one of them may still carry the back
	// channel even though this rebind does not.
	if !sm.connHasBackChannelBindingLocked(connectionID) {
		sm.releaseBackchannelStateLocked(connectionID)
	}

	return &BindConnResult{ServerDir: serverDir}, nil
}

// UnbindConnection removes a connection binding from all tracking maps.
// Called on TCP disconnect cleanup.
//
// The connection may have been the last one able to carry a callback for its
// client, which is a client-wide fact recorded under a different lock, so the
// sessions it served are collected here and the verdict re-derived once connMu
// is released.
//
// Thread-safe: acquires sm.connMu.Lock, then sm.mu.Lock.

func (sm *StateManager) UnbindConnection(connectionID uint64) {
	sm.connMu.Lock()
	sessionIDs := sm.unbindConnectionLocked(connectionID)
	sm.connMu.Unlock()

	sm.clearCBPathWithoutBackBinding(sessionIDs)
}

// unbindConnectionLocked removes a connection binding and returns the sessions
// it was serving. Caller must hold sm.connMu.

func (sm *StateManager) unbindConnectionLocked(connectionID uint64) []types.SessionId4 {
	// Backchannel state goes first, ahead of the binding lookup, because it
	// outlives the bindings. Destroying a session drops its bindings one at a
	// time, and dropping the last one removes the connection from the index
	// entirely; the socket then closes into this function, finds no bindings and
	// would return with the writer closure and the pending-reply demultiplexer
	// still held for the life of the state manager. This is where the connection
	// actually dies, so this is where they are released.
	sm.releaseBackchannelStateLocked(connectionID)

	bindings, ok := sm.connByID[connectionID]
	if !ok {
		return nil
	}
	delete(sm.connByID, connectionID)
	sessionIDs := make([]types.SessionId4, 0, len(bindings))
	for _, b := range bindings {
		sm.removeConnFromSessionLocked(connectionID, b.SessionID)
		sessionIDs = append(sessionIDs, b.SessionID)
	}
	return sessionIDs
}

// releaseBackchannelStateLocked drops a connection's callback writer and fails
// every caller waiting on a reply over it. Caller must hold sm.connMu.
//
// Every path that retires a connection routes through here, not only the socket
// close: a binding reaped as orphaned can be the connection's last one, and the
// writer and demultiplexer left behind would outlive the socket they name.
//
// The waiters are released before the table that routes to them is dropped.
// Dropping it alone only makes the replies unroutable; whoever is already
// waiting stays blocked until its own timeout, and the recall behind it waits
// with it.
func (sm *StateManager) releaseBackchannelStateLocked(connectionID uint64) {
	if pending := sm.cbRepliesByConn[connectionID]; pending != nil {
		pending.FailAll()
	}
	delete(sm.connWriters, connectionID)
	delete(sm.cbRepliesByConn, connectionID)
}

// dropConnBindingLocked removes one (connection, session) binding and releases
// the connection's callback state when no binding that can carry the back
// channel is left on it.
//
// Every path that tears a binding down goes through here — socket close,
// DESTROY_SESSION, and the reaper's orphan sweep — because the connection is
// equally gone in all three and the writer and pending-reply table are held per
// connection, not per binding. BindConnToSession is the one caller that removes
// a binding directly: its rebind re-adds one in the same breath, and releasing
// the writer it just registered would leave the rebound connection mute.
//
// The condition is direction, not presence. A connection may carry several
// sessions, so destroying the last back-bound one while a fore-only session
// stays would otherwise leave the writer and the pending-reply table installed
// for the life of the socket: nothing can carry a callback on it, late replies
// are consumed into a table nothing waits on, and it still ranks as a candidate
// for selection.
//
// Caller must hold sm.connMu.
func (sm *StateManager) dropConnBindingLocked(connectionID uint64, sessionID types.SessionId4) {
	sm.removeConnBindingLocked(connectionID, sessionID)
	sm.removeConnFromSessionLocked(connectionID, sessionID)
	if !sm.connHasBackChannelBindingLocked(connectionID) {
		sm.releaseBackchannelStateLocked(connectionID)
	}
}

// connHasBackChannelBindingLocked reports whether any binding the connection
// holds can carry the back channel. Caller must hold sm.connMu.
func (sm *StateManager) connHasBackChannelBindingLocked(connectionID uint64) bool {
	for _, b := range sm.connByID[connectionID] {
		if carriesBackChannel(b.Direction) {
			return true
		}
	}
	return false
}

// removeConnBindingLocked drops one (connection, session) binding from the
// connection index, leaving the connection's bindings to other sessions in
// place. Caller must hold sm.connMu.
func (sm *StateManager) removeConnBindingLocked(connectionID uint64, sessionID types.SessionId4) {
	bindings := sm.connByID[connectionID]
	for i, b := range bindings {
		if b.SessionID == sessionID {
			bindings = append(bindings[:i], bindings[i+1:]...)
			break
		}
	}
	if len(bindings) == 0 {
		delete(sm.connByID, connectionID)
		return
	}
	sm.connByID[connectionID] = bindings
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

// GetConnectionBinding returns a copy of the most recent binding for a
// specific connection, or nil when the connection is bound to no session.
// A connection may carry several sessions; use GetConnectionBindingsForConn
// when every one of them matters.
//
// Thread-safe: acquires sm.connMu.RLock.

func (sm *StateManager) GetConnectionBinding(connectionID uint64) *BoundConnection {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	bindings := sm.connByID[connectionID]
	if len(bindings) == 0 {
		return nil
	}
	copied := *bindings[len(bindings)-1]
	return &copied
}

// GetConnectionBindingsForConn returns a copy of every binding a connection
// holds, one per session it carries.
//
// Thread-safe: acquires sm.connMu.RLock.

func (sm *StateManager) GetConnectionBindingsForConn(connectionID uint64) []*BoundConnection {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	bindings := sm.connByID[connectionID]
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

// UpdateConnectionActivity updates the LastActivity timestamp for a connection.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) UpdateConnectionActivity(connectionID uint64) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	now := time.Now()
	for _, binding := range sm.connByID[connectionID] {
		binding.LastActivity = now
	}
}

// SetConnectionDraining sets the draining flag on a connection.
// When draining, the server returns NFS4ERR_DELAY for new requests.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) SetConnectionDraining(connectionID uint64, draining bool) error {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	bindings := sm.connByID[connectionID]
	if len(bindings) == 0 {
		return fmt.Errorf("connection %d not found", connectionID)
	}
	for _, binding := range bindings {
		binding.Draining = draining
	}
	return nil
}

// IsConnectionDraining returns true if the connection is being drained.
//
// Thread-safe: acquires sm.connMu.RLock.

func (sm *StateManager) IsConnectionDraining(connectionID uint64) bool {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	for _, binding := range sm.connByID[connectionID] {
		if binding.Draining {
			return true
		}
	}
	return false
}
