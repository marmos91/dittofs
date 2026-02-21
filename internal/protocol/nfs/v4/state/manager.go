package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
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

	// clientsByID maps server-assigned client IDs to client records.
	clientsByID map[uint64]*ClientRecord

	// clientsByName maps nfs_client_id4.id strings to confirmed client records.
	clientsByName map[string]*ClientRecord

	// unconfirmedByName maps nfs_client_id4.id strings to unconfirmed
	// client records (pending SETCLIENTID_CONFIRM).
	unconfirmedByName map[string]*ClientRecord

	// openStateByOther maps stateid "other" fields to OpenState records.
	// This is the primary lookup table for stateid validation.
	openStateByOther map[[types.NFS4_OTHER_SIZE]byte]*OpenState

	// openOwners maps open-owner keys to OpenOwner records.
	// Key is composite of clientID + hex(ownerData).
	openOwners map[openOwnerKey]*OpenOwner

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

	// lockManager is the unified lock manager for actual byte-range conflict
	// detection and cross-protocol locking. Set via SetLockManager() or constructor.
	lockManager *lock.Manager

	// bootEpoch is the server boot epoch, used as the high 32 bits of
	// client IDs to ensure uniqueness across server restarts.
	bootEpoch uint32

	// nextClientSeq is an atomic counter for the low 32 bits of client IDs.
	nextClientSeq uint32

	// nextStateSeq is an atomic counter for stateid "other" field generation.
	nextStateSeq uint64

	// leaseDuration is the configured lease duration for all clients.
	leaseDuration time.Duration

	// gracePeriod tracks the NFSv4 grace period state for server restart recovery.
	// Created lazily when StartGracePeriod is called.
	gracePeriod *GracePeriodState

	// graceDuration is the duration of the grace period.
	// Defaults to leaseDuration if not explicitly set.
	graceDuration time.Duration

	// ============================================================================
	// NFSv4.1 State (Phase 18+)
	// ============================================================================

	// v41ClientsByID maps server-assigned client IDs to v4.1 client records.
	v41ClientsByID map[uint64]*V41ClientRecord

	// v41ClientsByOwner maps owner ID bytes (string key) to v4.1 client records.
	// Uses string(ownerID) for byte-exact comparison.
	v41ClientsByOwner map[string]*V41ClientRecord

	// sessionsByID maps session IDs to session objects.
	sessionsByID map[types.SessionId4]*Session

	// sessionsByClientID maps client IDs to their sessions.
	sessionsByClientID map[uint64][]*Session

	// maxSessionsPerClient is the per-client session limit (default 16).
	maxSessionsPerClient int

	// sessionMetrics holds Prometheus metrics for session lifecycle.
	// nil-safe: all metric calls check for nil before recording.
	sessionMetrics *SessionMetrics

	// serverIdentity is the immutable server identity returned in EXCHANGE_ID responses.
	serverIdentity *ServerIdentity
}

