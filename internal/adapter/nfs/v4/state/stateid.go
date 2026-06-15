package state

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sync/atomic"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// State Type Constants
// ============================================================================

// State type tags used as byte 0 of the stateid "other" field.
// These allow the server to quickly determine what kind of state
// a stateid refers to without a map lookup.
const (
	// StateTypeOpen identifies an open stateid (created by OPEN, removed by CLOSE).
	StateTypeOpen byte = 0x01

	// StateTypeLock identifies a lock stateid (created by LOCK, removed by LOCKU).
	StateTypeLock byte = 0x02

	// StateTypeDeleg identifies a delegation stateid (created by OPEN delegation grant).
	StateTypeDeleg byte = 0x03
)

// ============================================================================
// NFS4StateError
// ============================================================================

// NFS4StateError is an error type that carries an NFS4 status code.
// Handlers map this to the appropriate wire response.
type NFS4StateError struct {
	Status  uint32
	Message string
}

func (e *NFS4StateError) Error() string {
	return e.Message
}

// Common state errors used throughout the state package.
var (
	ErrBadStateid   = &NFS4StateError{Status: types.NFS4ERR_BAD_STATEID, Message: "bad stateid"}
	ErrOldStateid   = &NFS4StateError{Status: types.NFS4ERR_OLD_STATEID, Message: "old stateid"}
	ErrStaleStateid = &NFS4StateError{Status: types.NFS4ERR_STALE_STATEID, Message: "stale stateid"}
	ErrExpired      = &NFS4StateError{Status: types.NFS4ERR_EXPIRED, Message: "lease expired"}
	ErrBadSeqid     = &NFS4StateError{Status: types.NFS4ERR_BAD_SEQID, Message: "bad seqid"}
	ErrShareDenied  = &NFS4StateError{Status: types.NFS4ERR_SHARE_DENIED, Message: "share reservation conflict"}
)

// StateidOp identifies the operation family using a stateid. It controls which
// special stateids are accepted: per RFC 7530 Section 9.1.4.3 the all-ones
// "READ bypass" stateid is valid only on READ; using it on a write-family
// operation (WRITE / SETATTR-size / LOCK) MUST yield NFS4ERR_BAD_STATEID.
type StateidOp uint8

const (
	// StateidOpRead is a READ-family operation. Both the anonymous (all-zeros)
	// and the READ-bypass (all-ones) special stateids are permitted.
	StateidOpRead StateidOp = iota

	// StateidOpWrite is a write-family operation (WRITE, SETATTR size change,
	// LOCK). The anonymous stateid is permitted; the READ-bypass stateid is not.
	StateidOpWrite
)

// ============================================================================
// Stateid Generation
// ============================================================================

// generateStateidOther creates a unique 12-byte "other" field for a stateid.
//
// Layout:
//   - Byte 0:    state type tag (open=0x01, lock=0x02, deleg=0x03)
//   - Bytes 1-3: boot epoch fragment (low 24 bits of sm.bootEpoch)
//   - Bytes 4-11: atomic sequence counter (8 bytes, big-endian)
//
// The boot epoch fragment allows ValidateStateid to detect stale stateids
// from a previous server incarnation without a map lookup.
func (sm *StateManager) generateStateidOther(stateType byte) [types.NFS4_OTHER_SIZE]byte {
	var other [types.NFS4_OTHER_SIZE]byte

	// Byte 0: type tag
	other[0] = stateType

	// Bytes 1-3: boot epoch fragment (low 24 bits)
	other[1] = byte(sm.bootEpoch >> 16)
	other[2] = byte(sm.bootEpoch >> 8)
	other[3] = byte(sm.bootEpoch)

	// Bytes 4-11: monotonic sequence counter
	seq := atomic.AddUint64(&sm.nextStateSeq, 1)
	other[4] = byte(seq >> 56)
	other[5] = byte(seq >> 48)
	other[6] = byte(seq >> 40)
	other[7] = byte(seq >> 32)
	other[8] = byte(seq >> 24)
	other[9] = byte(seq >> 16)
	other[10] = byte(seq >> 8)
	other[11] = byte(seq)

	return other
}

