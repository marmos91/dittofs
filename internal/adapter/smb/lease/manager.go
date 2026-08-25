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

// TraditionalOplockBreakWaitTimeout bounds the CREATE wait when the existing
// holder is a traditional SMB1/SMB2 oplock (LEVEL_II / Exclusive / Batch)
// rather than an SMB2.1+ lease. Per MS-SMB2 §3.3.4.6 step 4 the server
// "waits for an implementation-specific default value, typically 35 seconds"
// before declaring the break failed and granting the conflicting open.
//
// Traditional-oplock clients often DO ack within the standard 5 s lease
// window (most smbtorture batch* tests), but a non-acking holder must be
// given the spec-mandated 35 s grace before the server moves on. The
// smbtorture batch22a / batch22b tests assert this directly: the second open
// must succeed only AFTER `te ∈ [timeout-1, timeout+15]` where timeout=35.
//
// Leases keep the shorter handleLeaseBreakWaitTimeout — MS-SMB2 §3.3.4.7
// lease-break flow has different timing semantics and existing tests
// (timeout-disconnect, breaking3, etc.) rely on the shorter bound.
const TraditionalOplockBreakWaitTimeout = 35 * time.Second

// leaseRecordKey identifies the lease RECORD the lock manager holds: one
// share's lock manager, one file, one lease key. The lock manager matches a
// request against an existing record on exactly (handleKey, leaseKey) within
// one share's manager, so a property of the record — its protocol version —
// belongs to that triple and not to the lease key alone: two distinct clients
// may present the same numeric key on different files, and keying on the key
// alone lets whichever wrote last decide the other's value.
//
// HandleKey and Key hold the raw handle string and the raw 16-byte key (not a
// hex string): this is a map key on the lease hot path, so using them directly
// keeps those paths allocation-free. Hex is computed only for logging.
type leaseRecordKey struct {
	Share     string
	HandleKey string
	Key       [16]byte
}

// leaseClientKey identifies one client's BINDING to a lease key: (client,
// share, key). Per MS-SMB2 §3.3.5.9.8 a lease is bound to a
// (ClientGUID, LeaseKey) pair, and lock.Manager enforces that binding by
// refusing a key already held by the same Owner.ClientID on another file
// within the share (`hasLeaseKeyOnOtherFile`, Samba `lease_match`). That rule
// makes this triple resolve to at most one file at a time, so the binding can
// carry the file it is on rather than key on it.
//
// The share is part of the identity because the uniqueness rule is enforced
// per share — each share has its own lock manager — so one client can hold the
// same key on files in two different shares.
type leaseClientKey struct {
	ClientID string
	Share    string
	Key      [16]byte
}

// leaseBinding is what a client's hold on a lease key resolves to: the file
// the lease is on, the session that registered it, and the ClientGUID it is
// bound to.
type leaseBinding struct {
	// HandleKey is the file the lease was granted on. Recorded rather than
	// keyed on because (client, share, key) already determines it, and the
	// LEASE_BREAK_ACK path is given only a lease key on the wire.
	HandleKey string

	// SessionID is the session that most recently registered this binding.
	// Break notifications fall back to it when no ClientGUID was recorded.
	SessionID uint64

	// ClientGUID is the GUID recorded on the FIRST grant for this binding
	// and is sticky: a same-(client, share, key) reopen does not change it.
	// Per MS-SMB2 §3.3.5.9.8 break notifications are routed at the client
	// level (Samba `smbXsrv_pending_break_submit` in
	// source3/smbd/smb2_server.c picks the FIRST connection of
	// `client->connections` regardless of which session holds the open).
	//
	// Required by smbtorture smb2.lease.v2_complex1 — two sessions of the
	// same ClientGUID open with different lease keys, and breaks for either
	// lease must arrive on the FIRST session's primary transport.
	ClientGUID [16]byte

	// HasGUID distinguishes "bound to the zero GUID" from "never bound".
	// Callers without a CryptoState (older durable-reconnect paths, tests)
	// pass a zero GUID and get no ClientGUID-based routing.
	HasGUID bool
}

// leaseVersion is the create-context version a lease was established with.
// The zero value means the version is not yet known, which the response
// encoder distinguishes from V1 (a single bool cannot carry the third state).
type leaseVersion uint8

const (
	leaseVersionUnknown leaseVersion = iota
	leaseVersionV1
	leaseVersionV2
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
	resolver LockManagerResolver
	notifier LeaseBreakNotifier

	// bindings records, per (client, share, lease key), which file the lease
	// is on, which session registered it, and which ClientGUID it is bound
	// to. Break routing, share resolution and session teardown all read it.
	bindings map[leaseClientKey]leaseBinding

	// versions records the create-context version each lease RECORD was
	// established with. Per MS-SMB2 §2.2.23.2 the NewEpoch field of a break
	// notification MUST be zero for V1 leases and carries the incremented
	// lease epoch for V2 ones; the same distinction selects the 32-byte or
	// 52-byte RqLs response context on CREATE. Sending a non-zero NewEpoch on
	// a V1 break trips the client (smb2.multichannel.leases.test1-3).
	//
	// Keyed per record rather than per client because lock.Manager keeps ONE
	// lease record per (handleKey, leaseKey) and hands it to every requester
	// of that key on that file — including a second session of the same
	// client. The version has to follow the record it describes, or a reopen
	// on another session would answer in a different format than the break
	// notification for the same record. MarkLeaseVersionIfUnset documents the
	// sticky-on-first-grant semantics.
	versions map[leaseRecordKey]leaseVersion

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
		bindings:             make(map[leaseClientKey]leaseBinding),
		versions:             make(map[leaseRecordKey]leaseVersion),
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
//     ClientGUID-based routing for that lease — falls back to the binding's
//     own session), which keeps callers that don't have a CryptoState wired
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
		sessionID, clientGUID, ownerID, clientID, shareName, requestedState, isDirectory, false, false)
}

