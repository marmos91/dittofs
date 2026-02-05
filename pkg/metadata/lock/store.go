package lock

import (
	"context"
	"time"
)

// ============================================================================
// Lock Persistence Types and Interface
// ============================================================================

// PersistedLock represents a lock stored in the metadata store.
//
// This is the serializable form of EnhancedLock, designed for persistence
// across server restarts. All protocol-specific information is encoded
// in the OwnerID field as an opaque string.
//
// Persistence enables:
//   - Lock recovery after server restart
//   - Grace period for clients to reclaim locks
//   - Split-brain detection via ServerEpoch
//   - SMB lease state persistence and reclaim
type PersistedLock struct {
	// ID is the unique identifier for this lock (UUID).
	ID string `json:"id"`

	// ShareName is the share this lock belongs to.
	ShareName string `json:"share_name"`

	// FileID is the file identifier (string representation of FileHandle).
	FileID string `json:"file_id"`

	// OwnerID is the protocol-provided owner identifier.
	// Format: "{protocol}:{details}" - treated as opaque.
	OwnerID string `json:"owner_id"`

	// ClientID is the connection tracker client ID.
	// Used to clean up locks when a client disconnects.
	ClientID string `json:"client_id"`

	// LockType indicates shared (0) or exclusive (1).
	LockType int `json:"lock_type"`

	// Offset is the starting byte offset of the lock.
	Offset uint64 `json:"offset"`

	// Length is the number of bytes locked (0 = to EOF).
	Length uint64 `json:"length"`

	// ShareReservation is the SMB share mode (0=none, 1=deny-read, 2=deny-write, 3=deny-all).
	ShareReservation int `json:"share_reservation"`

	// AcquiredAt is when the lock was acquired.
	AcquiredAt time.Time `json:"acquired_at"`

	// ServerEpoch is the server epoch when the lock was acquired.
	// Used for split-brain detection and stale lock cleanup.
	ServerEpoch uint64 `json:"server_epoch"`

	// ========================================================================
	// Lease Fields (omitempty for byte-range locks)
	// ========================================================================

	// LeaseKey is the 128-bit client-generated key identifying the lease.
	// Non-empty (16 bytes) for leases, empty for byte-range locks.
	LeaseKey []byte `json:"lease_key,omitempty"`

	// LeaseState is the current R/W/H flags (LeaseStateRead|Write|Handle).
	// 0 for byte-range locks or None lease state.
	LeaseState uint32 `json:"lease_state,omitempty"`

	// LeaseEpoch is the SMB3 epoch counter, incremented on state change.
	// 0 for byte-range locks.
	LeaseEpoch uint16 `json:"lease_epoch,omitempty"`

	// BreakToState is the target state during an active lease break.
	// 0 if no break in progress.
	BreakToState uint32 `json:"break_to_state,omitempty"`

	// Breaking indicates a lease break is in progress awaiting acknowledgment.
	// False for byte-range locks.
	Breaking bool `json:"breaking,omitempty"`
}

// IsLease returns true if this persisted lock is an SMB lease.
func (pl *PersistedLock) IsLease() bool {
	return len(pl.LeaseKey) == 16
}

// LockQuery specifies filters for listing locks.
//
// All fields are optional. Empty fields are not used in filtering.
// Multiple fields are ANDed together.
type LockQuery struct {
	// FileID filters by file (string representation of FileHandle).
	// Empty string means no file filtering.
	FileID string

	// OwnerID filters by lock owner.
	// Empty string means no owner filtering.
	OwnerID string

	// ClientID filters by client.
	// Empty string means no client filtering.
	ClientID string

	// ShareName filters by share.
	// Empty string means no share filtering.
	ShareName string

	// IsLease filters by lock type.
	// nil means no type filtering (both leases and byte-range locks).
	// true means leases only.
	// false means byte-range locks only.
	IsLease *bool
}

// IsEmpty returns true if the query has no filters.
func (q LockQuery) IsEmpty() bool {
	return q.FileID == "" && q.OwnerID == "" && q.ClientID == "" && q.ShareName == "" && q.IsLease == nil
}

// MatchesLock returns true if the lock matches all query filters.
// Used by store implementations for consistent filtering logic.
func (q LockQuery) MatchesLock(lk *PersistedLock) bool {
	if q.FileID != "" && lk.FileID != q.FileID {
		return false
	}
	if q.OwnerID != "" && lk.OwnerID != q.OwnerID {
		return false
	}
	if q.ClientID != "" && lk.ClientID != q.ClientID {
		return false
	}
	if q.ShareName != "" && lk.ShareName != q.ShareName {
		return false
	}
	if q.IsLease != nil {
		isLease := lk.IsLease()
		if *q.IsLease != isLease {
			return false
		}
	}
	return true
}

