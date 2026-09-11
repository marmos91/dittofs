package state

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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

	// ownerSeq carries this owner's seqid sequence and replay cache.
	ownerSeq

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

// NFSLockClientIdentity builds the LockManager client identity for an NFSv4
// client. Every row this StateManager puts in the unified lock map — byte-range
// locks and delegations alike — carries it, so break paths that exclude by
// client can tell a client's own state from another client's. The v4 handlers
// build the same identity for the auth context's LockClientID, so the metadata
// layer's originator exclusion recognizes the delegation holder's own
// mutations.
func NFSLockClientIdentity(clientID uint64) string {
	return fmt.Sprintf("%s%d", lockManagerOwnerIDPrefix, clientID)
}

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

// EncodeLOCK4denied encodes the LOCK4denied structure in XDR format.
//
// A stored length of zero is the lock manager's spelling for "to end-of-file";
// on the wire that range is a length with every bit set. Zero is not a length a
// client may send, so emitting it would name a range the client cannot parse
// back into the lock it was denied by.
func EncodeLOCK4denied(buf *bytes.Buffer, denied *LOCK4denied) {
	length := denied.Length
	if length == 0 {
		length = math.MaxUint64
	}
	_ = xdr.WriteUint64(buf, denied.Offset)
	_ = xdr.WriteUint64(buf, length)
	_ = xdr.WriteUint32(buf, denied.LockType)
	_ = xdr.WriteUint64(buf, denied.Owner.ClientID)
	_ = xdr.WriteXDROpaque(buf, denied.Owner.OwnerData)
}

// ============================================================================
// Validation Helpers
// ============================================================================

// normalizeLockRange checks that offset and length describe a byte range the
// server will act on, and translates it into the lock manager's spelling.
//
// RFC 7530 Section 16.10.4 rejects a length of zero, and rejects a length that
// is not all-ones whose sum with the offset exceeds the maximum 64-bit unsigned
// value. A length with every bit set is the wire encoding for "from offset to
// end-of-file", so it is exempt from that sum. Sections 16.11.4 and 16.12.4
// apply the same two rules to LOCKT and LOCKU.
//
// The lock manager spells that same end-of-file range as a length of zero, so
// an accepted all-ones length is returned as zero and every other accepted
// length is returned unchanged.
//
// Returns NFS4ERR_INVAL on a rejected range.
func normalizeLockRange(offset, length uint64) (uint64, error) {
	if length == math.MaxUint64 {
		return 0, nil
	}
	if length != 0 && offset <= math.MaxUint64-length {
		return length, nil
	}
	return 0, &NFS4StateError{
		Status:  types.NFS4ERR_INVAL,
		Message: "invalid byte-range lock offset/length",
	}
}

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
	// The per-owner seqid is validated like any other value on a v4.0 client;
	// only a v4.1 session client skips owner sequencing (the slot table provides
	// replay protection there, and the handler zeroes the seqid it sends).
	// Derived from the caller client ID this op already carries.
	skipOwnerSeqid := sm.v41ClientLocked(callerClientID) != nil
	openSeqIsReplay := false
	if !skipOwnerSeqid {
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
	if !skipOwnerSeqid {
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
		}
		// A brand-new lock-owner + a replayed open seqid is an inconsistent
		// retransmit, with or without the v4.1 skip: the replayed OPEN's reply
		// predates any lock state, so there is nothing consistent to replay.
		if openSeqIsReplay && !ownerExists {
			return nil, ErrBadSeqid
		}
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
	// Special stateids cannot be used with LOCK
	if lockStateid.IsSpecialStateid() {
		return nil, ErrBadStateid
	}

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
	// The per-owner seqid is validated like any other value on a v4.0 client;
	// only a v4.1 session client skips owner sequencing (the slot table provides
	// replay protection there, and the handler zeroes the seqid it sends).
	// Derived from the caller client ID this op already carries.
	if sm.v41ClientLocked(callerClientID) == nil {
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
		ClientID:  NFSLockClientIdentity(lockState.LockOwner.ClientID),
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
		Owner:  lock.LockOwner{OwnerID: ownerID, ClientID: NFSLockClientIdentity(clientID)},
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
	// The per-owner seqid is validated like any other value on a v4.0 client;
	// only a v4.1 session client skips owner sequencing (the slot table provides
	// replay protection there, and the handler zeroes the seqid it sends).
	// Derived from the caller client ID this op already carries.
	if sm.v41ClientLocked(callerClientID) == nil {
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
			ClientID:  NFSLockClientIdentity(lockOwner.ClientID),
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
