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
}

// IsEmpty returns true if the query has no filters.
func (q LockQuery) IsEmpty() bool {
	return q.FileID == "" && q.OwnerID == "" && q.ClientID == "" && q.ShareName == ""
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
func ToPersistedLock(lock *EnhancedLock, epoch uint64) *PersistedLock {
	return &PersistedLock{
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
}

// FromPersistedLock converts a PersistedLock back to an EnhancedLock.
//
// Parameters:
//   - pl: The persisted lock from storage
//
// Returns:
//   - *EnhancedLock: In-memory lock for use in lock manager
func FromPersistedLock(pl *PersistedLock) *EnhancedLock {
	return &EnhancedLock{
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
}
