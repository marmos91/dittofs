package state

import (
	"context"
	"encoding/hex"
	"slices"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// recoveryPersistTimeout bounds every synchronous client-recovery store call.
// Mirrors the lock manager's persistTimeout so a hung backend cannot wedge a
// confirm/expiry under sm.mu. The in-memory state is authoritative for the
// running process; persistence is best-effort for cross-restart durability.
const recoveryPersistTimeout = 3 * time.Second

// SetClientRecoveryStore wires the server-global durable client-recovery store
// and the current server epoch. Called once by the NFS adapter after picking
// the first share's metadata store that implements lock.ClientRecoveryStore
// (mirroring the NSM ClientRegistrationStore designation).
//
// When never called (or called with a nil store), the StateManager behaves
// exactly as before: no records are persisted, boot-load is a no-op, and
// CLAIM_PREVIOUS is not verifier-gated. This keeps bare test constructions and
// the develop fast-path working unchanged.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) SetClientRecoveryStore(store lock.ClientRecoveryStore, serverEpoch uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.recoveryStore = store
	sm.serverEpoch = serverEpoch
}

// HasClientRecoveryStore reports whether a durable recovery store is wired.
// Thread-safe: acquires sm.mu.RLock.
func (sm *StateManager) HasClientRecoveryStore() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.recoveryStore != nil
}

// v41RecoveryKey derives the stable recovery-record key for a v4.1 client.
// v4.1 has no nfs_client_id4 string; the stable identity is co_ownerid, so we
// hex-encode it (prefixed to keep it disjoint from any v4.0 nfs_client_id4
// string, which is the raw client-supplied identifier).
func v41RecoveryKey(ownerID []byte) string {
	return "v41:" + hex.EncodeToString(ownerID)
}

// recoveryKeyForClientLocked resolves the durable recovery key for a confirmed
// client by numeric clientID. The key shape carries the minor version (v4.0 =
// the raw nfs_client_id4 string, v4.1 = the prefixed co_ownerid hex), so the
// record's version decides it. Returns "" when the client is unknown. Caller
// must hold sm.mu (R or W).
func (sm *StateManager) recoveryKeyForClientLocked(clientID uint64) string {
	rec := sm.clientRecordLocked(clientID)
	if rec == nil {
		return ""
	}
	if rec.MinorVersion == 1 {
		return v41RecoveryKey(rec.OwnerID)
	}
	return rec.ClientIDString
}

// persistClientRecoveryLocked stores a durable recovery record for a confirmed
// client. Best-effort under sm.mu, bounded by recoveryPersistTimeout: a failure
// logs a durability ALARM but the confirm STILL succeeds (the in-memory record
// is authoritative for this process). No-op when no recovery store is wired.
// Caller must hold sm.mu.
func (sm *StateManager) persistClientRecoveryLocked(clientID uint64, clientIDString string, bootVerifier [8]byte, principal string) {
	if sm.recoveryStore == nil {
		return
	}
	rec := &lock.V4ClientRecoveryRecord{
		ClientID:       clientID,
		ClientIDString: clientIDString,
		BootVerifier:   bootVerifier,
		Principal:      principal,
		ConfirmedAt:    time.Now(),
		ServerEpoch:    sm.serverEpoch,
	}
	ctx, cancel := context.WithTimeout(context.Background(), recoveryPersistTimeout)
	defer cancel()
	if err := sm.recoveryStore.PutClientRecovery(ctx, rec); err != nil {
		logger.Error("client-recovery persistence failed: client confirmed in memory but NOT durable across restart",
			"client_id", clientID,
			"client_id_str", clientIDString,
			"error", err)
	}
}

// deleteClientRecoveryLocked removes a client's durable recovery record on
// lease expiry / eviction / DESTROY_CLIENTID. Best-effort, bounded timeout.
// No-op when no recovery store is wired. Caller must hold sm.mu.
func (sm *StateManager) deleteClientRecoveryLocked(clientIDString string) {
	if sm.recoveryStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recoveryPersistTimeout)
	defer cancel()
	if err := sm.recoveryStore.DeleteClientRecovery(ctx, clientIDString); err != nil {
		logger.Error("client-recovery delete failed: stale recovery record may linger across restart",
			"client_id_str", clientIDString,
			"error", err)
	}
}

