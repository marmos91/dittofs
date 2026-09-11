package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// Session represents an NFSv4.1 session per RFC 8881 Section 2.10.
//
// A session ties together a session ID, client ID, fore/back channel
// slot tables, and negotiated channel attributes. Sessions are created
// by the CREATE_SESSION operation and looked up by ID in the SEQUENCE
// operation.
//
// This struct is intentionally independent of StateManager -- the
// CREATE_SESSION handler creates a Session via NewSession and then
// registers it with the manager separately.
type Session struct {
	// SessionID is the unique 16-byte session identifier (crypto/rand generated).
	SessionID types.SessionId4

	// ClientID is the server-assigned client ID that owns this session.
	ClientID uint64

	// ForeChannelSlots is the slot table for fore channel (client -> server).
	ForeChannelSlots *SlotTable

	// BackChannelSlots is the slot table for back channel (server -> client).
	// nil if no back channel was requested.
	BackChannelSlots *SlotTable

	// ForeChannelAttrs holds the negotiated fore channel attributes.
	ForeChannelAttrs types.ChannelAttrs

	// BackChannelAttrs holds the negotiated back channel attributes.
	BackChannelAttrs types.ChannelAttrs

	// Flags holds the CREATE_SESSION flags (e.g., CREATE_SESSION4_FLAG_CONN_BACK_CHAN).
	Flags uint32

	// CbProgram is the callback RPC program number from CREATE_SESSION.
	CbProgram uint32

	// CreatedAt is when this session was created.
	CreatedAt time.Time

	// ============================================================================
	// Backchannel State
	// ============================================================================

	// BackchannelSecParms stores the callback security parameters from
	// BACKCHANNEL_CTL. Updated when the client sends BACKCHANNEL_CTL.
	BackchannelSecParms []types.CallbackSecParms4

	// backchannelSender is the goroutine that sends callbacks over back-bound
	// connections. nil until first backchannel operation or first back-channel bind.
	backchannelSender *BackchannelSender
}

// NewSession creates a new Session with a crypto/rand-generated session ID,
// fore channel slot table, and optionally a back channel slot table if the
// CREATE_SESSION4_FLAG_CONN_BACK_CHAN flag is set.
//
// The fore channel slot table is always created from foreAttrs.MaxRequests.
// The back channel slot table is only created when flags includes
// CREATE_SESSION4_FLAG_CONN_BACK_CHAN.
//
// NewSlotTable clamps the slot count to [MinSlots, DefaultMaxSlots].
//
// This constructor does NOT register the session with StateManager.
// Registration is the CREATE_SESSION handler's responsibility.
func NewSession(clientID uint64, foreAttrs, backAttrs types.ChannelAttrs, flags, cbProgram uint32) (*Session, error) {
	var sid types.SessionId4

	// Generate a random 16-byte session ID using crypto/rand.
	// Session IDs are protocol-visible identifiers; predictable values could
	// allow session hijacking, so we fail rather than fall back to a weak source.
	if _, err := rand.Read(sid[:]); err != nil {
		return nil, fmt.Errorf("failed to generate session ID: %w", err)
	}

	s := &Session{
		SessionID:        sid,
		ClientID:         clientID,
		ForeChannelAttrs: foreAttrs,
		BackChannelAttrs: backAttrs,
		Flags:            flags,
		CbProgram:        cbProgram,
		CreatedAt:        time.Now(),
	}

	// Always create fore channel slot table.
	s.ForeChannelSlots = NewSlotTable(foreAttrs.MaxRequests)

	// Create back channel slot table only if CONN_BACK_CHAN flag is set.
	if flags&types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN != 0 {
		s.BackChannelSlots = NewSlotTable(backAttrs.MaxRequests)
	}

	return s, nil
}

// HasInFlightRequests returns true if the session's fore channel slot table
// has any slots currently in use (processing a request).
func (s *Session) HasInFlightRequests() bool {
	if s.ForeChannelSlots == nil {
		return false
	}
	return s.ForeChannelSlots.HasInFlightRequests()
}

