package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// DefaultLeaseDuration is the default NFSv4 lease duration (90 seconds),
// matching Linux nfsd.
const DefaultLeaseDuration = 90 * time.Second

// StateManager is the central coordinator for all NFSv4 state.
// It owns client records, open-owner tables, stateid maps, lease timers,
// and grace period state.
// All state modifications go through StateManager methods to ensure
// thread safety and consistency.
//
// Per the research anti-pattern advice, a single RWMutex protects all state
// to avoid deadlocks between interdependent lookups (client -> open-owner ->
// stateid -> lease).
type StateManager struct {
	mu sync.RWMutex

	// clientsByID maps server-assigned client IDs to client records of both
	// minor versions: v4.0 records (established by SETCLIENTID, MinorVersion 0)
	// and v4.1 records (established by EXCHANGE_ID, MinorVersion 1). The two
	// never hold the same ID because generateClientID draws both flows from one
	// sequence, and ClientRecord.MinorVersion says which flow minted each
	// entry. Version-sensitive operations read it through the v40Client /
	// v41Client helpers below; version-agnostic readers (e.g. the shared
	// recovery-key switch, delegation handback) may index the map directly.
	clientsByID map[uint64]*ClientRecord

	// clientsByName maps nfs_client_id4.id strings to confirmed client records.
	clientsByName map[string]*ClientRecord

	// unconfirmedByName maps nfs_client_id4.id strings to unconfirmed
	// client records (pending SETCLIENTID_CONFIRM).
	unconfirmedByName map[string]*ClientRecord

	// openStateByOther maps stateid "other" fields to OpenState records.
	// This is the primary lookup table for stateid validation.
	openStateByOther map[[types.NFS4_OTHER_SIZE]byte]*OpenState

	// openStateByFile indexes live OpenState records by file handle
	// (string(fileHandle)). It is a secondary index kept consistent with
	// openStateByOther at every insert/remove so share-conflict checks
	// (shareConflictLocked) and delegation grant checks (countOpensOnFile)
	// iterate only the opens on the target file instead of scanning every
	// open in the server. Maintained via addOpenStateToFileLocked /
	// removeOpenStateFromFileLocked.
	openStateByFile map[string][]*OpenState

	// openOwners maps open-owner keys to OpenOwner records.
	// Key is composite of clientID + hex(ownerData).
	openOwners map[openOwnerKey]*OpenOwner

	// closedOwnerByOther maps the "other" of a now-closed open stateid to the
	// retained open-owner. CLOSE deletes the OpenState, so a retransmitted
	// CLOSE can no longer resolve the owner via openStateByOther; this map lets
	// the CLOSE replay path find the owner (and its cached reply) to satisfy
	// the NFSv4.0 exactly-once contract (RFC 7530 §9.1.7). Entries are cleared
	// when the owner is reaped (lease expiry) or the stateid is reused.
	closedOwnerByOther map[[types.NFS4_OTHER_SIZE]byte]*OpenOwner

	// expiredStateids holds the "other" of every stateid whose state was freed
	// because the owning client's lease was cancelled. Without it the freed
	// stateid is simply absent from the tables and looks like one the server
	// never issued, so the client is told NFS4ERR_BAD_STATEID when RFC 7530
	// §9.6.3.2 requires NFS4ERR_EXPIRED ("When a lease is canceled, all locking
	// state associated with it is freed, and the use of any of the associated
	// stateids will result in NFS4ERR_EXPIRED being returned").
	//
	// ponytail: capped and dropped wholesale on overflow rather than aged out
	// per entry; losing an entry only degrades the answer to the
	// NFS4ERR_BAD_STATEID the server gave before, and both statuses send the
	// client into the same recovery. Give entries timestamps and sweep them if
	// a deployment is ever seen to overflow the cap with clients still retrying.
	expiredStateids map[[types.NFS4_OTHER_SIZE]byte]struct{}

	// lockOwners maps lock-owner keys to LockOwner records.
	// Key is composite of clientID + hex(ownerData), same pattern as openOwners.
	lockOwners map[lockOwnerKey]*LockOwner

	// lockStateByOther maps lock stateid "other" fields to LockState records.
	// This is the primary lookup table for lock stateid validation.
	lockStateByOther map[[types.NFS4_OTHER_SIZE]byte]*LockState

	// delegByOther maps delegation stateid "other" fields to DelegationState records.
	// This is the primary lookup table for delegation stateid validation.
	delegByOther map[[types.NFS4_OTHER_SIZE]byte]*DelegationState

	// delegByFile maps file handle (string key) to a list of DelegationState records.
	// Used for conflict detection: "does any client hold a delegation for this file?"
	delegByFile map[string][]*DelegationState

	// revokedDelegCount counts, per client ID, the number of delegations that
	// have been marked Revoked but are still retained in delegByOther (for
	// stale-stateid detection). It is a secondary index so GetStatusFlags can
	// answer "does this client have any revoked delegation?" in O(1) on the
	// SEQUENCE hot path instead of scanning every delegation in the server.
	// Maintained via markDelegRevokedLocked and deleteDelegByOtherLocked.
	revokedDelegCount map[uint64]int

	// delegStateidMap maps LockManager DelegationID (UUID string) to NFS Stateid4.
	// This enables NFSBreakHandler to look up the NFS wire-format stateid when
	// a delegation recall is dispatched by the shared LockManager.
	delegStateidMap map[string]types.Stateid4

	// recentlyRecalled tracks file handles that were recently involved in
	// delegation recalls. Prevents grant-recall-grant-recall storms.
	// Key: string(fileHandle), Value: time of recall.
	recentlyRecalled map[string]time.Time

	// recentlyRecalledTTL is the duration for which a file is considered
	// recently recalled. Defaults to RecentlyRecalledTTL (30s).
	recentlyRecalledTTL time.Duration

	// delegationsEnabled controls whether delegations can be granted.
	// When false, ShouldGrantDelegation always returns OPEN_DELEGATE_NONE.
	// Defaults to true; updated from live adapter settings.
	delegationsEnabled bool

	// maxDelegations is the maximum total outstanding delegations (file + directory).
	// 0 means unlimited. Updated via SetMaxDelegations.
	maxDelegations int

	// dirDelegBatchWindow is the duration of the notification batching window
	// for directory delegations. Defaults to 50ms.
	dirDelegBatchWindow time.Duration

	// lockManager is a static unified lock manager used as a fallback when no
	// resolver is set (primarily by tests). See SetLockManagerResolver.
	lockManager lock.LockManager

	// lockManagerResolver resolves the per-share unified lock manager for a file
	// handle, taking precedence over lockManager. See SetLockManagerResolver.
	lockManagerResolver func(handle []byte) lock.LockManager

	// bootEpoch identifies this incarnation of the server. It is the high 32
	// bits of every client ID and a 24-bit fragment of every stateid, and both
	// readers only ever test it for equality, never for order.
	bootEpoch uint32

	// nextClientSeq is an atomic counter for the low 32 bits of client IDs.
	nextClientSeq uint32

	// leaseDuration is the configured lease duration for all clients.
	leaseDuration time.Duration

	// gracePeriod tracks the NFSv4 grace period state for server restart recovery.
	// Created lazily when StartGracePeriod is called.
	gracePeriod *GracePeriodState

	// graceDuration is the duration of the grace period.
	// Defaults to leaseDuration if not explicitly set.
	graceDuration time.Duration

	// recoveryStore provides server-global durable persistence of confirmed
	// client identities for reboot/grace recovery. Optional: when
	// nil the StateManager behaves exactly as before (no durable recovery),
	// which keeps bare test constructions working. Set via
	// SetClientRecoveryStore, wired by the NFS adapter to the first share's
	// metadata store implementing lock.ClientRecoveryStore (mirroring the NSM
	// ClientRegistrationStore designation).
	recoveryStore lock.ClientRecoveryStore

	// serverEpoch is the epoch under which this server instance is running.
	// Stamped into every client-recovery record so stale records from prior
	// instances can be GC'd. Set alongside recoveryStore.
	serverEpoch uint64

	// bootRecoveryVerifiers snapshots, at boot-load time, the BootVerifier of
	// every prior-instance recovery record keyed by ClientIDString. CLAIM_PREVIOUS
	// is validated against THIS snapshot (RFC 7530 §9.1.4): a reclaiming client
	// whose verifier differs from its pre-restart durable verifier rebooted and
	// must not reclaim. Validating against the snapshot (not the live store) is
	// essential — a rebooting client's SETCLIENTID_CONFIRM overwrites its live
	// record with the new verifier before it reaches CLAIM_PREVIOUS. Populated
	// only by LoadClientRecovery; nil/empty otherwise (reclaim ungated).
	bootRecoveryVerifiers map[string][8]byte

	// pendingReclaimPersists tracks the at-most-one live retry chain per
	// recovery key (see pendingReclaimPersist). A re-schedule while an entry
	// exists adopts it instead of forking a second chain, so a down backend
	// cannot pile up chains and a new issuer's persist failure is not skipped
	// while an old chain lives. Entries are dropped on success or when the
	// issuer no longer holds the key.
	pendingReclaimPersists map[string]*pendingReclaimPersist

	// ============================================================================
	// Version-sensitive lookup helpers
	// ============================================================================
	// (methods on StateManager; see v40ClientLocked / v41ClientLocked below)

	// v41ClientsByOwner maps v4.1 owner ID bytes (string key) to client
	// records. EXCHANGE_ID resolves by owner, which no v4.0 flow supplies,
	// so every entry is a MinorVersion-1 record by construction.
	v41ClientsByOwner map[string]*ClientRecord

	// sessionsByID maps session IDs to session objects.
	sessionsByID map[types.SessionId4]*Session

	// sessionsByClientID maps client IDs to their sessions.
	sessionsByClientID map[uint64][]*Session

	// maxSessionsPerClient is the per-client session limit (default 16).
	maxSessionsPerClient int

	// foreMaxSlots is the server-imposed maximum fore channel slots per session.
	// Defaults to 64; updated via SetMaxSessionSlots from adapter settings.
	foreMaxSlots uint32

	// serverIdentity is the immutable server identity returned in EXCHANGE_ID responses.
	serverIdentity *ServerIdentity

	// ============================================================================
	// Connection Binding State
	// ============================================================================

	// connMu protects connection binding maps. Separate from sm.mu to avoid
	// contention between session operations (sm.mu) and connection binding
	// operations (connMu). Lock ordering: sm.mu before connMu (never reverse).
	connMu sync.RWMutex

	// connByID maps connectionID -> binding.
	connByID map[uint64]*BoundConnection

	// connBySession maps sessionID -> list of bindings.
	connBySession map[types.SessionId4][]*BoundConnection

	// maxConnsPerSession is the maximum number of connections per session (default 16).
	maxConnsPerSession int

	// ============================================================================
	// Backchannel State
	// ============================================================================

	// connWriters maps connectionID -> ConnWriter callback for writing backchannel
	// data to a TCP connection. Registered when a connection is bound for back-channel.
	// Protected by connMu.
	connWriters map[uint64]ConnWriter

	// cbRepliesByConn maps connectionID -> PendingCBReplies for routing backchannel
	// REPLY messages to the sender goroutine. Protected by connMu.
	cbRepliesByConn map[uint64]*PendingCBReplies

	// backchannelFaults tracks per-client backchannel fault state. Set when a
	// callback send fails, cleared on success. Protected by connMu.
	backchannelFaults map[uint64]bool

	// cbNullFunc verifies the callback path during SETCLIENTID_CONFIRM. It
	// defaults to SendCBNull and is only overridden by tests (set once at
	// construction, before any goroutine reads it).
	cbNullFunc func(context.Context, CallbackInfo) error
}

// NewStateManager creates a new StateManager with the given lease duration.
// The boot epoch is drawn at random.
// An optional graceDuration parameter controls the grace period length;
// if omitted or zero, the lease duration is used.
func NewStateManager(leaseDuration time.Duration, graceDuration ...time.Duration) *StateManager {
	if leaseDuration <= 0 {
		leaseDuration = DefaultLeaseDuration
	}

	gd := leaseDuration
	if len(graceDuration) > 0 && graceDuration[0] > 0 {
		gd = graceDuration[0]
	}

	// The boot epoch has to differ from the last incarnation's, because
	// generateClientID restarts its sequence at 0 every boot: two runs sharing
	// an epoch hand out byte-identical client IDs, and a stale one is then
	// admitted as a live client rather than refused. A clock read at seconds
	// resolution guaranteed that collision for any restart inside one second,
	// which is well within a supervisor's restart time.
	//
	// ponytail: random and not persisted, so nothing rules out drawing the same
	// epoch twice -- the 24-bit stateid fragment puts that at roughly one
	// restart in 16 million before a stale stateid could read as current.
	// Persisting the epoch and reloading it incremented removes the chance
	// outright; the durable client-recovery store is already handed this value,
	// so the seam to persist it through exists.
	// From Go 1.24 the default crypto/rand Reader calls fatal() rather than
	// returning an error, so a partial fill that would leave this epoch zeroed
	// is not reachable and the error is dead, exactly as at the other draw in
	// generateStateidOther. If the module ever drops below Go 1.24 both must
	// become real error checks.
	var epochBytes [4]byte
	_, _ = rand.Read(epochBytes[:])
	epoch := binary.BigEndian.Uint32(epochBytes[:])

	return &StateManager{
		clientsByID:         make(map[uint64]*ClientRecord),
		clientsByName:       make(map[string]*ClientRecord),
		openStateByOther:    make(map[[types.NFS4_OTHER_SIZE]byte]*OpenState),
		openStateByFile:     make(map[string][]*OpenState),
		openOwners:          make(map[openOwnerKey]*OpenOwner),
		closedOwnerByOther:  make(map[[types.NFS4_OTHER_SIZE]byte]*OpenOwner),
		expiredStateids:     make(map[[types.NFS4_OTHER_SIZE]byte]struct{}),
		lockOwners:          make(map[lockOwnerKey]*LockOwner),
		lockStateByOther:    make(map[[types.NFS4_OTHER_SIZE]byte]*LockState),
		delegByOther:        make(map[[types.NFS4_OTHER_SIZE]byte]*DelegationState),
		delegByFile:         make(map[string][]*DelegationState),
		revokedDelegCount:   make(map[uint64]int),
		delegStateidMap:     make(map[string]types.Stateid4),
		recentlyRecalled:    make(map[string]time.Time),
		recentlyRecalledTTL: RecentlyRecalledTTL,
		delegationsEnabled:  true,
		bootEpoch:           epoch,
		leaseDuration:       leaseDuration,
		graceDuration:       gd,
		// v4.0 SETCLIENTID confirmation state
		unconfirmedByName: make(map[string]*ClientRecord),
		// v4.1 state
		v41ClientsByOwner:      make(map[string]*ClientRecord),
		sessionsByID:           make(map[types.SessionId4]*Session),
		sessionsByClientID:     make(map[uint64][]*Session),
		maxSessionsPerClient:   16,
		foreMaxSlots:           64,
		serverIdentity:         newServerIdentity(epoch),
		pendingReclaimPersists: make(map[string]*pendingReclaimPersist),
		// Connection binding state
		connByID:           make(map[uint64]*BoundConnection),
		connBySession:      make(map[types.SessionId4][]*BoundConnection),
		maxConnsPerSession: 16,
		// Backchannel state
		connWriters:       make(map[uint64]ConnWriter),
		cbRepliesByConn:   make(map[uint64]*PendingCBReplies),
		backchannelFaults: make(map[uint64]bool),
		cbNullFunc:        SendCBNull,
	}
}

// LeaseDuration returns the configured lease duration.
func (sm *StateManager) LeaseDuration() time.Duration {
	return sm.leaseDuration
}

// BootEpoch returns the server boot epoch used for client ID generation.
func (sm *StateManager) BootEpoch() uint32 {
	return sm.bootEpoch
}

// generateClientID creates a unique 64-bit client ID by combining
// the boot epoch (high 32 bits) with a monotonic counter (low 32 bits).
// This ensures client IDs are unique across server restarts.
func (sm *StateManager) generateClientID() uint64 {
	seq := atomic.AddUint32(&sm.nextClientSeq, 1)
	return (uint64(sm.bootEpoch) << 32) | uint64(seq)
}

// generateConfirmVerifier creates an unpredictable 8-byte confirm verifier
// using crypto/rand. This prevents malicious or stale clients from guessing
// the verifier and confirming someone else's SETCLIENTID.
//
// Timestamps are not an acceptable source here: they are predictable, which is
// the whole property the verifier must not have.
func (sm *StateManager) generateConfirmVerifier() [8]byte {
	var v [8]byte
	_, _ = rand.Read(v[:])
	return v
}

