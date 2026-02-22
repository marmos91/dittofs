package state

import (
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// BuildDate is set via -ldflags "-X ...state.BuildDate=..." at compile time.
// Format: RFC 3339 (e.g. "2026-02-20T12:00:00Z"). Falls back to zero time if unset.
var BuildDate string

// ============================================================================
// V41 Client Record (separate from v4.0 ClientRecord)
// ============================================================================

// V41ClientRecord represents the server-side state for a single NFSv4.1 client
// registered via EXCHANGE_ID (op 42) per RFC 8881 Section 18.35.
//
// This is separate from the v4.0 ClientRecord because v4.1 uses a completely
// different client registration flow (EXCHANGE_ID -> CREATE_SESSION vs
// SETCLIENTID -> SETCLIENTID_CONFIRM).
type V41ClientRecord struct {
	// ClientID is the server-assigned 64-bit client identifier.
	ClientID uint64

	// OwnerID is the co_ownerid from client_owner4 (stored as bytes for byte-exact comparison).
	OwnerID []byte

	// Verifier is the co_verifier from client_owner4.
	Verifier [8]byte

	// ImplDomain is the implementation domain from nfs_impl_id4 (e.g. "kernel.org").
	ImplDomain string

	// ImplName is the implementation name from nfs_impl_id4 (e.g. "Linux NFS client").
	ImplName string

	// ImplDate is the build date from nfs_impl_id4.
	ImplDate time.Time

	// SequenceID is the eir_sequenceid -- starts at 1 for new clients.
	// Used by CREATE_SESSION (Phase 19) to detect replays.
	SequenceID uint32

	// Confirmed becomes true after CREATE_SESSION completes (Phase 19).
	// Always false in Phase 18.
	Confirmed bool

	// ClientAddr is the network address of the client (for logging/debugging).
	ClientAddr string

	// CreatedAt is when this record was created.
	CreatedAt time.Time

	// LastRenewal is the most recent lease renewal time.
	LastRenewal time.Time

	// Lease is the lease timer for this client (shared behavior via pointer, same as v4.0 pattern).
	Lease *LeaseState

	// CachedCreateSessionRes holds the full XDR-encoded CREATE_SESSION
	// response bytes for replay detection (RFC 8881 Section 18.36).
	// nil until the first successful CREATE_SESSION.
	CachedCreateSessionRes []byte
}

// ============================================================================
// Server Identity (immutable singleton)
// ============================================================================

// ServerIdentity holds the immutable server identification returned in every
// EXCHANGE_ID response. Created once at StateManager initialization.
//
// Per RFC 8881 Section 18.35:
//   - server_owner4 enables trunking detection (same major_id = same server)
//   - server_scope identifies the namespace boundary
//   - nfs_impl_id4 identifies the server implementation
type ServerIdentity struct {
	// ServerOwner identifies the server for trunking detection.
	// major_id = hostname, minor_id = bootEpoch as uint64.
	ServerOwner types.ServerOwner4

	// ServerScope identifies the namespace boundary (same as major_id).
	ServerScope []byte

	// ImplID identifies the server implementation.
	ImplID types.NfsImplId4
}

// newServerIdentity creates the immutable server identity singleton.
// Called once from NewStateManager.
func newServerIdentity(bootEpoch uint32) *ServerIdentity {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "dittofs-unknown"
	}

	// minor_id: bootEpoch as uint64
	minorID := uint64(bootEpoch)

	// Parse build date from ldflags, fallback to zero time
	var buildDate types.NFS4Time
	if BuildDate != "" {
		if t, err := time.Parse(time.RFC3339, BuildDate); err == nil {
			buildDate = types.NFS4Time{
				Seconds:  t.Unix(),
				Nseconds: uint32(t.Nanosecond()),
			}
		}
	}

	return &ServerIdentity{
		ServerOwner: types.ServerOwner4{
			MinorID: minorID,
			MajorID: []byte(hostname),
		},
		ServerScope: []byte(hostname),
		ImplID: types.NfsImplId4{
			Domain: "dittofs.io",
			Name:   "dittofs",
			Date:   buildDate,
		},
	}
}

// ============================================================================
// ExchangeID Result
// ============================================================================