// CreateSession implements the CREATE_SESSION algorithm per RFC 8881 Section 18.36.
//
// The algorithm uses the client's sequence ID to detect replays:
//   - sequenceID == record.SequenceID: replay -- return cached response
//   - sequenceID == record.SequenceID + 1: new request -- create session
//   - otherwise: misordered -- return error
//
// On success, returns the CreateSessionResult and nil cached bytes. The encoded
// XDR response is also cached on the client record under sm.mu in the same
// critical section as the sequence-ID bump, so a concurrent retransmit that
// matches record.SequenceID always observes a populated cache (RFC 8881
// Section 18.36 replay requirement) — there is no window where the seqid has
// advanced but the cache is still nil.
// On replay, returns nil result and the cached XDR response bytes.
// On error, returns an appropriate NFS4StateError.
//
// The first successful CREATE_SESSION confirms the client and starts its lease.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) CreateSession(
	clientID uint64,
	sequenceID uint32,
	flags uint32,
	foreAttrs, backAttrs types.ChannelAttrs,
	cbProgram uint32,
	cbSecParms []types.CallbackSecParms4,
	principal ...string,
) (*CreateSessionResult, []byte, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Case 1: Unknown client (or an ID a v4.0 record owns: the two flows draw
	// from one sequence, so a CREATE_SESSION can never reach a SETCLIENTID
	// record, but the version filter keeps that invariant explicit)
	record := sm.v41ClientLocked(clientID)
	if record == nil {
		return nil, nil, ErrStaleClientID
	}

	// An unconfirmed record not confirmed within a lease period is gone, so its
	// client ID no longer resolves (RFC 8881 Section 18.35.4). Checked here as
	// well as in the reaper so the client ID stops working the moment the lease
	// has passed rather than at the next sweep.
	if !record.Confirmed && time.Since(record.CreatedAt) > sm.leaseDuration {
		logger.Debug("CREATE_SESSION: unconfirmed client record expired",
			"client_id", fmt.Sprintf("0x%x", clientID),
			"age", time.Since(record.CreatedAt).String())
		sm.purgeV41Client(record)
		return nil, nil, ErrStaleClientID
	}

	// Confirming a record is where its principal is bound, so a confirmation
	// from another principal is a client-ID collision rather than the expected
	// confirmation, and nothing on the server changes (RFC 8881 Section
	// 18.36.3). A record already confirmed skips the confirmation phase
	// entirely, which is why a later principal change is allowed.
	if !record.Confirmed && principalHijacks(record.Principal, firstOrEmpty(principal)) {
		logger.Debug("CREATE_SESSION: confirmation attempted by another principal",
			"client_id", fmt.Sprintf("0x%x", clientID))
		return nil, nil, ErrClientIDInUse
	}

	// Case 2: Replay (same seqid)
	if sequenceID == record.SequenceID {
		if record.CachedCreateSessionRes == nil {
			return nil, nil, ErrSeqMisordered
		}
		return nil, record.CachedCreateSessionRes, nil
	}

	// Case 4: Misordered (not seqid+1)
	if sequenceID != record.SequenceID+1 {
		return nil, nil, ErrSeqMisordered
	}

	// Case 3: New request (seqid == record.SequenceID + 1)

	// A channel budget from which no COMPOUND could ever be sent must be
	// rejected before any session state is allocated: accepting it would arm a
	// slot table, a reply cache, and a lease for a channel that can never carry
	// traffic, which is the resource leak the conformance suite's TOOSMALL rows
	// probe. Negotiation itself only clamps downward from the server max and
	// has no floor, so the floor check runs ahead of it.
	if err := channelAttrsTooSmall(foreAttrs); err != nil {
		return nil, nil, err
	}
	if err := channelAttrsTooSmall(backAttrs); err != nil {
		return nil, nil, err
	}

	// Unknown flag bits draw NFS4ERR_INVAL because that is the answer the
	// conformance suite expects (CSESS15); RFC 8881 Section 18.36.3 defines
	// exactly three flag bits (PERSIST, CONN_BACK_CHAN, CONN_RDMA) and does
	// not specify handling for unrecognized ones, so returning INVAL instead
	// of silently masking is a deliberate choice: masking would let a client
	// believe it negotiated PERSIST or RDMA support it did not get. An
	// extension that adds a new flag bit (RFC 8178 sanctions adding bits to
	// flag fields) must extend this check.
	const knownFlags = uint32(types.CREATE_SESSION4_FLAG_PERSIST |
		types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN |
		types.CREATE_SESSION4_FLAG_CONN_RDMA)
	if flags&^knownFlags != 0 {
		return nil, nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: fmt.Sprintf("unknown CREATE_SESSION flag bits 0x%08x", flags&^knownFlags),
		}
	}

	// Check per-client session limit
	if len(sm.sessionsByClientID[clientID]) >= sm.maxSessionsPerClient {
		return nil, nil, ErrTooManySessions
	}

	// Negotiate channel attributes
	foreLimits := DefaultForeLimits()
	foreLimits.MaxSlots = sm.foreMaxSlots
	negotiatedFore := negotiateChannelAttrs(foreAttrs, foreLimits)
	negotiatedBack := negotiateChannelAttrs(backAttrs, DefaultBackLimits())

	// Compute response flags: clear PERSIST, set CONN_BACK_CHAN if requested
	responseFlags := flags & ^uint32(types.CREATE_SESSION4_FLAG_PERSIST)
	// Also clear CONN_RDMA (we don't support RDMA)
	responseFlags = responseFlags & ^uint32(types.CREATE_SESSION4_FLAG_CONN_RDMA)

	// Create session
	session, err := NewSession(clientID, negotiatedFore, negotiatedBack, responseFlags, cbProgram)
	if err != nil {
		return nil, nil, &NFS4StateError{
			Status:  types.NFS4ERR_SERVERFAULT,
			Message: fmt.Sprintf("failed to create session: %v", err),
		}
	}

	// Store session in maps
	sm.sessionsByID[session.SessionID] = session
	sm.sessionsByClientID[clientID] = append(sm.sessionsByClientID[clientID], session)

	// First CREATE_SESSION confirms the client
	if !record.Confirmed {
		// A record established by the client-restart case replaces the confirmed
		// record it superseded, which is destroyed here now that the session is
		// created (RFC 8881 Section 18.36.3).
		sm.collapseSupersededLocked(record)

		record.Confirmed = true
		record.Lease = NewLeaseState(record.ClientID, sm.leaseDuration, nil)
		record.LastRenewal = time.Now()

		// Persist a durable client-recovery record. v4.1 has no
		// nfs_client_id4 string; the stable identity is co_ownerid, so the
		// record is keyed by its string form. Best-effort under sm.mu.
		sm.persistClientRecoveryLocked(record.ClientID, v41RecoveryKey(record.OwnerID), record.Verifier, record.Principal)
	}

	// Increment sequence ID
	record.SequenceID++

	result := &CreateSessionResult{
		SessionID:        session.SessionID,
		SequenceID:       record.SequenceID,
		Flags:            responseFlags,
		ForeChannelAttrs: negotiatedFore,
		BackChannelAttrs: negotiatedBack,
	}

	// Encode and cache the XDR response under the same sm.mu critical section
	// as the seqid bump. This guarantees a retransmit matching record.SequenceID
	// always finds CachedCreateSessionRes populated (RFC 8881 Section 18.36),
	// closing the replay window that would otherwise return NFS4ERR_SEQ_MISORDERED.
	res := &types.CreateSessionRes{
		Status:           types.NFS4_OK,
		SessionID:        result.SessionID,
		SequenceID:       result.SequenceID,
		Flags:            result.Flags,
		ForeChannelAttrs: result.ForeChannelAttrs,
		BackChannelAttrs: result.BackChannelAttrs,
	}
	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		return nil, nil, &NFS4StateError{
			Status:  types.NFS4ERR_SERVERFAULT,
			Message: fmt.Sprintf("failed to encode CREATE_SESSION response: %v", err),
		}
	}
	cached := make([]byte, buf.Len())
	copy(cached, buf.Bytes())
	record.CachedCreateSessionRes = cached
	result.EncodedRes = cached

	logger.Info("CREATE_SESSION: session created",
		"client_id", fmt.Sprintf("0x%x", clientID),
		"session_id", session.SessionID.String(),
		"fore_slots", negotiatedFore.MaxRequests)

	return result, nil, nil
}

