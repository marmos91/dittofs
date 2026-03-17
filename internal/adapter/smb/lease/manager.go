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
	"fmt"
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
	if err != nil {
		return 0, 0, err
	}

	// Record session mapping if lease was granted
	if grantedState != lock.LeaseStateNone {
		keyHex := fmt.Sprintf("%x", leaseKey)
		lm.mu.Lock()
		lm.sessionMap[keyHex] = sessionID
		lm.leaseShare[keyHex] = shareName
		lm.mu.Unlock()
	}

	return grantedState, epoch, nil
}

// AcknowledgeLeaseBreak delegates to the shared LockManager.
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
		return fmt.Errorf("no lock manager for lease %s (share %q)", keyHex, shareName)
	}

	err := lockMgr.AcknowledgeLeaseBreak(ctx, leaseKey, acknowledgedState, epoch)
	if err != nil {
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

// resolveLockManager resolves the LockManager for a share name.
func (lm *LeaseManager) resolveLockManager(shareName string) lock.LockManager {
	if lm.resolver == nil || shareName == "" {
		return nil
	}
	return lm.resolver.GetLockManagerForShare(shareName)
}
