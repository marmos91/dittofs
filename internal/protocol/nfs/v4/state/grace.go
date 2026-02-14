package state

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

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

	// timer fires endGrace() after duration elapses without early completion.
	timer *time.Timer

	// expectedClients maps client IDs that existed before restart.
	// Used to determine if all clients have reclaimed (early exit).
	expectedClients map[uint64]bool

	// reclaimedClients maps clients that have completed reclaim.
	reclaimedClients map[uint64]bool

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

	// Populate expected clients map
	g.expectedClients = make(map[uint64]bool, len(expectedClientIDs))
	for _, id := range expectedClientIDs {
		g.expectedClients[id] = true
	}
	g.reclaimedClients = make(map[uint64]bool)

	logger.Info("NFSv4 grace period started",
		"duration", g.duration,
		"expected_clients", len(expectedClientIDs))

	// Start timer for automatic exit
	g.timer = time.AfterFunc(g.duration, func() {
		g.endGrace()
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

	g.reclaimedClients[clientID] = true

	logger.Debug("NFSv4 grace period: client reclaimed",
		"client_id", clientID,
		"reclaimed", len(g.reclaimedClients),
		"expected", len(g.expectedClients))

	// Check if all expected clients have reclaimed
	allReclaimed := true
	for id := range g.expectedClients {
		if !g.reclaimedClients[id] {
			allReclaimed = false
			break
		}
	}

	if allReclaimed {
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

// endGrace transitions out of the grace period. Idempotent.
// Called by the timer goroutine or by ClientReclaimed when all clients are done.
func (g *GracePeriodState) endGrace() {
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

	logger.Info("NFSv4 grace period ended",
		"reclaimed_clients", reclaimed,
		"expected_clients", expected)

	g.mu.Unlock()

	// Call callback outside lock to avoid deadlocks
	if callback != nil {
		callback()
	}
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
