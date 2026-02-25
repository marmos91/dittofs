// Package lock provides lock management types and operations for the metadata package.
// This package handles byte-range locking, deadlock detection, and lock persistence.
//
// Import graph: errors <- lock <- metadata <- store implementations
package lock

import (
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// File Handle Type
// ============================================================================

// FileHandle represents an opaque file handle.
// This is defined here to avoid circular imports with the metadata package.
// The metadata package also defines FileHandle as []byte.
type FileHandle string

// ============================================================================
// Enhanced Lock Types for Multi-Protocol Support
// ============================================================================

// LockType represents the type of lock (shared or exclusive).
type LockType int

const (
	// LockTypeShared is a shared (read) lock - multiple readers allowed.
	LockTypeShared LockType = iota

	// LockTypeExclusive is an exclusive (write) lock - no other locks allowed.
	LockTypeExclusive
)

// String returns a human-readable name for the lock type.
func (lt LockType) String() string {
	switch lt {
	case LockTypeShared:
		return "shared"
	case LockTypeExclusive:
		return "exclusive"
	default:
		return "unknown"
	}
}

// AccessMode represents SMB share mode reservations.
// These control what other clients can do while the file is open.
// NFS protocols ignore this field.
type AccessMode int

const (
	// AccessModeNone allows all operations by other clients (default).
	AccessModeNone AccessMode = iota

	// AccessModeDenyRead prevents other clients from reading.
	AccessModeDenyRead

	// AccessModeDenyWrite prevents other clients from writing.
	AccessModeDenyWrite

	// AccessModeDenyAll prevents other clients from reading or writing.
	AccessModeDenyAll
)

// String returns a human-readable name for the share reservation.
func (sr AccessMode) String() string {
	switch sr {
	case AccessModeNone:
		return "none"
	case AccessModeDenyRead:
		return "deny-read"
	case AccessModeDenyWrite:
		return "deny-write"
	case AccessModeDenyAll:
		return "deny-all"
	default:
		return "unknown"
	}
}

// LockOwner identifies the owner of a lock in a protocol-agnostic way.
//
// The OwnerID field is an opaque string that the lock manager does NOT parse.
// Different protocols encode their identity information differently:
//   - NLM:  "nlm:client1:pid123"
//   - SMB:  "smb:session456:pid789"
//   - NFSv4: "nfs4:clientid:stateid"
//
// This enables cross-protocol lock conflict detection (LOCK-04):
// if an NLM client and SMB client both request exclusive locks on the same
// range, they will correctly conflict because the OwnerIDs are different.
type LockOwner struct {
	// OwnerID is the protocol-provided owner identifier.
	// Format: "{protocol}:{details}" - treated as OPAQUE by lock manager.
	// The lock manager never parses this string; it only compares for equality.
	OwnerID string

	// ClientID is the connection tracker client ID.
	// Used to clean up locks when a client disconnects.
	ClientID string

	// ShareName is the share this lock belongs to.
	// Used for per-share lock tracking and cleanup.
	ShareName string
}

// UnifiedLock represents a byte-range lock or SMB lease with full protocol support.
//
// This extends the basic FileLock concept to support:
//   - Protocol-agnostic ownership (NLM, SMB, NFSv4)
//   - SMB share reservations
//   - SMB2/3 leases (R/W/H caching via Lease field)
//   - Reclaim tracking for grace periods
//   - Lock identification for management
//
// Lock Lifecycle:
//  1. Client requests lock via protocol handler
//  2. Lock manager checks for conflicts using OwnerID comparison
//  3. If no conflict, lock is acquired with unique ID
//  4. Lock persists until: explicitly released, file closed, session ends, or server restarts
//
// Lease vs Byte-Range Lock:
//   - Byte-range locks: Offset/Length define locked range, Lease is nil
//   - Leases: Whole-file (Offset=0, Length=0), Lease contains R/W/H state
//   - Use IsLease() to distinguish between the two
//
// Cross-Protocol Behavior:
// All protocols share the same lock namespace. An NLM lock on bytes 0-100
// will conflict with an SMB lock request for the same range, enabling
// unified locking across protocols. Leases also participate in cross-protocol
// conflict detection (e.g., NFS write triggers SMB Write lease break).
type UnifiedLock struct {
	// ID is a unique identifier for this lock (UUID).
	// Used for lock management, debugging, and metrics.
	ID string

	// Owner identifies who holds the lock.
	Owner LockOwner

	// FileHandle is the file this lock is on.
	// This is the store-specific file handle.
	FileHandle FileHandle

	// Offset is the starting byte offset of the lock.
	// For leases, this is always 0 (whole-file).
	Offset uint64

	// Length is the number of bytes locked.
	// 0 means "to end of file" (unbounded).
	// For leases, this is always 0 (whole-file).
	Length uint64

	// Type indicates whether this is a shared or exclusive lock.
	// For leases, this reflects the lease type:
	//   - LockTypeShared for Read-only leases
	//   - LockTypeExclusive for Write-containing leases
	Type LockType

	// AccessMode is the SMB share mode (NFS protocols ignore this).
	AccessMode AccessMode

	// AcquiredAt is when the lock was acquired.
	AcquiredAt time.Time

	// Blocking indicates whether this was a blocking (wait) request.
	// Non-blocking requests fail immediately on conflict.
	Blocking bool

	// Reclaim indicates whether this is a reclaim during grace period.
	// Reclaim locks have priority over new locks during grace period.
	Reclaim bool

	// Lease holds lease-specific state for SMB2/3 leases.
	// Nil for byte-range locks; non-nil for leases.
	// When non-nil, Offset=0 and Length=0 (whole-file).
	Lease *OpLock
}

// NewUnifiedLock creates a new UnifiedLock with a generated UUID.
func NewUnifiedLock(owner LockOwner, fileHandle FileHandle, offset, length uint64, lockType LockType) *UnifiedLock {
	return &UnifiedLock{
		ID:         uuid.New().String(),
		Owner:      owner,
		FileHandle: fileHandle,
		Offset:     offset,
		Length:     length,
		Type:       lockType,
		AcquiredAt: time.Now(),
	}
}

// IsExclusive returns true if this is an exclusive (write) lock.
func (el *UnifiedLock) IsExclusive() bool {
	return el.Type == LockTypeExclusive
}

// IsShared returns true if this is a shared (read) lock.
func (el *UnifiedLock) IsShared() bool {
	return el.Type == LockTypeShared
}

// End returns the end offset of the lock (exclusive).
// Returns 0 for unbounded locks (Length=0 means to EOF).
func (el *UnifiedLock) End() uint64 {
	if el.Length == 0 {
		return 0 // Unbounded
	}
	return el.Offset + el.Length
}

// Contains returns true if this lock fully contains the specified range.
func (el *UnifiedLock) Contains(offset, length uint64) bool {
	// Unbounded lock contains everything at or after its offset
	if el.Length == 0 {
		return offset >= el.Offset
	}

	// Bounded lock
	if length == 0 {
		// Unbounded query range - bounded lock can't contain it
		return false
	}

	// Both bounded - check containment
	return offset >= el.Offset && (offset+length) <= el.End()
}

// Overlaps returns true if this lock overlaps with the specified range.
func (el *UnifiedLock) Overlaps(offset, length uint64) bool {
	return RangesOverlap(el.Offset, el.Length, offset, length)
}

// Clone creates a deep copy of the lock.
func (el *UnifiedLock) Clone() *UnifiedLock {
	clone := &UnifiedLock{
		ID:               el.ID,
		Owner:            el.Owner,
		FileHandle:       el.FileHandle,
		Offset:           el.Offset,
		Length:           el.Length,
		Type:             el.Type,
		AccessMode: el.AccessMode,
		AcquiredAt:       el.AcquiredAt,
		Blocking:         el.Blocking,
		Reclaim:          el.Reclaim,
	}
	// Deep copy Lease if present
	if el.Lease != nil {
		clone.Lease = el.Lease.Clone()
	}
	return clone
}

// IsLease returns true if this is an SMB2/3 lease rather than a byte-range lock.
// Leases have the Lease field set and are whole-file (Offset=0, Length=0).
func (el *UnifiedLock) IsLease() bool {
	return el.Lease != nil
}

// ============================================================================
// Enhanced Lock Conflict Detection
// ============================================================================

// UnifiedLockConflict describes a conflicting lock for error reporting.
type UnifiedLockConflict struct {
	// Lock is the conflicting lock.
	Lock *UnifiedLock

	// Reason describes why the conflict occurred.
	Reason string
}

// IsUnifiedLockConflicting checks if two enhanced locks conflict with each other.
//
// This function handles three cases:
//  1. Lease vs Lease: Check lease-specific conflict rules
//  2. Lease vs Byte-Range Lock: Cross-type conflict detection
//  3. Byte-Range Lock vs Byte-Range Lock: Traditional range overlap + type check
//
// Conflict rules for byte-range locks:
//   - Shared locks don't conflict with other shared locks (multiple readers)
//   - Exclusive locks conflict with all other locks
//   - Locks from the same owner don't conflict (allows re-locking same range)
//   - Ranges must overlap for a conflict to occur
//
// Conflict rules for leases:
//   - Same LeaseKey = no conflict (same caching unit)
//   - Write leases require exclusive access (conflict with other leases)
//   - Read leases can coexist
//
// Cross-type conflict rules (lease vs byte-range):
//   - Lease with Write conflicts with exclusive byte-range locks
//   - Exclusive byte-range lock conflicts with Write leases
//
// Note: Owner comparison uses the full OwnerID string, enabling cross-protocol
// conflict detection. An NLM lock and SMB lock on the same range WILL conflict
// because they have different OwnerIDs.
func IsUnifiedLockConflicting(existing, requested *UnifiedLock) bool {
	// Same owner - no conflict (allows re-locking same range with different type)
	if existing.Owner.OwnerID == requested.Owner.OwnerID {
		return false
	}

	// Handle lease-specific conflict detection
	existingIsLease := existing.IsLease()
	requestedIsLease := requested.IsLease()

	// Case 1: Both are leases
	if existingIsLease && requestedIsLease {
		return OpLocksConflict(existing.Lease, requested.Lease)
	}

	// Case 2: One is a lease, one is a byte-range lock
	if existingIsLease && !requestedIsLease {
		// Existing is lease, requested is byte-range lock
		return opLockConflictsWithByteLock(existing.Lease, existing.Owner.OwnerID, requested)
	}
	if !existingIsLease && requestedIsLease {
		// Existing is byte-range lock, requested is lease
		return opLockConflictsWithByteLock(requested.Lease, requested.Owner.OwnerID, existing)
	}

	// Case 3: Both are byte-range locks (original logic)
	// Check range overlap first (common case: no overlap)
	if !RangesOverlap(existing.Offset, existing.Length, requested.Offset, requested.Length) {
		return false
	}

	// Both shared (read) locks - no conflict
	if existing.Type == LockTypeShared && requested.Type == LockTypeShared {
		return false
	}

	// At least one is exclusive and ranges overlap - conflict
	return true
}

// ============================================================================
// Range Helper Functions
// ============================================================================

// RangesOverlap returns true if two byte ranges overlap.
// Length of 0 means "to end of file" (unbounded).
func RangesOverlap(offset1, length1, offset2, length2 uint64) bool {
	// Calculate end positions
	// For length=0 (unbounded), we use max uint64 to represent "infinity"
	var end1, end2 uint64

	if length1 == 0 {
		end1 = ^uint64(0) // Max uint64 (infinity)
	} else {
		end1 = offset1 + length1
	}

	if length2 == 0 {
		end2 = ^uint64(0) // Max uint64 (infinity)
	} else {
		end2 = offset2 + length2
	}

	// Ranges overlap if each starts before the other ends
	return end1 > offset2 && end2 > offset1
}

// ============================================================================
// Lock Result
// ============================================================================

// LockResult represents the result of a lock operation.
type LockResult struct {
	// Success indicates whether the lock was acquired.
	Success bool

	// Lock is the acquired lock (nil if !Success).
	Lock *UnifiedLock

	// Conflict is the conflicting lock information (nil if Success).
	Conflict *UnifiedLockConflict

	// ShouldWait indicates whether the caller should wait and retry.
	// True when a blocking request found a conflict.
	ShouldWait bool

	// WaitFor is the list of owner IDs to wait for (for deadlock detection).
	WaitFor []string
}