// RequestLeaseStatOpen is the stat-open variant of RequestLease. The CREATE
// handler routes a lease request through this method when the CREATE's
// DesiredAccess is stat-open-only and the disposition is non-destructive
// (MS-SMB2 §3.3.5.9.8 / Samba `is_lease_stat_open`). The underlying lock
// manager grants the best coexisting state WITHOUT breaking existing holders
// (#751 smb2.lease.statopen4 CHECK_NO_BREAK).
func (lm *LeaseManager) RequestLeaseStatOpen(
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
		sessionID, clientGUID, ownerID, clientID, shareName, requestedState, isDirectory, false, true)
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
	clientGUID [16]byte,
	ownerID string,
	clientID string,
	shareName string,
	requestedState uint32,
	isDirectory bool,
) (grantedState uint32, epoch uint16, err error) {
	return lm.requestLeaseInternal(ctx, fileHandle, leaseKey, parentLeaseKey,
		sessionID, clientGUID, ownerID, clientID, shareName, requestedState, isDirectory, true, false)
}

// requestLeaseInternal is the shared body of RequestLease /
// RequestLeaseAsOplock / RequestLeaseStatOpen; the behavior change between them
// is which underlying Manager method we dispatch to so the new record gets the
// correct IsTraditionalOplock tag (cross-tier rules in bestGrantableState) and
// whether a cross-key conflict suppresses the break (stat-open carve-out).
// isTraditionalOplock and statOpen are mutually exclusive (a traditional oplock
// is never a stat-open lease request).
func (lm *LeaseManager) requestLeaseInternal(
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
	isTraditionalOplock bool,
	statOpen bool,
) (grantedState uint32, epoch uint16, err error) {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return lock.LeaseStateNone, 0, fmt.Errorf("no lock manager for share %q", shareName)
	}

	// Pre-register the binding BEFORE creating the lease in the LockManager.
	// The LockManager's RequestLease may trigger cross-key conflict breaks,
	// which dispatch through breakOpLocks → SMBBreakHandler. If the binding
	// isn't set yet, the break notification can't be routed to the correct
	// SMB client. Similarly, another goroutine's BreakHandleLeasesOnOpenAsync
	// may fire between the LockManager grant and the binding update, causing
	// a "no session" miss.
	//
	// Pre-registering is safe: if the grant fails or returns None, the
	// previous binding is put back below.
	ck := leaseClientKey{ClientID: clientID, Share: shareName, Key: leaseKey}
	lm.mu.Lock()
	prev, hadPrev := lm.bindings[ck]
	binding := prev
	binding.HandleKey = string(fileHandle)
	binding.SessionID = sessionID
	// Bind the lease to a ClientGUID on FIRST grant (sticky). The only paths
	// that re-enter here on the same (client, share, key) are same-client
	// reopens and upgrades — those must NOT rebind the GUID. Zero ClientGUID
	// callers (legacy paths) leave the binding unset and fall back to the
	// binding's session for break dispatch.
	if clientGUID != ([16]byte{}) {
		if !binding.HasGUID {
			binding.ClientGUID = clientGUID
			binding.HasGUID = true
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
	lm.bindings[ck] = binding
	lm.mu.Unlock()

	// restorePreRegistration undoes the pre-registration above when the grant
	// produced no record. A rejected grant must not leave this client's
	// binding pointing at the file it was refused — the client may still hold
	// the key on the file it bound earlier, which is exactly why the grant
	// was refused.
	restorePreRegistration := func() {
		lm.mu.Lock()
		if hadPrev {
			lm.bindings[ck] = prev
		} else {
			delete(lm.bindings, ck)
		}
		lm.mu.Unlock()
	}

	// Dispatch to the appropriate Manager method so the new record's
	// IsTraditionalOplock tag and stat-open break-suppression are set
	// correctly. The LockManager interface deliberately stays narrow (no
	// oplock / stat-open variants): when the configured store is a
	// *lock.Manager (the only production impl) call the tagged method;
	// otherwise fall through to the plain interface call so test doubles keep
	// working.
	mgr, isConcrete := lockMgr.(*lock.Manager)
	switch {
	case isConcrete && isTraditionalOplock:
		grantedState, epoch, err = mgr.RequestLeaseAsOplock(
			ctx, fileHandle, leaseKey, parentLeaseKey,
			ownerID, clientID, shareName,
			requestedState, isDirectory,
		)
	case isConcrete && statOpen:
		grantedState, epoch, err = mgr.RequestLeaseStatOpen(
			ctx, fileHandle, leaseKey, parentLeaseKey,
			ownerID, clientID, shareName,
			requestedState, isDirectory,
		)
	default:
		grantedState, epoch, err = lockMgr.RequestLease(
			ctx, fileHandle, leaseKey, parentLeaseKey,
			ownerID, clientID, shareName,
			requestedState, isDirectory,
		)
	}
	if err != nil && !errors.Is(err, lock.ErrLeaseBreakInProgress) {
		restorePreRegistration()
		return 0, 0, err
	}

	// Undo the pre-registration only if the LockManager has no record for
	// this key on this file. grantedState == None can mean either:
	//   - rejected request (no record created) — must reap pre-registration
	//   - successful None probe / existing released-to-None record — keep
	//     the binding so a later unsolicited or duplicate ack still resolves
	//     and surfaces ErrLeaseAckNotBreaking (smbtorture breaking5).
	//
	// The check is scoped to this file: a record under the same key on a
	// DIFFERENT file belongs to another client's lease and says nothing about
	// whether this grant created anything.
	if grantedState == lock.LeaseStateNone && !lm.HasLeaseOnHandle(fileHandle, shareName, leaseKey) {
		restorePreRegistration()
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
	sessionID uint64,
	connGUID [16]byte,
	acknowledgedState uint32,
	epoch uint16,
) error {
	lm.mu.RLock()
	ck, found := lm.resolveAckBindingLocked(leaseKey, sessionID, connGUID)
	lm.mu.RUnlock()
	if !found {
		logger.Debug("AcknowledgeLeaseBreak: no lease bound to this client (CLOSE-beat-ack), treating as success",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"sessionID", sessionID)
		return nil
	}

	lockMgr := lm.resolveLockManager(ck.Share)
	if lockMgr == nil {
		logger.Debug("AcknowledgeLeaseBreak: no lock manager for lease (CLOSE-beat-ack), treating as success",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"share", ck.Share)
		return nil
	}

	err := lockMgr.AcknowledgeLeaseBreak(ctx, leaseKey, acknowledgedState, epoch)
	if err != nil {
		if errors.Is(err, lock.ErrLeaseAckNotFound) {
			logger.Debug("AcknowledgeLeaseBreak: lease record absent (CLOSE-beat-ack), treating as success",
				"leaseKey", fmt.Sprintf("%x", leaseKey))
			lm.mu.Lock()
			delete(lm.bindings, ck)
			lm.mu.Unlock()
			return nil
		}
		return err
	}

	// Do NOT reap the binding on ack-to-None: the lock manager keeps the
	// record alive at state=None until CLOSE, so a duplicate ack on the same
	// key must continue to find the lockMgr and surface
	// ErrLeaseAckNotBreaking. ReleaseLeaseForHandle clears the binding when
	// no records remain (see the GetLeaseState-found check there).
	return nil
}

// resolveAckBindingLocked finds the lease binding a LEASE_BREAK_ACK refers to.
// The wire gives only a lease key, so the acknowledging connection supplies
// the rest of the identity, exactly as MS-SMB2 §3.3.5.22.2 step 1 requires
// ("locate the lease ... whose LeaseKey matches ... and ClientGuid matches
// Connection.ClientGuid").
//
// The owning session is preferred; a ClientGUID match covers multichannel and
// durable reconnect on a different session of the same client. A zero connGUID
// never matches a recorded GUID, so a stray ack cannot probe another client's
// lease. Must hold lm.mu (read or write).
//
// ponytail: linear over the bindings map, which holds one entry per open lease
// per client — single digits in practice, and only acks that are not from the
// owning session walk past the first match. Add a by-key index if a profile
// ever shows this scan.
func (lm *LeaseManager) resolveAckBindingLocked(leaseKey [16]byte, sessionID uint64, connGUID [16]byte) (leaseClientKey, bool) {
	var guidKey leaseClientKey
	var guidFound bool
	for ck, b := range lm.bindings {
		if ck.Key != leaseKey {
			continue
		}
		if b.SessionID == sessionID {
			return ck, true
		}
		if connGUID == ([16]byte{}) || !b.HasGUID || b.ClientGUID != connGUID {
			continue
		}
		// A client holding one key in two shares makes the ack ambiguous —
		// MS-SMB2 §3.3.5.9.8 binds a lease to (ClientGuid, LeaseKey) and does
		// not contemplate it. Break the tie on share name so the choice is at
		// least deterministic across runs.
		if !guidFound || ck.Share < guidKey.Share {
			guidKey, guidFound = ck, true
		}
	}
	return guidKey, guidFound
}

// ReleaseLease releases every record for a lease key in one share and drops
// the client's binding to it.
func (lm *LeaseManager) ReleaseLease(ctx context.Context, clientID, shareName string, leaseKey [16]byte) error {
	ck := leaseClientKey{ClientID: clientID, Share: shareName, Key: leaseKey}

	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		// Already released or no manager
		lm.mu.Lock()
		delete(lm.bindings, ck)
		lm.mu.Unlock()
		return nil
	}

	if err := lockMgr.ReleaseLease(ctx, leaseKey); err != nil {
		return err
	}

	lm.mu.Lock()
	delete(lm.bindings, ck)
	lm.mu.Unlock()
	return nil
}

// ReleaseLeaseForHandle releases lease records only under a specific handleKey
// bucket. Used by CLOSE so that opens on OTHER files sharing the same
// LeaseKey constant (typical in smbtorture, which reuses fixed LEASE1/LEASE2
// macros across tests) retain their records. The bindings are only torn down
// when the last record for the key is gone.
func (lm *LeaseManager) ReleaseLeaseForHandle(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, shareName string) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}

	handleKey := string(fileHandle)
	if err := lockMgr.ReleaseLeaseForHandle(ctx, handleKey, leaseKey); err != nil {
		return err
	}

	// The recorded version describes the record on THIS file, so it goes as
	// soon as that record does — a surviving record under the same key on
	// another file is a different lease with its own version.
	if !lockMgr.HasLeaseOnHandle(handleKey, leaseKey) {
		lm.mu.Lock()
		delete(lm.versions, leaseRecordKey{Share: shareName, HandleKey: handleKey, Key: leaseKey})
		lm.mu.Unlock()
	}

	// Only drop bindings if no lease records remain anywhere in this share
	// for this key — otherwise a concurrent open on a different file would
	// lose break-dispatch routing.
	if _, _, found := lockMgr.GetLeaseState(ctx, leaseKey); !found {
		lm.mu.Lock()
		for ck := range lm.bindings {
			if ck.Share == shareName && ck.Key == leaseKey {
				delete(lm.bindings, ck)
			}
		}
		lm.mu.Unlock()
	}
	return nil
}