// NewStateManager creates a new StateManager with the given lease duration.
// The boot epoch is derived from the current time.
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

	epoch := uint32(time.Now().Unix())

	return &StateManager{
		clientsByID:         make(map[uint64]*ClientRecord),
		clientsByName:       make(map[string]*ClientRecord),
		unconfirmedByName:   make(map[string]*ClientRecord),
		openStateByOther:    make(map[[types.NFS4_OTHER_SIZE]byte]*OpenState),
		openOwners:          make(map[openOwnerKey]*OpenOwner),
		lockOwners:          make(map[lockOwnerKey]*LockOwner),
		lockStateByOther:    make(map[[types.NFS4_OTHER_SIZE]byte]*LockState),
		delegByOther:        make(map[[types.NFS4_OTHER_SIZE]byte]*DelegationState),
		delegByFile:         make(map[string][]*DelegationState),
		recentlyRecalled:    make(map[string]time.Time),
		recentlyRecalledTTL: RecentlyRecalledTTL,
		delegationsEnabled:  true,
		bootEpoch:           epoch,
		leaseDuration:       leaseDuration,
		graceDuration:       gd,
		// NFSv4.1 state
		v41ClientsByID:       make(map[uint64]*V41ClientRecord),
		v41ClientsByOwner:    make(map[string]*V41ClientRecord),
		sessionsByID:         make(map[types.SessionId4]*Session),
		sessionsByClientID:   make(map[uint64][]*Session),
		maxSessionsPerClient: 16,
		serverIdentity:       newServerIdentity(epoch),
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
// Per research Pitfall 6: Do NOT use timestamps -- they are predictable.
func (sm *StateManager) generateConfirmVerifier() [8]byte {
	var v [8]byte
	if _, err := rand.Read(v[:]); err != nil {
		// crypto/rand.Read should never fail on supported platforms.
		// If it does, generate a non-zero fallback from time (degraded security).
		logger.Error("crypto/rand.Read failed, using time-based fallback", "error", err)
		now := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			v[i] = byte(now >> (uint(i) * 8))
		}
	}
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
func (sm *StateManager) SetClientID(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr string) (*SetClientIDResult, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	confirmed := sm.clientsByName[clientIDStr]
	unconfirmed := sm.unconfirmedByName[clientIDStr]

	switch {
	case confirmed == nil && unconfirmed == nil:
		// Case 1: Completely new client
		return sm.createNewClient(clientIDStr, verifier, callback, clientAddr)

	case confirmed != nil && confirmed.VerifierMatches(verifier):
		// Case 5: Same client, same verifier (re-SETCLIENTID, maybe callback update)
		// This handles the case where the client sends SETCLIENTID again with
		// the same verifier. We update the callback and return a new unconfirmed
		// record that, when confirmed, will replace the existing confirmed record.
		return sm.reuseConfirmedClient(confirmed, clientIDStr, verifier, callback, clientAddr)

	case confirmed != nil && !confirmed.VerifierMatches(verifier):
		// Case 3: Same client ID string, different verifier (client reboot)
		// The client has restarted. Create a new unconfirmed record.
		// The old confirmed record is NOT removed yet -- it gets replaced
		// when the new record is confirmed.
		return sm.handleClientReboot(clientIDStr, verifier, callback, clientAddr)

	case confirmed == nil && unconfirmed != nil:
		// Case 4: No confirmed record, unconfirmed exists -- replace unconfirmed
		return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback, clientAddr)

	default:
		// Case 2: Confirmed exists AND unconfirmed exists -- replace unconfirmed
		// This is a retransmit or new SETCLIENTID while another is pending.
		return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback, clientAddr)
	}
}

