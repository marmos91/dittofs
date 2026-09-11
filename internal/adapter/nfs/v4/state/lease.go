package state

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// LeaseState tracks the lease timer for a single NFSv4 client.
// Each confirmed client gets a LeaseState that fires a cleanup callback
// when the lease expires (no renewal within Duration).
//
// The mu mutex is separate from StateManager.mu to avoid deadlock:
// the timer callback must NOT hold lease.mu when calling into StateManager.
type LeaseState struct {
	// ClientID is the client this lease belongs to.
	ClientID uint64

	// Duration is the configured lease duration.
	Duration time.Duration

	// LastRenew is the most recent renewal timestamp.
	LastRenew time.Time

	// timer fires the onExpire callback after Duration without renewal.
	timer *time.Timer

	// onExpire is the callback invoked when the lease expires.
	onExpire func(clientID uint64)

	// mu protects timer reset operations.
	// Separate from StateManager lock to avoid lock ordering issues.
	mu sync.Mutex

	// stopped indicates the lease timer has been explicitly stopped.
	stopped bool
}

// NewLeaseState creates a LeaseState with a timer that fires onExpire
// after duration elapses without a Renew() call.
func NewLeaseState(clientID uint64, duration time.Duration, onExpire func(uint64)) *LeaseState {
	ls := &LeaseState{
		ClientID:  clientID,
		Duration:  duration,
		LastRenew: time.Now(),
		onExpire:  onExpire,
	}

	ls.timer = time.AfterFunc(duration, func() {
		// Check if the lease was renewed between timer fire and callback execution.
		// A Renew() call resets the timer, but if it races with this callback
		// the timer may fire before Reset takes effect. We re-check under the
		// lock and only expire if the lease is truly stale.
		ls.mu.Lock()
		if ls.stopped || time.Since(ls.LastRenew) < ls.Duration {
			ls.mu.Unlock()
			return
		}
		ls.mu.Unlock()

		// Timer callback must NOT hold ls.mu when calling onExpire
		// to avoid deadlock with StateManager.mu.
		if onExpire != nil {
			onExpire(clientID)
		}
	})

	return ls
}

// Renew resets the lease timer and updates the LastRenew timestamp.
// Thread-safe.
func (ls *LeaseState) Renew() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.stopped {
		return
	}

	ls.LastRenew = time.Now()
	ls.timer.Reset(ls.Duration)
}

// IsExpired returns true if the lease has expired (no renewal within Duration).
func (ls *LeaseState) IsExpired() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return time.Since(ls.LastRenew) > ls.Duration
}

// Stop stops the lease timer. Used for clean shutdown.
func (ls *LeaseState) Stop() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	ls.stopped = true
	ls.timer.Stop()
}

// RemainingTime returns how much time remains on the lease.
func (ls *LeaseState) RemainingTime() time.Duration {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	elapsed := time.Since(ls.LastRenew)
	if elapsed >= ls.Duration {
		return 0
	}
	return ls.Duration - elapsed
}

func (sm *StateManager) SetLockManager(lm lock.LockManager) {
	sm.lockManager = lm
}

// SetLockManagerResolver injects a function that resolves the per-share unified
// lock manager for a file handle. Called by the NFS adapter during construction.
//
// Lock managers are per-share, but the NFSv4 StateManager is global. The
// resolver lets each lock operation reach the same manager instance that SMB
// and NLM use for the same file, which is what enables cross-protocol byte-range
// lock conflict detection.
//
// Init-only: must be called before any goroutine serves requests. It is set once
// at startup and never reassigned, so lockManagerFor reads it without a lock. Do
// not call this after the adapter begins serving — there is no synchronization
// against concurrent readers.

func (sm *StateManager) SetLockManagerResolver(resolver func(handle []byte) lock.LockManager) {
	sm.lockManagerResolver = resolver
}

// lockManagerFor resolves the unified lock manager for a file handle. The
// per-handle resolver (production: per-share managers) takes precedence; the
// statically-set lockManager is the fallback (used by tests). May return nil.
//
// Lock-free by design: the resolver and lockManager are init-only (set once
// before any request is served — see SetLockManagerResolver), so reads need no
// synchronization. This also lets callers that already hold sm.mu use it without
// deadlocking.

func (sm *StateManager) lockManagerFor(handle []byte) lock.LockManager {
	if sm.lockManagerResolver != nil {
		return sm.lockManagerResolver(handle)
	}
	return sm.lockManager
}

// SetDelegationsEnabled controls whether delegations can be granted.
// When false, ShouldGrantDelegation always returns OPEN_DELEGATE_NONE.
// This is updated from live NFS adapter settings.
//
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) SetDelegationsEnabled(enabled bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.delegationsEnabled = enabled
}

// SetLeaseTime updates the lease duration used for new client leases.
// Existing leases are not affected (grandfathered).
//
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) SetLeaseTime(d time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if d > 0 {
		sm.leaseDuration = d
	}
}

// SetGracePeriodDuration updates the grace period duration used for future grace periods.
//
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) SetGracePeriodDuration(d time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if d > 0 {
		sm.graceDuration = d
	}
}

// LockNew implements the LOCK operation for a new lock-owner.
//
// This is the "open_to_lock_owner4" path where the client provides an open stateid
// and creates a new lock-owner and lock stateid.
//
// Per RFC 7530 Section 16.10:
//  1. Validate the open stateid and open-owner seqid
//  2. Validate open mode compatibility with lock type
//  3. Find or create the lock-owner
//  4. Find or create the lock state (one per lock-owner + open-state pair)
//  5. Acquire the lock via the unified lock manager
//  6. Update state on success
//
// Caller must NOT hold sm.mu.
