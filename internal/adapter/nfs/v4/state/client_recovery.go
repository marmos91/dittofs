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
// client by numeric clientID, checking the v4.0 then v4.1 tables. Returns ""
// when the client is unknown. Caller must hold sm.mu (R or W).
func (sm *StateManager) recoveryKeyForClientLocked(clientID uint64) string {
	if rec, ok := sm.clientsByID[clientID]; ok {
		return rec.ClientIDString
	}
	if rec, ok := sm.v41ClientsByID[clientID]; ok {
		return v41RecoveryKey(rec.OwnerID)
	}
	return ""
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

// recordReclaimCompleteLocked marks a client's recovery record reclaim-complete
// (v4.1 RECLAIM_COMPLETE, or first CLAIM_PREVIOUS for v4.0) so a second restart
// inside one grace window does not wait on an already-reclaimed client.
// Best-effort, bounded timeout. No-op when no recovery store is wired.
// Caller must hold sm.mu.
func (sm *StateManager) recordReclaimCompleteLocked(clientIDString string) {
	if sm.recoveryStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recoveryPersistTimeout)
	defer cancel()
	if err := sm.recoveryStore.RecordReclaimComplete(ctx, clientIDString); err != nil {
		logger.Error("client-recovery reclaim-complete persistence failed: a second restart may re-wait on this client",
			"client_id_str", clientIDString,
			"error", err)
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
// v4 grace period seeded with the prior clients' stable identity strings:
// today the v4 roster is empty on a fresh process so v4 grace
// is a no-op; now it is the durable prior-client set).
//
// Records whose ReclaimComplete is already true are NOT waited on (a second
// restart inside one grace window must not re-wait on a client that already
// finished reclaim — §3.2 RecordReclaimComplete rationale).
//
// Returns the number of clients added to the expected reclaim roster. When no
// recovery store is wired, or the store has no waitable records, this is a
// no-op and grace behaves exactly as on develop (empty roster -> skipped),
// which preserves the fresh-CI fast path. The hard grace timer remains the
// backstop regardless.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) LoadClientRecovery(ctx context.Context) int {
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