// isCurrentEpoch checks whether the boot epoch fragment in a stateid's
// "other" field matches the current server boot epoch.
func (sm *StateManager) isCurrentEpoch(other [types.NFS4_OTHER_SIZE]byte) bool {
	epochBytes := [3]byte{
		byte(sm.bootEpoch >> 16),
		byte(sm.bootEpoch >> 8),
		byte(sm.bootEpoch),
	}
	return other[1] == epochBytes[0] &&
		other[2] == epochBytes[1] &&
		other[3] == epochBytes[2]
}

// ============================================================================
// Stateid Validation
// ============================================================================

// ValidateStateid validates a stateid for the given operation family and
// returns the associated OpenState.
//
// Per RFC 7530 Section 9.1.4, validation checks:
//  1. Special stateids: the anonymous (all-zeros) stateid bypasses validation
//     on any op; the READ-bypass (all-ones) stateid is accepted ONLY when
//     op == StateidOpRead and otherwise rejected with NFS4ERR_BAD_STATEID
//     (RFC 7530 Section 9.1.4.3). Both return (nil, nil) when accepted.
//  2. Route by type tag: open -> openStateByOther, lock -> lockStateByOther
//     (returns the parent open state), delegation -> delegByOther
//  3. If not found -> NFS4ERR_BAD_STATEID (or NFS4ERR_STALE_STATEID for wrong epoch)
//  4. Compare seqid: < current -> NFS4ERR_OLD_STATEID; > current -> NFS4ERR_BAD_STATEID
//  5. Verify filehandle matches (if provided and non-nil) -> NFS4ERR_BAD_STATEID
//  6. Check lease expiry and implicit renewal on success
//
// For delegation stateids (type 0x03), returns nil OpenState on success
// (same as special stateids). The caller's permission checks at the metadata
// layer (PrepareWrite) still apply.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) ValidateStateid(stateid *types.Stateid4, currentFH []byte, op StateidOp) (*OpenState, error) {
	// Step 1: Special stateids.
	// The READ-bypass (all-ones) stateid is READ-only: reject it on
	// write-family operations so a client cannot use it to bypass share-mode
	// enforcement and byte-range locks (RFC 7530 Section 9.1.4.3).
	if stateid.IsReadBypassStateid() {
		if op != StateidOpRead {
			return nil, ErrBadStateid
		}
		return nil, nil
	}
	// The anonymous (all-zeros) stateid is permitted on READ and WRITE.
	if stateid.IsAnonymousStateid() {
		return nil, nil
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Step 2: Route by type tag in byte 0 of the "other" field
	stateType := stateid.Other[0]

	// Delegation stateids (type 0x03) are stored in delegByOther, not openStateByOther.
	if stateType == StateTypeDeleg {
		return sm.validateDelegStateid(stateid, currentFH)
	}

	// Lock stateids (type 0x02) are stored in lockStateByOther, never in
	// openStateByOther. RFC 7530 Section 9.1.4.1 permits a lock stateid on
	// READ/WRITE, so validate it against the lock state and return the parent
	// open state (whose share-access bits the caller enforces).
	if stateType == StateTypeLock {
		return sm.validateLockStateid(stateid, currentFH)
	}

	// Open stateids (type 0x01) use openStateByOther.
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		if !sm.isCurrentEpoch(stateid.Other) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_STALE_STATEID,
				Message: "stateid from previous server incarnation",
			}
		}
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "stateid not found",
		}
	}

	// Step 4: Compare seqid.
	// Per RFC 8881 Section 8.2.2 (NFSv4.1): if the client sends seqid=0
	// in a non-special stateid, the server MUST accept it regardless of
	// the current seqid value.  This is safe for v4.0 too since v4.0
	// clients never legitimately send seqid=0 for real stateids.
	if stateid.Seqid != 0 {
		if stateid.Seqid < openState.Stateid.Seqid {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_OLD_STATEID,
				Message: fmt.Sprintf("stateid seqid %d < current %d", stateid.Seqid, openState.Stateid.Seqid),
			}
		}
		if stateid.Seqid > openState.Stateid.Seqid {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: fmt.Sprintf("stateid seqid %d > current %d", stateid.Seqid, openState.Stateid.Seqid),
			}
		}
	}

	// Step 5: Verify filehandle matches (if provided)
	if len(currentFH) > 0 && len(openState.FileHandle) > 0 {
		if !bytes.Equal(currentFH, openState.FileHandle) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: "stateid filehandle mismatch",
			}
		}
	}

	// Step 6: Lease check and implicit renewal
	// Per RFC 7530 Section 9.6: any operation that uses a stateid implicitly
	// renews the lease for the associated client. This prevents READ-only
	// clients from having their state expire (Pitfall 3).
	if openState.Owner != nil && openState.Owner.ClientRecord != nil {
		lease := openState.Owner.ClientRecord.Lease
		if lease != nil {
			if lease.IsExpired() {
				return nil, ErrExpired
			}
			lease.Renew()
		}
	}

	return openState, nil
}