// ReleaseSessionLeases releases all leases owned by a session.
// This is called during session cleanup (LOGOFF / connection close).
func (lm *LeaseManager) ReleaseSessionLeases(ctx context.Context, sessionID uint64) error {
	lm.mu.RLock()
	// Collect the bindings this session registered. Each carries the file its
	// lease is on, so the release is scoped to that file: another client
	// holding the same key value on a different file keeps its lease.
	type sessionLease struct {
		key     leaseClientKey
		binding leaseBinding
	}
	var toRelease []sessionLease
	for ck, b := range lm.bindings {
		if b.SessionID == sessionID {
			toRelease = append(toRelease, sessionLease{key: ck, binding: b})
		}
	}
	lm.mu.RUnlock()

	for _, sl := range toRelease {
		if err := lm.ReleaseLeaseForHandle(ctx, lock.FileHandle(sl.binding.HandleKey), sl.key.Key, sl.key.Share); err != nil {
			logger.Warn("LeaseManager: failed to release session lease",
				"sessionID", sessionID,
				"leaseKey", fmt.Sprintf("%x", sl.key.Key),
				"error", err)
			// Continue releasing other leases
		}
		lm.mu.Lock()
		delete(lm.bindings, sl.key)
		lm.mu.Unlock()
	}

	// Reap any clientPrimarySession entries that pointed at the gone
	// session AND re-elect a successor where surviving leases of the same
	// ClientGUID still exist. Without re-election, breaks for those leases
	// would fall back to the binding's own session, deviating from the
	// "first live connection" semantics this map exists to enforce — Samba's
	// `client->connections` always rehomes to the next-oldest connection of
	// the client, not to whichever session most recently touched the lease.
	// We approximate "oldest surviving session" by picking the smallest
	// sessionID still bound to that ClientGUID; sessionIDs are monotonically
	// allocated, so smallest = earliest.
	lm.mu.Lock()
	for guid, sid := range lm.clientPrimarySession {
		if sid != sessionID {
			continue
		}
		var minSID uint64
		var found bool
		for _, b := range lm.bindings {
			if !b.HasGUID || b.ClientGUID != guid || b.SessionID == sessionID {
				continue
			}
			if !found || b.SessionID < minSID {
				minSID = b.SessionID
				found = true
			}
		}
		if found {
			lm.clientPrimarySession[guid] = minSID
		} else {
			delete(lm.clientPrimarySession, guid)
		}
	}
	lm.mu.Unlock()

	return nil
}