// SetClientID implements the five-case SETCLIENTID algorithm per RFC 7530 Section 9.1.1.
//
// The algorithm determines the action based on whether the server has
// a confirmed and/or unconfirmed record for the client's id string:
//
//   - Case 1: No confirmed, no unconfirmed -- create new unconfirmed record
//   - Case 2: Confirmed exists + unconfirmed exists -- replace unconfirmed
//   - Case 3: Confirmed exists, different verifier -- client reboot, create new unconfirmed
//   - Case 4: No confirmed, unconfirmed exists -- replace unconfirmed
//   - Case 5: Confirmed exists, same verifier -- re-SETCLIENTID (callback update)
//
// Returns the client ID and confirm verifier on success, or an error.
// The trailing principal is variadic so existing callers/tests that do not
// thread an auth principal keep compiling; production passes ctx.Principal().
func (sm *StateManager) SetClientID(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr string, principal ...string) (*SetClientIDResult, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	princ := firstOrEmpty(principal)
	confirmed := sm.clientsByName[clientIDStr]
	unconfirmed := sm.unconfirmedByName[clientIDStr]

	switch {
	case confirmed == nil && unconfirmed == nil:
		// Case 1: Completely new client
		return sm.createNewClient(clientIDStr, verifier, callback, clientAddr, princ)

	case confirmed != nil && confirmed.VerifierMatches(verifier):
		// Case 5: Same client, same verifier (re-SETCLIENTID, maybe callback update)
		// This handles the case where the client sends SETCLIENTID again with
		// the same verifier. We update the callback and return a new unconfirmed
		// record that, when confirmed, will replace the existing confirmed record.
		return sm.reuseConfirmedClient(confirmed, clientIDStr, verifier, callback, clientAddr, princ)

	case confirmed != nil && !confirmed.VerifierMatches(verifier):
		// Case 3: Same client ID string, different verifier (client reboot)
		// The client has restarted. Create a new unconfirmed record.
		// The old confirmed record is NOT removed yet -- it gets replaced
		// when the new record is confirmed.
		return sm.handleClientReboot(clientIDStr, verifier, callback, clientAddr, princ)

	case confirmed == nil && unconfirmed != nil:
		// Case 4: No confirmed record, unconfirmed exists -- replace unconfirmed
		return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback, clientAddr, princ)

	default:
		// Case 2: Confirmed exists AND unconfirmed exists -- replace unconfirmed
		// This is a retransmit or new SETCLIENTID while another is pending.
		return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback, clientAddr, princ)
	}
}

