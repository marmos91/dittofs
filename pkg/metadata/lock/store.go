package lock

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Lock Persistence Types and Interface
// ============================================================================

// PersistedLock represents a lock stored in the metadata store.
//
// This is the serializable form of UnifiedLock, designed for persistence
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

	// AccessMode is the SMB share mode (0=none, 1=deny-read, 2=deny-write, 3=deny-all).
	AccessMode int `json:"share_reservation"`

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

	// BreakToState is the in-flight notification's break-to target
	// (Samba `breaking_to_requested`). Used for ACK validation.
	// 0 if no break in progress.
	BreakToState uint32 `json:"break_to_state,omitempty"`

	// BreakingToRequired is the cumulative final break-to target
	// (Samba `breaking_to_required`). May be stricter than BreakToState
	// when concurrent breaks AND-merged a tighter target during an
	// in-flight stage. Note that 0 is also a valid final target
	// (break-to None lease state), so callers must consult Breaking
	// and/or BreakToState to distinguish that from "no break in progress".
	BreakingToRequired uint32 `json:"breaking_to_required,omitempty"`

	// Breaking indicates a lease break is in progress awaiting acknowledgment.
	// False for byte-range locks.
	Breaking bool `json:"breaking,omitempty"`

	// ParentLeaseKey is the V2 parent lease key for cache tree correlation.
	// Empty for byte-range locks and V1 leases.
	ParentLeaseKey []byte `json:"parent_lease_key,omitempty"`

	// IsDirectory indicates this lock is on a directory.
	// Shared by both leases and delegations: only one of Lease or Delegation
	// should be non-nil per UnifiedLock, so this field is unambiguous.
	// False for byte-range locks and file leases/delegations.
	IsDirectory bool `json:"is_directory,omitempty"`

	// ========================================================================
	// Delegation Fields (omitempty for non-delegation locks)
	// ========================================================================

	// DelegationID is the unique identifier for this delegation.
	// Empty for byte-range locks and leases.
	DelegationID string `json:"delegation_id,omitempty"`

	// DelegType is the delegation type (0=read, 1=write).
	// Only meaningful when DelegationID is non-empty.
	DelegType int `json:"deleg_type,omitempty"`

	// DelegBreaking indicates a delegation recall is in progress.
	DelegBreaking bool `json:"deleg_breaking,omitempty"`

	// DelegRecalled indicates the delegation recall was sent.
	DelegRecalled bool `json:"deleg_recalled,omitempty"`

	// DelegRevoked indicates the delegation was force-revoked.
	DelegRevoked bool `json:"deleg_revoked,omitempty"`

	// DelegNotificationMask is the directory change notification bitmask.
	DelegNotificationMask uint32 `json:"deleg_notification_mask,omitempty"`
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

	// ========================================================================
	// Lease Reclaim Operations
	// ========================================================================

	// ReclaimLease reclaims an existing lease during grace period.
	// This validates the lease existed in persistent storage before restart
	// and allows the client to re-establish the lease state.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: File handle for the lease
	//   - leaseKey: The 16-byte SMB lease key
	//   - clientID: Client identifier for ownership verification
	//
	// Returns:
	//   - *UnifiedLock: The reclaimed lease on success
	//   - error: ErrLockNotFound if lease doesn't exist
	ReclaimLease(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte, clientID string) (*UnifiedLock, error)
}

// ============================================================================
// Conversion Functions
// ============================================================================

