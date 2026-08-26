package lock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Unified Caching Break Operations
// ============================================================================

// CheckAndBreakCachingForWrite breaks all leases AND all delegations.
// Used for cross-protocol writes (e.g., NFS write breaking SMB leases).
func (lm *Manager) CheckAndBreakCachingForWrite(handleKey string, excludeOwner *LockOwner) error {
	if err := lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, BreakReasonUnspecified, func(lease *OpLock) bool {
		return lease.HasRead() || lease.HasWrite()
	}); err != nil {
		return err
	}
	lm.breakDelegations(handleKey, excludeOwner, func(deleg *Delegation) bool {
		return true
	})

	return nil
}

// CheckAndBreakCachingForRead breaks write leases (to Read) and write delegations.
// Read delegations and read leases coexist with reads.
func (lm *Manager) CheckAndBreakCachingForRead(handleKey string, excludeOwner *LockOwner) error {
	if err := lm.breakOpLocks(handleKey, excludeOwner, LeaseStateRead, BreakReasonUnspecified, func(lease *OpLock) bool {
		return lease.HasWrite()
	}); err != nil {
		return err
	}
	lm.breakDelegations(handleKey, excludeOwner, func(deleg *Delegation) bool {
		return deleg.DelegType == DelegTypeWrite
	})

	return nil
}

// CheckAndBreakLeasesForSMBOpen breaks conflicting leases for an SMB CREATE.
//
// Per MS-SMB2 3.3.5.9 / MS-FSA 2.1.4.12 ("Algorithm to Check for an Oplock Break"): When a new SMB open arrives,
// existing leases that hold Write caching must be broken. Unlike cross-protocol
// breaks (CheckAndBreakCachingForWrite), the break strips only the Write bit,
// preserving Read and Handle caching. This allows clients to continue read
// and handle caching while flushing dirty data.
//
//   - RWH -> RH (strip W, keep Read + Handle)
//   - RW  -> R  (strip W, keep Read)
//   - R   -> not broken (no Write to strip)
//   - RH  -> not broken (no Write to strip)
func (lm *Manager) CheckAndBreakLeasesForSMBOpen(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, BreakToStripWrite, BreakReasonUnspecified, func(lease *OpLock) bool {
		return lease.HasWrite()
	})
}

// BreakLeasesForByteRangeLock breaks every other-key lease that holds Read
// caching to None when an SMB byte-range lock is acquired.
//
// Per MS-SMB2 3.3.5.14 (Receiving an SMB2 LOCK Request) and Samba
// `source3/smbd/smb2_oplock.c::contend_level2_oplocks_begin_default`
// (lines 1391-1467) + `do_break_lease_to_none` (lines 1155-1206):
// when a byte-range lock is granted on an open, every other lease holder
// whose state has Read caching must be broken to None — Read caching
// becomes invalid because the locking client may now write data the
// remote cache can no longer observe.
//
// The locker's own lease (typically same lease key, possibly a same-key
// secondary handle) is excluded via excludeOwner.ExcludeLeaseKey, mirroring
// Samba's `smb2_lease_equal` no-self-break check.
//
// Leases without Read caching (None, or Write-only, which the protocol
// disallows in practice) are skipped: there is no read cache to flush.
// The break target is None — full revocation — not "strip W" or "strip H".
func (lm *Manager) BreakLeasesForByteRangeLock(handleKey string, excludeOwner *LockOwner) error {
	// An outstanding NFSv4 delegation is recalled for the same reason: the
	// delegated client handles byte-range locks locally, so while it holds the
	// delegation it can neither see this lock nor have its own locks seen. The
	// delegation's whole-file row also blocks the lock in AddUnifiedLock, so
	// without the recall the lock stays denied until the delegation's lease
	// expires — even after the original holder released.
	lm.breakDelegations(handleKey, excludeOwner, func(*Delegation) bool {
		return true
	})

	return lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, BreakReasonUnspecified, func(lease *OpLock) bool {
		return lease.HasRead()
	})
}

// BreakLeasesOnOpenConflict breaks leases held by other clients when an SMB
// CREATE arrives. Per MS-SMB2 3.3.4.7 and Samba
// `source3/smbd/open.c::delay_for_oplock_fn`. Per-lease target state is
// computed via ComputeLeaseBreakTo(state, reason); a lease is broken only
// when the computed target differs from its current state.
func (lm *Manager) BreakLeasesOnOpenConflict(handleKey string, excludeOwner *LockOwner, reason BreakReason) error {
	lm.PrepareBreakLeasesOnOpenConflict(handleKey, excludeOwner, reason)()
	return nil
}

// PrepareBreakLeasesOnOpenConflict records the same breaks as
// BreakLeasesOnOpenConflict but leaves the wire notifications unsent,
// returning the function that sends them. Splitting the two lets a caller
// order the notification after its own response while the set of leases the
// change breaks is still decided at the moment of the change.
func (lm *Manager) PrepareBreakLeasesOnOpenConflict(handleKey string, excludeOwner *LockOwner, reason BreakReason) func() {
	return lm.markOpLockBreaks(handleKey, excludeOwner, breakSentinelForReason(reason), reason, func(lease *OpLock) bool {
		return ComputeLeaseBreakTo(lease.LeaseState, reason) != lease.LeaseState
	})
}

