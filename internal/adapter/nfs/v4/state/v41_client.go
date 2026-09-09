package state

import (
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// BuildDate is set via -ldflags "-X ...state.BuildDate=..." at compile time.
// Format: RFC 3339 (e.g. "2026-02-20T12:00:00Z"). Falls back to zero time if unset.
var BuildDate string

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

// Sentinels for the EXCHANGE_ID update cases. They carry their own wire status
// so the handler maps them without a per-error case.
var (
	// ErrNoConfirmedRecord indicates an update request found no confirmed
	// record for the owner ID.
	ErrNoConfirmedRecord = &NFS4StateError{
		Status:  types.NFS4ERR_NOENT,
		Message: "no confirmed client record for this owner ID",
	}

	// ErrVerifierNotSame indicates an update request carried a verifier other
	// than the confirmed record's.
	ErrVerifierNotSame = &NFS4StateError{
		Status:  types.NFS4ERR_NOT_SAME,
		Message: "update verifier does not match the confirmed record",
	}

	// ErrUpdateNotPermitted indicates an update request came from a principal
	// other than the one that established the confirmed record.
	ErrUpdateNotPermitted = &NFS4StateError{
		Status:  types.NFS4ERR_PERM,
		Message: "update by a principal other than the record's",
	}
)

// ExchangeID implements the NFSv4.1 EXCHANGE_ID multi-case algorithm per
// RFC 8881 Section 18.35.4, whose case numbers the branches below name.
//
// Without EXCHGID4_FLAG_UPD_CONFIRMED_REC_A in flags the request establishes or
// replaces a record:
//
//   - Case 1, new owner ID: create an unconfirmed record with a fresh client ID.
//   - Case 2, non-update on a confirmed record whose verifier and principal both
//     match: return the same client ID and leave the record alone.
//   - Case 3, client collision -- a confirmed record whose principal differs:
//     replace it if it holds no state under a live lease, otherwise refuse with
//     NFS4ERR_CLID_INUSE and change nothing.
//   - Case 4, replacement of an unconfirmed record: discard it whatever its
//     verifier and principal and create a fresh one, so the owner ID never has
//     two readings at once.
//   - Case 5, client restart -- a confirmed record, same principal, different
//     verifier: add an unconfirmed record with a fresh client ID and keep the
//     confirmed record and its state until CREATE_SESSION confirms the new one.
//
// With the flag set the request is an update of an existing confirmed record:
//
//   - Case 6: verifier and principal both match -- apply the update in place.
//   - Case 7: no confirmed record -- NFS4ERR_NOENT, any unconfirmed record left
//     intact.
//   - Case 8: verifier differs -- NFS4ERR_NOT_SAME.
//   - Case 9: principal differs -- NFS4ERR_PERM.
//
// The server always sets EXCHGID4_FLAG_USE_NON_PNFS in the result, and adds
// EXCHGID4_FLAG_CONFIRMED_R when the record it returns is confirmed.
//
// Caller must NOT hold sm.mu.
// The trailing principal is variadic so existing callers/tests that do not
// thread an auth principal keep compiling; production passes ctx.Principal().
func (sm *StateManager) ExchangeID(
	ownerID []byte,
	verifier [8]byte,
	flags uint32,
	clientImplId []types.NfsImplId4,
	clientAddr string,
	principal ...string,
) (*ExchangeIDResult, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	princ := firstOrEmpty(principal)
	ownerKey := string(ownerID)
	existing := sm.v41ClientsByOwner[ownerKey]

	// A record that superseded a still-live confirmed one is discarded here and
	// hands the owner ID back, so the rest of the algorithm sees a single record
	// per owner (case 5, "delete the unconfirmed record and process the
	// EXCHANGE_ID in its entirety").
	if existing != nil && existing.Superseded != nil {
		logger.Debug("EXCHANGE_ID: discarding a superseding record that was never confirmed",
			"client_id", existing.ClientID,
			"client_addr", clientAddr)
		sm.purgeV41Client(existing)
		existing = sm.v41ClientsByOwner[ownerKey]
	}

	if flags&types.EXCHGID4_FLAG_UPD_CONFIRMED_REC_A != 0 {
		return sm.exchangeIDUpdateLocked(existing, verifier, clientImplId, clientAddr, princ)
	}

	var record *ClientRecord

	switch {
	case existing == nil:
		// Case 1: new owner ID.
		record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr, princ)
		logger.Info("EXCHANGE_ID: new v4.1 client registered",
			"client_id", record.ClientID,
			"client_addr", clientAddr)

	case !existing.Confirmed:
		// Case 4: replacement of an unconfirmed record.
		sm.purgeV41Client(existing)
		record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr, princ)
		logger.Info("EXCHANGE_ID: new v4.1 client",
			"reason", "replaced unconfirmed",
			"old_client_id", existing.ClientID,
			"new_client_id", record.ClientID,
			"client_addr", clientAddr)

	case principalHijacks(existing.Principal, princ):
		// Case 3: client collision. A confirmed record still holding state under
		// a live lease belongs to a working client, so the caller is told to pick
		// another owner ID rather than having that client's state deleted under
		// it. A record holding nothing is taken over instead.
		if sm.v41ClientHasStateLocked(existing) {
			logger.Debug("EXCHANGE_ID: owner ID collision with a client that holds state",
				"client_id", existing.ClientID,
				"client_addr", clientAddr)
			return nil, ErrClientIDInUse
		}
		sm.purgeV41Client(existing)
		record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr, princ)
		logger.Info("EXCHANGE_ID: new v4.1 client",
			"reason", "owner ID collision, previous client held no state",
			"old_client_id", existing.ClientID,
			"new_client_id", record.ClientID,
			"client_addr", clientAddr)

	case existing.Verifier == verifier:
		// Case 2: non-update on a confirmed record. Only the properties the
		// server tracks move; the record itself is unchanged.
		existing.ClientAddr = clientAddr
		// Only adopt a non-empty incoming principal; never clear a stored
		// principal with an empty one (that would weaken the collision guard).
		if princ != "" {
			existing.Principal = princ
		}
		existing.LastRenewal = time.Now()
		applyImplInfo(existing, clientImplId)
		record = existing
		logger.Debug("EXCHANGE_ID: idempotent return for existing v4.1 client",
			"client_id", record.ClientID,
			"client_addr", clientAddr)

	default:
		// Case 5: client restart. The new incarnation gets its own unconfirmed
		// record; the previous one keeps its client ID, sessions and locks until
		// CREATE_SESSION confirms the replacement.
		record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr, princ)
		record.Superseded = existing
		logger.Info("EXCHANGE_ID: new v4.1 client",
			"reason", "client reboot detected",
			"old_client_id", existing.ClientID,
			"new_client_id", record.ClientID,
			"client_addr", clientAddr)
	}

	return sm.exchangeIDResultLocked(record), nil
}