// GetLeaseState returns the state and epoch of a lease key in one share.
//
// The share is a parameter rather than resolved from the key because the key
// alone does not identify a lease: two clients may present the same 16-byte
// value in different shares, and the wrong share's lock manager reports a
// foreign lease's state and epoch — values that go straight back out on the
// wire on a durable reconnect or a replayed CREATE.
func (lm *LeaseManager) GetLeaseState(ctx context.Context, shareName string, leaseKey [16]byte) (state uint32, epoch uint16, found bool) {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return lock.LeaseStateNone, 0, false
	}

	return lockMgr.GetLeaseState(ctx, leaseKey)
}

// HasLeaseOnHandle reports whether a lease record with this key already exists
// on this file in this share.
func (lm *LeaseManager) HasLeaseOnHandle(fileHandle lock.FileHandle, shareName string, leaseKey [16]byte) bool {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return false
	}
	return lockMgr.HasLeaseOnHandle(string(fileHandle), leaseKey)
}

// GetSessionForLease returns the sessionID bound to a lease by one client in
// one share.
func (lm *LeaseManager) GetSessionForLease(clientID, shareName string, leaseKey [16]byte) (sessionID uint64, found bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	b, ok := lm.bindings[leaseClientKey{ClientID: clientID, Share: shareName, Key: leaseKey}]
	return b.SessionID, ok
}