// LockStore defines operations for persisting locks to the metadata store.
//
// This interface enables lock state to survive server restarts, supporting:
//   - NLM/SMB grace period for lock reclamation
//   - Split-brain detection via server epochs
//   - Client disconnect cleanup
//
// Thread Safety:
// Implementations must be safe for concurrent use by multiple goroutines.
// Operations within a transaction (via Transaction interface) share the
// transaction's isolation level.
type LockStore interface {
	// ========================================================================
	// Lock CRUD Operations
	// ========================================================================

	// PutLock persists a lock. Overwrites if lock with same ID exists.
	PutLock(ctx context.Context, lock *PersistedLock) error

	// GetLock retrieves a lock by ID.
	// Returns ErrLockNotFound if lock doesn't exist.
	GetLock(ctx context.Context, lockID string) (*PersistedLock, error)

	// DeleteLock removes a lock by ID.
	// Returns ErrLockNotFound if lock doesn't exist.
	DeleteLock(ctx context.Context, lockID string) error

	// ListLocks returns locks matching the query.
	// Empty query returns all locks.
	ListLocks(ctx context.Context, query LockQuery) ([]*PersistedLock, error)

	// ========================================================================
	// Bulk Operations
	// ========================================================================

	// DeleteLocksByClient removes all locks for a client.
	// Returns number of locks deleted.
	// Used when a client disconnects to clean up its locks.
	DeleteLocksByClient(ctx context.Context, clientID string) (int, error)

	// DeleteLocksByFile removes all locks for a file.
	// Returns number of locks deleted.
	// Used when a file is deleted.
	DeleteLocksByFile(ctx context.Context, fileID string) (int, error)

	// ========================================================================
	// Server Epoch Operations
	// ========================================================================

	// GetServerEpoch returns current server epoch.
	// Returns 0 for a fresh server (never started).
	GetServerEpoch(ctx context.Context) (uint64, error)

	// IncrementServerEpoch increments and returns new epoch.
	// Called during server startup to detect restarts.
	// Locks with epoch < current epoch are stale.
	IncrementServerEpoch(ctx context.Context) (uint64, error)
}

// ============================================================================
// Conversion Functions
// ============================================================================

// ToPersistedLock converts an EnhancedLock to a PersistedLock for storage.
//
// Parameters:
//   - lock: The in-memory lock to persist
//   - epoch: Current server epoch to stamp on the lock
//
// Returns:
//   - *PersistedLock: Serializable lock ready for storage
//
// For leases, the Lease field must be non-nil. The 128-bit LeaseKey,
// LeaseState, Epoch, BreakToState, and Breaking are all preserved.
func ToPersistedLock(lock *EnhancedLock, epoch uint64) *PersistedLock {
	pl := &PersistedLock{
		ID:               lock.ID,
		ShareName:        lock.Owner.ShareName,
		FileID:           string(lock.FileHandle),
		OwnerID:          lock.Owner.OwnerID,
		ClientID:         lock.Owner.ClientID,
		LockType:         int(lock.Type),
		Offset:           lock.Offset,
		Length:           lock.Length,
		ShareReservation: int(lock.ShareReservation),
		AcquiredAt:       lock.AcquiredAt,
		ServerEpoch:      epoch,
	}

	// Persist lease fields if this is a lease
	if lock.Lease != nil {
		pl.LeaseKey = lock.Lease.LeaseKey[:]
		pl.LeaseState = lock.Lease.LeaseState
		pl.LeaseEpoch = lock.Lease.Epoch
		pl.BreakToState = lock.Lease.BreakToState
		pl.Breaking = lock.Lease.Breaking
	}

	return pl
}

// FromPersistedLock converts a PersistedLock back to an EnhancedLock.
//
// Parameters:
//   - pl: The persisted lock from storage
//
// Returns:
//   - *EnhancedLock: In-memory lock for use in lock manager
//
// For leases (identified by non-empty LeaseKey), the LeaseInfo struct
// is populated with the persisted lease state. Blocking and Reclaim
// are runtime-only and not restored.
func FromPersistedLock(pl *PersistedLock) *EnhancedLock {
	el := &EnhancedLock{
		ID: pl.ID,
		Owner: LockOwner{
			OwnerID:   pl.OwnerID,
			ClientID:  pl.ClientID,
			ShareName: pl.ShareName,
		},
		FileHandle:       FileHandle(pl.FileID),
		Offset:           pl.Offset,
		Length:           pl.Length,
		Type:             LockType(pl.LockType),
		ShareReservation: ShareReservation(pl.ShareReservation),
		AcquiredAt:       pl.AcquiredAt,
		// Blocking and Reclaim are runtime-only, not persisted
	}

	// Restore lease fields if this is a lease (16-byte key present)
	if len(pl.LeaseKey) == 16 {
		var leaseKey [16]byte
		copy(leaseKey[:], pl.LeaseKey)
		el.Lease = &LeaseInfo{
			LeaseKey:     leaseKey,
			LeaseState:   pl.LeaseState,
			Epoch:        pl.LeaseEpoch,
			BreakToState: pl.BreakToState,
			Breaking:     pl.Breaking,
			// BreakStarted is runtime-only, not persisted
		}
	}

	return el
}
