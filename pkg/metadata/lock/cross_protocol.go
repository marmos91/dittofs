// Package lock provides cross-protocol translation helpers for lock visibility.
//
// This file contains helpers for translating lock information between protocols
// (NLM and SMB), enabling cross-protocol conflict reporting and logging.
//
// Use Cases:
//   - When NLM TEST/LOCK fails due to SMB lease, translate lease to NLM holder info
//   - When SMB lock/lease fails due to NLM lock, generate human-readable reason
//   - Cross-protocol conflict logging at INFO level
package lock

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// ============================================================================
// NLM Holder Info Translation
// ============================================================================

// NLMHolderInfo represents lock holder information in NLM-compatible format.
//
// This struct is used to construct NLM4_DENIED responses when an NFS client
// attempts to acquire a lock that conflicts with an SMB lease. The NLM protocol
// expects specific fields to identify the lock holder.
//
// Cross-Protocol Semantics (per CONTEXT.md):
//   - CallerName: "smb:<clientID>" identifies the SMB client
//   - Svid: 0 (SMB has no concept of process ID like Unix)
//   - OH: First 8 bytes of LeaseKey (owner handle)
//   - Offset: 0 (leases are whole-file)
//   - Length: ^uint64(0) (max value, meaning whole file)
//   - Exclusive: true if lease has Write caching permission
type NLMHolderInfo struct {
	// CallerName identifies the lock holder.
	// For SMB leases: "smb:<clientID>"
	// For NLM locks: Original caller_name from lock request
	CallerName string

	// Svid is the server-unique identifier (process ID in Unix terms).
	// For SMB leases: Always 0 (SMB has no process ID concept)
	// For NLM locks: Original svid from lock request
	Svid int32

	// OH is the owner handle (opaque identifier).
	// For SMB leases: First 8 bytes of the 128-bit LeaseKey
	// For NLM locks: Original oh from lock request
	OH []byte

	// Offset is the starting byte offset of the lock.
	// For SMB leases: Always 0 (whole file)
	// For NLM locks: Original offset from lock
	Offset uint64

	// Length is the number of bytes locked.
	// For SMB leases: ^uint64(0) (max value = whole file)
	// For NLM locks: Original length from lock
	Length uint64

	// Exclusive indicates if this is an exclusive (write) lock.
	// For SMB leases: true if lease has Write caching permission
	// For NLM locks: true if exclusive lock type
	Exclusive bool
}

// TranslateToNLMHolder converts an SMB lease to NLM holder format.
//
// This is used to construct NLM4_DENIED responses when an NFS client's
// lock request conflicts with an SMB lease. The translation follows
// the semantics defined in CONTEXT.md.
//
// Parameters:
//   - lease: The EnhancedLock representing an SMB lease (must have Lease != nil)
//
// Returns:
//   - NLMHolderInfo: NLM-compatible holder information
//
// Panics:
//   - If lease is nil or lease.Lease is nil (not a valid lease)
//
// Example:
//
//	lease := getConflictingLease(...)
//	holderInfo := TranslateToNLMHolder(lease)
//	// Use holderInfo fields in NLM4_DENIED response
func TranslateToNLMHolder(lease *EnhancedLock) NLMHolderInfo {
	if lease == nil || lease.Lease == nil {
		panic("TranslateToNLMHolder called with non-lease lock")
	}

	// Extract client ID from owner ID
	// Owner format: "smb:<clientID>" or other protocol-specific format
	callerName := extractClientID(lease.Owner.OwnerID)

	// Use first 8 bytes of 16-byte LeaseKey as owner handle
	oh := make([]byte, 8)
	copy(oh, lease.Lease.LeaseKey[:8])

	return NLMHolderInfo{
		CallerName: callerName,
		Svid:       0,           // SMB has no process ID concept
		OH:         oh,          // First 8 bytes of LeaseKey
		Offset:     0,           // Leases are whole-file
		Length:     ^uint64(0),  // Max uint64 = whole file
		Exclusive:  lease.Lease.HasWrite(),
	}
}

