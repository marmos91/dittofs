// Package lease provides the thin SMB LeaseManager wrapper.
//
// LeaseManager delegates all lease business logic to the shared LockManager
// (pkg/metadata/lock) and only holds SMB-specific state: the session-to-lease
// mapping needed for break notification routing.
//
// This mirrors the NFS pattern (internal/adapter/nfs/v4/state/) where the
// protocol adapter holds a thin wrapper over the shared LockManager.
package lease

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// LockManagerResolver resolves the LockManager for a given share name.
// This allows the LeaseManager to work across multiple shares without
// holding a reference to a specific share's LockManager.
type LockManagerResolver interface {
	// GetLockManagerForShare returns the LockManager for the given share.
	// Returns nil if no LockManager exists for the share.
	GetLockManagerForShare(shareName string) lock.LockManager
}

// LeaseManager is the thin SMB-side wrapper that delegates lease CRUD to
// the shared LockManager and maintains sessionID-to-leaseKey mapping for
// break notification dispatch.
//
// Thread-safe: all mutable state is protected by mu.
type LeaseManager struct {
	resolver   LockManagerResolver
	notifier   LeaseBreakNotifier
	sessionMap map[string]uint64 // hex(leaseKey) -> sessionID
	leaseShare map[string]string // hex(leaseKey) -> shareName (for resolution)
	mu         sync.RWMutex
}

// NewLeaseManager creates a new SMB LeaseManager.
//
// Parameters:
//   - resolver: Resolves the per-share LockManager for lease operations.
//   - notifier: The transport-level notifier for sending break notifications
//     to SMB clients. May be nil if break notifications are not yet wired.
func NewLeaseManager(resolver LockManagerResolver, notifier LeaseBreakNotifier) *LeaseManager {
	return &LeaseManager{
		resolver:   resolver,
		notifier:   notifier,
		sessionMap: make(map[string]uint64),
		leaseShare: make(map[string]string),
	}
}

// RequestLease requests a lease through the shared LockManager and records
// the sessionID mapping for break notifications.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file handle for the lease
//   - leaseKey: Client-generated 128-bit key identifying the lease
//   - parentLeaseKey: Parent directory lease key (V2 only, zero for V1)
//   - sessionID: The SMB session ID (for break notification routing)
//   - ownerID: The lock owner identifier
//   - clientID: The connection tracker client ID
//   - shareName: The share name
//   - requestedState: Requested R/W/H state flags
//   - isDirectory: True if the target is a directory
//
// Returns the granted state, epoch, and any error.
func (lm *LeaseManager) RequestLease(
	ctx context.Context,
	fileHandle lock.FileHandle,
	leaseKey [16]byte,
	parentLeaseKey [16]byte,
	sessionID uint64,
	ownerID string,
	clientID string,
	shareName string,
	requestedState uint32,
	isDirectory bool,
) (grantedState uint32, epoch uint16, err error) {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return lock.LeaseStateNone, 0, fmt.Errorf("no lock manager for share %q", shareName)
	}

	// Delegate to shared LockManager
	grantedState, epoch, err = lockMgr.RequestLease(
		ctx, fileHandle, leaseKey, parentLeaseKey,
		ownerID, clientID, shareName,
		requestedState, isDirectory,
	)
	if err != nil && !errors.Is(err, lock.ErrLeaseBreakInProgress) {
		return 0, 0, err
	}

	// Record session mapping if lease was granted (or is in breaking state).
	// For ErrLeaseBreakInProgress, grantedState contains the current breaking
	// state which is non-None — we still need the session map entry for
	// break-notification routing.
	if grantedState != lock.LeaseStateNone {
		keyHex := fmt.Sprintf("%x", leaseKey)
		lm.mu.Lock()
		lm.sessionMap[keyHex] = sessionID
		lm.leaseShare[keyHex] = shareName
		lm.mu.Unlock()
	}

	return grantedState, epoch, err
}

