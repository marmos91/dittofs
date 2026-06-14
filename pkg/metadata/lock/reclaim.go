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

	"github.com/marmos91/dittofs/internal/logger"
)

// reclaimLeaseImpl validates grace period and restores a persisted lease.
//
// Steps:
//  1. Check grace period is active (reclaim only allowed during grace)
//  2. Query persistent store for the lease, pre-filtered by clientID when non-empty
//  3. Assert the persisted record's ClientID matches clientID (lease-stealing guard)
//  4. Optionally assert incomingPrincipal matches V4ClientRecoveryRecord.Principal
//  5. For directories, verify handle still exists via HandleChecker
//  6. Validate requestedState is a subset of the persisted state
//  7. Restore lease in memory and return the reclaimed UnifiedLock
//
// clientID is the reclaiming client's stable identity. When non-empty it is
// both pushed into the store query AND re-asserted against the matched record;
// a mismatch is rejected with a lock-not-found error so one client cannot steal
// another's lease. When empty, the owner check is skipped (callers with no
// stable client identity).
//
// incomingPrincipal is the RPCSEC_GSS / AUTH_SYS principal of the reclaiming
// NFSv4 client. The principal check runs only when lm.clientRecoveryStore is
// wired and incomingPrincipal is non-empty; SMB callers pass "".
//
// When no lockStore is configured:
//   - If the lease is already in memory, mark it reclaimed and return it.
//   - Otherwise return an explicit error; there is no file handle to anchor a
//     stub, and returning a dangling, uncleanable object would orphan it.
func (lm *Manager) reclaimLeaseImpl(ctx context.Context, leaseKey [16]byte,
	requestedState uint32, isDirectory bool, clientID string, incomingPrincipal string) (*UnifiedLock, error) {

	// Step 1: Check grace period
	if !lm.IsInGracePeriod() {
		return nil, fmt.Errorf("lease reclaim not allowed: not in grace period")
	}

	// Step 2: Try to find persisted lease via lockStore
	if lm.lockStore != nil {
		isLease := true
		query := LockQuery{IsLease: &isLease}
		if clientID != "" {
			query.ClientID = clientID
		}
		persistedLocks, err := lm.lockStore.ListLocks(ctx, query)
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

			// Step 3: Caller-identity check — reject lease-stealing. The store
			// query is already filtered by clientID when non-empty, but assert
			// here too so an over-permissive store backend cannot let a wrong
			// client through (defence in depth at the trust boundary).
			if clientID != "" && pl.ClientID != clientID {
				logger.Debug("ReclaimLease: clientID mismatch, rejecting steal attempt",
					"leaseKey", fmt.Sprintf("%x", leaseKey),
					"wantClientID", clientID,
					"gotClientID", pl.ClientID)
				return nil, NewLockNotFoundError("")
			}

			// Step 4: NFSv4 principal check (only when the recovery store is
			// wired and the caller supplied a principal to verify). A reclaiming
			// client whose principal differs from the one recorded at confirm
			// time must NOT reclaim another client's prior state.
			if lm.clientRecoveryStore != nil && incomingPrincipal != "" {
				recs, rerr := lm.clientRecoveryStore.ListClientRecovery(ctx)
				if rerr != nil {
					return nil, fmt.Errorf("failed to fetch client recovery records: %w", rerr)
				}
				for _, rec := range recs {
					if rec.ClientIDString == pl.ClientID {
						if rec.Principal != "" && rec.Principal != incomingPrincipal {
							logger.Debug("ReclaimLease: principal mismatch, rejecting reclaim",
								"leaseKey", fmt.Sprintf("%x", leaseKey),
								"wantPrincipal", incomingPrincipal,
								"recordPrincipal", rec.Principal)
							return nil, NewLockNotFoundError("")
						}
						break
					}
				}
			}

			// Found matching persisted lease
			// Validate isDirectory matches persisted record
			if isDirectory != pl.IsDirectory {
				return nil, fmt.Errorf("isDirectory mismatch: request=%v, persisted=%v", isDirectory, pl.IsDirectory)
			}

			// Step 5: For directories, check handle still exists
			if pl.IsDirectory && lm.handleChecker != nil {
				if !lm.handleChecker.HandleExists(FileHandle(pl.FileID)) {
					return nil, fmt.Errorf("directory handle no longer exists, cannot reclaim lease")
				}
			}

			// Step 6: Validate requested state is subset of persisted
			if requestedState&^pl.LeaseState != 0 {
				return nil, fmt.Errorf("requested state %s exceeds persisted state %s",
					LeaseStateToString(requestedState),
					LeaseStateToString(pl.LeaseState))
			}

			// Record the reclaim so grace can exit early once every expected
			// client has recovered (X/Open NLMv4 / RFC 7530 §9.6.2). Both the
			// fresh-restore and already-in-memory branches below are successful
			// reclaims, so mark here once for both. MarkReclaimed takes the grace
			// manager's own mutex, not lm.mu, so it is safe outside lm.mu.
			lm.MarkReclaimed(pl.ClientID)

			// Step 7: Restore in memory (idempotent: skip if already reclaimed)
			lock := FromPersistedLock(pl)
			lock.Lease.LeaseState = requestedState
			lock.Reclaim = true

			lm.mu.Lock()
			handleKey := string(lock.FileHandle)
			if _, existing, _ := lm.findLeaseByKey(leaseKey); existing != nil {
				// Already reclaimed - update state and return existing
				existing.Lease.LeaseState = requestedState
				existing.Reclaim = true
				lm.mu.Unlock()
				logger.Debug("ReclaimLease: lease already in memory, updated",
					"leaseKey", fmt.Sprintf("%x", leaseKey))
				return existing.Clone(), nil
			}
			lm.unifiedLocks[handleKey] = append(lm.unifiedLocks[handleKey], lock)
			lm.indexAddLockLocked(handleKey, lock)
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

	// No lock store AND no in-memory lease: there is no file handle to anchor a
	// stub, so fabricating one and returning it would orphan an object that
	// RemoveFileUnifiedLocks / RemoveClientLocks can never clean up. Return an
	// explicit error instead.
	return nil, fmt.Errorf("cannot reclaim: no file handle and no lock store")
}