// BreakReadLeasesForParentDir breaks Read leases on a parent directory when
// a child file is modified (SET_INFO, WRITE, DELETE). Per MS-FSA 2.1.4.12 ("Algorithm to Check for an Oplock Break"):
// changes to directory contents or child metadata invalidate Read caching,
// so clients holding R or RW leases on the directory must be notified.
//
// The break goes to None (full revocation):
//   - R  -> None
//   - RW -> None
func (lm *Manager) BreakReadLeasesForParentDir(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, BreakReasonUnspecified, func(lease *OpLock) bool {
		return lease.IsDirectory && lease.HasRead()
	})
}

// CheckAndBreakCachingForDelete breaks all leases AND all delegations.
func (lm *Manager) CheckAndBreakCachingForDelete(handleKey string, excludeOwner *LockOwner) error {
	if err := lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, BreakReasonUnspecified, func(lease *OpLock) bool {
		return lease.LeaseState != LeaseStateNone
	}); err != nil {
		return err
	}
	lm.breakDelegations(handleKey, excludeOwner, func(deleg *Delegation) bool {
		return true
	})

	return nil
}

// WaitForBreakCompletionExceptKey is WaitForBreakCompletion scoped to ignore
// any breaking lease whose LeaseKey matches exceptKey. Used by the SMB CREATE
// path on a same-key reopen: MS-SMB2 3.3.5.9.8 requires the opener to observe
// Breaking=true on its own key (to emit SMB2_LEASE_FLAG_BREAK_IN_PROGRESS),
// which forceCompleteBreaks would otherwise clear — but any other-key breaks
// still need to drain before the CREATE proceeds (MS-SMB2 3.3.4.7). On
// timeout, own-key is preserved and only other-key leases are force-completed.
func (lm *Manager) WaitForBreakCompletionExceptKey(ctx context.Context, handleKey string, exceptKey [16]byte) error {
	for {
		lm.mu.Lock()
		hasOther := false
		for _, l := range lm.unifiedLocks[handleKey] {
			if l.Lease != nil && l.Lease.Breaking && l.Lease.LeaseKey != exceptKey {
				hasOther = true
				break
			}
			if l.Delegation != nil && l.Delegation.Breaking {
				hasOther = true
				break
			}
		}
		if !hasOther {
			lm.unlock()
			return nil
		}

		ch, ok := lm.breakWaitChans[handleKey]
		if !ok {
			ch = make(chan struct{})
			lm.breakWaitChans[handleKey] = ch
		}
		lm.unlock()

		select {
		case <-ctx.Done():
			lm.forceCompleteBreaksExceptKey(handleKey, exceptKey)
			return ctx.Err()
		case <-ch:
			continue
		}
	}
}

// WaitForByteRangeLeaseBreak blocks until every breaking SMB lease on handleKey
// resolves, IGNORING in-flight NFSv4 delegation breaks. It is the wait used by
// the byte-range-lock acquisition path (NFSv4 LOCK / NLM), which runs while the
// caller holds the NFSv4 StateManager mutex.
//
// It deliberately does NOT wait for delegation breaks, unlike
// WaitForBreakCompletionExceptKey. A delegation break is resolved by
// DELEGRETURN, whose handler must take the StateManager mutex; waiting for it
// here while that same mutex is held would be a circular wait (only force-
// broken after the bounded timeout, then re-stalled on the next LOCK, since
// forceCompleteBreaksExceptKey only force-completes leases). Not waiting stays
// correct: BreakLeasesForByteRangeLock has already dispatched the recall by the
// time this runs, and the delegation's whole-file row keeps AddUnifiedLock
// denying the acquire until the client returns it, so the lock is retried
// rather than granted alongside an unrecalled delegation. On timeout the
// breaking leases are force-downgraded to None (Samba
// lease_timeout parity) and ctx.Err() is returned.
func (lm *Manager) WaitForByteRangeLeaseBreak(ctx context.Context, handleKey string) error {
	for {
		lm.mu.Lock()
		hasBreakingLease := false
		for _, l := range lm.unifiedLocks[handleKey] {
			if l.Lease != nil && l.Lease.Breaking {
				hasBreakingLease = true
				break
			}
		}
		if !hasBreakingLease {
			lm.unlock()
			return nil
		}

		ch, ok := lm.breakWaitChans[handleKey]
		if !ok {
			ch = make(chan struct{})
			lm.breakWaitChans[handleKey] = ch
		}
		lm.unlock()

		select {
		case <-ctx.Done():
			// Force-break the holder's lease only when our own wait deadline
			// expired (the holder genuinely failed to ACK in time). On a client
			// cancellation no byte-range lock will be inserted, so we must not
			// downgrade the SMB holder's lease as a side effect.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				lm.forceCompleteBreaksExceptKey(handleKey, [16]byte{})
			}
			return ctx.Err()
		case <-ch:
			continue
		}
	}
}

