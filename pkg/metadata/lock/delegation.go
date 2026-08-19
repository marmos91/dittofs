// Delegation management for cross-protocol caching.
//
// Delegations are the protocol-neutral equivalent of NFS delegations and
// SMB leases, representing caching permissions granted to a client. Unlike
// leases (which are SMB-specific with LeaseKey and R/W/H flags), delegations
// are a simpler read/write model that can be mapped to either protocol.
//
// This file contains the Delegation struct, type enum, coexistence rules
// with leases, and helper functions.
//
// Reference: RFC 8881 Section 10 (NFS Delegations)

package lock

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
)

// DelegationType represents the type of delegation (read or write).
type DelegationType int

const (
	// DelegTypeRead is a read delegation - client may cache reads.
	// Multiple read delegations can coexist on the same file.
	DelegTypeRead DelegationType = iota

	// DelegTypeWrite is a write delegation - client may cache writes.
	// Only one write delegation can exist per file. Write delegations
	// conflict with both read and write leases.
	DelegTypeWrite
)

// String returns a human-readable name for the delegation type.
func (dt DelegationType) String() string {
	switch dt {
	case DelegTypeRead:
		return "read"
	case DelegTypeWrite:
		return "write"
	default:
		return "unknown"
	}
}

// Delegation holds protocol-neutral delegation state.
//
// A delegation grants a client permission to cache file data locally,
// reducing network round-trips. The server can recall delegations when
// another client needs access.
//
// No NFS-specific fields (no Stateid4, no *time.Timer) are stored here.
// Protocol adapters map between this struct and their protocol-specific types.
//
// Lifecycle:
//  1. Client requests delegation via protocol handler
//  2. LockManager grants delegation (stored as UnifiedLock with Delegation field)
//  3. Conflicting operation triggers recall (Breaking=true, BreakStarted set)
//  4. Client returns delegation or timeout expires
//  5. Delegation removed from lock manager
type Delegation struct {
	// DelegationID is a unique identifier for this delegation (UUID).
	DelegationID string

	// DelegType is the type of delegation (read or write).
	DelegType DelegationType

	// IsDirectory indicates this delegation is on a directory.
	IsDirectory bool

	// ClientID identifies the client holding the delegation.
	ClientID string

	// ShareName is the share this delegation belongs to.
	ShareName string

	// Breaking indicates a delegation recall is in progress.
	// When true, the client has been notified to return the delegation.
	Breaking bool

	// BreakStarted records when the recall was initiated.
	// Used to enforce recall timeout (force revoke if client does not return).
	BreakStarted time.Time

	// Recalled indicates the delegation recall notification was sent.
	Recalled bool

	// Revoked indicates the delegation was force-revoked (timeout expired).
	Revoked bool

	// NotificationMask is a bitmask of directory change notification types
	// this delegation is interested in. Used for directory delegations.
	NotificationMask uint32
}

// NewDelegation creates a new Delegation with a generated UUID.
func NewDelegation(delegType DelegationType, clientID, shareName string, isDirectory bool) *Delegation {
	return &Delegation{
		DelegationID: uuid.New().String(),
		DelegType:    delegType,
		ClientID:     clientID,
		ShareName:    shareName,
		IsDirectory:  isDirectory,
	}
}

// Clone returns a deep copy of the Delegation.
// Returns nil if the receiver is nil.
func (d *Delegation) Clone() *Delegation {
	if d == nil {
		return nil
	}
	return &Delegation{
		DelegationID:     d.DelegationID,
		DelegType:        d.DelegType,
		IsDirectory:      d.IsDirectory,
		ClientID:         d.ClientID,
		ShareName:        d.ShareName,
		Breaking:         d.Breaking,
		BreakStarted:     d.BreakStarted,
		Recalled:         d.Recalled,
		Revoked:          d.Revoked,
		NotificationMask: d.NotificationMask,
	}
}

