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
	//
	// Sticky version semantics: per smbtorture v2_epoch2 / v2_epoch3, the
	// lease's protocol version is set on the FIRST grant for a given key
	// and does not change across reopens — even when a subsequent request
	// uses the other version's create-context format. To distinguish
	// V1-established (mark exists, value false) from
	// version-not-yet-known (mark absent), we track BOTH versions
	// explicitly via parallel maps; a single bool can't carry the third
	// state. leaseV1 is true iff first grant was V1.
	leaseV2 map[string]bool // hex(leaseKey) -> true iff V2-established
	leaseV1 map[string]bool // hex(leaseKey) -> true iff V1-established

	// leaseClientGUID records the ClientGUID that first granted each lease.
	// Per MS-SMB2 §3.3.5.9.8 a lease is bound to a (ClientGUID, LeaseKey) pair
	// and break notifications are routed at the client level (Samba
	// `smbXsrv_pending_break_submit` in source3/smbd/smb2_server.c picks the
	// FIRST connection of `client->connections` regardless of which session
	// holds the open). Sticky on FIRST grant: a same-key reopen on a
	// different session of the same client does NOT change the recorded
	// GUID; cross-client key reuse is rejected upstream by lease_match.
	//
	// Required by smbtorture smb2.lease.v2_complex1 — two sessions of the
	// same ClientGUID open with different lease keys, and breaks for either
	// lease must arrive on the FIRST session's primary transport.
	leaseClientGUID map[string][16]byte // hex(leaseKey) -> ClientGUID

	// clientPrimarySession records the FIRST sessionID seen for each
	// ClientGUID (first-write wins). When a lease must be broken, its
	// recorded ClientGUID is resolved to this primary sessionID and the
	// notifier delivers on that session's primary connection. This mirrors
	// Samba's `client->connections` head: breaks always go to the oldest
	// live connection for the client, not to whichever session created the
	// open. Zero ClientGUID is never registered (would conflate clients
	// that never sent a NEGOTIATE).
	clientPrimarySession map[[16]byte]uint64

	mu sync.RWMutex
}

