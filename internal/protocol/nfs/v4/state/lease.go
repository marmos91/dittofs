package state

import (
	"sync"
	"time"
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