// ToPersistedLock converts an UnifiedLock to a PersistedLock for storage.
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
func ToPersistedLock(lock *UnifiedLock, epoch uint64) *PersistedLock {
	// Guard invariant: at most one of Lease or Delegation should be non-nil.
	// Both being set would cause IsDirectory to be overwritten ambiguously.
	if lock.Lease != nil && lock.Delegation != nil {
		logger.Error("ToPersistedLock: invariant violation - lock has both Lease and Delegation",
			"lockID", lock.ID)
		// Persist only the lease path; delegation fields are skipped to avoid
		// ambiguous IsDirectory. Callers should fix the root cause.
	}

	pl := &PersistedLock{
		ID:          lock.ID,
		ShareName:   lock.Owner.ShareName,
		FileID:      string(lock.FileHandle),
		OwnerID:     lock.Owner.OwnerID,
		ClientID:    lock.Owner.ClientID,
		LockType:    int(lock.Type),
		Offset:      lock.Offset,
		Length:      lock.Length,
		AccessMode:  int(lock.AccessMode),
		AcquiredAt:  lock.AcquiredAt,
		ServerEpoch: epoch,
	}

	// Invariant: a UnifiedLock should have at most one of Lease or Delegation set.
	// If both are set, prefer lease fields and skip delegation to avoid inconsistent
	// IsDirectory values.
	if lock.Lease != nil && lock.Delegation != nil {
		logger.Warn("ToPersistedLock: UnifiedLock has both Lease and Delegation set, persisting lease only",
			"lockID", lock.ID, "owner", lock.Owner.ClientID)
	}

	// Persist lease fields if this is a lease
	if lock.Lease != nil {
		pl.LeaseKey = lock.Lease.LeaseKey[:]
		pl.LeaseState = lock.Lease.LeaseState
		pl.LeaseEpoch = lock.Lease.Epoch
		pl.BreakToState = lock.Lease.BreakToState
		pl.BreakingToRequired = lock.Lease.BreakingToRequired
		pl.Breaking = lock.Lease.Breaking
		pl.IsDirectory = lock.Lease.IsDirectory

		// Only set ParentLeaseKey when non-zero so omitempty works for V1 leases
		if lock.Lease.ParentLeaseKey != [16]byte{} {
			pl.ParentLeaseKey = lock.Lease.ParentLeaseKey[:]
		}
	}

	// Persist delegation fields (only when lease is not also set)
	if lock.Delegation != nil && lock.Lease == nil {
		pl.DelegationID = lock.Delegation.DelegationID
		pl.DelegType = int(lock.Delegation.DelegType)
		pl.IsDirectory = lock.Delegation.IsDirectory
		pl.DelegBreaking = lock.Delegation.Breaking
		pl.DelegRecalled = lock.Delegation.Recalled
		pl.DelegRevoked = lock.Delegation.Revoked
		pl.DelegNotificationMask = lock.Delegation.NotificationMask
	}

	return pl
}

// FromPersistedLock converts a PersistedLock back to an UnifiedLock.
//
// Parameters:
//   - pl: The persisted lock from storage
//
// Returns:
//   - *UnifiedLock: In-memory lock for use in lock manager
//
// For leases (identified by non-empty LeaseKey), the OpLock struct
// is populated with the persisted lease state. Blocking and Reclaim
// are runtime-only and not restored.
func FromPersistedLock(pl *PersistedLock) *UnifiedLock {
	el := &UnifiedLock{
		ID: pl.ID,
		Owner: LockOwner{
			OwnerID:   pl.OwnerID,
			ClientID:  pl.ClientID,
			ShareName: pl.ShareName,
		},
		FileHandle: FileHandle(pl.FileID),
		Offset:     pl.Offset,
		Length:     pl.Length,
		Type:       LockType(pl.LockType),
		AccessMode: AccessMode(pl.AccessMode),
		AcquiredAt: pl.AcquiredAt,
		// Blocking and Reclaim are runtime-only, not persisted
	}

	// Restore lease fields if this is a lease (16-byte key present)
	if len(pl.LeaseKey) == 16 {
		var leaseKey [16]byte
		copy(leaseKey[:], pl.LeaseKey)

		var parentLeaseKey [16]byte
		if len(pl.ParentLeaseKey) == 16 {
			copy(parentLeaseKey[:], pl.ParentLeaseKey)
		}

		el.Lease = &OpLock{
			LeaseKey:           leaseKey,
			LeaseState:         pl.LeaseState,
			Epoch:              pl.LeaseEpoch,
			BreakToState:       pl.BreakToState,
			BreakingToRequired: pl.BreakingToRequired,
			Breaking:           pl.Breaking,
			ParentLeaseKey:     parentLeaseKey,
			IsDirectory:        pl.IsDirectory,
			// BreakStarted is runtime-only, not persisted
		}
		// Backwards compat: locks persisted before BreakingToRequired existed
		// have BreakingToRequired==0. The zero value is ambiguous (could mean
		// "break-to None" for an active break, or "no break" otherwise).
		// Restore the invariant: when not breaking, BreakingToRequired tracks
		// LeaseState; when breaking, it defaults to BreakToState (the in-flight
		// target) since older records didn't track a stricter cumulative
		// target.
		if el.Lease.BreakingToRequired == 0 {
			if el.Lease.Breaking {
				el.Lease.BreakingToRequired = el.Lease.BreakToState
			} else {
				el.Lease.BreakingToRequired = el.Lease.LeaseState
			}
		}
	}

	// Restore delegation fields if this is a delegation (DelegationID present)
	if pl.DelegationID != "" {
		el.Delegation = &Delegation{
			DelegationID:     pl.DelegationID,
			DelegType:        DelegationType(pl.DelegType),
			IsDirectory:      pl.IsDirectory,
			ClientID:         pl.ClientID,
			ShareName:        pl.ShareName,
			Breaking:         pl.DelegBreaking,
			Recalled:         pl.DelegRecalled,
			Revoked:          pl.DelegRevoked,
			NotificationMask: pl.DelegNotificationMask,
			// BreakStarted is runtime-only, not persisted
		}
	}

	return el
}