// DelegationConflictsWithLease checks if a delegation conflicts with an SMB lease.
//
// Coexistence rules:
//   - Read delegation + Read-only lease = OK (both are read-only caching)
//   - Write delegation + any lease = CONFLICT (write delegation is exclusive)
//   - Any delegation + Write lease = CONFLICT (write lease requires exclusive access)
//   - Read delegation + Read+Handle lease = OK (no write involved)
//
// Returns false if either input is nil.
func DelegationConflictsWithLease(deleg *Delegation, lease *OpLock) bool {
	if deleg == nil || lease == nil {
		return false
	}

	// Write delegation conflicts with any active lease (not LeaseStateNone)
	if deleg.DelegType == DelegTypeWrite {
		return lease.LeaseState != LeaseStateNone
	}

	// Any delegation conflicts with write lease
	if lease.HasWrite() {
		return true
	}

	// Read delegation + non-write lease = OK
	return false
}

// ============================================================================
// Delegation CRUD Operations
// ============================================================================

// GrantDelegation grants a delegation on a file.
// Returns error if a conflicting lease or byte-range lock exists, or the file
// was recently broken.
func (lm *Manager) GrantDelegation(handleKey string, delegation *Delegation) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Check anti-storm cache inside the lock to be atomic with lease conflict check.
	if lm.recentlyBroken != nil && lm.recentlyBroken.IsRecentlyBroken(handleKey) {
		return fmt.Errorf("delegation denied: file recently had caching broken")
	}

	locks := lm.unifiedLocks[handleKey]

	// Check lease conflicts. Delegation-vs-delegation conflicts (e.g., at most
	// one write delegation per file) are enforced by the protocol layer (NFS
	// state manager, SMB handler) before calling GrantDelegation.
	for _, lock := range locks {
		if lock.Lease != nil && DelegationConflictsWithLease(delegation, lock.Lease) {
			return fmt.Errorf("delegation conflicts with existing lease (state=%s)",
				LeaseStateToString(lock.Lease.LeaseState))
		}
	}

	newLock := &UnifiedLock{
		ID: delegation.DelegationID,
		Owner: LockOwner{
			OwnerID:   DelegationOwnerID(delegation.ClientID, delegation.DelegationID),
			ClientID:  delegation.ClientID,
			ShareName: delegation.ShareName,
		},
		FileHandle: FileHandle(handleKey),
		Offset:     0,
		Length:     0, // Whole file
		Type:       delegationToLockType(delegation.DelegType),
		AcquiredAt: time.Now(),
		Delegation: delegation,
	}

	// A byte-range lock already held on this file denies the delegation. A
	// delegated client is entitled to satisfy its own byte-range locks locally,
	// without ever asking the server, so handing out a delegation over an
	// existing lock makes that lock invisible to the delegated client — the
	// cross-protocol conflict that AddUnifiedLock enforces in the other
	// direction would simply never be evaluated.
	//
	// Both lock maps are consulted: NLM/NFSv4 locks live in unifiedLocks and
	// SMB locks in locks. The delegation is weighed as the whole-file byte-range
	// lock it stands in for, so a write delegation (exclusive) is denied by any
	// foreign lock and a read delegation (shared) only by a foreign exclusive
	// one — the same rule byte-range locks apply to each other, including its
	// exemption for the holder's own client.
	for _, lock := range locks {
		if lock.Lease != nil || lock.Delegation != nil {
			continue
		}
		if newLock.ConflictsWith(lock) {
			return fmt.Errorf("delegation conflicts with existing byte-range lock (owner=%s)",
				lock.Owner.OwnerID)
		}
	}
	// The SMB map needs a plain byte-range stand-in: fileLockConflictsWithUnified
	// excludes delegation rows by design. SMB locks never share a client with an
	// NFS delegation, so no same-client exemption applies here.
	asByteRange := *newLock
	asByteRange.Delegation = nil
	smbLocks := lm.locks[handleKey]
	for i := range smbLocks {
		if fileLockConflictsWithUnified(&smbLocks[i], &asByteRange) {
			return fmt.Errorf("delegation conflicts with existing SMB byte-range lock (owner=%s)",
				lockOwnerID(&smbLocks[i]))
		}
	}

	lm.unifiedLocks[handleKey] = append(locks, newLock)
	lm.indexAddLockLocked(handleKey, newLock)
	return nil
}

// DelegationOwnerID returns the OwnerID that GrantDelegation assigns to a
// delegation. This is useful for constructing an excludeOwner that matches
// the delegation's LockOwner.
func DelegationOwnerID(clientID, delegationID string) string {
	return fmt.Sprintf("deleg:%s:%s", clientID, delegationID)
}

