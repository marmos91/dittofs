// Package lock provides lock management types and operations for the metadata package.
// This file contains SMB2/3 lease types integrated with the unified lock manager.
//
// SMB2.1+ leases provide client caching permissions using Read/Write/Handle flags.
// Leases are whole-file (not byte-range) and use a 128-bit client-generated key
// to group multiple file handles into a single caching unit.
//
// Reference: MS-SMB2 2.2.13.2.8 SMB2_CREATE_REQUEST_LEASE_V2
package lock

import (
	"fmt"
	"slices"
	"time"
)

// ============================================================================
// Lease State Constants (MS-SMB2 2.2.13.2.8)
// ============================================================================

const (
	// LeaseStateNone indicates no caching is permitted.
	LeaseStateNone uint32 = 0x00

	// LeaseStateRead (SMB2_LEASE_READ_CACHING) permits caching reads.
	// Multiple clients can hold Read leases simultaneously.
	LeaseStateRead uint32 = 0x01

	// LeaseStateWrite (SMB2_LEASE_WRITE_CACHING) permits caching writes.
	// Only one client can hold a Write lease; requires exclusive access.
	// Client with Write lease has dirty data that must be flushed on break.
	LeaseStateWrite uint32 = 0x02

	// LeaseStateHandle (SMB2_LEASE_HANDLE_CACHING) permits caching open handles.
	// Client can delay close operations until another client needs access.
	LeaseStateHandle uint32 = 0x04
)

// ============================================================================
// Valid Lease State Combinations
// ============================================================================

// ValidFileLeaseStates contains all valid lease state combinations for files.
// Per MS-SMB2: Write and Handle alone are not valid; they require Read.
// Valid combinations: None, R, RW, RH, RWH
var ValidFileLeaseStates = []uint32{
	LeaseStateNone,                                      // 0x00 - No caching
	LeaseStateRead,                                      // 0x01 - Read only
	LeaseStateRead | LeaseStateWrite,                    // 0x03 - Read + Write
	LeaseStateRead | LeaseStateHandle,                   // 0x05 - Read + Handle
	LeaseStateRead | LeaseStateWrite | LeaseStateHandle, // 0x07 - Full (RWH)
}

// ValidDirectoryLeaseStates contains valid lease state combinations for directories.
// Directories can only have Read or Read+Handle leases.
// Write leases are not permitted on directories.
var ValidDirectoryLeaseStates = []uint32{
	LeaseStateNone,                    // 0x00 - No caching
	LeaseStateRead,                    // 0x01 - Read only
	LeaseStateRead | LeaseStateHandle, // 0x05 - Read + Handle
}

// ============================================================================
// Lease Info Type
// ============================================================================

// OpLock holds SMB2/3 lease-specific state.
//
// A lease provides caching permissions (R/W/H) to SMB clients. Unlike byte-range
// locks, leases are whole-file and identified by a client-generated 128-bit key.
// Multiple file handles with the same LeaseKey share the lease state.
//
// Lease Break Flow:
//  1. Conflicting operation detected (e.g., NFS write to file with SMB Write lease)
//  2. Server sets Breaking=true, BreakToState=target state
//  3. Server sends LEASE_BREAK_NOTIFICATION to client
//  4. Client flushes dirty data (if Write lease), acknowledges break
//  5. Server updates LeaseState to BreakToState, clears Breaking
//
// Reference: MS-SMB2 3.3.5.9.11 Processing a Lease-Break Acknowledgment
type OpLock struct {
	// LeaseKey is the 128-bit client-generated key identifying this lease.
	// Multiple file handles with the same key share the lease.
	LeaseKey [16]byte

	// LeaseState is the current lease state (R/W/H flags bitwise OR'd).
	// Use HasRead(), HasWrite(), HasHandle() to check individual flags.
	LeaseState uint32

	// BreakToState is the target state during an active break.
	// Zero if no break is in progress.
	BreakToState uint32

	// Breaking indicates a lease break is in progress awaiting acknowledgment.
	// When true, the client has been notified and must acknowledge.
	Breaking bool

	// Epoch is incremented on every lease state change (SMB3).
	// Used by clients to detect stale state notifications.
	Epoch uint16

	// BreakStarted records when the break was initiated.
	// Used to enforce break timeout (force revoke if client doesn't acknowledge).
	BreakStarted time.Time

	// Reclaim indicates this lease was reclaimed during grace period.
	// Set when SMB client reconnects after server restart and successfully
	// reclaims its previously held lease.
	Reclaim bool
}

// ============================================================================
// Lease State Helper Methods
// ============================================================================

// HasRead returns true if the lease includes Read caching permission.
func (l *OpLock) HasRead() bool {
	return l.LeaseState&LeaseStateRead != 0
}