// validateDelegStateid validates a delegation stateid (type 0x03).
// Returns nil OpenState on success (delegation validated, caller should proceed).
// Caller must hold sm.mu.RLock.
func (sm *StateManager) validateDelegStateid(stateid *types.Stateid4, currentFH []byte) (*OpenState, error) {
	deleg, exists := sm.delegByOther[stateid.Other]
	if !exists {
		if !sm.isCurrentEpoch(stateid.Other) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_STALE_STATEID,
				Message: "stateid from previous server incarnation",
			}
		}
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "delegation stateid not found",
		}
	}

	// Revoked delegations are no longer valid
	if deleg.Revoked {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "delegation has been revoked",
		}
	}

	// Compare seqid (seqid=0 means "any" per RFC 8881 Section 8.2.2)
	if stateid.Seqid != 0 {
		if stateid.Seqid < deleg.Stateid.Seqid {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_OLD_STATEID,
				Message: fmt.Sprintf("delegation stateid seqid %d < current %d", stateid.Seqid, deleg.Stateid.Seqid),
			}
		}
		if stateid.Seqid > deleg.Stateid.Seqid {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: fmt.Sprintf("delegation stateid seqid %d > current %d", stateid.Seqid, deleg.Stateid.Seqid),
			}
		}
	}

	// Verify filehandle matches
	if len(currentFH) > 0 && len(deleg.FileHandle) > 0 {
		if !bytes.Equal(currentFH, deleg.FileHandle) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: "delegation stateid filehandle mismatch",
			}
		}
	}

	// Implicit lease renewal for the delegation's client
	client, clientExists := sm.clientsByID[deleg.ClientID]
	if clientExists && client.Lease != nil {
		if client.Lease.IsExpired() {
			return nil, ErrExpired
		}
		client.Lease.Renew()
	}

	return nil, nil
}