// delegationToLockType converts a DelegationType to a LockType.
func delegationToLockType(dt DelegationType) LockType {
	if dt == DelegTypeWrite {
		return LockTypeExclusive
	}
	return LockTypeShared
}

// RevokeDelegation force-revokes a delegation, removing it from the lock map.
func (lm *Manager) RevokeDelegation(handleKey string, delegationID string) error {
	found := lm.removeMatchingLocksAndSignal(handleKey, false, func(l *UnifiedLock) bool {
		return l.Delegation != nil && l.Delegation.DelegationID == delegationID
	})
	if !found {
		return fmt.Errorf("delegation %s not found on handle %s", delegationID, handleKey)
	}
	return nil
}

// revokeTimedOutLease removes all in-memory lease records matching leaseKey
// from handleKey, deletes their persisted records (best-effort), and signals
// WaitForBreakCompletion waiters. Called by OpLockBreakScanner after the
// persistent record has already been deleted by the scanner's DeleteLock call;
// this method handles the in-memory half.
//
// This is the lease analogue of RevokeDelegation: same mutex discipline,
// same signalBreakWait call so parked CREATEs unblock immediately rather
// than waiting for their context deadline.
func (lm *Manager) revokeTimedOutLease(handleKey string, leaseKey [16]byte) {
	// Best-effort delete of each removed record's persisted copy: the scanner
	// already deleted the canonical PersistedLock by ID; this handles any
	// sibling records sharing the same LeaseKey.
	lm.removeMatchingLocksAndSignal(handleKey, true, func(l *UnifiedLock) bool {
		return l.Lease != nil && l.Lease.LeaseKey == leaseKey
	})
}

// removeMatchingLocksAndSignal removes every UnifiedLock on handleKey for which
// match returns true, then wakes any WaitForBreakCompletion / parked-CREATE
// waiters. When deletePersisted is true, each removed lock's persisted record is
// deleted (best-effort) under lm.mu. Returns true if at least one lock matched.
//
// Shared by RevokeDelegation and revokeTimedOutLease: same mutex discipline,
// same post-unlock signalBreakWait so parked CREATEs unblock immediately rather
// than waiting for their context deadline.
func (lm *Manager) removeMatchingLocksAndSignal(handleKey string, deletePersisted bool, match func(*UnifiedLock) bool) bool {
	lm.mu.Lock()
	old := lm.unifiedLocks[handleKey]
	var remaining []*UnifiedLock
	found := false
	for _, l := range old {
		if match(l) {
			found = true
			if deletePersisted {
				lm.deleteUnifiedLockLocked(l)
			}
			continue
		}
		remaining = append(remaining, l)
	}
	if !found {
		lm.mu.Unlock()
		return false
	}
	if len(remaining) == 0 {
		delete(lm.unifiedLocks, handleKey)
	} else {
		lm.unifiedLocks[handleKey] = remaining
	}
	lm.reindexHandleLocked(handleKey, old)
	lm.mu.Unlock()

	lm.signalBreakWait(handleKey)
	return true
}

// ReturnDelegation handles a client returning a delegation. Idempotent:
// returns nil even if the delegation was not found.
func (lm *Manager) ReturnDelegation(handleKey string, delegationID string) error {
	lm.mu.Lock()

	locks := lm.unifiedLocks[handleKey]
	var remaining []*UnifiedLock
	for _, l := range locks {
		if l.Delegation != nil && l.Delegation.DelegationID == delegationID {
			continue
		}
		remaining = append(remaining, l)
	}

	if len(remaining) == 0 {
		delete(lm.unifiedLocks, handleKey)
	} else {
		lm.unifiedLocks[handleKey] = remaining
	}
	lm.reindexHandleLocked(handleKey, locks)
	lm.mu.Unlock()

	lm.signalBreakWait(handleKey)
	return nil
}

// GetDelegation retrieves a specific delegation by ID.
// Returns nil if not found.
func (lm *Manager) GetDelegation(handleKey string, delegationID string) *Delegation {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	for _, lock := range lm.unifiedLocks[handleKey] {
		if lock.Delegation != nil && lock.Delegation.DelegationID == delegationID {
			return lock.Delegation.Clone()
		}
	}
	return nil
}

