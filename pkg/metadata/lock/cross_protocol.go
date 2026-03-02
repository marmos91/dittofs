// Package lock provides cross-protocol translation helpers for lock visibility.
//
// Translates lock information between NLM and SMB for cross-protocol conflict
// reporting and logging.
package lock

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// NLM Holder Info Translation
// ============================================================================

// NLMHolderInfo represents lock holder information in NLM-compatible format.
//
// Used to construct NLM4_DENIED responses when an NFS client's lock request
// conflicts with an SMB lease.
//
// Cross-Protocol Semantics (per CONTEXT.md):
//   - CallerName: "smb:<clientID>" identifies the SMB client
//   - Svid: 0 (SMB has no concept of process ID like Unix)
//   - OH: First 8 bytes of LeaseKey (owner handle)
//   - Offset: 0 (leases are whole-file)
//   - Length: ^uint64(0) (max value, meaning whole file)
//   - Exclusive: true if lease has Write caching permission
type NLMHolderInfo struct {
	CallerName string
	Svid       int32
	OH         []byte
	Offset     uint64
	Length     uint64
	Exclusive  bool
}

// TranslateToNLMHolder converts an SMB lease to NLM holder format for
// NLM4_DENIED responses. Panics if lease is nil or lease.Lease is nil.
func TranslateToNLMHolder(lease *UnifiedLock) NLMHolderInfo {
	if lease == nil || lease.Lease == nil {
		panic("TranslateToNLMHolder called with non-lease lock")
	}

	callerName := extractClientID(lease.Owner.OwnerID)

	oh := make([]byte, 8)
	copy(oh, lease.Lease.LeaseKey[:8])

	return NLMHolderInfo{
		CallerName: callerName,
		Svid:       0,          // SMB has no process ID concept
		OH:         oh,         // First 8 bytes of LeaseKey
		Offset:     0,          // Leases are whole-file
		Length:     ^uint64(0), // Max uint64 = whole file
		Exclusive:  lease.Lease.HasWrite(),
	}
}

// TranslateByteRangeLockToNLMHolder converts a byte-range lock to NLM holder
// format. Panics if lock is nil.
func TranslateByteRangeLockToNLMHolder(lock *UnifiedLock) NLMHolderInfo {
	if lock == nil {
		panic("TranslateByteRangeLockToNLMHolder called with nil lock")
	}

	// Extract components from owner ID if it's in NLM format
	// NLM format: "nlm:{caller_name}:{svid}:{oh_hex}"
	callerName, svid, oh := parseNLMOwnerID(lock.Owner.OwnerID)

	return NLMHolderInfo{
		CallerName: callerName,
		Svid:       svid,
		OH:         oh,
		Offset:     lock.Offset,
		Length:     lock.Length,
		Exclusive:  lock.IsExclusive(),
	}
}

// ============================================================================
// SMB Conflict Reason Translation
// ============================================================================

// TranslateSMBConflictReason generates a human-readable reason for SMB denial
// due to an NLM lock conflict. Used for INFO-level cross-protocol logging.
func TranslateSMBConflictReason(lock *UnifiedLock) string {
	if lock == nil {
		return "unknown conflict"
	}

	clientName := extractClientID(lock.Owner.OwnerID)

	lockType := "shared"
	if lock.IsExclusive() {
		lockType = "exclusive"
	}

	var rangeStr string
	if lock.Length == 0 {
		rangeStr = fmt.Sprintf("bytes %d to end of file", lock.Offset)
	} else if lock.Offset == 0 && lock.Length == ^uint64(0) {
		rangeStr = "entire file"
	} else {
		rangeStr = fmt.Sprintf("bytes %d-%d", lock.Offset, lock.Offset+lock.Length)
	}

	return fmt.Sprintf("NFS client '%s' holds %s lock on %s", clientName, lockType, rangeStr)
}

// TranslateNFSConflictReason generates a human-readable reason for NFS denial
// due to an SMB lease conflict. Used for INFO-level cross-protocol logging.
func TranslateNFSConflictReason(lease *UnifiedLock) string {
	if lease == nil || lease.Lease == nil {
		return "unknown SMB lease conflict"
	}

	clientName := extractClientID(lease.Owner.OwnerID)

	leaseType := "Read"
	if lease.Lease.HasWrite() {
		leaseType = "Write"
	} else if lease.Lease.HasHandle() {
		leaseType = "Handle"
	}

	stateStr := lease.Lease.StateString()

	return fmt.Sprintf("SMB client '%s' holds %s lease (%s)", clientName, leaseType, stateStr)
}