// validateLockStateid validates a lock stateid (type 0x02) presented to an
// I/O operation (READ/WRITE/SETATTR-size). Lock stateids live in
// lockStateByOther; on success this returns the parent OpenState so the caller
// can enforce that open's share-access mode (RFC 7530 Section 9.1.4.1).
// Caller must hold sm.mu.RLock.
func (sm *StateManager) validateLockStateid(stateid *types.Stateid4, currentFH []byte) (*OpenState, error) {
	lockState, exists := sm.lockStateByOther[stateid.Other]
	if !exists {
		if !sm.isCurrentEpoch(stateid.Other) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_STALE_STATEID,
				Message: "stateid from previous server incarnation",
			}
		}
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "lock stateid not found",
		}
	}

	// Compare seqid. Per RFC 8881 Section 8.2.2, seqid=0 in a non-special
	// stateid bypasses the seqid comparison entirely.
	if stateid.Seqid != 0 {
		if stateid.Seqid < lockState.Stateid.Seqid {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_OLD_STATEID,
				Message: fmt.Sprintf("lock stateid seqid %d < current %d", stateid.Seqid, lockState.Stateid.Seqid),
			}
		}
		if stateid.Seqid > lockState.Stateid.Seqid {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: fmt.Sprintf("lock stateid seqid %d > current %d", stateid.Seqid, lockState.Stateid.Seqid),
			}
		}
	}

	// Verify filehandle matches the locked file.
	if len(currentFH) > 0 && len(lockState.FileHandle) > 0 {
		if !bytes.Equal(currentFH, lockState.FileHandle) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: "lock stateid filehandle mismatch",
			}
		}
	}

	openState := lockState.OpenState
	if openState == nil {
		// A lock stateid must always derive from an open; a nil parent is an
		// internal invariant violation, not a client error.
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_SERVERFAULT,
			Message: "lock stateid has no parent open state",
		}
	}

	// Implicit lease renewal for the owning client (RFC 7530 Section 9.6).
	if openState.Owner != nil && openState.Owner.ClientRecord != nil {
		lease := openState.Owner.ClientRecord.Lease
		if lease != nil {
			if lease.IsExpired() {
				return nil, ErrExpired
			}
			lease.Renew()
		}
	}

	return openState, nil
}

// ============================================================================
// FreeStateid (RFC 8881 Section 18.38)
// ============================================================================

// isSpecialOther returns true if the other field is all-zeros or all-ones.
func isSpecialOther(other [types.NFS4_OTHER_SIZE]byte) bool {
	allZeros := true
	allOnes := true
	for _, b := range other {
		if b != 0x00 {
			allZeros = false
		}
		if b != 0xFF {
			allOnes = false
		}
		if !allZeros && !allOnes {
			return false
		}
	}
	return true
}

// FreeStateid implements the NFSv4.1 FREE_STATEID operation per
// RFC 8881 Section 18.38.
//
// It releases a stateid that is no longer needed by the client. The operation
// handles lock, open, and delegation stateids with appropriate guards:
//   - Lock stateids are removed directly
//   - Open stateids are rejected with NFS4ERR_LOCKS_HELD if locks exist
//   - Delegation stateids are removed directly
//   - Special stateids (all-zeros, all-ones) return NFS4ERR_BAD_STATEID
//
// No cache flush is triggered (trusts existing COMMIT/cache/WAL flow).
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) FreeStateid(clientID uint64, stateid *types.Stateid4) error {
	// Reject special stateids
	if isSpecialOther(stateid.Other) {
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "cannot free special stateid",
		}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	stateType := stateid.Other[0]

	switch stateType {
	case StateTypeLock:
		return sm.freeLockStateidLocked(clientID, stateid)
	case StateTypeOpen:
		return sm.freeOpenStateidLocked(clientID, stateid)
	case StateTypeDeleg:
		return sm.freeDelegStateidLocked(clientID, stateid)
	default:
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: fmt.Sprintf("unknown stateid type %d", stateType),
		}
	}
}

