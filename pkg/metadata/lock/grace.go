package lock

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Grace Period State Machine
// ============================================================================

// GraceState represents the state of the grace period.
type GraceState int

const (
	// GraceStateNormal indicates normal operation - all lock operations allowed.
	GraceStateNormal GraceState = iota

	// GraceStateActive indicates grace period is active - only reclaims allowed.
	GraceStateActive
)

// String returns a human-readable name for the grace state.
func (gs GraceState) String() string {
	switch gs {
	case GraceStateNormal:
		return "normal"
	case GraceStateActive:
		return "active"
	default:
		return "unknown"
	}
}

// Operation describes the type of lock operation for grace period checking.
type Operation struct {
	// IsReclaim indicates this is a lock reclaim during grace period.
	IsReclaim bool

	// IsTest indicates this is a lock test (query) operation.
	IsTest bool

	// IsNew indicates this is a new lock request (not reclaim or test).
	IsNew bool
}

// GracePeriodManager manages the grace period state machine.
//
// After server restart, clients need time to reclaim their locks. The grace
// period allows reclaims while blocking new lock requests. This prevents
// races where a new client could acquire a lock before the previous owner
// has a chance to reclaim it.
//
// The grace period ends when:
//   - The configured duration expires (timer-based exit)
//   - All expected clients have reclaimed their locks (early exit)
//   - ExitGracePeriod() is explicitly called
//
// Thread Safety:
// All methods are safe for concurrent use by multiple goroutines.
type GracePeriodManager struct {
	mu sync.RWMutex

	// state is the current grace period state
	state GraceState

	// graceEnd is when the grace period is scheduled to expire
	graceEnd time.Time

	// duration is the configured grace period duration
	duration time.Duration

	// expectedClients is the set of clients expected to reclaim (from persisted locks)
	expectedClients map[string]bool

	// reclaimedClients is the set of clients that have reclaimed
	reclaimedClients map[string]bool

	// timer is the grace period timeout timer
	timer *time.Timer

	// onGraceEnd is the callback invoked when grace period ends
	onGraceEnd func()
}

// NewGracePeriodManager creates a new GracePeriodManager.
//
// Parameters:
//   - duration: The grace period duration (how long to wait for reclaims)
//   - onGraceEnd: Callback invoked when grace period ends (can be nil).
//     This is typically used to clean up unclaimed locks.
//
// The manager starts in Normal state.
func NewGracePeriodManager(duration time.Duration, onGraceEnd func()) *GracePeriodManager {
	return &GracePeriodManager{
		state:            GraceStateNormal,
		duration:         duration,
		expectedClients:  make(map[string]bool),
		reclaimedClients: make(map[string]bool),
		onGraceEnd:       onGraceEnd,
	}
}

// EnterGracePeriod transitions to the Active state.
//
// This is called on server startup when persisted locks exist. During the
// grace period:
//   - Reclaim operations are allowed
//   - Lock test operations are allowed
//   - New lock requests are denied with ErrGracePeriod
//
// Parameters:
//   - expectedClients: List of client IDs expected to reclaim their locks.
//     If all these clients reclaim, the grace period exits early.
//
// If already in Active state, this is a no-op.
func (gpm *GracePeriodManager) EnterGracePeriod(expectedClients []string) {
	gpm.mu.Lock()
	defer gpm.mu.Unlock()

	if gpm.state == GraceStateActive {
		return // Already in grace period
	}

	gpm.state = GraceStateActive
	gpm.graceEnd = time.Now().Add(gpm.duration)

	// Store expected clients
	gpm.expectedClients = make(map[string]bool)
	for _, clientID := range expectedClients {
		gpm.expectedClients[clientID] = true
	}
	gpm.reclaimedClients = make(map[string]bool)

	logger.Info("Entering grace period",
		"duration", gpm.duration,
		"expected_clients", len(expectedClients))

	// Start timer for automatic exit
	gpm.timer = time.AfterFunc(gpm.duration, func() {
		gpm.exitGracePeriodInternal()
	})
}

// ExitGracePeriod manually exits the grace period.
//
// This transitions to Normal state, cancels any pending timer, and
// invokes the onGraceEnd callback.
//
// If already in Normal state, this is a no-op.
func (gpm *GracePeriodManager) ExitGracePeriod() {
	gpm.mu.Lock()

	if gpm.state == GraceStateNormal {
		gpm.mu.Unlock()
		return // Already in normal state
	}

	// Stop timer if running
	if gpm.timer != nil {
		gpm.timer.Stop()
		gpm.timer = nil
	}

	gpm.state = GraceStateNormal
	gpm.graceEnd = time.Time{}

	callback := gpm.onGraceEnd

	logger.Info("Grace period ended (manual exit)")

	gpm.mu.Unlock()

	// Call callback outside of lock to avoid deadlocks
	if callback != nil {
		callback()
	}
}

// exitGracePeriodInternal is called when the timer expires or early exit is triggered.
// Caller must NOT hold the lock.
func (gpm *GracePeriodManager) exitGracePeriodInternal() {
	gpm.mu.Lock()

	if gpm.state == GraceStateNormal {
		gpm.mu.Unlock()
		return // Already in normal state
	}

	gpm.state = GraceStateNormal
	gpm.graceEnd = time.Time{}
	gpm.timer = nil

	callback := gpm.onGraceEnd

	logger.Info("Grace period ended",
		"reclaimed_clients", len(gpm.reclaimedClients),
		"expected_clients", len(gpm.expectedClients))

	gpm.mu.Unlock()

	// Call callback outside of lock to avoid deadlocks
	if callback != nil {
		callback()
	}
}