// byteRangeLockBreakWaitTimeout bounds how long a byte-range LOCK (NFSv4 or NLM)
// waits for a conflicting SMB write-lease to finish breaking before it inserts
// the lock. It mirrors the SMB lease-break ack-wait timeout: a holder that has
// not ACKed within this window is treated as non-responsive, its lease is
// force-downgraded to None (Samba lease_timeout parity), and the lock is granted.
const byteRangeLockBreakWaitTimeout = 5 * time.Second

// WaitForByteRangeLockBreak drains an in-flight lease break before a byte-range
// LOCK inserts its lock. Callers must invoke it between
// BreakLeasesForByteRangeLock and AddUnifiedLock.
//
// BreakLeasesForByteRangeLock is fire-and-forget: it marks the conflicting lease
// Breaking and dispatches the break asynchronously, but the lease keeps its state
// until the holder ACKs. The byte-lock conflict check gates on the lease's Write
// bit, not on the Breaking flag, so inserting the lock before the break drains
// returns a spurious DENIED (surfaced to the client as EIO) — the #1501
// regression.
//
// Deadlock safety: it waits via WaitForByteRangeLeaseBreak, which is leases-only
// (it never blocks on an NFSv4 delegation break — DELEGRETURN needs the
// StateManager mutex this path holds) and releases the lock-manager mutex before
// blocking, so it cannot deadlock against the break dispatch or the SMB
// LEASE_BREAK_ACK path. The wait is bounded by byteRangeLockBreakWaitTimeout so a
// stuck holder cannot stall the lock indefinitely; on expiry the breaking lease
// is force-downgraded to None and the lock is granted. The empty exclude key
// means "no exclusion": NFS/NLM owners hold no SMB lease of their own.
//
// It returns a non-nil error only when the originating ctx itself was cancelled
// (e.g. the client disconnected) — the caller must then abort rather than insert
// a lock nobody is waiting for. A derived deadline expiry returns nil: the lease
// was force-downgraded above, so granting the lock is correct.
func WaitForByteRangeLockBreak(ctx context.Context, lm LockManager, handleKey string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, byteRangeLockBreakWaitTimeout)
	defer cancel()
	switch err := lm.WaitForByteRangeLeaseBreak(waitCtx, handleKey); {
	case err == nil:
		// Break drained (ACK or no breaking lease) — caller may insert the lock.
		return nil
	case ctx.Err() != nil:
		// The originating request was cancelled/expired; abort the lock so we
		// don't insert one nobody is waiting for.
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		// Our derived deadline expired: the holder never ACKed, the lease was
		// force-downgraded to None, and the lock may now be granted.
		return nil
	default:
		// Unexpected error from the wait implementation — surface it.
		return err
	}
}

// AnyHolderHasLeaseBits reports whether any lease on handleKey (other than
// exceptKey) currently has any bit in mask set. Non-blocking. Used by the SMB
// CREATE post-break park decision per Samba `delay_for_oplock_fn`: a new opener
// only needs to wait for the in-flight break ACK when the existing holder's
// lease type intersects the delay_mask. Zero exceptKey means "no exclusion".
func (lm *Manager) AnyHolderHasLeaseBits(handleKey string, exceptKey [16]byte, mask uint32) bool {
	if mask == 0 {
		return false
	}
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	hasExclusion := exceptKey != ([16]byte{})
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil {
			continue
		}
		if hasExclusion && l.Lease.LeaseKey == exceptKey {
			continue
		}
		if l.Lease.LeaseState&mask != 0 {
			return true
		}
	}
	return false
}

// HasActiveLeaseRecord reports whether handleKey has any lease record (other
// than one keyed on excludeKey) that is not a timeout tombstone. A holder
// kept alive at LeaseState=None after ack-to-None still counts as active —
// Samba's `disallow_write_lease` predicate (source3/smbd/open.c lines
// 2397-2403) gates on `op_type != NO_OPLOCK`, not on lease state. Timeout
// tombstones (BrokenViaTimeout=true) are excluded so a new opener after the
// abandoned holder's timeout is not constrained by the dead record.
func (lm *Manager) HasActiveLeaseRecord(handleKey string, excludeKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil {
			continue
		}
		if l.Lease.LeaseKey == excludeKey {
			continue
		}
		if l.Lease.BrokenViaTimeout {
			continue
		}
		return true
	}
	return false
}

// IsLeaseBrokenViaTimeout reports whether the lease identified by (handleKey,
// leaseKey) is a timeout tombstone — its break was force-completed because the
// holder never acknowledged it. The handle may still be open; the lease itself
// is dead. The SMB CREATE W-strip uses this to tell a deadbeat same-client
// lease (smb2.lease.timeout: keep WRITE off the new grant) from an active
// same-client lease being upgraded (upgrade2/upgrade3: bypass the strip).
//
// Scoped to handleKey so a lease key bound on more than one file resolves to
// the record on the file being granted, not an arbitrary same-key match.
func (lm *Manager) IsLeaseBrokenViaTimeout(handleKey string, leaseKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.LeaseKey == leaseKey {
			return l.Lease.BrokenViaTimeout
		}
	}
	return false
}

