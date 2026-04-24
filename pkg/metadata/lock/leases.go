// Package lock provides lease CRUD operations on the Manager.
//
// This file implements RequestLease, AcknowledgeLeaseBreak, ReleaseLease,
// and GetLeaseState methods on the Manager struct. These are the core lease
// management operations shared across SMB and NFS protocols.
//
// All lease state changes go through advanceEpoch to ensure epoch monotonicity.
//
// Reference: MS-SMB2 3.3.5.9 Processing an SMB2 CREATE Request
package lock

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
)

// ErrLeaseBreakInProgress is returned by RequestLease when a same-key lease
// is in Breaking state. Per MS-SMB2 3.3.5.9.8, the caller must set
// SMB2_LEASE_FLAG_BREAK_IN_PROGRESS (0x02) in the CREATE response.
// The returned state and epoch are the current values of the breaking lease.
var ErrLeaseBreakInProgress = errors.New("lease break in progress")

// ErrInvalidLeaseState is returned by RequestLease when the requested lease
// state is not a valid combination (e.g., Write without Read, Handle without
// Read). Per MS-SMB2 3.3.5.9.8, the caller must return STATUS_INVALID_PARAMETER.
var ErrInvalidLeaseState = errors.New("invalid lease state")

// validUpgrades defines allowed lease state upgrade transitions.
// A lease can only be upgraded (more permissions), never downgraded via RequestLease.
// Downgrade happens only through lease break.
var validUpgrades = map[uint32][]uint32{
	LeaseStateRead: {
		LeaseStateRead | LeaseStateWrite,
		LeaseStateRead | LeaseStateHandle,
		LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
	},
	LeaseStateRead | LeaseStateHandle: {
		LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
	},
	LeaseStateRead | LeaseStateWrite: {
		LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
	},
}

// isValidUpgrade checks if transitioning from currentState to requestedState is allowed.
func isValidUpgrade(currentState, requestedState uint32) bool {
	allowed, ok := validUpgrades[currentState]
	if !ok {
		return false
	}
	return slices.Contains(allowed, requestedState)
}

// advanceEpoch increments the epoch counter on a lease.
// Called on every state change: grant, break initiate, upgrade.
//
// Break ACK is NOT a state change: MS-SMB2 §3.3.4.7 specifies that the
// server sets NewEpoch = Epoch + 1 and commits Epoch = Epoch + 1 when
// the break notification is dispatched. The subsequent ACK confirms a
// transition already announced and counted; advancing again on ACK
// drifts the server one past what the client tracks and trips V2 lease
// verification on any subsequent break (see #417).
func advanceEpoch(lease *OpLock) {
	lease.Epoch++
}

// findLeaseByKey scans unifiedLocks for a lock with the given leaseKey.
// Returns (handleKey, *UnifiedLock, index) or ("", nil, -1) if not found.
// Must be called with lm.mu held.
func (lm *Manager) findLeaseByKey(leaseKey [16]byte) (string, *UnifiedLock, int) {
	for handleKey, locks := range lm.unifiedLocks {
		for i, lock := range locks {
			if lock.Lease != nil && lock.Lease.LeaseKey == leaseKey {
				return handleKey, lock, i
			}
		}
	}
	return "", nil, -1
}