// reclaimPersistRetryBase / reclaimPersistRetryCap bound the backoff of the
// asynchronous reclaim-complete persist retry. The base sits well under the
// lease duration so a failed write is repaired long before the next restart
// could re-wait on the client; the cap keeps a persistently down backend from
// piling up attempts.
const (
	reclaimPersistRetryBase = 2 * time.Second
	reclaimPersistRetryCap  = 30 * time.Second
)

// pendingReclaimPersist carries one retry of a failed reclaim-complete persist:
// the recovery key, the client ID that issued the mark, and the next backoff
// delay. The pendingReclaimPersists map on StateManager tracks at most one
// chain per key: a re-schedule while an entry exists adopts the live entry
// (updating its client ID to the latest issuer) instead of forking a second
// chain, so a down backend cannot pile up attempts and a new issuer's persist
// failure cannot be skipped while an old chain lives. Before each write the
// retry re-validates that the issuing client still holds the key, so a client
// that re-registers (and whose durable record is then deleted or replaced) is
// in the common case skipped by the validation; the write is best-effort and
// out of lock, so a narrow stale-success window remains (a retry racing the
// removal path) and self-heals on the client's next reclaim-complete.
type pendingReclaimPersist struct {
	key      string
	clientID uint64
	delay    time.Duration
}

// recordReclaimCompleteLocked marks a client's recovery record reclaim-complete
// (v4.1 RECLAIM_COMPLETE, or first CLAIM_PREVIOUS for v4.0) so a second restart
// inside one grace window does not wait on an already-reclaimed client.
// Best-effort, bounded timeout. No-op when no recovery store is wired.
// The decision runs under sm.mu (caller holds it); the store write itself
// runs after the caller releases the lock, so a slow backend never wedges
// state operations — the same discipline the retry chain follows.
func (sm *StateManager) recordReclaimCompleteLocked(clientID uint64, key string) {
	if sm.recoveryStore == nil {
		return
	}
	store := sm.recoveryStore
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), recoveryPersistTimeout)
		defer cancel()
		if err := store.RecordReclaimComplete(ctx, key); err != nil {
			logger.Error("client-recovery reclaim-complete persistence failed: retrying in background; until it lands a second restart may re-wait on this client",
				"client_id_str", key,
				"error", err)
			sm.mu.Lock()
			sm.scheduleReclaimPersistRetryLocked(clientID, key, reclaimPersistRetryBase)
			sm.mu.Unlock()
		}
	}()
}

// scheduleReclaimPersistRetryLocked arms the asynchronous retry of a failed
// reclaim-complete persist. The retry runs OFF sm.mu (a synchronous backoff
// under the lock would wedge every state operation behind the down backend,
// which is what recoveryPersistTimeout exists to prevent), and re-validates
// before each write that the client that issued the mark still holds the key
// — a client that re-registers after a restart gets a fresh in-memory record
// with ReclaimComplete clear, so a stale retry is abandoned once the issuer
// is gone and in the common case never reaches the durable write; the write
// runs out of lock, so a narrow stale-success window remains (a retry racing
// the removal path) and self-heals on the client's next reclaim-complete.
// At most one chain per key exists: a schedule while an entry lives
// adopts it (updating the issuer and arming a fresh timer at the new delay;
// the old timer's fire becomes a no-op retry that finds the same entry and
// either lands the write or reschedules with the adopted state). Caller must
// hold sm.mu.
func (sm *StateManager) scheduleReclaimPersistRetryLocked(clientID uint64, key string, delay time.Duration) {
	if sm.recoveryStore == nil {
		delete(sm.pendingReclaimPersists, key)
		return
	}
	if pending, ok := sm.pendingReclaimPersists[key]; ok {
		// Adopt the live chain: point it at the latest issuer so the validity
		// check tracks the client whose persist most recently failed, and arm
		// a fresh timer at the new delay so the adopted issuer's failure gets
		// the base delay the field promises. The old timer, still pending,
		// fires into a retry of the same chain: it re-validates the adopted
		// issuer and either lands the write or reschedules — so no attempt is
		// lost and no second chain forks.
		pending.clientID = clientID
		pending.delay = delay
		go func() {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			<-timer.C
			sm.retryReclaimPersist(pending)
		}()
		return
	}

	pending := &pendingReclaimPersist{key: key, clientID: clientID, delay: delay}
	sm.pendingReclaimPersists[key] = pending
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		sm.retryReclaimPersist(pending)
	}()
}