// AnyHolderIsTraditionalOplock reports whether any record on handleKey is a
// traditional oplock (IsTraditionalOplock=true). Used by the SMB CREATE path
// to apply the narrower oplock stat-open mask when a traditional holder is
// present (MS-SMB2 §3.3.5.9 / Samba `is_oplock_stat_open`).
func (lm *Manager) AnyHolderIsTraditionalOplock(handleKey string) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.IsTraditionalOplock {
			return true
		}
	}
	return false
}

// OnlyTimeoutTombstoneRecords reports whether handleKey has at least one
// lease record AND every present lease record is a timeout tombstone
// (BrokenViaTimeout=true). Returns false when no records exist at all, or
// when at least one record is not a timeout tombstone.
//
// Used by the CREATE-grant LEVEL_II coercion to distinguish "holder timed
// out and the server moved on" (only timeout tombstones present → don't
// constrain the new grant by the abandoned holder) from "holder normally
// acked or is still active" (at least one live record → defer to
// bestGrantableState or fall back to non-stat-open coercion).
//
// Covers the contrast between smbtorture batch22b (timeout → tree2 BATCH
// expected) and exclusive9 SUPERSEDE iteration (ack → tree2 LEVEL_II
// expected) which both leave LeaseState=None records but originate from
// different paths.
func (lm *Manager) OnlyTimeoutTombstoneRecords(handleKey string) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	any := false
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil {
			continue
		}
		any = true
		if !l.Lease.BrokenViaTimeout {
			return false
		}
	}
	return any
}

// HasOtherBreakingLeases reports whether any lease (other than exceptKey) or
// any delegation on handleKey is currently Breaking. Non-blocking. Used by the
// SMB CREATE async-park path to decide whether to emit STATUS_PENDING and
// resume the CREATE from a goroutine. A zero exceptKey means "no exclusion" —
// any Breaking lease matches.
func (lm *Manager) HasOtherBreakingLeases(handleKey string, exceptKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	hasExclusion := exceptKey != ([16]byte{})
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.Breaking {
			if !hasExclusion || l.Lease.LeaseKey != exceptKey {
				return true
			}
		}
		if l.Delegation != nil && l.Delegation.Breaking {
			return true
		}
	}
	return false
}

// WaitForBreakCompletion blocks until all breaking locks on a file resolve
// or the context is cancelled. Multiple goroutines may wait concurrently;
// signalBreakWait uses close() to broadcast to all waiters.
//
// On timeout (context cancellation), all leases still in Breaking state are
// automatically downgraded to their BreakToState, as if the client had
// acknowledged. Per MS-SMB2 3.3.5.22.2: if the client fails to acknowledge
// within the timeout, the server completes the break.
func (lm *Manager) WaitForBreakCompletion(ctx context.Context, handleKey string) error {
	for {
		lm.mu.Lock()
		hasBreaking := false
		for _, lock := range lm.unifiedLocks[handleKey] {
			if lock.Lease != nil && lock.Lease.Breaking {
				hasBreaking = true
				break
			}
			if lock.Delegation != nil && lock.Delegation.Breaking {
				hasBreaking = true
				break
			}
		}

		if !hasBreaking {
			lm.unlock()
			return nil
		}

		// Get or create the wait channel while still holding the lock,
		// so no signal from signalBreakWait can be missed.
		ch, ok := lm.breakWaitChans[handleKey]
		if !ok {
			ch = make(chan struct{})
			lm.breakWaitChans[handleKey] = ch
		}
		lm.unlock()

		select {
		case <-ctx.Done():
			// Timeout: auto-downgrade all breaking leases to their break-to state.
			lm.forceCompleteBreaks(handleKey)
			return ctx.Err()
		case <-ch:
			continue
		}
	}
}

