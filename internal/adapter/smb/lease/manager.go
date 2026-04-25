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
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// parentLeaseBreakWaitTimeout bounds how long a CREATE/MODIFY waits for other
// clients to acknowledge a parent-directory lease break. On expiry,
// WaitForBreakCompletion's forceCompleteBreaks path auto-downgrades the lease
// state, yielding a deterministic post-break view.
//
// Required by WPTS BVT BVT_DirectoryLeasing_LeaseBreakOnMultiClients and
// MS-SMB2 3.3.4.7 (server must wait for LEASE_BREAK_ACK when
// SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED is set).
const parentLeaseBreakWaitTimeout = 5 * time.Second

// handleLeaseBreakWaitTimeout bounds how long a CREATE waits for the existing
// lease holder to acknowledge a Handle-strip break before falling back to
// forceCompleteBreaks (auto-downgrade) and proceeding to the share-mode check.
//
// Without a bound, the wait inherits the auth context which only cancels on
// session disconnect — non-acking clients hang the conflicting open for as
// long as the test harness tolerates. Samba bounds this at ~32 s
// (2× OPLOCK_BREAK_TIMEOUT, schedule_defer_open in source3/smbd/open.c); we
// use the same 5 s as the parent break for consistency.
const handleLeaseBreakWaitTimeout = 5 * time.Second

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
	// leaseV2 records whether each lease was granted from an
	// SMB2_CREATE_REQUEST_LEASE_V2 context. Per MS-SMB2 §2.2.23.2 the
	// NewEpoch field of a break notification MUST be zero for V1 leases;
	// for V2 leases it carries the incremented lease epoch. Sending a
	// non-zero NewEpoch on a V1 break trips the client (#417 root cause
	// for smb2.multichannel.leases.test1-3).
	leaseV2 map[string]bool // hex(leaseKey) -> true iff V2 lease
	mu      sync.RWMutex
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
		leaseV2:    make(map[string]bool),
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

	// Pre-register the session mapping BEFORE creating the lease in the
	// LockManager. The LockManager's RequestLease may trigger cross-key
	// conflict breaks, which dispatch through breakOpLocks → SMBBreakHandler.
	// If the session mapping isn't set yet, the break notification can't be
	// routed to the correct SMB client. Similarly, another goroutine's
	// BreakHandleLeasesOnOpenAsync may fire between the LockManager grant
	// and the session map update, causing a "no session" miss.
	//
	// Pre-registering is safe: if the grant fails or returns None, we
	// remove the entry below.
	keyHex := hex.EncodeToString(leaseKey[:])
	lm.mu.Lock()
	lm.sessionMap[keyHex] = sessionID
	lm.leaseShare[keyHex] = shareName
	lm.mu.Unlock()

	// Delegate to shared LockManager
	grantedState, epoch, err = lockMgr.RequestLease(
		ctx, fileHandle, leaseKey, parentLeaseKey,
		ownerID, clientID, shareName,
		requestedState, isDirectory,
	)
	if err != nil && !errors.Is(err, lock.ErrLeaseBreakInProgress) {
		lm.removeLeaseMapping(keyHex)
		return 0, 0, err
	}

	// Remove pre-registered mapping if the lease was denied (None state means
	// the LockManager rejected the request without an error code).
	if grantedState == lock.LeaseStateNone {
		lm.removeLeaseMapping(keyHex)
	}

	return grantedState, epoch, err
}

