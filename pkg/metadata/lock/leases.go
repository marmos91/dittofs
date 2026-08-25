// Lease CRUD operations on the Manager.
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
	storeerrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ErrLeaseBreakInProgress is returned by RequestLease when a same-key lease
// is in Breaking state. Per MS-SMB2 3.3.5.9.8, the caller must set
// SMB2_LEASE_FLAG_BREAK_IN_PROGRESS (0x02) in the CREATE response.
// The returned state and epoch are the current values of the breaking lease.
var ErrLeaseBreakInProgress = errors.New("lease break in progress")

// ErrInvalidLeaseState is reserved for future use. RequestLease no longer
// returns this for file lease states that lack the Read bit (W, H, WH); per
// Samba source3/smbd/open.c::delay_for_oplock and the smbtorture
// smb2.lease.request matrix, those combinations are silently coerced to
// LeaseState=None and the CREATE succeeds with NT_STATUS_OK. The sentinel is
// kept so external callers that previously imported it continue to compile;
// production code paths no longer surface it.
var ErrInvalidLeaseState = errors.New("invalid lease state")

// ErrAcknowledgedStateExceedsBreakTo is returned by AcknowledgeLeaseBreak when
// the client acknowledges with a state containing bits not present in the
// server's BreakToState. Per MS-SMB2 3.3.5.22.2, the caller must return
// STATUS_REQUEST_NOT_ACCEPTED.
var ErrAcknowledgedStateExceedsBreakTo = errors.New("acknowledged state exceeds break-to state")

// ErrLeaseAckNotFound is returned by AcknowledgeLeaseBreak when no lease
// exists for the given lease key (e.g., the client sent CLOSE before the
// ack and the lease was released). The SMB wrapper treats this as a no-op
// success; if it surfaces to the wire it maps to STATUS_OBJECT_NAME_NOT_FOUND.
var ErrLeaseAckNotFound = errors.New("no lease for key")

// ErrLeaseAckNotBreaking is returned by AcknowledgeLeaseBreak when the lease
// exists but is not in the Breaking state (e.g., the client acks a break that
// did not require acknowledgment, or re-acks an already-completed break).
// Per MS-SMB2 3.3.5.22.2, the caller must return STATUS_UNSUCCESSFUL.
var ErrLeaseAckNotBreaking = errors.New("lease not in breaking state")

// ErrLeaseKeyInUse is returned by RequestLease when the supplied lease key is
// already bound to a record on a different file (different handleKey bucket).
// Per MS-SMB2 3.3.5.9.8 and Samba's source3/smbd/smb2_lease.c::lease_match,
// a lease key MUST be unique across files for a given client; reusing a key
// across files MUST fail with STATUS_INVALID_PARAMETER.
var ErrLeaseKeyInUse = errors.New("lease key already in use on another file")