// RequestLease requests a new or upgraded lease on a file or directory.
//
// For new leases, the granted state may be less than requested if conflicts exist.
// For existing leases with the same key, this performs an upgrade if the transition is valid.
//
// Returns (LeaseStateNone, 0, nil) for rejected requests (invalid state, recently-broken,
// NLM conflicts, cross-key conflicts, invalid downgrade).
func (lm *Manager) requestLeaseImpl(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
	parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
	requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error) {

	// Validate requested state against valid lease combinations.
	// Always validate against file lease states here (which accepts R, RW, RH, RWH).
	// For directories, states like RH/RWH are not directly grantable but are not
	// protocol errors -- they will be downgraded by bestGrantableState later.
	// Only truly invalid states (W, H, WH -- missing Read) return an error.
	if !IsValidFileLeaseState(requestedState) {
		logger.Debug("RequestLease: invalid lease state",
			"state", LeaseStateToString(requestedState),
			"fileHandle", string(fileHandle),
			"isDirectory", isDirectory)
		return LeaseStateNone, 0, ErrInvalidLeaseState
	}

	// None is always granted trivially
	if requestedState == LeaseStateNone {
		return LeaseStateNone, 0, nil
	}

	handleKey := string(fileHandle)

	// Check recently-broken cache for directories
	if isDirectory && lm.recentlyBroken != nil && lm.recentlyBroken.IsRecentlyBroken(handleKey) {
		logger.Debug("RequestLease: directory recently broken, denying",
			"fileHandle", handleKey)
		return LeaseStateNone, 0, nil
	}

	// Check NLM lock conflicts
	if lm.lockStore != nil {
		if CheckNLMLocksForLeaseConflict(lm.lockStore, ctx, handleKey, requestedState, clientID) {
			logger.Debug("RequestLease: NLM lock conflict",
				"fileHandle", handleKey,
				"requestedState", LeaseStateToString(requestedState))
			return LeaseStateNone, 0, nil
		}
	}

	lm.mu.Lock()

	locks := lm.unifiedLocks[handleKey]

	// Check for delegation conflicts before granting a lease
	for _, lock := range locks {
		if lock.Delegation != nil {
			// Create a temporary OpLock to check coexistence
			tempLease := &OpLock{LeaseState: requestedState}
			if DelegationConflictsWithLease(lock.Delegation, tempLease) {
				lm.mu.Unlock()
				logger.Debug("RequestLease: delegation conflict, denying lease",
					"fileHandle", handleKey,
					"delegationType", lock.Delegation.DelegType.String(),
					"requestedState", LeaseStateToString(requestedState))
				return LeaseStateNone, 0, fmt.Errorf("lease denied: conflicts with %s delegation on file",
					lock.Delegation.DelegType.String())
			}
		}
	}

	// Search for existing lease with same key
	for i, lock := range locks {
		if lock.Lease == nil || lock.Lease.LeaseKey != leaseKey {
			continue
		}

		// Same-key found
		currentState := lock.Lease.LeaseState

		// Per MS-SMB2 3.3.5.9.8: If the lease is in Breaking state, do NOT
		// modify it. Return the current LeaseState and signal break-in-progress
		// to the caller so it can set SMB2_LEASE_FLAG_BREAK_IN_PROGRESS (0x02).
		if lock.Lease.Breaking {
			epoch := lock.Lease.Epoch
			lm.mu.Unlock()
			logger.Debug("RequestLease: same-key lease is breaking, returning current state with break-in-progress",
				"fileHandle", handleKey,
				"currentState", LeaseStateToString(currentState),
				"epoch", epoch)
			return currentState, epoch, ErrLeaseBreakInProgress
		}

		// Same state requested - return current (no-op)
		if currentState == requestedState {
			lm.mu.Unlock()
			return currentState, lock.Lease.Epoch, nil
		}

		// Check if this is a valid upgrade
		if isValidUpgrade(currentState, requestedState) {
			// Upgrade the lease
			locks[i].Lease.LeaseState = requestedState
			advanceEpoch(locks[i].Lease)

			logger.Debug("RequestLease: upgraded lease",
				"fileHandle", handleKey,
				"from", LeaseStateToString(currentState),
				"to", LeaseStateToString(requestedState),
				"epoch", locks[i].Lease.Epoch)

			// Persist if store available
			if lm.lockStore != nil {
				pl := ToPersistedLock(locks[i], 0)
				if err := lm.lockStore.PutLock(ctx, pl); err != nil {
					logger.Error("RequestLease: failed to persist lease upgrade", "fileHandle", handleKey, "error", err)
				}
			}

			epoch := locks[i].Lease.Epoch
			lm.mu.Unlock()
			return requestedState, epoch, nil
		}

		// Invalid transition (downgrade)
		lm.mu.Unlock()
		logger.Debug("RequestLease: invalid state transition (downgrade)",
			"fileHandle", handleKey,
			"from", LeaseStateToString(currentState),
			"to", LeaseStateToString(requestedState))
		return LeaseStateNone, 0, nil
	}

	// No existing lease with same key. Check for cross-key conflicts.
	// Per MS-SMB2 3.3.5.9: break conflicting leases, then grant the best
	// available state (may be less than requested).
	var breakDispatched bool
	for _, lock := range locks {
		if lock.Lease == nil {
			continue
		}

		// Create temporary OpLock for conflict check
		requested := &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: requestedState,
		}

		if OpLocksConflict(lock.Lease, requested) {
			// Compute break-to state via ComputeLeaseBreakTo per MS-SMB2
			// 3.3.4.7 and Samba delay_for_oplock_fn. RequestLease runs only
			// after the new opener's CREATE has already passed share-mode
			// resolution (STATUS_SHARING_VIOLATION would have returned
			// before RqLs processing), so hasSharingViolation is always
			// false here — strip Write, keep Read + Handle. RWH→RH, RW→R.
			breakTo := ComputeLeaseBreakTo(lock.Lease.LeaseState, false)

			// If existing lease has no Write bit, the break is a no-op
			// (e.g., existing=R, breakTo=R). In this case, don't dispatch
			// a break -- just proceed to downgrade the new request.
			if breakTo == lock.Lease.LeaseState {
				logger.Debug("RequestLease: cross-key conflict but existing has no Write, skipping break",
					"fileHandle", handleKey,
					"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
					"existingState", LeaseStateToString(lock.Lease.LeaseState),
					"requestedState", LeaseStateToString(requestedState))
				break
			}

			logger.Debug("RequestLease: cross-key conflict, initiating break",
				"fileHandle", handleKey,
				"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
				"requestedKey", fmt.Sprintf("%x", leaseKey),
				"existingState", LeaseStateToString(lock.Lease.LeaseState),
				"requestedState", LeaseStateToString(requestedState),
				"breakToState", LeaseStateToString(breakTo))

			// Mark lease as breaking before dispatching callbacks
			lock.Lease.Breaking = true
			lock.Lease.BreakToState = breakTo
			lock.Lease.BreakStarted = time.Now()
			advanceEpoch(lock.Lease)

			// Persist the breaking state
			if lm.lockStore != nil {
				pl := ToPersistedLock(lock, 0)
				if err := lm.lockStore.PutLock(ctx, pl); err != nil {
					logger.Error("RequestLease: failed to persist breaking state", "fileHandle", handleKey, "error", err)
				}
			}

			// Clone the lock before releasing mu so that dispatchOpLockBreak
			// receives a snapshot. Without this, concurrent AcknowledgeLeaseBreak
			// can mutate the live *UnifiedLock while the callback reads it.
			lockSnapshot := lock.Clone()

			// Release lock before dispatching break callbacks. The dispatch
			// itself is synchronous: by the time dispatchOpLockBreak returns,
			// the LEASE_BREAK_NOTIFICATION is already on the wire to the
			// existing client (see internal/adapter/smb/lease/notifier.go,
			// SMBBreakHandler.OnOpLockBreak which calls SendLeaseBreak inline).
			// Per MS-SMB2 3.3.4.7 the notification ordering requirement is
			// therefore satisfied without further synchronization.
			lm.mu.Unlock()
			lm.dispatchOpLockBreak(handleKey, lockSnapshot, breakTo)

			// Do NOT wait for the LEASE_BREAK_ACK before returning to the
			// second opener. Waiting here causes a fatal deadlock in
			// multi-client scenarios such as WPTS
			// BVT_DirectoryLeasing_LeaseBreakOnMultiClients: the test (and
			// in general any single-threaded client driver) only sends the
			// ack from the first client AFTER the second client's CREATE
			// returns. Blocking the second CREATE on that ack prevents the
			// ack from ever being sent, and the wait either burns the
			// client's CREATE timeout or runs out our own bounded deadline
			// for nothing.
			//
			// The breaking lease remains in unifiedLocks with Breaking=true
			// and BreakToState set; OpLocksConflict (oplock.go:229-233)
			// already evaluates conflicts against BreakToState in that case,
			// so bestGrantableState below computes the correct downgraded
			// grant for the new opener without needing the ack to land
			// first. The same async-dispatch pattern is used by
			// internal/adapter/smb/lease/manager.go BreakHandleLeasesOnOpenAsync,
			// whose comment explicitly documents this deadlock.
			breakDispatched = true
			break
		}
	}

	// After any break (or no-op skip), find the best grantable state.
	// Per MS-SMB2 3.3.5.9: the server MUST grant the best available oplock
	// level. Try the full requested state first, then progressively lower
	// states: strip Write, then strip Handle, then Read only, then None.
	if breakDispatched {
		lm.mu.Lock()
		locks = lm.unifiedLocks[handleKey]
	}
	// lm.mu is held here (either from initial Lock or re-Lock after break)

	grantState := bestGrantableState(locks, leaseKey, requestedState, isDirectory)
	if grantState == LeaseStateNone {
		lm.mu.Unlock()
		logger.Debug("RequestLease: no compatible state after conflict resolution",
			"fileHandle", handleKey,
			"requestedState", LeaseStateToString(requestedState))
		return LeaseStateNone, 0, nil
	}

	granted, epoch := lm.createAndGrantLease(ctx, handleKey, fileHandle,
		leaseKey, parentLeaseKey, ownerID, clientID, shareName,
		grantState, isDirectory)
	lm.mu.Unlock()

	logger.Debug("RequestLease: granted lease",
		"fileHandle", handleKey,
		"requested", LeaseStateToString(requestedState),
		"granted", LeaseStateToString(granted),
		"isDirectory", isDirectory,
		"downgraded", grantState != requestedState,
		"epoch", epoch)

	return granted, epoch, nil
}