// AcknowledgeLeaseBreak delegates to the shared LockManager.
//
// If the lease has already been released (e.g. the client sent CLOSE before
// the break ack arrived), the acknowledgment is treated as a successful no-op.
// Per MS-SMB2 3.3.5.22.2, a break ack for a lease that no longer exists is
// not an error condition -- the desired state (lease relinquished) has already
// been achieved.
func (lm *LeaseManager) AcknowledgeLeaseBreak(
	ctx context.Context,
	leaseKey [16]byte,
	acknowledgedState uint32,
	epoch uint16,
) error {
	keyHex := fmt.Sprintf("%x", leaseKey)

	// Resolve the LockManager for this lease's share
	lm.mu.RLock()
	shareName := lm.leaseShare[keyHex]
	lm.mu.RUnlock()

	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		// Lease was already released (CLOSE cleaned up the maps).
		// The break ack is a no-op -- the lease is already gone.
		logger.Debug("AcknowledgeLeaseBreak: lease already released, treating as success",
			"leaseKey", keyHex)
		return nil
	}

	err := lockMgr.AcknowledgeLeaseBreak(ctx, leaseKey, acknowledgedState, epoch)
	if err != nil {
		// If the underlying lock manager says "no lease found", the lease was
		// released between our map lookup and the ack call. Treat as success.
		if strings.Contains(err.Error(), "no lease found") {
			logger.Debug("AcknowledgeLeaseBreak: lease not found in lock manager, treating as success",
				"leaseKey", keyHex)
			// Clean up our maps if they still have stale entries
			lm.mu.Lock()
			delete(lm.sessionMap, keyHex)
			delete(lm.leaseShare, keyHex)
			lm.mu.Unlock()
			return nil
		}
		return err
	}

	// If acknowledged to None, remove from session map
	if acknowledgedState == lock.LeaseStateNone {
		lm.mu.Lock()
		delete(lm.sessionMap, keyHex)
		delete(lm.leaseShare, keyHex)
		lm.mu.Unlock()
	}

	return nil
}

// ReleaseLease delegates to the shared LockManager and removes the session mapping.
func (lm *LeaseManager) ReleaseLease(ctx context.Context, leaseKey [16]byte) error {
	keyHex := fmt.Sprintf("%x", leaseKey)

	// Resolve the LockManager for this lease's share
	lm.mu.RLock()
	shareName := lm.leaseShare[keyHex]
	lm.mu.RUnlock()

	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		// Already released or no manager
		lm.mu.Lock()
		delete(lm.sessionMap, keyHex)
		delete(lm.leaseShare, keyHex)
		lm.mu.Unlock()
		return nil
	}

	err := lockMgr.ReleaseLease(ctx, leaseKey)
	if err != nil {
		return err
	}

	lm.mu.Lock()
	delete(lm.sessionMap, keyHex)
	delete(lm.leaseShare, keyHex)
	lm.mu.Unlock()

	return nil
}

// ReleaseSessionLeases releases all leases owned by a session.
// This is called during session cleanup (LOGOFF / connection close).
func (lm *LeaseManager) ReleaseSessionLeases(ctx context.Context, sessionID uint64) error {
	lm.mu.RLock()
	// Collect all lease keys for this session
	var keysToRelease [][16]byte
	for keyHex, sid := range lm.sessionMap {
		if sid == sessionID {
			var key [16]byte
			if b, err := hex.DecodeString(keyHex); err == nil && len(b) == 16 {
				copy(key[:], b)
			} else {
				logger.Warn("LeaseManager: invalid lease key hex", "keyHex", keyHex, "error", err)
				continue
			}
			keysToRelease = append(keysToRelease, key)
		}
	}
	lm.mu.RUnlock()

	// Release each lease
	for _, key := range keysToRelease {
		if err := lm.ReleaseLease(ctx, key); err != nil {
			logger.Warn("LeaseManager: failed to release session lease",
				"sessionID", sessionID,
				"leaseKey", fmt.Sprintf("%x", key),
				"error", err)
			// Continue releasing other leases
		}
	}

	return nil
}

// GetLeaseState delegates to the shared LockManager.
func (lm *LeaseManager) GetLeaseState(ctx context.Context, leaseKey [16]byte) (state uint32, epoch uint16, found bool) {
	keyHex := fmt.Sprintf("%x", leaseKey)

	lm.mu.RLock()
	shareName := lm.leaseShare[keyHex]
	lm.mu.RUnlock()

	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return lock.LeaseStateNone, 0, false
	}

	return lockMgr.GetLeaseState(ctx, leaseKey)
}

// GetSessionForLease returns the sessionID associated with a lease key.
func (lm *LeaseManager) GetSessionForLease(leaseKey [16]byte) (sessionID uint64, found bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	sid, ok := lm.sessionMap[fmt.Sprintf("%x", leaseKey)]
	return sid, ok
}

// UpdateSessionForLease updates the session ID associated with a lease key.
// Used during durable handle reconnect to associate the existing lease with
// the new session for break notification routing.
func (lm *LeaseManager) UpdateSessionForLease(leaseKey [16]byte, sessionID uint64) {
	keyHex := fmt.Sprintf("%x", leaseKey)
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.sessionMap[keyHex] = sessionID
}

// SetNotifier sets the lease break notifier for sending break notifications.
func (lm *LeaseManager) SetNotifier(notifier LeaseBreakNotifier) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.notifier = notifier
}

