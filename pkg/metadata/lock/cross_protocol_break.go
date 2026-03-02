// Package lock provides cross-protocol break coordination helpers.
//
// Coordinates break operations that span protocol boundaries (e.g., SMB write
// breaking NFS delegation, NFS open breaking SMB lease).
package lock

import (
	"fmt"
	"time"
)

// BreakResult captures the outcome of a cross-protocol break operation.
// Records how many leases (SMB) and delegations (NFS) were broken, along
// with the file handle and duration.
type BreakResult struct {
	LeasesBroken      int
	DelegationsBroken int
	HandleKey         string
	Duration          time.Duration
}

// IsCrossProtocol returns true if both leases and delegations were broken,
// indicating a true cross-protocol conflict scenario.
func (r BreakResult) IsCrossProtocol() bool {
	return r.LeasesBroken > 0 && r.DelegationsBroken > 0
}

// Total returns the total number of caching states broken.
func (r BreakResult) Total() int {
	return r.LeasesBroken + r.DelegationsBroken
}

// FormatCrossProtocolBreak formats a BreakResult for INFO-level logging.
func FormatCrossProtocolBreak(result BreakResult) string {
	if result.LeasesBroken > 0 && result.DelegationsBroken > 0 {
		return fmt.Sprintf("cross-protocol break on %s: %d lease(s) + %d delegation(s) broken in %v",
			result.HandleKey, result.LeasesBroken, result.DelegationsBroken, result.Duration)
	}
	if result.LeasesBroken > 0 {
		return fmt.Sprintf("lease break on %s: %d lease(s) broken in %v",
			result.HandleKey, result.LeasesBroken, result.Duration)
	}
	if result.DelegationsBroken > 0 {
		return fmt.Sprintf("delegation recall on %s: %d delegation(s) recalled in %v",
			result.HandleKey, result.DelegationsBroken, result.Duration)
	}
	return fmt.Sprintf("no breaks needed on %s", result.HandleKey)
}

// IsCrossProtocolConflict returns true if both leases and delegations exist
// on the same file handle, indicating a cross-protocol caching scenario.
func IsCrossProtocolConflict(locks []*UnifiedLock) bool {
	hasLease := false
	hasDeleg := false

	for _, l := range locks {
		if l.Lease != nil && l.Lease.LeaseState != LeaseStateNone {
			hasLease = true
		}
		if l.Delegation != nil && !l.Delegation.Revoked {
			hasDeleg = true
		}
		if hasLease && hasDeleg {
			return true
		}
	}
	return false
}

// ClassifyBreakScenario returns a human-readable description of the
// cross-protocol break scenario for logging.
func ClassifyBreakScenario(operationType string, result BreakResult) string {
	if result.LeasesBroken > 0 && result.DelegationsBroken > 0 {
		return fmt.Sprintf("%s breaks NFS delegation + SMB lease", operationType)
	}
	if result.DelegationsBroken > 0 {
		return fmt.Sprintf("%s breaks NFS delegation", operationType)
	}
	if result.LeasesBroken > 0 {
		return fmt.Sprintf("%s breaks SMB lease", operationType)
	}
	return fmt.Sprintf("%s: no breaks needed", operationType)
}

// CountBreakableState counts the number of non-breaking leases and delegations
// on a file handle that could potentially need breaking.
func CountBreakableState(locks []*UnifiedLock) (leases, delegations int) {
	for _, l := range locks {
		if l.Lease != nil && l.Lease.LeaseState != LeaseStateNone && !l.Lease.Breaking {
			leases++
		}
		if l.Delegation != nil && !l.Delegation.Revoked && !l.Delegation.Breaking {
			delegations++
		}
	}
	return
}