// createNewClient handles Case 1: completely new client.
// Creates a new unconfirmed record with a fresh client ID and confirm verifier.
// Caller must hold sm.mu.
func (sm *StateManager) createNewClient(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr string) (*SetClientIDResult, error) {
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
func (sm *StateManager) reuseConfirmedClient(confirmed *ClientRecord, clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr string) (*SetClientIDResult, error) {
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
func (sm *StateManager) handleClientReboot(clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr string) (*SetClientIDResult, error) {
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
func (sm *StateManager) replaceUnconfirmed(old *ClientRecord, clientIDStr string, verifier [8]byte, callback CallbackInfo, clientAddr string) (*SetClientIDResult, error) {
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

	// Look up the record by client ID
	record, exists := sm.clientsByID[clientID]
	if !exists {
		return fmt.Errorf("%w: client ID %d not found", ErrStaleClientID, clientID)
	}

	// If already confirmed, check for a pending re-SETCLIENTID (Case 5)
	// where an unconfirmed record exists with the same client ID.
	if record.Confirmed {
		// Check if there's an unconfirmed record for the same client name
		// that reuses this client ID (Case 5: re-SETCLIENTID for confirmed client)
		if unconfirmed := sm.unconfirmedByName[record.ClientIDString]; unconfirmed != nil && unconfirmed.ClientID == clientID {
			// Use the unconfirmed record instead - this is confirming the re-SETCLIENTID
			record = unconfirmed
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
		delete(sm.clientsByID, oldConfirmed.ClientID)
		logger.Info("SETCLIENTID_CONFIRM: replaced old confirmed client",
			"old_client_id", oldConfirmed.ClientID,
			"new_client_id", clientID)
	}

	// Mark as confirmed and store in confirmed map
	record.Confirmed = true
	sm.clientsByName[record.ClientIDString] = record

	// Create lease timer for the newly confirmed client
	record.Lease = NewLeaseState(clientID, sm.leaseDuration, sm.onLeaseExpired)
	record.LastRenewal = time.Now()

	logger.Info("SETCLIENTID_CONFIRM: client confirmed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString,
		"client_addr", record.ClientAddr)

	// Verify callback path asynchronously via CB_NULL.
	// This runs in a goroutine so SETCLIENTID_CONFIRM returns immediately.
	if record.Callback.Addr != "" {
		cbInfo := record.Callback
		go func() {
			err := SendCBNull(context.Background(), cbInfo)
			sm.mu.Lock()
			defer sm.mu.Unlock()
			rec, ok := sm.clientsByID[clientID]
			if !ok {
				return // Client was removed while CB_NULL was in flight
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
func (sm *StateManager) GetClient(clientID uint64) *ClientRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.clientsByID[clientID]
}

// RemoveClient removes a client record and all associated state.
// Used by lease expiry to clean up expired clients.
func (sm *StateManager) RemoveClient(clientID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record, exists := sm.clientsByID[clientID]
	if !exists {
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

	record, exists := sm.clientsByID[clientID]
	if !exists {
		return
	}

	logger.Info("NFSv4 client lease expired, cleaning up state",
		"client_id", clientID,
		"client_id_str", record.ClientIDString,
		"client_addr", record.ClientAddr)

	// Remove all open states AND lock states for all owners
	for _, owner := range record.OpenOwners {
		for _, openState := range owner.OpenStates {
			// Phase 10: Clean up lock states associated with this open
			for _, lockState := range openState.LockStates {
				// Remove from lockStateByOther map
				delete(sm.lockStateByOther, lockState.Stateid.Other)

				// Remove actual locks from unified lock manager
				if sm.lockManager != nil {
					ownerID := fmt.Sprintf("nfs4:%d:%s", lockState.LockOwner.ClientID,
						hex.EncodeToString(lockState.LockOwner.OwnerData))
					handleKey := string(lockState.FileHandle)
					locks := sm.lockManager.ListEnhancedLocks(handleKey)
					for _, l := range locks {
						if l.Owner.OwnerID == ownerID {
							_ = sm.lockManager.RemoveEnhancedLock(
								handleKey,
								l.Owner, l.Offset, l.Length,
							)
						}
					}
				}

				// Remove lock-owner from lockOwners map
				if lockState.LockOwner != nil {
					lockKey := makeLockOwnerKey(lockState.LockOwner.ClientID, lockState.LockOwner.OwnerData)
					delete(sm.lockOwners, lockKey)
				}
			}

			delete(sm.openStateByOther, openState.Stateid.Other)
		}
		// Remove the owner from the openOwners map
		ownerKey := makeOwnerKey(owner.ClientID, owner.OwnerData)
		delete(sm.openOwners, ownerKey)
	}

	// Phase 11: Clean up delegations for the expired client
	for other, deleg := range sm.delegByOther {
		if deleg.ClientID != clientID {
			continue
		}
		delete(sm.delegByOther, other)
		sm.removeDelegFromFile(deleg)

		logger.Info("Delegation revoked on lease expiry",
			"client_id", clientID,
			"deleg_type", deleg.DelegType)
	}

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
	defer sm.mu.Unlock()

	deleg, exists := sm.delegByOther[delegOther]
	if !exists || deleg.Revoked {
		return
	}

	deleg.Revoked = true
	sm.removeDelegFromFile(deleg)
	sm.addRecentlyRecalled(deleg.FileHandle)

	// Keep in delegByOther for stale stateid detection.

	logger.Warn("Delegation revoked due to recall timeout",
		"client_id", deleg.ClientID,
		"deleg_type", deleg.DelegType)
}

// Shutdown stops all active lease timers, recall timers, and the grace period
// for graceful server shutdown.
func (sm *StateManager) Shutdown() {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, record := range sm.clientsByID {
		if record.Lease != nil {
			record.Lease.Stop()
		}
	}

	// Stop all active delegation recall timers to prevent timer goroutines
	// firing after shutdown.
	for _, deleg := range sm.delegByOther {
		deleg.StopRecallTimer()
	}

	if sm.gracePeriod != nil {
		sm.gracePeriod.Stop()
	}

	logger.Info("StateManager: all lease and recall timers stopped")
}

// ============================================================================
// Grace Period Operations (Plan 09-04)
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

// CheckGraceForNewState returns NFS4ERR_GRACE if the server is in a grace period
// and the operation would create new state. Returns nil if the operation is allowed.
//
// This should be called before any new state-creating operation (OPEN with
// CLAIM_NULL, LOCK). Operations that use existing state (READ, WRITE, RENEW,
// CLOSE) should NOT call this.
//
// NOTE: LOCK operations (Phase 10) will also need to check this.
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
// Open File Operations (Plan 09-02)
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
) (*OpenFileResult, error) {
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
		// Notify the grace period that this client has reclaimed
		sm.mu.RLock()
		gp := sm.gracePeriod
		sm.mu.RUnlock()
		if gp != nil {
			gp.ClientReclaimed(clientID)
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up or create the open-owner
	ownerKey := makeOwnerKey(clientID, ownerData)
	owner, ownerExists := sm.openOwners[ownerKey]

	if ownerExists {
		// Existing owner: validate seqid
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
	} else {
		// New owner: create it
		clientRecord := sm.clientsByID[clientID]
		owner = &OpenOwner{
			ClientID:     clientID,
			OwnerData:    make([]byte, len(ownerData)),
			LastSeqID:    0, // Will be set to seqid after success
			Confirmed:    false,
			OpenStates:   make([]*OpenState, 0),
			ClientRecord: clientRecord,
		}
		copy(owner.OwnerData, ownerData)
		sm.openOwners[ownerKey] = owner

		// Register with client record if available
		if clientRecord != nil {
			clientRecord.OpenOwners[string(ownerData)] = owner
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
			Stateid:     resultStateid,
			Owner:       owner,
			FileHandle:  fhCopy,
			ShareAccess: shareAccess,
			ShareDeny:   shareDeny,
			Confirmed:   owner.Confirmed,
		}

		owner.OpenStates = append(owner.OpenStates, openState)
		sm.openStateByOther[other] = openState
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

// CacheOpenResult stores the cached result for an open-owner so that
// replay detection returns the correct response. Called by the handler
// after encoding the OPEN response.
func (sm *StateManager) CacheOpenResult(clientID uint64, ownerData []byte, status uint32, data []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ownerKey := makeOwnerKey(clientID, ownerData)
	owner, exists := sm.openOwners[ownerKey]
	if !exists {
		return
	}

	owner.LastResult = &CachedResult{
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
func (sm *StateManager) ConfirmOpen(stateid *types.Stateid4, seqid uint32) (*types.Stateid4, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "stateid not found for OPEN_CONFIRM",
		}
	}

	owner := openState.Owner

	// Validate seqid on the owner
	validation := owner.ValidateSeqID(seqid)
	switch validation {
	case SeqIDReplay:
		if owner.LastResult != nil {
			// For OPEN_CONFIRM replay, return the confirmed stateid
			resultStateid := openState.Stateid
			return &resultStateid, nil
		}
		return nil, ErrBadSeqid
	case SeqIDBad:
		return nil, ErrBadSeqid
	case SeqIDOK:
		// Continue
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

	return &resultStateid, nil
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
func (sm *StateManager) CloseFile(stateid *types.Stateid4, seqid uint32) (*types.Stateid4, error) {
	// Handle special stateids (all-zeros, all-ones): no state to clean up
	if stateid.IsSpecialStateid() {
		var zeroed types.Stateid4
		return &zeroed, nil
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "stateid not found for CLOSE",
		}
	}

	owner := openState.Owner

	// Validate seqid on the owner
	validation := owner.ValidateSeqID(seqid)
	switch validation {
	case SeqIDReplay:
		// CLOSE replay: return zeroed stateid
		var zeroed types.Stateid4
		return &zeroed, nil
	case SeqIDBad:
		return nil, ErrBadSeqid
	case SeqIDOK:
		// Continue
	}

	// Phase 10: Check for held locks before closing
	// Per RFC 7530, CLOSE MUST fail if byte-range locks are held.
	// Client must LOCKU all locks before CLOSE.
	if len(openState.LockStates) > 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_LOCKS_HELD,
			Message: "cannot close: byte-range locks still held, use LOCKU first",
		}
	}

	// Remove the open state from the "other" map
	delete(sm.openStateByOther, stateid.Other)

	// Remove from owner's OpenStates list
	for i, os := range owner.OpenStates {
		if os == openState {
			owner.OpenStates = append(owner.OpenStates[:i], owner.OpenStates[i+1:]...)
			break
		}
	}

	// Update owner seqid
	owner.LastSeqID = seqid

	// If owner has no more open states, clean up
	if len(owner.OpenStates) == 0 {
		ownerKey := makeOwnerKey(owner.ClientID, owner.OwnerData)
		delete(sm.openOwners, ownerKey)

		// Remove from client record
		if owner.ClientRecord != nil {
			delete(owner.ClientRecord.OpenOwners, string(owner.OwnerData))
		}

		logger.Debug("CloseFile: owner removed (no more open states)",
			"client_id", owner.ClientID)
	}

	logger.Debug("CloseFile: state removed",
		"client_id", owner.ClientID,
		"seqid", seqid)

	// Return zeroed stateid
	var zeroed types.Stateid4
	return &zeroed, nil
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
func (sm *StateManager) DowngradeOpen(stateid *types.Stateid4, seqid uint32, newShareAccess, newShareDeny uint32) (*types.Stateid4, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Look up the open state
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "stateid not found for OPEN_DOWNGRADE",
		}
	}

	owner := openState.Owner

	// Validate seqid on the owner
	validation := owner.ValidateSeqID(seqid)
	switch validation {
	case SeqIDReplay:
		resultStateid := openState.Stateid
		return &resultStateid, nil
	case SeqIDBad:
		return nil, ErrBadSeqid
	case SeqIDOK:
		// Continue
	}

	// Verify the new access is a subset of current (can only remove bits)
	if newShareAccess & ^openState.ShareAccess != 0 {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_INVAL,
			Message: "OPEN_DOWNGRADE cannot add share_access bits",
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

	return &resultStateid, nil
}

// GetOpenState returns the OpenState for a given stateid "other" field,
// or nil if not found. Used for read-only lookups that don't need validation.
func (sm *StateManager) GetOpenState(other [types.NFS4_OTHER_SIZE]byte) *OpenState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.openStateByOther[other]
}

// ============================================================================
// Lease Operations (Plan 09-02, Task 4)
// ============================================================================

// RenewLease implements the RENEW operation's state management.
//
// Per RFC 7530 Section 16.29:
//   - Validates the client ID exists and is confirmed
//   - Resets the lease timer and updates LastRenewal
//   - Returns ErrStaleClientID if client is unknown or unconfirmed
//   - Returns NFS4ERR_EXPIRED if the lease has already expired
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) RenewLease(clientID uint64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record, exists := sm.clientsByID[clientID]
	if !exists {
		return ErrStaleClientID
	}

	if !record.Confirmed {
		return ErrStaleClientID
	}

	// Check if lease has expired
	if record.Lease != nil && record.Lease.IsExpired() {
		return ErrExpired
	}

	// Renew the lease timer
	if record.Lease != nil {
		record.Lease.Renew()
	}
	record.LastRenewal = time.Now()

	logger.Debug("RenewLease: lease renewed",
		"client_id", clientID,
		"client_id_str", record.ClientIDString)

	return nil
}

// ============================================================================
// Lock Manager Integration (Plan 10-01)
// ============================================================================

// SetLockManager sets the unified lock manager for byte-range conflict detection.
// Called by the NFS adapter after construction to inject the lock manager.
func (sm *StateManager) SetLockManager(lm *lock.Manager) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.lockManager = lm
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
	lockClientID uint64, lockOwnerData []byte, lockSeqid uint32,
	openStateid *types.Stateid4, openSeqid uint32,
	fileHandle []byte, lockType uint32, offset, length uint64, reclaim bool,
) (*LockResult, error) {
	// Grace period check (before acquiring sm.mu)
	if !reclaim {
		if err := sm.CheckGraceForNewState(); err != nil {
			return nil, err
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Validate open stateid
	openState, exists := sm.openStateByOther[openStateid.Other]
	if !exists {
		return nil, ErrBadStateid
	}

	// 2. Validate open-owner seqid
	validation := openState.Owner.ValidateSeqID(openSeqid)
	switch validation {
	case SeqIDBad:
		return nil, ErrBadSeqid
	case SeqIDReplay:
		// For replay on a lock-new, we don't have a cached lock result
		// on the open-owner (those belong to OPEN operations).
		return nil, ErrBadSeqid
	case SeqIDOK:
		// Continue
	}

	// 3. Validate open mode for lock type
	if err := validateOpenModeForLock(openState, lockType); err != nil {
		return nil, err
	}

	// 4. Find or create lock-owner
	loKey := makeLockOwnerKey(lockClientID, lockOwnerData)
	lockOwner, ownerExists := sm.lockOwners[loKey]
	if !ownerExists {
		clientRecord := sm.clientsByID[lockClientID]
		lockOwner = &LockOwner{
			ClientID:     lockClientID,
			OwnerData:    make([]byte, len(lockOwnerData)),
			LastSeqID:    0,
			ClientRecord: clientRecord,
		}
		copy(lockOwner.OwnerData, lockOwnerData)
		sm.lockOwners[loKey] = lockOwner
	}

	// 5. Find or create lock state for (lock-owner, open-state) pair
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

	// 6. Validate lock seqid on lock-owner
	if ownerExists {
		lockValidation := lockOwner.ValidateSeqID(lockSeqid)
		switch lockValidation {
		case SeqIDBad:
			return nil, ErrBadSeqid
		case SeqIDReplay:
			// Return cached result if available
			if lockOwner.LastResult != nil {
				return nil, ErrBadSeqid
			}
			return nil, ErrBadSeqid
		case SeqIDOK:
			// Continue
		}
	}

	// 7. Acquire the lock via unified lock manager
	denied, err := sm.acquireLock(lockState, lockType, offset, length, reclaim)
	if err != nil {
		return nil, err
	}
	if denied != nil {
		return &LockResult{Denied: denied}, nil
	}

	// 8. Success: update state
	lockState.Stateid.Seqid = nextSeqID(lockState.Stateid.Seqid)
	lockOwner.LastSeqID = lockSeqid
	openState.Owner.LastSeqID = openSeqid

	return &LockResult{Stateid: lockState.Stateid}, nil
}

// LockExisting implements the LOCK operation for an existing lock-owner.
//
// This is the "exist_lock_owner4" path where the client provides an existing
// lock stateid to acquire additional locks.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) LockExisting(
	lockStateid *types.Stateid4, lockSeqid uint32,
	fileHandle []byte, lockType uint32, offset, length uint64, reclaim bool,
) (*LockResult, error) {
	// Grace period check
	if !reclaim {
		if err := sm.CheckGraceForNewState(); err != nil {
			return nil, err
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Look up lock state
	lockState, exists := sm.lockStateByOther[lockStateid.Other]
	if !exists {
		if !sm.isCurrentEpoch(lockStateid.Other) {
			return nil, ErrStaleStateid
		}
		return nil, ErrBadStateid
	}

	// Validate seqid: old vs bad
	if lockStateid.Seqid < lockState.Stateid.Seqid {
		return nil, ErrOldStateid
	}
	if lockStateid.Seqid > lockState.Stateid.Seqid {
		return nil, ErrBadStateid
	}

	// 2. Validate open mode for lock type
	if err := validateOpenModeForLock(lockState.OpenState, lockType); err != nil {
		return nil, err
	}

	// 3. Validate lock seqid on lock-owner
	lockOwner := lockState.LockOwner
	lockValidation := lockOwner.ValidateSeqID(lockSeqid)
	switch lockValidation {
	case SeqIDBad:
		return nil, ErrBadSeqid
	case SeqIDReplay:
		return nil, ErrBadSeqid
	case SeqIDOK:
		// Continue
	}

	// 4. Acquire the lock
	denied, err := sm.acquireLock(lockState, lockType, offset, length, reclaim)
	if err != nil {
		return nil, err
	}
	if denied != nil {
		return &LockResult{Denied: denied}, nil
	}

	// 5. Success: update state
	lockState.Stateid.Seqid = nextSeqID(lockState.Stateid.Seqid)
	lockOwner.LastSeqID = lockSeqid

	return &LockResult{Stateid: lockState.Stateid}, nil
}

// acquireLock attempts to acquire a byte-range lock via the unified lock manager.
//
// Returns (nil, nil) on success, (*LOCK4denied, nil) on conflict,
// or (nil, error) on internal errors.
//
// Caller must hold sm.mu.
func (sm *StateManager) acquireLock(lockState *LockState, lockType uint32, offset, length uint64, reclaim bool) (*LOCK4denied, error) {
	if sm.lockManager == nil {
		return nil, fmt.Errorf("no lock manager configured")
	}

	// Build the protocol-agnostic lock owner
	owner := lock.LockOwner{
		OwnerID:   fmt.Sprintf("nfs4:%d:%s", lockState.LockOwner.ClientID, hex.EncodeToString(lockState.LockOwner.OwnerData)),
		ClientID:  fmt.Sprintf("nfs4:%d", lockState.LockOwner.ClientID),
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
	enhLock := lock.NewEnhancedLock(owner, lock.FileHandle(lockState.FileHandle), offset, length, mappedType)
	enhLock.Reclaim = reclaim

	// Try to add the lock
	handleKey := string(lockState.FileHandle)
	err := sm.lockManager.AddEnhancedLock(handleKey, enhLock)
	if err != nil {
		// Lock conflict: query existing locks to find the conflicting one
		// for the LOCK4denied response
		existingLocks := sm.lockManager.ListEnhancedLocks(handleKey)
		for _, el := range existingLocks {
			if lock.IsEnhancedLockConflicting(el, enhLock) {
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
				// Parse OwnerID to extract clientID and ownerData
				// Format: "nfs4:{clientid}:{owner_hex}"
				denied.Owner.ClientID = 0 // Default if parsing fails
				denied.Owner.OwnerData = []byte(el.Owner.OwnerID)
				return denied, nil
			}
		}

		// Conflict exists but we couldn't identify the exact lock (shouldn't happen)
		denied := &LOCK4denied{
			Offset:   offset,
			Length:   length,
			LockType: lockType,
		}
		return denied, nil
	}

	return nil, nil
}

// ============================================================================
// LOCKT - Lock Test (Plan 10-02)
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
	if sm.lockManager == nil {
		// No lock manager = no locks possible = no conflicts
		return nil, nil
	}

	// Build the owner ID string using the same format as acquireLock
	ownerID := fmt.Sprintf("nfs4:%d:%s", clientID, hex.EncodeToString(ownerData))

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

	// Create a temporary test lock (not added to the manager)
	testLock := &lock.EnhancedLock{
		Owner:  lock.LockOwner{OwnerID: ownerID},
		Offset: offset,
		Length: length,
		Type:   mappedType,
	}

	// Query existing locks on this file
	handleKey := string(fileHandle)
	existingLocks := sm.lockManager.ListEnhancedLocks(handleKey)

	// Check each existing lock for conflict
	for _, el := range existingLocks {
		if lock.IsEnhancedLockConflicting(el, testLock) {
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
	}

	return nil, nil
}

// ============================================================================
// LOCKU - Unlock File (Plan 10-02)
// ============================================================================

// UnlockFile releases a byte-range lock via the lock manager using POSIX split semantics.
//
// Per RFC 7530 Section 16.12:
//   - Validates the lock stateid and seqid
//   - Calls the lock manager's RemoveEnhancedLock for POSIX splitting
//   - Increments the lock stateid seqid on success
//   - The lock state is NOT removed (persists for future LOCK operations)
//   - RELEASE_LOCKOWNER (Plan 10-03) handles state cleanup
//
// Lock-not-found from the lock manager is treated as success (idempotent unlock).
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) UnlockFile(
	lockStateid *types.Stateid4, seqid uint32,
	lockType uint32, offset, length uint64,
) (*types.Stateid4, error) {
	// Special stateids cannot be used with LOCKU
	if lockStateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Look up lock state
	lockState, exists := sm.lockStateByOther[lockStateid.Other]
	if !exists {
		// Check if it's a stale stateid from a previous boot
		if !sm.isCurrentEpoch(lockStateid.Other) {
			return nil, ErrStaleStateid
		}
		return nil, ErrBadStateid
	}

	// 2. Validate stateid seqid
	if lockStateid.Seqid < lockState.Stateid.Seqid {
		return nil, ErrOldStateid
	}
	if lockStateid.Seqid > lockState.Stateid.Seqid {
		return nil, ErrBadStateid
	}

	// 3. Validate seqid on lock-owner
	lockOwner := lockState.LockOwner
	validation := lockOwner.ValidateSeqID(seqid)
	switch validation {
	case SeqIDBad:
		return nil, ErrBadSeqid
	case SeqIDReplay:
		// For LOCKU replay, return current stateid (idempotent)
		resultStateid := lockState.Stateid
		return &resultStateid, nil
	case SeqIDOK:
		// Continue
	}

	// 4. Release the lock via unified lock manager
	if sm.lockManager != nil {
		owner := lock.LockOwner{
			OwnerID:   fmt.Sprintf("nfs4:%d:%s", lockOwner.ClientID, hex.EncodeToString(lockOwner.OwnerData)),
			ClientID:  fmt.Sprintf("nfs4:%d", lockOwner.ClientID),
			ShareName: "",
		}

		handleKey := string(lockState.FileHandle)
		err := sm.lockManager.RemoveEnhancedLock(handleKey, owner, offset, length)
		if err != nil {
			// Lock-not-found is OK for LOCKU (idempotent).
			// Only fail on unexpected errors.
			// RemoveEnhancedLock returns StoreError with ErrLockNotFound code.
			// We treat all errors as non-fatal for idempotency.
			logger.Debug("LOCKU: lock manager RemoveEnhancedLock returned error (idempotent OK)",
				"error", err,
				"handle", handleKey,
				"offset", offset,
				"length", length)
		}
	}

	// 5. Success: increment lock stateid seqid
	lockState.Stateid.Seqid = nextSeqID(lockState.Stateid.Seqid)
	lockOwner.LastSeqID = seqid

	resultStateid := lockState.Stateid
	return &resultStateid, nil
}

// ============================================================================
// RELEASE_LOCKOWNER (Plan 10-03)
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

	// Check if this lock-owner has any active locks in the lock manager
	if sm.lockManager != nil {
		ownerID := fmt.Sprintf("nfs4:%d:%s", lockOwner.ClientID,
			hex.EncodeToString(lockOwner.OwnerData))

		// Find all lock states for this lock-owner by scanning lockStateByOther
		for _, ls := range sm.lockStateByOther {
			if ls.LockOwner != lockOwner {
				continue
			}
			// Check if any locks are held for this owner on this file
			handleKey := string(ls.FileHandle)
			locks := sm.lockManager.ListEnhancedLocks(handleKey)
			for _, l := range locks {
				if l.Owner.OwnerID == ownerID {
					return &NFS4StateError{
						Status:  types.NFS4ERR_LOCKS_HELD,
						Message: "cannot release lock-owner: locks still held",
					}
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
// NFSv4.1 Session Management (Phase 19)
// ============================================================================

// SetSessionMetrics sets the Prometheus metrics collector for session lifecycle.
// Must be called before any session operations. Safe to leave nil (no-op metrics).
func (sm *StateManager) SetSessionMetrics(m *SessionMetrics) {
	sm.sessionMetrics = m
}

// CreateSession implements the CREATE_SESSION algorithm per RFC 8881 Section 18.36.
//
// The algorithm uses the client's sequence ID to detect replays:
//   - sequenceID == record.SequenceID: replay -- return cached response
//   - sequenceID == record.SequenceID + 1: new request -- create session
//   - otherwise: misordered -- return error
//
// On success, returns the CreateSessionResult and nil cached bytes.
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
) (*CreateSessionResult, []byte, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Case 1: Unknown client
	record, exists := sm.v41ClientsByID[clientID]
	if !exists {
		return nil, nil, ErrStaleClientID
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

	// Check per-client session limit
	if len(sm.sessionsByClientID[clientID]) >= sm.maxSessionsPerClient {
		return nil, nil, ErrTooManySessions
	}

	// Negotiate channel attributes
	negotiatedFore := negotiateChannelAttrs(foreAttrs, DefaultForeLimits())
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
		record.Confirmed = true
		record.Lease = NewLeaseState(record.ClientID, sm.leaseDuration, nil)
		record.LastRenewal = time.Now()
	}

	// Increment sequence ID
	record.SequenceID++

	// Record metrics
	sm.sessionMetrics.recordCreated()

	logger.Info("CREATE_SESSION: session created",
		"client_id", fmt.Sprintf("0x%x", clientID),
		"session_id", session.SessionID.String(),
		"fore_slots", negotiatedFore.MaxRequests)

	return &CreateSessionResult{
		SessionID:        session.SessionID,
		SequenceID:       record.SequenceID,
		Flags:            responseFlags,
		ForeChannelAttrs: negotiatedFore,
		BackChannelAttrs: negotiatedBack,
	}, nil, nil
}

// CacheCreateSessionResponse stores the full XDR-encoded CREATE_SESSION
// response bytes on the client record for replay detection.
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) CacheCreateSessionResponse(clientID uint64, responseBytes []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	record, exists := sm.v41ClientsByID[clientID]
	if !exists {
		return
	}

	// Copy the bytes to avoid holding references to caller's buffer
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

	// Record metrics
	duration := time.Since(session.CreatedAt).Seconds()
	sm.sessionMetrics.recordDestroyed(reason, duration)

	logger.Info("Session destroyed",
		"session_id", session.SessionID.String(),
		"client_id", fmt.Sprintf("0x%x", session.ClientID),
		"reason", reason,
		"duration_s", fmt.Sprintf("%.1f", duration))

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
// The reaper runs every 30 seconds and checks:
//   - Clients with expired leases: destroys all sessions, purges client
//   - Unconfirmed clients older than 2x lease duration: purges client
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
	var toPurge []*V41ClientRecord

	for _, record := range sm.v41ClientsByID {
		// Check lease expiry for confirmed clients
		if record.Lease != nil && record.Lease.IsExpired() {
			logger.Info("Session reaper: lease expired",
				"client_id", fmt.Sprintf("0x%x", record.ClientID),
				"client_addr", record.ClientAddr)

			// Destroy all sessions for this client with "lease_expired" reason
			for _, session := range sm.sessionsByClientID[record.ClientID] {
				delete(sm.sessionsByID, session.SessionID)
				sm.sessionMetrics.recordDestroyed("lease_expired", time.Since(session.CreatedAt).Seconds())
			}
			delete(sm.sessionsByClientID, record.ClientID)

			toPurge = append(toPurge, record)
			continue
		}

		// Check for unconfirmed clients that timed out
		if !record.Confirmed && now.Sub(record.CreatedAt) > 2*sm.leaseDuration {
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
}
