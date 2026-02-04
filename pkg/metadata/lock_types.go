package metadata

import (
	"time"

	"github.com/google/uuid"
)

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

// ShareReservation represents SMB share mode reservations.
// These control what other clients can do while the file is open.
// NFS protocols ignore this field.
type ShareReservation int

const (
	// ShareReservationNone allows all operations by other clients (default).
	ShareReservationNone ShareReservation = iota

	// ShareReservationDenyRead prevents other clients from reading.
	ShareReservationDenyRead

	// ShareReservationDenyWrite prevents other clients from writing.
	ShareReservationDenyWrite

	// ShareReservationDenyAll prevents other clients from reading or writing.
	ShareReservationDenyAll
)

// String returns a human-readable name for the share reservation.
func (sr ShareReservation) String() string {
	switch sr {
	case ShareReservationNone:
		return "none"
	case ShareReservationDenyRead:
		return "deny-read"
	case ShareReservationDenyWrite:
		return "deny-write"
	case ShareReservationDenyAll:
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

// EnhancedLock represents a byte-range lock with full protocol support.
//
// This extends the basic FileLock concept to support:
//   - Protocol-agnostic ownership (NLM, SMB, NFSv4)
//   - SMB share reservations
//   - Reclaim tracking for grace periods
//   - Lock identification for management
//
// Lock Lifecycle:
//  1. Client requests lock via protocol handler
//  2. Lock manager checks for conflicts using OwnerID comparison
//  3. If no conflict, lock is acquired with unique ID
//  4. Lock persists until: explicitly released, file closed, session ends, or server restarts
//
// Cross-Protocol Behavior:
// All protocols share the same lock namespace. An NLM lock on bytes 0-100
// will conflict with an SMB lock request for the same range, enabling
// unified locking across protocols.
type EnhancedLock struct {
	// ID is a unique identifier for this lock (UUID).
	// Used for lock management, debugging, and metrics.
	ID string

	// Owner identifies who holds the lock.
	Owner LockOwner

	// FileHandle is the file this lock is on.
	// This is the store-specific file handle.
	FileHandle FileHandle

	// Offset is the starting byte offset of the lock.
	Offset uint64

	// Length is the number of bytes locked.
	// 0 means "to end of file" (unbounded).
	Length uint64

	// Type indicates whether this is a shared or exclusive lock.
	Type LockType

	// ShareReservation is the SMB share mode (NFS protocols ignore this).
	ShareReservation ShareReservation

	// AcquiredAt is when the lock was acquired.
	AcquiredAt time.Time

	// Blocking indicates whether this was a blocking (wait) request.
	// Non-blocking requests fail immediately on conflict.
	Blocking bool

	// Reclaim indicates whether this is a reclaim during grace period.
	// Reclaim locks have priority over new locks during grace period.
	Reclaim bool
}

// NewEnhancedLock creates a new EnhancedLock with a generated UUID.
func NewEnhancedLock(owner LockOwner, fileHandle FileHandle, offset, length uint64, lockType LockType) *EnhancedLock {
	return &EnhancedLock{
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
func (el *EnhancedLock) IsExclusive() bool {
	return el.Type == LockTypeExclusive
}

// IsShared returns true if this is a shared (read) lock.
func (el *EnhancedLock) IsShared() bool {
	return el.Type == LockTypeShared
}

// End returns the end offset of the lock (exclusive).
// Returns 0 for unbounded locks (Length=0 means to EOF).
func (el *EnhancedLock) End() uint64 {
	if el.Length == 0 {
		return 0 // Unbounded
	}
	return el.Offset + el.Length
}

// Contains returns true if this lock fully contains the specified range.
func (el *EnhancedLock) Contains(offset, length uint64) bool {
	// Unbounded lock contains everything at or after its offset
	if el.Length == 0 {
		if length == 0 {
			return offset >= el.Offset
		}
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
func (el *EnhancedLock) Overlaps(offset, length uint64) bool {
	return RangesOverlap(el.Offset, el.Length, offset, length)
}

// Clone creates a deep copy of the lock.
func (el *EnhancedLock) Clone() *EnhancedLock {
	return &EnhancedLock{
		ID:               el.ID,
		Owner:            el.Owner,
		FileHandle:       el.FileHandle,
		Offset:           el.Offset,
		Length:           el.Length,
		Type:             el.Type,
		ShareReservation: el.ShareReservation,
		AcquiredAt:       el.AcquiredAt,
		Blocking:         el.Blocking,
		Reclaim:          el.Reclaim,
	}
}

// ============================================================================
// Enhanced Lock Conflict Detection
// ============================================================================

// EnhancedLockConflict describes a conflicting lock for error reporting.
type EnhancedLockConflict struct {
	// Lock is the conflicting lock.
	Lock *EnhancedLock

	// Reason describes why the conflict occurred.
	Reason string
}

// IsEnhancedLockConflicting checks if two enhanced locks conflict with each other.
//
// Conflict rules:
//   - Shared locks don't conflict with other shared locks (multiple readers)
//   - Exclusive locks conflict with all other locks
//   - Locks from the same owner don't conflict (allows re-locking same range)
//   - Ranges must overlap for a conflict to occur
//
// Note: Owner comparison uses the full OwnerID string, enabling cross-protocol
// conflict detection. An NLM lock and SMB lock on the same range WILL conflict
// because they have different OwnerIDs.
func IsEnhancedLockConflicting(existing, requested *EnhancedLock) bool {
	// Same owner - no conflict (allows re-locking same range with different type)
	if existing.Owner.OwnerID == requested.Owner.OwnerID {
		return false
	}

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