// AcknowledgeLeaseBreak delegates to the shared LockManager.
//
// Two distinct "lease no longer exists" scenarios must produce different wire
// responses:
//
//   - CLOSE-beat-ACK race: the holder closed the file (and the wrapper-side
//     leaseShare mapping was reaped) before the ack landed. The break has
//     already been resolved by the close path; per MS-SMB2 3.3.5.22.2 the
//     desired state is achieved, so the wrapper returns success silently.
//     Required by WPTS BVT_DirectoryLeasing_*.
//   - Re-ack of an already-acked NONE: the lock-manager lease record is
//     gone but the wrapper-side leaseShare entry is still present (we keep
//     it across ack-to-NONE for exactly this reason). lockMgr returns
//     ErrLeaseAckNotFound, which we propagate so the handler maps it to
//     STATUS_UNSUCCESSFUL. Required by smbtorture breaking2 / breaking5.
func (lm *LeaseManager) AcknowledgeLeaseBreak(
	ctx context.Context,
	leaseKey [16]byte,
	acknowledgedState uint32,
	epoch uint16,
) error {
	keyHex := hex.EncodeToString(leaseKey[:])

	lm.mu.RLock()
	shareName := lm.leaseShare[keyHex]
	lm.mu.RUnlock()

	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		// CLOSE-beat-ACK: the desired state (lease relinquished) is already
		// achieved. Treat as success.
		logger.Debug("AcknowledgeLeaseBreak: lease already released, treating as success",
			"leaseKey", keyHex)
		return nil
	}

	err := lockMgr.AcknowledgeLeaseBreak(ctx, leaseKey, acknowledgedState, epoch)
	if err != nil {
		// Re-ack or stale ack: propagate to the wire as STATUS_UNSUCCESSFUL.
		// Do NOT reap leaseShare on ErrLeaseAckNotFound — keeping it ensures
		// further re-acks continue surfacing the error rather than silently
		// succeeding via the lockMgr==nil branch above. The wrapper-side
		// mapping is reaped exclusively by ReleaseLease / ReleaseLeaseForHandle
		// on the CLOSE path.
		return err
	}

	return nil
}

// ReleaseLease delegates to the shared LockManager and removes the session mapping.
func (lm *LeaseManager) ReleaseLease(ctx context.Context, leaseKey [16]byte) error {
	keyHex := hex.EncodeToString(leaseKey[:])

	// Resolve the LockManager for this lease's share
	lm.mu.RLock()
	shareName := lm.leaseShare[keyHex]
	lm.mu.RUnlock()

	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		// Already released or no manager
		lm.removeLeaseMapping(keyHex)
		return nil
	}

	err := lockMgr.ReleaseLease(ctx, leaseKey)
	if err != nil {
		return err
	}

	lm.removeLeaseMapping(keyHex)
	return nil
}

// ReleaseLeaseForHandle releases lease records only under a specific handleKey
// bucket. Used by CLOSE so that opens on OTHER files sharing the same
// LeaseKey constant (typical in smbtorture, which reuses fixed LEASE1/LEASE2
// macros across tests) retain their records. The session/share mappings are
// only torn down when the last record for the key is gone.
func (lm *LeaseManager) ReleaseLeaseForHandle(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, shareName string) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	if err := lockMgr.ReleaseLeaseForHandle(ctx, string(fileHandle), leaseKey); err != nil {
		return err
	}

	// Only drop session/share mappings if no lease records remain anywhere for
	// this key — otherwise a concurrent open on a different file would lose
	// break-dispatch routing.
	if _, _, found := lockMgr.GetLeaseState(ctx, leaseKey); !found {
		lm.removeLeaseMapping(hex.EncodeToString(leaseKey[:]))
	}
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
	keyHex := hex.EncodeToString(leaseKey[:])

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
	sid, ok := lm.sessionMap[hex.EncodeToString(leaseKey[:])]
	return sid, ok
}

// UpdateSessionForLease updates the session ID associated with a lease key.
// Used during durable handle reconnect to associate the existing lease with
// the new session for break notification routing.
func (lm *LeaseManager) UpdateSessionForLease(leaseKey [16]byte, sessionID uint64) {
	keyHex := hex.EncodeToString(leaseKey[:])
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

// HasOtherBreakingLeases reports whether any lease on fileHandle except excludeKey
// is currently Breaking. Non-blocking. Used by the SMB CREATE async-park path
// to decide whether to emit STATUS_PENDING after dispatching the break.
// Returns false when no LockManager is bound for the share.
func (lm *LeaseManager) HasOtherBreakingLeases(fileHandle lock.FileHandle, shareName string, excludeKey [16]byte) bool {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return false
	}
	return lockMgr.HasOtherBreakingLeases(string(fileHandle), excludeKey)
}