// VerifyLeaseAckOwnership reports whether a session presenting a
// LEASE_BREAK_ACK for leaseKey is entitled to acknowledge it: a lease is bound
// to a (ClientGuid, LeaseKey) pair, so an ack is valid only when it arrives
// from the owning client. Returns false when the ack resolves to no binding,
// so a stray ack for a key the sender never held cannot probe state.
//
// This resolves the same binding AcknowledgeLeaseBreak then acts on (see
// resolveAckBindingLocked for the matching rule), so the authorization and the
// action cannot disagree about which lease is meant.
func (lm *LeaseManager) VerifyLeaseAckOwnership(leaseKey [16]byte, sessionID uint64, connGUID [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	_, found := lm.resolveAckBindingLocked(leaseKey, sessionID, connGUID)
	return found
}

// GetSessionForBreak returns the sessionID that should receive a break
// notification for the given lease. Per MS-SMB2 §3.3.4.7 and Samba
// `smbXsrv_pending_break_submit` (source3/smbd/smb2_server.c) the break is
// delivered to the FIRST connection of `client->connections` — i.e. the
// oldest live connection of the lease's ClientGUID — irrespective of which
// session opened the file. When the binding has a recorded ClientGUID and a
// primary session is registered for that GUID, this method returns that
// session. Otherwise it falls back to the binding's own session (legacy
// callers without a ClientGUID; durable-reconnect tests that don't thread a
// CryptoState).
//
// Required by smbtorture smb2.lease.v2_complex1, which opens two sessions
// of the same ClientGUID and asserts every lease break (including breaks
// for leases held only by the second session) arrives on the first
// session's transport.
//
// clientID and shareName scope the lookup: pass `ul.Owner.ClientID` and
// `ul.Owner.ShareName` from the breaking record so two clients holding the
// same numeric leaseKey on different files don't cross-route their breaks,
// and so the fallback session is the one that registered THIS lease rather
// than whichever client registered the key value last.
func (lm *LeaseManager) GetSessionForBreak(clientID, shareName string, leaseKey [16]byte) (sessionID uint64, found bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	b, ok := lm.bindings[leaseClientKey{ClientID: clientID, Share: shareName, Key: leaseKey}]
	if !ok {
		return 0, false
	}
	if b.HasGUID {
		if sid, ok := lm.clientPrimarySession[b.ClientGUID]; ok {
			return sid, true
		}
	}
	return b.SessionID, true
}

// UpdateSessionForLease updates the session ID bound to a lease by one client
// in one share. Used during durable handle reconnect to associate the existing
// lease with the new session for break notification routing.
func (lm *LeaseManager) UpdateSessionForLease(clientID, shareName string, leaseKey [16]byte, sessionID uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	ck := leaseClientKey{ClientID: clientID, Share: shareName, Key: leaseKey}
	b, ok := lm.bindings[ck]
	if !ok {
		return
	}
	b.SessionID = sessionID
	lm.bindings[ck] = b
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

// BreakLeasesOnByteRangeLock breaks every lease holding Read caching to None
// when an SMB byte-range LOCK is acquired. Per MS-SMB2 3.3.5.14 and Samba
// `source3/smbd/smb2_oplock.c::contend_level2_oplocks_begin_default`, a BRL
// invalidates read caches: another client must now observe writes from the
// locking client.
//
// The locker's own lease is excluded ONLY when it holds Write caching. A
// client that was broken from Batch/Exclusive to Level II must self-break to
// None on BRL acquisition — the Level II read cache is invalidated because
// BRL grants imply the locking client will write within the locked range.
// Required by smbtorture smb2.oplock.brl1 and brl3.
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
	if len(excludeOwner) > 0 && excludeOwner[0] != nil {
		eo := excludeOwner[0]
		if eo.ExcludeLeaseKey != ([16]byte{}) {
			ctx := context.Background()
			state, _, found := lockMgr.GetLeaseState(ctx, eo.ExcludeLeaseKey)
			isTraditional := lockMgr.IsTraditionalOplockForKey(eo.ExcludeLeaseKey)
			if found && (!isTraditional || state&lock.LeaseStateWrite != 0) {
				exclude = eo
			}
		} else {
			exclude = eo
		}
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

// SignalParkedCreates wakes any parked CREATE waiter on fileHandle so it
// re-evaluates its post-break gate. SMB CLOSE invokes this after removing the
// closing open from the open-file table so a parked dir CREATE that was
// share-mode-conflicting with the just-closed holder can re-check share-mode
// against the now-shrunk table and complete with OK rather than stalling
// until the 5 s async-break wait timeout. Required by smbtorture
// smb2.dirlease.v2_request.
func (lm *LeaseManager) SignalParkedCreates(fileHandle lock.FileHandle, shareName string) {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return
	}
	lockMgr.SignalParkedCreates(string(fileHandle))
}

// AnyHolderIsTraditionalOplock reports whether any holder on fileHandle is a
// traditional oplock (synthetic-key record). Returns false when no LockManager
// is bound for the share.
func (lm *LeaseManager) AnyHolderIsTraditionalOplock(fileHandle lock.FileHandle, shareName string) bool {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return false
	}
	if mgr, ok := lockMgr.(*lock.Manager); ok {
		return mgr.AnyHolderIsTraditionalOplock(string(fileHandle))
	}
	return false
}

// OnlyTimeoutTombstoneRecords reports whether handleKey has at least one
// lease record AND every present record is a timeout tombstone
// (BrokenViaTimeout=true). Used by the CREATE-grant LEVEL_II coercion to
// recognize the "holder timed out, server moved on" case and decline to
// constrain the new opener's grant by an abandoned holder.
func (lm *LeaseManager) OnlyTimeoutTombstoneRecords(fileHandle lock.FileHandle, shareName string) bool {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return false
	}
	if mgr, ok := lockMgr.(*lock.Manager); ok {
		return mgr.OnlyTimeoutTombstoneRecords(string(fileHandle))
	}
	return false
}

