package state

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"

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
	ErrLocked       = &NFS4StateError{Status: types.NFS4ERR_LOCKED, Message: "share reservation denies this I/O"}
)

// StateidOp identifies the operation family using a stateid. It controls how
// the two special stateids of RFC 7530 Section 9.1.4.3 are treated: on READ the
// all-ones "READ bypass" stateid skips share-mode enforcement, while on a
// write-family operation it is treated exactly like the anonymous stateid
// (RFC 7530 Section 16.36.4).
type StateidOp uint8

const (
	// StateidOpRead is a READ-family operation. The anonymous (all-zeros)
	// stateid is subject to the file's share-deny modes; the READ-bypass
	// (all-ones) stateid is not.
	StateidOpRead StateidOp = iota

	// StateidOpWrite is a write-family operation (WRITE, SETATTR size change,
	// LOCK). Both special stateids are subject to the file's share-deny modes.
	StateidOpWrite
)

// ============================================================================
// Stateid Generation
// ============================================================================

// generateStateidOther creates a 12-byte "other" field for a stateid.
//
// Layout:
//   - Byte 0:    state type tag (open=0x01, lock=0x02, deleg=0x03)
//   - Bytes 1-3: boot epoch fragment (low 24 bits of sm.bootEpoch)
//   - Bytes 4-11: 64 bits from crypto/rand
//
// The boot epoch fragment allows ValidateStateid to detect stale stateids
// from a previous server incarnation without a map lookup.
//
// The low eight bytes are random rather than sequential, so holding one
// stateid reveals nothing about any other.
//
// ponytail: uniqueness is probabilistic, around one chance in 40 million at a
// million concurrent stateids; retry against the by-other map at each mint
// site if a deployment ever holds enough live state to care.
func (sm *StateManager) generateStateidOther(stateType byte) [types.NFS4_OTHER_SIZE]byte {
	var other [types.NFS4_OTHER_SIZE]byte

	// Byte 0: type tag
	other[0] = stateType

	// Bytes 1-3: boot epoch fragment (low 24 bits)
	other[1] = byte(sm.bootEpoch >> 16)
	other[2] = byte(sm.bootEpoch >> 8)
	other[3] = byte(sm.bootEpoch)

	// Bytes 4-11: unpredictable. From Go 1.24 the default crypto/rand Reader
	// calls fatal() rather than returning an error, so a partial fill that
	// leaves guessable zeros here is not reachable and the error is dead. If
	// the module ever drops below Go 1.24 this must become a real error check.
	_, _ = rand.Read(other[4:])

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

// checkStateidOwner returns NFS4ERR_BAD_STATEID when a stateid names state
// owned by a client other than the caller, and nil when the caller may use it
// (RFC 8881 Section 18.38.3). A stateid is not a bearer token: a client
// presenting another client's stateid could otherwise write through that
// client's exclusive-deny open, or inside a byte range it holds an exclusive
// lock on.
//
// A zero callerClientID means the caller has no trusted client identity —
// NFSv4.0 carries no clientid4 on an I/O operation and has no session to
// derive one from — so the comparison is skipped and the stateid stays a
// bearer token there.
//
// That zero-skip is why the free*StateidLocked helpers below compare inline
// rather than calling this: FREE_STATEID rejects a mismatch whatever the
// caller's client ID, including zero. The two rules look alike and are not.
func checkStateidOwner(callerClientID, ownerClientID uint64) error {
	if callerClientID == 0 || callerClientID == ownerClientID {
		return nil
	}
	return &NFS4StateError{
		Status:  types.NFS4ERR_BAD_STATEID,
		Message: "stateid does not belong to the calling client",
	}
}

// stateidMissError classifies a stateid that names no live state: state freed
// when a lease was cancelled answers NFS4ERR_EXPIRED (RFC 7530 Section 9.6.3.2),
// a stateid minted by an earlier server incarnation answers
// NFS4ERR_STALE_STATEID, and anything else was never issued at all.
//
// A special stateid must be rejected before it reaches here. Its "other" is
// all-zeros or all-ones, so the boot-epoch fragment reads as some other
// incarnation's and the miss would be answered stale rather than bad.
//
// Caller must hold sm.mu (read or write).
func (sm *StateManager) stateidMissError(other [types.NFS4_OTHER_SIZE]byte) error {
	if sm.isExpiredStateidLocked(other) {
		return ErrExpired
	}
	if !sm.isCurrentEpoch(other) {
		return ErrStaleStateid
	}
	return ErrBadStateid
}

// checkStateidSeqid compares the seqid a client presented against the current
// seqid of the state its stateid names: an earlier one is NFS4ERR_OLD_STATEID
// and a later one NFS4ERR_BAD_STATEID (RFC 7530 Section 9.1.4). Seqid zero asks
// for "the most recent seqid" (RFC 8881 Section 8.2.2) and skips the comparison.
//
// An operation that also sequences an owner must compare here only AFTER the
// owner's own seqid check. A retransmission carries the pre-operation stateid,
// whose seqid is by then one behind, and RFC 7530 Section 9.1.7 wants it
// answered from the owner's reply cache rather than rejected as old.
func checkStateidSeqid(presented, current uint32) error {
	if presented == 0 || presented == current {
		return nil
	}
	if presented < current {
		return &NFS4StateError{
			Status:  types.NFS4ERR_OLD_STATEID,
			Message: fmt.Sprintf("stateid seqid %d < current %d", presented, current),
		}
	}
	return &NFS4StateError{
		Status:  types.NFS4ERR_BAD_STATEID,
		Message: fmt.Sprintf("stateid seqid %d > current %d", presented, current),
	}
}

// ValidateStateid validates a stateid for the given operation family and
// returns the associated OpenState.
//
// Per RFC 7530 Section 9.1.4, validation checks:
//  1. Special stateids: the anonymous (all-zeros) stateid carries no open
//     state, so it is checked against the share-deny modes of the opens that
//     do exist on the file; the READ-bypass (all-ones) stateid skips that
//     check on READ and is treated as the anonymous stateid everywhere else
//     (RFC 7530 Sections 9.1.4.3 and 16.36.4). Both return (nil, nil) when
//     accepted, and NFS4ERR_LOCKED when an open denies the access.
//  2. Route by type tag: open -> openStateByOther, lock -> lockStateByOther
//     (returns the parent open state), delegation -> delegByOther
//  3. If not found -> NFS4ERR_BAD_STATEID (or NFS4ERR_STALE_STATEID for wrong epoch)
//  4. Verify the state belongs to clientID, the caller's trusted client
//     identity -> NFS4ERR_BAD_STATEID. A zero clientID means the caller has
//     none (NFSv4.0) and skips the check; see checkStateidOwner.
//  5. Compare seqid: < current -> NFS4ERR_OLD_STATEID; > current -> NFS4ERR_BAD_STATEID
//  6. Verify filehandle matches (if provided and non-nil) -> NFS4ERR_BAD_STATEID
//  7. Check lease expiry and implicit renewal on success
//
// For delegation stateids (type 0x03), returns nil OpenState on success
// (same as special stateids). The caller's permission checks at the metadata
// layer (PrepareWrite) still apply.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) ValidateStateid(stateid *types.Stateid4, currentFH []byte, op StateidOp, clientID uint64) (*OpenState, error) {
	// Step 1: Special stateids.
	// The READ-bypass (all-ones) stateid asks the server to serve a READ even
	// where an open would deny it, so on READ it skips the share-deny check
	// below. On a write-family operation it carries no such licence: RFC 7530
	// Section 16.36.4 makes it behave exactly like the anonymous stateid.
	if stateid.IsReadBypassStateid() && op == StateidOpRead {
		return nil, nil
	}
	// Neither special stateid names an open state, so the share reservations
	// standing on the file are all the server has to judge the I/O by.
	if stateid.IsSpecialStateid() {
		if err := sm.anonymousIOBlocked(currentFH, op); err != nil {
			return nil, err
		}
		return nil, nil
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Step 2: Route by type tag in byte 0 of the "other" field
	stateType := stateid.Other[0]

	// Delegation stateids (type 0x03) are stored in delegByOther, not openStateByOther.
	if stateType == StateTypeDeleg {
		return sm.validateDelegStateid(stateid, currentFH, clientID)
	}

	// Lock stateids (type 0x02) are stored in lockStateByOther, never in
	// openStateByOther. RFC 7530 Section 9.1.4.1 permits a lock stateid on
	// READ/WRITE, so validate it against the lock state and return the parent
	// open state (whose share-access bits the caller enforces).
	if stateType == StateTypeLock {
		return sm.validateLockStateid(stateid, currentFH, clientID)
	}

	// Open stateids (type 0x01) use openStateByOther.
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return nil, sm.stateidMissError(stateid.Other)
	}

	// Step 4: the state must belong to the calling client.
	var ownerClientID uint64
	if openState.Owner != nil {
		ownerClientID = openState.Owner.ClientID
	}
	if err := checkStateidOwner(clientID, ownerClientID); err != nil {
		return nil, err
	}

	// Step 5: Compare seqid.
	if err := checkStateidSeqid(stateid.Seqid, openState.Stateid.Seqid); err != nil {
		return nil, err
	}

	// Step 6: Verify filehandle matches (if provided)
	if len(currentFH) > 0 && len(openState.FileHandle) > 0 {
		if !bytes.Equal(currentFH, openState.FileHandle) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: "stateid filehandle mismatch",
			}
		}
	}

	// Step 7: Lease check and implicit renewal
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
func (sm *StateManager) validateDelegStateid(stateid *types.Stateid4, currentFH []byte, clientID uint64) (*OpenState, error) {
	deleg, exists := sm.delegByOther[stateid.Other]
	if !exists {
		return nil, sm.stateidMissError(stateid.Other)
	}

	// Revoked delegations are no longer valid
	if deleg.Revoked {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_BAD_STATEID,
			Message: "delegation has been revoked",
		}
	}

	if err := checkStateidOwner(clientID, deleg.ClientID); err != nil {
		return nil, err
	}

	// Compare seqid.
	if err := checkStateidSeqid(stateid.Seqid, deleg.Stateid.Seqid); err != nil {
		return nil, err
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
func (sm *StateManager) validateLockStateid(stateid *types.Stateid4, currentFH []byte, clientID uint64) (*OpenState, error) {
	lockState, exists := sm.lockStateByOther[stateid.Other]
	if !exists {
		return nil, sm.stateidMissError(stateid.Other)
	}

	var ownerClientID uint64
	if lockState.LockOwner != nil {
		ownerClientID = lockState.LockOwner.ClientID
	}
	if err := checkStateidOwner(clientID, ownerClientID); err != nil {
		return nil, err
	}

	// Compare seqid.
	if err := checkStateidSeqid(stateid.Seqid, lockState.Stateid.Seqid); err != nil {
		return nil, err
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
	sm.removeOwnerLocksLocked(lockState)

	// Drop the lock-owner once nothing references it any more.
	sm.dropLockOwnerIfUnreferencedLocked(lockState.LockOwner)

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

// freeOpenStateidLocked answers FREE_STATEID for an open stateid.
// A live open is itself a held lock, so this always refuses.
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

	// An open stateid that is still in openStateByOther names a live open, and
	// RFC 8881 Section 18.38.3 counts an open among the "locks (of any kind)"
	// that make FREE_STATEID return NFS4ERR_LOCKS_HELD. CLOSE, not FREE_STATEID,
	// is what releases an open; freeing it here would drop the share
	// reservation while the client still believes it holds one.
	return &NFS4StateError{
		Status:  types.NFS4ERR_LOCKS_HELD,
		Message: fmt.Sprintf("open stateid is still open (%d lock stateids)", len(openState.LockStates)),
	}
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
// A stateid belonging to another client is reported as NFS4ERR_BAD_STATEID
// rather than as valid, so the operation cannot be used as an existence oracle
// for state the caller has no claim to; see checkStateidOwner.
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) TestStateids(stateids []types.Stateid4, callerClientID uint64) []uint32 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	results := make([]uint32, len(stateids))
	for i := range stateids {
		results[i] = sm.testSingleStateid(&stateids[i], callerClientID)
	}

	logger.Debug("TEST_STATEID: tested stateids",
		"count", len(stateids))

	return results
}

// testSingleStateid validates a single stateid without lease renewal.
// Returns the NFS4 status code for the stateid.
// Caller must hold sm.mu.RLock.
func (sm *StateManager) testSingleStateid(stateid *types.Stateid4, callerClientID uint64) uint32 {
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
		return sm.testOpenStateid(stateid, callerClientID)
	case StateTypeLock:
		return sm.testLockStateid(stateid, callerClientID)
	case StateTypeDeleg:
		return sm.testDelegStateid(stateid, callerClientID)
	default:
		return types.NFS4ERR_BAD_STATEID
	}
}