// firstOrEmpty returns the first element of ss, or "" if ss is empty. Helper
// for the variadic principal parameter on SetClientID / ExchangeID.
func firstOrEmpty(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

// clientHasLiveStateLocked reports whether clientID still holds leased state
// that another principal's SETCLIENTID would cancel -- an open, a byte-range
// lock or a delegation -- under a lease that has not lapsed.
//
// Locks are counted separately from opens rather than through them: a lock
// hangs off an open state, but LOCK takes the lock-owner's client ID from the
// wire and does not require it to match the client that owns that open, so a
// client can hold live lock state without owning an open here.
//
// This is what makes the principal check in RFC 7530 Section 16.33.5
// conditional. Section 9.1.2 spells the condition out: when a SETCLIENTID
// arrives "for a client ID that currently has no state, or it has state but
// the lease has expired, rather than returning NFS4ERR_CLID_INUSE, the server
// MUST allow the SETCLIENTID". The security rule the check exists for is the
// MUST NOT in Section 9.1.1 against cancelling leased state established by a
// different principal, and a record holding none has nothing to cancel.
//
// Refusing unconditionally makes a client id string permanently unusable by
// every other principal once one has touched it. Clients derive that string
// from the hostname rather than from their credential, so a second user on the
// same host would never get a client id at all.
//
// Caller must hold sm.mu.
func (sm *StateManager) clientHasLiveStateLocked(clientID uint64) bool {
	if sm.clientLeaseLapsedLocked(clientID) {
		return false
	}
	for _, owner := range sm.openOwners {
		if owner.ClientID == clientID && len(owner.OpenStates) > 0 {
			return true
		}
	}
	for _, lockState := range sm.lockStateByOther {
		if lockState.LockOwner != nil && lockState.LockOwner.ClientID == clientID {
			return true
		}
	}
	for _, deleg := range sm.delegByOther {
		if deleg.ClientID == clientID {
			return true
		}
	}
	return false
}

// unknownClientIDError answers a client ID that no record matches. There are
// two ways that happens and they call for different errors. RFC 7530 Section
// 9.6.3.2 covers the first: once a lease is cancelled, "the use of the
// associated clientid will result in NFS4ERR_EXPIRED being returned", telling
// the client its state is gone and a new client id is what it needs. An id no
// boot of this server could have issued means something else entirely -- the
// server restarted -- and stays NFS4ERR_STALE_CLIENTID. generateClientID puts
// the boot epoch in the high 32 bits, which is what keeps the two apart.
//
// Answering STALE_CLIENTID for both makes a client that merely fell behind on
// RENEW conclude the server rebooted.
func (sm *StateManager) unknownClientIDError(clientID uint64) error {
	if uint32(clientID>>32) == sm.bootEpoch {
		return ErrExpired
	}
	return ErrStaleClientID
}

// createNewClient handles Case 1: completely new client.
// Creates a new unconfirmed record with a fresh client ID and confirm verifier.
// Caller must hold sm.mu.
func (sm *StateManager) createNewClient(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	clientID := sm.generateClientID()
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        clientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	// Store as unconfirmed
	sm.unconfirmedByName[clientIDStr] = record
	sm.clientsByID[clientID] = record

	logger.Info("SETCLIENTID: new client registered (unconfirmed)",
		"client_id_str", clientIDStr,
		"client_id", clientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        clientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// reuseConfirmedClient handles Case 5: re-SETCLIENTID with same verifier.
// The confirmed record exists with a matching verifier. Create a new
// unconfirmed record that will replace the confirmed one when confirmed.
// Caller must hold sm.mu.
func (sm *StateManager) reuseConfirmedClient(confirmed *ClientRecord, clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	// Reject a re-SETCLIENTID by anyone but the principal that established the
	// confirmed record: it would hijack the client's lease and state. See
	// principalHijacks and clientHasLiveStateLocked.
	if principalHijacks(confirmed.Principal, principal) && sm.clientHasLiveStateLocked(confirmed.ClientID) {
		return nil, ErrClientIDInUse
	}

	// Remove any existing unconfirmed record for this name
	if old := sm.unconfirmedByName[clientIDStr]; old != nil {
		// Only delete from clientsByID if it's a different ID than the confirmed client.
		// The unconfirmed record reuses confirmed.ClientID, so we must not delete
		// the confirmed client's entry from clientsByID.
		if old.ClientID != confirmed.ClientID {
			delete(sm.clientsByID, old.ClientID)
		}
		delete(sm.unconfirmedByName, clientIDStr)
	}

	// Create new unconfirmed record with the SAME client ID
	// (re-SETCLIENTID reuses the confirmed client ID)
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        confirmed.ClientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	sm.unconfirmedByName[clientIDStr] = record
	// Note: don't overwrite clientsByID[confirmed.ClientID] here --
	// the confirmed record still holds it until SETCLIENTID_CONFIRM.

	logger.Debug("SETCLIENTID: re-SETCLIENTID for confirmed client",
		"client_id_str", clientIDStr,
		"client_id", confirmed.ClientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        confirmed.ClientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// handleClientReboot handles Case 3: client reboot (different verifier).
// Creates a new unconfirmed record. The old confirmed record stays until
// the new one is confirmed in SETCLIENTID_CONFIRM.
// Caller must hold sm.mu.
func (sm *StateManager) handleClientReboot(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	// A reboot (different verifier) claimed by anyone but the principal that
	// established the confirmed record is a hijack attempt, not a real reboot.
	// See principalHijacks and clientHasLiveStateLocked.
	if confirmed := sm.clientsByName[clientIDStr]; confirmed != nil {
		if principalHijacks(confirmed.Principal, principal) && sm.clientHasLiveStateLocked(confirmed.ClientID) {
			return nil, ErrClientIDInUse
		}
	}

	// Remove any existing unconfirmed record for this name
	if old := sm.unconfirmedByName[clientIDStr]; old != nil {
		delete(sm.clientsByID, old.ClientID)
		delete(sm.unconfirmedByName, clientIDStr)
	}

	// Create a brand new client ID (reboot = new identity)
	clientID := sm.generateClientID()
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        clientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	sm.unconfirmedByName[clientIDStr] = record
	sm.clientsByID[clientID] = record

	logger.Info("SETCLIENTID: client reboot detected, new unconfirmed record",
		"client_id_str", clientIDStr,
		"new_client_id", clientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        clientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// replaceUnconfirmed handles Case 2 and Case 4: replace existing unconfirmed.
// Removes the old unconfirmed record and creates a new one.
// Caller must hold sm.mu.
func (sm *StateManager) replaceUnconfirmed(old *ClientRecord, clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr, principal string) (*SetClientIDResult, error) {
	// Remove old unconfirmed record
	delete(sm.clientsByID, old.ClientID)
	delete(sm.unconfirmedByName, clientIDStr)

	// Create new unconfirmed record
	clientID := sm.generateClientID()
	confirmVerf := sm.generateConfirmVerifier()

	record := &ClientRecord{
		ClientID:        clientID,
		ClientIDString:  clientIDStr,
		Verifier:        verifier,
		ConfirmVerifier: confirmVerf,
		Confirmed:       false,
		Callback:        callback,
		ClientAddr:      clientAddr,
		Principal:       principal,
		CreatedAt:       time.Now(),
		OpenOwners:      make(map[string]*OpenOwner),
	}

	sm.unconfirmedByName[clientIDStr] = record
	sm.clientsByID[clientID] = record

	logger.Debug("SETCLIENTID: replaced unconfirmed record",
		"client_id_str", clientIDStr,
		"new_client_id", clientID,
		"client_addr", clientAddr)

	return &SetClientIDResult{
		ClientID:        clientID,
		ConfirmVerifier: confirmVerf,
	}, nil
}

// ConfirmClientID implements SETCLIENTID_CONFIRM per RFC 7530 Section 9.1.1.
//
// It validates the confirm verifier and promotes the unconfirmed record to
// confirmed status. If a different confirmed record existed for the same
// client ID string, it is replaced.
//
// After confirmation, a lease timer is created for the client.
//
// Returns nil on success, or ErrStaleClientID if the client ID is unknown,
// or ErrStaleClientID if the confirm verifier doesn't match.
func (sm *StateManager) ConfirmClientID(clientID uint64, confirmVerifier [8]byte) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the record by client ID. SETCLIENTID_CONFIRM is a v4.0-only
	// operation, so a v4.1 record under this ID must not be reachable here:
	// confirming it would arm the CBPathUp probe and the lease timer the
	// EXCHANGE_ID flow manages through CREATE_SESSION instead.
	record := sm.v40ClientLocked(clientID)
	if record == nil {
		return fmt.Errorf("%w: client ID %d not found", ErrStaleClientID, clientID)
	}

	// If already confirmed, check for a pending re-SETCLIENTID (Case 5)
	// where an unconfirmed record exists with the same client ID.
	if record.Confirmed {
		if unconfirmed := sm.unconfirmedByName[record.ClientIDString]; unconfirmed != nil && unconfirmed.ClientID == clientID {
			// The re-SETCLIENTID reuses the client ID, so confirm the record
			// that already owns it and fold in what the new one carried.
			// Swapping the unconfirmed record in instead would leave two
			// records under one ID, and the one clientsByID does not point at
			// keeps a lease timer that RENEW never refreshes yet that still
			// fires and reaps the client.
			//
			// Validated before the fold, not by the shared check below: the
			// fold writes through to the live client, so a confirm carrying the
			// wrong verifier must be refused while the record it names is still
			// untouched. A stale retransmit of the previous confirm reaches
			// here whenever a re-SETCLIENTID is pending.
			if unconfirmed.ConfirmVerifier != confirmVerifier {
				return fmt.Errorf("%w: confirm verifier mismatch for client %d", ErrStaleClientID, clientID)
			}
			record.Verifier = unconfirmed.Verifier
			record.ConfirmVerifier = unconfirmed.ConfirmVerifier
			record.Callback = unconfirmed.Callback
			record.ClientAddr = unconfirmed.ClientAddr
			record.Principal = unconfirmed.Principal
			// The callback address just changed, so the path to it is unproven
			// again until the CB_NULL below says otherwise. Carrying the old
			// generation's verdict forward would let delegations be granted in
			// the window before that probe answers, and recalled to an address
			// this client never confirmed it listens on.
			record.CBPathUp = false
		} else {
			// True retransmit - validate verifier matches the confirmed record
			if record.ConfirmVerifier != confirmVerifier {
				return fmt.Errorf("%w: confirm verifier mismatch for confirmed client %d", ErrStaleClientID, clientID)
			}
			logger.Debug("SETCLIENTID_CONFIRM: retransmit for already-confirmed client",
				"client_id", clientID)
			return nil
		}
	}

	// Validate confirm verifier
	if record.ConfirmVerifier != confirmVerifier {
		return fmt.Errorf("%w: confirm verifier mismatch for client %d", ErrStaleClientID, clientID)
	}

	// Promote to confirmed -- remove from unconfirmed
	delete(sm.unconfirmedByName, record.ClientIDString)

	// If there's an existing confirmed record for the same name, remove it
	// (this happens on client reboot: Case 3 followed by CONFIRM)
	if oldConfirmed := sm.clientsByName[record.ClientIDString]; oldConfirmed != nil && oldConfirmed.ClientID != record.ClientID {
		// Stop old client's lease timer before removing
		if oldConfirmed.Lease != nil {
			oldConfirmed.Lease.Stop()
		}
		// The client rebooted, and RFC 7530 Section 16.34.5 requires more of
		// this confirm than forgetting the record: where a confirmed record
		// for the same id string already exists, "the server MUST remove
		// client x's relevant leased client state". Leaving it behind keeps
		// every stateid the client held before the reboot working, so the
		// files stay share-reserved and byte-range locked on behalf of an
		// incarnation that no longer exists and will never close them.
		sm.releaseClientStateLocked(oldConfirmed.ClientID)
		delete(sm.clientsByID, oldConfirmed.ClientID)
		logger.Info("SETCLIENTID_CONFIRM: replaced old confirmed client",
			"old_client_id", oldConfirmed.ClientID,
			"new_client_id", clientID)
	}

	// Mark as confirmed and store in confirmed map
	record.Confirmed = true
	sm.clientsByName[record.ClientIDString] = record

	// Create the lease timer for the newly confirmed client, replacing any
	// timer the record already carries: an orphaned timer still fires
	// onLeaseExpired for this client ID on its original schedule, reaping the
	// client however often RENEW refreshes the lease that replaced it.
	if record.Lease != nil {
		record.Lease.Stop()
	}
	record.Lease = NewLeaseState(clientID, sm.leaseDuration, sm.onLeaseExpired)
	record.LastRenewal = time.Now()

	logger.Info("SETCLIENTID_CONFIRM: client confirmed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString,
		"client_addr", record.ClientAddr)

	// Persist a durable client-recovery record so this client can reclaim its
	// state after an ungraceful server restart. Best-effort under
	// sm.mu: a persist failure logs a durability alarm but the confirm STILL
	// succeeds (the in-memory record is authoritative for this process). No-op
	// when no recovery store is wired.
	sm.persistClientRecoveryLocked(record.ClientID, record.ClientIDString, record.Verifier, record.Principal)

	// Verify callback path asynchronously via CB_NULL.
	// This runs in a goroutine so SETCLIENTID_CONFIRM returns immediately.
	if record.Callback.Addr != "" {
		cbInfo := record.Callback
		recordPtr := record // capture this record generation's pointer identity
		go func() {
			err := sm.cbNullFunc(context.Background(), cbInfo)
			sm.mu.Lock()
			defer sm.mu.Unlock()
			rec := sm.v40ClientLocked(clientID)
			if rec == nil || rec != recordPtr || rec.Callback != cbInfo {
				// Client was removed, this client ID now points at a different
				// record generation (reboot) while CB_NULL was in flight, or the
				// record kept its identity but moved to another callback address
				// (re-SETCLIENTID). Do not report this probe's verdict about an
				// address the record no longer uses.
				return
			}
			rec.CBPathUp = (err == nil)
			if err != nil {
				logger.Warn("CB_NULL failed, delegations disabled for client",
					"client_id", clientID, "error", err)
			} else {
				logger.Debug("CB_NULL succeeded, delegations enabled for client",
					"client_id", clientID)
			}
		}()
	}

	return nil
}

// GetClient returns the client record for the given client ID, or nil
// if no record exists. Used by RENEW and other operations that need
// to look up client state.
//
// v4.0 only: the shared index holds both minor versions, so a v4.1 client ID
// is filtered out here and comes back nil. Use clientRecordLocked for a
// lookup that should not care which flow minted the ID.
func (sm *StateManager) GetClient(clientID uint64) *ClientRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.v40ClientLocked(clientID)
}

// clientRecordLocked returns the record for clientID from the shared client
// index, or nil when no client owns that ID.
//
// Callers hold a client ID and have no reason to know which minor version
// minted it, which is why version-independent policy reads the record through
// here rather than naming a version.
//
// Caller must hold sm.mu.
func (sm *StateManager) clientRecordLocked(clientID uint64) *ClientRecord {
	return sm.clientsByID[clientID]
}

// v40ClientLocked returns the v4.0 record for clientID, or nil when the ID is
// unknown or names a v4.1 record. Every SETCLIENTID-flow operation reads the
// index through here: a v4.1 client must be invisible to RENEW, to
// SETCLIENTID_CONFIRM, and to the v4.0 expiry path, all of which would
// otherwise act on state the EXCHANGE_ID flow owns.
//
// Caller must hold sm.mu.
func (sm *StateManager) v40ClientLocked(clientID uint64) *ClientRecord {
	record := sm.clientsByID[clientID]
	if record == nil || record.MinorVersion != 0 {
		return nil
	}
	return record
}

// v41ClientLocked returns the v4.1 record for clientID, or nil when the ID is
// unknown or names a v4.0 record. Every EXCHANGE_ID-flow operation reads the
// index through here, mirroring v40ClientLocked.
//
// Caller must hold sm.mu.
func (sm *StateManager) v41ClientLocked(clientID uint64) *ClientRecord {
	record := sm.clientsByID[clientID]
	if record == nil || record.MinorVersion != 1 {
		return nil
	}
	return record
}

// renewConfirmedClient admits a confirmed client whose lease is still live and
// stamps the renewal. Shared by RENEW and by the implicit renewal every
// operation carrying a valid clientid gets, so the two admission paths cannot
// disagree about what a renewal updates.
//
// Caller must hold sm.mu.
func renewConfirmedClient(record *ClientRecord) error {
	if !record.Confirmed {
		return ErrStaleClientID
	}
	if record.Lease != nil {
		if record.Lease.IsExpired() {
			return ErrExpired
		}
		record.Lease.Renew()
	}
	record.LastRenewal = time.Now()
	return nil
}

// ValidateAndRenewClient admits clientID for an operation and renews its lease
// in one critical section. Unknown or unconfirmed gives ErrStaleClientID (RFC
// 7530 Sections 9.1.1 and 13.1.10.2); a lapsed lease not yet reaped gives
// ErrExpired (Section 9.6.3.2).
//
// Renewal is what Section 9.6.2 grants any operation carrying a valid clientid.
// Without it a v4.0 client that stops sending RENEW once it holds no open state
// has an active mount mistaken for an idle one.
//
// Callers admit before doing any work: OPEN creates or truncates its target
// before it establishes state, so a clientid rejected afterwards strands a file
// the client was told it had not created.
func (sm *StateManager) ValidateAndRenewClient(clientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record := sm.clientRecordLocked(clientID)
	if record == nil {
		return sm.unknownClientIDError(clientID)
	}
	return renewConfirmedClient(record)
}

// RemoveClient removes a client record and all associated state.
// Used by lease expiry to clean up expired clients.
func (sm *StateManager) RemoveClient(clientID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record := sm.v40ClientLocked(clientID)
	if record == nil {
		return
	}

	// Stop lease timer
	if record.Lease != nil {
		record.Lease.Stop()
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

	logger.Info("Client record removed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString)
}

// removeClientOpenStateLocked drops every open-owner belonging to clientID
// together with its open states, its lock states, and the locks those hold in
// the unified lock manager.
//
// It scans sm.openOwners by ClientID rather than walking the record's
// OpenOwners map: that map is only populated on the v4.0 path, and it goes
// stale even there because freeOpenStateidLocked removes owners from
// sm.openOwners without removing them from it.
//
// Caller must hold sm.mu.
func (sm *StateManager) removeClientOpenStateLocked(clientID uint64) {
	for key, owner := range sm.openOwners {
		if owner.ClientID != clientID {
			continue
		}
		for _, openState := range owner.OpenStates {
			for _, lockState := range openState.LockStates {
				delete(sm.lockStateByOther, lockState.Stateid.Other)
				sm.removeOwnerLocksLocked(lockState)
				if lockState.LockOwner != nil {
					delete(sm.lockOwners, lockState.LockOwner.Key())
				}
			}
			delete(sm.openStateByOther, openState.Stateid.Other)
			sm.removeOpenStateFromFileLocked(openState)
		}
		delete(sm.openOwners, key)
	}

	// Drop any retained closed-stateid -> owner replay entries for this client.
	for other, owner := range sm.closedOwnerByOther {
		if owner.ClientID == clientID {
			delete(sm.closedOwnerByOther, other)
		}
	}
}

// removeClientLockStateLocked frees the byte-range locks clientID holds on
// opens it does not own. LOCK takes the lock-owner's client ID from the wire
// and does not require it to match the client owning the open the lock hangs
// from, so removeClientOpenStateLocked -- which reaches locks through this
// client's own opens -- does not see these. Left behind, they stay held in the
// cross-protocol lock manager on behalf of a client that is gone, with nothing
// left that could ever unlock them.
//
// Caller must hold sm.mu.
func (sm *StateManager) removeClientLockStateLocked(clientID uint64) {
	for other, lockState := range sm.lockStateByOther {
		if lockState.LockOwner == nil || lockState.LockOwner.ClientID != clientID {
			continue
		}

		sm.removeOwnerLocksLocked(lockState)
		delete(sm.lockStateByOther, other)
		delete(sm.lockOwners, lockState.LockOwner.Key())
		detachLockStateFromOpen(lockState)
	}
}

// detachLockStateFromOpen drops a lock state from the open state it hangs off,
// so the open does not keep reporting a lock that is gone.
func detachLockStateFromOpen(lockState *LockState) {
	if lockState.OpenState == nil {
		return
	}
	for i, ls := range lockState.OpenState.LockStates {
		if ls != lockState {
			continue
		}
		lockState.OpenState.LockStates = append(
			lockState.OpenState.LockStates[:i],
			lockState.OpenState.LockStates[i+1:]...,
		)
		return
	}
}

// releaseClientStateLocked frees every open, lock and delegation held by
// clientID, first remembering the stateids so that a client which comes back
// and uses one is told its lease expired rather than told the stateid was
// never valid.
//
// It does not touch the client record itself: the caller knows why the state
// went away and which maps the record still belongs in.
//
// Caller must hold sm.mu.
func (sm *StateManager) releaseClientStateLocked(clientID uint64) {
	sm.markClientStateidsExpiredLocked(clientID)
	sm.removeClientOpenStateLocked(clientID)
	sm.removeClientLockStateLocked(clientID)

	for other, deleg := range sm.delegByOther {
		if deleg.ClientID != clientID {
			continue
		}
		// Both timers outlive the tables they fire against, so they are
		// stopped before the delegation leaves them.
		sm.cleanupDirDelegation(deleg)
		deleg.StopRecallTimer()
		sm.deleteDelegByOtherLocked(other)
		sm.removeDelegFromFile(deleg)

		logger.Info("Delegation revoked with the client's state",
			"client_id", clientID,
			"deleg_type", deleg.DelegType)
	}
}

// clientLeaseLapsedLocked reports whether a confirmed client's lease has run
// out. Both client generations are checked: the caller has a client ID and no
// reason to know which minor version minted it.
//
// Caller must hold sm.mu.
func (sm *StateManager) clientLeaseLapsedLocked(clientID uint64) bool {
	record := sm.clientRecordLocked(clientID)
	return record != nil && record.Confirmed && record.Lease != nil && record.Lease.IsExpired()
}

// expireLapsedHoldersLocked releases the state of every client that holds an
// open on fileHandle under a lease that has already run out, except for the
// clients in keepClientIDs. Callers name every client whose records they are
// holding a pointer into, since releasing one frees its opens and locks.
//
// What keeps an expired client's opens and locks alive is courtesy: a client
// that merely lost contact for a moment should not come back to find its locks
// broken, so the state outlives the lease and a sweeper collects it later.
// RFC 7530 Section 9.6.3.1 says what happens when someone else then wants the
// file: on "a lock or I/O request that conflicts with one of the courtesy
// locks", a courtesy lock that is not a delegation "MUST free the courtesy
// lock and grant the new request".
//
// So the collision, not the sweeper's schedule, is what ends the courtesy.
// Deferring to the sweep refuses a request that nothing live objects to, for
// however much of the sweep interval is left, which is why the same request is
// granted or refused depending on when it arrives.
//
// Caller must hold sm.mu.
func (sm *StateManager) expireLapsedHoldersLocked(fileHandle []byte, keepClientIDs ...uint64) {
	// Collected before anything is released: expiring a client rewrites the
	// index this ranges over.
	//
	// ponytail: linear scans of a slice rather than two sets. n is the distinct
	// clients holding state on ONE file, which is one or two outside a
	// share-reservation fight, and the allocation two maps would add lands on
	// every OPEN and LOCK. Switch to sets if a file ever collects enough
	// simultaneous holders for this to show up in a profile.
	var lapsed []uint64
	consider := func(clientID uint64) {
		if slices.Contains(keepClientIDs, clientID) || slices.Contains(lapsed, clientID) {
			return
		}
		if sm.clientLeaseLapsedLocked(clientID) {
			lapsed = append(lapsed, clientID)
		}
	}
	for _, os := range sm.openStateByFile[string(fileHandle)] {
		if os.Owner != nil {
			consider(os.Owner.ClientID)
		}
		// The lock-owner's client can differ from the open-owner's, and it is
		// the one holding the lock this request may be colliding with.
		for _, lockState := range os.LockStates {
			if lockState.LockOwner != nil {
				consider(lockState.LockOwner.ClientID)
			}
		}
	}

	for _, clientID := range lapsed {
		logger.Info("Expiring a lapsed client to resolve a conflicting request",
			"client_id", clientID)

		if v41 := sm.v41ClientLocked(clientID); v41 != nil {
			sm.markClientStateidsExpiredLocked(clientID)
			sm.purgeV41Client(v41)
			continue
		}
		sm.expireV40ClientLocked(clientID)
	}
}

// maxExpiredStateids caps how many freed-by-lease-cancellation stateids the
// server remembers; see the expiredStateids field for what overflow costs.
const maxExpiredStateids = 4096

// markClientStateidsExpiredLocked remembers every open, lock, and delegation
// stateid belonging to clientID as freed by a lease cancellation, so later use
// of one answers NFS4ERR_EXPIRED rather than NFS4ERR_BAD_STATEID (RFC 7530
// Section 9.6.3.2). Call it before the state itself is dropped.
//
// Caller must hold sm.mu.
func (sm *StateManager) markClientStateidsExpiredLocked(clientID uint64) {
	if len(sm.expiredStateids) >= maxExpiredStateids {
		sm.expiredStateids = make(map[[types.NFS4_OTHER_SIZE]byte]struct{})
	}

	for _, owner := range sm.openOwners {
		if owner.ClientID != clientID {
			continue
		}
		for _, openState := range owner.OpenStates {
			sm.expiredStateids[openState.Stateid.Other] = struct{}{}
			for _, lockState := range openState.LockStates {
				sm.expiredStateids[lockState.Stateid.Other] = struct{}{}
			}
		}
	}

	for other, lockState := range sm.lockStateByOther {
		if lockState.LockOwner != nil && lockState.LockOwner.ClientID == clientID {
			sm.expiredStateids[other] = struct{}{}
		}
	}

	for other, deleg := range sm.delegByOther {
		if deleg.ClientID == clientID {
			sm.expiredStateids[other] = struct{}{}
		}
	}
}

// isExpiredStateidLocked reports whether the state this stateid named was freed
// when the server cancelled the owning client's lease. Callers use it on a
// table miss, before falling back to NFS4ERR_BAD_STATEID.
//
// Caller must hold sm.mu (read or write).
func (sm *StateManager) isExpiredStateidLocked(other [types.NFS4_OTHER_SIZE]byte) bool {
	_, ok := sm.expiredStateids[other]
	return ok
}

// onLeaseExpired is the callback invoked when a client's lease timer fires.
// It cleans up all state for the expired client: open states, open owners,
// and the client record itself.
//
// IMPORTANT: This runs from a timer goroutine and must NOT hold any lease.mu
// when calling into StateManager. The timer callback in NewLeaseState is a
// simple function that calls this method directly.
func (sm *StateManager) onLeaseExpired(clientID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.expireV40ClientLocked(clientID)
}

// expireV40ClientLocked drops a v4.0 client and everything it holds. It is what
// a lapsed lease does, and what a conflicting request does to a client whose
// lease already lapsed.
//
// Caller must hold sm.mu.
func (sm *StateManager) expireV40ClientLocked(clientID uint64) {
	record := sm.v40ClientLocked(clientID)
	if record == nil {
		return
	}

	logger.Info("NFSv4 client lease expired, cleaning up state",
		"client_id", clientID,
		"client_id_str", record.ClientIDString,
		"client_addr", record.ClientAddr)

	// The timer is already spent when its own callback brought us here, but a
	// conflicting request can expire a lapsed client while the timer is still
	// armed, and a later fire would look up a client that no longer exists.
	if record.Lease != nil {
		record.Lease.Stop()
	}

	// The client's lease lapsed without renewal: it no longer holds reclaimable
	// state, so drop its durable recovery record. Best-effort; no-op
	// when no recovery store is wired.
	sm.deleteClientRecoveryLocked(record.ClientIDString)

	sm.releaseClientStateLocked(clientID)

	// Remove client from all maps
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
}

// RevokeDelegation revokes a delegation by its stateid "other" field.
//
// Called by the recall timer when the client does not respond to CB_RECALL
// within the lease period. Per RFC 7530 Section 10.4.6.
//
// The delegation is marked as Revoked and removed from delegByFile,
// but kept in delegByOther for stale stateid detection.
// The file handle is added to the recently-recalled cache.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) RevokeDelegation(delegOther [types.NFS4_OTHER_SIZE]byte) {
	sm.mu.Lock()

	deleg, exists := sm.delegByOther[delegOther]
	if !exists || deleg.Revoked {
		sm.mu.Unlock()
		return
	}

	sm.markDelegRevokedLocked(deleg)
	sm.removeDelegFromFile(deleg)
	sm.addRecentlyRecalled(deleg.FileHandle)

	// Clean up LockManager delegation and stateid mapping
	lmDelegID := deleg.LockManagerDelegID
	fhKey := string(deleg.FileHandle)
	if lmDelegID != "" {
		delete(sm.delegStateidMap, lmDelegID)
	}

	// Capture lockManager reference before releasing mu
	lockMgr := sm.lockManagerFor(deleg.FileHandle)

	// Keep in delegByOther for stale stateid detection.

	logger.Warn("Delegation revoked due to recall timeout",
		"client_id", deleg.ClientID,
		"deleg_type", deleg.DelegType)

	sm.mu.Unlock()

	// Revoke in LockManager outside sm.mu (avoids deadlock per Pitfall 2)
	if lockMgr != nil && lmDelegID != "" {
		_ = lockMgr.RevokeDelegation(fhKey, lmDelegID)
	}
}

// Shutdown stops all active lease timers, recall timers, and the grace period
// for graceful server shutdown.
func (sm *StateManager) Shutdown() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, record := range sm.clientsByID {
		if record.Lease != nil {
			record.Lease.Stop()
		}
	}

	// Stop all active delegation recall timers and directory delegation
	// batch timers to prevent timer goroutines firing after shutdown.
	for _, deleg := range sm.delegByOther {
		deleg.StopRecallTimer()
		sm.cleanupDirDelegation(deleg)
	}

	// Stop all backchannel senders to prevent orphan goroutines
	for _, session := range sm.sessionsByID {
		if session.backchannelSender != nil {
			session.backchannelSender.Stop()
			session.backchannelSender = nil
		}
	}

	if sm.gracePeriod != nil {
		sm.gracePeriod.Stop()
	}

	logger.Info("StateManager: all lease, recall timers, and backchannel senders stopped")
}

// ============================================================================
// Grace Period Operations
// ============================================================================

// StartGracePeriod creates and starts a grace period for server restart recovery.
//
// The NFS adapter should call this on startup if there were previous clients
// (loaded from a saved client state file). During the grace period:
//   - OPEN with CLAIM_NULL returns NFS4ERR_GRACE
//   - OPEN with CLAIM_PREVIOUS is allowed (reclaim)
//   - RENEW, CLOSE, READ/WRITE with existing stateids work normally
//
// The grace period ends automatically after graceDuration, or early if
// all expectedClientIDs have reclaimed. If expectedClientIDs is empty,
// the grace period is skipped entirely.
func (sm *StateManager) StartGracePeriod(expectedClientIDs []uint64) {
	sm.mu.Lock()
	gp := NewGracePeriodState(sm.graceDuration, func() {
		logger.Info("NFSv4 grace period ended")
	})
	sm.gracePeriod = gp
	sm.mu.Unlock()

	// StartGrace handles its own locking
	gp.StartGrace(expectedClientIDs)
}

// IsInGrace returns true if the server is currently in a grace period.
func (sm *StateManager) IsInGrace() bool {
	sm.mu.RLock()
	gp := sm.gracePeriod
	sm.mu.RUnlock()

	if gp == nil {
		return false
	}
	return gp.IsInGrace()
}

// GraceStatus returns structured information about the grace period.
// Returns a zero GraceStatusInfo if no grace period has been configured.
func (sm *StateManager) GraceStatus() GraceStatusInfo {
	sm.mu.RLock()
	gp := sm.gracePeriod
	sm.mu.RUnlock()

	if gp == nil {
		return GraceStatusInfo{}
	}
	return gp.Status()
}

// ForceEndGrace immediately ends the grace period.
// No-op if no grace period is active.
func (sm *StateManager) ForceEndGrace() {
	sm.mu.RLock()
	gp := sm.gracePeriod
	sm.mu.RUnlock()

	if gp == nil {
		return
	}
	gp.ForceEnd()
}

// ReclaimComplete marks a client as having finished reclaiming state.
//
// oneFS selects which of the two RECLAIM_COMPLETE scopes the client is
// retiring (RFC 8881 Section 18.51.3). A global reclaim (oneFS false) covers
// every lock the client held on the previous server instance. A file
// system-specific reclaim (oneFS true) covers only the file system named by
// the current filehandle, and only because that file system is migrating. A
// client may legitimately issue both forms in either order, so the two do not
// deduplicate against each other.
//
// Section 18.51.4 scopes the duplicate to "once for each server instance or
// occasion of the transition of a file system", so only the global reclaim is
// tracked here: it returns NFS4ERR_COMPLETE_ALREADY on a second global call.
// No file system ever migrates here, and Section 18.51.3 requires that a
// file system-specific reclaim naming a file system that is not migrating
// "returns NFS4_OK and is otherwise ignored".
//
// The first global call succeeds whether or not a grace period is running:
// RECLAIM_COMPLETE outside grace is not an error, it just has nothing to
// reclaim. When a grace period is running, it also retires the client from the
// reclaim roster so the window can end early.
func (sm *StateManager) ReclaimComplete(clientID uint64, oneFS bool) error {
	// ponytail: no per-file-system reclaim set, because nothing here migrates
	// and an ignored call needs no bookkeeping; add one keyed by file system
	// if migration ever lands, and refuse the second call per file system.
	if oneFS {
		return nil
	}

	sm.mu.Lock()
	gp := sm.gracePeriod
	// Resolve the durable recovery key for this client (v4.1 = co_ownerid,
	// v4.0 = nfs_client_id4 string) so the boot-loaded string roster early-exits
	// and the reclaim-done marker is persisted.
	recoveryKey := sm.recoveryKeyForClientLocked(clientID)

	// A caller with no record has nothing to deduplicate against, so it is let
	// through: RECLAIM_COMPLETE is SEQUENCE-gated, so a live session always
	// resolves to a record, and an unknown client ID is refused by the session
	// lookup before it reaches here.
	if record := sm.clientRecordLocked(clientID); record != nil {
		if record.ReclaimComplete {
			sm.mu.Unlock()
			return ErrCompleteAlready
		}
		record.ReclaimComplete = true
	}
	if recoveryKey != "" {
		sm.recordReclaimCompleteLocked(clientID, recoveryKey)
	}
	sm.mu.Unlock()

	if gp != nil {
		if recoveryKey != "" {
			gp.ClientReclaimedByString(recoveryKey)
		}
		gp.ClientReclaimed(clientID)
	}
	return nil
}

// CheckGraceForNewState returns NFS4ERR_GRACE if the server is in a grace period
// and the operation would create new state. Returns nil if the operation is allowed.
//
// This should be called before any new state-creating operation (OPEN with
// CLAIM_NULL, LOCK). Operations that use existing state (READ, WRITE, RENEW,
// CLOSE) should NOT call this.
//
// NOTE: LOCK operations will also need to check this.
func (sm *StateManager) CheckGraceForNewState() error {
	if sm.IsInGrace() {
		return ErrGrace
	}
	return nil
}

// GetConfirmedClientIDs returns a list of all confirmed client IDs.
// Used for saving client state before shutdown so the grace period
// can identify which clients need to reclaim on restart.
func (sm *StateManager) GetConfirmedClientIDs() []uint64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	ids := make([]uint64, 0, len(sm.clientsByName))
	for _, record := range sm.clientsByName {
		ids = append(ids, record.ClientID)
	}
	return ids
}

// LoadPreviousClients populates the expected clients list for grace period startup.
// Called by the NFS adapter after reading saved client IDs from disk.
// This is a convenience wrapper around StartGracePeriod.
func (sm *StateManager) LoadPreviousClients(clientIDs []uint64) {
	sm.StartGracePeriod(clientIDs)
}

// SaveClientState returns snapshots of all confirmed clients for serialization.
// The NFS adapter calls this during graceful shutdown to persist client state
// to disk, enabling grace period recovery on the next startup.
func (sm *StateManager) SaveClientState() []ClientSnapshot {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	snapshots := make([]ClientSnapshot, 0, len(sm.clientsByName))
	for _, record := range sm.clientsByName {
		snapshots = append(snapshots, ClientSnapshot{
			ClientID:       record.ClientID,
			ClientIDString: record.ClientIDString,
			Verifier:       record.Verifier,
			ClientAddr:     record.ClientAddr,
		})
	}
	return snapshots
}

// ============================================================================
// Open File Operations
// ============================================================================

// OpenFile implements the state management side of OPEN.
//
// It looks up or creates an OpenOwner for (clientID, ownerData), validates
// the seqid, and either creates a new OpenState or updates an existing one
// (share_access/share_deny accumulation for same file).
//
// Grace period rules:
//   - CLAIM_NULL (new open): blocked with NFS4ERR_GRACE during grace period
//   - CLAIM_PREVIOUS (reclaim): allowed during grace period, blocked with
//     NFS4ERR_NO_GRACE outside grace period
//
// Per RFC 7530 Section 9.1.7:
//   - First OPEN for a new owner creates unconfirmed state + sets OPEN4_RESULT_CONFIRM
//   - Subsequent OPENs from a confirmed owner do not set CONFIRM
//   - Same owner + same file => OR the share_access and share_deny bits
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) OpenFile(
	clientID uint64,
	ownerData []byte,
	seqid uint32,
	fileHandle []byte,
	shareAccess, shareDeny uint32,
	claimType uint32,
	principal ...string,
) (result *OpenFileResult, err error) {
	// Grace period checks (before acquiring sm.mu)
	switch claimType {
	case types.CLAIM_NULL:
		// New open: blocked during grace period
		if sm.IsInGrace() {
			return nil, ErrGrace
		}
	case types.CLAIM_PREVIOUS:
		// Reclaim: only allowed during grace period
		if !sm.IsInGrace() {
			return nil, ErrNoGrace
		}
		// Verifier-gated reclaim (RFC 7530 §9.1.4): a reclaiming
		// client whose ClientIDString matches a pre-restart record but whose
		// BootVerifier changed rebooted and must NOT reclaim prior state.
		// validateReclaimVerifier self-gates on the boot-load snapshot, so it is
		// a no-op when no durable prior record exists (reclaim allowed as before).
		sm.mu.RLock()
		gp := sm.gracePeriod
		rec := sm.v40ClientLocked(clientID)
		sm.mu.RUnlock()
		if rec != nil {
			if err := sm.validateReclaimVerifier(rec.ClientIDString, rec.Verifier); err != nil {
				return nil, err
			}
		}
		// Notify the grace period that this client has reclaimed. Mark both by
		// numeric clientID (same-epoch reclaim) and by ClientIDString (the
		// boot-loaded roster is keyed by string since reclaiming clients get a
		// fresh clientID after restart).
		if gp != nil {
			gp.ClientReclaimed(clientID)
			if rec != nil {
				gp.ClientReclaimedByString(rec.ClientIDString)
			}
		}
		// v4.0 has no RECLAIM_COMPLETE; the first successful CLAIM_PREVIOUS is
		// the analog reclaim marker. On that false→true transition, set the
		// in-memory flag (the durable write mirrors it, and a pending retry
		// re-validates against it) and persist it so a second restart inside
		// one grace window does not wait on this client again. Later reclaim
		// OPENs short-circuit: persisting per OPEN would serialize a
		// recoveryPersistTimeout store call under sm.mu for every reclaimed
		// file.
		if rec != nil {
			sm.mu.Lock()
			firstReclaim := !rec.ReclaimComplete
			rec.ReclaimComplete = true
			if firstReclaim {
				sm.recordReclaimCompleteLocked(clientID, rec.ClientIDString)
			}
			sm.mu.Unlock()
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up or create the open-owner
	ownerKey := makeOwnerKey(clientID, ownerData)
	owner, ownerExists := sm.openOwners[ownerKey]

	if ownerExists {
		// Existing owner: validate seqid
		// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
		if seqid != 0 {
			validation := owner.ValidateSeqID(seqid)
			switch validation {
			case SeqIDReplay:
				// Return cached result
				if owner.LastResult != nil {
					return &OpenFileResult{
						IsReplay:     true,
						CachedStatus: owner.LastResult.Status,
						CachedData:   owner.LastResult.Data,
					}, nil
				}
				// No cached result (shouldn't happen), treat as bad seqid
				return nil, ErrBadSeqid
			case SeqIDBad:
				return nil, ErrBadSeqid
			case SeqIDOK:
				// Continue with normal processing
			}
		}
	} else {
		// New owner: create it
		clientRecord := sm.clientRecordLocked(clientID)
		owner = &OpenOwner{
			ClientID:  clientID,
			OwnerData: make([]byte, len(ownerData)),
			// ponytail: recorded once, at the OPEN that creates the owner,
			// rather than per open state as FreeBSD's ls_uid is. Every client
			// that matters keys its open-owners by credential (Linux
			// nfs4_get_state_owner takes the cred), so one principal per owner
			// holds; track it per OpenState only if a client turns up that
			// shares an owner across users.
			Principal: firstOrEmpty(principal),

			Confirmed:    false,
			OpenStates:   make([]*OpenState, 0),
			ClientRecord: clientRecord,
			key:          ownerKey,
		}
		copy(owner.OwnerData, ownerData)
		sm.openOwners[ownerKey] = owner

		// OpenOwners is populated on the v4.0 path only; it is left nil on a
		// v4.1 record, whose owners are torn down by purgeV41Client instead.
		if clientRecord != nil && clientRecord.OpenOwners != nil {
			clientRecord.OpenOwners[string(ownerData)] = owner
		}
	}

	// The seqid is now this owner's, whatever the OPEN goes on to return: a
	// failure below (share reservation conflict) consumes it just as success
	// does, and the client advances either way.
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Enforce share reservations (RFC 7530 Section 9.9; Linux nfsd
	// nfs4_share_conflict). This runs AFTER the owner seqid/replay gate above:
	// a replayed OPEN must return its cached result and a bad seqid must return
	// NFS4ERR_BAD_SEQID — neither may be turned into NFS4ERR_SHARE_DENIED by a
	// conflict that arose after the original request. Reclaim (CLAIM_PREVIOUS)
	// re-establishes prior state and is exempt. The scan runs under sm.mu so it
	// observes a consistent snapshot of every live open.
	if claimType != types.CLAIM_PREVIOUS {
		sm.expireLapsedHoldersLocked(fileHandle, clientID)

		if conflict := sm.shareConflictLocked(fileHandle, shareAccess, shareDeny); conflict {
			logger.Debug("OpenFile: share reservation conflict",
				"client_id", clientID,
				"owner", string(ownerData),
				"req_access", shareAccess,
				"req_deny", shareDeny)
			return nil, ErrShareDenied
		}
	}

	// Check if this owner already has an open on this file
	var existingState *OpenState
	for _, os := range owner.OpenStates {
		if bytes.Equal(os.FileHandle, fileHandle) {
			existingState = os
			break
		}
	}

	var resultStateid types.Stateid4

	if existingState != nil {
		// Accumulate share_access and share_deny (Pitfall 7)
		existingState.ShareAccess |= shareAccess
		existingState.ShareDeny |= shareDeny
		existingState.openedAccessModes |= shareModeBit(shareAccess)

		// Increment the stateid seqid for this operation
		existingState.Stateid.Seqid = nextSeqID(existingState.Stateid.Seqid)
		resultStateid = existingState.Stateid
	} else {
		// Create new OpenState
		other := sm.generateStateidOther(StateTypeOpen)
		resultStateid = types.Stateid4{
			Seqid: 1,
			Other: other,
		}

		fhCopy := make([]byte, len(fileHandle))
		copy(fhCopy, fileHandle)

		openState := &OpenState{
			Stateid:           resultStateid,
			Owner:             owner,
			FileHandle:        fhCopy,
			ShareAccess:       shareAccess,
			ShareDeny:         shareDeny,
			openedAccessModes: shareModeBit(shareAccess),
			Confirmed:         owner.Confirmed,
		}

		owner.OpenStates = append(owner.OpenStates, openState)
		sm.openStateByOther[other] = openState
		sm.addOpenStateToFileLocked(openState)
	}

	// Determine rflags: OPEN4_RESULT_CONFIRM only if owner is not yet confirmed
	var rflags uint32
	rflags |= types.OPEN4_RESULT_LOCKTYPE_POSIX
	if !owner.Confirmed {
		rflags |= types.OPEN4_RESULT_CONFIRM
	}

	// Update owner seqid and cache
	owner.LastSeqID = seqid

	logger.Debug("OpenFile: state created/updated",
		"client_id", clientID,
		"owner", string(ownerData),
		"seqid", seqid,
		"stateid_seqid", resultStateid.Seqid,
		"share_access", shareAccess,
		"share_deny", shareDeny,
		"confirm_required", !owner.Confirmed)

	return &OpenFileResult{
		Stateid: resultStateid,
		RFlags:  rflags,
	}, nil
}

// shareConflictLocked reports whether granting an OPEN with the requested
// share_access / share_deny on fileHandle would conflict with an open already
// held on it. A conflict exists when the requested access is denied by an
// existing open, or the requested deny would exclude an existing open's access.
//
// RFC 7530 Section 9.9 gives the rule as pseudo-code over the file's
// accumulated state -- "(request.access & file_state.deny) || (request.deny &
// file_state.access)" -- and then says in as many words that "this checking of
// share reservations on OPEN is done with no exception for an existing OPEN for
// the same open-owner". Skipping the requesting owner's own opens, on the theory
// that share bits merely accumulate per owner, let an owner that had denied
// READ to everyone go on to open the same file for reading itself. Linux nfsd
// keeps the deny mask on the file (nfs4_file, fi_share_deny) for the same
// reason.
//
// Caller must hold sm.mu, for reading or for writing.
func (sm *StateManager) shareConflictLocked(
	fileHandle []byte,
	reqAccess, reqDeny uint32,
) bool {
	// Iterate only the opens on this file via the secondary per-file index
	// (openStateByFile), not every open in the server.
	for _, os := range sm.openStateByFile[string(fileHandle)] {
		if reqAccess&os.ShareDeny != 0 || reqDeny&os.ShareAccess != 0 {
			return true
		}
	}
	return false
}

// addOpenStateToFileLocked inserts an OpenState into the per-file open index.
// Caller must hold sm.mu.
func (sm *StateManager) addOpenStateToFileLocked(os *OpenState) {
	fhKey := string(os.FileHandle)
	sm.openStateByFile[fhKey] = append(sm.openStateByFile[fhKey], os)
}

// removeOpenStateFromFileLocked removes an OpenState from the per-file open
// index, dropping the file's slot entirely when it becomes empty so the map
// stays bounded. Caller must hold sm.mu.
func (sm *StateManager) removeOpenStateFromFileLocked(os *OpenState) {
	fhKey := string(os.FileHandle)
	states := sm.openStateByFile[fhKey]
	for i, s := range states {
		if s != os {
			continue
		}
		// Shift the tail down, nil out the now-unused last slot so the backing
		// array does not retain the removed OpenState, then reslice.
		copy(states[i:], states[i+1:])
		states[len(states)-1] = nil
		states = states[:len(states)-1]
		if len(states) == 0 {
			delete(sm.openStateByFile, fhKey)
		} else {
			sm.openStateByFile[fhKey] = states
		}
		return
	}
}

// ReplayOpenSeqid reports the status to replay when seqid retransmits an OPEN
// this server refused on its own, so the caller can answer it without running
// the OPEN a second time (RFC 7530 Section 9.1.7).
//
// Re-executing such a retransmission answers from the state of the world now
// rather than the state it had when the client first asked, so a refusal whose
// cause has since gone away -- the colliding name removed, the permission
// granted -- came back as a success the client had no reply slot for.
//
// It covers only the refusals ConsumeOpenSeqid recorded. An OPEN that reached
// the state layer is replayed by OpenFile from the owner's shared reply cache,
// and that cache must not be consulted here: CLOSE, OPEN_CONFIRM and
// OPEN_DOWNGRADE write to it too, so it may hold a reply of a different shape,
// which an OPEN replaying it would return under its own operation number.
func (sm *StateManager) ReplayOpenSeqid(clientID uint64, ownerData []byte, seqid uint32) (uint32, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	owner, exists := sm.openOwners[makeOwnerKey(clientID, ownerData)]
	if !exists || owner.openRefusal == nil {
		return 0, false
	}
	if owner.openRefusal.seqid != seqid || owner.ValidateSeqID(seqid) != SeqIDReplay {
		return 0, false
	}
	return owner.openRefusal.status, true
}

// ConsumeOpenSeqid records against an open-owner's sequence an OPEN that failed
// before it ever reached OpenFile, and caches the status so a retransmission
// replays it.
//
// RFC 7530 Section 9.1.7 advances an owner's sequence for every OPEN that
// reaches seqid checking, and the client advances its own whether the server
// answered success or a consuming error. An OPEN the handler refuses on its own
// -- a create collision, a target of the wrong object type, a name the server
// rejects -- never reached the state manager, so its seqid went unrecorded and
// the server fell one behind the client. Every later OPEN for that owner was
// then answered NFS4ERR_BAD_SEQID, which a client can only escape by tearing
// the owner down.
//
// It is a no-op for an owner that does not exist yet, because a first OPEN that
// fails leaves no owner behind and the client's retry at seqid 1 is valid
// against a fresh one; for a seqid that is not the owner's expected next one,
// so a replay returns its cached reply rather than consuming a second seqid;
// and for the statuses Section 9.1.7 exempts.
func (sm *StateManager) ConsumeOpenSeqid(clientID uint64, ownerData []byte, seqid, status uint32) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	owner, exists := sm.openOwners[makeOwnerKey(clientID, ownerData)]
	if !exists {
		return
	}
	if owner.ValidateSeqID(seqid) != SeqIDOK {
		return
	}
	before := owner.LastSeqID
	owner.consumeSeqidOnError(seqid, &NFS4StateError{Status: status})
	// consumeSeqidOnError leaves the sequence alone for the statuses
	// Section 9.1.7 exempts; nothing was recorded, so there is nothing to
	// replay either.
	if owner.LastSeqID != before {
		owner.openRefusal = &openRefusal{seqid: seqid, status: status}
	}
}

// CacheOpenOwnerResult stores the encoded reply for an open-owner so that a
// replay (retransmit at the same seqid) returns the exact original response.
//
// It must be called by the handler after encoding the reply of EVERY
// owner-seqid-advancing op (OPEN/CLOSE/OPEN_DOWNGRADE/OPEN_CONFIRM), not just
// OPEN — otherwise a retransmit of a CLOSE/DOWNGRADE/CONFIRM would replay stale
// OPEN bytes (or none), causing NFS4ERR_BAD_SEQID storms (RFC 7530 §9.1.7,
// mirrors Linux nfsd so_replay). data must be a caller-owned copy; it is
// retained until the next op or lease expiry.
func (sm *StateManager) CacheOpenOwnerResult(clientID uint64, ownerData []byte, status uint32, data []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ownerKey := makeOwnerKey(clientID, ownerData)
	if owner, exists := sm.openOwners[ownerKey]; exists {
		owner.LastResult = &CachedResult{Status: status, Data: data}
		return
	}

	// The owner may have just been removed from the live table by CLOSE (its
	// last open state went away). It is still reachable via closedOwnerByOther
	// so the CLOSE reply can be cached for a retransmitted CLOSE replay.
	for _, owner := range sm.closedOwnerByOther {
		if owner.ClientID == clientID && bytes.Equal(owner.OwnerData, ownerData) {
			owner.LastResult = &CachedResult{Status: status, Data: data}
			return
		}
	}
}

// CacheLockOwnerResult stores the encoded reply for a lock-owner so that a
// replayed LOCK/LOCKU at the same lock-owner seqid returns the exact original
// response instead of NFS4ERR_BAD_SEQID (which the Linux client treats as
// fatal, dropping the lock-owner and silently losing locks). Symmetric to
// CacheOpenOwnerResult; see RFC 7530 §9.1.7 / §8.20.
func (sm *StateManager) CacheLockOwnerResult(clientID uint64, ownerData []byte, status uint32, data []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	loKey := makeLockOwnerKey(clientID, ownerData)
	lockOwner, exists := sm.lockOwners[loKey]
	if !exists {
		return
	}

	lockOwner.LastResult = &CachedResult{
		Status: status,
		Data:   data,
	}
}

// ConfirmOpen implements the OPEN_CONFIRM operation's state management.
//
// Per RFC 7530 Section 16.20:
//   - Validates the stateid
//   - Validates the seqid on the owner
//   - Promotes the open-owner and open-state to confirmed
//   - Increments the stateid seqid
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) ConfirmOpen(stateid *types.Stateid4, seqid uint32, callerClientID uint64) (result *OpenSeqResult, err error) {
	// A special stateid names no state at all, and RFC 7530 Section 9.1.4.3
	// admits one only on READ, WRITE and SETATTR. stateidMissError documents
	// why it must be rejected before a table miss is classified.
	if stateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: reject one that names another client's
	// state before acting on it; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return nil, err
	}

	owner := openState.Owner

	// The request is attributable to this owner, so any failure below consumes
	// its seqid (RFC 7530 Section 9.1.7).
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Validate seqid on the owner
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if seqid != 0 {
		validation := owner.ValidateSeqID(seqid)
		switch validation {
		case SeqIDReplay:
			// Replay the exact cached reply of the original op at this seqid.
			if owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// The stateid must name this open's current seqid, compared after the owner
	// seqid above so a retransmit still replays; see checkStateidSeqid.
	if err := checkStateidSeqid(stateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// An open confirms once. A second OPEN_CONFIRM finds no unconfirmed state,
	// so the stateid it carries no longer names anything OPEN_CONFIRM can act
	// on (RFC 7530 Section 16.18.5).
	if openState.Confirmed {
		return nil, ErrBadStateid
	}

	// Promote to confirmed
	openState.Confirmed = true
	owner.Confirmed = true

	// Increment stateid seqid
	openState.Stateid.Seqid = nextSeqID(openState.Stateid.Seqid)

	// Update owner seqid and cache
	owner.LastSeqID = seqid

	resultStateid := openState.Stateid

	logger.Debug("ConfirmOpen: owner confirmed",
		"client_id", owner.ClientID,
		"stateid_seqid", resultStateid.Seqid)

	return &OpenSeqResult{
		Stateid:       resultStateid,
		OwnerClientID: owner.ClientID,
		OwnerData:     owner.OwnerData,
	}, nil
}

// ConfirmOpenV41 confirms an open-owner for NFSv4.1 without incrementing
// the stateid seqid. In v4.1, OPEN_CONFIRM doesn't exist; owners are
// implicitly confirmed through the session/slot mechanism. The stateid
// must remain at seqid=1 (the initial value) because the Linux NFS client's
// nfs_set_open_stateid_locked() expects sequential stateids starting from 1.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) ConfirmOpenV41(stateid *types.Stateid4, callerClientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: another client's open must not be
	// confirmed through this caller; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return err
	}

	openState.Confirmed = true
	openState.Owner.Confirmed = true
	return nil
}

// CloseFile implements the CLOSE operation's state management.
//
// Per RFC 7530 Section 16.3:
//   - Validates the stateid
//   - Validates the seqid on the owner
//   - Removes the OpenState from all maps
//   - If owner has no more OpenStates, cleans up the owner
//   - Returns a zeroed stateid
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) CloseFile(stateid *types.Stateid4, seqid uint32, callerClientID uint64) (result *OpenSeqResult, err error) {
	// A special stateid names no open state, and RFC 7530 Section 9.1.4.3
	// admits one only on READ, WRITE and SETATTR, so CLOSE has nothing to act
	// on. stateidMissError documents why it must be rejected before a table
	// miss is classified.
	if stateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		// The state was already removed by a prior CLOSE. If this is a
		// retransmit of that CLOSE (same owner-seqid), replay its cached reply
		// (RFC 7530 §9.1.7) rather than returning NFS4ERR_BAD_STATEID.
		// The cached reply belongs to the owner that sent the original CLOSE, so
		// it is only replayed to that owner's client; see checkStateidOwner.
		if owner, ok := sm.closedOwnerByOther[stateid.Other]; ok && seqid != 0 &&
			checkStateidOwner(callerClientID, owner.ClientID) == nil {
			if owner.ValidateSeqID(seqid) == SeqIDReplay && owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
		}
		return nil, sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: reject one that names another client's
	// state before acting on it; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return nil, err
	}

	owner := openState.Owner

	// The request is attributable to this owner, so any failure below consumes
	// its seqid (RFC 7530 Section 9.1.7). NFS4ERR_LOCKS_HELD is the case that
	// matters in practice: a database closing a file while byte-range locks are
	// outstanding hits it routinely, and leaving the seqid behind would answer
	// every later CLOSE and LOCK for this owner with NFS4ERR_BAD_SEQID.
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Validate seqid on the owner
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if seqid != 0 {
		validation := owner.ValidateSeqID(seqid)
		switch validation {
		case SeqIDReplay:
			// Replay the exact cached CLOSE reply rather than re-deriving it.
			if owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// The stateid must name this open's current seqid, compared after the owner
	// seqid above so a retransmit still replays; see checkStateidSeqid.
	if err := checkStateidSeqid(stateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// Refuse while ranges are genuinely held -- RFC 7530 Section 16.2.4 permits
	// refusing or freeing, and this server refuses.
	if sm.hasOutstandingLocksLocked(openState) {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_LOCKS_HELD,
			Message: "cannot close: byte-range locks still held, use LOCKU first",
		}
	}

	// Remove the open state from the "other" map and per-file index.
	delete(sm.openStateByOther, stateid.Other)
	sm.removeOpenStateFromFileLocked(openState)

	// The open's lock stateids die with it: they hold no ranges (checked above)
	// and nothing may use them once the open they derive from is gone.
	for _, lockState := range openState.LockStates {
		delete(sm.lockStateByOther, lockState.Stateid.Other)
	}
	for _, lockState := range openState.LockStates {
		sm.dropLockOwnerIfUnreferencedLocked(lockState.LockOwner)
	}
	openState.LockStates = nil

	// Remember which retained owner this now-closed stateid belonged to so a
	// retransmitted CLOSE can still resolve the owner's cached reply.
	sm.closedOwnerByOther[stateid.Other] = owner

	// Remove from owner's OpenStates list
	for i, os := range owner.OpenStates {
		if os == openState {
			owner.OpenStates = append(owner.OpenStates[:i], owner.OpenStates[i+1:]...)
			break
		}
	}

	// Update owner seqid
	owner.LastSeqID = seqid

	// If the owner has no more open states, drop it from the LIVE owner table
	// (openOwners + client record) so a subsequent OPEN by the same owner name
	// is treated as a brand-new owner — NOT subjected to this now-stale seqid,
	// which would otherwise mis-classify a genuinely new OPEN as a replay (and
	// return the cached CLOSE reply) or as NFS4ERR_BAD_SEQID. This matches the
	// pre-retention behavior. The owner object itself stays reachable via
	// closedOwnerByOther (installed above), so a retransmitted CLOSE can still
	// replay its cached reply (RFC 7530 §9.1.7); that entry is reaped on lease
	// expiry.
	if len(owner.OpenStates) == 0 {
		delete(sm.openOwners, owner.Key())
		if owner.ClientRecord != nil {
			delete(owner.ClientRecord.OpenOwners, string(owner.OwnerData))
		}
		logger.Debug("CloseFile: owner removed from live table (no more open states), retained for CLOSE replay",
			"client_id", owner.ClientID)
	}

	logger.Debug("CloseFile: state removed",
		"client_id", owner.ClientID,
		"seqid", seqid)

	// Return zeroed stateid plus owner identity for reply caching.
	return &OpenSeqResult{
		OwnerClientID: owner.ClientID,
		OwnerData:     owner.OwnerData,
	}, nil
}

// hasOutstandingLocksLocked reports whether any byte-range lock derived from
// openState is still held in the lock manager. Not the same question as whether
// openState has lock stateids: one stays valid after LOCKU frees its last range
// (RFC 7530 Section 9.1.4.4), so that count never drops back to zero.
//
// Caller must hold sm.mu.
func (sm *StateManager) hasOutstandingLocksLocked(openState *OpenState) bool {
	for _, lockState := range openState.LockStates {
		if lockState.LockOwner == nil {
			continue
		}
		lm := sm.lockManagerFor(lockState.FileHandle)
		if lm == nil {
			continue
		}
		ownerID := lockState.LockOwner.LockManagerOwnerID()
		for _, l := range lm.ListUnifiedLocks(string(lockState.FileHandle)) {
			if l.Owner.OwnerID == ownerID {
				return true
			}
		}
	}
	return false
}

// removeOwnerLocksLocked releases every byte-range lock that lockState's owner
// holds on lockState's file.
//
// Caller must hold sm.mu.
func (sm *StateManager) removeOwnerLocksLocked(lockState *LockState) {
	lm := sm.lockManagerFor(lockState.FileHandle)
	if lm == nil || lockState.LockOwner == nil {
		return
	}
	ownerID := lockState.LockOwner.LockManagerOwnerID()
	handleKey := string(lockState.FileHandle)
	for _, l := range lm.ListUnifiedLocks(handleKey) {
		if l.Owner.OwnerID == ownerID {
			_ = lm.RemoveUnifiedLock(handleKey, l.Owner, l.Offset, l.Length)
		}
	}
}

// dropLockOwnerIfUnreferencedLocked removes a lock-owner from the owner table
// once no lock state references it any more. A lock-owner is shared across the
// opens it locked (one lock state per open-state/file), so dropping it with the
// first of them would blind replay detection for the ones still live.
//
// Caller must hold sm.mu.
func (sm *StateManager) dropLockOwnerIfUnreferencedLocked(lockOwner *LockOwner) {
	if lockOwner == nil {
		return
	}
	for _, ls := range sm.lockStateByOther {
		if ls.LockOwner == lockOwner {
			return
		}
	}
	delete(sm.lockOwners, lockOwner.Key())
}

// DowngradeOpen implements the OPEN_DOWNGRADE operation's state management.
//
// Per RFC 7530 Section 16.19:
//   - Validates the stateid
//   - Validates the seqid on the owner
//   - Verifies new access <= existing (can only remove bits, not add)
//   - Updates ShareAccess and ShareDeny
//   - Increments the stateid seqid
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) DowngradeOpen(stateid *types.Stateid4, seqid uint32, newShareAccess, newShareDeny uint32, callerClientID uint64) (result *OpenSeqResult, err error) {
	// A special stateid names no state at all, and RFC 7530 Section 9.1.4.3
	// admits one only on READ, WRITE and SETATTR. stateidMissError documents
	// why it must be rejected before a table miss is classified.
	if stateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, sm.stateidMissError(stateid.Other)
	}

	// A stateid is not a bearer token: reject one that names another client's
	// state before acting on it; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return nil, err
	}

	owner := openState.Owner

	// The request is attributable to this owner, so any failure below consumes
	// its seqid (RFC 7530 Section 9.1.7), the NFS4ERR_INVAL rejections included.
	defer func() { owner.consumeSeqidOnError(seqid, err) }()

	// Validate seqid on the owner
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if seqid != 0 {
		validation := owner.ValidateSeqID(seqid)
		switch validation {
		case SeqIDReplay:
			// Replay the exact cached reply (original stateid bytes), not the
			// current stateid which a later DOWNGRADE may have advanced.
			if owner.LastResult != nil {
				return nil, &ReplayError{Status: owner.LastResult.Status, Data: owner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// The stateid must name this open's current seqid, compared after the owner
	// seqid above so a retransmit still replays; see checkStateidSeqid.
	if err := checkStateidSeqid(stateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// The new share_access must be a mode some OPEN behind this state actually
	// asked for, which is stricter than being a subset of the accumulated
	// union: one OPEN for BOTH leaves READ and WRITE standing in that union
	// with neither ever opened, and downgrading to one of them would name a
	// mode the client never held (RFC 7530 Section 16.19.4).
	if openState.openedAccessModes&shareModeBit(newShareAccess) == 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: "OPEN_DOWNGRADE to a share_access mode no OPEN asked for",
		}
	}
	if newShareDeny & ^openState.ShareDeny != 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: "OPEN_DOWNGRADE cannot add share_deny bits",
		}
	}

	// newShareAccess must be non-zero (must have at least read or write)
	if newShareAccess == 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: "OPEN_DOWNGRADE share_access cannot be zero",
		}
	}

	// Update share modes
	openState.ShareAccess = newShareAccess
	openState.ShareDeny = newShareDeny

	// Forget the opened modes this downgrade drops: a later OPEN_DOWNGRADE may
	// only name one this one kept. Downgrading to a single mode leaves that
	// mode as the only one opened; BOTH still covers all three, so it drops
	// nothing.
	if newShareAccess != types.OPEN4_SHARE_ACCESS_BOTH {
		openState.openedAccessModes = shareModeBit(newShareAccess)
	}

	// Increment stateid seqid
	openState.Stateid.Seqid = nextSeqID(openState.Stateid.Seqid)

	// Update owner seqid
	owner.LastSeqID = seqid

	resultStateid := openState.Stateid

	logger.Debug("DowngradeOpen: share modes updated",
		"client_id", owner.ClientID,
		"new_access", newShareAccess,
		"new_deny", newShareDeny,
		"stateid_seqid", resultStateid.Seqid)

	return &OpenSeqResult{
		Stateid:       resultStateid,
		OwnerClientID: owner.ClientID,
		OwnerData:     owner.OwnerData,
	}, nil
}