// ListDelegations returns all delegations on a file.
func (lm *Manager) ListDelegations(handleKey string) []*Delegation {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	var result []*Delegation
	for _, lock := range lm.unifiedLocks[handleKey] {
		if lock.Delegation != nil {
			result = append(result, lock.Delegation.Clone())
		}
	}
	return result
}

// ExpiredDelegation holds info about a delegation whose recall has timed out.
type ExpiredDelegation struct {
	HandleKey    string
	DelegationID string
}

// CollectExpiredDelegationRecalls returns delegations that are in the breaking
// state and have exceeded the given timeout. This allows external scanners to
// query for expired recalls without accessing internal fields.
func (lm *Manager) CollectExpiredDelegationRecalls(now time.Time, timeout time.Duration) []ExpiredDelegation {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	var expired []ExpiredDelegation
	for handleKey, locks := range lm.unifiedLocks {
		for _, lock := range locks {
			if lock.Delegation == nil || !lock.Delegation.Breaking {
				continue
			}
			if now.After(lock.Delegation.BreakStarted.Add(timeout)) {
				expired = append(expired, ExpiredDelegation{
					HandleKey:    handleKey,
					DelegationID: lock.Delegation.DelegationID,
				})
			}
		}
	}
	return expired
}

// breakDelegations collects delegations matching the predicate and dispatches
// recall notifications. Releases mutex before dispatching to avoid deadlock.
//
// excludeOwner skips delegations whose Owner.OwnerID matches. Delegation
// OwnerIDs use the format "deleg:{clientID}:{delegationID}". Callers that
// want to exclude by client identity should match on Owner.ClientID instead,
// or construct the OwnerID in the same format.
func (lm *Manager) breakDelegations(
	handleKey string,
	excludeOwner *LockOwner,
	shouldBreak func(deleg *Delegation) bool,
) {
	lm.mu.Lock()
	locks := lm.unifiedLocks[handleKey]

	var toBreak []*UnifiedLock
	for _, lock := range locks {
		if lock.Delegation == nil {
			continue
		}
		if excludeOwner != nil &&
			(lock.Owner.OwnerID == excludeOwner.OwnerID ||
				(excludeOwner.ClientID != "" && lock.Owner.ClientID == excludeOwner.ClientID)) {
			continue
		}
		if lock.Delegation.Breaking {
			continue
		}
		if shouldBreak(lock.Delegation) {
			lock.Delegation.Breaking = true
			lock.Delegation.BreakStarted = time.Now()
			// Clone before dispatch to prevent race with concurrent ack/release.
			toBreak = append(toBreak, lock.Clone())
		}
	}
	lm.mu.Unlock()

	if len(toBreak) > 0 && lm.recentlyBroken != nil {
		lm.recentlyBroken.Mark(handleKey)
	}

	for _, lock := range toBreak {
		lm.dispatchDelegationRecall(handleKey, lock)
	}
}

// dispatchDelegationRecall notifies all registered break callbacks about a delegation recall.
func (lm *Manager) dispatchDelegationRecall(handleKey string, lock *UnifiedLock) {
	lm.mu.RLock()
	callbacks := make([]BreakCallbacks, len(lm.breakCallbacks))
	copy(callbacks, lm.breakCallbacks)
	lm.mu.RUnlock()

	if len(callbacks) == 0 {
		logger.Debug("delegation recall with no callbacks registered",
			"handleKey", handleKey,
			"delegationID", lock.Delegation.DelegationID)
		return
	}

	for _, cb := range callbacks {
		cb.OnDelegationRecall(handleKey, lock)
	}
}

// dispatchOpLockBreak notifies all registered break callbacks about an oplock break.
func (lm *Manager) dispatchOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	lm.mu.RLock()
	callbacks := make([]BreakCallbacks, len(lm.breakCallbacks))
	copy(callbacks, lm.breakCallbacks)
	lm.mu.RUnlock()

	if len(callbacks) == 0 {
		logger.Debug("oplock break with no callbacks registered",
			"handleKey", handleKey,
			"owner", lock.Owner.OwnerID,
			"breakToState", LeaseStateToString(breakToState))
		return
	}

	for _, cb := range callbacks {
		cb.OnOpLockBreak(handleKey, lock, breakToState)
	}
}
