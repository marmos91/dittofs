package nfs

import (
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
// NFSv4-client-persistence dependency (honest seam note): StateManager grace
// only meaningfully activates when it is told which v4 client IDs are expected
// to reclaim — StartGracePeriod with an empty set is a no-op by design
// (grace.go StartGrace). DittoFS does not yet persist NFSv4 confirmed clients
// across restart (SaveClientState/LoadPreviousClients are unwired), so today
// the v4 expected set is empty and the v4 window is a no-op even though this
// coordinator drives it. The coupling is wired end-to-end so that the moment
// v4 client persistence lands, both machines enter and exit together with no
// further change here. The lock-manager + NLM grace window (the area-5 H-1 /
// NFS H15 fix) is fully live regardless.
type nfsGraceCoordinator struct {
	sm *v4state.StateManager
}

// OnLockGraceStart drives the NFSv4 StateManager into its grace period when a
// share's lock-manager grace begins. The lock manager's expected clients are
// NLM/SMB opaque client IDs, which do not map onto NFSv4 numeric client IDs;
// the v4 expected-reclaim roster is the v4 confirmed-client set, which the
// StateManager derives from its own persisted state (empty until v4 client
// persistence is wired — see the type doc).
func (c *nfsGraceCoordinator) OnLockGraceStart(expectedClients []string) {
	if c.sm == nil {
		return
	}
	if c.sm.IsInGrace() {
		return // already coupled in by an earlier share's recovery
	}
	v4Clients := c.sm.GetConfirmedClientIDs()
	logger.Info("NFSv4 grace coupled to lock-manager grace",
		"lock_clients", len(expectedClients), "v4_clients", len(v4Clients))
	c.sm.StartGracePeriod(v4Clients)
}

// OnLockGraceEnd ends the NFSv4 grace period when the lock-manager grace window
// closes (timer, early-exit, or sweep), keeping the two windows aligned.
func (c *nfsGraceCoordinator) OnLockGraceEnd() {
	if c.sm == nil {
		return
	}
	c.sm.ForceEndGrace()
}