// exchangeIDUpdateLocked handles an EXCHANGE_ID that carries
// EXCHGID4_FLAG_UPD_CONFIRMED_REC_A (RFC 8881 Section 18.35.4 cases 6-9).
// existing is the record the owner ID currently resolves to, or nil.
//
// Caller must hold sm.mu.
func (sm *StateManager) exchangeIDUpdateLocked(
	existing *ClientRecord,
	verifier [8]byte,
	clientImplId []types.NfsImplId4,
	clientAddr string,
	princ string,
) (*ExchangeIDResult, error) {
	// Case 7: there is no confirmed record to update. An unconfirmed record is
	// not updatable and is left intact for its own CREATE_SESSION.
	if existing == nil || !existing.Confirmed {
		return nil, ErrNoConfirmedRecord
	}

	// Case 8: the verifier belongs to another incarnation, which cannot update
	// this record. Reported ahead of the principal, matching the case-8 record
	// pattern that leaves the principal unconstrained.
	if existing.Verifier != verifier {
		return nil, ErrVerifierNotSame
	}

	// Case 9: only the principal that established the record may update it.
	if principalHijacks(existing.Principal, princ) {
		return nil, ErrUpdateNotPermitted
	}

	// Case 6: the update is allowed and the client record is left intact apart
	// from the properties it carries.
	existing.ClientAddr = clientAddr
	if princ != "" {
		existing.Principal = princ
	}
	existing.LastRenewal = time.Now()
	applyImplInfo(existing, clientImplId)

	logger.Debug("EXCHANGE_ID: updated confirmed v4.1 client",
		"client_id", existing.ClientID,
		"client_addr", clientAddr)

	return sm.exchangeIDResultLocked(existing), nil
}