// WaitForShareConflictClear blocks until one of three conditions, whichever
// comes first, then returns. It re-evaluates conflictPresent on every break-wait
// signal AND on a short poll:
//
//   - conflictPresent() reports false → the holder CLOSEd (its open-table entry
//     is gone), the conflict cleared → returns nil; the caller's CREATE proceeds.
//   - the holder's break drained but the conflict is still present → the holder
//     ACKed the break while keeping its open (e.g. RWH→RW) → returns nil; the
//     caller's final recheck then yields SHARING_VIOLATION. This early exit is
//     deterministic, so ack-sane does not stall to the deadline.
//   - ctx is cancelled (genuine never-released conflict) → returns ctx.Err();
//     the caller's recheck yields SHARING_VIOLATION against the still-live holder.
//
// A nil return therefore means "stop waiting and recheck", NOT "conflict
// cleared" — the caller MUST re-run the share-mode check to decide the outcome.
//
// Crucially, unlike WaitForBreakCompletion, NONE of these paths force-complete
// (tombstone) the holder's breaking lease: the deferred-open contract
// (MS-SMB2 §3.3.5.9, Samba defer_open→retry_open) lets the holder ack on its own
// schedule, and forcing the lease to None here would make a later ACK fail
// STATUS_UNSUCCESSFUL (smbtorture dhv2-pending1n-vs-violation-lease-ack-sane).
func (lm *Manager) WaitForShareConflictClear(ctx context.Context, handleKey string, conflictPresent func() bool) error {
	// Poll interval. A holder CLOSE that resolves the conflict does NOT signal
	// the breakWaitChans channel for file leases (that signal is scoped to
	// directory leases so a file-lease holder closing without ACKing cannot
	// prematurely wake a regular parked CREATE — smbtorture
	// smb2.kernel-oplocks.kernel_oplocks7). The deferred-open resume instead
	// re-evaluates the live share-mode predicate on a short poll, plus on every
	// break-wait signal, so it wakes promptly on either a CLOSE or an ACK
	// without disturbing the regular break-completion wait path.
	const pollInterval = 100 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if !conflictPresent() {
			return nil
		}

		lm.mu.Lock()
		// Early deterministic exit: the conflict is still present but no break
		// is in flight any more. The holder resolved its break by ACKing
		// (keeping its open, e.g. RWH→RW) rather than CLOSEing, so the share
		// conflict will never clear on its own. Return now so the caller's
		// final recheck yields SHARING_VIOLATION promptly instead of stalling
		// until the deadline (smbtorture dhv2-pending1n-vs-violation-lease-ack-sane).
		if !lm.hasBreakingLeaseLocked(handleKey) {
			lm.unlock()
			return nil
		}
		ch, ok := lm.breakWaitChans[handleKey]
		if !ok {
			ch = make(chan struct{})
			lm.breakWaitChans[handleKey] = ch
		}
		lm.unlock()

		// Re-check after subscribing so a signal racing between the predicate
		// evaluation above and channel registration is not missed.
		if !conflictPresent() {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			continue
		case <-ticker.C:
			continue
		}
	}
}

// hasBreakingLeaseLocked reports whether any lease or delegation on handleKey is
// currently in the Breaking state. Caller holds lm.mu.
func (lm *Manager) hasBreakingLeaseLocked(handleKey string) bool {
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.Breaking {
			return true
		}
		if l.Delegation != nil && l.Delegation.Breaking {
			return true
		}
	}
	return false
}

// forceCompleteBreaks force-revokes all breaking leases on a file to None when
// the break wait times out. Records are kept alive at LeaseState=None
// (handle-bound lifetime) so a later unsolicited ack surfaces as
// ErrLeaseAckNotBreaking → STATUS_UNSUCCESSFUL.
func (lm *Manager) forceCompleteBreaks(handleKey string) {
	lm.forceCompleteBreaksExceptKey(handleKey, [16]byte{})
}

// forceCompleteBreaksExceptKey is forceCompleteBreaks that leaves any lease
// keyed on exceptKey untouched. Zero exceptKey means "no exclusion" (same
// semantics as forceCompleteBreaks).
//
// Forces breaking leases to None, mirroring Samba's lease_timeout_handler
// (source3/smbd/smb2_oplock.c) which calls
// `downgrade_lease(..., SMB2_LEASE_NONE)` regardless of the in-flight or
// cumulative break target. A non-acking client must not be allowed to retain
// any lease bits past the timeout — otherwise a later opener observes stale
// state (smb2.lease.timeout: probe of original lease key returns RH instead
// of the spec-mandated empty state) and stale R/H rights would generate
// spurious break notifications on subsequent IO.
func (lm *Manager) forceCompleteBreaksExceptKey(handleKey string, exceptKey [16]byte) {
	lm.mu.Lock()
	defer lm.unlock()

	modified := false
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil || !l.Lease.Breaking || l.Lease.LeaseKey == exceptKey {
			continue
		}
		modified = true
		// Force-revoke to None and keep the record alive at LeaseState=None
		// (handle-bound lifetime) so a later unsolicited or duplicate ack
		// surfaces as ErrLeaseAckNotBreaking → STATUS_UNSUCCESSFUL. Same
		// rationale as applyBreakStageLocked.
		//
		// Do NOT advance Epoch: this is the timeout/internal completion
		// path for an already-dispatched break. Per MS-SMB2 §3.3.4.7 the
		// epoch advances only when a break notification is dispatched (and
		// was already advanced when the in-flight break started). Bumping
		// it here would invalidate any straggling client ack still echoing
		// the original epoch.
		l.Lease.LeaseState = LeaseStateNone
		l.Lease.BreakingToRequired = LeaseStateNone
		l.Lease.Breaking = false
		l.Lease.BreakToState = 0
		l.Lease.BreakStarted = time.Time{}
		l.Lease.BrokenViaTimeout = true
		l.Type = lockTypeForLeaseState(l.Lease.LeaseState)

		lm.persistUnifiedLockLocked(l)
		logger.Debug("forceCompleteBreaks: auto-downgraded lease",
			"handleKey", handleKey,
			"leaseKey", fmt.Sprintf("%x", l.Lease.LeaseKey),
			"newState", LeaseStateToString(l.Lease.LeaseState))
	}

	if modified {
		lm.signalBreakWaitLocked(handleKey)
	}
}

