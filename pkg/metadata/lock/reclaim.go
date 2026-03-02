// Package lock provides unified lease reclaim for both SMB and NFS protocols.
//
// Lease reclaim is used during grace period after server restart. Clients
// that held leases before the restart can reclaim them during the grace window.
//
// For directory leases, additional validation checks that the directory still
// exists (via HandleChecker) since deleted directories cannot have leases reclaimed.
//
// Reference: MS-SMB2 3.3.5.9 Processing an SMB2 CREATE Request (Lease Reclaim)
package lock

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
)

// reclaimLeaseImpl validates grace period and restores a persisted lease.
//
// Steps:
//  1. Check grace period is active (reclaim only allowed during grace)
//  2. Query persistent store for the lease by leaseKey
//  3. For directories, verify handle still exists via HandleChecker
//  4. Validate requestedState matches or is subset of persisted state
//  5. Restore lease in memory and return the reclaimed UnifiedLock
func (lm *Manager) reclaimLeaseImpl(ctx context.Context, leaseKey [16]byte,
	requestedState uint32, isDirectory bool) (*UnifiedLock, error) {

	// Step 1: Check grace period
	if !lm.IsInGracePeriod() {
		return nil, fmt.Errorf("lease reclaim not allowed: not in grace period")
	}

	// Step 2: Try to find persisted lease via lockStore
	if lm.lockStore != nil {
		isLease := true
		persistedLocks, err := lm.lockStore.ListLocks(ctx, LockQuery{
			IsLease: &isLease,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to query persisted leases: %w", err)
		}

		// Find the lease with matching key
		for _, pl := range persistedLocks {
			if len(pl.LeaseKey) != 16 {
				continue
			}
			var plKey [16]byte
			copy(plKey[:], pl.LeaseKey)
			if plKey != leaseKey {
				continue
			}

			// Found matching persisted lease
			// Step 3: For directories, check handle still exists
			if isDirectory && lm.handleChecker != nil {
				if !lm.handleChecker.HandleExists(FileHandle(pl.FileID)) {
					return nil, fmt.Errorf("directory handle no longer exists, cannot reclaim lease")
				}
			}

			// Step 4: Validate requested state is subset of persisted
			if requestedState&^pl.LeaseState != 0 {
				return nil, fmt.Errorf("requested state %s exceeds persisted state %s",
					LeaseStateToString(requestedState),
					LeaseStateToString(pl.LeaseState))
			}

			// Step 5: Restore in memory
			lock := FromPersistedLock(pl)
			lock.Lease.LeaseState = requestedState
			lock.Lease.IsDirectory = isDirectory
			lock.Reclaim = true

			lm.mu.Lock()
			handleKey := string(lock.FileHandle)
			lm.unifiedLocks[handleKey] = append(lm.unifiedLocks[handleKey], lock)
			lm.mu.Unlock()

			logger.Debug("ReclaimLease: lease reclaimed",
				"leaseKey", fmt.Sprintf("%x", leaseKey),
				"state", LeaseStateToString(requestedState),
				"isDirectory", isDirectory)

			return lock.Clone(), nil
		}

		return nil, fmt.Errorf("no persisted lease found with key %x", leaseKey)
	}

	// No lockStore: try to find in memory (for testing without persistence)
	lm.mu.Lock()
	defer lm.mu.Unlock()

	_, existingLock, _ := lm.findLeaseByKey(leaseKey)
	if existingLock != nil {
		// Lease already in memory - mark as reclaimed
		existingLock.Reclaim = true
		return existingLock.Clone(), nil
	}

	// Create a minimal reclaimed lease in memory
	reclaimedLock := &UnifiedLock{
		ID: uuid.New().String(),
		Owner: LockOwner{
			OwnerID: fmt.Sprintf("reclaim:%x", leaseKey),
		},
		AcquiredAt: time.Now(),
		Reclaim:    true,
		Lease: &OpLock{
			LeaseKey:    leaseKey,
			LeaseState:  requestedState,
			IsDirectory: isDirectory,
			Epoch:       1,
		},
	}

	// We don't have a file handle for this lease without persistence
	// This path is primarily for testing
	return reclaimedLock, nil
}
