package state

import (
	"context"
	"encoding/hex"
	"time"

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
// with ReclaimComplete clear, so the fresh incarnation must still send
// RECLAIM_COMPLETE and a stale retry must not stamp its durable record as
// done. At most one chain per key exists: a schedule while an entry lives
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