// HasActiveLeaseRecord reports whether handleKey has any lease record that is
// NOT a timeout tombstone (BrokenViaTimeout=true) and is owned by a key other
// than excludeKey. A "live" record — including a holder that has acked-to-None
// (Samba `disallow_write_lease` keeps treating it as a real holder) — counts
// as active and constrains the new opener's grant. Timeout tombstones are
// excluded so smb2.oplock.batch22b can still grant a fresh BATCH after the
// abandoned holder times out. Used by the CREATE-grant LEVEL_II coercion when
// the existing OpenFile is stat-only — covers smbtorture
// smb2.oplock.batch9a / batch13 / batch14 / batch16 where the prior holder's
// lease record is the only signal of "another holder is alive".
func (lm *LeaseManager) HasActiveLeaseRecord(fileHandle lock.FileHandle, shareName string, excludeKey [16]byte) bool {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return false
	}
	if mgr, ok := lockMgr.(*lock.Manager); ok {
		return mgr.HasActiveLeaseRecord(string(fileHandle), excludeKey)
	}
	return false
}

// IsLeaseBrokenViaTimeout reports whether the lease identified by (handleKey,
// leaseKey) on shareName is a timeout tombstone (its break was force-completed
// because the holder never acknowledged it). Used by the CREATE W-strip to keep
// WRITE off a new grant when a same-client lease holder timed out but its handle
// is still open (smb2.lease.timeout). handleKey scopes the lookup to the file
// being granted.
func (lm *LeaseManager) IsLeaseBrokenViaTimeout(shareName, handleKey string, leaseKey [16]byte) bool {
	if lm == nil {
		return false
	}
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return false
	}
	if mgr, ok := lockMgr.(*lock.Manager); ok {
		return mgr.IsLeaseBrokenViaTimeout(handleKey, leaseKey)
	}
	return false
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