// TranslateByteRangeLockToNLMHolder converts a byte-range lock to NLM holder format.
//
// This is used when an NFS client's lock request conflicts with another
// byte-range lock (from NLM or SMB). The translation preserves the original
// lock's range information.
//
// Parameters:
//   - lock: The EnhancedLock representing a byte-range lock (Lease == nil)
//
// Returns:
//   - NLMHolderInfo: NLM-compatible holder information
func TranslateByteRangeLockToNLMHolder(lock *EnhancedLock) NLMHolderInfo {
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
// due to an NLM lock conflict.
//
// This is used for INFO-level logging when an SMB operation is denied due
// to an existing NLM byte-range lock. Per CONTEXT.md, cross-protocol conflicts
// are logged at INFO level since they're working as designed.
//
// Parameters:
//   - lock: The conflicting NLM byte-range lock
//
// Returns:
//   - string: Human-readable conflict reason for logging
//
// Example output:
//
//	"NFS client 'host1' holds exclusive lock on bytes 0-1024"
//	"NFS client 'host1' holds shared lock on entire file"
func TranslateSMBConflictReason(lock *EnhancedLock) string {
	if lock == nil {
		return "unknown conflict"
	}

	// Extract client name from owner ID
	clientName := extractClientID(lock.Owner.OwnerID)

	// Format lock type
	lockType := "shared"
	if lock.IsExclusive() {
		lockType = "exclusive"
	}

	// Format range
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
// due to an SMB lease conflict.
//
// This is used for INFO-level logging when an NFS operation is denied due
// to an existing SMB lease.
//
// Parameters:
//   - lease: The conflicting SMB lease
//
// Returns:
//   - string: Human-readable conflict reason for logging
//
// Example output:
//
//	"SMB client 'client1' holds Write lease (RW)"
//	"SMB client 'client1' holds Handle lease (RH)"
func TranslateNFSConflictReason(lease *EnhancedLock) string {
	if lease == nil || lease.Lease == nil {
		return "unknown SMB lease conflict"
	}

	// Extract client name from owner ID
	clientName := extractClientID(lease.Owner.OwnerID)

	// Describe the lease type
	leaseType := "Read"
	if lease.Lease.HasWrite() {
		leaseType = "Write"
	} else if lease.Lease.HasHandle() {
		leaseType = "Handle"
	}

	// Get full state string
	stateStr := lease.Lease.StateString()

	return fmt.Sprintf("SMB client '%s' holds %s lease (%s)", clientName, leaseType, stateStr)
}

// ============================================================================
// Helper Functions
// ============================================================================

// extractClientID extracts a client identifier from an owner ID string.
//
// Owner ID formats:
//   - NLM: "nlm:{caller_name}:{svid}:{oh_hex}" -> returns "nlm:{caller_name}"
//   - SMB: "smb:{clientID}" -> returns "smb:{clientID}"
//   - Other: Returns full owner ID
func extractClientID(ownerID string) string {
	if ownerID == "" {
		return "unknown"
	}

	// For NLM format, extract protocol:caller_name
	if strings.HasPrefix(ownerID, "nlm:") {
		parts := strings.SplitN(ownerID, ":", 4)
		if len(parts) >= 2 {
			return "nlm:" + parts[1]
		}
	}

	// For SMB format, return as-is (already "smb:{clientID}")
	if strings.HasPrefix(ownerID, "smb:") {
		return ownerID
	}

	// For other formats, return full owner ID
	return ownerID
}

// parseNLMOwnerID parses an NLM owner ID into its components.
//
// NLM owner ID format: "nlm:{caller_name}:{svid}:{oh_hex}"
//
// Returns:
//   - callerName: The original caller_name
//   - svid: The server-unique identifier (process ID)
//   - oh: The owner handle bytes
func parseNLMOwnerID(ownerID string) (callerName string, svid int32, oh []byte) {
	if !strings.HasPrefix(ownerID, "nlm:") {
		// Not NLM format - return defaults
		return extractClientID(ownerID), 0, nil
	}

	parts := strings.SplitN(ownerID, ":", 4)
	if len(parts) < 4 {
		// Incomplete NLM format
		if len(parts) >= 2 {
			callerName = parts[1]
		} else {
			callerName = ownerID
		}
		return callerName, 0, nil
	}

	callerName = parts[1]

	// Parse svid
	var svidVal int64
	fmt.Sscanf(parts[2], "%d", &svidVal)
	svid = int32(svidVal)

	// Parse oh (hex encoded)
	oh, _ = hex.DecodeString(parts[3])

	return callerName, svid, oh
}