// GetOpenState returns the OpenState for a given stateid "other" field,
// or nil if not found. Used for read-only lookups that don't need validation.
func (sm *StateManager) GetOpenState(other [types.NFS4_OTHER_SIZE]byte) *OpenState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.openStateByOther[other]
}

// ============================================================================
// Lease Operations (, Task 4)
// ============================================================================

// renewPrincipalAllowed reports whether principal may renew record's lease.
//
// RFC 7530 Section 16.28.5 names exactly two permitted callers: the principal
// that established the client ID via SETCLIENTID_CONFIRM, and any principal
// that currently has an OPEN file on the server under that client ID. A RENEW
// from anyone else MUST be rejected with NFS4ERR_ACCESS.
//
// This does not put the lease out of a stranger's reach, and is not meant to.
// Section 9.5 has DELEGPURGE, LOCK, LOCKT, OPEN and RELEASE_LOCKOWNER renew
// every lease of the client whose clientid they carry, with no principal
// restriction -- ValidateAndRenewClient does that on the OPEN path. The
// restriction is scoped to the one operation whose only effect is the renewal.
//
// The second caller is what keeps a multi-user mount working: one client ID
// covers every user on the client, and the RFC expects a lease held on behalf
// of all of them to be renewable by any of them.
//
// One caller is admitted that the RFC does not name: a record with no
// principal recorded. SETCLIENTID under AUTH_NONE stores no identity, and there
// is nothing to compare a later RENEW against.
//
// Root gets no exemption. It would have to be an exemption for the string
// "uid:0" rather than for a verified machine credential, because Principal()
// renders a GSS-resolved uid and a client-asserted AUTH_SYS uid identically and
// RENEW carries no filehandle for an export's sec= policy to judge -- so it
// would hand any AUTH_SYS peer claiming uid 0 the lease of a client established
// under Kerberos. Nothing needs it: the Linux client renews under the same
// machine credential it established the client ID with, so the equality above
// already matches, and a client that falls back to a state owner's credential
// is covered by the open-holder scan below.
//
// Caller must hold sm.mu.
func renewPrincipalAllowed(record *ClientRecord, principal string) bool {
	if record.Principal == "" || principal == record.Principal {
		return true
	}
	for _, owner := range record.OpenOwners {
		if owner.Principal == principal && len(owner.OpenStates) > 0 {
			return true
		}
	}
	return false
}