// retryReclaimPersist runs one retry of a failed reclaim-complete persist.
// On success or on an issuer that no longer holds the key (re-registered,
// expired, destroyed) the pending entry is dropped; on a store failure it
// reschedules itself with the backoff doubled up to reclaimPersistRetryCap.
// Thread-safe: acquires sm.mu.
func (sm *StateManager) retryReclaimPersist(pending *pendingReclaimPersist) {
	sm.mu.Lock()
	store := sm.recoveryStore
	if store == nil {
		delete(sm.pendingReclaimPersists, pending.key)
		sm.mu.Unlock()
		return
	}
	// A stale timer (whose chain succeeded, was adopted, or was replaced) must
	// not issue a store write for an entry it no longer owns: drop the entry
	// when this timer's chain is not the live one.
	if live, ok := sm.pendingReclaimPersists[pending.key]; !ok || live != pending {
		sm.mu.Unlock()
		return
	}
	// The client that issued the mark must still hold the key: its in-memory
	// record's ReclaimComplete is what the durable write mirrors.
	if !sm.clientHoldsKeyLocked(pending.clientID, pending.key) {
		delete(sm.pendingReclaimPersists, pending.key)
		sm.mu.Unlock()
		return
	}
	sm.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), recoveryPersistTimeout)
	defer cancel()
	err := store.RecordReclaimComplete(ctx, pending.key)

	sm.mu.Lock()
	defer sm.mu.Unlock()
	// A stale timer (one whose chain was adopted, so a fresher timer now owns
	// the entry) must not reschedule after its write lands: if the entry was
	// replaced, its delay was reset, and this write's doubled delay would
	// overwrite it. Only the write whose entry is still the one it scheduled
	// may drive the chain forward; the freshest timer always is.
	live, ok := sm.pendingReclaimPersists[pending.key]
	if !ok || live != pending {
		return
	}
	if err == nil {
		delete(sm.pendingReclaimPersists, pending.key)
		return
	}
	delay := pending.delay * 2
	if delay > reclaimPersistRetryCap {
		delay = reclaimPersistRetryCap
	}
	pending.delay = delay
	logger.Error("client-recovery reclaim-complete persist retry failed; retrying with backoff",
		"client_id_str", pending.key,
		"next_delay", delay,
		"error", err)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		sm.retryReclaimPersist(pending)
	}()
}

// clientHoldsKeyLocked reports whether the given client still owns the recovery
// key and has ReclaimComplete set in memory. Caller must hold sm.mu (write).
func (sm *StateManager) clientHoldsKeyLocked(clientID uint64, key string) bool {
	rec := sm.clientRecordLocked(clientID)
	if rec == nil {
		return false
	}
	switch {
	case rec.ClientIDString == key:
		return rec.ReclaimComplete
	case v41RecoveryKey(rec.OwnerID) == key:
		return rec.ReclaimComplete
	default:
		return false
	}
}