// WaitForShareConflictClear waits for the SMB CREATE share-violation park to be
// ready to recheck the share mode: it returns when the conflicting holder CLOSEs
// (conflictPresent() goes false → CREATE proceeds), when the holder ACKs the
// break but keeps its open (break drains with the conflict live → deterministic
// SHARING_VIOLATION on the caller's recheck), or on ctx timeout. A nil return
// means "recheck", not "conflict cleared". Unlike WaitForOtherKeyBreaks this
// never force-completes the holder's lease, so the holder's deferred ACK still
// succeeds (smbtorture replay dhv2-pending1n-vs-violation-lease-{close,ack}-sane).
func (lm *LeaseManager) WaitForShareConflictClear(ctx context.Context, fileHandle lock.FileHandle, shareName string, conflictPresent func() bool) error {
	lockMgr := lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil
	}
	return lockMgr.WaitForShareConflictClear(ctx, string(fileHandle), conflictPresent)
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
// Dispatch is fire-and-forget: BreakLeasesOnRename does NOT wait for ACK. Callers that
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
//
// excludeParentLeaseKey + hasExcludeKey carry the originating handle's
// ParentLeaseKey (when set on its RqLs) so the dir-lease parent-key
// suppression rule (MS-SMB2 §3.3.4.20) is applied in breakOpLocks.
func (lm *LeaseManager) resolveParentBreakArgs(
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
	excludeParentLeaseKey [16]byte,
	hasExcludeKey bool,
) (lockMgr lock.LockManager, handleKey string, excludeOwner *lock.LockOwner) {
	lockMgr = lm.resolveLockManager(shareName)
	if lockMgr == nil {
		return nil, "", nil
	}
	handleKey = string(parentHandle)
	if excludeClientID != "" || hasExcludeKey {
		excludeOwner = &lock.LockOwner{
			ClientID:                    excludeClientID,
			ExcludeParentDirLeaseKey:    excludeParentLeaseKey,
			HasExcludeParentDirLeaseKey: hasExcludeKey,
		}
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
//
// excludeParentLeaseKey + hasExcludeKey carry the originating handle's RqLs
// ParentLeaseKey for the dir-lease parent-key suppression rule (MS-SMB2
// §3.3.4.20). Pass the zero key + false when there is no parent-key
// linkage to honor.
func (lm *LeaseManager) BreakParentHandleLeasesOnCreate(
	ctx context.Context,
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
	excludeParentLeaseKey [16]byte,
	hasExcludeKey bool,
) error {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(
		parentHandle, shareName, excludeClientID, excludeParentLeaseKey, hasExcludeKey)
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

// BreakParentDirLeasesOnDestructiveCreate breaks every parent-directory lease
// to None in a single notification when a child file is being OVERWRITTEN or
// SUPERSEDED by a CREATE from another client. Per MS-SMB2 §3.3.5.9 and Samba
// `delay_for_oplock_fn` (source3/smbd/open.c) the destructive `will_overwrite`
// arm strips both SMB2_LEASE_HANDLE and SMB2_LEASE_READ from the break_to
// mask atomically — i.e. an RH parent lease must transition RH → "" via ONE
// LEASE_BREAK_NOTIFICATION, not via the two-step (strip-H then strip-R) pattern
// used by the non-destructive CREATE path (BreakParentHandleLeasesOnCreate +
// BreakParentReadLeasesOnModify). smb2.dirlease.overwrite asserts
// exactly two break notifications for the test scenario — one on the file
// lease, one on the dir lease — so the two-step pattern produces three and
// fails the test.
//
// Waits for LEASE_BREAK_ACK with parentLeaseBreakWaitTimeout (same ack-wait
// guarantee as BreakParentHandleLeasesOnCreate). excludeClientID +
// excludeParentLeaseKey + hasExcludeKey carry the suppression rules from
// MS-SMB2 §3.3.4.20 so a same-client or parent-key-linked dir
// lease is not broken.
func (lm *LeaseManager) BreakParentDirLeasesOnDestructiveCreate(
	ctx context.Context,
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
	excludeParentLeaseKey [16]byte,
	hasExcludeKey bool,
) error {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(
		parentHandle, shareName, excludeClientID, excludeParentLeaseKey, hasExcludeKey)
	if lockMgr == nil {
		return nil
	}
	// BreakReasonDestructive collapses the strip-H + strip-R steps into a
	// single break-to-None notification per ComputeLeaseBreakTo. The parent
	// handle hosts only directory leases (we never request file leases on a
	// directory handle key), so this does not over-break unrelated file
	// leases.
	if err := lockMgr.BreakLeasesOnOpenConflict(handleKey, excludeOwner, lock.BreakReasonDestructive); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, parentLeaseBreakWaitTimeout)
	defer cancel()
	return lockMgr.WaitForBreakCompletion(waitCtx, handleKey)
}

// PrepareParentDirLeaseBreakOnContentChange records the same single
// break-to-None per holder as BreakParentDirLeasesOnDestructiveCreate
// but WITHOUT waiting for the LEASE_BREAK_ACK. Used by content-change paths
// (SET_INFO rename / hardlink / disposition, CLOSE-on-delete, WRITE-mark)
// where the triggering request must complete on its own transport: waiting for
// the holder's ACK inline causes a server-side timeout that force-completes
// the lease, so when the test client (which deferred the ACK via lease_skip_ack
// to capture the break for replay) eventually re-acks, the lease is no longer
// in the breaking state and the ACK returns STATUS_UNSUCCESSFUL.
//
// Mirrors Samba `contend_dirleases` → `send_break_to_none`
// (source3/smbd/smb2_oplock.c): the dispatch is fire-and-forget; the holder
// acks on its own schedule and that ACK completes the per-lease state
// transition. Required by smbtorture smb2.dirlease.{rename, hardlink,
// unlink_different_set_and_close, unlink_*_initial_and_close} which all set
// lease_skip_ack=true before the triggering op and replay the captured ACK
// after the response returns.
//
// The break is RECORDED on the leases that exist when this is called and the
// returned function only puts the notifications on the wire. Callers that
// defer that function past their own response therefore break exactly the
// leases the change contended with: a dir lease granted in the meantime is
// not caught by a change that predates it. Evaluating the holder set at
// dispatch time instead let a write-close's deferred break revoke a dir lease
// granted by the very next CREATE, which the client counted as a second
// LEASE_BREAK for one change.
//
// The returned function is always non-nil and is a no-op when nothing broke.
func (lm *LeaseManager) PrepareParentDirLeaseBreakOnContentChange(
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
	excludeParentLeaseKey [16]byte,
	hasExcludeKey bool,
) func() {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(
		parentHandle, shareName, excludeClientID, excludeParentLeaseKey, hasExcludeKey)
	if lockMgr == nil {
		return func() {}
	}
	return lockMgr.PrepareBreakLeasesOnOpenConflict(handleKey, excludeOwner, lock.BreakReasonDestructive)
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
// excludeParentLeaseKey + hasExcludeKey: see BreakParentHandleLeasesOnCreate.
func (lm *LeaseManager) BreakParentReadLeasesOnModify(
	ctx context.Context,
	parentHandle lock.FileHandle,
	shareName string,
	excludeClientID string,
	excludeParentLeaseKey [16]byte,
	hasExcludeKey bool,
) error {
	lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(
		parentHandle, shareName, excludeClientID, excludeParentLeaseKey, hasExcludeKey)
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

// SetLeaseEpoch sets the epoch on an existing lease identified by leaseKey in
// one share. Per MS-SMB2 3.3.5.9: for V2 leases, the server should track the
// client's epoch from the RqLs create context.
//
// The share is a parameter for the same reason it is one on GetLeaseState: the
// key alone does not identify a lease, and the epoch written here is the
// NewEpoch of the next break notification for whatever lease it lands on.
func (lm *LeaseManager) SetLeaseEpoch(shareName string, leaseKey [16]byte, epoch uint16) {
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
// For SMB2.1+ leases: the writer's own lease is excluded ONLY when it holds
// Write caching (W bit set). A client with RW or RWH can write without self-
// breaking because the Write lease already permits cached writes.
//
// For traditional oplocks (Level II / Read-only): the writer MUST self-break.
// Per Samba `contend_level2_oplocks_begin_default` (source3/smbd/smb2_oplock.c),
// ALL Level II holders — including the writer — are broken to None on any data
// modification. Without this, a client that was broken from Batch/Exclusive to
// Level II and then writes would retain stale Read caching permissions.
// Required by smbtorture smb2.oplock.batch1, batch6, batch9, batch10, levelii500.
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
		ctx := context.Background()
		state, _, found := lockMgr.GetLeaseState(ctx, excludeLeaseKey)
		isTraditional := lockMgr.IsTraditionalOplockForKey(excludeLeaseKey)
		// Leases: always exclude the writer's own key (nobreakself per MS-SMB2 §3.3.5.9).
		// Traditional oplocks: exclude only when the writer has Write caching.
		// A Level II oplock holder must self-break on write per Samba
		// contend_level2_oplocks_begin_default.
		if found && (!isTraditional || state&lock.LeaseStateWrite != 0) {
			exclude = &lock.LockOwner{ExcludeLeaseKey: excludeLeaseKey}
		}
	}

	return lockMgr.CheckAndBreakOpLocksForWrite(handleKey, exclude)
}

// LeaseCount returns the number of active lease bindings tracked by this
// manager. Used for state debugging instrumentation.
func (lm *LeaseManager) LeaseCount() int {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return len(lm.bindings)
}

// RangeLeases iterates over all tracked lease bindings, calling fn for each.
// The callback receives (leaseKey, sessionID, shareName); callers that need a
// hex string for logging format it themselves (fmt.Sprintf("%x", leaseKey)).
// Return false to stop iteration. Used for state debugging instrumentation.
func (lm *LeaseManager) RangeLeases(fn func(leaseKey [16]byte, sessionID uint64, shareName string) bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for ck, b := range lm.bindings {
		if !fn(ck.Key, b.SessionID, ck.Share) {
			return
		}
	}
}

// MarkLeaseVersionIfUnset records the lease's protocol version on the FIRST
// grant for a record. Subsequent calls for the same record are no-ops — per
// smbtorture v2_epoch2 / v2_epoch3 the version is sticky from the originating
// grant: a V2-established lease keeps responding V2 even to V1 reopens, and
// a V1-established lease keeps responding V1 even when a V2 upgrade comes in.
//
// Scoped to (share, file, key) — the identity of the record the version
// describes, see leaseRecordKey.
//
// Callers must invoke this after a successful RequestLease whenever the
// grantedState is non-None, passing isV2 derived from the request's
// create-context size (V2 = 52 bytes, V1 = 32 bytes).
func (lm *LeaseManager) MarkLeaseVersionIfUnset(fileHandle lock.FileHandle, shareName string, leaseKey [16]byte, isV2 bool) {
	rk := leaseRecordKey{Share: shareName, HandleKey: string(fileHandle), Key: leaseKey}
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if lm.versions[rk] != leaseVersionUnknown {
		return
	}
	if isV2 {
		lm.versions[rk] = leaseVersionV2
	} else {
		lm.versions[rk] = leaseVersionV1
	}
}

// IsV2 reports whether the lease was first granted from a V2 create context.
// Returns false for V1-established leases AND for unknown records (safe
// default: treat as V1 and send NewEpoch = 0 rather than leak a non-zero
// epoch).
func (lm *LeaseManager) IsV2(fileHandle lock.FileHandle, shareName string, leaseKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.versions[leaseRecordKey{Share: shareName, HandleKey: string(fileHandle), Key: leaseKey}] == leaseVersionV2
}

// IsLeaseVersionKnown reports whether the record's version has been recorded
// (i.e. a successful grant has occurred for this key on this file and
// MarkLeaseVersionIfUnset fired). Used by the response-encoding path to decide
// whether to use the established version or fall back to the current request's
// format.
func (lm *LeaseManager) IsLeaseVersionKnown(fileHandle lock.FileHandle, shareName string, leaseKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.versions[leaseRecordKey{Share: shareName, HandleKey: string(fileHandle), Key: leaseKey}] != leaseVersionUnknown
}

// resolveLockManager resolves the LockManager for a share name.
func (lm *LeaseManager) resolveLockManager(shareName string) lock.LockManager {
	if lm.resolver == nil || shareName == "" {
		return nil
	}
	return lm.resolver.GetLockManagerForShare(shareName)
}