// CacheCreateSessionResponse stores the full XDR-encoded CREATE_SESSION response
// bytes on the client record for replay detection.
//
// The normal CREATE_SESSION path no longer needs this: CreateSession already
// encodes and caches the response atomically with the sequence-ID bump (see
// above), which is what closes the replay window. This method remains as an
// explicit override for the rare caller that wants to replace the cached bytes.
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) CacheCreateSessionResponse(clientID uint64, responseBytes []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record := sm.v41ClientLocked(clientID)
	if record == nil {
		return
	}

	cached := make([]byte, len(responseBytes))
	copy(cached, responseBytes)
	record.CachedCreateSessionRes = cached
}

// DestroySession removes a session from the state manager.
// Returns ErrBadSession if the session is not found, or ErrDelay if
// the session has in-flight requests.
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) DestroySession(sessionID types.SessionId4) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	return sm.destroySessionLocked(sessionID, false, "client_request")
}

// ForceDestroySession removes a session from the state manager, bypassing
// the in-flight request check. Used by admin eviction.
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) ForceDestroySession(sessionID types.SessionId4) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	return sm.destroySessionLocked(sessionID, true, "admin_evict")
}

// destroySessionLocked removes a session. Caller must hold sm.mu.
// If force is false, returns ErrDelay when the session has in-flight requests.

