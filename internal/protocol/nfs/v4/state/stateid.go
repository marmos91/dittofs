package state

import (
	"bytes"
	"fmt"
	"sync/atomic"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
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
	// Reserved for Phase 10.
	StateTypeLock byte = 0x02

	// StateTypeDeleg identifies a delegation stateid (created by OPEN delegation grant).
	// Reserved for Phase 11.
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

// ValidateStateid validates a stateid and returns the associated OpenState.
//
// Per RFC 7530 Section 9.1.4, validation checks:
//  1. Special stateids bypass validation (return nil, nil)
//  2. Route by type tag: open stateids -> openStateByOther, delegation -> delegByOther
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
func (sm *StateManager) ValidateStateid(stateid *types.Stateid4, currentFH []byte) (*OpenState, error) {
	// Step 1: Special stateids bypass validation
	if stateid.IsSpecialStateid() {
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

	// Open stateids (type 0x01) and lock stateids (type 0x02) use openStateByOther
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

	// Step 4: Compare seqid
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

	// Compare seqid
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