// freeLockStateidLocked frees a lock stateid.
// Caller must hold sm.mu.
func (sm *StateManager) freeLockStateidLocked(clientID uint64, stateid *types.Stateid4) error {
	lockState, exists := sm.lockStateByOther[stateid.Other]
	if !exists {
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "lock stateid not found",
		}
	}

	// RFC 8881 Section 18.38.3: the stateid must belong to the requesting
	// session's client. Reject cross-client frees so a peer that learns
	// another client's stateid.Other bytes cannot destroy its locks.
	if lockState.LockOwner == nil || lockState.LockOwner.ClientID != clientID {
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "lock stateid does not belong to client",
		}
	}

	// Remove from lockStateByOther
	delete(sm.lockStateByOther, stateid.Other)

	// Remove actual locks from unified lock manager
	if sm.lockManager != nil && lockState.LockOwner != nil {
		ownerID := lockState.LockOwner.LockManagerOwnerID()
		handleKey := string(lockState.FileHandle)
		for _, l := range sm.lockManager.ListUnifiedLocks(handleKey) {
			if l.Owner.OwnerID == ownerID {
				_ = sm.lockManager.RemoveUnifiedLock(handleKey, l.Owner, l.Offset, l.Length)
			}
		}
	}

	// Remove lock-owner from lockOwners map only when no other LockState
	// still references this owner (reference-count to zero). A LockOwner can
	// be shared across multiple LockState entries (one per open-state/file),
	// so deleting it on the first free would blind replay-detection and the
	// owner caches for the still-live lock states.
	if lockState.LockOwner != nil {
		ownerStillReferenced := false
		for _, ls := range sm.lockStateByOther {
			if ls.LockOwner == lockState.LockOwner {
				ownerStillReferenced = true
				break
			}
		}
		if !ownerStillReferenced {
			delete(sm.lockOwners, lockState.LockOwner.Key())
		}
	}

	// Remove from parent open state's LockStates slice
	if lockState.OpenState != nil {
		for i, ls := range lockState.OpenState.LockStates {
			if ls == lockState {
				lockState.OpenState.LockStates = append(
					lockState.OpenState.LockStates[:i],
					lockState.OpenState.LockStates[i+1:]...,
				)
				break
			}
		}
	}

	logger.Info("FREE_STATEID: lock stateid freed",
		"client_id", clientID,
		"stateid_other", hex.EncodeToString(stateid.Other[:]))

	return nil
}

// freeOpenStateidLocked frees an open stateid.
// Returns NFS4ERR_LOCKS_HELD if the open has associated locks.
// Caller must hold sm.mu.
func (sm *StateManager) freeOpenStateidLocked(clientID uint64, stateid *types.Stateid4) error {
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "open stateid not found",
		}
	}

	// RFC 8881 Section 18.38.3: the stateid must belong to the requesting
	// session's client.
	if openState.Owner == nil || openState.Owner.ClientID != clientID {
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "open stateid does not belong to client",
		}
	}

	// Check if any lock stateids reference this open
	if len(openState.LockStates) > 0 {
		return &NFS4StateError{
			Status:  types.NFS4ERR_LOCKS_HELD,
			Message: fmt.Sprintf("open stateid has %d locks held", len(openState.LockStates)),
		}
	}

	// Remove from openStateByOther and the per-file index
	delete(sm.openStateByOther, stateid.Other)
	sm.removeOpenStateFromFileLocked(openState)

	// Remove from owner's OpenStates slice
	if openState.Owner != nil {
		for i, os := range openState.Owner.OpenStates {
			if os == openState {
				openState.Owner.OpenStates = append(
					openState.Owner.OpenStates[:i],
					openState.Owner.OpenStates[i+1:]...,
				)
				break
			}
		}

		// If owner has no more open states, clean up the owner
		if len(openState.Owner.OpenStates) == 0 {
			delete(sm.openOwners, openState.Owner.Key())
		}
	}

	logger.Info("FREE_STATEID: open stateid freed",
		"client_id", clientID,
		"stateid_other", hex.EncodeToString(stateid.Other[:]))

	return nil
}

// freeDelegStateidLocked frees a delegation stateid.
// Caller must hold sm.mu.
func (sm *StateManager) freeDelegStateidLocked(clientID uint64, stateid *types.Stateid4) error {
	deleg, exists := sm.delegByOther[stateid.Other]
	if !exists {
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "delegation stateid not found",
		}
	}

	// RFC 8881 Section 18.38.3: the stateid must belong to the requesting
	// session's client.
	if deleg.ClientID != clientID {
		return &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "delegation stateid does not belong to client",
		}
	}

	// Stop recall timer if running
	deleg.StopRecallTimer()

	// Remove from delegByOther (keeps revoked-delegation index consistent)
	sm.deleteDelegByOtherLocked(stateid.Other)

	// Remove from delegByFile
	sm.removeDelegFromFile(deleg)

	logger.Info("FREE_STATEID: delegation stateid freed",
		"client_id", clientID,
		"stateid_other", hex.EncodeToString(stateid.Other[:]))

	return nil
}

// ============================================================================
// TestStateids (RFC 8881 Section 18.48)
// ============================================================================