// signalBreakWait broadcasts to all waiters by closing the wait channel and
// removing it from the map. The next WaitForBreakCompletion call will create
// a fresh channel if needed. Acquires lm.mu internally.
func (lm *Manager) signalBreakWait(handleKey string) {
	lm.mu.Lock()
	lm.signalBreakWaitLocked(handleKey)
	lm.unlock()
}

// SignalParkedCreates is the LockManager-interface entry point for
// signalBreakWait, exposed so the SMB CLOSE path can wake a parked CREATE
// waiter after the open-file table entry has been removed. See interface doc.
func (lm *Manager) SignalParkedCreates(handleKey string) {
	lm.signalBreakWait(handleKey)
}

// signalBreakWaitLocked is the lock-held variant of signalBreakWait.
// Caller must hold lm.mu.
func (lm *Manager) signalBreakWaitLocked(handleKey string) {
	if ch, ok := lm.breakWaitChans[handleKey]; ok {
		close(ch)
		delete(lm.breakWaitChans, handleKey)
	}
}

// breakOpLocks marks matching oplocks as breaking and dispatches their break
// notifications immediately. See markOpLockBreaks for the marking rules.
func (lm *Manager) breakOpLocks(
	handleKey string,
	excludeOwner *LockOwner,
	breakToState uint32,
	reason BreakReason,
	shouldBreak func(lease *OpLock) bool,
) error {
	lm.markOpLockBreaks(handleKey, excludeOwner, breakToState, reason, shouldBreak)()
	return nil
}

