package state

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// GraceStatusInfo holds structured information about the grace period state.
// Returned by GracePeriodState.Status() for API consumers.
type GraceStatusInfo struct {
	// Active indicates whether the grace period is currently in effect.
	Active bool

	// RemainingSeconds is the estimated time remaining (0 if not active).
	RemainingSeconds float64

	// TotalDuration is the configured grace period duration.
	TotalDuration time.Duration

	// ExpectedClients is the number of clients expected to reclaim.
	ExpectedClients int

	// ReclaimedClients is the number of clients that have completed reclaim.
	ReclaimedClients int

	// StartedAt is when the grace period started (zero if never started).
	StartedAt time.Time
}

// GracePeriodState manages the NFSv4 grace period for server restart recovery.
//
// When the server restarts, clients need a window to reclaim their previously-held
// state (open files, locks). During the grace period:
//   - New state-creating operations are blocked (return NFS4ERR_GRACE)
//   - Reclaim operations (OPEN with CLAIM_PREVIOUS) are allowed
//   - RENEW, CLOSE, READ, WRITE with existing stateids continue working
//
// The grace period ends when:
//   - The configured duration expires (timer-based)
//   - All expected clients have reclaimed (early exit)
//   - Stop() is called explicitly
//
// Per RFC 7530 Section 9.6.
type GracePeriodState struct {
	mu sync.Mutex

	// active indicates the grace period is currently in effect.
	active bool

	// duration is the configured grace period duration.
	duration time.Duration

	// startedAt is when the grace period was started.
	startedAt time.Time

	// timer fires endGraceWithReason after duration elapses without early completion.
	timer *time.Timer

	// expectedClients maps client IDs that existed before restart.
	// Used to determine if all clients have reclaimed (early exit).
	expectedClients map[uint64]bool

	// reclaimedClients maps clients that have completed reclaim.
	reclaimedClients map[uint64]bool

	// reclaimCompleted tracks per-client RECLAIM_COMPLETE tracking (RFC 8881).
	// Separate from reclaimedClients which tracks grace period early-exit.
	reclaimCompleted map[uint64]bool

	// onGraceEnd is the callback invoked when the grace period ends.
	// Called outside the mutex to avoid deadlocks.
	onGraceEnd func()
}

// NewGracePeriodState creates a new GracePeriodState with the given duration
// and end callback. The grace period starts inactive; call StartGrace() to begin.
func NewGracePeriodState(duration time.Duration, onGraceEnd func()) *GracePeriodState {
	return &GracePeriodState{
		duration:         duration,
		expectedClients:  make(map[uint64]bool),
		reclaimedClients: make(map[uint64]bool),
		reclaimCompleted: make(map[uint64]bool),
		onGraceEnd:       onGraceEnd,
	}
}

// StartGrace begins the grace period with the given set of expected client IDs.
//
// If expectedClientIDs is empty, the grace period is skipped entirely (no clients
// to reclaim state from). Otherwise, sets active=true and starts a timer for
// automatic grace period exit after duration.
func (g *GracePeriodState) StartGrace(expectedClientIDs []uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.active {
		return // Already active
	}

	// No expected clients: skip grace period entirely
	if len(expectedClientIDs) == 0 {
		logger.Info("NFSv4 grace period skipped: no previous clients to reclaim")
		return
	}

	g.active = true
	g.startedAt = time.Now()

	// Populate expected clients map
	g.expectedClients = make(map[uint64]bool, len(expectedClientIDs))
	for _, id := range expectedClientIDs {
		g.expectedClients[id] = true
	}
	g.reclaimedClients = make(map[uint64]bool)
	g.reclaimCompleted = make(map[uint64]bool)

	logger.Info("NFSv4 grace period started",
		"duration", g.duration,
		"expected_clients", len(expectedClientIDs))

	// Start timer for automatic exit
	g.timer = time.AfterFunc(g.duration, func() {
		g.endGraceWithReason("ended")
	})
}

// IsInGrace returns true if the grace period is currently active.
func (g *GracePeriodState) IsInGrace() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// ClientReclaimed marks a client as having completed reclaim.
// If ALL expected clients have now reclaimed, the grace period ends early.
func (g *GracePeriodState) ClientReclaimed(clientID uint64) {
	g.mu.Lock()

	if !g.active {
		g.mu.Unlock()
		return
	}

	// Only track clients that were expected during this grace period.
	// Clients created after grace started should not affect the reclaim quota.
	if !g.expectedClients[clientID] {
		g.mu.Unlock()
		return
	}

	g.reclaimedClients[clientID] = true

	logger.Debug("NFSv4 grace period: client reclaimed",
		"client_id", clientID,
		"reclaimed", len(g.reclaimedClients),
		"expected", len(g.expectedClients))

	// Check if all expected clients have reclaimed
	if len(g.reclaimedClients) >= len(g.expectedClients) {
		logger.Info("NFSv4 grace period ending early: all expected clients reclaimed",
			"reclaimed", len(g.reclaimedClients))

		// Stop timer, set inactive, and call callback outside lock
		if g.timer != nil {
			g.timer.Stop()
			g.timer = nil
		}
		g.active = false
		callback := g.onGraceEnd
		g.mu.Unlock()

		if callback != nil {
			callback()
		}
		return
	}

	g.mu.Unlock()
}