// bestGrantableState finds the best lease state that can be granted without
// conflicting with existing leases from other keys. It tries the requested
// state first, then progressively lower states per MS-SMB2 3.3.5.9:
// requested -> strip W -> strip H -> R only -> None.
//
// Precondition: caller must hold lm.mu (read or write). The locks slice is
// read from lm.unifiedLocks[handleKey] under that lock, so no concurrent
// mutation can occur while this function iterates.
func bestGrantableState(locks []*UnifiedLock, leaseKey [16]byte, requestedState uint32, isDirectory bool) uint32 {
	candidates := downgradeCandidates(requestedState, isDirectory)

outer:
	for _, candidate := range candidates {
		tempLease := &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: candidate,
		}
		for _, lock := range locks {
			if lock.Lease == nil || lock.Lease.LeaseKey == leaseKey {
				continue
			}
			if OpLocksConflict(lock.Lease, tempLease) {
				continue outer
			}
		}
		return candidate
	}
	return LeaseStateNone
}

// downgradeCandidates returns the ordered list of lease states to try,
// starting with the requested state and progressively removing flags.
// Per MS-SMB2 3.3.5.9: try full request, then strip Write, then strip
// Handle, then Read only.
func downgradeCandidates(requestedState uint32, isDirectory bool) []uint32 {
	isValidState := IsValidFileLeaseState
	if isDirectory {
		isValidState = IsValidDirectoryLeaseState
	}

	// At most 4 unique candidates after dedup, so linear scan beats map allocation.
	var candidates []uint32
	addIfValid := func(state uint32) {
		if state == LeaseStateNone || slices.Contains(candidates, state) || !isValidState(state) {
			return
		}
		candidates = append(candidates, state)
	}

	// 1. Try full requested state
	addIfValid(requestedState)
	// 2. Strip Write (RWH -> RH, RW -> R)
	addIfValid(requestedState &^ LeaseStateWrite)
	// 3. Strip Handle (RWH -> RW, RH -> R)
	addIfValid(requestedState &^ LeaseStateHandle)
	// 4. Strip both Write and Handle (RWH -> R)
	addIfValid(requestedState &^ (LeaseStateWrite | LeaseStateHandle))
	// 5. Read only as fallback
	addIfValid(LeaseStateRead)

	return candidates
}