// markOpLockBreaks records the break on every matching lease under handleKey
// and returns the function that puts the still-unsent notifications on the
// wire. The victim set is decided here, under lm.mu, so a lease granted after
// this returns is never caught by a change that predates it. The mutex is
// released before the returned function dispatches, to avoid deadlock.
//
// breakToState is the target state for the break. Pass BreakToStripWrite
// to compute the per-lease break-to state by stripping the Write bit from
// each lease's current state (preserving Read and Handle).
//
// Concurrent-break behavior: when a lease is already Breaking, the new
// target is AND-merged into BreakingToRequired (cumulative final target)
// without dispatching a new notification or advancing the epoch. This
// mirrors Samba `process_oplock_break_message` lines 956-965; the next
// progressive stage is dispatched from acknowledgeLeaseBreakImpl after the
// in-flight ACK arrives.
func (lm *Manager) markOpLockBreaks(
	handleKey string,
	excludeOwner *LockOwner,
	breakToState uint32,
	reason BreakReason,
	shouldBreak func(lease *OpLock) bool,
) func() {
	lm.mu.Lock()
	locks := lm.unifiedLocks[handleKey]

	type breakEntry struct {
		lock         *UnifiedLock
		breakToState uint32
	}
	var toBreak []breakEntry
	// Dedup wire break notifications by lease key. A handleKey can hold more
	// than one *UnifiedLock with the same lease key — e.g. an orphaned
	// ack-to-None record left by an unclean disconnect coexisting with a live
	// regrant under the smbtorture-reused DLEASE constant. Samba dispatches
	// exactly one LEASE_BREAK per distinct holder/lease key per change
	// (source3/smbd/smb2_oplock.c contend_dirleases); dispatching once per
	// record sends duplicate notifications that all route to the same live
	// session via the shared ClientGUID, producing the intermittent
	// smb2.dirlease.unlink_*_and_close "lease_break_info.count got 0x2" flake.
	dispatchedKeys := make(map[[16]byte]struct{}, len(locks))
	// canonicalByKey remembers the record whose break stage drove the single
	// wire notification for each lease key. A sibling sharing that key (an
	// orphaned ack-to-None record left by an unclean disconnect coexisting
	// with the live regrant) must NOT send a second notification, but its
	// internal break stage MUST be mirrored from the canonical record so a
	// later scan can't treat the stale sibling as an active non-breaking
	// lease and dispatch a fresh spurious break (per MS-SMB2 §3.3.5.9: opens
	// sharing a lease key share one logical lease).
	canonicalByKey := make(map[[16]byte]*OpLock, len(locks))
	for _, lock := range locks {
		if lock.Lease == nil {
			continue
		}
		if excludeOwner != nil {
			if lock.Owner.OwnerID == excludeOwner.OwnerID ||
				(excludeOwner.ClientID != "" && lock.Owner.ClientID == excludeOwner.ClientID) {
				continue
			}
			// Per MS-SMB2 3.3.5.9: opens with the same lease key must not
			// break each other's leases ("If Open.Lease.LeaseKey == the new
			// open's LeaseKey, no break is required").
			if excludeOwner.ExcludeLeaseKey != ([16]byte{}) &&
				lock.Lease.LeaseKey == excludeOwner.ExcludeLeaseKey {
				continue
			}
			// Per MS-SMB2 §3.3.4.20 / Samba dirlease_should_break: when a
			// child CREATE or SET_INFO carries an RqLs whose ParentLeaseKey
			// matches the parent dir lease's key, suppress that dir-lease
			// break. Scoped to dir leases to avoid suppressing an unrelated
			// file-lease break that happens to share the key value (#470 C2).
			if excludeOwner.HasExcludeParentDirLeaseKey &&
				lock.Lease.IsDirectory &&
				lock.Lease.LeaseKey == excludeOwner.ExcludeParentDirLeaseKey {
				continue
			}
		}
		if !shouldBreak(lock.Lease) {
			continue
		}

		// One notification per distinct lease key (see dispatchedKeys above):
		// a duplicate record under this handleKey must not produce a second
		// wire break for the same lease. Marked once the key is accepted for
		// a fresh dispatch below so the live record drives the notification and
		// any sibling sharing its key is skipped.
		if _, already := dispatchedKeys[lock.Lease.LeaseKey]; already {
			// Mirror the canonical record's break stage onto this sibling
			// (no wire notification, no second epoch advance) so its state
			// stays consistent with the one logical lease.
			if canonical := canonicalByKey[lock.Lease.LeaseKey]; canonical != nil {
				lm.mirrorBreakStageLocked(lock, canonical, true)
			}
			continue
		}

		targetState := computeFreshTarget(lock.Lease.LeaseState, breakToState)

		// Per Samba `delay_for_oplock_fn` (source3/smbd/open.c lines 2439-2444):
		// traditional oplocks only support breaking to R or NONE — any Handle
		// or Write residue in the strip-W/strip-H target must be cleared so the
		// holder lands at R (LEVEL_II) or 0 (NONE). Lease holders retain the
		// fine-grained break-to bits; this mask only applies to traditional
		// oplocks tagged at grant time. Required by smbtorture
		// smb2.oplock.batch9a (BATCH attrs-only holder must break to R so the
		// subsequent normal-open BATCH request can be granted LEVEL_II via
		// bestGrantableState).
		if lock.Lease.IsTraditionalOplock {
			targetState &^= LeaseStateHandle | LeaseStateWrite
		}

		if lock.Lease.Breaking {
			// Concurrent break: AND-merge the new opener's target into the
			// cumulative final target. No notification, no epoch bump
			// (Samba intentionally skips the bump per its inline comment).
			// The next progressive stage will be dispatched on ACK.
			lock.Lease.BreakingToRequired &= targetState
			// Upgrade the recorded reason if this opener classifies the break
			// (e.g. an open-conflict lease downgrade merging into an in-flight
			// fire-and-forget break); never clobber a real reason with Unspecified.
			if reason != BreakReasonUnspecified {
				lock.Lease.BreakReason = reason
			}
			lm.persistUnifiedLockLocked(lock)
			// This key's break is already in flight; record it so a sibling
			// record sharing the key is not freshly dispatched below and so
			// later siblings mirror this record's break stage.
			dispatchedKeys[lock.Lease.LeaseKey] = struct{}{}
			canonicalByKey[lock.Lease.LeaseKey] = lock.Lease
			continue
		}

		// Fresh dispatch: BreakingToRequired starts at this opener's target.
		// Subsequent concurrent breaks may tighten it via the AND-merge above.
		// Advance the epoch here so the dispatched notification's NewEpoch is
		// pre-bumped (per MS-SMB2 2.2.23.2). Post-ACK progressive stages do
		// NOT advance — the multi-stage progression is one logical break.
		lock.Lease.BreakingToRequired = targetState
		lock.Lease.BreakReason = reason
		advanceEpoch(lock.Lease)
		snapshot := lm.applyBreakStageLocked(lock, targetState)
		// Persist the in-flight Breaking state so a crash/restart preserves
		// the break-in-progress and parked CREATEs aren't stranded waiting
		// for a notification that was already sent over the wire.
		// applyBreakStageLocked only persists the fire-and-forget downgrade
		// path; the ack-required path (which is the common case) is
		// persisted here.
		lm.persistUnifiedLockLocked(lock)
		dispatchedKeys[lock.Lease.LeaseKey] = struct{}{}
		canonicalByKey[lock.Lease.LeaseKey] = lock.Lease
		toBreak = append(toBreak, breakEntry{lock: snapshot, breakToState: targetState})
	}
	lm.unlock()

	return func() {
		for _, entry := range toBreak {
			lm.dispatchOpLockBreak(handleKey, entry.lock, entry.breakToState)
		}
	}
}

