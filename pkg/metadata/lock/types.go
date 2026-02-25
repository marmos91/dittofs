// Package lock provides lock management types and operations for the metadata package.
// This package handles byte-range locking, deadlock detection, and lock persistence.
//
// Import graph: errors <- lock <- metadata <- store implementations
package lock

import (
	"time"

	"github.com/google/uuid"
)

// FileHandle represents an opaque file handle.
// This is defined here to avoid circular imports with the metadata package.
// The metadata package also defines FileHandle as []byte.
type FileHandle string

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
func (ul *UnifiedLock) IsExclusive() bool {
	return ul.Type == LockTypeExclusive
}

// IsShared returns true if this is a shared (read) lock.
func (ul *UnifiedLock) IsShared() bool {
	return ul.Type == LockTypeShared
}

// End returns the end offset of the lock (exclusive).
// Returns 0 for unbounded locks (Length=0 means to EOF).
func (ul *UnifiedLock) End() uint64 {
	if ul.Length == 0 {
		return 0 // Unbounded
	}
	return ul.Offset + ul.Length
}

// Contains returns true if this lock fully contains the specified range.
func (ul *UnifiedLock) Contains(offset, length uint64) bool {
	// Unbounded lock contains everything at or after its offset
	if ul.Length == 0 {
		return offset >= ul.Offset
	}

	// Bounded lock
	if length == 0 {
		// Unbounded query range - bounded lock can't contain it
		return false
	}

	// Both bounded - check containment
	return offset >= ul.Offset && (offset+length) <= ul.End()
}

// Overlaps returns true if this lock overlaps with the specified range.
func (ul *UnifiedLock) Overlaps(offset, length uint64) bool {
	return RangesOverlap(ul.Offset, ul.Length, offset, length)
}

// Clone creates a deep copy of the lock.
func (ul *UnifiedLock) Clone() *UnifiedLock {
	clone := &UnifiedLock{
		ID:         ul.ID,
		Owner:      ul.Owner,
		FileHandle: ul.FileHandle,
		Offset:     ul.Offset,
		Length:     ul.Length,
		Type:       ul.Type,
		AccessMode: ul.AccessMode,
		AcquiredAt: ul.AcquiredAt,
		Blocking:   ul.Blocking,
		Reclaim:    ul.Reclaim,
	}
	if ul.Lease != nil {
		clone.Lease = ul.Lease.Clone()
	}
	return clone
}

// IsLease returns true if this is an SMB2/3 lease rather than a byte-range lock.
// Leases have the Lease field set and are whole-file (Offset=0, Length=0).
func (ul *UnifiedLock) IsLease() bool {
	return ul.Lease != nil
}

// ConflictsWith checks if this lock conflicts with another lock.
//
// This method handles all 4 conflict cases:
//  1. Access mode conflicts (SMB deny modes)
//  2. OpLock vs OpLock (lease-to-lease conflicts)
//  3. OpLock vs byte-range (cross-type conflicts)
//  4. Byte-range vs byte-range (traditional range overlap + type check)
//
// Same owner never conflicts (allows re-locking and upgrading).
//
// Returns true if the locks conflict and one must be denied or broken.
func (ul *UnifiedLock) ConflictsWith(other *UnifiedLock) bool {
	// Same owner = no conflict
	if ul.Owner.OwnerID == other.Owner.OwnerID {
		return false
	}

	// Case 1: Access mode conflicts (SMB share modes)
	if accessModesConflict(ul.AccessMode, other.AccessMode) {
		return true
	}

	// Case 2: OpLock vs OpLock
	if ul.IsLease() && other.IsLease() {
		return OpLocksConflict(ul.Lease, other.Lease)
	}

	// Case 3: OpLock vs byte-range (one has oplock, other doesn't)
	if ul.IsLease() != other.IsLease() {
		if ul.IsLease() {
			return opLockConflictsWithByteLock(ul.Lease, ul.Owner.OwnerID, other)
		}
		return opLockConflictsWithByteLock(other.Lease, other.Owner.OwnerID, ul)
	}

	// Case 4: Byte-range vs byte-range
	if !RangesOverlap(ul.Offset, ul.Length, other.Offset, other.Length) {
		return false
	}
	return ul.Type != LockTypeShared || other.Type != LockTypeShared
}

// accessModesConflict checks if two access modes (SMB share reservations) conflict.
//
// AccessModeNone never conflicts with anything. Any deny mode (DenyRead,
// DenyWrite, DenyAll) conflicts with any non-None mode on the other side.
// The check is symmetric: if a denies and b is non-None, or vice versa.
func accessModesConflict(a, b AccessMode) bool {
	return a != AccessModeNone && b != AccessModeNone
}

// UnifiedLockConflict describes a conflicting lock for error reporting.
type UnifiedLockConflict struct {
	// Lock is the conflicting lock.
	Lock *UnifiedLock

	// Reason describes why the conflict occurred.
	Reason string
}

// IsUnifiedLockConflicting checks if two unified locks conflict with each other.
// It delegates to ConflictsWith which handles all conflict cases.
//
// This standalone function is kept for backward compatibility and convenience
// in code that operates on two locks without a clear "this vs other" relationship.
func IsUnifiedLockConflicting(existing, requested *UnifiedLock) bool {
	return existing.ConflictsWith(requested)
}

// RangesOverlap returns true if two byte ranges overlap.
// Length of 0 means "to end of file" (unbounded).
func RangesOverlap(offset1, length1, offset2, length2 uint64) bool {
	end1 := rangeEnd(offset1, length1)
	end2 := rangeEnd(offset2, length2)
	return end1 > offset2 && end2 > offset1
}

// rangeEnd returns the exclusive end of a byte range.
// For unbounded ranges (length=0), returns max uint64 to represent infinity.
func rangeEnd(offset, length uint64) uint64 {
	if length == 0 {
		return ^uint64(0)
	}
	return offset + length
}

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