// validUpgrades defines allowed lease state upgrade transitions.
// A lease can only be upgraded (more permissions), never downgraded via RequestLease.
// Downgrade happens only through lease break.
//
// LeaseStateNone is a re-lease source: a record kept alive after ack-to-None
// (handle-bound lifetime) can be re-granted to any valid state by a same-key
// RequestLease. Without this entry the persisted None record would be treated
// as a downgrade source and the request would be rejected (smbtorture
// nobreakself: a same-key reopen after a break must re-grant the lease).
var validUpgrades = map[uint32][]uint32{
	LeaseStateNone: {
		LeaseStateRead,
		LeaseStateRead | LeaseStateWrite,
		LeaseStateRead | LeaseStateHandle,
		LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
	},
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

// findLeaseByKey resolves the lock holding the given leaseKey.
// Returns (handleKey, *UnifiedLock, index) or ("", nil, -1) if not found.
// Must be called with lm.mu held.
//
// Uses leaseKeyIndex (leaseKey -> set of holding handleKeys, maintained by
// reindexHandleLocked) to probe only the buckets that actually hold the key
// instead of scanning every lock in unifiedLocks. The same numeric key may be
// bound on multiple files; this returns the first matching record found among
// the candidate buckets — which one is unspecified (callers that must act on
// every holder, e.g. ReleaseLease, scan unifiedLocks directly). The in-bucket
// scan locates the record's slice index, which the index does not (and must
// not) track because slice positions shift on every filtered rebuild. The
// index is reconciled from the live slice on every mutation; if a bucket entry
// is ever stale the in-bucket scan simply finds no match there and moves on.
//
// Because which holder comes back is unspecified, this must not decide a value
// that goes out on the wire, and no longer does: the paths that read or write a
// lease's state and epoch take the file as a parameter and resolve through
// leaseRecordsOnHandleLocked. What remains here is the LEASE_BREAK_ACK and
// reclaim routing, which are given only a lease key.
func (lm *Manager) findLeaseByKey(leaseKey [16]byte) (string, *UnifiedLock, int) {
	buckets := lm.leaseKeyIndex[leaseKey]
	if buckets == nil {
		return "", nil, -1
	}
	for handleKey := range buckets {
		for i, lock := range lm.unifiedLocks[handleKey] {
			if lock.Lease != nil && lock.Lease.LeaseKey == leaseKey {
				return handleKey, lock, i
			}
		}
	}
	return "", nil, -1
}

// hasLeaseKeyOnOtherFile reports whether leaseKey is bound to a lease record
// owned by clientID on a handleKey other than excludeHandleKey. Lease records
// persisted at LeaseState=None after ack-to-None still count as bound: per
// MS-SMB2 3.3.5.9.8 the binding lasts until CLOSE removes the record.
//
// Spec scoping is per-(ClientGuid, LeaseKey). The SMB adapter currently
// derives clientID from the per-session SessionID ("smb:%d"), not the
// negotiated SMB ClientGuid, so the rejection fires across opens within a
// single session but NOT across sessions of the same ClientGuid (e.g.
// multichannel binds, where two channels of the same client get distinct
// session IDs). This matches the repo's existing ClientID concept (used by
// NLM lock conflict detection and lock owner tracking) and is sufficient
// for the smbtorture single-session duplicate_create / duplicate_open
// cases. Tightening to true ClientGuid scoping is tracked under the
// multichannel work (#361) where ClientGuid threading is needed
// for cross-channel break fan-out anyway.
//
// Probes only the buckets leaseKeyIndex names for leaseKey; the ones it omits
// hold no record for the key and could never have matched.
//
// Must be called with lm.mu held (read or write).
func (lm *Manager) hasLeaseKeyOnOtherFile(leaseKey [16]byte, excludeHandleKey, clientID string) bool {
	for handleKey := range lm.leaseKeyIndex[leaseKey] {
		if handleKey == excludeHandleKey {
			continue
		}
		for _, lock := range lm.unifiedLocks[handleKey] {
			if lock.Lease == nil || lock.Lease.LeaseKey != leaseKey {
				continue
			}
			if lock.Owner.ClientID != clientID {
				continue
			}
			return true
		}
	}
	return false
}

// hasPersistedLeaseKeyOnOtherFile is the post-restart backstop for the
// in-memory hasLeaseKeyOnOtherFile check. After a server restart the
// unifiedLocks map is empty until clients reclaim during the grace window;
// without this lookup, two clients (or the same client across reconnects)
// could each succeed at binding the same lease key to different files
// before either reclaim happens, breaking MS-SMB2 3.3.5.9.8 uniqueness.
//
// Implementation pulls the client-scoped lease set from lockStore and walks
// for a matching key on a different FileID. Same scoping caveats as
// hasLeaseKeyOnOtherFile (clientID is session-scoped today; tracked under
// #361). Called BEFORE lm.mu.Lock() — same pattern as the existing
// CheckNLMLocksForLeaseConflict pre-check — so external IO does not block
// the in-memory critical section. The race window between this snapshot and
// the in-memory grant is closed by the second hasLeaseKeyOnOtherFile call
// inside the critical section: any intervening reclaim or grant lands in
// unifiedLocks and is caught there.
//
// On a transient ListLocks failure the function fails CLOSED — returns true
// to reject the CREATE with STATUS_INVALID_PARAMETER. The MS-SMB2 §3.3.5.9.8
// uniqueness invariant is a hard correctness contract: silently allowing a
// potentially conflicting grant would be worse than a retriable false
// positive. The error is logged at Error level for ops visibility.
func (lm *Manager) hasPersistedLeaseKeyOnOtherFile(ctx context.Context, leaseKey [16]byte, excludeHandleKey, clientID string) bool {
	if lm.lockStore == nil || clientID == "" {
		return false
	}
	isLease := true
	persisted, err := lm.lockStore.ListLocks(ctx, LockQuery{
		ClientID: clientID,
		IsLease:  &isLease,
	})
	if err != nil {
		logger.Error("hasPersistedLeaseKeyOnOtherFile: ListLocks failed; failing closed to preserve cross-file lease-key uniqueness",
			"clientID", clientID,
			"error", err)
		return true
	}
	for _, pl := range persisted {
		if len(pl.LeaseKey) != 16 {
			continue
		}
		var plKey [16]byte
		copy(plKey[:], pl.LeaseKey)
		if plKey != leaseKey {
			continue
		}
		if pl.FileID == excludeHandleKey {
			continue
		}
		return true
	}
	return false
}

// leaseRequest carries one lease-grant request through the steps of the grant
// path. It is normalized once by requestLeaseImpl and then passed by value to
// each step, so no step can observe a pre-normalization state.
//
// `isTraditionalOplock` distinguishes real SMB2.1+ leases from synthetic-key
// records modeling traditional oplocks (LEVEL_II/Exclusive/Batch). The flag is
// consumed by `bestGrantableState` to apply the MS-SMB2 §3.3.5.9 cross-tier
// rules:
//
//   - traditional-oplock requestor + any other-key holder with H bit → NONE
//     (Samba `state.got_handle_lease` in `delay_for_oplock_fn`)
//   - real-lease requestor + any other-key traditional-oplock holder → strip H
//     (Samba `state.got_oplock`)
//
// And it propagates onto the new record via `createAndGrantLease` so subsequent
// grants can detect it.
//
// `suppressConflictBreak` carves out the stat-open case (MS-SMB2 §3.3.5.9.8 /
// Samba `is_lease_stat_open` in source3/smbd/open.c). A stat-open requester
// (FILE_READ_ATTRIBUTES / FILE_WRITE_ATTRIBUTES / READ_CONTROL / SYNCHRONIZE only)
// wants to cache attributes alongside existing holders without forcing them to
// drop their caches — so a cross-key conflict must NOT dispatch a break against
// the existing holder. The stat-opener instead receives the best state it can
// coexist with (`bestGrantableState`). Without this carve-out the break is
// timing-dependent: it fires only while the existing holder still carries the
// Write bit that the stat-opener's Read "conflicts" with, producing the
// intermittent spurious break that smb2.lease.statopen4's CHECK_NO_BREAK
// observes (#751).
type leaseRequest struct {
	fileHandle     FileHandle
	handleKey      string // string(fileHandle), the unifiedLocks bucket key
	leaseKey       [16]byte
	parentLeaseKey [16]byte
	ownerID        string
	clientID       string
	shareName      string
	state          uint32 // requested lease state, normalized

	isDirectory           bool
	isTraditionalOplock   bool
	suppressConflictBreak bool
}

// isNoReadCachingCombination reports whether a requested state asks for W, H or
// HW caching without R. Per Samba source3/smbd/open.c::delay_for_oplock the
// rule "any W or H without R → SMB2_LEASE_NONE" applies universally (files and
// directories alike) and is enforced before any conflict resolution. The
// smbtorture smb2.lease.request matrix asserts NT_STATUS_OK with granted
// state="" for H, W, and HW.
//
// The gate is on the R/W/H bits with reserved bits masked off first, so a
// request like 0x09 (R + reserved bit 0x08) is still treated as R-bearing and
// passes through to bestGrantableState rather than being coerced to None —
// matching Samba's behavior of ignoring reserved bits while still honoring Read.
func isNoReadCachingCombination(state uint32) bool {
	const knownLeaseBits = LeaseStateRead | LeaseStateWrite | LeaseStateHandle
	masked := state & knownLeaseBits
	return masked != LeaseStateNone && masked&LeaseStateRead == 0
}

// requestLeaseImpl is the underlying lease-grant implementation. It normalizes
// the request, answers the cheap denials and probes, then resolves conflicts
// under lm.mu and grants the best available state.
func (lm *Manager) requestLeaseImpl(ctx context.Context, req leaseRequest) (grantedState uint32, epoch uint16, err error) {
	// Coerce no-Read caching combinations to LeaseState=None and grant
	// successfully. Returning here (instead of falling through to
	// bestGrantableState) is deliberate: that helper's degradation chain ends at
	// LeaseStateRead, which would wrongly grant R for a W/H/HW request whose
	// original intent was a non-Read caching right.
	if isNoReadCachingCombination(req.state) {
		logger.Debug("RequestLease: no-Read caching combination, coercing to None",
			"state", LeaseStateToString(req.state),
			"fileHandle", string(req.fileHandle),
			"isDirectory", req.isDirectory)
		return LeaseStateNone, 0, nil
	}

	// Directories may only be granted Read/Handle caching, never Write
	// (MS-SMB2 §3.3.5.9.11 / IsValidDirectoryLeaseState allows only None, R,
	// RH). Strip Write here, at the single request-normalization point, so
	// BOTH the same-key upgrade path (isValidUpgrade) and the initial-grant
	// path (bestGrantableState) observe a W-stripped request. The grant path
	// already re-validates via IsValidDirectoryLeaseState, but isValidUpgrade
	// is directory-blind — without this an RH→RWH upgrade would hand a
	// directory an illegal write-caching lease. Windows then treats its local
	// directory view as authoritative, serves a stale (empty) listing from
	// cache, and deletes the folder without first enumerating and removing its
	// children — so the rmdir fails with STATUS_DIRECTORY_NOT_EMPTY (#1570).
	if req.isDirectory {
		req.state &^= LeaseStateWrite
	}

	req.handleKey = string(req.fileHandle)

	if req.state == LeaseStateNone {
		return lm.probeLeaseState(req)
	}

	if denied, derr := lm.preGrantDenial(ctx, req); denied {
		return LeaseStateNone, 0, derr
	}

	lm.mu.Lock()

	// Cross-(client, file) lease key uniqueness (MS-SMB2 3.3.5.9.8 / Samba
	// lease_match in source3/smbd/smb2_lease.c). A lease key bound by THIS
	// CLIENT to a record on a different file MUST fail the request with
	// STATUS_INVALID_PARAMETER. The check runs inside the write lock so that
	// uniqueness and grant are atomic — a downgrade-then-Lock split would
	// open a TOCTOU window where a concurrent CLOSE turns the rejection into
	// a false-positive, and where two parallel grants on different files
	// could both observe "no conflict" and create duplicate records.
	//
	// Same-file reopen (h1a/h1b in smbtorture breaking2) lands in the same
	// handleKey bucket and is allowed; ack-to-None records persisted under
	// the original handleKey still count as bindings here (handle-bound
	// lifetime, PR #452).
	if lm.hasLeaseKeyOnOtherFile(req.leaseKey, req.handleKey, req.clientID) {
		lm.unlock()
		logger.Debug("RequestLease: lease key already bound to another file for this client",
			"leaseKey", fmt.Sprintf("%x", req.leaseKey),
			"fileHandle", req.handleKey,
			"clientID", req.clientID,
			"requestedState", LeaseStateToString(req.state))
		return LeaseStateNone, 0, ErrLeaseKeyInUse
	}

	locks := lm.unifiedLocks[req.handleKey]

	if deleg := conflictingDelegation(locks, req.state); deleg != nil {
		lm.unlock()
		logger.Debug("RequestLease: delegation conflict, denying lease",
			"fileHandle", req.handleKey,
			"delegationType", deleg.DelegType.String(),
			"requestedState", LeaseStateToString(req.state))
		return LeaseStateNone, 0, fmt.Errorf("lease denied: conflicts with %s delegation on file",
			deleg.DelegType.String())
	}

	if handled, state, ep, serr := lm.resolveSameKeyLeaseLocked(req, locks); handled {
		lm.unlock()
		return state, ep, serr
	}

	// No existing lease with the same key: break conflicting other-key holders,
	// then grant the best available state (may be less than requested).
	locks = lm.breakConflictingLeasesLocked(req, locks)

	// After any break (or no-op skip), find the best grantable state.
	// Per MS-SMB2 3.3.5.9: the server MUST grant the best available oplock
	// level. Try the full requested state first, then progressively lower
	// states: strip Write, then strip Handle, then Read only, then None.
	grantState := bestGrantableState(locks, req.leaseKey, req.state, req.isDirectory, req.isTraditionalOplock)
	if grantState == LeaseStateNone {
		lm.unlock()
		logger.Debug("RequestLease: no compatible state after conflict resolution",
			"fileHandle", req.handleKey,
			"requestedState", LeaseStateToString(req.state))
		return LeaseStateNone, 0, nil
	}

	granted, grantedEpoch := lm.createAndGrantLease(ctx, req, grantState)
	lm.unlock()

	logger.Debug("RequestLease: granted lease",
		"fileHandle", req.handleKey,
		"requested", LeaseStateToString(req.state),
		"granted", LeaseStateToString(granted),
		"isDirectory", req.isDirectory,
		"downgraded", grantState != req.state,
		"epoch", grantedEpoch)

	return granted, grantedEpoch, nil
}

// probeLeaseState answers a LeaseStateNone request. Clients (and smbtorture
// breaking4 / upgrade2) issue empty-state requests to query the current lease
// without taking new caching rights. Per Samba upgrade2 the response is the
// current state of any same-key lease (R returns R, RH returns RH, …) — *not*
// always None. A None probe with no same-key lease returns None trivially. The
// probe short-circuits the whole grant path so a None request never enters the
// cross-key break dispatch, and it is exempt from lease_match uniqueness: a
// zero-state request cannot acquire caching rights.
func (lm *Manager) probeLeaseState(req leaseRequest) (uint32, uint16, error) {
	lm.mu.Lock()
	for _, lock := range lm.unifiedLocks[req.handleKey] {
		if lock.Lease == nil || lock.Lease.LeaseKey != req.leaseKey {
			continue
		}
		currentState := lock.Lease.LeaseState
		epoch := lock.Lease.Epoch
		breaking := lock.Lease.Breaking
		lm.unlock()
		if breaking {
			logger.Debug("RequestLease: None-probe on breaking same-key lease, surfacing break-in-progress",
				"fileHandle", req.handleKey,
				"currentState", LeaseStateToString(currentState),
				"epoch", epoch)
			return currentState, epoch, ErrLeaseBreakInProgress
		}
		return currentState, epoch, nil
	}
	lm.unlock()
	return LeaseStateNone, 0, nil
}

// preGrantDenial runs the lock-free denials that precede conflict resolution:
// the directory recently-broken cache, the byte-range lock conflict gate, and
// the persisted cross-file lease-key uniqueness backstop. denied=true means the
// caller must answer LeaseStateNone with the returned error (nil for a plain
// denial).
func (lm *Manager) preGrantDenial(ctx context.Context, req leaseRequest) (bool, error) {
	if req.isDirectory && lm.recentlyBroken != nil && lm.recentlyBroken.IsRecentlyBroken(req.handleKey) {
		logger.Debug("RequestLease: directory recently broken, denying",
			"fileHandle", req.handleKey)
		return true, nil
	}

	// Per MS-SMB2 §3.3.5.9.8: if any byte-range lock is outstanding on the
	// file, the server MUST grant leaseState = NONE. Check both the
	// persisted lockStore (NLM-side) and the in-memory lm.locks map
	// (SMB2 LOCK callers; not yet pushed through lockStore).
	if lm.hasByteRangeLockConflictForLease(ctx, req.handleKey, req.state, req.clientID) {
		logger.Debug("RequestLease: byte-range lock conflict, denying lease",
			"fileHandle", req.handleKey,
			"requestedState", LeaseStateToString(req.state))
		return true, nil
	}

	// Cross-file lease-key uniqueness — persisted backstop for post-restart
	// state. The in-memory check inside lm.mu catches the steady-state case;
	// this pre-check covers the window after a restart but before the owning
	// client has reclaimed the lease into memory.
	if lm.hasPersistedLeaseKeyOnOtherFile(ctx, req.leaseKey, req.handleKey, req.clientID) {
		logger.Debug("RequestLease: lease key already bound to another file (persisted record)",
			"leaseKey", fmt.Sprintf("%x", req.leaseKey),
			"fileHandle", req.handleKey,
			"clientID", req.clientID,
			"requestedState", LeaseStateToString(req.state))
		return true, ErrLeaseKeyInUse
	}

	return false, nil
}

// conflictingDelegation returns the first delegation on the file that cannot
// coexist with a lease in the requested state, or nil. Must hold lm.mu.
func conflictingDelegation(locks []*UnifiedLock, requestedState uint32) *Delegation {
	for _, lock := range locks {
		if lock.Delegation == nil {
			continue
		}
		// Create a temporary OpLock to check coexistence
		tempLease := &OpLock{LeaseState: requestedState}
		if DelegationConflictsWithLease(lock.Delegation, tempLease) {
			return lock.Delegation
		}
	}
	return nil
}

// resolveSameKeyLeaseLocked answers a request whose lease key already holds a
// record on this file: break-in-progress, exact no-op, valid upgrade, or a
// non-superset request. handled=false means no same-key record exists and the
// caller falls through to cross-key conflict resolution. Must hold lm.mu; the
// caller releases it.
func (lm *Manager) resolveSameKeyLeaseLocked(req leaseRequest, locks []*UnifiedLock) (handled bool, state uint32, epoch uint16, err error) {
	for i, lock := range locks {
		if lock.Lease == nil || lock.Lease.LeaseKey != req.leaseKey {
			continue
		}

		currentState := lock.Lease.LeaseState

		// Per MS-SMB2 3.3.5.9.8: If the lease is in Breaking state, do NOT
		// modify it. Return the current LeaseState and signal break-in-progress
		// to the caller so it can set SMB2_LEASE_FLAG_BREAK_IN_PROGRESS (0x02).
		if lock.Lease.Breaking {
			logger.Debug("RequestLease: same-key lease is breaking, returning current state with break-in-progress",
				"fileHandle", req.handleKey,
				"currentState", LeaseStateToString(currentState),
				"epoch", lock.Lease.Epoch)
			return true, currentState, lock.Lease.Epoch, ErrLeaseBreakInProgress
		}

		// Same state requested - return current (no-op)
		if currentState == req.state {
			return true, currentState, lock.Lease.Epoch, nil
		}

		// Check if this is a valid upgrade AND can coexist with any other
		// leases on the same file (Samba upgrade3 contended-case rule):
		// the upgrade applies iff the requested state is a strict superset
		// of the current AND does not conflict with any other-key holder.
		// If the upgrade would conflict, leave the current state unchanged
		// — the rule explicitly forbids breaking other holders to satisfy
		// a same-key upgrade.
		canUpgrade := isValidUpgrade(currentState, req.state)
		if canUpgrade {
			requestedLease := &OpLock{LeaseKey: req.leaseKey, LeaseState: req.state}
			for _, other := range locks {
				if other.Lease == nil || other.Lease.LeaseKey == req.leaseKey {
					continue
				}
				if OpLocksConflict(other.Lease, requestedLease) {
					canUpgrade = false
					logger.Debug("RequestLease: upgrade blocked by other-key holder",
						"fileHandle", req.handleKey,
						"current", LeaseStateToString(currentState),
						"requested", LeaseStateToString(req.state),
						"otherState", LeaseStateToString(other.Lease.LeaseState))
					break
				}
			}
		}
		if canUpgrade {
			locks[i].Lease.LeaseState = req.state
			advanceEpoch(locks[i].Lease)

			logger.Debug("RequestLease: upgraded lease",
				"fileHandle", req.handleKey,
				"from", LeaseStateToString(currentState),
				"to", LeaseStateToString(req.state),
				"epoch", locks[i].Lease.Epoch)

			// Persist if store available
			lm.persistUnifiedLockLocked(locks[i])

			return true, req.state, locks[i].Lease.Epoch, nil
		}

		// Non-superset request (downgrade or sidegrade): per Samba upgrade2,
		// same-key RequestLease changes the lease iff requested is a strict
		// superset of current. Otherwise the existing state is returned
		// unchanged (e.g. RH + request RW → return RH; R + request "" → R).
		// Returning None here would silently drop the holder's caching
		// rights and break the smbtorture upgrade / upgrade2 / upgrade3
		// matrix.
		logger.Debug("RequestLease: same-key non-superset request, returning existing state",
			"fileHandle", req.handleKey,
			"current", LeaseStateToString(currentState),
			"requested", LeaseStateToString(req.state))
		return true, currentState, locks[i].Lease.Epoch, nil
	}
	return false, LeaseStateNone, 0, nil
}

// breakConflictingLeasesLocked walks the other-key holders on this file and,
// per MS-SMB2 3.3.5.9, initiates the break against the first one that conflicts
// with the requested state. At most one break is started per request.
//
// Must hold lm.mu on entry; lm.mu is held again on return, but the lock IS
// released around the notification dispatch, so the caller must treat any slice
// or pointer it held across this call as stale. The returned slice is the
// current lock list for the file and replaces the caller's.
func (lm *Manager) breakConflictingLeasesLocked(req leaseRequest, locks []*UnifiedLock) []*UnifiedLock {
	for _, lock := range locks {
		if lock.Lease == nil {
			continue
		}

		// Create temporary OpLock for conflict check
		requested := &OpLock{
			LeaseKey:   req.leaseKey,
			LeaseState: req.state,
		}

		if !OpLocksConflict(lock.Lease, requested) {
			continue
		}

		// Stat-open carve-out (Samba `is_lease_stat_open`): the requester
		// only wants to cache attributes and must NOT force the existing
		// holder to drop its caches. Skip the break entirely; the
		// stat-opener falls through to bestGrantableState and receives the
		// best state it can coexist with. Suppressing dispatch here (rather
		// than at the CREATE layer alone) closes the timing window that made
		// the break depend on whether the holder still carried its Write bit
		// (#751 smb2.lease.statopen4 CHECK_NO_BREAK).
		if req.suppressConflictBreak {
			logger.Debug("RequestLease: stat-open requester, suppressing cross-key break",
				"fileHandle", req.handleKey,
				"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
				"existingState", LeaseStateToString(lock.Lease.LeaseState),
				"requestedState", LeaseStateToString(req.state))
			continue
		}

		// CREATE-time SMB share-mode/disposition checks run before
		// RqLs processing, so the cross-key conflicts that reach this
		// path are non-violating, non-destructive lease conflicts —
		// strip Write, keep Read + Handle. RWH→RH, RW→R.
		//
		// Effective state mirrors OpLocksConflict's view: a breaking
		// holder's pending downgrade (BreakingToRequired) is what the
		// new opener will actually contend with, so the break-to is
		// computed against that rather than the pre-break LeaseState.
		// Without this, a holder mid-break to RH still triggers a
		// fresh "strip W" dispatch here because LeaseState is still
		// RWH on paper — even though BreakingToRequired (RH) already
		// equals breakTo and the AND-merge below would be the only
		// useful action.
		effectiveState := lock.Lease.LeaseState
		if lock.Lease.Breaking {
			effectiveState = lock.Lease.BreakingToRequired
		}
		breakTo := ComputeLeaseBreakTo(effectiveState, BreakReasonDefault)

		// If the existing lease's effective state already satisfies
		// the break-to target, no further dispatch is needed: either
		// the holder has no Write bit to strip (e.g. fresh RH), or
		// the in-flight break is heading there already (cumulative
		// target via prior pre-RqLs break). The new opener proceeds
		// straight to bestGrantableState; the holder either stays put
		// or completes its in-flight break on its own.
		if breakTo == effectiveState {
			logger.Debug("RequestLease: cross-key conflict already satisfied by holder effective state, skipping break",
				"fileHandle", req.handleKey,
				"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
				"existingState", LeaseStateToString(lock.Lease.LeaseState),
				"effectiveState", LeaseStateToString(effectiveState),
				"breaking", lock.Lease.Breaking,
				"requestedState", LeaseStateToString(req.state))
			return locks
		}

		// Already-breaking lease: the SMB CREATE handler dispatches the
		// pre-RqLs break via BreakLeasesOnOpenConflict before invoking
		// RequestLease (see create_post_break.go::breakAndMaybeParkCreate).
		// AND-merge the new opener's target into BreakingToRequired but
		// suppress dispatch and epoch bump — re-marking would put a
		// duplicate LEASE_BREAK_NOTIFICATION on the wire and double-bump
		// the epoch. Mirrors the cumulative-target semantics in
		// breakOpLocks; the next progressive stage (if any) is dispatched
		// from acknowledgeLeaseBreakImpl after the in-flight ACK arrives.
		//
		// Required by smbtorture smb2.multichannel.leases.test3 (#436):
		// exactly ONE RWH→RH break, not two.
		//
		// Returns the caller's slice unchanged: lm.mu was never released, so
		// re-reading unifiedLocks would be pointless work.
		if lock.Lease.Breaking {
			lock.Lease.BreakingToRequired &= breakTo
			lm.persistUnifiedLockLocked(lock)
			logger.Debug("RequestLease: cross-key conflict on already-breaking lease, suppressed duplicate break",
				"fileHandle", req.handleKey,
				"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
				"requestedKey", fmt.Sprintf("%x", req.leaseKey),
				"existingBreakingTo", LeaseStateToString(lock.Lease.BreakingToRequired),
				"requestedState", LeaseStateToString(req.state))
			return locks
		}

		logger.Debug("RequestLease: cross-key conflict, initiating break",
			"fileHandle", req.handleKey,
			"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
			"requestedKey", fmt.Sprintf("%x", req.leaseKey),
			"existingState", LeaseStateToString(lock.Lease.LeaseState),
			"requestedState", LeaseStateToString(req.state),
			"breakToState", LeaseStateToString(breakTo))

		// Mark lease as breaking before dispatching callbacks. This is the
		// open-time lease-conflict downgrade (breakTo computed with
		// BreakReasonDefault above), so record the reason for symmetry with
		// breakOpLocks — a deadbeat holder that times out here must also
		// surface STATUS_UNSUCCESSFUL on a late ack.
		lock.Lease.Breaking = true
		lock.Lease.BreakToState = breakTo
		lock.Lease.BreakingToRequired = breakTo
		lock.Lease.BreakStarted = time.Now()
		lock.Lease.BreakReason = BreakReasonDefault
		advanceEpoch(lock.Lease)

		// Persist the breaking state
		lm.persistUnifiedLockLocked(lock)

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
		lm.unlock()
		lm.dispatchOpLockBreak(req.handleKey, lockSnapshot, breakTo)

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
		// so bestGrantableState computes the correct downgraded grant for
		// the new opener without needing the ack to land first. The same
		// async-dispatch pattern is used by
		// internal/adapter/smb/lease/manager.go BreakHandleLeasesOnOpenAsync,
		// whose comment explicitly documents this deadlock.
		lm.mu.Lock()
		return lm.unifiedLocks[req.handleKey]
	}
	return locks
}

// bestGrantableState finds the best lease state that can be granted without
// conflicting with existing leases from other keys. It tries the requested
// state first, then progressively lower states per MS-SMB2 3.3.5.9:
// requested -> strip W -> strip H -> R only -> None.
//
// `isTraditionalOplock` distinguishes the requestor's tier (real lease vs.
// synthetic-key traditional oplock). Per MS-SMB2 §3.3.5.9 and Samba
// `source3/smbd/open.c::grant_fsp_oplock_type` (lines 2663-2680):
//
//   - traditional-oplock requestor + any other-key holder with H bit
//     => NONE (Samba `state.got_handle_lease`).
//   - real-lease requestor + any other-key traditional-oplock holder
//     => strip H from the candidate before conflict check (Samba
//     `state.got_oplock`).
//
// The H bit in an existing holder is read from BreakingToRequired when
// the holder is mid-break (so a still-flushing RWH that is heading to RH
// keeps its H presence visible until ack lands), otherwise from
// LeaseState — same convention as `OpLocksConflict`.
//
// Precondition: caller must hold lm.mu (read or write). The locks slice is
// read from lm.unifiedLocks[handleKey] under that lock, so no concurrent
// mutation can occur while this function iterates.
func bestGrantableState(locks []*UnifiedLock, leaseKey [16]byte, requestedState uint32, isDirectory bool, isTraditionalOplock bool) uint32 {
	// Cross-tier pre-pass: scan once for the two sentinels Samba tracks in
	// `delay_for_oplock_fn` (got_handle_lease, got_oplock). Reading
	// effectiveLeaseState here keeps the post-break view consistent with
	// OpLocksConflict — a holder breaking to RH still counts as having H.
	var otherHasHandle, otherIsTradOplock bool
	for _, lock := range locks {
		if lock.Lease == nil || lock.Lease.LeaseKey == leaseKey {
			continue
		}
		state := lock.Lease.LeaseState
		if lock.Lease.Breaking {
			state = lock.Lease.BreakingToRequired
		}
		if state&LeaseStateHandle != 0 {
			otherHasHandle = true
		}
		if lock.Lease.IsTraditionalOplock {
			otherIsTradOplock = true
		}
	}

	// Rule 1: traditional-oplock requestor against any H-holder => NONE.
	if isTraditionalOplock && otherHasHandle {
		return LeaseStateNone
	}

	// Rule 2 mask: real-lease requestor against any traditional-oplock holder
	// must have H stripped from each candidate before the conflict check.
	// Loop-invariant so compute once.
	var stripMask uint32
	if !isTraditionalOplock && otherIsTradOplock {
		stripMask = LeaseStateHandle
	}

	// Active-holder cap (Samba `grant_fsp_oplock_type` source3/smbd/open.c
	// lines 2397-2403 + 2637-2643 + 2662-2672): when any other-key lease or
	// trad-oplock record on the file is still alive (NOT a timeout
	// tombstone), the new grant cannot carry W (Samba's
	// `disallow_write_lease`), and a trad-oplock requestor additionally
	// drops H so a BATCH/EXCLUSIVE request collapses to LEVEL_II — per the
	// `map_lease_type_to_oplock` round-trip which Samba applies for non-
	// LEASE_OPLOCK requests. Timeout tombstones (BrokenViaTimeout=true)
	// are excluded so smb2.oplock.batch22b can grant a fresh BATCH after
	// the abandoned holder times out. Required by smbtorture
	// smb2.oplock.batch9a / batch13 / batch14 / batch16.
	var hasOtherActiveHolder bool
	for _, lock := range locks {
		if lock.Lease == nil || lock.Lease.LeaseKey == leaseKey {
			continue
		}
		if lock.Lease.BrokenViaTimeout {
			continue
		}
		hasOtherActiveHolder = true
		break
	}
	if hasOtherActiveHolder {
		stripMask |= LeaseStateWrite
		if isTraditionalOplock {
			stripMask |= LeaseStateHandle
		}
	}

	candidates := downgradeCandidates(requestedState, isDirectory)

outer:
	for _, candidate := range candidates {
		effective := candidate &^ stripMask
		// Stripping may collapse to a state already tried; dedup is
		// unnecessary because the grant is idempotent.
		tempLease := &OpLock{
			LeaseKey:   leaseKey,
			LeaseState: effective,
		}
		for _, lock := range locks {
			if lock.Lease == nil || lock.Lease.LeaseKey == leaseKey {
				continue
			}
			if OpLocksConflict(lock.Lease, tempLease) {
				continue outer
			}
		}
		// effective may differ from candidate (H stripped); return what
		// was actually granted so the caller persists the post-strip state.
		return effective
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
func (lm *Manager) createAndGrantLease(_ context.Context, req leaseRequest, grantState uint32) (uint32, uint16) {
	newLock := &UnifiedLock{
		ID: uuid.New().String(),
		Owner: LockOwner{
			OwnerID:   req.ownerID,
			ClientID:  req.clientID,
			ShareName: req.shareName,
		},
		FileHandle: req.fileHandle,
		Offset:     0,
		Length:     0,
		Type:       lockTypeForLeaseState(grantState),
		AcquiredAt: time.Now(),
		Lease: &OpLock{
			LeaseKey:            req.leaseKey,
			LeaseState:          grantState,
			ParentLeaseKey:      req.parentLeaseKey,
			IsDirectory:         req.isDirectory,
			IsTraditionalOplock: req.isTraditionalOplock,
			Epoch:               1, // New leases start at epoch 1
		},
	}

	lm.unifiedLocks[req.handleKey] = append(lm.unifiedLocks[req.handleKey], newLock)
	lm.indexAddLockLocked(req.handleKey, newLock)

	lm.persistUnifiedLockLocked(newLock)

	return grantState, 1
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
// is LeaseStateNone, the lease is downgraded to None but the record is kept
// alive until the holding handle CLOSEs (see ack-to-None block below).
func (lm *Manager) acknowledgeLeaseBreakImpl(_ context.Context, leaseKey [16]byte,
	acknowledgedState uint32, epoch uint16) error {

	lm.mu.Lock()
	defer lm.unlock()

	handleKey, lock, _ := lm.findLeaseByKey(leaseKey)
	if lock == nil {
		return ErrLeaseAckNotFound
	}

	if !lock.Lease.Breaking {
		return classifyLateAck(lock, leaseKey, acknowledgedState)
	}

	if err := validateAck(lock, acknowledgedState, epoch); err != nil {
		return err
	}

	if acknowledgedState == LeaseStateNone {
		lm.completeAckToNoneLocked(handleKey, leaseKey, lock)
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

	// Progressive multi-stage break: if the cumulative final target
	// (BreakingToRequired) is stricter than what the client just acknowledged,
	// the break is not done and the next stage goes out now.
	if acknowledgedState&^lock.Lease.BreakingToRequired != 0 {
		lm.dispatchNextBreakStageLocked(handleKey, leaseKey, lock, acknowledgedState)
		return nil
	}

	// Reached BreakingToRequired (or full release): mirror invariant
	// "BreakingToRequired == LeaseState when not Breaking" and signal.
	lock.Lease.BreakingToRequired = acknowledgedState

	// Persist updated state
	lm.persistUnifiedLockLocked(lock)
	lm.clearBreakingSiblingsLocked(leaseKey, lock)

	logger.Debug("AcknowledgeLeaseBreak: break acknowledged",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"newState", LeaseStateToString(acknowledgedState),
		"epoch", lock.Lease.Epoch)

	lm.signalBreakWaitLocked(handleKey)
	return nil
}

// classifyLateAck answers an ACK for a lease that is no longer Breaking.
//
// Late ACK after server-side timeout: forceCompleteBreaks already auto-revoked
// the lease to None and tagged BrokenViaTimeout. The client's late ACK is then a
// benign acknowledgment of a break the server has already completed — return
// STATUS_OK silently, regardless of which (subset) state the client names. We do
// NOT resurrect any bits: LeaseState stays None. BrokenViaTimeout is left
// untouched so the downstream grant-coercion path (OnlyTimeoutTombstoneRecords)
// still treats the record as a tombstone for smbtorture batch22b semantics.
//
// The acknowledgedState is intentionally NOT constrained to None: a parked
// share-violation CREATE that force-completes the holder's RWH lease (revoking
// to None) races the holder's deferred Handle-strip ACK (break-to RW). When the
// 5 s force-complete wins under CI jitter, the holder ACKs RW post-timeout and
// must still succeed — smbtorture dhv2-pending1n-vs-violation-lease-ack-sane
// (#1322). forceCompleteBreaks zeroes BreakToState, so there is no surviving
// break-to to validate the subset against; the tombstoned None is the
// authoritative outcome either way.
//
// Also required by WPTS BVT_DirectoryLeasing_ReadWriteHandleCaching (#454) where
// the SUT controller's synchronous CreateFile holds the test client past the 5 s
// parent-break timeout, so the ACK can only be sent post-force-complete.
// smbtorture breaking2 / breaking5 ACK within the breaking window, so their
// post-ack duplicate has BrokenViaTimeout=false and still surfaces as
// STATUS_UNSUCCESSFUL via the fall-through. Must hold lm.mu.
func classifyLateAck(lock *UnifiedLock, leaseKey [16]byte, acknowledgedState uint32) error {
	if !lock.Lease.BrokenViaTimeout || lock.Lease.LeaseState != LeaseStateNone {
		return ErrLeaseAckNotBreaking
	}

	// A plain open-time lease-conflict downgrade (BreakReasonDefault on a file
	// lease) that the holder ignored straight through the force-complete is a
	// genuine deadbeat: MS-SMB2 §3.3.5.22.2 requires the late ACK to fail with
	// STATUS_UNSUCCESSFUL (smb2.lease.timeout). Other force-completes —
	// sharing-violation handle-strips (#1322) and parent-directory breaks
	// (#454/WPTS) — fire under CI jitter while the holder's ACK is merely in
	// flight, and must still succeed. The break reason, recorded at break time
	// and preserved across force-complete, is the only signal that distinguishes
	// them (post-force-complete state is otherwise identical).
	if lock.Lease.BreakReason == BreakReasonDefault && !lock.Lease.IsDirectory {
		logger.Debug("AcknowledgeLeaseBreak: late ACK after lease-conflict force-complete → UNSUCCESSFUL",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"acknowledgedState", LeaseStateToString(acknowledgedState))
		return ErrLeaseAckNotBreaking
	}
	logger.Debug("AcknowledgeLeaseBreak: late ACK after timeout force-complete, treating as success",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"acknowledgedState", LeaseStateToString(acknowledgedState),
		"breakReason", lock.Lease.BreakReason)
	return nil
}

// validateAck rejects a stale epoch and an over-claiming acknowledged state.
// Must hold lm.mu.
func validateAck(lock *UnifiedLock, acknowledgedState uint32, epoch uint16) error {
	// Validate epoch if provided (V2 staleness check).
	// The epoch was already advanced during break initiation, so the client
	// should echo the current epoch value from the break notification.
	if epoch != 0 && lock.Lease.Epoch != epoch {
		return fmt.Errorf("stale epoch: expected %d, got %d", lock.Lease.Epoch, epoch)
	}

	// Client cannot claim bits not offered (bitwise subset check).
	// Per MS-SMB2 3.3.5.22.2, this must surface as STATUS_REQUEST_NOT_ACCEPTED.
	if acknowledgedState & ^lock.Lease.BreakToState != 0 {
		return fmt.Errorf("%w: %s exceeds break-to %s",
			ErrAcknowledgedStateExceedsBreakTo,
			LeaseStateToString(acknowledgedState),
			LeaseStateToString(lock.Lease.BreakToState))
	}
	return nil
}

// completeAckToNoneLocked keeps the record alive at LeaseState=None until the
// holding handle CLOSEs (ReleaseLeaseForHandle removes it). This mirrors Samba
// behavior and lets the wrapper distinguish a duplicate ack on an already-
// released lease (record present, Breaking=false → ErrLeaseAckNotBreaking →
// STATUS_UNSUCCESSFUL, smbtorture breaking2/breaking5) from a CLOSE-beat-ack
// race (record gone → ErrLeaseAckNotFound → silent success, WPTS
// BVT_DirectoryLeasing_ReadWriteHandleCaching). Must hold lm.mu.
func (lm *Manager) completeAckToNoneLocked(handleKey string, leaseKey [16]byte, lock *UnifiedLock) {
	lock.Lease.LeaseState = LeaseStateNone
	lock.Lease.Breaking = false
	lock.Lease.BreakToState = 0
	lock.Lease.BreakingToRequired = LeaseStateNone
	lock.Lease.BreakStarted = time.Time{}
	lock.Type = lockTypeForLeaseState(LeaseStateNone)

	lm.persistUnifiedLockLocked(lock)
	lm.clearBreakingSiblingsLocked(leaseKey, lock)

	logger.Debug("AcknowledgeLeaseBreak: lease released to None (record kept until CLOSE)",
		"leaseKey", fmt.Sprintf("%x", leaseKey))
	lm.signalBreakWaitLocked(handleKey)
	// Deliver the next directory RH-lease break on this directory that was
	// deferred behind this one, so multi-lease dir breaks serialize.
	lm.dispatchNextDeferredDirBreakLocked(handleKey)
}

// dispatchNextBreakStageLocked sends the next stage of a progressive multi-stage
// break, for an ACK that landed short of the cumulative final target. Mirrors
// Samba `downgrade_lease` (source3/smbd/smb2_oplock.c lines 569-586): if the
// acked state still has W or H, the next target keeps R as an intermediate;
// otherwise it drops straight to the cumulative required.
//
// This produces the smbtorture breaking3 / v2_breaking3 wire shape:
//
//	ack RWH→RH  ⇒ next target = R  ⇒ wire: RH→R
//	ack RH→R    ⇒ next target = 0  ⇒ wire: R→""
//
// Must hold lm.mu on entry; lm.mu is held again on return, but it IS released
// around the dispatch, so `lock` must be treated as stale afterwards.
func (lm *Manager) dispatchNextBreakStageLocked(handleKey string, leaseKey [16]byte, lock *UnifiedLock, acknowledgedState uint32) {
	nextTarget := nextProgressiveBreakTarget(acknowledgedState, lock.Lease.BreakingToRequired)
	snapshot := lm.applyBreakStageLocked(lock, nextTarget)

	// Persist the next-stage state BEFORE releasing lm.mu so the durable
	// store reflects Breaking=true / BreakToState=nextTarget. Otherwise a
	// crash between the ACK-clear (Breaking=false written by the caller) and
	// the next-stage-set would lose the second progressive stage on restart,
	// leaving parked CREATEs to wait until the scanner timeout. The persist
	// MUST land, so errors are logged (not swallowed) by the helper.
	lm.persistUnifiedLockLocked(lock)

	logger.Debug("AcknowledgeLeaseBreak: progressive break next stage",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"ackedState", LeaseStateToString(acknowledgedState),
		"required", LeaseStateToString(lock.Lease.BreakingToRequired),
		"nextTarget", LeaseStateToString(nextTarget),
		"epoch", lock.Lease.Epoch)

	// Release lm.mu before dispatching to avoid deadlock with the SMB transport
	// callback (mirrors breakOpLocks pattern). The deferred re-Lock keeps the
	// caller's `defer lm.unlock()` correct even if dispatchOpLockBreak panics
	// — without it the unwind would run through an unlocked mutex and the outer
	// defer would double-unlock.
	func() {
		lm.unlock()
		defer lm.mu.Lock()
		lm.dispatchOpLockBreak(handleKey, snapshot, nextTarget)
	}()

	// Re-validate: a concurrent CLOSE / release / timeout could have removed the
	// lease during the dispatch window. The `lock` pointer may now reference an
	// orphaned UnifiedLock — read fields off the re-found record (or signal
	// waiters and return when gone).
	_, currentLock, _ := lm.findLeaseByKey(leaseKey)
	if currentLock == nil {
		lm.signalBreakWaitLocked(handleKey)
		return
	}

	// Signal waiters only when the break has fully drained: either the inline
	// fire-and-forget path already updated LeaseState to nextTarget (no further
	// ACK will arrive), or a concurrent path removed the lease. Otherwise the
	// break is still in progress and waiters must keep waiting.
	if nextTarget == currentLock.Lease.LeaseState {
		lm.signalBreakWaitLocked(handleKey)
	}
}

// clearBreakingSiblingsLocked syncs every other lease record sharing leaseKey to
// primary's post-acknowledge state. Opens sharing a lease key are one logical
// lease: OnDirChange and breakOpLocks put such sibling records into Breaking=true
// via mirrorBreakStageLocked without ever sending a wire notification (only one
// break is dispatched for the shared key). A client's single acknowledge resolves
// through findLeaseByKey to exactly one record, so without this sync the mirrored
// siblings stay Breaking=true forever and any WaitForBreakCompletion on their
// handleKey blocks until the force-complete timeout — a per-operation stall under
// directory churn. Progressive multi-stage breaks (partial acks) are file-lease
// only and never mirror across a shared key, so they are unaffected. Must hold lm.mu.
func (lm *Manager) clearBreakingSiblingsLocked(leaseKey [16]byte, primary *UnifiedLock) {
	for handleKey := range lm.leaseKeyIndex[leaseKey] {
		cleared := false
		for _, lock := range lm.unifiedLocks[handleKey] {
			if lock == primary || lock.Lease == nil ||
				lock.Lease.LeaseKey != leaseKey || !lock.Lease.Breaking {
				continue
			}
			lock.Lease.LeaseState = primary.Lease.LeaseState
			lock.Lease.Breaking = false
			lock.Lease.BreakToState = 0
			lock.Lease.BreakingToRequired = primary.Lease.BreakingToRequired
			lock.Lease.BreakStarted = time.Time{}
			lock.Type = lockTypeForLeaseState(primary.Lease.LeaseState)
			lm.persistUnifiedLockLocked(lock)
			cleared = true
		}
		if cleared {
			lm.signalBreakWaitLocked(handleKey)
		}
	}
}

// ReleaseLease releases all lease state for the given lease key.
func (lm *Manager) releaseLeaseImpl(ctx context.Context, leaseKey [16]byte) error {
	lm.mu.Lock()
	defer lm.unlock()

	// Find and remove all locks with matching lease key. The same lease key
	// constant can be bound on multiple files (distinct handleKey buckets), and
	// findLeaseByKey returns only one holder, so this intentionally scans every
	// bucket — ReleaseLease must scrub the key everywhere, not just one bucket.
	for handleKey, locks := range lm.unifiedLocks {
		var remaining []*UnifiedLock
		mutated := false
		for _, lock := range locks {
			if lock.Lease != nil && lock.Lease.LeaseKey == leaseKey {
				// Remove from persistent store
				lm.deleteUnifiedLockLocked(lock)
				mutated = true
				continue // Skip (remove) this lock
			}
			remaining = append(remaining, lock)
		}

		if !mutated {
			continue
		}
		if len(remaining) == 0 {
			delete(lm.unifiedLocks, handleKey)
		} else {
			lm.unifiedLocks[handleKey] = remaining
		}
		lm.reindexHandleLocked(handleKey, locks)
	}

	return nil
}

// ReleaseLeaseForHandle removes lease records matching leaseKey from a single
// handleKey bucket. Unlike ReleaseLease, this does NOT touch records on other
// handles that happen to share the same key.
//
// The same LeaseKey constant can appear on different files (different
// handleKey buckets) — typical for smbtorture which uses fixed LEASE1/LEASE2
// macros across tests. Releasing one open on file A must not erase the lease
// record for a concurrent open on file B; otherwise stale records accumulate
// on the surviving file and break ACK lookup / break-to matching.
func (lm *Manager) releaseLeaseForHandleImpl(ctx context.Context, handleKey string, leaseKey [16]byte) error {
	// The deletes are queued on the file's persist lane like every other
	// mutation, and they run before releaseLeaseRecords returns, so their
	// errors are complete by the time they are read here.
	var deleteErrs []error
	lm.releaseLeaseRecords(ctx, handleKey, leaseKey, &deleteErrs)

	if len(deleteErrs) > 0 {
		return fmt.Errorf("release lease for handle %q: %w", handleKey, errors.Join(deleteErrs...))
	}
	return nil
}

func (lm *Manager) releaseLeaseRecords(ctx context.Context, handleKey string, leaseKey [16]byte, deleteErrs *[]error) {
	lm.mu.Lock()
	defer lm.unlock()

	locks := lm.unifiedLocks[handleKey]
	if len(locks) == 0 {
		return
	}

	// hadBreaking records whether any lease we remove here was in the Breaking
	// state, so we can wake WaitForBreakCompletion waiters before they hit the
	// 5s timeout. The releaseLeaseForHandle path is the no-ack way to complete a
	// break (the holder closes its conflicting handle in response to the break
	// notification instead of sending LEASE_BREAK_ACK), and the waiting party
	// cannot make progress until either the signal fires or the deadline
	// expires.
	//
	// Directory leases: a parent-dir Handle-break wait set up by SET_INFO
	// rename / hardlink (smb2.dirlease.rename_dst_parent phase-2).
	//
	// File leases / traditional oplocks: a parked CREATE behind the holder's
	// break must finalize the MOMENT the holder releases, not only on the
	// break-wait timeout. smbtorture smb2.replay.dhv2-pending2*-sane disconnects
	// the parked CREATE's originating channel and replays on a surviving channel
	// before the 5s timer fires; the replay must find the finalized completion
	// (FILE_NOT_AVAILABLE while parked → OK once the holder closes). The wake is
	// gated on the released lease having been Breaking, so it fires only when a
	// break was actually in flight — holders that ACK (smb2.lease.breaking3) wake
	// via acknowledgeLeaseBreakImpl, and holders that neither ACK nor close
	// (smb2.lease.timeout-disconnect) never reach this release path. Waking a
	// parked file CREATE on the holder's release does not regress
	// smb2.kernel-oplocks.kernel_oplocks7: that test explicitly accepts BOTH the
	// re-open-first (EXCLUSIVE) and parked-create-first (NONE) orderings.
	var remaining []*UnifiedLock
	hadBreaking := false

	for _, lock := range locks {
		if lock.Lease != nil && lock.Lease.LeaseKey == leaseKey {
			if lock.Lease.Breaking {
				hadBreaking = true
			}
			if lm.lockStore != nil {
				store := lm.lockStore
				lockID := lock.ID
				lm.enqueuePersistLocked(handleKey, func() {
					if err := store.DeleteLock(ctx, lockID); err != nil && !storeerrors.IsNotFoundError(err) {
						// In-memory removal proceeds regardless: the persisted
						// record will be reaped by the next client-disconnect or
						// file-deletion sweep. Surface the error so observability
						// catches a misbehaving store rather than the lease leak
						// going silent.
						logger.Error("ReleaseLeaseForHandle: persistent DeleteLock failed",
							"handleKey", handleKey,
							"lockID", lockID,
							"leaseKey", fmt.Sprintf("%x", leaseKey),
							"error", err)
						*deleteErrs = append(*deleteErrs, err)
					}
				})
			}
			continue
		}
		remaining = append(remaining, lock)
	}

	if len(remaining) == 0 {
		delete(lm.unifiedLocks, handleKey)
	} else {
		lm.unifiedLocks[handleKey] = remaining
	}
	lm.reindexHandleLocked(handleKey, locks)

	if hadBreaking {
		lm.signalBreakWaitLocked(handleKey)
	}
}

// leaseRecordsOnHandleLocked returns the records for leaseKey on handleKey.
//
// (handleKey, leaseKey) is the identity of a lease record within one share's
// manager: it is what resolveSameKeyLeaseLocked matches a request against when
// it decides to reuse a record rather than create one. The lease key alone is
// not that identity — two clients may present the same 16-byte value on
// different files of one share, which the cross-file uniqueness rule permits
// because it is scoped per (client, file).
//
// Must be called with lm.mu held (read or write).
func (lm *Manager) leaseRecordsOnHandleLocked(handleKey string, leaseKey [16]byte) []*UnifiedLock {
	var out []*UnifiedLock
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.LeaseKey == leaseKey {
			out = append(out, l)
		}
	}
	return out
}

// GetLeaseState returns the current state and epoch of the lease on handleKey.
func (lm *Manager) getLeaseStateImpl(_ context.Context, handleKey string, leaseKey [16]byte) (state uint32, epoch uint16, found bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	records := lm.leaseRecordsOnHandleLocked(handleKey, leaseKey)
	if len(records) == 0 {
		return 0, 0, false
	}

	return records[0].Lease.LeaseState, records[0].Lease.Epoch, true
}

// HasLeaseOnHandle reports whether a lease record with this key already exists
// on this file. The predicate matches the one resolveSameKeyLeaseLocked uses to
// decide whether a request reuses an existing record or falls through to
// createAndGrantLease, so a false answer means the next grant on this file with
// this key creates a record.
func (lm *Manager) HasLeaseOnHandle(handleKey string, leaseKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.LeaseKey == leaseKey {
			return true
		}
	}
	return false
}

func (lm *Manager) IsTraditionalOplockForKey(handleKey string, leaseKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	records := lm.leaseRecordsOnHandleLocked(handleKey, leaseKey)
	return len(records) > 0 && records[0].Lease.IsTraditionalOplock
}

// ============================================================================
// Lease Operations (implementations in leases.go and reclaim.go)
// ============================================================================

// RequestLease requests a new or upgraded lease on a file or directory.
func (lm *Manager) RequestLease(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
	parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
	requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error) {
	return lm.requestLeaseImpl(ctx, leaseRequest{
		fileHandle:            fileHandle,
		leaseKey:              leaseKey,
		parentLeaseKey:        parentLeaseKey,
		ownerID:               ownerID,
		clientID:              clientID,
		shareName:             shareName,
		state:                 requestedState,
		isDirectory:           isDirectory,
		isTraditionalOplock:   false,
		suppressConflictBreak: false,
	})
}

// RequestLeaseAsOplock is the traditional-oplock variant of RequestLease.
// The SMB adapter calls this when a CREATE arrives with a non-Lease
// OplockLevel (LEVEL_II / Exclusive / Batch); the new record is tagged
// `IsTraditionalOplock=true` so subsequent grants observe the cross-tier
// rules described in `bestGrantableState`. All other parameters and
// semantics match `RequestLease`.
//
// Reference: MS-SMB2 §3.3.5.9 / Samba `source3/smbd/open.c::grant_fsp_oplock_type`.
func (lm *Manager) RequestLeaseAsOplock(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
	parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
	requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error) {
	return lm.requestLeaseImpl(ctx, leaseRequest{
		fileHandle:            fileHandle,
		leaseKey:              leaseKey,
		parentLeaseKey:        parentLeaseKey,
		ownerID:               ownerID,
		clientID:              clientID,
		shareName:             shareName,
		state:                 requestedState,
		isDirectory:           isDirectory,
		isTraditionalOplock:   true,
		suppressConflictBreak: false,
	})
}

// RequestLeaseStatOpen is the stat-open variant of RequestLease. The SMB
// adapter calls this when a CREATE's DesiredAccess is stat-open-only
// (FILE_READ_ATTRIBUTES / FILE_WRITE_ATTRIBUTES / READ_CONTROL / SYNCHRONIZE) and
// the disposition is non-destructive. The grant proceeds normally except that
// a cross-key conflict with an existing holder MUST NOT dispatch a break: a
// stat-opener caches attributes alongside existing holders without forcing
// them to drop their caches. It instead receives the best state it can coexist
// with (`bestGrantableState`).
//
// Reference: MS-SMB2 §3.3.5.9.8 / Samba `is_lease_stat_open`
// (source3/smbd/open.c). Closes the timing-dependent spurious break that
// smb2.lease.statopen4 CHECK_NO_BREAK observes (#751).
func (lm *Manager) RequestLeaseStatOpen(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
	parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
	requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error) {
	return lm.requestLeaseImpl(ctx, leaseRequest{
		fileHandle:            fileHandle,
		leaseKey:              leaseKey,
		parentLeaseKey:        parentLeaseKey,
		ownerID:               ownerID,
		clientID:              clientID,
		shareName:             shareName,
		state:                 requestedState,
		isDirectory:           isDirectory,
		isTraditionalOplock:   false,
		suppressConflictBreak: true,
	})
}

// AcknowledgeLeaseBreak processes a client's lease break acknowledgment.
func (lm *Manager) AcknowledgeLeaseBreak(ctx context.Context, leaseKey [16]byte,
	acknowledgedState uint32, epoch uint16) error {
	return lm.acknowledgeLeaseBreakImpl(ctx, leaseKey, acknowledgedState, epoch)
}

// ReleaseLease releases all lease state for the given lease key.
func (lm *Manager) ReleaseLease(ctx context.Context, leaseKey [16]byte) error {
	return lm.releaseLeaseImpl(ctx, leaseKey)
}

// ReclaimLease reclaims a lease during grace period. clientID must match the
// lease owner recorded on the persisted record (lease-stealing guard); pass ""
// to skip the owner check. This is the LockManager-interface entrypoint used by
// SMB, which has no RPCSEC_GSS principal to verify.
func (lm *Manager) ReclaimLease(ctx context.Context, leaseKey [16]byte,
	requestedState uint32, isDirectory bool, clientID string) (*UnifiedLock, error) {
	return lm.reclaimLeaseImpl(ctx, leaseKey, requestedState, isDirectory, clientID, "")
}

// ReclaimLeaseWithPrincipal is the NFSv4 reclaim path that additionally
// verifies the incoming RPCSEC_GSS / AUTH_SYS principal matches the one
// recorded in the V4ClientRecoveryRecord for clientID. The principal check
// runs only when a ClientRecoveryStore is wired (SetClientRecoveryStore) and
// incomingPrincipal is non-empty; an empty principal skips the check. Returns
// a lock-not-found error when the clientID or principal does not match.
func (lm *Manager) ReclaimLeaseWithPrincipal(ctx context.Context, leaseKey [16]byte,
	requestedState uint32, isDirectory bool, clientID string, incomingPrincipal string) (*UnifiedLock, error) {
	return lm.reclaimLeaseImpl(ctx, leaseKey, requestedState, isDirectory, clientID, incomingPrincipal)
}

// ReleaseLeaseForHandle removes lease records matching leaseKey from a
// single handleKey bucket only. See releaseLeaseForHandleImpl for details.
func (lm *Manager) ReleaseLeaseForHandle(ctx context.Context, handleKey string, leaseKey [16]byte) error {
	return lm.releaseLeaseForHandleImpl(ctx, handleKey, leaseKey)
}

// GetLeaseState returns the current state and epoch of the lease on handleKey.
func (lm *Manager) GetLeaseState(ctx context.Context, handleKey string, leaseKey [16]byte) (state uint32, epoch uint16, found bool) {
	return lm.getLeaseStateImpl(ctx, handleKey, leaseKey)
}

// SetLeaseEpoch sets the epoch on the lease (handleKey, leaseKey) holds.
// Per MS-SMB2 3.3.5.9: For V2 leases, the server should track the client's
// epoch from the RqLs create context. SetLeaseEpoch is called after RequestLease
// to initialize the epoch to the client's requested value.
// Returns false if no lease was found on that file with the given key.
func (lm *Manager) SetLeaseEpoch(handleKey string, leaseKey [16]byte, epoch uint16) bool {
	lm.mu.Lock()
	defer lm.unlock()

	// Update every record of THIS lease, not just the first found. Reopens and
	// reclaims can leave more than one record for a key on one file, so scoping
	// to the first match can miss the one RequestLease just granted — leaving
	// it at Epoch=1 (createAndGrantLease default) while the response to the
	// client carries the higher requested epoch. Subsequent break notifications
	// then dispatch with Epoch=2 instead of requestedEpoch+2, regressing
	// smbtorture V2 tests (break_twice, breaking*, v2_breaking3).
	//
	// Two passes so all of this lease's records converge to the SAME epoch. A
	// per-record `if epoch >= lock.Lease.Epoch` guard lets siblings that start
	// at different epochs diverge (e.g. one at 5 stays 5 while another at 1
	// moves to 4), and a read of the lease then answers from whichever the
	// slice surfaces first. Compute the max of the requested epoch and every
	// record's current epoch, then assign that single max to all of them so the
	// lease has one epoch.
	//
	// The convergence stops at this file. A lease key is not an identity: the
	// cross-file uniqueness rule is per (client, file), so another client may
	// hold the same 16-byte value on a different file of this share. Folding
	// that client's records into the max hands it an epoch no grant of its own
	// produced, and the gap is what a client acts on — per MS-SMB2 §3.2.5.19.2
	// a NewEpoch more than 1 past the one it last saw forces a cache purge, and
	// one more than 32767 past it makes the client discard the break's new
	// lease state outright.
	records := lm.leaseRecordsOnHandleLocked(handleKey, leaseKey)
	target := epoch
	for _, lock := range records {
		if lock.Lease.Epoch > target {
			target = lock.Lease.Epoch
		}
	}
	for _, lock := range records {
		lock.Lease.Epoch = target
		// Persist the new epoch like every other lease state-change path.
		// Without this, RestoreLocks rebuilds the lease at the stale grant-time
		// epoch after a restart and a later break dispatches a NewEpoch below
		// the client's last-seen value, violating MS-SMB2 §2.2.14.2.11 /
		// §3.3.4.7 epoch monotonicity.
		lm.persistUnifiedLockLocked(lock)
	}
	return len(records) > 0
}