// TestStateids implements the NFSv4.1 TEST_STATEID operation per
// RFC 8881 Section 18.48.
//
// It validates an array of stateids and returns per-stateid NFS4 status codes.
// This is a read-only operation with no side effects: it does NOT renew leases
// (Pitfall 5 from research).
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) TestStateids(stateids []types.Stateid4) []uint32 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	results := make([]uint32, len(stateids))
	for i := range stateids {
		results[i] = sm.testSingleStateid(&stateids[i])
	}

	logger.Debug("TEST_STATEID: tested stateids",
		"count", len(stateids))

	return results
}

// testSingleStateid validates a single stateid without lease renewal.
// Returns the NFS4 status code for the stateid.
// Caller must hold sm.mu.RLock.
func (sm *StateManager) testSingleStateid(stateid *types.Stateid4) uint32 {
	// Special stateids are always valid
	if stateid.IsSpecialStateid() {
		return types.NFS4_OK
	}

	// Check boot epoch first
	if !sm.isCurrentEpoch(stateid.Other) {
		return types.NFS4ERR_STALE_STATEID
	}

	stateType := stateid.Other[0]

	switch stateType {
	case StateTypeOpen:
		return sm.testOpenStateid(stateid)
	case StateTypeLock:
		return sm.testLockStateid(stateid)
	case StateTypeDeleg:
		return sm.testDelegStateid(stateid)
	default:
		return types.NFS4ERR_BAD_STATEID
	}
}

// testOpenStateid validates an open stateid without lease renewal.
// Caller must hold sm.mu.RLock.
func (sm *StateManager) testOpenStateid(stateid *types.Stateid4) uint32 {
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return types.NFS4ERR_BAD_STATEID
	}

	// Check seqid (seqid=0 means "any" per RFC 8881 Section 8.2.2)
	if stateid.Seqid != 0 {
		if stateid.Seqid < openState.Stateid.Seqid {
			return types.NFS4ERR_OLD_STATEID
		}
		if stateid.Seqid > openState.Stateid.Seqid {
			return types.NFS4ERR_BAD_STATEID
		}
	}

	// Check lease expiry WITHOUT renewal (read-only test)
	if openState.Owner != nil && openState.Owner.ClientRecord != nil {
		lease := openState.Owner.ClientRecord.Lease
		if lease != nil && lease.IsExpired() {
			return types.NFS4ERR_EXPIRED
		}
	}

	return types.NFS4_OK
}

// testLockStateid validates a lock stateid without lease renewal.
// Caller must hold sm.mu.RLock.
func (sm *StateManager) testLockStateid(stateid *types.Stateid4) uint32 {
	lockState, exists := sm.lockStateByOther[stateid.Other]
	if !exists {
		return types.NFS4ERR_BAD_STATEID
	}

	// Check seqid (seqid=0 means "any" per RFC 8881 Section 8.2.2)
	if stateid.Seqid != 0 {
		if stateid.Seqid < lockState.Stateid.Seqid {
			return types.NFS4ERR_OLD_STATEID
		}
		if stateid.Seqid > lockState.Stateid.Seqid {
			return types.NFS4ERR_BAD_STATEID
		}
	}

	return types.NFS4_OK
}

// testDelegStateid validates a delegation stateid without lease renewal.
// Caller must hold sm.mu.RLock.
func (sm *StateManager) testDelegStateid(stateid *types.Stateid4) uint32 {
	deleg, exists := sm.delegByOther[stateid.Other]
	if !exists {
		return types.NFS4ERR_BAD_STATEID
	}

	if deleg.Revoked {
		return types.NFS4ERR_BAD_STATEID
	}

	// Check seqid (seqid=0 means "any" per RFC 8881 Section 8.2.2)
	if stateid.Seqid != 0 {
		if stateid.Seqid < deleg.Stateid.Seqid {
			return types.NFS4ERR_OLD_STATEID
		}
		if stateid.Seqid > deleg.Stateid.Seqid {
			return types.NFS4ERR_BAD_STATEID
		}
	}

	return types.NFS4_OK
}