// ExchangeIDResult holds the output of the ExchangeID algorithm for the handler
// to encode into the EXCHANGE_ID response.
type ExchangeIDResult struct {
	ClientID     uint64
	SequenceID   uint32
	Flags        uint32
	ServerOwner  types.ServerOwner4
	ServerScope  []byte
	ServerImplId []types.NfsImplId4
}

// ============================================================================
// ExchangeID Algorithm (RFC 8881 Section 18.35)
// ============================================================================

// ExchangeID implements the NFSv4.1 EXCHANGE_ID multi-case algorithm per
// RFC 8881 Section 18.35.
//
// The algorithm determines the action based on whether the server has an
// existing record for the client's owner ID:
//
//   - Case 1 (new client): No existing record -> create new client with fresh clientID
//   - Case 2 (same owner+verifier): Existing record with matching verifier -> idempotent return
//   - Case 3 (same owner, different verifier): Client reboot -> replace with fresh clientID
//   - Case 4 (unconfirmed record exists): Supersede unconfirmed record
//
// The flags parameter from the client is currently ignored; the server always
// sets EXCHGID4_FLAG_USE_NON_PNFS. EXCHGID4_FLAG_CONFIRMED_R is set only
// if the record has been confirmed via CREATE_SESSION.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) ExchangeID(
	ownerID []byte,
	verifier [8]byte,
	_ uint32,
	clientImplId []types.NfsImplId4,
	clientAddr string,
) (*ExchangeIDResult, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	existing := sm.v41ClientsByOwner[string(ownerID)]

	var record *V41ClientRecord

	switch {
	case existing == nil:
		// Case 1: New client -- no existing record for this owner
		record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr)
		logger.Info("EXCHANGE_ID: new v4.1 client registered",
			"client_id", record.ClientID,
			"client_addr", clientAddr)

	case existing.Verifier == verifier:
		// Case 2: Same owner + same verifier -- idempotent (update impl info)
		existing.ClientAddr = clientAddr
		existing.LastRenewal = time.Now()
		applyImplInfo(existing, clientImplId)
		record = existing
		logger.Debug("EXCHANGE_ID: idempotent return for existing v4.1 client",
			"client_id", record.ClientID,
			"client_addr", clientAddr)

	default:
		// Cases 3 & 4: Different verifier -- purge and replace.
		// Case 4 (unconfirmed) and Case 3 (confirmed reboot) take the same
		// action: discard the old record and create a fresh one.
		reason := "client reboot detected"
		if !existing.Confirmed {
			reason = "replaced unconfirmed"
		}
		sm.purgeV41Client(existing)
		record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr)
		logger.Info("EXCHANGE_ID: new v4.1 client",
			"reason", reason,
			"old_client_id", existing.ClientID,
			"new_client_id", record.ClientID,
			"client_addr", clientAddr)
	}

	// Build result flags
	resultFlags := uint32(types.EXCHGID4_FLAG_USE_NON_PNFS)
	if record.Confirmed {
		resultFlags |= types.EXCHGID4_FLAG_CONFIRMED_R
	}

	return &ExchangeIDResult{
		ClientID:     record.ClientID,
		SequenceID:   record.SequenceID,
		Flags:        resultFlags,
		ServerOwner:  sm.serverIdentity.ServerOwner,
		ServerScope:  sm.serverIdentity.ServerScope,
		ServerImplId: []types.NfsImplId4{sm.serverIdentity.ImplID},
	}, nil
}

// createV41Client creates and stores a new V41ClientRecord.
// Caller must hold sm.mu.
func (sm *StateManager) createV41Client(
	ownerID []byte,
	verifier [8]byte,
	clientImplId []types.NfsImplId4,
	clientAddr string,
) *V41ClientRecord {
	now := time.Now()

	record := &V41ClientRecord{
		ClientID:    sm.generateClientID(),
		OwnerID:     make([]byte, len(ownerID)),
		Verifier:    verifier,
		SequenceID:  1,
		ClientAddr:  clientAddr,
		CreatedAt:   now,
		LastRenewal: now,
	}
	copy(record.OwnerID, ownerID)
	applyImplInfo(record, clientImplId)

	ownerKey := string(ownerID)
	sm.v41ClientsByID[record.ClientID] = record
	sm.v41ClientsByOwner[ownerKey] = record

	return record
}