// ============================================================================
// NLM Lock Conflict Detection for Leases (shared package)
// ============================================================================

// CheckNLMLocksForLeaseConflict queries the lock store for NLM byte-range locks
// that would conflict with a requested SMB lease.
//
// Conflict Rules:
//   - Write lease requested: ANY NLM lock conflicts
//   - Read lease requested: ONLY exclusive NLM locks conflict
//   - Handle lease (alone): No conflict with NLM locks
//
// Returns false if lockStore is nil or no conflicts exist.
func CheckNLMLocksForLeaseConflict(lockStore LockStore, ctx context.Context, handleKey string, requestedState uint32, clientID string) bool {
	if lockStore == nil {
		return false
	}

	isLease := false
	locks, err := lockStore.ListLocks(ctx, LockQuery{
		FileID:  handleKey,
		IsLease: &isLease,
	})
	if err != nil {
		logger.Warn("CheckNLMLocksForLeaseConflict: failed to query NLM locks",
			"handleKey", handleKey,
			"error", err)
		return false
	}

	wantsWrite := requestedState&LeaseStateWrite != 0
	wantsRead := requestedState&LeaseStateRead != 0

	for _, pl := range locks {
		el := FromPersistedLock(pl)

		if el.IsLease() {
			continue
		}

		if !wantsWrite && (!wantsRead || !el.IsExclusive()) {
			continue
		}

		logger.Debug("CheckNLMLocksForLeaseConflict: NLM lock conflicts with lease",
			"handleKey", handleKey,
			"nlmOwner", el.Owner.OwnerID,
			"clientID", clientID,
			"wantsWrite", wantsWrite,
			"wantsRead", wantsRead,
			"nlmExclusive", el.IsExclusive())
		return true
	}

	return false
}

// ============================================================================
// Delegation Conflict Translation
// ============================================================================

// DelegationConflictReason describes why a delegation conflicts with a lease.
type DelegationConflictReason struct {
	DelegationType DelegationType
	LeaseState     uint32
	Reason         string
}

// FormatDelegationConflict generates a human-readable description of a
// cross-protocol delegation conflict for logging.
func FormatDelegationConflict(deleg *Delegation, lease *OpLock) string {
	if deleg == nil && lease == nil {
		return "unknown delegation conflict"
	}
	if deleg == nil {
		return fmt.Sprintf("lease (%s) conflicts with unknown delegation",
			LeaseStateToString(lease.LeaseState))
	}
	if lease == nil {
		return fmt.Sprintf("%s delegation conflicts with unknown lease",
			deleg.DelegType.String())
	}
	return fmt.Sprintf("%s delegation (client=%s) conflicts with lease (%s)",
		deleg.DelegType.String(), extractClientID(deleg.ClientID),
		LeaseStateToString(lease.LeaseState))
}

// ============================================================================
// Helper Functions
// ============================================================================

// extractClientID extracts a client identifier from an owner ID string.
// NLM format: "nlm:{caller_name}:{svid}:{oh_hex}" -> "nlm:{caller_name}"
// SMB format: "smb:{clientID}" -> returned as-is
// Other formats: returned as-is
func extractClientID(ownerID string) string {
	if ownerID == "" {
		return "unknown"
	}

	if strings.HasPrefix(ownerID, "nlm:") {
		parts := strings.SplitN(ownerID, ":", 4)
		if len(parts) >= 2 {
			return "nlm:" + parts[1]
		}
	}

	return ownerID
}

// parseNLMOwnerID parses an NLM owner ID ("nlm:{caller_name}:{svid}:{oh_hex}")
// into its components. Returns defaults for non-NLM or incomplete formats.
func parseNLMOwnerID(ownerID string) (callerName string, svid int32, oh []byte) {
	if !strings.HasPrefix(ownerID, "nlm:") {
		return extractClientID(ownerID), 0, nil
	}

	parts := strings.SplitN(ownerID, ":", 4)
	if len(parts) < 4 {
		if len(parts) >= 2 {
			callerName = parts[1]
		} else {
			callerName = ownerID
		}
		return callerName, 0, nil
	}

	callerName = parts[1]
	_, _ = fmt.Sscanf(parts[2], "%d", &svid)
	oh, _ = hex.DecodeString(parts[3])

	return callerName, svid, oh
}