// RenewLease implements the RENEW operation's state management.
//
// Per RFC 7530 Section 16.28:
//   - Validates the client ID exists and is confirmed
//   - Validates the caller may renew this client's lease (Section 16.28.5)
//   - Resets the lease timer and updates LastRenewal
//   - Returns ErrStaleClientID if client is unknown or unconfirmed
//   - Returns NFS4ERR_EXPIRED if the lease has already expired
//   - Returns NFS4ERR_ACCESS if the caller is not one of the principals
//     Section 16.28.5 permits
//
// The trailing principal is variadic so callers and tests that do not thread an
// auth principal keep compiling; production passes ctx.Principal().
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) RenewLease(clientID uint64, principal ...string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// v4.0 only: RENEW does not exist in v4.1, where SEQUENCE renews the lease.
	// A v4.1 record must be unreachable through this renewal: it would stamp a
	// lease the SEQUENCE handler owns.
	record := sm.v40ClientLocked(clientID)
	if record == nil {
		return sm.unknownClientIDError(clientID)
	}

	// Checked before the renewal below: a refused RENEW must leave the lease
	// exactly where it was, or the rejection still hands the caller the effect
	// it asked for.
	if !renewPrincipalAllowed(record, firstOrEmpty(principal)) {
		return ErrRenewAccess
	}

	if err := renewConfirmedClient(record); err != nil {
		return err
	}

	logger.Debug("RenewLease: lease renewed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString)

	return nil
}

// ============================================================================
// Lock Manager Integration
// ============================================================================

// SetLockManager sets the static unified lock manager for byte-range conflict
// detection. Init-only: must be called during construction, before any goroutine
// serves requests, so lock-free reads in lockManagerFor are safe. Primarily used
// by tests; production uses SetLockManagerResolver.
func (sm *StateManager) SetLockManager(lm lock.LockManager) {
	sm.lockManager = lm
}

// SetLockManagerResolver injects a function that resolves the per-share unified
// lock manager for a file handle. Called by the NFS adapter during construction.
//
// Lock managers are per-share, but the NFSv4 StateManager is global. The
// resolver lets each lock operation reach the same manager instance that SMB
// and NLM use for the same file, which is what enables cross-protocol byte-range
// lock conflict detection.
//
// Init-only: must be called before any goroutine serves requests. It is set once
// at startup and never reassigned, so lockManagerFor reads it without a lock. Do
// not call this after the adapter begins serving — there is no synchronization
// against concurrent readers.
func (sm *StateManager) SetLockManagerResolver(resolver func(handle []byte) lock.LockManager) {
	sm.lockManagerResolver = resolver
}

// lockManagerFor resolves the unified lock manager for a file handle. The
// per-handle resolver (production: per-share managers) takes precedence; the
// statically-set lockManager is the fallback (used by tests). May return nil.
//
// Lock-free by design: the resolver and lockManager are init-only (set once
// before any request is served — see SetLockManagerResolver), so reads need no
// synchronization. This also lets callers that already hold sm.mu use it without
// deadlocking.
func (sm *StateManager) lockManagerFor(handle []byte) lock.LockManager {
	if sm.lockManagerResolver != nil {
		return sm.lockManagerResolver(handle)
	}
	return sm.lockManager
}

// SetDelegationsEnabled controls whether delegations can be granted.
// When false, ShouldGrantDelegation always returns OPEN_DELEGATE_NONE.
// This is updated from live NFS adapter settings.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) SetDelegationsEnabled(enabled bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.delegationsEnabled = enabled
}