// applyImplInfo extracts the first nfs_impl_id4 entry from clientImplId
// and applies it to the record. Shared between createV41Client and the
// idempotent branch of ExchangeID.
func applyImplInfo(record *V41ClientRecord, clientImplId []types.NfsImplId4) {
	if len(clientImplId) == 0 {
		return
	}
	impl := clientImplId[0]
	record.ImplDomain = impl.Domain
	record.ImplName = impl.Name
	if impl.Date.Seconds != 0 || impl.Date.Nseconds != 0 {
		record.ImplDate = time.Unix(impl.Date.Seconds, int64(impl.Date.Nseconds))
	}
}

// purgeV41Client removes a V41ClientRecord from both lookup maps
// and destroys all associated sessions.
// Only deletes from v41ClientsByOwner if the map entry still points to this
// record (guards against a concurrent createV41Client having already replaced it).
// Caller must hold sm.mu.
func (sm *StateManager) purgeV41Client(record *V41ClientRecord) {
	if record.Lease != nil {
		record.Lease.Stop()
	}

	// Clean up all delegations (file + directory) for this client
	for other, deleg := range sm.delegByOther {
		if deleg.ClientID != record.ClientID {
			continue
		}
		sm.cleanupDirDelegation(deleg)
		deleg.StopRecallTimer()
		delete(sm.delegByOther, other)
		sm.removeDelegFromFile(deleg)
	}

	// Destroy all sessions for this client
	for _, session := range sm.sessionsByClientID[record.ClientID] {
		delete(sm.sessionsByID, session.SessionID)
		sm.sessionMetrics.recordDestroyed("purged", time.Since(session.CreatedAt).Seconds())
	}
	delete(sm.sessionsByClientID, record.ClientID)

	delete(sm.v41ClientsByID, record.ClientID)
	ownerKey := string(record.OwnerID)
	if existing := sm.v41ClientsByOwner[ownerKey]; existing == record {
		delete(sm.v41ClientsByOwner, ownerKey)
	}
}

// ============================================================================
// Helper Methods (for REST API in Plan 02)
// ============================================================================

// ListV41Clients returns copies of all registered v4.1 client records.
// Thread-safe: acquires sm.mu.RLock.
func (sm *StateManager) ListV41Clients() []V41ClientRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	clients := make([]V41ClientRecord, 0, len(sm.v41ClientsByID))
	for _, record := range sm.v41ClientsByID {
		rec := *record
		if rec.OwnerID != nil {
			ownerCopy := make([]byte, len(rec.OwnerID))
			copy(ownerCopy, rec.OwnerID)
			rec.OwnerID = ownerCopy
		}
		rec.Lease = nil
		clients = append(clients, rec)
	}
	return clients
}

// ListV40Clients returns copies of all registered v4.0 confirmed client records.
// Thread-safe: acquires sm.mu.RLock.
func (sm *StateManager) ListV40Clients() []ClientRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	clients := make([]ClientRecord, 0, len(sm.clientsByName))
	for _, record := range sm.clientsByName {
		clients = append(clients, *record)
	}
	return clients
}

// EvictV41Client removes a v4.1 client record by client ID.
// Also destroys all associated sessions (handled by purgeV41Client).
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) EvictV41Client(clientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record, exists := sm.v41ClientsByID[clientID]
	if !exists {
		return fmt.Errorf("v4.1 client %d not found", clientID)
	}

	sm.purgeV41Client(record)
	logger.Info("EvictV41Client: v4.1 client evicted",
		"client_id", clientID,
		"client_addr", record.ClientAddr)
	return nil
}

// DestroyV41ClientID implements the NFSv4.1 DESTROY_CLIENTID operation per
// RFC 8881 Section 18.50.
//
// The operation destroys a client ID and all associated state (sessions,
// delegations, open state, lock state, backchannel). The destruction is
// synchronous: after returning NFS4_OK, the client ID is immediately invalid.
//
// Returns:
//   - NFS4ERR_STALE_CLIENTID if the client ID is not found
//   - NFS4ERR_CLIENTID_BUSY if the client still has active sessions
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) DestroyV41ClientID(clientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record, exists := sm.v41ClientsByID[clientID]
	if !exists {
		return &NFS4StateError{
			Status:  types.NFS4ERR_STALE_CLIENTID,
			Message: fmt.Sprintf("v4.1 client %d not found", clientID),
		}
	}

	// Strict RFC compliance: reject if sessions remain
	if sessions := sm.sessionsByClientID[clientID]; len(sessions) > 0 {
		return &NFS4StateError{
			Status:  types.NFS4ERR_CLIENTID_BUSY,
			Message: fmt.Sprintf("v4.1 client %d has %d active sessions", clientID, len(sessions)),
		}
	}

	// Synchronous purge of all v4.1 client state
	sm.purgeV41Client(record)

	// If grace period is active and this client was expected, notify grace
	// to prevent the grace period from hanging (Pitfall 6).
	if sm.gracePeriod != nil {
		// ClientReclaimed handles its own locking and checks if active.
		sm.gracePeriod.ClientReclaimed(clientID)
	}

	logger.Info("DESTROY_CLIENTID: client destroyed",
		"client_id", clientID,
		"client_addr", record.ClientAddr)

	return nil
}

