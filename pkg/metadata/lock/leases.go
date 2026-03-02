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
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
)

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
// Called on every state change: grant, break initiate, break ack, upgrade.
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

	// Validate requested state
	valid := IsValidFileLeaseState(requestedState)
	if isDirectory {
		valid = IsValidDirectoryLeaseState(requestedState)
	}
	if !valid {
		logger.Debug("RequestLease: invalid lease state",
			"state", LeaseStateToString(requestedState),
			"fileHandle", string(fileHandle),
			"isDirectory", isDirectory)
		return LeaseStateNone, 0, nil
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
	defer lm.mu.Unlock()

	locks := lm.unifiedLocks[handleKey]

	// Search for existing lease with same key
	for i, lock := range locks {
		if lock.Lease == nil || lock.Lease.LeaseKey != leaseKey {
			continue
		}

		// Same-key found
		currentState := lock.Lease.LeaseState

		// Same state requested - return current (no-op)
		if currentState == requestedState {
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
				_ = lm.lockStore.PutLock(ctx, pl)
			}

			return requestedState, locks[i].Lease.Epoch, nil
		}

		// Invalid transition (downgrade)
		logger.Debug("RequestLease: invalid state transition (downgrade)",
			"fileHandle", handleKey,
			"from", LeaseStateToString(currentState),
			"to", LeaseStateToString(requestedState))
		return LeaseStateNone, 0, nil
	}

	// No existing lease with same key. Check for cross-key conflicts.
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
			// Conflict detected - initiate break and deny
			logger.Debug("RequestLease: cross-key conflict, initiating break",
				"fileHandle", handleKey,
				"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
				"requestedKey", fmt.Sprintf("%x", leaseKey),
				"existingState", LeaseStateToString(lock.Lease.LeaseState),
				"requestedState", LeaseStateToString(requestedState))

			// Release lock before dispatching break callbacks
			lm.mu.Unlock()
			lm.dispatchOpLockBreak(handleKey, lock, LeaseStateNone)
			lm.mu.Lock()

			return LeaseStateNone, 0, nil
		}
	}

	// No conflicts - create new lease
	newLock := &UnifiedLock{
		ID: uuid.New().String(),
		Owner: LockOwner{
			OwnerID:   ownerID,
			ClientID:  clientID,
			ShareName: shareName,
		},
		FileHandle: fileHandle,
		Offset:     0,
		Length:     0, // Whole file
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

	lm.unifiedLocks[handleKey] = append(locks, newLock)

	// Persist if store available
	if lm.lockStore != nil {
		pl := ToPersistedLock(newLock, 0)
		_ = lm.lockStore.PutLock(ctx, pl)
	}

	logger.Debug("RequestLease: granted new lease",
		"fileHandle", handleKey,
		"state", LeaseStateToString(requestedState),
		"isDirectory", isDirectory,
		"epoch", uint16(1))

	return requestedState, 1, nil
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

	// Validate epoch if provided (V2 staleness check)
	if epoch != 0 && lock.Lease.Epoch+1 != epoch {
		return fmt.Errorf("stale epoch: expected %d, got %d", lock.Lease.Epoch+1, epoch)
	}

	// Client cannot claim more than offered
	if acknowledgedState > lock.Lease.BreakToState {
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
		return nil
	}

	// Update lease state
	lock.Lease.LeaseState = acknowledgedState
	lock.Lease.Breaking = false
	lock.Lease.BreakToState = 0
	lock.Lease.BreakStarted = time.Time{}
	advanceEpoch(lock.Lease)

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