// exchangeIDResultLocked builds the EXCHANGE_ID reply for a record.
//
// Caller must hold sm.mu.
func (sm *StateManager) exchangeIDResultLocked(record *ClientRecord) *ExchangeIDResult {
	// EXCHGID4_FLAG_UPD_CONFIRMED_REC_A is never reflected back; CONFIRMED_R
	// reports whether the record returned has been confirmed.
	resultFlags := uint32(types.EXCHGID4_FLAG_USE_NON_PNFS)
	if record.Confirmed {
		resultFlags |= types.EXCHGID4_FLAG_CONFIRMED_R
	}

	// Per RFC 8881 Section 18.35, eir_sequenceid must be the value the
	// server will accept in the next CREATE_SESSION. Since CreateSession
	// checks csa_sequenceid == slot+1, we return slot+1 here so the
	// client sends exactly that value.
	return &ExchangeIDResult{
		ClientID:     record.ClientID,
		SequenceID:   record.SequenceID + 1,
		Flags:        resultFlags,
		ServerOwner:  sm.serverIdentity.ServerOwner,
		ServerScope:  sm.serverIdentity.ServerScope,
		ServerImplId: []types.NfsImplId4{sm.serverIdentity.ImplID},
	}
}

// v41ClientHasStateLocked reports whether a client still holds state a colliding
// owner ID must not destroy: an active session, an open owner, a byte-range lock
// or a delegation. A client whose lease has already expired holds nothing that
// needs protecting (RFC 8881 Section 18.35.4 case 3).
//
// ponytail: linear scans of the open-owner, lock and delegation indexes, none of
// which is keyed by client ID; this runs once per colliding EXCHANGE_ID, so add
// a per-client index only if a profile says these scans matter.
//
// Caller must hold sm.mu.
func (sm *StateManager) v41ClientHasStateLocked(record *ClientRecord) bool {
	if record.Lease != nil && record.Lease.IsExpired() {
		return false
	}
	if len(sm.sessionsByClientID[record.ClientID]) > 0 {
		return true
	}
	for _, owner := range sm.openOwners {
		if owner.ClientID == record.ClientID {
			return true
		}
	}
	for _, lockState := range sm.lockStateByOther {
		if lockState.LockOwner != nil && lockState.LockOwner.ClientID == record.ClientID {
			return true
		}
	}
	for _, deleg := range sm.delegByOther {
		if deleg.ClientID == record.ClientID {
			return true
		}
	}
	return false
}

// collapseSupersededLocked destroys the confirmed record a newly confirmed one
// replaces, together with its sessions and locking state. Called when
// CREATE_SESSION confirms a record established by the client-restart case, which
// is where the owner ID's two records collapse into one (RFC 8881 Sections
// 18.35.4 case 5 and 18.36.3).
//
// The superseded record is looked up by client ID rather than trusted directly,
// so a reaper that already removed it is not purged twice.
//
// Caller must hold sm.mu.
func (sm *StateManager) collapseSupersededLocked(record *ClientRecord) {
	superseded := record.Superseded
	record.Superseded = nil
	if superseded == nil {
		return
	}
	if sm.v41ClientsByID[superseded.ClientID] != superseded {
		return
	}

	logger.Info("CREATE_SESSION: destroying the client incarnation this one replaces",
		"old_client_id", superseded.ClientID,
		"new_client_id", record.ClientID)
	sm.purgeV41Client(superseded)
}