// IsOperationAllowed checks if a lock operation is allowed in the current state.
//
// Parameters:
//   - op: The lock operation to check
//
// Returns:
//   - (true, nil) if the operation is allowed
//   - (false, ErrGracePeriod error) if the operation is blocked
//
// During Normal state: all operations allowed.
// During Active state:
//   - Reclaims: allowed
//   - Tests: allowed
//   - New locks: denied with ErrGracePeriod
func (gpm *GracePeriodManager) IsOperationAllowed(op Operation) (bool, error) {
	gpm.mu.RLock()
	defer gpm.mu.RUnlock()

	// Normal state - all operations allowed
	if gpm.state == GraceStateNormal {
		return true, nil
	}

	// Active state - check operation type
	if op.IsReclaim || op.IsTest {
		return true, nil
	}

	// New lock request during grace period - denied
	remaining := int(time.Until(gpm.graceEnd).Seconds())
	if remaining < 0 {
		remaining = 0
	}

	return false, NewGracePeriodError(remaining)
}

// MarkReclaimed records that a client has reclaimed their locks.
//
// If all expected clients have reclaimed, the grace period exits early.
//
// Parameters:
//   - clientID: The client ID that has reclaimed
func (gpm *GracePeriodManager) MarkReclaimed(clientID string) {
	gpm.mu.Lock()

	if gpm.state == GraceStateNormal {
		gpm.mu.Unlock()
		return // Not in grace period
	}

	gpm.reclaimedClients[clientID] = true

	logger.Debug("Client reclaimed locks",
		"client_id", clientID,
		"reclaimed", len(gpm.reclaimedClients),
		"expected", len(gpm.expectedClients))

	// Check for early exit
	if gpm.checkEarlyExitLocked() {
		// Stop timer
		if gpm.timer != nil {
			gpm.timer.Stop()
			gpm.timer = nil
		}

		gpm.state = GraceStateNormal
		gpm.graceEnd = time.Time{}

		callback := gpm.onGraceEnd

		logger.Info("Grace period ending early: all clients reclaimed",
			"reclaimed", len(gpm.reclaimedClients))

		gpm.mu.Unlock()

		// Call callback outside of lock
		if callback != nil {
			callback()
		}
		return
	}

	gpm.mu.Unlock()
}

// checkEarlyExitLocked checks if all expected clients have reclaimed.
// Caller must hold the lock.
func (gpm *GracePeriodManager) checkEarlyExitLocked() bool {
	if len(gpm.expectedClients) == 0 {
		return false // No expected clients, don't exit early
	}

	for clientID := range gpm.expectedClients {
		if !gpm.reclaimedClients[clientID] {
			return false // Still waiting for this client
		}
	}

	return true // All expected clients have reclaimed
}

// GetState returns the current grace period state.
func (gpm *GracePeriodManager) GetState() GraceState {
	gpm.mu.RLock()
	defer gpm.mu.RUnlock()
	return gpm.state
}

// GetRemainingTime returns the time remaining until the grace period ends.
//
// Returns 0 if not in grace period or if the grace period has expired.
func (gpm *GracePeriodManager) GetRemainingTime() time.Duration {
	gpm.mu.RLock()
	defer gpm.mu.RUnlock()

	if gpm.state == GraceStateNormal {
		return 0
	}

	remaining := time.Until(gpm.graceEnd)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// GetExpectedClients returns the list of expected client IDs.
//
// This is useful for debugging and monitoring.
func (gpm *GracePeriodManager) GetExpectedClients() []string {
	gpm.mu.RLock()
	defer gpm.mu.RUnlock()

	result := make([]string, 0, len(gpm.expectedClients))
	for clientID := range gpm.expectedClients {
		result = append(result, clientID)
	}
	return result
}

// GetReclaimedClients returns the list of clients that have reclaimed.
//
// This is useful for debugging and monitoring.
func (gpm *GracePeriodManager) GetReclaimedClients() []string {
	gpm.mu.RLock()
	defer gpm.mu.RUnlock()

	result := make([]string, 0, len(gpm.reclaimedClients))
	for clientID := range gpm.reclaimedClients {
		result = append(result, clientID)
	}
	return result
}

// GetDuration returns the configured grace period duration.
func (gpm *GracePeriodManager) GetDuration() time.Duration {
	gpm.mu.RLock()
	defer gpm.mu.RUnlock()
	return gpm.duration
}

// Close stops the grace period manager and cleans up resources.
//
// This cancels any pending timer but does NOT call the onGraceEnd callback.
func (gpm *GracePeriodManager) Close() {
	gpm.mu.Lock()
	defer gpm.mu.Unlock()

	if gpm.timer != nil {
		gpm.timer.Stop()
		gpm.timer = nil
	}

	gpm.state = GraceStateNormal
	gpm.expectedClients = make(map[string]bool)
	gpm.reclaimedClients = make(map[string]bool)
}