// Stop cleanly shuts down the grace period state, stopping any pending timer.
// Does NOT invoke the onGraceEnd callback.
func (g *GracePeriodState) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.active = false
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// ============================================================================
// Grace Period Status, Force End, and RECLAIM_COMPLETE
// ============================================================================

// Status returns structured information about the grace period state.
// Thread-safe: acquires g.mu.
func (g *GracePeriodState) Status() GraceStatusInfo {
	g.mu.Lock()
	defer g.mu.Unlock()

	info := GraceStatusInfo{
		Active:           g.active,
		TotalDuration:    g.duration,
		ExpectedClients:  len(g.expectedClients),
		ReclaimedClients: len(g.reclaimedClients),
		StartedAt:        g.startedAt,
	}

	if g.active {
		elapsed := time.Since(g.startedAt)
		remaining := g.duration - elapsed
		if remaining < 0 {
			remaining = 0
		}
		info.RemainingSeconds = remaining.Seconds()
	}

	return info
}

// ForceEnd immediately ends the grace period and invokes the callback.
// Idempotent: no-op if grace period is not active.
func (g *GracePeriodState) ForceEnd() {
	g.endGraceWithReason("force-ended")
}

// endGraceWithReason deactivates the grace period and invokes the end callback.
// The reason string is used for logging ("ended", "force-ended").
// Idempotent: no-op if grace period is not active.
func (g *GracePeriodState) endGraceWithReason(reason string) {
	g.mu.Lock()

	if !g.active {
		g.mu.Unlock()
		return
	}

	g.active = false

	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}

	callback := g.onGraceEnd
	reclaimed := len(g.reclaimedClients)
	expected := len(g.expectedClients)

	logger.Info("NFSv4 grace period "+reason,
		"reclaimed_clients", reclaimed,
		"expected_clients", expected)

	g.mu.Unlock()

	// Call callback outside lock to avoid deadlocks
	if callback != nil {
		callback()
	}
}

// ReclaimComplete tracks per-client RECLAIM_COMPLETE per RFC 8881 Section 18.51.
//
// Returns:
//   - NFS4ERR_COMPLETE_ALREADY if the client has already sent RECLAIM_COMPLETE
//   - nil on success (including when not in grace period, per RFC 8881)
//
// When in grace period, also calls ClientReclaimed() for grace period early-exit tracking.
func (g *GracePeriodState) ReclaimComplete(clientID uint64) error {
	g.mu.Lock()

	// Check for duplicate
	if g.reclaimCompleted[clientID] {
		g.mu.Unlock()
		return &NFS4StateError{
			Status:  types.NFS4ERR_COMPLETE_ALREADY,
			Message: "reclaim already completed for this client",
		}
	}

	// Mark as reclaim-complete
	g.reclaimCompleted[clientID] = true

	isActive := g.active
	var reclaimDuration time.Duration
	if isActive {
		reclaimDuration = time.Since(g.startedAt)
	}

	logger.Info("RECLAIM_COMPLETE: client reclaim complete",
		"client_id", clientID,
		"in_grace", isActive,
		"reclaim_duration", reclaimDuration)

	g.mu.Unlock()

	// If in grace period, also track for early exit
	if isActive {
		g.ClientReclaimed(clientID)
	}

	return nil
}

// ============================================================================
// Grace Period Errors
// ============================================================================

// ErrGrace is returned when an operation is blocked by the grace period.
var ErrGrace = &NFS4StateError{
	Status:  types.NFS4ERR_GRACE,
	Message: "server in grace period",
}

// ErrNoGrace is returned when CLAIM_PREVIOUS is attempted outside a grace period.
var ErrNoGrace = &NFS4StateError{
	Status:  types.NFS4ERR_NO_GRACE,
	Message: "no grace period available for reclaim",
}

// ============================================================================
// Client Snapshot (for shutdown persistence)
// ============================================================================

// ClientSnapshot captures the essential client state for serialization
// during graceful shutdown. The NFS adapter saves these to a file so
// the grace period can identify which clients need to reclaim on restart.
type ClientSnapshot struct {
	// ClientID is the server-assigned client identifier.
	ClientID uint64

	// ClientIDString is the client-provided opaque identifier.
	ClientIDString string

	// Verifier is the client-provided boot verifier.
	Verifier [8]byte

	// ClientAddr is the client's network address.
	ClientAddr string
}