// createV41Client creates and stores a new ClientRecord.
// Caller must hold sm.mu.
func (sm *StateManager) createV41Client(
	ownerID []byte,
	verifier [8]byte,
	clientImplId []types.NfsImplId4,
	clientAddr string,
	principal string,
) *ClientRecord {
	now := time.Now()

	record := &ClientRecord{
		ClientID:    sm.generateClientID(),
		OwnerID:     make([]byte, len(ownerID)),
		Verifier:    verifier,
		SequenceID:  0,
		ClientAddr:  clientAddr,
		Principal:   principal,
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
func applyImplInfo(record *ClientRecord, clientImplId []types.NfsImplId4) {
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

// purgeV41Client removes a ClientRecord from both lookup maps, releases the
// open and lock state its owners hold, and destroys all associated sessions.
// Only deletes from v41ClientsByOwner if the map entry still points to this
// record (guards against a concurrent createV41Client having already replaced it);
// where the record superseded a still-live confirmed one, the owner ID goes back
// to that record instead of being dropped.
// Caller must hold sm.mu.
func (sm *StateManager) purgeV41Client(record *ClientRecord) {
	// Drop the durable recovery record: a purged client (eviction, DESTROY_CLIENTID,
	// or reboot-replace) cannot reclaim under this identity. Best-effort; no-op when
	// the client was never confirmed (no record was ever persisted) or no store wired.
	if record.Confirmed {
		sm.deleteClientRecoveryLocked(v41RecoveryKey(record.OwnerID))
	}

	if record.Lease != nil {
		record.Lease.Stop()
	}

	sm.removeClientOpenStateLocked(record.ClientID)
	sm.removeClientLockStateLocked(record.ClientID)

	// Clean up all delegations (file + directory) for this client
	for other, deleg := range sm.delegByOther {
		if deleg.ClientID != record.ClientID {
			continue
		}
		sm.cleanupDirDelegation(deleg)
		deleg.StopRecallTimer()
		sm.deleteDelegByOtherLocked(other)
		sm.removeDelegFromFile(deleg)
	}

	// Destroy all sessions for this client. Stop each session's backchannel
	// sender BEFORE removing it from sessionsByID -- stopBackchannelSender
	// looks the session up there, so deleting first would leak the goroutine.
	for _, session := range sm.sessionsByClientID[record.ClientID] {
		sm.stopBackchannelSender(session.SessionID)
		delete(sm.sessionsByID, session.SessionID)
	}
	delete(sm.sessionsByClientID, record.ClientID)

	delete(sm.v41ClientsByID, record.ClientID)
	ownerKey := string(record.OwnerID)
	if existing := sm.v41ClientsByOwner[ownerKey]; existing == record {
		// A record that superseded a still-live confirmed one hands the owner ID
		// back to it, so the confirmed client remains reachable by owner ID once
		// its unconfirmed replacement is gone.
		if superseded := record.Superseded; superseded != nil &&
			sm.v41ClientsByID[superseded.ClientID] == superseded {
			sm.v41ClientsByOwner[ownerKey] = superseded
		} else {
			delete(sm.v41ClientsByOwner, ownerKey)
		}
	}
}

// ============================================================================
// Helper Methods (for REST API in)
// ============================================================================

// ListV41Clients returns pointers to all registered v4.1 client records.
// Thread-safe: acquires sm.mu.RLock.
func (sm *StateManager) ListV41Clients() []*ClientRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	clients := make([]*ClientRecord, 0, len(sm.v41ClientsByID))
	for _, record := range sm.v41ClientsByID {
		clients = append(clients, record)
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

	// Drop the durable recovery record: an evicted client cannot reclaim.
	sm.deleteClientRecoveryLocked(record.ClientIDString)

	// Stop lease timer
	if record.Lease != nil {
		record.Lease.Stop()
	}

	sm.removeClientOpenStateLocked(clientID)

	// Clean up delegations for the evicted client
	for other, deleg := range sm.delegByOther {
		if deleg.ClientID != clientID {
			continue
		}
		deleg.StopRecallTimer()
		sm.cleanupDirDelegation(deleg)
		sm.deleteDelegByOtherLocked(other)
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

// Channel size floors. A requested budget below these cannot carry a COMPOUND
// at all: the XDR framing of even a status-only COMPOUND request or reply
// exceeds it, so CREATE_SESSION answers NFS4ERR_TOOSMALL instead of
// negotiating a channel that can never carry traffic (RFC 8881 Section
// 18.36.3: if a replier on a channel could never send a response, the server
// SHOULD return NFS4ERR_TOOSMALL). Both floors sit under the small-but-workable
// budgets the conformance suite accepts (request 400, response 400, whose
// reply-size answers fire at COMPOUND time) and over the budgets it forces
// TOOSMALL answers on (request 20 and 10, response 0), so the floors must stay
// inside (20, 400] on the request side and (0, 400] on the response side.
const (
	minChannelRequestSize  uint32 = 256
	minChannelResponseSize uint32 = 256
)

// channelAttrsTooSmall returns an NFS4ERR_TOOSMALL error when a requested
// channel could never carry a COMPOUND request or reply. MaxResponseSizeCached
// is deliberately not floored here: the answer to an unusable cache budget is
// NFS4ERR_REP_TOO_BIG_TO_CACHE on the operation that overflows it, not a
// CREATE_SESSION rejection.
func channelAttrsTooSmall(requested types.ChannelAttrs) *NFS4StateError {
	switch {
	case requested.MaxRequestSize < minChannelRequestSize:
		return &NFS4StateError{
			Status:  types.NFS4ERR_TOOSMALL,
			Message: fmt.Sprintf("ca_maxrequestsize %d below the %d-byte floor; no COMPOUND request could fit", requested.MaxRequestSize, minChannelRequestSize),
		}
	case requested.MaxResponseSize < minChannelResponseSize:
		return &NFS4StateError{
			Status:  types.NFS4ERR_TOOSMALL,
			Message: fmt.Sprintf("ca_maxresponsesize %d below the %d-byte floor; no COMPOUND reply could fit", requested.MaxResponseSize, minChannelResponseSize),
		}
	default:
		return nil
	}
}

// DefaultBackLimits returns the default server limits for back channel negotiation.
// MaxSlots must be at least 16 because the Linux kernel NFS client requests 16
// back channel slots and rejects CREATE_SESSION with EINVAL if the server
// returns fewer than requested.
func DefaultBackLimits() ChannelLimits {
	return ChannelLimits{
		MaxSlots:              32,
		MaxRequestSize:        65536, // 64KB
		MaxResponseSize:       65536, // 64KB
		MaxResponseSizeCached: 65536, // 64KB
		MinRequestSize:        8192,  // 8KB
		MinResponseSize:       8192,  // 8KB
	}
}

// negotiateChannelAttrs negotiates channel attributes per RFC 8881 Section 18.36.
// The server MUST NOT return values larger than the client requested (the client
// knows its own resource limits). The server MAY reduce values down to its own
// minimums.
func negotiateChannelAttrs(requested types.ChannelAttrs, limits ChannelLimits) types.ChannelAttrs {
	negotiated := types.ChannelAttrs{
		// HeaderPadSize is always 0 (no RDMA).
		HeaderPadSize: 0,
		// MaxOperations: min(client request, server limit).
		// Server MUST NOT exceed client's requested value (RFC 8881).
		MaxOperations: min(requested.MaxOperations, types.MaxCompoundOps),
		// RdmaIrd is always nil (no RDMA per locked decision).
		RdmaIrd: nil,
	}

	// MaxRequests (slots): at least 1, at most the server's limit,
	// but never more than what the client asked for.
	negotiated.MaxRequests = clampUint32(requested.MaxRequests, 1, limits.MaxSlots)

	// Size fields: min(requested, server_max). Never exceed the client's request.
	negotiated.MaxRequestSize = min(requested.MaxRequestSize, limits.MaxRequestSize)
	negotiated.MaxResponseSize = min(requested.MaxResponseSize, limits.MaxResponseSize)
	negotiated.MaxResponseSizeCached = min(requested.MaxResponseSizeCached, limits.MaxResponseSizeCached)

	return negotiated
}

// clampUint32 clamps v to [lo, hi].
func clampUint32(v, lo, hi uint32) uint32 {
	return min(max(v, lo), hi)
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
	// NFS4ERR_RESOURCE is absent from CREATE_SESSION's valid-error list in
	// RFC 8881 Section 18.36; the resource-exhaustion answer there is
	// NFS4ERR_NOSPC, which is what this sentinel must carry.
	ErrTooManySessions = &NFS4StateError{Status: types.NFS4ERR_NOSPC, Message: "per-client session limit exceeded"}

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

	// EncodedRes holds the full XDR-encoded success response. It is produced and
	// cached for replay under sm.mu in the same critical section that bumps the
	// client sequence ID, so the handler can return it directly without a second
	// encode/cache round-trip (which would reopen the replay window).
	EncodedRes []byte
}