// EvictV40Client removes a v4.0 client record by client ID and cleans up all
// associated state (open owners, open states, lock states, delegations).
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) EvictV40Client(clientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record, exists := sm.clientsByID[clientID]
	if !exists {
		return fmt.Errorf("v4.0 client %d not found", clientID)
	}

	// Stop lease timer
	if record.Lease != nil {
		record.Lease.Stop()
	}

	// Remove all open states and lock states for all owners
	for _, owner := range record.OpenOwners {
		for _, openState := range owner.OpenStates {
			for _, lockState := range openState.LockStates {
				delete(sm.lockStateByOther, lockState.Stateid.Other)

				// Remove actual locks from unified lock manager (matches onLeaseExpired)
				if sm.lockManager != nil && lockState.LockOwner != nil {
					ownerID := fmt.Sprintf("nfs4:%d:%s", lockState.LockOwner.ClientID,
						hex.EncodeToString(lockState.LockOwner.OwnerData))
					handleKey := string(lockState.FileHandle)
					for _, l := range sm.lockManager.ListEnhancedLocks(handleKey) {
						if l.Owner.OwnerID == ownerID {
							_ = sm.lockManager.RemoveEnhancedLock(handleKey, l.Owner, l.Offset, l.Length)
						}
					}
				}

				if lockState.LockOwner != nil {
					lockKey := makeLockOwnerKey(lockState.LockOwner.ClientID, lockState.LockOwner.OwnerData)
					delete(sm.lockOwners, lockKey)
				}
			}
			delete(sm.openStateByOther, openState.Stateid.Other)
		}
		ownerKey := makeOwnerKey(owner.ClientID, owner.OwnerData)
		delete(sm.openOwners, ownerKey)
	}

	// Clean up delegations for the evicted client
	for other, deleg := range sm.delegByOther {
		if deleg.ClientID != clientID {
			continue
		}
		deleg.StopRecallTimer()
		sm.cleanupDirDelegation(deleg)
		delete(sm.delegByOther, other)
		sm.removeDelegFromFile(deleg)
	}

	// Remove from all maps
	delete(sm.clientsByID, clientID)
	if record.Confirmed {
		if confirmed := sm.clientsByName[record.ClientIDString]; confirmed != nil && confirmed.ClientID == clientID {
			delete(sm.clientsByName, record.ClientIDString)
		}
	} else {
		if unconfirmed := sm.unconfirmedByName[record.ClientIDString]; unconfirmed != nil && unconfirmed.ClientID == clientID {
			delete(sm.unconfirmedByName, record.ClientIDString)
		}
	}

	logger.Info("EvictV40Client: v4.0 client evicted",
		"client_id", clientID,
		"client_id_str", record.ClientIDString,
		"client_addr", record.ClientAddr)
	return nil
}

// ServerInfo returns the immutable server identity for API responses.
func (sm *StateManager) ServerInfo() *ServerIdentity {
	return sm.serverIdentity
}

// ============================================================================
// Channel Negotiation (RFC 8881 Section 18.36)
// ============================================================================

// ChannelLimits defines server-imposed limits for channel attribute negotiation.
type ChannelLimits struct {
	MaxSlots              uint32
	MaxRequestSize        uint32
	MaxResponseSize       uint32
	MaxResponseSizeCached uint32
	MinRequestSize        uint32
	MinResponseSize       uint32
}

// DefaultForeLimits returns the default server limits for fore channel negotiation.
func DefaultForeLimits() ChannelLimits {
	return ChannelLimits{
		MaxSlots:              64,
		MaxRequestSize:        1048576, // 1MB
		MaxResponseSize:       1048576, // 1MB
		MaxResponseSizeCached: 65536,   // 64KB
		MinRequestSize:        8192,    // 8KB
		MinResponseSize:       8192,    // 8KB
	}
}