// createAndGrantLease creates a new lease lock, appends it to unifiedLocks[handleKey],
// persists it, and returns the granted state. Must be called with lm.mu held; the
// caller is responsible for unlocking after this returns.
func (lm *Manager) createAndGrantLease(
	ctx context.Context,
	handleKey string,
	fileHandle FileHandle,
	leaseKey, parentLeaseKey [16]byte,
	ownerID, clientID, shareName string,
	requestedState uint32,
	isDirectory bool,
) (uint32, uint16) {
	newLock := &UnifiedLock{
		ID: uuid.New().String(),
		Owner: LockOwner{
			OwnerID:   ownerID,
			ClientID:  clientID,
			ShareName: shareName,
		},
		FileHandle: fileHandle,
		Offset:     0,
		Length:     0,
		Type:       lockTypeForLeaseState(requestedState),
		AcquiredAt: time.Now(),
		Lease: &OpLock{
			LeaseKey:       leaseKey,
			LeaseState:     requestedState,
			ParentLeaseKey: parentLeaseKey,
			IsDirectory:    isDirectory,
			Epoch:          1, // New leases start at epoch 1
		},
	}

	lm.unifiedLocks[handleKey] = append(lm.unifiedLocks[handleKey], newLock)

	if lm.lockStore != nil {
		pl := ToPersistedLock(newLock, 0)
		if err := lm.lockStore.PutLock(ctx, pl); err != nil {
			logger.Error("RequestLease: failed to persist new lease", "fileHandle", handleKey, "error", err)
		}
	}

	return requestedState, 1
}

// lockTypeForLeaseState returns the appropriate LockType for a lease state.
func lockTypeForLeaseState(state uint32) LockType {
	if state&LeaseStateWrite != 0 {
		return LockTypeExclusive
	}
	return LockTypeShared
}