// WaitForOtherKeyBreaks waits on ctx for all breaks on fileHandle other than
// excludeKey to drain. The caller controls the cancellation context — the
// SMB CREATE async-park path passes a context whose lifetime is bound to
// session teardown + a bounded server-side timeout. On ctx.Err, breaks on
// non-excluded keys are auto-downgraded exactly as the synchronous timeout
// path does (see Manager.forceCompleteBreaksExceptKey).
//
// A zero excludeKey means "no exclusion" — wait for every Breaking lease to
// drain, routed to WaitForBreakCompletion.
func (lm *LeaseManager) WaitForOtherKeyBreaks(ctx context.Context, fileHandle lock.FileHandle, shareName string, excludeKey [16]byte) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}
	if excludeKey == ([16]byte{}) {
		return lockMgr.WaitForBreakCompletion(ctx, string(fileHandle))
	}
	return lockMgr.WaitForBreakCompletionExceptKey(ctx, string(fileHandle), excludeKey)
}

// AsyncCreateBreakWaitTimeout bounds the server-side wait for a parked CREATE.
// Matches handleLeaseBreakWaitTimeout so sync and async paths have identical
// auto-downgrade timing — the difference is that async emits an interim
// STATUS_PENDING first, letting the client observe the request as cancellable.
const AsyncCreateBreakWaitTimeout = handleLeaseBreakWaitTimeout

// BreakHandleLeasesOnOpenAsync dispatches lease break notifications without
// waiting for acknowledgment. Used for directory opens where blocking would
// deadlock the single-threaded test driver: the other client only acks after
// this CREATE returns.
//
// reason selects the per-lease break-to mask via ComputeLeaseBreakTo
// (Default → strip W, SharingViolation → strip H, Destructive → break to None).
// excludeOwner is optional and can contain ExcludeLeaseKey to prevent
// breaking same-key leases (nobreakself per MS-SMB2).
func (lm *LeaseManager) BreakHandleLeasesOnOpenAsync(
	fileHandle lock.FileHandle,
	shareName string,
	reason lock.BreakReason,
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

	return lockMgr.BreakLeasesOnOpenConflict(handleKey, exclude, reason)
}

// BreakFileHandleLeasesOnDelete strips Handle caching from all leases on a
// file that is about to be unlinked (RH → R, RWH → RW). Per MS-FSA 2.1.5.1.5
// and Samba: deleting a file invalidates Handle caching for every other open
// (the reopen path no longer exists), but Read and Write remain valid for as
// long as the in-flight handles stay alive.
//
// Async dispatch: the break is triggered from the close/TDIS/LOGOFF/disconnect
// teardown path, where the lease holder is a DIFFERENT session on the same
// transport. Waiting for the ACK here would deadlock the in-flight SMB
// request; the holder acks on its own transport after we return.
//
// Required by smbtorture smb2.lease.initial_delete_tdis / logoff / disconnect.
func (lm *LeaseManager) BreakFileHandleLeasesOnDelete(
	fileHandle lock.FileHandle,
	shareName string,
	excludeOwner ...*lock.LockOwner,
) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	var exclude *lock.LockOwner
	if len(excludeOwner) > 0 {
		exclude = excludeOwner[0]
	}
	// SharingViolation reason selects the strip-Handle mask via
	// ComputeLeaseBreakTo; the triggering "conflict" here is the unlink,
	// not a share-mode violation, but the break-to outcome is identical.
	return lockMgr.BreakLeasesOnOpenConflict(string(fileHandle), exclude, lock.BreakReasonSharingViolation)
}

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
// Per MS-SMB2 3.3.4.7, the server MUST wait for LEASE_BREAK_ACK when the break
// is sent with SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED set, before completing
// the triggering CREATE. The wait is bounded by parentLeaseBreakWaitTimeout;
// on expiry, WaitForBreakCompletion's forceCompleteBreaks path auto-downgrades
// the lease state so the post-break view is deterministic.
//
// Self-deadlock is impossible because excludeClientID removes the triggering
// CREATE's own session from the breakable set: breakOpLocks (manager.go) honors
// excludeOwner.ClientID so the triggering session's parent-dir lease (if any)
// is never in the toBreak set, and the wait only blocks on OTHER clients' acks.
//
// Required by WPTS BVT BVT_DirectoryLeasing_LeaseBreakOnMultiClients.
func (lm *LeaseManager) BreakParentHandleLeasesOnCreate(
	ctx context.Context,
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
) error {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(parentHandle, shareName, excludeClientID)
	if lockMgr == nil {
		return nil
	}
	// Parent directory Handle-lease break on child create: strip Handle
	// (not Write) so cached entries are invalidated. SharingViolation
	// reason selects the Handle-strip mask in ComputeLeaseBreakTo;
	// semantically this is MS-FSA 2.1.5.14 (child-set change invalidates
	// directory Handle caching), not a share-mode violation, but the
	// break-to matrix collapses to the same strip-H outcome.
	if err := lockMgr.BreakLeasesOnOpenConflict(handleKey, excludeOwner, lock.BreakReasonSharingViolation); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, parentLeaseBreakWaitTimeout)
	defer cancel()
	return lockMgr.WaitForBreakCompletion(waitCtx, handleKey)
}