// testOpenStateid validates an open stateid without lease renewal.
// Caller must hold sm.mu.RLock.
func (sm *StateManager) testOpenStateid(stateid *types.Stateid4, callerClientID uint64) uint32 {
	openState, exists := sm.openStateByOther[stateid.Other]
	if !exists {
		return types.NFS4ERR_BAD_STATEID
	}
	if err := checkStateidOwner(callerClientID, openState.Owner.ClientID); err != nil {
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
func (sm *StateManager) testLockStateid(stateid *types.Stateid4, callerClientID uint64) uint32 {
	lockState, exists := sm.lockStateByOther[stateid.Other]
	if !exists {
		return types.NFS4ERR_BAD_STATEID
	}
	if err := checkStateidOwner(callerClientID, lockState.LockOwner.ClientID); err != nil {
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
func (sm *StateManager) testDelegStateid(stateid *types.Stateid4, callerClientID uint64) uint32 {
	deleg, exists := sm.delegByOther[stateid.Other]
	if !exists {
		return types.NFS4ERR_BAD_STATEID
	}
	if err := checkStateidOwner(callerClientID, deleg.ClientID); err != nil {
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

// anonymousIOBlocked reports NFS4ERR_LOCKED when a share reservation on the file
// denies an I/O issued under the anonymous stateid.
//
// RFC 7530 Section 9.1.4: "Regardless of whether an anonymous stateid or a
// stateid returned by the server is used, if there is a conflicting share
// reservation or mandatory byte-range lock held on the file, the server MUST
// refuse to service the READ or WRITE operation ... Share reservations are
// established by OPEN operations and by their nature are mandatory in that when
// the OPEN denies READ or WRITE operations, that denial results in such
// operations being rejected with error NFS4ERR_LOCKED."
//
// Without this a deny mode was advisory: it refused a conflicting OPEN but not
// the I/O of a client that skipped OPEN and used the anonymous stateid, which is
// the case the deny mode exists to stop. Linux nfsd applies the same rule in
// check_special_stateids.
//
// The READ-bypass stateid reaches this on a write-family operation, where
// RFC 7530 Section 16.36.4 makes it behave exactly like the anonymous stateid.
// On READ it does not: bypassing this check is the whole point of it.
func (sm *StateManager) anonymousIOBlocked(currentFH []byte, op StateidOp) error {
	if len(currentFH) == 0 {
		return nil
	}

	// The I/O is judged exactly as an OPEN requesting that access would be, so
	// it goes through the same conflict test rather than a second copy of the
	// rule: with no deny of its own to assert, only the "requested access is
	// denied by an existing open" half can fire.
	access := uint32(types.OPEN4_SHARE_ACCESS_READ)
	if op == StateidOpWrite {
		access = types.OPEN4_SHARE_ACCESS_WRITE
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.shareConflictLocked(currentFH, access, types.OPEN4_SHARE_DENY_NONE) {
		return ErrLocked
	}
	return nil
}