// SetLeaseTime updates the lease duration used for new client leases.
// Existing leases are not affected (grandfathered).
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) SetLeaseTime(d time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if d > 0 {
		sm.leaseDuration = d
	}
}

// SetGracePeriodDuration updates the grace period duration used for future grace periods.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) SetGracePeriodDuration(d time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if d > 0 {
		sm.graceDuration = d
	}
}

// LockNew implements the LOCK operation for a new lock-owner.
//
// This is the "open_to_lock_owner4" path where the client provides an open stateid
// and creates a new lock-owner and lock stateid.
//
// Per RFC 7530 Section 16.10:
//  1. Validate the open stateid and open-owner seqid
//  2. Validate open mode compatibility with lock type
//  3. Find or create the lock-owner
//  4. Find or create the lock state (one per lock-owner + open-state pair)
//  5. Acquire the lock via the unified lock manager
//  6. Update state on success
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) LockNew(
	ctx context.Context,
	lockClientID uint64, lockOwnerData []byte, lockSeqid uint32,
	openStateid *types.Stateid4, openSeqid uint32,
	fileHandle []byte, lockType uint32, offset, length uint64, reclaim bool,
	callerClientID uint64,
) (result *LockResult, err error) {
	// Grace period check (before acquiring sm.mu)
	if !reclaim {
		if err := sm.CheckGraceForNewState(); err != nil {
			return nil, err
		}
	}

	// A special stateid names no state at all, and RFC 7530 Section 9.1.4.3
	// admits one only on READ, WRITE and SETATTR. stateidMissError documents
	// why it must be rejected before a table miss is classified.
	if openStateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Validate open stateid
	openState, exists := sm.openStateByOther[openStateid.Other]
	if !exists {
		return nil, sm.stateidMissError(openStateid.Other)
	}

	// A stateid is not a bearer token: reject one that names another client's
	// state before acting on it; see checkStateidOwner.
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
		return nil, err
	}

	// The request is attributable to this open-owner, so any failure below
	// consumes its seqid (RFC 7530 Section 9.1.7). The lock-owner's own seqid is
	// dealt with once that owner exists, further down.
	defer func() { openState.Owner.consumeSeqidOnError(openSeqid, err) }()

	// 2. Validate open-owner seqid.
	// In the open_to_lock_owner4 (LockNew) path the LOCK advances BOTH the
	// open-owner and lock-owner seqids, so a retransmit is a replay against
	// both. The authoritative cached reply lives on the lock-owner (it is the
	// LOCK reply), so on an open-owner replay we do not fail here; we resolve
	// the lock-owner below (step 4) and return its cached result.
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	openSeqIsReplay := false
	if openSeqid != 0 {
		validation := openState.Owner.ValidateSeqID(openSeqid)
		switch validation {
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDReplay:
			openSeqIsReplay = true
		case SeqIDOK:
			// Continue
		}
	}

	// 3. Probe the lock-owner WITHOUT allocating. Seqid validation (step 4)
	// must run before any state is inserted into the maps; otherwise a bad
	// lock seqid would strand a freshly-allocated lock-owner / lock-state
	// (they would be reused by a later valid LOCK with a wrong LastSeqID=0
	// baseline).
	loKey := makeLockOwnerKey(lockClientID, lockOwnerData)
	lockOwner, ownerExists := sm.lockOwners[loKey]

	// 4. Validate lock seqid on lock-owner BEFORE any allocation.
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if lockSeqid != 0 {
		if ownerExists {
			lockValidation := lockOwner.ValidateSeqID(lockSeqid)
			switch lockValidation {
			case SeqIDBad:
				return nil, ErrBadSeqid
			case SeqIDReplay:
				// Replay the exact cached LOCK reply. Returning NFS4ERR_BAD_SEQID
				// here (the previous behavior) is treated as fatal by the Linux
				// client and drops the lock-owner -> silent lock loss.
				if lockOwner.LastResult != nil {
					return nil, &ReplayError{Status: lockOwner.LastResult.Status, Data: lockOwner.LastResult.Data}
				}
				return nil, ErrBadSeqid
			case SeqIDOK:
				// A fresh lock seqid alongside a replayed open seqid is an
				// inconsistent retransmit; treat as bad seqid.
				if openSeqIsReplay {
					return nil, ErrBadSeqid
				}
			}
		} else {
			// Brand-new lock-owner: the only valid lock seqid is nextSeqID(0)
			// (== 1) per RFC 7530 Section 9.1.4. Reject anything else before
			// allocating, so a bad seqid leaves no orphaned state behind.
			if lockSeqid != nextSeqID(0) {
				return nil, ErrBadSeqid
			}
		}
	} else if openSeqIsReplay {
		// Open seqid replayed but the lock-owner is brand new / not yet
		// seqid-tracked: nothing consistent to replay.
		return nil, ErrBadSeqid
	}

	// The open stateid must name that open's current seqid, compared after both
	// seqid checks above so a retransmit still replays; see checkStateidSeqid.
	if err := checkStateidSeqid(openStateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// 5. Find or create lock-owner -- only after seqid validation passes.
	if !ownerExists {
		clientRecord := sm.clientRecordLocked(lockClientID)
		lockOwner = &LockOwner{
			ClientID:  lockClientID,
			OwnerData: make([]byte, len(lockOwnerData)),

			ClientRecord: clientRecord,
			key:          loKey,
		}
		copy(lockOwner.OwnerData, lockOwnerData)
		sm.lockOwners[loKey] = lockOwner
	}

	// The lock-owner now exists, so its seqid can be consumed like the
	// open-owner's above. Nothing is lost by starting here: every failure before
	// this point is a seqid verdict, and those are exempt anyway.
	defer func() { lockOwner.consumeSeqidOnError(lockSeqid, err) }()

	// 6. Validate the byte range and the open mode for the lock type. Both run
	// after the seqid checks so a bad seqid, which must leave the sequence
	// untouched, outranks NFS4ERR_INVAL and NFS4ERR_OPENMODE, which consume it.
	length, err = normalizeLockRange(offset, length)
	if err != nil {
		return nil, err
	}
	if err := validateOpenModeForLock(openState, lockType); err != nil {
		return nil, err
	}

	// 7. Find or create lock state for (lock-owner, open-state) pair
	var lockState *LockState
	for _, ls := range openState.LockStates {
		if ls.LockOwner == lockOwner {
			lockState = ls
			break
		}
	}

	if lockState == nil {
		// Create new lock state
		other := sm.generateStateidOther(StateTypeLock)
		lockState = &LockState{
			Stateid: types.Stateid4{
				Seqid: 1,
				Other: other,
			},
			LockOwner:  lockOwner,
			OpenState:  openState,
			FileHandle: make([]byte, len(fileHandle)),
		}
		copy(lockState.FileHandle, fileHandle)

		// Register in maps
		openState.LockStates = append(openState.LockStates, lockState)
		sm.lockStateByOther[other] = lockState
	}

	// 8. Acquire the lock via unified lock manager
	denied, err := sm.acquireLock(ctx, lockState, lockType, offset, length, reclaim, callerClientID)
	if err != nil {
		return nil, err
	}
	if denied != nil {
		// A DENIED LOCK still advances the lock-owner seqid, so the encoded
		// LOCK4denied reply must also be cached for replay.
		lockOwner.LastSeqID = lockSeqid
		openState.Owner.LastSeqID = openSeqid
		return &LockResult{
			Denied:        denied,
			OwnerClientID: lockOwner.ClientID,
			OwnerData:     lockOwner.OwnerData,
		}, nil
	}

	// 9. Success: update state
	lockState.Stateid.Seqid = nextSeqID(lockState.Stateid.Seqid)
	lockOwner.LastSeqID = lockSeqid
	openState.Owner.LastSeqID = openSeqid

	return &LockResult{
		Stateid:       lockState.Stateid,
		OwnerClientID: lockOwner.ClientID,
		OwnerData:     lockOwner.OwnerData,
	}, nil
}