// NewLeaseManager creates a new SMB LeaseManager.
//
// Parameters:
//   - resolver: Resolves the per-share LockManager for lease operations.
//   - notifier: The transport-level notifier for sending break notifications
//     to SMB clients. May be nil if break notifications are not yet wired.
func NewLeaseManager(resolver LockManagerResolver, notifier LeaseBreakNotifier) *LeaseManager {
	return &LeaseManager{
		resolver:             resolver,
		notifier:             notifier,
		sessionMap:           make(map[string]uint64),
		leaseShare:           make(map[string]string),
		leaseV2:              make(map[string]bool),
		leaseV1:              make(map[string]bool),
		leaseClientGUID:      make(map[string][16]byte),
		clientPrimarySession: make(map[[16]byte]uint64),
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
//   - clientGUID: The 128-bit ClientGUID from the request's connection
//     (NEGOTIATE). Used to bind the lease to its client at MS-SMB2 §3.3.5.9.8
//     granularity and to route break notifications to the client's primary
//     session (Samba `client->connections` head). Zero is accepted (no
//     ClientGUID-based routing for that lease — falls back to per-lease
//     sessionMap), which keeps callers that don't have a CryptoState wired
//     (older durable-reconnect paths, tests) working.
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
	clientGUID [16]byte,
	ownerID string,
	clientID string,
	shareName string,
	requestedState uint32,
	isDirectory bool,
) (grantedState uint32, epoch uint16, err error) {
	return lm.requestLeaseInternal(ctx, fileHandle, leaseKey, parentLeaseKey,
		sessionID, ownerID, clientID, shareName, requestedState, isDirectory, false)
}

// RequestLeaseAsOplock is the traditional-oplock variant of RequestLease.
// CREATE handlers route LEVEL_II / Exclusive / Batch oplock requests through
// this method (under a synthetic lease key derived from the FileID) so the
// underlying lock manager tags the resulting record `IsTraditionalOplock`
// and can apply the MS-SMB2 §3.3.5.9 cross-tier rules during subsequent
// grants. See `bestGrantableState` in `pkg/metadata/lock/leases.go`.
func (lm *LeaseManager) RequestLeaseAsOplock(
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
	return lm.requestLeaseInternal(ctx, fileHandle, leaseKey, parentLeaseKey,
		sessionID, ownerID, clientID, shareName, requestedState, isDirectory, true)
}

// requestLeaseInternal is the shared body of RequestLease /
// RequestLeaseAsOplock; the only behavior change between the two is which
// underlying Manager method we dispatch to so the new record gets the
// correct IsTraditionalOplock tag and the cross-tier rules in
// bestGrantableState see the right requestor tier.
func (lm *LeaseManager) requestLeaseInternal(
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
	isTraditionalOplock bool,
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
	// Bind the lease to a ClientGUID on FIRST grant (sticky). Cross-client
	// key reuse is rejected upstream by lease_match (ErrLeaseKeyInUse), so
	// the only paths that re-enter here on the same key are same-client
	// reopens / upgrades — those must NOT rebind the GUID. Zero ClientGUID
	// callers (legacy paths) leave the binding unset and fall back to the
	// per-lease sessionMap for break dispatch.
	if clientGUID != ([16]byte{}) {
		if _, bound := lm.leaseClientGUID[keyHex]; !bound {
			lm.leaseClientGUID[keyHex] = clientGUID
		}
		// Register this session as the primary for the ClientGUID iff no
		// session is currently registered (first-write wins). Mirrors the
		// Samba head-of-list semantics for `client->connections`: the first
		// connection of the client receives all break notifications even
		// when subsequent sessions of the same client open additional opens
		// or leases (smbtorture v2_complex1 line 4006/4033/4047 expect every
		// lease break on tree1a's transport, the connection set up first).
		if _, ok := lm.clientPrimarySession[clientGUID]; !ok {
			lm.clientPrimarySession[clientGUID] = sessionID
		}
	}
	lm.mu.Unlock()

	// Dispatch to the appropriate Manager method so the new record's
	// IsTraditionalOplock tag is set correctly. The LockManager interface
	// deliberately stays narrow (no oplock variant): when the configured
	// store is a *lock.Manager (the only production impl) and this is a
	// traditional-oplock request, call the tagged method; otherwise fall
	// through to the plain interface call so test doubles keep working.
	if mgr, ok := lockMgr.(*lock.Manager); ok && isTraditionalOplock {
		grantedState, epoch, err = mgr.RequestLeaseAsOplock(
			ctx, fileHandle, leaseKey, parentLeaseKey,
			ownerID, clientID, shareName,
			requestedState, isDirectory,
		)
	} else {
		grantedState, epoch, err = lockMgr.RequestLease(
			ctx, fileHandle, leaseKey, parentLeaseKey,
			ownerID, clientID, shareName,
			requestedState, isDirectory,
		)
	}
	if err != nil && !errors.Is(err, lock.ErrLeaseBreakInProgress) {
		lm.removeLeaseMapping(keyHex)
		return 0, 0, err
	}

	// Remove pre-registered mapping only if the LockManager has no record
	// for this key. grantedState == None can mean either:
	//   - rejected request (no record created) — must reap pre-registration
	//   - successful None probe / existing released-to-None record — keep
	//     the mapping so a later unsolicited or duplicate ack still resolves
	//     and surfaces ErrLeaseAckNotBreaking (smbtorture breaking5).
	if grantedState == lock.LeaseStateNone {
		if _, _, found := lockMgr.GetLeaseState(ctx, leaseKey); !found {
			lm.removeLeaseMapping(keyHex)
		}
	}

	return grantedState, epoch, err
}

// AcknowledgeLeaseBreak delegates to the shared LockManager.
//
// Two failure modes are wire-indistinguishable from this layer but must
// produce different SMB statuses:
//
//   - Duplicate or unsolicited ack on a lease that has already been released
//     to None: smbtorture breaking2/breaking5 require STATUS_UNSUCCESSFUL
//     per MS-SMB2 3.3.5.22.2. The lock manager keeps the record alive at
//     LeaseState=None until CLOSE, so this surfaces as ErrLeaseAckNotBreaking
//     and propagates to the handler.
//
//   - CLOSE-beat-ACK race (client closed the handle before its own ack
//     arrived): the record is gone and the desired state is already achieved.
//     WPTS BVT_DirectoryLeasing_* requires silent success here. We detect
//     this via ErrLeaseAckNotFound (lock manager scrubbed the record on
//     ReleaseLeaseForHandle) or a missing wrapper-side mapping.
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
		logger.Debug("AcknowledgeLeaseBreak: no lock manager for lease (CLOSE-beat-ack), treating as success",
			"leaseKey", keyHex)
		return nil
	}

	err := lockMgr.AcknowledgeLeaseBreak(ctx, leaseKey, acknowledgedState, epoch)
	if err != nil {
		if errors.Is(err, lock.ErrLeaseAckNotFound) {
			logger.Debug("AcknowledgeLeaseBreak: lease record absent (CLOSE-beat-ack), treating as success",
				"leaseKey", keyHex)
			lm.removeLeaseMapping(keyHex)
			return nil
		}
		return err
	}

	// Do NOT reap leaseShare on ack-to-None: the lock manager keeps the
	// record alive at state=None until CLOSE, so a duplicate ack on the same
	// key must continue to find the lockMgr and surface
	// ErrLeaseAckNotBreaking. ReleaseLeaseForHandle clears the mapping when
	// no records remain (see GetLeaseState-found check there).
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

	// Reap any clientPrimarySession entries that pointed at the gone
	// session. Without this the next break for a lease bound to that
	// ClientGUID would route to a dead sessionID and the notifier would
	// silently drop the notification (see GetSessionForBreak fallback).
	// We do not re-elect a new primary here — the next CREATE / lease
	// upgrade from a surviving session will repopulate via the
	// first-write-wins path in RequestLease.
	lm.mu.Lock()
	for guid, sid := range lm.clientPrimarySession {
		if sid == sessionID {
			delete(lm.clientPrimarySession, guid)
		}
	}
	lm.mu.Unlock()

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