// computeFreshTarget resolves a breakOpLocks sentinel against the current
// lease state, returning the actual per-lease target. Direct state values
// pass through unchanged.
func computeFreshTarget(currentState, sentinel uint32) uint32 {
	switch sentinel {
	case BreakToStripWrite:
		// Per MS-SMB2 3.3.5.9: RWH -> RH, RW -> R.
		return currentState &^ LeaseStateWrite
	case BreakToStripHandle:
		// Per MS-SMB2 3.3.5.9 Step 10: RWH -> RW, RH -> R.
		return currentState &^ LeaseStateHandle
	}
	return sentinel
}

// applyBreakStageLocked performs a single break stage on lock targeting
// `target`. Caller must hold lm.mu, must have already set
// lock.Lease.BreakingToRequired appropriately, and is responsible for
// dispatching the returned snapshot via dispatchOpLockBreak after releasing
// lm.mu.
//
// Per MS-SMB2 3.3.4.7, a break is ack-required iff the current state is NOT
// pure Read. Without ACK_REQUIRED the client never responds, so leaving
// Breaking=true would block same-key reopens — instead we resolve inline.
//
// For target=None on the inline (fire-and-forget) path we keep the record
// alive at LeaseState=None (handle-bound lifetime) so a later unsolicited
// ack from the client surfaces as ErrLeaseAckNotBreaking →
// STATUS_UNSUCCESSFUL per MS-SMB2 3.3.5.22.2 (smbtorture breaking5). The
// record is removed when the holding handle CLOSEs (ReleaseLeaseForHandle).
func (lm *Manager) applyBreakStageLocked(lock *UnifiedLock, target uint32) *UnifiedLock {
	// Snapshot while LeaseState still holds the pre-break value for
	// CurrentLeaseState in the notification. Caller is responsible for
	// advancing the epoch on fresh dispatch (per MS-SMB2 2.2.23.2). Progressive
	// next-stage dispatch from a post-ACK re-eval does NOT advance epoch — the
	// multi-stage break is one continuous progression and Samba's
	// `downgrade_lease` (source3/smbd/smb2_oplock.c line 607) reuses the
	// existing epoch unchanged for each intermediate stage.
	snapshot := lock.Clone()

	ackRequired := lock.Lease.LeaseState != LeaseStateRead
	if ackRequired {
		lock.Lease.Breaking = true
		lock.Lease.BreakToState = target
		lock.Lease.BreakStarted = time.Now()
		return snapshot
	}

	// Fire-and-forget downgrade: client won't ACK (current state is pure R).
	lock.Lease.Breaking = false
	lock.Lease.BreakToState = 0
	lock.Lease.BreakStarted = time.Time{}
	lock.Lease.LeaseState = target
	lock.Lease.BreakingToRequired = target
	lock.Type = lockTypeForLeaseState(target)
	lm.persistUnifiedLockLocked(lock)
	return snapshot
}

// mirrorBreakStageLocked copies the break-stage fields of a canonical lease
// record onto a sibling that shares its lease key, WITHOUT sending a wire
// notification or independently advancing the sibling's epoch. Per MS-SMB2
// §3.3.5.9 opens sharing a lease key share one logical lease, so the sibling
// must end in the same break stage (Breaking / BreakToState / BreakingToRequired
// / BreakStarted / Epoch / LeaseState) as the canonical record. Without this,
// the skipped sibling keeps stale pre-break values and a later scan could treat
// it as an active non-breaking lease and dispatch a fresh spurious break.
//
// The canonical record's epoch was advanced exactly once on its fresh dispatch
// (or intentionally not advanced on the concurrent-break merge path); copying it
// keeps the sibling in lockstep rather than diverging via a second advance.
//
// When persist is true the sibling is persisted (the caller persists the
// canonical too); callers whose canonical path does not persist pass false so
// the sibling stays in lockstep with the canonical's durability. Caller must
// hold lm.mu.
func (lm *Manager) mirrorBreakStageLocked(sibling *UnifiedLock, canonical *OpLock, persist bool) {
	sibling.Lease.Breaking = canonical.Breaking
	sibling.Lease.BreakToState = canonical.BreakToState
	sibling.Lease.BreakingToRequired = canonical.BreakingToRequired
	sibling.Lease.BreakStarted = canonical.BreakStarted
	sibling.Lease.Epoch = canonical.Epoch
	sibling.Lease.LeaseState = canonical.LeaseState
	sibling.Type = lockTypeForLeaseState(canonical.LeaseState)
	if persist {
		lm.persistUnifiedLockLocked(sibling)
	}
}

// ============================================================================
// Break Callback Registration
// ============================================================================

// RegisterBreakCallbacks registers typed callbacks for break notifications.
//
// Multiple callbacks can be registered (one per protocol adapter).
// Callbacks are invoked in registration order during break operations.
func (lm *Manager) RegisterBreakCallbacks(callbacks BreakCallbacks) {
	lm.mu.Lock()
	defer lm.unlock()
	lm.breakCallbacks = append(lm.breakCallbacks, callbacks)
}