// AcknowledgeLeaseBreak processes a client's lease break acknowledgment.
//
// The client must acknowledge with a state <= breakToState. If acknowledgedState
// is LeaseStateNone, the lease is fully released (removed).
func (lm *Manager) acknowledgeLeaseBreakImpl(ctx context.Context, leaseKey [16]byte,
	acknowledgedState uint32, epoch uint16) error {

	lm.mu.Lock()
	defer lm.mu.Unlock()

	handleKey, lock, idx := lm.findLeaseByKey(leaseKey)
	if lock == nil {
		return fmt.Errorf("no lease found with key %x", leaseKey)
	}

	if !lock.Lease.Breaking {
		return fmt.Errorf("lease %x is not in breaking state", leaseKey)
	}

	// Validate epoch if provided (V2 staleness check).
	// The epoch was already advanced during break initiation, so the client
	// should echo the current epoch value from the break notification.
	if epoch != 0 && lock.Lease.Epoch != epoch {
		return fmt.Errorf("stale epoch: expected %d, got %d", lock.Lease.Epoch, epoch)
	}

	// Client cannot claim bits not offered (bitwise subset check)
	if acknowledgedState & ^lock.Lease.BreakToState != 0 {
		return fmt.Errorf("acknowledged state %s exceeds break-to state %s",
			LeaseStateToString(acknowledgedState),
			LeaseStateToString(lock.Lease.BreakToState))
	}

	// If acknowledging to None, remove the lease entirely
	if acknowledgedState == LeaseStateNone {
		locks := lm.unifiedLocks[handleKey]
		lm.unifiedLocks[handleKey] = append(locks[:idx], locks[idx+1:]...)
		if len(lm.unifiedLocks[handleKey]) == 0 {
			delete(lm.unifiedLocks, handleKey)
		}

		// Remove from persistent store
		if lm.lockStore != nil {
			_ = lm.lockStore.DeleteLock(ctx, lock.ID)
		}

		logger.Debug("AcknowledgeLeaseBreak: lease fully released",
			"leaseKey", fmt.Sprintf("%x", leaseKey))
		lm.signalBreakWaitLocked(handleKey)
		return nil
	}

	// Update lease state. Do NOT advance Epoch here: the state change was
	// already counted when the break notification was dispatched per MS-SMB2
	// §3.3.4.7 ("NewEpoch = Epoch + 1 ... Epoch = Epoch + 1"). Advancing on
	// ACK drifts the server one past the client (#417).
	lock.Lease.LeaseState = acknowledgedState
	lock.Lease.Breaking = false
	lock.Lease.BreakToState = 0
	lock.Lease.BreakStarted = time.Time{}

	// Update lock type based on new state
	lock.Type = lockTypeForLeaseState(acknowledgedState)

	// Persist updated state
	if lm.lockStore != nil {
		pl := ToPersistedLock(lock, 0)
		_ = lm.lockStore.PutLock(ctx, pl)
	}

	logger.Debug("AcknowledgeLeaseBreak: break acknowledged",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"newState", LeaseStateToString(acknowledgedState),
		"epoch", lock.Lease.Epoch)

	lm.signalBreakWaitLocked(handleKey)
	return nil
}

// ReleaseLease releases all lease state for the given lease key.
func (lm *Manager) releaseLeaseImpl(ctx context.Context, leaseKey [16]byte) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Find and remove all locks with matching lease key
	for handleKey, locks := range lm.unifiedLocks {
		var remaining []*UnifiedLock
		for _, lock := range locks {
			if lock.Lease != nil && lock.Lease.LeaseKey == leaseKey {
				// Remove from persistent store
				if lm.lockStore != nil {
					_ = lm.lockStore.DeleteLock(ctx, lock.ID)
				}
				continue // Skip (remove) this lock
			}
			remaining = append(remaining, lock)
		}

		if len(remaining) == 0 {
			delete(lm.unifiedLocks, handleKey)
		} else {
			lm.unifiedLocks[handleKey] = remaining
		}
	}

	return nil
}

// GetLeaseState returns the current state and epoch for a lease key.
func (lm *Manager) getLeaseStateImpl(_ context.Context, leaseKey [16]byte) (state uint32, epoch uint16, found bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	_, lock, _ := lm.findLeaseByKey(leaseKey)
	if lock == nil || lock.Lease == nil {
		return 0, 0, false
	}

	return lock.Lease.LeaseState, lock.Lease.Epoch, true
}