func (sm *StateManager) destroySessionLocked(sessionID types.SessionId4, force bool, reason string) error {
	session, exists := sm.sessionsByID[sessionID]
	if !exists {
		return ErrBadSession
	}

	// Check for in-flight requests (unless force-destroying)
	if !force && session.HasInFlightRequests() {
		return ErrDelay
	}

	// Stop backchannel sender before removing session (prevents orphan goroutines)
	sm.stopBackchannelSender(sessionID)

	// Remove from sessionsByID
	delete(sm.sessionsByID, sessionID)

	// Remove from sessionsByClientID
	sessions := sm.sessionsByClientID[session.ClientID]
	for i, s := range sessions {
		if s.SessionID == sessionID {
			sm.sessionsByClientID[session.ClientID] = append(sessions[:i], sessions[i+1:]...)
			break
		}
	}
	// Clean up empty slice
	if len(sm.sessionsByClientID[session.ClientID]) == 0 {
		delete(sm.sessionsByClientID, session.ClientID)
	}

	// Clean up connection bindings for this session.
	// Lock ordering: sm.mu (held by caller) before connMu.
	sm.connMu.Lock()
	for _, b := range sm.connBySession[sessionID] {
		delete(sm.connByID, b.ConnectionID)
	}
	delete(sm.connBySession, sessionID)
	sm.connMu.Unlock()

	logger.Info("Session destroyed",
		"session_id", session.SessionID.String(),
		"client_id", fmt.Sprintf("0x%x", session.ClientID),
		"reason", reason)

	return nil
}

// GetSession returns the session for the given session ID, or nil if not found.
// Thread-safe: acquires sm.mu.RLock.

func (sm *StateManager) GetSession(sessionID types.SessionId4) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return sm.sessionsByID[sessionID]
}

// ListSessionsForClient returns a copy of the session slice for the given client.
// Thread-safe: acquires sm.mu.RLock.

func (sm *StateManager) ListSessionsForClient(clientID uint64) []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessions := sm.sessionsByClientID[clientID]
	if len(sessions) == 0 {
		return nil
	}

	result := make([]*Session, len(sessions))
	copy(result, sessions)
	return result
}

// StartSessionReaper starts a background goroutine that periodically sweeps
// for expired client leases and unconfirmed clients, destroying their sessions.
//
// Callers (typically the NFS adapter startup path) MUST invoke this after
// constructing the StateManager, passing a context that is cancelled on
// shutdown. If not started, expired/unconfirmed v4.1 clients will never be reaped.
//
// The reaper runs every 30 seconds and checks:
//   - Clients with expired leases: destroys all sessions, purges client
//   - Unconfirmed clients older than the lease duration: purges client
//
// Stops when ctx is cancelled.

func (sm *StateManager) StartSessionReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sm.reapExpiredSessions()
			}
		}
	}()
}

// reapExpiredSessions checks for and cleans up expired/unconfirmed v4.1 clients.
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) reapExpiredSessions() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()

	// Collect client IDs to purge (avoid modifying map during iteration)
	var toPurge []*ClientRecord

	for _, record := range sm.clientsByID {
		if record.MinorVersion != 1 {
			continue
		}
		// Check lease expiry for confirmed clients
		if record.Lease != nil && record.Lease.IsExpired() {
			logger.Info("Session reaper: lease expired",
				"client_id", fmt.Sprintf("0x%x", record.ClientID),
				"client_addr", record.ClientAddr)

			// Defer all session teardown to purgeV41Client, which stops each
			// session's backchannel sender before deleting it. Deleting the
			// sessions here would empty sessionsByClientID and leak the senders.
			toPurge = append(toPurge, record)
			continue
		}

		// Check for unconfirmed clients that timed out. A record not confirmed
		// within a lease period is removed (RFC 8881 Section 18.35.4).
		if !record.Confirmed && now.Sub(record.CreatedAt) > sm.leaseDuration {
			logger.Info("Session reaper: unconfirmed client timed out",
				"client_id", fmt.Sprintf("0x%x", record.ClientID),
				"client_addr", record.ClientAddr,
				"age", now.Sub(record.CreatedAt).String())
			toPurge = append(toPurge, record)
		}
	}

	// Purge collected records
	for _, record := range toPurge {
		sm.purgeV41Client(record)
	}

	// Clean up orphaned connection bindings (connections referencing sessions
	// that no longer exist). This handles edge cases where a session was
	// destroyed but the connection was not yet unbound.
	sm.connMu.Lock()
	for connID, binding := range sm.connByID {
		if _, exists := sm.sessionsByID[binding.SessionID]; !exists {
			delete(sm.connByID, connID)
			sm.removeConnFromSessionLocked(connID, binding.SessionID)
		}
	}
	sm.connMu.Unlock()
}