// BreakParentReadLeasesOnModify breaks Read leases on a parent directory
// when a child file's metadata is modified via SET_INFO, WRITE, or DELETE.
// Per MS-FSA 2.1.5.14: changes to directory contents invalidate Read caching,
// so clients holding R or RW leases on the directory must be notified.
// Breaks to None (full revocation of Read caching).
//
// Per MS-SMB2 3.3.4.7, the server waits for LEASE_BREAK_ACK before completing
// the triggering operation; the wait is bounded by parentLeaseBreakWaitTimeout
// and self-deadlock is prevented by excludeClientID (see
// BreakParentHandleLeasesOnCreate for the full rationale).
func (lm *LeaseManager) BreakParentReadLeasesOnModify(
	ctx context.Context,
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
) error {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(parentHandle, shareName, excludeClientID)
	if lockMgr == nil {
		return nil
	}
	if err := lockMgr.BreakReadLeasesForParentDir(handleKey, excludeOwner); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, parentLeaseBreakWaitTimeout)
	defer cancel()
	return lockMgr.WaitForBreakCompletion(waitCtx, handleKey)
}

// SetLeaseEpoch sets the epoch on an existing lease identified by leaseKey.
// Per MS-SMB2 3.3.5.9: For V2 leases, the server should track the client's
// epoch from the RqLs create context.
func (lm *LeaseManager) SetLeaseEpoch(leaseKey [16]byte, epoch uint16) {
	keyHex := hex.EncodeToString(leaseKey[:])

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

// removeLeaseMapping removes a lease key from the session and share maps.
// Must be called without lm.mu held.
func (lm *LeaseManager) removeLeaseMapping(keyHex string) {
	lm.mu.Lock()
	delete(lm.sessionMap, keyHex)
	delete(lm.leaseShare, keyHex)
	delete(lm.leaseV2, keyHex)
	lm.mu.Unlock()
}

// MarkLeaseV2 records that the lease with the given key was granted from an
// SMB2_CREATE_REQUEST_LEASE_V2 context. Callers must invoke this after a
// successful RequestLease whenever the originating create context was V2 so
// that subsequent break notifications carry the epoch per MS-SMB2 §2.2.23.2.
// Leases not marked are treated as V1 and get NewEpoch = 0 on break.
func (lm *LeaseManager) MarkLeaseV2(leaseKey [16]byte) {
	keyHex := hex.EncodeToString(leaseKey[:])
	lm.mu.Lock()
	lm.leaseV2[keyHex] = true
	lm.mu.Unlock()
}

// IsV2 reports whether the lease was granted from a V2 create context.
// Returns false for unknown keys (safe default: treat as V1 and send
// NewEpoch = 0 rather than leak a non-zero epoch).
func (lm *LeaseManager) IsV2(leaseKey [16]byte) bool {
	keyHex := hex.EncodeToString(leaseKey[:])
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.leaseV2[keyHex]
}

// resolveLockManager resolves the LockManager for a share name.
func (lm *LeaseManager) resolveLockManager(shareName string) lock.LockManager {
	if lm.resolver == nil || shareName == "" {
		return nil
	}
	return lm.resolver.GetLockManagerForShare(shareName)
}