// DefaultBackLimits returns the default server limits for back channel negotiation.
func DefaultBackLimits() ChannelLimits {
	return ChannelLimits{
		MaxSlots:              8,
		MaxRequestSize:        65536, // 64KB
		MaxResponseSize:       65536, // 64KB
		MaxResponseSizeCached: 65536, // 64KB
		MinRequestSize:        8192,  // 8KB
		MinResponseSize:       8192,  // 8KB
	}
}

// negotiateChannelAttrs clamps the client-requested channel attributes
// to the server-imposed limits, per RFC 8881 Section 18.36.
func negotiateChannelAttrs(requested types.ChannelAttrs, limits ChannelLimits) types.ChannelAttrs {
	negotiated := types.ChannelAttrs{
		// HeaderPadSize is always 0 (no RDMA).
		HeaderPadSize: 0,
		// MaxOperations is 0 (unlimited per locked decision).
		MaxOperations: 0,
		// RdmaIrd is always nil (no RDMA per locked decision).
		RdmaIrd: nil,
	}

	// Clamp MaxRequests (slots) to [1, limits.MaxSlots]
	negotiated.MaxRequests = clampUint32(requested.MaxRequests, 1, limits.MaxSlots)

	// Clamp MaxRequestSize to [MinRequestSize, limits.MaxRequestSize]
	negotiated.MaxRequestSize = clampUint32(requested.MaxRequestSize, limits.MinRequestSize, limits.MaxRequestSize)

	// Clamp MaxResponseSize to [MinResponseSize, limits.MaxResponseSize]
	negotiated.MaxResponseSize = clampUint32(requested.MaxResponseSize, limits.MinResponseSize, limits.MaxResponseSize)

	// Clamp MaxResponseSizeCached to min(requested, limits.MaxResponseSizeCached)
	negotiated.MaxResponseSizeCached = requested.MaxResponseSizeCached
	if negotiated.MaxResponseSizeCached > limits.MaxResponseSizeCached {
		negotiated.MaxResponseSizeCached = limits.MaxResponseSizeCached
	}

	return negotiated
}

// clampUint32 clamps v to [min, max].
func clampUint32(v, min, max uint32) uint32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// HasAcceptableCallbackSecurity checks whether at least one of the callback
// security parameters uses an acceptable auth flavor (AUTH_NONE=0 or AUTH_SYS=1).
// Returns true if the slice is empty (no callback security = ok) or contains
// at least one acceptable flavor. Rejects RPCSEC_GSS-only (flavor 6).
func HasAcceptableCallbackSecurity(secParms []types.CallbackSecParms4) bool {
	if len(secParms) == 0 {
		return true
	}
	for _, sp := range secParms {
		if sp.CbSecFlavor == 0 || sp.CbSecFlavor == 1 {
			return true
		}
	}
	return false
}

// ============================================================================
// Session Error Sentinels
// ============================================================================

var (
	// ErrBadSession indicates the session ID is not recognized.
	ErrBadSession = &NFS4StateError{Status: types.NFS4ERR_BADSESSION, Message: "session not found"}

	// ErrDelay indicates the operation cannot proceed now; the client should retry.
	ErrDelay = &NFS4StateError{Status: types.NFS4ERR_DELAY, Message: "operation in progress, retry later"}

	// ErrTooManySessions indicates the per-client session limit has been reached.
	ErrTooManySessions = &NFS4StateError{Status: types.NFS4ERR_RESOURCE, Message: "per-client session limit exceeded"}

	// ErrSeqMisordered indicates a CREATE_SESSION sequence ID mismatch.
	ErrSeqMisordered = &NFS4StateError{Status: types.NFS4ERR_SEQ_MISORDERED, Message: "sequence ID misordered"}
)

// ============================================================================
// CreateSessionResult
// ============================================================================

// CreateSessionResult holds the output of StateManager.CreateSession for the
// handler to encode into the CREATE_SESSION response.
type CreateSessionResult struct {
	SessionID        types.SessionId4
	SequenceID       uint32
	Flags            uint32
	ForeChannelAttrs types.ChannelAttrs
	BackChannelAttrs types.ChannelAttrs
}
