// Package lock provides lock management types and operations for the metadata package.
// This file implements the lease break timeout scanner.
//
// The LeaseBreakScanner monitors breaking leases and force-revokes them on timeout.
// Per MS-SMB2 and CONTEXT.md: "Force revoke on timeout - don't retry, just revoke
// and allow conflicting operation"
package lock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Lease Break Configuration
// ============================================================================

const (
	// DefaultLeaseBreakTimeout is the Windows default (35 seconds).
	// Per MS-SMB2 3.3.6.5: "implementation-specific default value in milliseconds"
	DefaultLeaseBreakTimeout = 35 * time.Second

	// LeaseBreakScanInterval is how often to check for expired breaks.
	LeaseBreakScanInterval = 1 * time.Second
)

// ============================================================================
// Lease Break Callback Interface
// ============================================================================

// LeaseBreakCallback is called when a lease break times out.
// The callback allows the OplockManager to clean up internal state.
type LeaseBreakCallback interface {
	// OnLeaseBreakTimeout is called when a lease break times out without acknowledgment.
	// The lease has already been force-revoked (deleted from store).
	OnLeaseBreakTimeout(leaseKey [16]byte)
}

// ============================================================================
// Lease Break Scanner
// ============================================================================

// LeaseBreakScanner monitors breaking leases and force-revokes on timeout.
//
// The scanner runs in the background, periodically checking for leases
// that are in the "breaking" state and have exceeded the timeout.
//
// When a break times out:
//  1. The lease is deleted from the store (force-revoked)
//  2. The callback is notified so it can clean up tracking state
//  3. The conflicting operation can proceed
type LeaseBreakScanner struct {
	lockStore    LockStore
	callback     LeaseBreakCallback
	timeout      time.Duration
	scanInterval time.Duration

	stop    chan struct{}
	stopped chan struct{}
	mu      sync.Mutex
	running bool
}

// NewLeaseBreakScanner creates a new lease break scanner.
//
// Parameters:
//   - lockStore: The lock store to query for breaking leases
//   - callback: Called when a break times out (can be nil)
//   - timeout: Break timeout (0 = DefaultLeaseBreakTimeout)
func NewLeaseBreakScanner(
	lockStore LockStore,
	callback LeaseBreakCallback,
	timeout time.Duration,
) *LeaseBreakScanner {
	if timeout == 0 {
		timeout = DefaultLeaseBreakTimeout
	}
	return &LeaseBreakScanner{
		lockStore:    lockStore,
		callback:     callback,
		timeout:      timeout,
		scanInterval: LeaseBreakScanInterval,
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
	}
}

// NewLeaseBreakScannerWithInterval creates a new lease break scanner with custom scan interval.
// This is primarily useful for testing.
func NewLeaseBreakScannerWithInterval(
	lockStore LockStore,
	callback LeaseBreakCallback,
	timeout time.Duration,
	scanInterval time.Duration,
) *LeaseBreakScanner {
	if timeout == 0 {
		timeout = DefaultLeaseBreakTimeout
	}
	if scanInterval == 0 {
		scanInterval = LeaseBreakScanInterval
	}
	return &LeaseBreakScanner{
		lockStore:    lockStore,
		callback:     callback,
		timeout:      timeout,
		scanInterval: scanInterval,
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
	}
}

// Start begins the background scan loop.
// Safe to call multiple times (subsequent calls are no-ops).
func (s *LeaseBreakScanner) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stop = make(chan struct{})
	s.stopped = make(chan struct{})
	s.mu.Unlock()

	go s.scanLoop()
}

// Stop stops the background scan loop.
// Blocks until the loop has exited.
// Safe to call multiple times.
func (s *LeaseBreakScanner) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stop)
	s.mu.Unlock()

	<-s.stopped
}

// IsRunning returns true if the scanner is currently running.
func (s *LeaseBreakScanner) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// SetTimeout updates the break timeout.
// This only affects future timeout calculations, not breaks already in progress.
func (s *LeaseBreakScanner) SetTimeout(timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeout = timeout
}

// GetTimeout returns the current break timeout.
func (s *LeaseBreakScanner) GetTimeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeout
}

// ============================================================================
// Internal Implementation
// ============================================================================

// scanLoop is the main background loop.
func (s *LeaseBreakScanner) scanLoop() {
	defer close(s.stopped)

	ticker := time.NewTicker(s.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.scanExpiredBreaks(now)
		}
	}
}

// scanExpiredBreaks checks for and revokes expired lease breaks.
func (s *LeaseBreakScanner) scanExpiredBreaks(now time.Time) {
	ctx := context.Background()

	// Get current timeout (under lock)
	s.mu.Lock()
	timeout := s.timeout
	s.mu.Unlock()

	// Query all leases
	isLease := true
	leases, err := s.lockStore.ListLocks(ctx, LockQuery{
		IsLease: &isLease,
	})
	if err != nil {
		logger.Warn("LeaseBreakScanner: failed to list locks", "error", err)
		return
	}

	for _, pl := range leases {
		// Skip non-leases (should not happen with IsLease filter, but be safe)
		if len(pl.LeaseKey) != 16 {
			continue
		}

		// Skip non-breaking leases
		if !pl.Breaking {
			continue
		}

		// Check if break has expired
		// We use AcquiredAt as the break start time (updated when break initiated)
		breakDeadline := pl.AcquiredAt.Add(timeout)
		if now.After(breakDeadline) {
			var leaseKey [16]byte
			copy(leaseKey[:], pl.LeaseKey)

			logger.Debug("LeaseBreakScanner: break timeout expired",
				"leaseKey", fmt.Sprintf("%x", leaseKey),
				"breakStarted", pl.AcquiredAt,
				"deadline", breakDeadline,
				"timeout", timeout)

			// Force revoke - delete the lease
			if err := s.lockStore.DeleteLock(ctx, pl.ID); err != nil {
				logger.Warn("LeaseBreakScanner: failed to delete expired lease",
					"leaseKey", fmt.Sprintf("%x", leaseKey),
					"error", err)
				continue
			}

			logger.Debug("LeaseBreakScanner: lease force-revoked",
				"leaseKey", fmt.Sprintf("%x", leaseKey))

			// Notify callback (allows conflicting operation to proceed)
			if s.callback != nil {
				s.callback.OnLeaseBreakTimeout(leaseKey)
			}
		}
	}
}