// GetSessionForBreak returns the sessionID that should receive a break
// notification for the given lease. Per MS-SMB2 §3.3.4.7 and Samba
// `smbXsrv_pending_break_submit` (source3/smbd/smb2_server.c) the break is
// delivered to the FIRST connection of `client->connections` — i.e. the
// oldest live connection of the lease's ClientGUID — irrespective of which
// session opened the file. When the lease has a recorded ClientGUID and a
// primary session is registered for that GUID, this method returns that
// session. Otherwise it falls back to the lease's per-record sessionMap
// entry (legacy callers without ClientGUID; durable-reconnect tests that
// don't thread a CryptoState).
//
// Required by smbtorture smb2.lease.v2_complex1, which opens two sessions
// of the same ClientGUID and asserts every lease break (including breaks
// for leases held only by the second session) arrives on the first
// session's transport.
func (lm *LeaseManager) GetSessionForBreak(leaseKey [16]byte) (sessionID uint64, found bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	keyHex := hex.EncodeToString(leaseKey[:])
	if guid, ok := lm.leaseClientGUID[keyHex]; ok {
		if sid, ok := lm.clientPrimarySession[guid]; ok {
			return sid, true
		}
	}
	sid, ok := lm.sessionMap[keyHex]
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

// BreakLeasesOnByteRangeLock breaks every lease (other than the locker's own
// lease key) holding Read caching to None when an SMB byte-range LOCK is
// acquired. Per MS-SMB2 3.3.5.14 and Samba
// `source3/smbd/smb2_oplock.c::contend_level2_oplocks_begin_default`, a BRL
// invalidates remote read caches: another client must now observe writes
// from the locking client. Unlike CREATE breaks (strip W, preserve R+H),
// this is full revocation to None. The locker's own lease is preserved via
// excludeOwner.ExcludeLeaseKey ("nobreakself").
func (lm *LeaseManager) BreakLeasesOnByteRangeLock(
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

	return lockMgr.BreakLeasesForByteRangeLock(handleKey, exclude)
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

// AnyHolderHasLeaseBits reports whether any lease on fileHandle except
// excludeKey currently has any bit in mask set. Non-blocking. Used by the SMB
// CREATE post-break park decision: per Samba `delay_for_oplock_fn`, a CREATE
// delays only when the existing holder's lease type intersects the delay_mask
// (W for non-violation/destructive, H for sharing-violation). Without that
// bit, the new opener proceeds inline while the holder is notified
// asynchronously. Returns false when no LockManager is bound for the share.
func (lm *LeaseManager) AnyHolderHasLeaseBits(fileHandle lock.FileHandle, shareName string, excludeKey [16]byte, mask uint32) bool {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return false
	}
	return lockMgr.AnyHolderHasLeaseBits(string(fileHandle), excludeKey, mask)
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

// BreakLeasesOnRename dispatches lease break notifications on the source
// (and optionally destination) file before a SET_INFO FileRenameInformation
// applies to metadata. Per MS-FSA §2.1.5.14.10 and Samba
// `source3/smbd/smb2_setinfo.c::smbd_smb2_rename`, rename participates in the
// same break processing as CREATE: any concurrent open whose Handle caching
// would be invalidated by the rename must be notified first.
//
// The renamer's own lease (renamerLeaseKey) is excluded so a same-key rename
// produces no self-break. Exclusion is by lease key only — NOT by ClientID —
// because a single client may hold two distinct leases on the same file (one
// per handle, smbtorture rename_wait LEASE1=h1 / LEASE2=h2 case); a client
// scoped exclusion would skip both and miss the required break.
//
// Source file leases break with BreakReasonSharingViolation (strip H,
// preserve R+W): smbtorture rename_wait expects RH→R, v2_rename_target_overwrite
// expects RWH→RW. Destination file leases (if dstHandle is non-empty AND
// isOverwrite=true) break the same way; the destination's holder is by
// definition someone other than the renamer, so no exclusion is applied there.
//
// Dispatch is fire-and-forget: this method does NOT wait for ACK. Callers that
// must park the request behind the break (smbtorture rename_wait) check
// HasOtherBreakingLeases on the source handle and route to
// WaitForOtherKeyBreaks. This mirrors the round-3 / round-4 CREATE async-park
// pattern in create_post_break.go and avoids the multi-client deadlock
// documented on BreakHandleLeasesOnOpenAsync.
func (lm *LeaseManager) BreakLeasesOnRename(
	srcHandle lock.FileHandle,
	dstHandle lock.FileHandle,
	shareName string,
	renamerLeaseKey [16]byte,
	isOverwrite bool,
) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	srcExclude := &lock.LockOwner{ExcludeLeaseKey: renamerLeaseKey}
	if err := lockMgr.BreakLeasesOnOpenConflict(string(srcHandle), srcExclude, lock.BreakReasonSharingViolation); err != nil {
		return err
	}

	if isOverwrite && dstHandle != "" && dstHandle != srcHandle {
		if err := lockMgr.BreakLeasesOnOpenConflict(string(dstHandle), nil, lock.BreakReasonSharingViolation); err != nil {
			return err
		}
	}

	return nil
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
//
// clientPrimarySession is intentionally NOT cleared here: it is keyed by
// ClientGUID, not by lease key, and other leases of the same client may
// still need it for break routing. The map is reaped by ReleaseSessionLeases
// when the corresponding session disappears.
func (lm *LeaseManager) removeLeaseMapping(keyHex string) {
	lm.mu.Lock()
	delete(lm.sessionMap, keyHex)
	delete(lm.leaseShare, keyHex)
	delete(lm.leaseV2, keyHex)
	delete(lm.leaseV1, keyHex)
	delete(lm.leaseClientGUID, keyHex)
	lm.mu.Unlock()
}

// MarkLeaseVersionIfUnset records the lease's protocol version on FIRST grant
// for the given key. Subsequent calls on the same key are no-ops — per
// smbtorture v2_epoch2 / v2_epoch3 the version is sticky from the originating
// grant: a V2-established lease keeps responding V2 even to V1 reopens, and
// a V1-established lease keeps responding V1 even when a V2 upgrade comes in.
//
// Callers must invoke this after a successful RequestLease whenever the
// grantedState is non-None, passing isV2 derived from the request's
// create-context size (V2 = 52 bytes, V1 = 32 bytes).
func (lm *LeaseManager) MarkLeaseVersionIfUnset(leaseKey [16]byte, isV2 bool) {
	keyHex := hex.EncodeToString(leaseKey[:])
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if lm.leaseV1[keyHex] || lm.leaseV2[keyHex] {
		return
	}
	if isV2 {
		lm.leaseV2[keyHex] = true
	} else {
		lm.leaseV1[keyHex] = true
	}
}

// IsV2 reports whether the lease was first granted from a V2 create context.
// Returns false for V1-established leases AND for unknown keys (safe default:
// treat as V1 and send NewEpoch = 0 rather than leak a non-zero epoch).
func (lm *LeaseManager) IsV2(leaseKey [16]byte) bool {
	keyHex := hex.EncodeToString(leaseKey[:])
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.leaseV2[keyHex]
}

// IsLeaseVersionKnown reports whether the lease's version has been recorded
// (i.e. a successful grant has occurred for this key and MarkLeaseVersionIfUnset
// fired). Used by the response-encoding path to decide whether to use the
// established version or fall back to the current request's format.
func (lm *LeaseManager) IsLeaseVersionKnown(leaseKey [16]byte) bool {
	keyHex := hex.EncodeToString(leaseKey[:])
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.leaseV1[keyHex] || lm.leaseV2[keyHex]
}

// resolveLockManager resolves the LockManager for a share name.
func (lm *LeaseManager) resolveLockManager(shareName string) lock.LockManager {
	if lm.resolver == nil || shareName == "" {
		return nil
	}
	return lm.resolver.GetLockManagerForShare(shareName)
}
