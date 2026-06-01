package nfs

import (
	"sync"

	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/logger"
)

// nfsGraceCoordinator couples the per-share lock-manager grace machine (NLM /
// SMB leases, owned by MetadataService) with the SEPARATE NFSv4 StateManager
// grace machine. When a share recovers persisted locks at registration the
// lock manager enters grace and OnLockGraceStart fires; this coordinator drives
// the NFSv4 StateManager into grace in lockstep, and ends it together on
// OnLockGraceEnd. It implements metadata.GraceCoordinator.
//
// Why two machines exist: NLM/SMB byte-range + lease state lives in the
// lock.Manager, while NFSv4 open/lock state lives in the v4 StateManager. Both
// must reject conflicting NEW state during the post-restart window so a prior
// owner can reclaim first (X/Open NLMv4, RFC 7530 §9.6.2). Coupling them here
// is the single seam that keeps the two windows aligned.
//
// Per-share vs global asymmetry, REFCOUNTED (REVIEW slice-3): lock-manager
// grace is PER-SHARE, while the NFSv4 StateManager grace machine is GLOBAL (one
// per server). The coordinator counts open lock-manager grace windows: each
// OnLockGraceStart increments, each OnLockGraceEnd decrements, and the global
// v4 grace is only force-ended when the count returns to zero (the LAST share's
// lock-grace window closes). The OLD "first share in starts, first share out
// ends" policy let one share's early grace-end — e.g. via RemoveStoreForShare
// or that one share's reclaim/timer — prematurely lift global v4 grace while
// OTHER shares' windows were still open, admitting conflicting new v4 state
// before those shares' prior owners could reclaim. With slice-2 wiring real v4
// client persistence (LoadClientRecovery seeds a boot roster), that latent bug
// is now live, hence the refcount.
//
// Interaction with the boot-loaded v4 roster: LoadClientRecovery may seed the
// v4 grace machine with a durable reclaim roster on boot, INDEPENDENT of this
// coordinator. In that case the very first OnLockGraceStart observes v4 grace
// already active and the coordinator does NOT claim ownership of it
// (startedByCoordinator stays false). At refcount zero the coordinator then
// leaves the lift to the v4 machine's OWN governance — its reclaim early-exit
// and its hard grace timer — rather than force-ending and bypassing the roster.
// The v4 grace timer is the always-on backstop (grace.go startGrace), so v4
// grace can never wedge regardless. The coordinator only force-ends v4 grace
// when IT started it (coupled, no independent roster); then refcount-zero is
// the correct lockstep lift.
type nfsGraceCoordinator struct {
	sm *v4state.StateManager

	mu sync.Mutex
	// active counts open per-share lock-manager grace windows coupled through
	// this coordinator. v4 grace is lifted only when this returns to zero.
	active int
	// startedByCoordinator is true when THIS coordinator started the v4 grace
	// machine (v4 grace was not already active at the first start). Only then
	// does the coordinator force-end v4 grace at refcount zero; when v4 grace
	// was boot-seeded with a roster the coordinator defers to the v4 machine's
	// own timer/reclaim lift.
	startedByCoordinator bool
}

// OnLockGraceStart drives the NFSv4 StateManager into its grace period when a
// share's lock-manager grace begins, and increments the open-window refcount.
// The lock manager's expected clients are NLM/SMB opaque client IDs, which do
// not map onto NFSv4 numeric client IDs; the v4 expected-reclaim roster is the
// v4 confirmed-client set, which the StateManager derives from its own state
// (boot-loaded via LoadClientRecovery, else the confirmed-client set).
func (c *nfsGraceCoordinator) OnLockGraceStart(expectedClients []string) {
	if c.sm == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.active++

	// Only the FIRST open window starts (or couples to) v4 grace; subsequent
	// shares just add to the refcount so the window stays open until the last
	// one closes.
	if c.active > 1 {
		return
	}

	if c.sm.IsInGrace() {
		// v4 grace already active — boot-seeded by LoadClientRecovery with a
		// durable roster, or coupled in by a prior cycle. We do NOT own it, so
		// at refcount zero we will leave the lift to the v4 machine's own
		// timer/reclaim governance rather than force-ending.
		c.startedByCoordinator = false
		return
	}

	v4Clients := c.sm.GetConfirmedClientIDs()
	logger.Info("NFSv4 grace coupled to lock-manager grace",
		"lock_clients", len(expectedClients), "v4_clients", len(v4Clients))
	c.sm.StartGracePeriod(v4Clients)
	c.startedByCoordinator = true
}

// OnLockGraceEnd decrements the open-window refcount and ends the NFSv4 grace
// period only when the LAST coupled lock-manager grace window closes (refcount
// reaches zero) AND this coordinator owns the v4 grace window. When v4 grace
// was boot-seeded with a durable roster the coordinator defers to the v4
// machine's own timer/reclaim lift, so an early lock-manager grace-end (timer,
// reclaim, or RemoveStoreForShare) can never prematurely lift global v4 grace
// while other shares — or the v4 reclaim roster — are still outstanding.
func (c *nfsGraceCoordinator) OnLockGraceEnd() {
	if c.sm == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.active == 0 {
		// Unbalanced end (already at zero): ignore rather than underflow. The v4
		// machine's hard timer remains the backstop, so this cannot wedge.
		return
	}
	c.active--
	if c.active > 0 {
		// Other shares' lock-manager grace windows are still open; keep v4 grace
		// up so their prior owners can still reclaim.
		return
	}

	// Last window closed. Force-end v4 grace only if WE started it; otherwise the
	// v4 machine was independently boot-seeded with a roster and governs its own
	// lift (reclaim early-exit + hard timer backstop).
	if c.startedByCoordinator {
		c.sm.ForceEndGrace()
		c.startedByCoordinator = false
	}
}