// LockExisting implements the LOCK operation for an existing lock-owner.
//
// This is the "exist_lock_owner4" path where the client provides an existing
// lock stateid to acquire additional locks.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) LockExisting(
	ctx context.Context,
	lockStateid *types.Stateid4, lockSeqid uint32,
	fileHandle []byte, lockType uint32, offset, length uint64, reclaim bool,
	callerClientID uint64,
) (result *LockResult, err error) {
	// Grace period check
	if !reclaim {
		if err := sm.CheckGraceForNewState(); err != nil {
			return nil, err
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Look up lock state. The same check runs again on recommit, once the
	// lock manager has been called with sm.mu released; here it rejects a
	// stateid that is not the caller's before any cross-protocol work happens.
	lockState, exists := sm.lockStateByOther[lockStateid.Other]
	if !exists {
		return nil, sm.stateidMissError(lockStateid.Other)
	}
	if err := sm.revalidateLockStateLocked(lockState, callerClientID); err != nil {
		return nil, err
	}

	lockOwner := lockState.LockOwner

	// The request is attributable to this lock-owner, so any failure below
	// consumes its seqid (RFC 7530 Section 9.1.7), the stateid and open-mode
	// rejections included.
	defer func() { lockOwner.consumeSeqidOnError(lockSeqid, err) }()

	// 2. Validate lock seqid on lock-owner FIRST.
	// A LOCK replay resends the original (pre-LOCK) lock stateid, whose seqid is
	// now one behind lockState.Stateid.Seqid; the strict stateid-seqid check
	// below would otherwise reject it as NFS4ERR_OLD_STATEID before the replay
	// is detected (RFC 7530 §9.1.7).
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if lockSeqid != 0 {
		lockValidation := lockOwner.ValidateSeqID(lockSeqid)
		switch lockValidation {
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDReplay:
			// Replay the exact cached LOCK reply (see LockNew step 6).
			if lockOwner.LastResult != nil {
				return nil, &ReplayError{Status: lockOwner.LastResult.Status, Data: lockOwner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// 3. Validate stateid seqid (only for non-replay LOCK).
	if err := checkStateidSeqid(lockStateid.Seqid, lockState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// 4. Validate the byte range and the open mode for the lock type
	length, err = normalizeLockRange(offset, length)
	if err != nil {
		return nil, err
	}
	if err := validateOpenModeForLock(lockState.OpenState, lockType); err != nil {
		return nil, err
	}

	// 5. Acquire the lock
	denied, err := sm.acquireLock(ctx, lockState, lockType, offset, length, reclaim, callerClientID)
	if err != nil {
		return nil, err
	}
	if denied != nil {
		// A DENIED LOCK still advances the lock-owner seqid; cache its reply.
		lockOwner.LastSeqID = lockSeqid
		return &LockResult{
			Denied:        denied,
			OwnerClientID: lockOwner.ClientID,
			OwnerData:     lockOwner.OwnerData,
		}, nil
	}

	// 6. Success: update state
	lockState.Stateid.Seqid = nextSeqID(lockState.Stateid.Seqid)
	lockOwner.LastSeqID = lockSeqid

	return &LockResult{
		Stateid:       lockState.Stateid,
		OwnerClientID: lockOwner.ClientID,
		OwnerData:     lockOwner.OwnerData,
	}, nil
}

// acquireLock attempts to acquire a byte-range lock via the unified lock manager.
//
// Returns (nil, nil) on success, (*LOCK4denied, nil) on conflict,
// or (nil, error) on internal errors.
//
// Caller must hold sm.mu. The mutex is RELEASED for the duration of the lock
// manager call and held again on return: sm.mu serializes every client's state
// operation server-wide, while the manager is cross-protocol and its acquire
// path waits for an in-flight lease break in another protocol to drain, for as
// long as lock.WaitForByteRangeLockBreak's timeout allows. Holding sm.mu across
// that stalls every other client's SEQUENCE, OPEN, CLOSE and RENEW behind one
// client's LOCK.
//
// The state the caller resolved before that gap is re-validated on return, so a
// caller may treat a nil error as "still safe to commit against lockState".
func (sm *StateManager) acquireLock(ctx context.Context, lockState *LockState, lockType uint32, offset, length uint64, reclaim bool, callerClientID uint64) (*LOCK4denied, error) {
	lm := sm.lockManagerFor(lockState.FileHandle)
	if lm == nil {
		return nil, fmt.Errorf("no lock manager configured")
	}

	// Build the protocol-agnostic lock owner
	owner := lock.LockOwner{
		OwnerID:   lockState.LockOwner.LockManagerOwnerID(),
		ClientID:  nfsClientIdentity(lockState.LockOwner.ClientID),
		ShareName: "",
	}

	// Map lock type to shared/exclusive
	var mappedType lock.LockType
	switch lockType {
	case types.READ_LT, types.READW_LT:
		mappedType = lock.LockTypeShared
	case types.WRITE_LT, types.WRITEW_LT:
		mappedType = lock.LockTypeExclusive
	default:
		mappedType = lock.LockTypeExclusive
	}

	// Create enhanced lock
	enhLock := lock.NewUnifiedLock(owner, lock.FileHandle(lockState.FileHandle), offset, length, mappedType)
	enhLock.Reclaim = reclaim

	handleKey := string(lockState.FileHandle)

	// A lock held on behalf of a client whose lease has already run out is
	// courtesy state, and this request is the collision that ends the courtesy.
	// Released here rather than left to the sweeper, the answer no longer
	// depends on where in the sweep interval the request happened to land.
	//
	// A reclaim is exempt, as CLAIM_PREVIOUS is on the OPEN side: during grace
	// every client is re-establishing state it already held, and one that has
	// reclaimed its opens but not yet its locks can outlive the fresh lease it
	// was given, which would make its half-rebuilt state look abandoned.
	if !reclaim {
		sm.expireLapsedHoldersLocked(lockState.FileHandle,
			lockState.LockOwner.ClientID, lockState.OpenState.Owner.ClientID)
	}

	sm.mu.Unlock()
	denied, err := acquireUnifiedLock(ctx, lm, handleKey, enhLock, lockType)
	sm.mu.Lock()

	if err != nil {
		return nil, err
	}

	// While sm.mu was released the resolved state may have been freed under it —
	// by a CLOSE, a RELEASE_LOCKOWNER, or the lease sweeper expiring the client.
	// Committing a seqid bump onto freed state would hand the client a stateid
	// the server no longer knows, and would strand the byte-range lock just
	// inserted with no NFSv4 state left to ever release it. Give the lock back
	// and fail the operation instead.
	if staleErr := sm.revalidateLockStateLocked(lockState, callerClientID); staleErr != nil {
		if denied == nil {
			sm.mu.Unlock()
			_ = lm.RemoveUnifiedLock(handleKey, owner, offset, length)
			sm.mu.Lock()
		}
		return nil, staleErr
	}

	return denied, nil
}

// revalidateLockStateLocked reports whether the lock state and its lock-owner
// are still the records the StateManager's maps point at, and whether the
// lock-owner belongs to the calling client. It is both the admission check for
// a lock stateid arriving from the wire and the recommit check for a path that
// resolves state under sm.mu, releases the mutex for an external call, and then
// writes a result back — the same three facts have to hold at both points.
//
// The comparison is by pointer identity, not by presence: a stateid "other" and
// a lock-owner key can both be handed out again once the original records are
// freed, so "something exists under this key" does not mean "the record
// resolved earlier is still live".
//
// The parent open state needs no separate check. Every path that frees an open
// state frees its lock stateids in the same critical section — CLOSE at
// openStateByOther, and the lease sweeper via releaseClientStateLocked — so a
// live lock state implies a live open.
//
// Caller must hold sm.mu.
func (sm *StateManager) revalidateLockStateLocked(lockState *LockState, callerClientID uint64) error {
	if sm.lockStateByOther[lockState.Stateid.Other] != lockState {
		return sm.stateidMissError(lockState.Stateid.Other)
	}
	lockOwner := lockState.LockOwner
	if lockOwner == nil || sm.lockOwners[lockOwner.Key()] != lockOwner {
		return ErrBadStateid
	}

	// A stateid is not a bearer token: a client presenting another client's lock
	// stateid could otherwise unlock, or lock inside, a byte range it has no
	// state on; see checkStateidOwner.
	return checkStateidOwner(callerClientID, lockOwner.ClientID)
}

// acquireUnifiedLock performs the cross-protocol half of a byte-range lock
// acquire: break conflicting leases, drain the break, insert the lock, and on
// refusal describe the conflicting holder. lockType is the requested NFS4 lock
// type, needed only to describe an unidentifiable conflict.
//
// It touches no StateManager state, so it runs without sm.mu.
func acquireUnifiedLock(
	ctx context.Context,
	lm lock.LockManager,
	handleKey string,
	enhLock *lock.UnifiedLock,
	lockType uint32,
) (*LOCK4denied, error) {
	// Break any conflicting cross-protocol read leases (e.g. an SMB read/write
	// oplock) before acquiring the byte-range lock. A held lease lets another
	// protocol cache the bytes this lock is about to protect, so it must be
	// revoked first — exactly as the SMB LOCK handler does before its own
	// acquire (see internal/adapter/smb/handlers/lock.go). Without this the
	// lease is recorded as a conflict and the NFS lock is denied even though no
	// real byte-range lock is held. We pass this lock's owner as excludeOwner for
	// symmetry with the SMB path; NFS owners never hold SMB leases, so in practice
	// nothing is excluded.
	_ = lm.BreakLeasesForByteRangeLock(handleKey, &enhLock.Owner)

	// Drain the in-flight lease break before inserting the lock: the break is
	// fire-and-forget, so a still-present (Breaking, not-yet-ACKed) write lease is
	// otherwise observed as a spurious DENIED → client EIO. See
	// lock.WaitForByteRangeLockBreak for the deadlock-safety and timeout reasoning.
	// A non-nil error means the originating request was cancelled, so don't insert
	// a lock nobody is waiting for.
	if err := lock.WaitForByteRangeLockBreak(ctx, lm, handleKey); err != nil {
		return nil, err
	}

	// Try to add the lock
	err := lm.AddUnifiedLock(handleKey, enhLock)
	if err != nil {
		// Lock conflict: query existing locks to find the conflicting one
		// for the LOCK4denied response
		existingLocks := lm.ListUnifiedLocks(handleKey)
		for _, el := range existingLocks {
			if lock.IsUnifiedLockConflicting(el, enhLock) {
				// Map the conflicting lock type back to NFS4
				var conflictType uint32
				if el.Type == lock.LockTypeExclusive {
					conflictType = types.WRITE_LT
				} else {
					conflictType = types.READ_LT
				}

				denied := &LOCK4denied{
					Offset:   el.Offset,
					Length:   el.Length,
					LockType: conflictType,
				}
				// Parse OwnerID to extract clientID and ownerData.
				// Format: "nfs4:{clientid}:{owner_hex}". Use the shared helper
				// so OwnerData is the decoded opaque bytes, not the raw format
				// string (LOCKT uses the same path).
				denied.Owner.ClientID = 0    // default; parseConflictOwner overwrites on success
				denied.Owner.OwnerData = nil // default
				parseConflictOwner(el.Owner.OwnerID, denied)
				return denied, nil
			}
		}

		// Conflict exists but we couldn't identify the exact lock (shouldn't happen)
		denied := &LOCK4denied{
			Offset:   enhLock.Offset,
			Length:   enhLock.Length,
			LockType: lockType,
		}
		return denied, nil
	}

	return nil, nil
}

// ============================================================================
// LOCKT - Lock Test
// ============================================================================

// TestLock tests for byte-range lock conflicts without creating any state.
//
// IMPORTANT: LOCKT does NOT create lock-owners, lock stateids, or any other state.
// It only queries the lock manager for existing conflicting locks.
//
// Per RFC 7530 Section 16.11:
//   - Returns nil (no conflict) if the lock could be acquired
//   - Returns *LOCK4denied with conflict details if a conflicting lock exists
//   - Uses clientID + ownerData to build the owner ID for comparison
//
// The lockType parameter maps NFS4 lock types to shared/exclusive.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) TestLock(
	clientID uint64, ownerData []byte,
	fileHandle []byte, lockType uint32, offset, length uint64,
) (*LOCK4denied, error) {
	length, err := normalizeLockRange(offset, length)
	if err != nil {
		return nil, err
	}

	lm := sm.lockManagerFor(fileHandle)
	if lm == nil {
		// No lock manager = no locks possible = no conflicts
		return nil, nil
	}

	// Build the owner ID string using the same format as acquireLock
	ownerID := lockManagerOwnerID(clientID, ownerData)

	// Map lock type to shared/exclusive
	var mappedType lock.LockType
	switch lockType {
	case types.READ_LT, types.READW_LT:
		mappedType = lock.LockTypeShared
	case types.WRITE_LT, types.WRITEW_LT:
		mappedType = lock.LockTypeExclusive
	default:
		mappedType = lock.LockTypeExclusive
	}

	// Create a temporary test lock (not added to the manager). It carries the
	// same client identity an actual LOCK would, so LOCKT reports the client's
	// own delegation as free rather than as a conflict it would never hit.
	testLock := &lock.UnifiedLock{
		Owner:  lock.LockOwner{OwnerID: ownerID, ClientID: nfsClientIdentity(clientID)},
		Offset: offset,
		Length: length,
		Type:   mappedType,
	}

	// TestUnifiedLock cross-checks the SMB byte-range map (lm.locks) too, so
	// NFSv4 LOCKT reports the same conflict an actual LOCK would be denied by
	// when an overlapping SMB byte-range lock exists (xproto H1).
	handleKey := string(fileHandle)
	conflict := lm.TestUnifiedLock(handleKey, testLock)
	if conflict != nil {
		el := conflict.Lock
		// Build LOCK4denied from the conflicting lock
		var conflictType uint32
		if el.Type == lock.LockTypeExclusive {
			conflictType = types.WRITE_LT
		} else {
			conflictType = types.READ_LT
		}

		denied := &LOCK4denied{
			Offset:   el.Offset,
			Length:   el.Length,
			LockType: conflictType,
		}

		// Parse OwnerID to extract clientID and ownerData
		// Format: "nfs4:{clientid}:{owner_hex}"
		denied.Owner.ClientID = 0    // Default
		denied.Owner.OwnerData = nil // Default
		parseConflictOwner(el.Owner.OwnerID, denied)

		return denied, nil
	}

	return nil, nil
}

// ============================================================================
// LOCKU - Unlock File
// ============================================================================

// UnlockFile releases a byte-range lock via the lock manager using POSIX split semantics.
//
// Per RFC 7530 Section 16.12:
//   - Validates the lock stateid and seqid
//   - Calls the lock manager's RemoveUnifiedLock for POSIX splitting
//   - Increments the lock stateid seqid on success
//   - The lock state is NOT removed (persists for future LOCK operations)
//   - RELEASE_LOCKOWNER handles state cleanup
//
// Lock-not-found from the lock manager is treated as success (idempotent unlock).
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) UnlockFile(
	lockStateid *types.Stateid4, seqid uint32,
	lockType uint32, offset, length uint64,
	callerClientID uint64,
) (result *LockResult, err error) {
	// Special stateids cannot be used with LOCKU
	if lockStateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Look up lock state. As in LockExisting, the same check runs again on
	// recommit; here it rejects a stateid that is not the caller's before the
	// lock manager is touched.
	lockState, exists := sm.lockStateByOther[lockStateid.Other]
	if !exists {
		return nil, sm.stateidMissError(lockStateid.Other)
	}
	if err := sm.revalidateLockStateLocked(lockState, callerClientID); err != nil {
		return nil, err
	}

	lockOwner := lockState.LockOwner

	// The request is attributable to this lock-owner, so any failure below
	// consumes its seqid (RFC 7530 Section 9.1.7), the stateid rejections
	// included.
	defer func() { lockOwner.consumeSeqidOnError(seqid, err) }()

	// 2. Validate seqid on lock-owner FIRST.
	// A LOCKU replay resends the original (pre-LOCKU) lock stateid, whose seqid
	// is now one behind lockState.Stateid.Seqid. The strict stateid-seqid check
	// below would reject that as NFS4ERR_OLD_STATEID, so the lock-owner replay
	// must be detected before it (RFC 7530 §9.1.7: exactly-once wins).
	// seqid=0 is the v4.1 bypass convention: slot table provides replay protection
	if seqid != 0 {
		validation := lockOwner.ValidateSeqID(seqid)
		switch validation {
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDReplay:
			// Replay the exact cached LOCKU reply (the original stateid bytes),
			// not the current stateid which a later LOCKU may have advanced.
			if lockOwner.LastResult != nil {
				return nil, &ReplayError{Status: lockOwner.LastResult.Status, Data: lockOwner.LastResult.Data}
			}
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// 3. Validate stateid seqid (only for non-replay LOCKU).
	if err := checkStateidSeqid(lockStateid.Seqid, lockState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// 4. Validate the byte range
	length, err = normalizeLockRange(offset, length)
	if err != nil {
		return nil, err
	}

	// 5. Release the lock via the unified lock manager, with sm.mu released for
	// the call: the manager is cross-protocol and sm.mu serializes every
	// client's state operation server-wide (see acquireLock).
	if lm := sm.lockManagerFor(lockState.FileHandle); lm != nil {
		owner := lock.LockOwner{
			OwnerID:   lockOwner.LockManagerOwnerID(),
			ClientID:  nfsClientIdentity(lockOwner.ClientID),
			ShareName: "",
		}

		handleKey := string(lockState.FileHandle)
		sm.mu.Unlock()
		rmErr := lm.RemoveUnifiedLock(handleKey, owner, offset, length)
		sm.mu.Lock()
		if rmErr != nil {
			// Lock-not-found is OK for LOCKU (idempotent).
			// Only fail on unexpected errors.
			// RemoveUnifiedLock returns StoreError with ErrLockNotFound code.
			// We treat all errors as non-fatal for idempotency.
			logger.Debug("LOCKU: lock manager RemoveUnifiedLock returned error (idempotent OK)",
				"error", rmErr,
				"handle", handleKey,
				"offset", offset,
				"length", length)
		}

		// The state resolved above may have been freed while sm.mu was
		// released. Removing the lock was the direction LOCKU was heading
		// anyway, so it stands; the seqid bump below must not be committed onto
		// state the server has since forgotten.
		if staleErr := sm.revalidateLockStateLocked(lockState, callerClientID); staleErr != nil {
			return nil, staleErr
		}
	}

	// 6. Success: increment lock stateid seqid
	lockState.Stateid.Seqid = nextSeqID(lockState.Stateid.Seqid)
	lockOwner.LastSeqID = seqid

	return &LockResult{
		Stateid:       lockState.Stateid,
		OwnerClientID: lockOwner.ClientID,
		OwnerData:     lockOwner.OwnerData,
	}, nil
}

// ============================================================================
// RELEASE_LOCKOWNER
// ============================================================================

// ReleaseLockOwner releases all state associated with a lock-owner.
//
// Per RFC 7530 Section 16.34:
//   - If the lock-owner has active locks (in the lock manager), return NFS4ERR_LOCKS_HELD.
//   - If the lock-owner has no active locks, remove all LockStates from maps and
//     remove the lock-owner from sm.lockOwners.
//   - Releasing an unknown lock-owner is a no-op (return nil / NFS4_OK).
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) ReleaseLockOwner(clientID uint64, ownerData []byte) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the lock-owner
	loKey := makeLockOwnerKey(clientID, ownerData)
	lockOwner, exists := sm.lockOwners[loKey]
	if !exists {
		// Unknown lock-owner: no-op per RFC
		return nil
	}

	// Check if this lock-owner has any active locks in the lock manager.
	// Lock managers are per-share, so resolve one per file handle.
	ownerID := lockOwner.LockManagerOwnerID()

	// Find all lock states for this lock-owner by scanning lockStateByOther
	for _, ls := range sm.lockStateByOther {
		if ls.LockOwner != lockOwner {
			continue
		}
		lm := sm.lockManagerFor(ls.FileHandle)
		if lm == nil {
			continue
		}
		// Check if any locks are held for this owner on this file
		handleKey := string(ls.FileHandle)
		locks := lm.ListUnifiedLocks(handleKey)
		for _, l := range locks {
			if l.Owner.OwnerID == ownerID {
				return &NFS4StateError{
					Status:  types.NFS4ERR_LOCKS_HELD,
					Message: "cannot release lock-owner: locks still held",
				}
			}
		}
	}

	// No active locks: clean up all state for this lock-owner.
	// Remove all LockStates from lockStateByOther and from their OpenState.LockStates slices.
	for other, ls := range sm.lockStateByOther {
		if ls.LockOwner != lockOwner {
			continue
		}
		// Remove from lockStateByOther map
		delete(sm.lockStateByOther, other)

		// Remove from the OpenState's LockStates slice
		if ls.OpenState != nil {
			for i, ols := range ls.OpenState.LockStates {
				if ols == ls {
					ls.OpenState.LockStates = append(ls.OpenState.LockStates[:i], ls.OpenState.LockStates[i+1:]...)
					break
				}
			}
		}
	}

	// Remove the lock-owner from lockOwners map
	delete(sm.lockOwners, loKey)

	logger.Debug("ReleaseLockOwner: lock-owner removed",
		"client_id", clientID,
		"owner_data", hex.EncodeToString(ownerData))

	return nil
}

// parseConflictOwner extracts clientID and ownerData from a conflicting lock's
// OwnerID string in the format "nfs4:{clientid}:{owner_hex}".
// On parse failure, the defaults (0 / raw OwnerID bytes) are used.
func parseConflictOwner(ownerID string, denied *LOCK4denied) {
	var parsedClientID uint64
	var ownerHex string

	// Try to parse "nfs4:{clientid}:{owner_hex}"
	n, _ := fmt.Sscanf(ownerID, "nfs4:%d:%s", &parsedClientID, &ownerHex)
	if n >= 1 {
		denied.Owner.ClientID = parsedClientID
	}
	if n >= 2 {
		if decoded, err := hex.DecodeString(ownerHex); err == nil {
			denied.Owner.OwnerData = decoded
			return
		}
	}
	// Fallback: use raw OwnerID as opaque data
	denied.Owner.OwnerData = []byte(ownerID)
}

// ============================================================================
// NFSv4.1 Lease and Status
// ============================================================================

// RenewV41Lease renews the lease for a v4.1 client by updating LastRenewal.
// Called by the SEQUENCE handler on every successful validation, per
// RFC 8881 Section 8.1.3 (implicit lease renewal).
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) RenewV41Lease(clientID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record := sm.v41ClientLocked(clientID)
	if record == nil {
		return
	}

	record.LastRenewal = time.Now()
	if record.Lease != nil {
		record.Lease.Renew()
	}

	logger.Debug("RenewV41Lease: v4.1 lease renewed",
		"client_id", fmt.Sprintf("0x%x", clientID))
}

// GetStatusFlags computes the SEQ4_STATUS_* bitmask for a SEQUENCE response.
//
// Per RFC 8881 Section 18.46, the server reports status flags covering:
//   - Callback path health (CB_PATH_DOWN, BACKCHANNEL_FAULT)
//   - Lease expiry (EXPIRED_ALL_STATE_REVOKED)
//   - Delegation state (RECALLABLE_STATE_REVOKED)
//
// Thread-safe: acquires sm.mu.RLock.
func (sm *StateManager) GetStatusFlags(session *Session) uint32 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var flags uint32

	// CB_PATH_DOWN: set when no back-bound connection exists for any session
	// of this client. Cleared when at least one back-bound connection is present.
	sm.connMu.RLock()
	hasBack := sm.hasBackBoundConnection(session.ClientID)
	hasFault := sm.backchannelFaults[session.ClientID]
	sm.connMu.RUnlock()

	if session.BackChannelSlots == nil || !hasBack {
		flags |= types.SEQ4_STATUS_CB_PATH_DOWN
	}

	// BACKCHANNEL_FAULT: set when a callback send actually fails.
	if hasFault {
		flags |= types.SEQ4_STATUS_BACKCHANNEL_FAULT
	}

	// Check client lease expiry
	if record := sm.v41ClientLocked(session.ClientID); record != nil && record.Lease != nil && record.Lease.IsExpired() {
		flags |= types.SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED
	}

	// Check for revoked delegations for this client via the per-client
	// revoked-delegation index (O(1)) instead of scanning every delegation.
	if sm.revokedDelegCount[session.ClientID] > 0 {
		flags |= types.SEQ4_STATUS_RECALLABLE_STATE_REVOKED
	}

	return flags
}

// ============================================================================
// NFSv4.1 Session Management
// ============================================================================

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

// ============================================================================
// Connection Binding
// ============================================================================

// BindConnToSession associates a TCP connection with a session.
//
// Per RFC 8881 Section 18.34, the server:
//   - Validates the session exists
//   - Negotiates the channel direction (generous policy)
//   - Silently unbinds the connection from a previous session if needed
//   - Enforces a per-session connection limit (NFS4ERR_RESOURCE)
//   - Ensures at least one fore-channel connection remains (NFS4ERR_INVAL)
//
// Thread-safe: acquires sm.mu.RLock then sm.connMu.Lock.
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
func (sm *StateManager) SetMaxConnectionsPerSession(max int) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	if max >= 0 {
		sm.maxConnsPerSession = max
	}
}

// SetMaxSessionSlots sets the maximum fore channel slots per session.
// Only positive values are accepted; zero or negative values are ignored.
// Values exceeding DefaultMaxSlots are clamped to prevent advertising more
// slots than NewSlotTable allocates (which would cause NFS4ERR_BADSLOT).
func (sm *StateManager) SetMaxSessionSlots(n int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if n <= 0 {
		return
	}
	if n > int(DefaultMaxSlots) {
		n = int(DefaultMaxSlots)
	}
	sm.foreMaxSlots = uint32(n)
}

// SetMaxSessionsPerClient sets the maximum number of sessions per client.
// Only positive values are accepted; zero or negative values are ignored.
func (sm *StateManager) SetMaxSessionsPerClient(n int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if n > 0 {
		sm.maxSessionsPerClient = n
	}
}

// ============================================================================
// Backchannel Operations
// ============================================================================

// RegisterConnWriter registers a ConnWriter callback for a back-bound connection.
// Called by the NFS adapter when a connection is bound for back-channel.
// Also creates a PendingCBReplies instance for the connection.
//
// Thread-safe: acquires sm.connMu.Lock.
func (sm *StateManager) RegisterConnWriter(connectionID uint64, writer ConnWriter) *PendingCBReplies {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	sm.connWriters[connectionID] = writer
	pending := NewPendingCBReplies()
	sm.cbRepliesByConn[connectionID] = pending
	return pending
}

// UnregisterConnWriter removes the ConnWriter and PendingCBReplies for a connection.
// Called on disconnect cleanup.
//
// Thread-safe: acquires sm.connMu.Lock.
func (sm *StateManager) UnregisterConnWriter(connectionID uint64) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	delete(sm.connWriters, connectionID)
	delete(sm.cbRepliesByConn, connectionID)
}

// GetPendingCBReplies returns the PendingCBReplies for a connection, or nil.
//
// Thread-safe: acquires sm.connMu.RLock.
func (sm *StateManager) GetPendingCBReplies(connectionID uint64) *PendingCBReplies {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()
	return sm.cbRepliesByConn[connectionID]
}

// StartBackchannelSender creates and starts a BackchannelSender for a session
// if the session has back-channel slots and no sender exists yet.
// Called lazily on first back-channel bind or first callback enqueue.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) StartBackchannelSender(ctx context.Context, sessionID types.SessionId4) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessionsByID[sessionID]
	if !exists || session.BackChannelSlots == nil {
		return
	}
	if session.backchannelSender != nil {
		return // Already started
	}

	sender := NewBackchannelSender(
		sessionID,
		session.ClientID,
		session.CbProgram,
		session.BackChannelSlots,
		sm,
	)
	session.backchannelSender = sender

	go sender.Run(ctx)

	// The back channel just became writable, which is the first moment a
	// CB_NULL to this client can succeed or fail for a real reason. Probing
	// here rather than in CreateSession keeps the callback round-trip off the
	// mount path, and this function is the once-per-session gate: it returned
	// above if a sender already existed.
	go sm.probeV41CallbackPath(ctx, sender)

	logger.Info("BackchannelSender started for session",
		"session_id", sessionID.String(),
		"client_id", fmt.Sprintf("0x%x", session.ClientID))
}

// stopBackchannelSender stops the BackchannelSender for a session.
// Called from destroySessionLocked to prevent orphan goroutines.
//
// Caller must hold sm.mu.
func (sm *StateManager) stopBackchannelSender(sessionID types.SessionId4) {
	session, exists := sm.sessionsByID[sessionID]
	if !exists {
		return
	}
	if session.backchannelSender != nil {
		session.backchannelSender.Stop()
		session.backchannelSender = nil
	}
}

// getBackchannelSender returns the BackchannelSender for the client's first
// session that has a backchannel. Returns nil if no v4.1 backchannel exists.
//
// Thread-safe: acquires sm.mu.RLock.
func (sm *StateManager) getBackchannelSender(clientID uint64) *BackchannelSender {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, session := range sm.sessionsByClientID[clientID] {
		if session.backchannelSender != nil {
			return session.backchannelSender
		}
	}
	return nil
}

// getBackBoundConnWriter finds a back-bound connection for the session,
// optionally excluding a specific connection ID (pass 0 for no exclusion).
// Selects the connection with the most recent fore-channel activity.
//
// Lock ordering: acquires sm.connMu.RLock only (no sm.mu needed).
func (sm *StateManager) getBackBoundConnWriter(sessionID types.SessionId4, excludeConnID uint64) (uint64, ConnWriter, *PendingCBReplies, bool) {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	return sm.getBackBoundConnWriterLocked(sessionID, excludeConnID)
}

// getBackBoundConnWriterLocked is the common implementation for finding a
// back-bound connection. Caller must hold sm.connMu.RLock.
func (sm *StateManager) getBackBoundConnWriterLocked(sessionID types.SessionId4, excludeConnID uint64) (uint64, ConnWriter, *PendingCBReplies, bool) {
	bindings := sm.connBySession[sessionID]
	var bestConn *BoundConnection
	var bestTime time.Time

	for _, b := range bindings {
		if b.ConnectionID == excludeConnID {
			continue
		}
		if b.Direction != ConnDirBack && b.Direction != ConnDirBoth {
			continue
		}
		if bestConn == nil || b.LastActivity.After(bestTime) {
			bestConn = b
			bestTime = b.LastActivity
		}
	}

	if bestConn == nil {
		return 0, nil, nil, false
	}

	writer, ok := sm.connWriters[bestConn.ConnectionID]
	if !ok {
		return 0, nil, nil, false
	}
	pending := sm.cbRepliesByConn[bestConn.ConnectionID]
	if pending == nil {
		return 0, nil, nil, false
	}

	return bestConn.ConnectionID, writer, pending, true
}

// UpdateBackchannelParams stores new callback parameters on a session.
// Called by the BACKCHANNEL_CTL handler.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) UpdateBackchannelParams(sessionID types.SessionId4, cbProgram uint32, secParms []types.CallbackSecParms4) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessionsByID[sessionID]
	if !exists {
		return ErrBadSession
	}

	session.CbProgram = cbProgram
	session.BackchannelSecParms = secParms

	// Update the sender's program number if it exists. The sender's Run
	// goroutine reads cbProgram without sm.mu, so the field is atomic.
	if session.backchannelSender != nil {
		session.backchannelSender.cbProgram.Store(cbProgram)
	}

	logger.Info("Backchannel params updated",
		"session_id", sessionID.String(),
		"cb_program", fmt.Sprintf("0x%x", cbProgram),
		"sec_parms_count", len(secParms))

	return nil
}

// setBackchannelFault sets or clears the backchannel fault flag for a client.
// Called by BackchannelSender on send failure/success.
//
// Thread-safe: acquires sm.connMu.Lock.
func (sm *StateManager) setBackchannelFault(clientID uint64, fault bool) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	if fault {
		sm.backchannelFaults[clientID] = true
	} else {
		delete(sm.backchannelFaults, clientID)
	}
}

// hasBackBoundConnection returns true if the client has at least one
// back-bound connection across any of its sessions.
//
// Caller must hold sm.mu.RLock and sm.connMu.RLock (or ensure no concurrent access).
func (sm *StateManager) hasBackBoundConnection(clientID uint64) bool {
	for _, session := range sm.sessionsByClientID[clientID] {
		for _, b := range sm.connBySession[session.SessionID] {
			if b.Direction == ConnDirBack || b.Direction == ConnDirBoth {
				return true
			}
		}
	}
	return false
}