// HasWrite returns true if the lease includes Write caching permission.
func (l *OpLock) HasWrite() bool {
	return l.LeaseState&LeaseStateWrite != 0
}

// HasHandle returns true if the lease includes Handle caching permission.
func (l *OpLock) HasHandle() bool {
	return l.LeaseState&LeaseStateHandle != 0
}

// IsBreaking returns true if a lease break is in progress.
func (l *OpLock) IsBreaking() bool {
	return l.Breaking
}

// StateString returns a human-readable string representation of the lease state.
// Examples: "None", "R", "RW", "RH", "RWH"
func (l *OpLock) StateString() string {
	return LeaseStateToString(l.LeaseState)
}

// LeaseStateToString converts a lease state value to a human-readable string.
func LeaseStateToString(state uint32) string {
	if state == LeaseStateNone {
		return "None"
	}

	result := ""
	if state&LeaseStateRead != 0 {
		result += "R"
	}
	if state&LeaseStateWrite != 0 {
		result += "W"
	}
	if state&LeaseStateHandle != 0 {
		result += "H"
	}

	if result == "" {
		return fmt.Sprintf("Unknown(0x%02x)", state)
	}
	return result
}

// ============================================================================
// Lease State Validation
// ============================================================================

// IsValidFileLeaseState returns true if the state is a valid lease combination for files.
//
// Valid file states: None, R, RW, RH, RWH
// Invalid states: W alone, H alone, WH (Write/Handle without Read)
func IsValidFileLeaseState(state uint32) bool {
	return slices.Contains(ValidFileLeaseStates, state)
}

// IsValidDirectoryLeaseState returns true if the state is a valid lease combination for directories.
//
// Valid directory states: None, R, RH
// Invalid: W, RW, H, WH, RWH (no Write caching for directories)
func IsValidDirectoryLeaseState(state uint32) bool {
	return slices.Contains(ValidDirectoryLeaseStates, state)
}

// ============================================================================
// Lease Clone
// ============================================================================

// Clone creates a deep copy of the OpLock.
func (l *OpLock) Clone() *OpLock {
	if l == nil {
		return nil
	}
	clone := &OpLock{
		LeaseKey:     l.LeaseKey, // Fixed-size array, copied by value
		LeaseState:   l.LeaseState,
		BreakToState: l.BreakToState,
		Breaking:     l.Breaking,
		Epoch:        l.Epoch,
		BreakStarted: l.BreakStarted,
		Reclaim:      l.Reclaim,
	}
	return clone
}

// ============================================================================
// Lease Conflict Detection
// ============================================================================

// OpLocksConflict checks if two leases on the same file conflict.
//
// Conflict rules:
//   - Same LeaseKey = no conflict (same client caching unit)
//   - Different keys with overlapping exclusive states (W) = conflict
//   - Multiple Read leases from different clients = no conflict
//   - Write lease requires exclusive access = conflicts with other leases
//   - Handle lease without Read/Write = no data conflict
//
// Returns true if the leases conflict and one must be broken.
func OpLocksConflict(existing, requested *OpLock) bool {
	// Same lease key - no conflict (same caching unit)
	if existing.LeaseKey == requested.LeaseKey {
		return false
	}

	// If existing lease is breaking, treat as having BreakToState
	existingState := existing.LeaseState
	if existing.Breaking {
		existingState = existing.BreakToState
	}

	// Check Write conflicts
	// Write lease requires exclusive access
	if existingState&LeaseStateWrite != 0 {
		// Existing has Write - conflicts with any other lease (except Handle-only)
		if requested.LeaseState&(LeaseStateRead|LeaseStateWrite) != 0 {
			return true
		}
	}
	if requested.LeaseState&LeaseStateWrite != 0 {
		// Requested wants Write - conflicts with existing Read or Write
		if existingState&(LeaseStateRead|LeaseStateWrite) != 0 {
			return true
		}
	}

	// Read leases can coexist (multiple readers allowed)
	// Handle-only leases don't conflict with data access
	return false
}

// opLockConflictsWithByteLock checks if a lease conflicts with a byte-range lock.
//
// Conflict rules:
//   - Lease with Write conflicts with exclusive byte-range locks from other owners
//   - Exclusive byte-range lock conflicts with Write leases from other owners
//   - Read leases don't conflict with shared byte-range locks
//
// The leaseOwnerID and lockOwnerID are used to determine same-owner (no conflict).
func opLockConflictsWithByteLock(lease *OpLock, leaseOwnerID string, lock *UnifiedLock) bool {
	// Same owner - no conflict (same client)
	if leaseOwnerID == lock.Owner.OwnerID {
		return false
	}

	// A lease with Write conflicts with exclusive byte-range locks (and vice versa).
	// Read leases can coexist with shared byte-range locks.
	return lease.HasWrite() && lock.IsExclusive()
}