// GetNotifier returns the current lease break notifier.
func (lm *LeaseManager) GetNotifier() LeaseBreakNotifier {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.notifier
}

// RegisterOplockFileID registers a synthetic lease key → FileID mapping
// for traditional oplock break notification support.
func (lm *LeaseManager) RegisterOplockFileID(leaseKey [16]byte, fileID [16]byte) {
	lm.mu.RLock()
	notifier := lm.notifier
	lm.mu.RUnlock()
	if reg, ok := notifier.(OplockFileIDRegistrar); ok {
		reg.RegisterOplockFileID(leaseKey, fileID)
	}
}

// UnregisterOplockFileID removes a synthetic lease key → FileID mapping.
func (lm *LeaseManager) UnregisterOplockFileID(leaseKey [16]byte) {
	lm.mu.RLock()
	notifier := lm.notifier
	lm.mu.RUnlock()
	if reg, ok := notifier.(OplockFileIDRegistrar); ok {
		reg.UnregisterOplockFileID(leaseKey)
	}
}

// BreakConflictingOplocksOnOpen breaks any existing oplocks/leases that conflict
// with a new open operation on a file. Per MS-SMB2 3.3.5.9, this must happen
// regardless of whether the new opener requests an oplock/lease.
//
// Both read and write opens break Write leases (strip W, preserve R+H).
// excludeOwner is optional and can contain ExcludeLeaseKey to prevent
// breaking same-key leases (nobreakself per MS-SMB2).
func (lm *LeaseManager) BreakConflictingOplocksOnOpen(
	fileHandle lock.FileHandle,
	shareName string,
	excludeOwner ...*lock.LockOwner,
) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	handleKey := string(fileHandle)

	var exclude *lock.LockOwner
	if len(excludeOwner) > 0 {
		exclude = excludeOwner[0]
	}

	// Use SMB-specific break method that strips only the Write bit
	// (preserves Read and Handle), per MS-SMB2 3.3.5.9.
	// Both read and write opens break Write leases (strip W, preserve R+H).
	// This is different from cross-protocol breaks which go to NONE.
	return lockMgr.CheckAndBreakLeasesForSMBOpen(handleKey, exclude)
}

// BreakHandleLeasesOnOpen breaks Handle leases before share mode conflict check.
// Per MS-SMB2 3.3.5.9 Step 10: "If any existing Open on the target file has a
// lease with Handle caching, the server MUST initiate a lease break to remove
// Handle caching." This must happen BEFORE the share mode conflict check so that
// clients relinquish cached handles, avoiding spurious SHARING_VIOLATION errors.
//
// After breaking, the caller should wait for break completion and then re-check
// share mode conflicts.
// excludeOwner is optional and can contain ExcludeLeaseKey to prevent
// breaking same-key leases (nobreakself per MS-SMB2).
func (lm *LeaseManager) BreakHandleLeasesOnOpen(
	ctx context.Context,
	fileHandle lock.FileHandle,
	shareName string,
	excludeOwner ...*lock.LockOwner,
) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	handleKey := string(fileHandle)

	var exclude *lock.LockOwner
	if len(excludeOwner) > 0 {
		exclude = excludeOwner[0]
	}

	// Break Handle leases (RWH -> RW, RH -> R)
	if err := lockMgr.BreakHandleLeasesForSMBOpen(handleKey, exclude); err != nil {
		return err
	}

	// Wait for break to complete before caller re-checks share modes
	return lockMgr.WaitForBreakCompletion(ctx, handleKey)
}

// BreakHandleLeasesOnOpenAsync dispatches Handle lease break notifications
// without waiting for acknowledgment. Used for directory opens where share
// mode conflicts are not a concern and blocking would deadlock: the other
// client needs this CREATE's response before it processes the break.
// excludeOwner is optional and can contain ExcludeLeaseKey to prevent
// breaking same-key leases (nobreakself per MS-SMB2).
func (lm *LeaseManager) BreakHandleLeasesOnOpenAsync(
	fileHandle lock.FileHandle,
	shareName string,
	excludeOwner ...*lock.LockOwner,
) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	handleKey := string(fileHandle)

	var exclude *lock.LockOwner
	if len(excludeOwner) > 0 {
		exclude = excludeOwner[0]
	}

	return lockMgr.BreakHandleLeasesForSMBOpen(handleKey, exclude)
}

