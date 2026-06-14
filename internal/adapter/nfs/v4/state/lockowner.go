package state

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// Lock Owner
// ============================================================================

// LockOwner represents an NFSv4 lock-owner for state tracking.
// A lock-owner is identified by the combination of clientid + owner opaque data.
// Each lock-owner has an independent seqid sequence for replay detection.
//
// Per RFC 7530 Section 9.4, lock-owners are analogous to open-owners but
// track byte-range lock state rather than open state.
type LockOwner struct {
	// ClientID is the server-assigned client identifier.
	ClientID uint64

	// OwnerData is the opaque owner identifier from the client.
	OwnerData []byte

	// LastSeqID is the last successfully processed seqid for this owner.
	LastSeqID uint32

	// LastResult is the cached result of the last operation on this owner.
	// Used for replay detection (same seqid returns cached result).
	LastResult *CachedResult

	// ClientRecord is a back-reference to the owning client record.
	ClientRecord *ClientRecord

	// key is the precomputed composite map key (clientID + hex(ownerData)),
	// cached at creation so hot-path lookups reuse it instead of re-running
	// makeLockOwnerKey. ClientID/OwnerData are immutable after creation.
	key lockOwnerKey
}

// Key returns the cached composite map key for this lock-owner. Owners
// constructed via the StateManager always carry a cached key; the fallback
// covers literal-built instances (e.g. in tests).
func (lo *LockOwner) Key() lockOwnerKey {
	if lo.key == "" {
		return makeLockOwnerKey(lo.ClientID, lo.OwnerData)
	}
	return lo.key
}

// ValidateSeqID checks whether a seqid is valid for this lock-owner.
//
// Uses the same algorithm as OpenOwner.ValidateSeqID:
//   - Expected = LastSeqID + 1 (with wrap: 0xFFFFFFFF -> 1, not 0)
//   - seqid == expected -> SeqIDOK
//   - seqid == LastSeqID -> SeqIDReplay
//   - else -> SeqIDBad
func (lo *LockOwner) ValidateSeqID(seqid uint32) SeqIDValidation {
	expected := nextSeqID(lo.LastSeqID)

	if seqid == expected {
		return SeqIDOK
	}
	if seqid == lo.LastSeqID {
		return SeqIDReplay
	}
	return SeqIDBad
}

// ============================================================================
// Lock State
// ============================================================================

// LockState represents the state of a lock-owner on a specific open file.
// Created by LOCK with new_lock_owner=true, removed by RELEASE_LOCKOWNER.
//
// Per RFC 7530 Section 9.4, there is one lock stateid per (lock-owner, open-state) pair.
type LockState struct {
	// Stateid is the server-assigned state identifier for this lock state.
	Stateid types.Stateid4

	// LockOwner is the lock-owner that created this state.
	LockOwner *LockOwner

	// OpenState is the open state this lock is derived from.
	OpenState *OpenState

	// FileHandle is the file handle of the locked file.
	FileHandle []byte
}

// ============================================================================
// Lock Owner Key
// ============================================================================

// lockOwnerKey is a composite key for looking up lock-owners in maps.
// It combines the client ID and hex-encoded owner data for uniqueness.
type lockOwnerKey string

// makeLockOwnerKey creates a lockOwnerKey from a client ID and owner data.
func makeLockOwnerKey(clientID uint64, ownerData []byte) lockOwnerKey {
	return lockOwnerKey(fmt.Sprintf("%d:%s", clientID, hex.EncodeToString(ownerData)))
}

// lockManagerOwnerIDPrefix is the namespace prefix the StateManager stamps onto
// every LockManager owner ID so NFSv4 byte-range locks are distinguishable from
// other protocols (e.g. SMB) sharing the unified lock map.
const lockManagerOwnerIDPrefix = "nfs4:"

