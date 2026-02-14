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
//  2. Look up in openStateByOther map
//  3. If not found -> NFS4ERR_BAD_STATEID
//  4. Check boot epoch matches -> NFS4ERR_STALE_STATEID
//  5. Compare seqid: < current -> NFS4ERR_OLD_STATEID; > current -> NFS4ERR_BAD_STATEID
//  6. Verify filehandle matches (if provided and non-nil) -> NFS4ERR_BAD_STATEID
//  7. Check lease expiry (placeholder for Phase 9-04)
//  8. Implicit lease renewal on success
//
// Caller must NOT hold sm.mu.
func (sm *StateManager) ValidateStateid(stateid *types.Stateid4, currentFH []byte) (*OpenState, error) {
	// Step 1: Special stateids bypass validation
	if stateid.IsSpecialStateid() {
		return nil, nil
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Step 2-4: Look up by "other" field, checking boot epoch if not found
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

	// Step 5: Compare seqid
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

	// Step 6: Verify filehandle matches (if provided)
	if currentFH != nil && len(currentFH) > 0 && len(openState.FileHandle) > 0 {
		if !bytes.Equal(currentFH, openState.FileHandle) {
			return nil, &NFS4StateError{
				Status:  types.NFS4ERR_BAD_STATEID,
				Message: "stateid filehandle mismatch",
			}
		}
	}

	// Step 7-8: Lease check and implicit renewal
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