// BreakParentHandleLeasesOnCreate breaks Handle leases on a parent directory
// when a file is created, overwritten, or superseded inside it.
//
// Per MS-SMB2 3.3.5.9 and MS-FSA 2.1.5.1.2.1: modifying directory contents
// (create/overwrite/supersede) must break Handle leases on the directory for
// other clients. The creating client's leases are excluded via excludeClientID.
//
// This enables BVT_DirectoryLeasing_LeaseBreakOnMultiClients: when one client
// creates a file, other clients holding RH leases on the parent directory
// receive a lease break notification.
//
// resolveParentBreakArgs resolves the lock manager, handle key, and exclude
// owner for parent directory lease break operations. Returns nil lockMgr if
// the share has no lock manager.
func (lm *LeaseManager) resolveParentBreakArgs(
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
) (lockMgr lock.LockManager, handleKey string, excludeOwner *lock.LockOwner) {
	lockMgr = lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil, "", nil
	}
	handleKey = string(parentHandle)
	if excludeClientID != "" {
		excludeOwner = &lock.LockOwner{ClientID: excludeClientID}
	}
	return lockMgr, handleKey, excludeOwner
}

// BreakParentHandleLeasesOnCreate breaks Handle leases on a parent directory
// when a child is created, overwritten, or superseded (RH -> R, RWH -> RW).
//
// This does NOT wait for the break to complete. The parent directory break is
// an informational notification: the file creation has already committed, and
// the other client just needs to invalidate its cached directory handle.
func (lm *LeaseManager) BreakParentHandleLeasesOnCreate(
	_ context.Context,
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
) error {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(parentHandle, shareName, excludeClientID)
	if lockMgr == nil {
		return nil
	}
	return lockMgr.BreakHandleLeasesForSMBOpen(handleKey, excludeOwner)
}

// BreakParentReadLeasesOnModify breaks Read leases on a parent directory
// when a child file's metadata is modified via SET_INFO, WRITE, or DELETE.
// Per MS-FSA 2.1.5.14: changes to directory contents invalidate Read caching,
// so clients holding R or RW leases on the directory must be notified.
// Breaks to None (full revocation of Read caching).
func (lm *LeaseManager) BreakParentReadLeasesOnModify(
	_ context.Context,
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
) error {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(parentHandle, shareName, excludeClientID)
	if lockMgr == nil {
		return nil
	}
	return lockMgr.BreakReadLeasesForParentDir(handleKey, excludeOwner)
}

// SetLeaseEpoch sets the epoch on an existing lease identified by leaseKey.
// Per MS-SMB2 3.3.5.9: For V2 leases, the server should track the client's
// epoch from the RqLs create context.
func (lm *LeaseManager) SetLeaseEpoch(leaseKey [16]byte, epoch uint16) {
	keyHex := fmt.Sprintf("%x", leaseKey)

	lm.mu.RLock()
	shareName := lm.leaseShare[keyHex]
	lm.mu.RUnlock()

	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return
	}

	lockMgr.SetLeaseEpoch(leaseKey, epoch)
}

// BreakReadLeasesOnWrite breaks Read (Level II) oplocks/leases held by other
// opens on a file when a WRITE is performed. Per MS-SMB2 3.3.5.16, writes must
// break all Read caching on the file so that other clients see the updated data.
//
// The writer's own lease (identified by excludeLeaseKey) is NOT broken.
// Read leases are broken to None (complete revocation).
func (lm *LeaseManager) BreakReadLeasesOnWrite(
	fileHandle lock.FileHandle,
	shareName string,
	excludeLeaseKey [16]byte,
) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	handleKey := string(fileHandle)

	var exclude *lock.LockOwner
	if excludeLeaseKey != ([16]byte{}) {
		exclude = &lock.LockOwner{ExcludeLeaseKey: excludeLeaseKey}
	}

	// Break all Read/Write leases to None. The writer's own lease is excluded
	// via ExcludeLeaseKey. This ensures Level II (Read-only) leases from other
	// clients are broken when data changes.
	return lockMgr.CheckAndBreakOpLocksForWrite(handleKey, exclude)
}

// LeaseCount returns the number of active leases tracked by this manager.
// Used for state debugging instrumentation.
func (lm *LeaseManager) LeaseCount() int {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return len(lm.sessionMap)
}

// RangeLeases iterates over all tracked leases, calling fn for each.
// The callback receives (leaseKeyHex, sessionID, shareName).
// Return false to stop iteration. Used for state debugging instrumentation.
func (lm *LeaseManager) RangeLeases(fn func(leaseKeyHex string, sessionID uint64, shareName string) bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for keyHex, sid := range lm.sessionMap {
		shareName := lm.leaseShare[keyHex]
		if !fn(keyHex, sid, shareName) {
			return
		}
	}
}

// resolveLockManager resolves the LockManager for a share name.
func (lm *LeaseManager) resolveLockManager(shareName string) lock.LockManager {
	if lm.resolver == nil || shareName == "" {
		return nil
	}
	return lm.resolver.GetLockManagerForShare(shareName)
}
