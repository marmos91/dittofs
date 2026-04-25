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

// Lease state constants per MS-SMB2 2.2.13.2.8.
const (
	// LeaseStateNone indicates no caching is permitted.
	LeaseStateNone uint32 = 0x00

	// LeaseStateRead (SMB2_LEASE_READ_CACHING) permits caching reads.
	// Multiple clients can hold Read leases simultaneously.
	LeaseStateRead uint32 = 0x01

	// LeaseStateHandle (SMB2_LEASE_HANDLE_CACHING) permits caching open handles.
	// Client can delay close operations until another client needs access.
	LeaseStateHandle uint32 = 0x02

	// LeaseStateWrite (SMB2_LEASE_WRITE_CACHING) permits caching writes.
	// Only one client can hold a Write lease; requires exclusive access.
	// Client with Write lease has dirty data that must be flushed on break.
	LeaseStateWrite uint32 = 0x04
)

// Sentinel values for breakOpLocks break-to state computation.
const (
	// BreakToStripWrite indicates the break-to state should be computed
	// per-lease by stripping the Write bit (preserving Read and Handle).
	// Per MS-SMB2 3.3.5.9: RWH -> RH, RW -> R.
	BreakToStripWrite uint32 = 0xFFFFFFFF

	// BreakToStripHandle indicates the break-to state should be computed
	// per-lease by stripping the Handle bit (preserving Read and Write).
	// Per MS-SMB2 3.3.5.9 Step 10: RWH -> RW, RH -> R.
	BreakToStripHandle uint32 = 0xFFFFFFFE
)

// BreakReason classifies why an existing lease must be broken so
// ComputeLeaseBreakTo can pick the correct target state per MS-SMB2 3.3.4.7
// (and Samba `source3/smbd/open.c::delay_for_oplock_fn`).
type BreakReason uint8

const (
	// BreakReasonDefault: a non-conflicting open that just needs the existing
	// holder to flush dirty data. Strip Write, keep Read + Handle.
	// (RWH→RH, RW→R, RH/R→unchanged)
	BreakReasonDefault BreakReason = iota

	// BreakReasonSharingViolation: the new open conflicts on share mode, so
	// the existing holder must release its cached handles. Strip Handle, keep
	// Read + Write. (RWH→RW, RH→R, RW/R→unchanged)
	BreakReasonSharingViolation

	// BreakReasonDestructive: the new opener will replace existing content
	// (FILE_OVERWRITE / FILE_OVERWRITE_IF / FILE_SUPERSEDE), so cached data
	// and handles are both invalid. Break to None.
	// (any state → None)
	BreakReasonDestructive
)

// ComputeLeaseBreakTo returns the new lease state for a break triggered by a
// conflicting SMB open, given why the conflict arose.
//
// Examples:
//
//	existing=RWH, reason=Default          → RH  (strip W)
//	existing=RWH, reason=SharingViolation → RW  (strip H)
//	existing=RWH, reason=Destructive      → None
//	existing=RH,  reason=SharingViolation → R   (strip H)
//	existing=RW,  reason=Default          → R   (strip W)
//	existing=R,   reason=Default          → R   (no-op, nothing to strip)
func ComputeLeaseBreakTo(existingState uint32, reason BreakReason) uint32 {
	switch reason {
	case BreakReasonDestructive:
		return LeaseStateNone
	case BreakReasonSharingViolation:
		return existingState &^ LeaseStateHandle
	default:
		return existingState &^ LeaseStateWrite
	}
}

// breakSentinelForReason returns the breakOpLocks sentinel value that pairs
// with reason. Kept next to ComputeLeaseBreakTo so the two stay in sync —
// a lease is broken iff ComputeLeaseBreakTo returns a different state, and
// the sentinel selects how breakOpLocks computes the per-lease target.
func breakSentinelForReason(reason BreakReason) uint32 {
	switch reason {
	case BreakReasonDestructive:
		return LeaseStateNone
	case BreakReasonSharingViolation:
		return BreakToStripHandle
	default:
		return BreakToStripWrite
	}
}

// ValidFileLeaseStates contains all valid lease state combinations for files.
// Per MS-SMB2: Write and Handle alone are not valid; they require Read.
// Valid combinations: None, R, RH, RW, RWH
var ValidFileLeaseStates = []uint32{
	LeaseStateNone,                                      // 0x00 - No caching
	LeaseStateRead,                                      // 0x01 - Read only
	LeaseStateRead | LeaseStateHandle,                   // 0x03 - Read + Handle
	LeaseStateRead | LeaseStateWrite,                    // 0x05 - Read + Write
	LeaseStateRead | LeaseStateWrite | LeaseStateHandle, // 0x07 - Full (RWH)
}

