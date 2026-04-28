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
// multichannel Phase 2 work (#361) where ClientGuid threading is needed
// for cross-channel break fan-out anyway.
//
// Must be called with lm.mu held (read or write).
func (lm *Manager) hasLeaseKeyOnOtherFile(leaseKey [16]byte, excludeHandleKey, clientID string) bool {
	for handleKey, locks := range lm.unifiedLocks {
		if handleKey == excludeHandleKey {
			continue
		}
		for _, lock := range locks {
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
// #361 Phase 2). Called BEFORE lm.mu.Lock() — same pattern as the existing
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
	return lm.requestLeaseImplWithMode(ctx, fileHandle, leaseKey, parentLeaseKey,
		ownerID, clientID, shareName, requestedState, isDirectory, false)
}

// requestLeaseImplWithMode is the underlying lease-grant implementation with
// the additional `isTraditionalOplock` flag distinguishing real SMB2.1+ leases
// from synthetic-key records modeling traditional oplocks (LEVEL_II/Exclusive/
// Batch). The flag is consumed by `bestGrantableState` to apply the MS-SMB2
// §3.3.5.9 cross-tier rules:
//
//   - traditional-oplock requestor + any other-key holder with H bit → NONE
//     (Samba `state.got_handle_lease` in `delay_for_oplock_fn`)
//   - real-lease requestor + any other-key traditional-oplock holder → strip H
//     (Samba `state.got_oplock`)
//
// And it propagates the flag onto the new record via `createAndGrantLease`
// so subsequent grants can detect it.
func (lm *Manager) requestLeaseImplWithMode(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
	parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
	requestedState uint32, isDirectory bool, isTraditionalOplock bool) (grantedState uint32, epoch uint16, err error) {

	// Coerce no-Read caching combinations (W=0x04, H=0x02, HW=0x06) to
	// LeaseState=None and grant successfully. Per Samba
	// source3/smbd/open.c::delay_for_oplock the rule "any W or H without R
	// → SMB2_LEASE_NONE" applies universally (files and directories alike)
	// and is enforced before any conflict resolution. The smbtorture
	// smb2.lease.request matrix asserts NT_STATUS_OK with granted state=""
	// for H, W, and HW.
	//
	// Gate explicitly on the R/W/H bits (mask off reserved bits first) so
	// requests like 0x09 (R + reserved bit 0x08) are still treated as
	// R-bearing and pass through to bestGrantableState rather than being
	// coerced to None — matching Samba's behavior of ignoring reserved
	// bits while still honoring Read.
	//
	// Returning here (instead of falling through to bestGrantableState) is
	// deliberate: that helper's degradation chain ends at LeaseStateRead,
	// which would wrongly grant R for a W/H/HW request whose original
	// intent was a non-Read caching right.
	const knownLeaseBits = LeaseStateRead | LeaseStateWrite | LeaseStateHandle
	maskedKnown := requestedState & knownLeaseBits
	if maskedKnown != LeaseStateNone && maskedKnown&LeaseStateRead == 0 {
		logger.Debug("RequestLease: no-Read caching combination, coercing to None",
			"state", LeaseStateToString(requestedState),
			"fileHandle", string(fileHandle),
			"isDirectory", isDirectory)
		return LeaseStateNone, 0, nil
	}

	handleKey := string(fileHandle)

	// LeaseStateNone probe: clients (and smbtorture breaking4 / upgrade2)
	// issue empty-state requests to query the current lease without taking
	// new caching rights. Per Samba upgrade2 the response is the current
	// state of any same-key lease (R returns R, RH returns RH, …) — *not*
	// always None. A None probe with no same-key lease still returns None
	// trivially, short-circuited here so we don't enter the cross-key break
	// dispatch path with requestedState=None.
	if requestedState == LeaseStateNone {
		lm.mu.Lock()
		for _, lock := range lm.unifiedLocks[handleKey] {
			if lock.Lease == nil || lock.Lease.LeaseKey != leaseKey {
				continue
			}
			currentState := lock.Lease.LeaseState
			epoch := lock.Lease.Epoch
			breaking := lock.Lease.Breaking
			lm.mu.Unlock()
			if breaking {
				logger.Debug("RequestLease: None-probe on breaking same-key lease, surfacing break-in-progress",
					"fileHandle", handleKey,
					"currentState", LeaseStateToString(currentState),
					"epoch", epoch)
				return currentState, epoch, ErrLeaseBreakInProgress
			}
			return currentState, epoch, nil
		}
		lm.mu.Unlock()
		return LeaseStateNone, 0, nil
	}

	// Check recently-broken cache for directories
	if isDirectory && lm.recentlyBroken != nil && lm.recentlyBroken.IsRecentlyBroken(handleKey) {
		logger.Debug("RequestLease: directory recently broken, denying",
			"fileHandle", handleKey)
		return LeaseStateNone, 0, nil
	}

	// Per MS-SMB2 §3.3.5.9.8: if any byte-range lock is outstanding on the
	// file, the server MUST grant leaseState = NONE. Check both the
	// persisted lockStore (NLM-side) and the in-memory lm.locks map
	// (SMB2 LOCK callers; not yet pushed through lockStore).
	if lm.hasByteRangeLockConflictForLease(ctx, handleKey, requestedState, clientID) {
		logger.Debug("RequestLease: byte-range lock conflict, denying lease",
			"fileHandle", handleKey,
			"requestedState", LeaseStateToString(requestedState))
		return LeaseStateNone, 0, nil
	}

	// Cross-file lease-key uniqueness — persisted backstop for post-restart
	// state. The in-memory check inside lm.mu below catches the steady-state
	// case; this pre-check covers the window after a restart but before the
	// owning client has reclaimed the lease into memory.
	if lm.hasPersistedLeaseKeyOnOtherFile(ctx, leaseKey, handleKey, clientID) {
		logger.Debug("RequestLease: lease key already bound to another file (persisted record)",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"fileHandle", handleKey,
			"clientID", clientID,
			"requestedState", LeaseStateToString(requestedState))
		return LeaseStateNone, 0, ErrLeaseKeyInUse
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
	// Skipped on None probes: zero-state requests are pure state queries that
	// cannot acquire caching rights and are not subject to lease_match.
	// Same-file reopen (h1a/h1b in smbtorture breaking2) lands in the same
	// handleKey bucket and is allowed; ack-to-None records persisted under
	// the original handleKey still count as bindings here (handle-bound
	// lifetime, PR #452).
	if requestedState != LeaseStateNone && lm.hasLeaseKeyOnOtherFile(leaseKey, handleKey, clientID) {
		lm.mu.Unlock()
		logger.Debug("RequestLease: lease key already bound to another file for this client",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"fileHandle", handleKey,
			"clientID", clientID,
			"requestedState", LeaseStateToString(requestedState))
		return LeaseStateNone, 0, ErrLeaseKeyInUse
	}

	locks := lm.unifiedLocks[handleKey]

	// Check for delegation conflicts before granting a lease
	for _, lock := range locks {
		if lock.Delegation != nil {
			// Create a temporary OpLock to check coexistence
			tempLease := &OpLock{LeaseState: requestedState}
			if DelegationConflictsWithLease(lock.Delegation, tempLease) {
				lm.mu.Unlock()
				logger.Debug("RequestLease: delegation conflict, denying lease",
					"fileHandle", handleKey,
					"delegationType", lock.Delegation.DelegType.String(),
					"requestedState", LeaseStateToString(requestedState))
				return LeaseStateNone, 0, fmt.Errorf("lease denied: conflicts with %s delegation on file",
					lock.Delegation.DelegType.String())
			}
		}
	}

	// Search for existing lease with same key
	for i, lock := range locks {
		if lock.Lease == nil || lock.Lease.LeaseKey != leaseKey {
			continue
		}

		// Same-key found
		currentState := lock.Lease.LeaseState

		// Per MS-SMB2 3.3.5.9.8: If the lease is in Breaking state, do NOT
		// modify it. Return the current LeaseState and signal break-in-progress
		// to the caller so it can set SMB2_LEASE_FLAG_BREAK_IN_PROGRESS (0x02).
		if lock.Lease.Breaking {
			epoch := lock.Lease.Epoch
			lm.mu.Unlock()
			logger.Debug("RequestLease: same-key lease is breaking, returning current state with break-in-progress",
				"fileHandle", handleKey,
				"currentState", LeaseStateToString(currentState),
				"epoch", epoch)
			return currentState, epoch, ErrLeaseBreakInProgress
		}

		// Same state requested - return current (no-op)
		if currentState == requestedState {
			lm.mu.Unlock()
			return currentState, lock.Lease.Epoch, nil
		}

		// Check if this is a valid upgrade AND can coexist with any other
		// leases on the same file (Samba upgrade3 contended-case rule):
		// the upgrade applies iff the requested state is a strict superset
		// of the current AND does not conflict with any other-key holder.
		// If the upgrade would conflict, leave the current state unchanged
		// — the rule explicitly forbids breaking other holders to satisfy
		// a same-key upgrade.
		canUpgrade := isValidUpgrade(currentState, requestedState)
		if canUpgrade {
			requestedLease := &OpLock{LeaseKey: leaseKey, LeaseState: requestedState}
			for _, other := range locks {
				if other.Lease == nil || other.Lease.LeaseKey == leaseKey {
					continue
				}
				if OpLocksConflict(other.Lease, requestedLease) {
					canUpgrade = false
					logger.Debug("RequestLease: upgrade blocked by other-key holder",
						"fileHandle", handleKey,
						"current", LeaseStateToString(currentState),
						"requested", LeaseStateToString(requestedState),
						"otherState", LeaseStateToString(other.Lease.LeaseState))
					break
				}
			}
		}
		if canUpgrade {
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
				if err := lm.lockStore.PutLock(ctx, pl); err != nil {
					logger.Error("RequestLease: failed to persist lease upgrade", "fileHandle", handleKey, "error", err)
				}
			}

			epoch := locks[i].Lease.Epoch
			lm.mu.Unlock()
			return requestedState, epoch, nil
		}

		// Non-superset request (downgrade or sidegrade): per Samba upgrade2,
		// same-key RequestLease changes the lease iff requested is a strict
		// superset of current. Otherwise the existing state is returned
		// unchanged (e.g. RH + request RW → return RH; R + request "" → R).
		// Returning None here would silently drop the holder's caching
		// rights and break the smbtorture upgrade / upgrade2 / upgrade3
		// matrix.
		epoch := locks[i].Lease.Epoch
		lm.mu.Unlock()
		logger.Debug("RequestLease: same-key non-superset request, returning existing state",
			"fileHandle", handleKey,
			"current", LeaseStateToString(currentState),
			"requested", LeaseStateToString(requestedState))
		return currentState, epoch, nil
	}

	// No existing lease with same key. Check for cross-key conflicts.
	// Per MS-SMB2 3.3.5.9: break conflicting leases, then grant the best
	// available state (may be less than requested).
	var breakDispatched bool
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
			// CREATE-time SMB share-mode/disposition checks run before
			// RqLs processing, so the cross-key conflicts that reach this
			// path are non-violating, non-destructive lease conflicts —
			// strip Write, keep Read + Handle. RWH→RH, RW→R.
			breakTo := ComputeLeaseBreakTo(lock.Lease.LeaseState, BreakReasonDefault)

			// If existing lease has no Write bit, the break is a no-op
			// (e.g., existing=R, breakTo=R). In this case, don't dispatch
			// a break -- just proceed to downgrade the new request.
			if breakTo == lock.Lease.LeaseState {
				logger.Debug("RequestLease: cross-key conflict but existing has no Write, skipping break",
					"fileHandle", handleKey,
					"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
					"existingState", LeaseStateToString(lock.Lease.LeaseState),
					"requestedState", LeaseStateToString(requestedState))
				break
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
			// exactly ONE RHW→RH break, not two.
			//
			// Falls through to bestGrantableState WITHOUT setting
			// breakDispatched: we never released lm.mu, so the post-break
			// re-Lock below must be skipped to avoid self-deadlock.
			if lock.Lease.Breaking {
				lock.Lease.BreakingToRequired &= breakTo
				if lm.lockStore != nil {
					pl := ToPersistedLock(lock, 0)
					if err := lm.lockStore.PutLock(ctx, pl); err != nil {
						logger.Error("RequestLease: failed to persist tightened break target",
							"fileHandle", handleKey, "error", err)
					}
				}
				logger.Debug("RequestLease: cross-key conflict on already-breaking lease, suppressed duplicate break",
					"fileHandle", handleKey,
					"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
					"requestedKey", fmt.Sprintf("%x", leaseKey),
					"existingBreakingTo", LeaseStateToString(lock.Lease.BreakingToRequired),
					"requestedState", LeaseStateToString(requestedState))
				break
			}

			logger.Debug("RequestLease: cross-key conflict, initiating break",
				"fileHandle", handleKey,
				"existingKey", fmt.Sprintf("%x", lock.Lease.LeaseKey),
				"requestedKey", fmt.Sprintf("%x", leaseKey),
				"existingState", LeaseStateToString(lock.Lease.LeaseState),
				"requestedState", LeaseStateToString(requestedState),
				"breakToState", LeaseStateToString(breakTo))

			// Mark lease as breaking before dispatching callbacks
			lock.Lease.Breaking = true
			lock.Lease.BreakToState = breakTo
			lock.Lease.BreakingToRequired = breakTo
			lock.Lease.BreakStarted = time.Now()
			advanceEpoch(lock.Lease)

			// Persist the breaking state
			if lm.lockStore != nil {
				pl := ToPersistedLock(lock, 0)
				if err := lm.lockStore.PutLock(ctx, pl); err != nil {
					logger.Error("RequestLease: failed to persist breaking state", "fileHandle", handleKey, "error", err)
				}
			}

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
			lm.mu.Unlock()
			lm.dispatchOpLockBreak(handleKey, lockSnapshot, breakTo)

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
			// so bestGrantableState below computes the correct downgraded
			// grant for the new opener without needing the ack to land
			// first. The same async-dispatch pattern is used by
			// internal/adapter/smb/lease/manager.go BreakHandleLeasesOnOpenAsync,
			// whose comment explicitly documents this deadlock.
			breakDispatched = true
			break
		}
	}

	// After any break (or no-op skip), find the best grantable state.
	// Per MS-SMB2 3.3.5.9: the server MUST grant the best available oplock
	// level. Try the full requested state first, then progressively lower
	// states: strip Write, then strip Handle, then Read only, then None.
	if breakDispatched {
		lm.mu.Lock()
		locks = lm.unifiedLocks[handleKey]
	}
	// lm.mu is held here (either from initial Lock or re-Lock after break)

	grantState := bestGrantableState(locks, leaseKey, requestedState, isDirectory, isTraditionalOplock)
	if grantState == LeaseStateNone {
		lm.mu.Unlock()
		logger.Debug("RequestLease: no compatible state after conflict resolution",
			"fileHandle", handleKey,
			"requestedState", LeaseStateToString(requestedState))
		return LeaseStateNone, 0, nil
	}

	granted, epoch := lm.createAndGrantLease(ctx, handleKey, fileHandle,
		leaseKey, parentLeaseKey, ownerID, clientID, shareName,
		grantState, isDirectory, isTraditionalOplock)
	lm.mu.Unlock()

	logger.Debug("RequestLease: granted lease",
		"fileHandle", handleKey,
		"requested", LeaseStateToString(requestedState),
		"granted", LeaseStateToString(granted),
		"isDirectory", isDirectory,
		"downgraded", grantState != requestedState,
		"epoch", epoch)

	return granted, epoch, nil
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
func (lm *Manager) createAndGrantLease(
	ctx context.Context,
	handleKey string,
	fileHandle FileHandle,
	leaseKey, parentLeaseKey [16]byte,
	ownerID, clientID, shareName string,
	requestedState uint32,
	isDirectory bool,
	isTraditionalOplock bool,
) (uint32, uint16) {
	newLock := &UnifiedLock{
		ID: uuid.New().String(),
		Owner: LockOwner{
			OwnerID:   ownerID,
			ClientID:  clientID,
			ShareName: shareName,
		},
		FileHandle: fileHandle,
		Offset:     0,
		Length:     0,
		Type:       lockTypeForLeaseState(requestedState),
		AcquiredAt: time.Now(),
		Lease: &OpLock{
			LeaseKey:            leaseKey,
			LeaseState:          requestedState,
			ParentLeaseKey:      parentLeaseKey,
			IsDirectory:         isDirectory,
			IsTraditionalOplock: isTraditionalOplock,
			Epoch:               1, // New leases start at epoch 1
		},
	}

	lm.unifiedLocks[handleKey] = append(lm.unifiedLocks[handleKey], newLock)

	if lm.lockStore != nil {
		pl := ToPersistedLock(newLock, 0)
		if err := lm.lockStore.PutLock(ctx, pl); err != nil {
			logger.Error("RequestLease: failed to persist new lease", "fileHandle", handleKey, "error", err)
		}
	}

	return requestedState, 1
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
func (lm *Manager) acknowledgeLeaseBreakImpl(ctx context.Context, leaseKey [16]byte,
	acknowledgedState uint32, epoch uint16) error {

	lm.mu.Lock()
	defer lm.mu.Unlock()

	handleKey, lock, _ := lm.findLeaseByKey(leaseKey)
	if lock == nil {
		return ErrLeaseAckNotFound
	}

	if !lock.Lease.Breaking {
		return ErrLeaseAckNotBreaking
	}

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

	// Ack-to-None: keep the record alive at LeaseState=None until the holding
	// handle CLOSEs (ReleaseLeaseForHandle removes it). This mirrors Samba
	// behavior and lets the wrapper distinguish a duplicate ack on an
	// already-released lease (record present, Breaking=false → ErrLeaseAck-
	// NotBreaking → STATUS_UNSUCCESSFUL, smbtorture breaking2/breaking5)
	// from a CLOSE-beat-ack race (record gone → ErrLeaseAckNotFound →
	// silent success, WPTS BVT_DirectoryLeasing_ReadWriteHandleCaching).
	if acknowledgedState == LeaseStateNone {
		lock.Lease.LeaseState = LeaseStateNone
		lock.Lease.Breaking = false
		lock.Lease.BreakToState = 0
		lock.Lease.BreakingToRequired = LeaseStateNone
		lock.Lease.BreakStarted = time.Time{}
		lock.Type = lockTypeForLeaseState(LeaseStateNone)

		if lm.lockStore != nil {
			pl := ToPersistedLock(lock, 0)
			_ = lm.lockStore.PutLock(ctx, pl)
		}

		logger.Debug("AcknowledgeLeaseBreak: lease released to None (record kept until CLOSE)",
			"leaseKey", fmt.Sprintf("%x", leaseKey))
		lm.signalBreakWaitLocked(handleKey)
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
	// (BreakingToRequired) is stricter than what the client just
	// acknowledged, dispatch the next stage. Mirrors Samba
	// `downgrade_lease` (source3/smbd/smb2_oplock.c lines 569-586): if the
	// acked state still has W or H, the next target keeps R as an
	// intermediate; otherwise drop straight to the cumulative required.
	//
	// This produces the smbtorture breaking3 / v2_breaking3 wire shape:
	//   ack RWH→RH  ⇒ next target = R  ⇒ wire: RH→R
	//   ack RH→R    ⇒ next target = 0  ⇒ wire: R→""
	if acknowledgedState != LeaseStateNone &&
		acknowledgedState&^lock.Lease.BreakingToRequired != 0 {
		nextTarget := nextProgressiveBreakTarget(acknowledgedState, lock.Lease.BreakingToRequired)
		snapshot := lm.applyBreakStageLocked(lock, nextTarget)

		// Persist the next-stage state BEFORE releasing lm.mu so the durable
		// store reflects Breaking=true / BreakToState=nextTarget. Otherwise a
		// crash between the ACK-clear (Breaking=false written above) and the
		// next-stage-set would lose the second progressive stage on restart,
		// leaving parked CREATEs to wait until the scanner timeout.
		if lm.lockStore != nil {
			pl := ToPersistedLock(lock, 0)
			_ = lm.lockStore.PutLock(ctx, pl)
		}

		logger.Debug("AcknowledgeLeaseBreak: progressive break next stage",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"ackedState", LeaseStateToString(acknowledgedState),
			"required", LeaseStateToString(lock.Lease.BreakingToRequired),
			"nextTarget", LeaseStateToString(nextTarget),
			"epoch", lock.Lease.Epoch)

		// Release lm.mu before dispatching to avoid deadlock with the
		// SMB transport callback (mirrors breakOpLocks pattern). Wrap in a
		// closure with a deferred re-Lock so the surrounding function's
		// `defer lm.mu.Unlock()` always sees the mutex held — without this,
		// a panic inside dispatchOpLockBreak would unwind through an
		// unlocked mutex and the outer defer would double-unlock.
		func() {
			lm.mu.Unlock()
			defer lm.mu.Lock()
			lm.dispatchOpLockBreak(handleKey, snapshot, nextTarget)
		}()

		// Re-validate: a concurrent CLOSE / release / timeout could have
		// removed the lease during the dispatch window. The `lock` pointer
		// may now reference an orphaned UnifiedLock — read fields off the
		// re-found record (or signal waiters and return when gone).
		_, currentLock, _ := lm.findLeaseByKey(leaseKey)
		if currentLock == nil {
			lm.signalBreakWaitLocked(handleKey)
			return nil
		}

		// Signal waiters only when the break has fully drained: either the
		// inline fire-and-forget path already updated LeaseState to nextTarget
		// (no further ACK will arrive), or a concurrent path removed the lease.
		// Otherwise the break is still in progress and waiters must keep waiting.
		if nextTarget == currentLock.Lease.LeaseState {
			lm.signalBreakWaitLocked(handleKey)
		}
		return nil
	}

	// Reached BreakingToRequired (or full release): mirror invariant
	// "BreakingToRequired == LeaseState when not Breaking" and signal.
	lock.Lease.BreakingToRequired = acknowledgedState

	// Persist updated state
	if lm.lockStore != nil {
		pl := ToPersistedLock(lock, 0)
		_ = lm.lockStore.PutLock(ctx, pl)
	}

	logger.Debug("AcknowledgeLeaseBreak: break acknowledged",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"newState", LeaseStateToString(acknowledgedState),
		"epoch", lock.Lease.Epoch)

	lm.signalBreakWaitLocked(handleKey)
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
	lm.mu.Lock()
	defer lm.mu.Unlock()

	locks := lm.unifiedLocks[handleKey]
	if len(locks) == 0 {
		return nil
	}

	var remaining []*UnifiedLock
	var deleteErrs []error
	for _, lock := range locks {
		if lock.Lease != nil && lock.Lease.LeaseKey == leaseKey {
			if lm.lockStore != nil {
				if err := lm.lockStore.DeleteLock(ctx, lock.ID); err != nil && !storeerrors.IsNotFoundError(err) {
					// In-memory removal proceeds regardless: the persisted
					// record will be reaped by the next client-disconnect or
					// file-deletion sweep. Surface the error so observability
					// catches a misbehaving store rather than the lease leak
					// going silent (round-3 follow-up).
					logger.Error("ReleaseLeaseForHandle: persistent DeleteLock failed",
						"handleKey", handleKey,
						"lockID", lock.ID,
						"leaseKey", fmt.Sprintf("%x", leaseKey),
						"error", err)
					deleteErrs = append(deleteErrs, err)
				}
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

	if len(deleteErrs) > 0 {
		return fmt.Errorf("release lease for handle %q: %w", handleKey, errors.Join(deleteErrs...))
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