// validateReclaimVerifier rejects a CLAIM_PREVIOUS reclaim whose boot verifier
// does not match the PRE-RESTART durable verifier (RFC 7530 §9.1.4):
// a changed verifier means the client rebooted and must NOT reclaim prior
// state.
//
// It compares against the boot-load snapshot (bootRecoveryVerifiers), not the
// live store: a rebooting client's SETCLIENTID_CONFIRM overwrites its live
// recovery record with the NEW verifier before the CLAIM_PREVIOUS reaches here,
// so the live store would always "match" and the reboot would go undetected.
//
//   - snapshot has identity + verifier matches -> nil (valid reclaimer)
//   - snapshot has identity + verifier differs -> ErrNoGrace (rebooted; reject)
//   - identity absent from snapshot              -> nil (nothing to gate on)
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) validateReclaimVerifier(clientIDString string, bootVerifier [8]byte) error {
	sm.mu.RLock()
	want, ok := sm.bootRecoveryVerifiers[clientIDString]
	sm.mu.RUnlock()
	if !ok {
		// No pre-restart record for this identity: nothing to gate on.
		return nil
	}
	if want == bootVerifier {
		return nil // valid reclaimer
	}
	logger.Warn("CLAIM_PREVIOUS rejected: boot verifier changed since prior confirm (client rebooted)",
		"client_id_str", clientIDString)
	return ErrNoGrace
}

// LoadClientRecovery reads the durable recovery records on boot and starts the
// v4 grace period seeded with the prior clients' stable identity strings.
// Records whose ReclaimComplete is already true are NOT waited on: a second
// restart inside one grace window must not re-wait on a client that already
// finished reclaim.
//
// The boot verifier snapshot is taken unconditionally, because the
// CLAIM_PREVIOUS verifier gate must be armed for any prior client whether or
// not a reclaim window is opened.
//
// armGrace decides whether the roster opens a window. A record is written for
// every client that reaches SETCLIENTID_CONFIRM, even one that never opened or
// locked anything, and a v4.0 client with nothing to reclaim can never retire
// its own entry (it returns with CLAIM_NULL, and v4.0 has no RECLAIM_COMPLETE).
// Arming on the roster alone therefore burns a full grace duration on every
// later start, refusing every CLAIM_NULL OPEN with no client able to end it
// early.
//
// Returns the number of clients added to the expected reclaim roster; 0 when no
// recovery store is wired, no records are waitable, or armGrace is false. The
// hard grace timer remains the backstop regardless.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) LoadClientRecovery(ctx context.Context, armGrace bool) int {
	sm.mu.RLock()
	store := sm.recoveryStore
	sm.mu.RUnlock()
	if store == nil {
		return 0
	}

	records, err := store.ListClientRecovery(ctx)
	if err != nil {
		logger.Error("client-recovery boot load failed: v4 grace roster will be empty",
			"error", err)
		return 0
	}

	expectedStrings := make([]string, 0, len(records))
	verifiers := make(map[string][8]byte, len(records))
	for _, rec := range records {
		// Snapshot every prior verifier (including reclaim-complete ones) so the
		// CLAIM_PREVIOUS verifier gate can detect a rebooted client regardless of
		// whether it is still on the waitable roster.
		verifiers[rec.ClientIDString] = rec.BootVerifier
		if rec.ReclaimComplete {
			// Already reclaimed in a prior (very recent) grace window; do not
			// wait on it again.
			continue
		}
		expectedStrings = append(expectedStrings, rec.ClientIDString)
	}

	// Record the verifier snapshot regardless of whether grace is seeded: a
	// reclaim attempt is only honored during grace, but the gate must be armed
	// even when the only prior records are already reclaim-complete.
	sm.mu.Lock()
	sm.bootRecoveryVerifiers = verifiers
	sm.mu.Unlock()

	if len(expectedStrings) == 0 {
		logger.Info("client-recovery boot load: no waitable prior clients; v4 grace not seeded")
		return 0
	}

	if !armGrace {
		logger.Info("client-recovery boot load: no reclaimable state on this start; v4 grace not seeded",
			"prior_clients", len(expectedStrings))
		return 0
	}

	sm.mu.Lock()
	gp := NewGracePeriodState(sm.graceDuration, func() {
		logger.Info("NFSv4 grace period ended (boot-loaded roster)")
	})
	sm.gracePeriod = gp
	sm.mu.Unlock()

	gp.StartGraceWithRoster(nil, expectedStrings)

	logger.Info("client-recovery boot load: v4 grace period seeded",
		"expected_clients", len(expectedStrings))
	return len(expectedStrings)
}

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