// ValidDirectoryLeaseStates contains valid lease state combinations for directories.
// Per MS-SMB2 3.3.5.9: directories support Read and Handle caching but NOT
// Write caching. Write caching requires exclusive access semantics that don't
// apply to directory opens.
// When RWH is requested, bestGrantableState will downgrade to RH.
// When RW is requested, bestGrantableState will downgrade to R.
// Valid combinations: None, R, RH
var ValidDirectoryLeaseStates = []uint32{
	LeaseStateNone,                    // 0x00 - No caching
	LeaseStateRead,                    // 0x01 - Read only
	LeaseStateRead | LeaseStateHandle, // 0x03 - Read + Handle
}

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

	// BreakToState is the target state of the IN-FLIGHT break notification
	// (Samba `breaking_to_requested`). Zero if no break is in progress. Used
	// to validate ACKs: an ACK that claims state outside this mask is rejected
	// with STATUS_REQUEST_NOT_ACCEPTED. May be a multi-stage intermediate
	// (e.g. RH→R while the cumulative final target is None).
	BreakToState uint32

	// BreakingToRequired is the cumulative FINAL break-to target across all
	// conflicting opens that arrived while this lease was Breaking (Samba
	// `breaking_to_required`, source3/smbd/smb2_oplock.c). May be stricter
	// (smaller bitmask) than BreakToState. On each ACK, if LeaseState still
	// has bits beyond BreakingToRequired, the next progressive stage is
	// dispatched via nextProgressiveBreakTarget. Zero only when there is no
	// active break or when the target is full release.
	BreakingToRequired uint32

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

	// ParentLeaseKey is the V2 parent lease key for cache tree correlation.
	// Used by SMB2.1+ Lease V2 to associate directory and file leases into
	// a hierarchical caching tree, enabling directory lease breaks when
	// child entries change.
	// Reference: MS-SMB2 2.2.13.2.10
	ParentLeaseKey [16]byte

	// IsDirectory indicates this lease is on a directory.
	// When true, valid lease states are restricted to ValidDirectoryLeaseStates
	// (None, R, RH).
	IsDirectory bool
}

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
// Invalid: W alone, H alone, WH, RW, RWH (Write not valid for directories)
func IsValidDirectoryLeaseState(state uint32) bool {
	return slices.Contains(ValidDirectoryLeaseStates, state)
}

// Clone creates a deep copy of the OpLock.
func (l *OpLock) Clone() *OpLock {
	if l == nil {
		return nil
	}
	return &OpLock{
		LeaseKey:           l.LeaseKey, // Fixed-size array, copied by value
		LeaseState:         l.LeaseState,
		BreakToState:       l.BreakToState,
		BreakingToRequired: l.BreakingToRequired,
		Breaking:           l.Breaking,
		Epoch:              l.Epoch,
		BreakStarted:       l.BreakStarted,
		Reclaim:            l.Reclaim,
		ParentLeaseKey:     l.ParentLeaseKey, // Fixed-size array, copied by value
		IsDirectory:        l.IsDirectory,
	}
}

// nextProgressiveBreakTarget computes the next break-to target after a client
// has acknowledged a partial break. Mirrors Samba's behavior in
// source3/smbd/smb2_oplock.c::downgrade_lease lines 569-586: when the
// acknowledged state still has W or H, the next stage keeps R as an
// intermediate; otherwise drop straight to required.
//
// Produces the wire sequence smbtorture asserts for breaking3/v2_breaking3:
//
//	ack RWH→RH (has H)  → next = required(0) | R = R   ⇒ wire: RH→R
//	ack RH→R   (no W/H) → next = required(0)            ⇒ wire: R→""
func nextProgressiveBreakTarget(ackedState, required uint32) uint32 {
	next := required
	if ackedState&(LeaseStateWrite|LeaseStateHandle) != 0 {
		next |= LeaseStateRead
	}
	return next
}

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

	// If existing lease is breaking, treat as having its cumulative final
	// target (BreakingToRequired) rather than the in-flight intermediate
	// (BreakToState). Otherwise a same-handle reopen arriving mid-stage
	// could re-dispatch a redundant strip-W against an RH that is heading
	// to None.
	existingState := existing.LeaseState
	if existing.Breaking {
		existingState = existing.BreakingToRequired
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