// lockManagerOwnerID builds the LockManager owner ID for an NFSv4 lock-owner.
// It is exactly lockManagerOwnerIDPrefix + makeLockOwnerKey(...) so the lock
// manager identity stays in lock-step with the internal lock-owner map key:
// callers that have a (clientID, ownerData) pair but no *LockOwner use this.
func lockManagerOwnerID(clientID uint64, ownerData []byte) string {
	return lockManagerOwnerIDPrefix + string(makeLockOwnerKey(clientID, ownerData))
}

// LockManagerOwnerID returns the LockManager owner ID for this lock-owner,
// reusing the cached map key instead of recomputing the hex encoding. It agrees
// byte-for-byte with the free lockManagerOwnerID(clientID, ownerData) helper.
// Lock-owners constructed via the StateManager always have a cached key; the
// fallback covers literal-built instances (e.g. in tests) so the method is
// always correct.
func (lo *LockOwner) LockManagerOwnerID() string {
	if lo.key == "" {
		return lockManagerOwnerID(lo.ClientID, lo.OwnerData)
	}
	return lockManagerOwnerIDPrefix + string(lo.key)
}

// ============================================================================
// Lock Result Types
// ============================================================================

// LockResult is the result returned by StateManager.LockNew/LockExisting.
// On success, Stateid is set and Denied is nil.
// On conflict, Denied is set with the conflicting lock details.
type LockResult struct {
	// Stateid is the lock stateid (set on success).
	Stateid types.Stateid4

	// Denied is the conflict information (set on NFS4ERR_DENIED).
	// Nil on success.
	Denied *LOCK4denied

	// OwnerClientID and OwnerData identify the lock-owner whose seqid this op
	// advanced. The handler uses them to cache the encoded reply for replay
	// detection (CacheLockOwnerResult) on both success and DENIED outcomes.
	OwnerClientID uint64
	OwnerData     []byte
}

// LOCK4denied describes a conflicting lock for NFS4ERR_DENIED responses.
// Per RFC 7530 Section 16.10.4:
//
//	struct LOCK4denied {
//	    offset4      offset;
//	    length4      length;
//	    nfs_lock_type4 locktype;
//	    lock_owner4  owner;
//	};
type LOCK4denied struct {
	Offset   uint64
	Length   uint64
	LockType uint32
	Owner    struct {
		ClientID  uint64
		OwnerData []byte
	}
}

// EncodeLOCK4denied encodes a LOCK4denied structure in XDR format.
func EncodeLOCK4denied(buf *bytes.Buffer, denied *LOCK4denied) {
	_ = xdr.WriteUint64(buf, denied.Offset)
	_ = xdr.WriteUint64(buf, denied.Length)
	_ = xdr.WriteUint32(buf, denied.LockType)
	_ = xdr.WriteUint64(buf, denied.Owner.ClientID)
	_ = xdr.WriteXDROpaque(buf, denied.Owner.OwnerData)
}

// ============================================================================
// Validation Helpers
// ============================================================================

// validateOpenModeForLock checks that the open state's share_access mode
// is compatible with the requested lock type.
//
// Per RFC 7530 Section 16.10.5:
//   - WRITE_LT / WRITEW_LT requires OPEN4_SHARE_ACCESS_WRITE
//   - READ_LT / READW_LT requires OPEN4_SHARE_ACCESS_READ
//
// Returns NFS4ERR_OPENMODE on mismatch.
func validateOpenModeForLock(openState *OpenState, lockType uint32) error {
	switch lockType {
	case types.WRITE_LT, types.WRITEW_LT:
		if openState.ShareAccess&types.OPEN4_SHARE_ACCESS_WRITE == 0 {
			return &NFS4StateError{
				Status:  types.NFS4ERR_OPENMODE,
				Message: "write lock requires OPEN4_SHARE_ACCESS_WRITE",
			}
		}
	case types.READ_LT, types.READW_LT:
		if openState.ShareAccess&types.OPEN4_SHARE_ACCESS_READ == 0 {
			return &NFS4StateError{
				Status:  types.NFS4ERR_OPENMODE,
				Message: "read lock requires OPEN4_SHARE_ACCESS_READ",
			}
		}
	}
	return nil
}
