// Package metadata provides the UnifiedLockView for cross-protocol lock visibility.
//
// UnifiedLockView provides a single API to query all locks and leases on a file,
// enabling any protocol handler (NFS/NLM, SMB) to see the complete locking state.
// This is the foundation for cross-protocol conflict detection.
//
// Design Decision:
// UnifiedLockView is in pkg/metadata/ (not pkg/metadata/lock/) because it is
// owned by MetadataService and provides a higher-level abstraction over the
// lock store. The lock package contains lower-level lock management primitives.
package metadata

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// FileLocksInfo contains the result of querying all locks on a file.
//
// Locks are separated by type to allow protocol handlers to process them
// appropriately:
//   - ByteRangeLocks: Traditional byte-range locks from NLM or SMB lock requests
//   - Leases: SMB2/3 whole-file leases (R/W/H caching permissions)
//
// Cross-Protocol Usage:
//   - NFS handler queries this to check for SMB leases before write operations
//   - SMB handler queries this to check for NLM locks before granting leases
//   - Both use this for conflict error responses with holder info
type FileLocksInfo struct {
	// ByteRangeLocks are NLM or SMB byte-range locks on the file.
	// These have specific Offset/Length ranges and are nil for leases.
	ByteRangeLocks []*lock.EnhancedLock

	// Leases are SMB2/3 leases on the file.
	// These are whole-file (Offset=0, Length=0) and have Lease != nil.
	Leases []*lock.EnhancedLock
}

// HasAnyLocks returns true if there are any locks or leases on the file.
func (f *FileLocksInfo) HasAnyLocks() bool {
	return len(f.ByteRangeLocks) > 0 || len(f.Leases) > 0
}

// TotalCount returns the total number of locks and leases.
func (f *FileLocksInfo) TotalCount() int {
	return len(f.ByteRangeLocks) + len(f.Leases)
}

// UnifiedLockView provides a unified API to query all locks and leases on a file.
//
// It wraps a LockStore and provides higher-level methods for cross-protocol
// lock visibility. Protocol handlers use this to:
//   - Check for conflicting locks before operations
//   - Build holder info for denial responses
//   - Determine if lease breaks are needed
//
// Thread Safety:
// UnifiedLockView is safe for concurrent use. It delegates to the underlying
// LockStore which provides its own synchronization.
//
// Lifecycle:
// One UnifiedLockView instance is created per share, alongside the LockManager.
// It is owned by MetadataService and initialized when a share's LockStore is available.
type UnifiedLockView struct {
	lockStore lock.LockStore
}

// NewUnifiedLockView creates a new UnifiedLockView wrapping the given LockStore.
//
// Parameters:
//   - store: The LockStore to query for locks. Must not be nil.
//
// Returns:
//   - *UnifiedLockView: The new view instance
func NewUnifiedLockView(store lock.LockStore) *UnifiedLockView {
	return &UnifiedLockView{
		lockStore: store,
	}
}

// GetAllLocksOnFile queries all locks and leases on a file.
//
// This is the primary method for cross-protocol lock visibility. It returns
// both NLM byte-range locks and SMB leases in a single call, separated by type
// for easy processing.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file handle to query locks for
//
// Returns:
//   - *FileLocksInfo: Contains ByteRangeLocks and Leases slices (never nil)
//   - error: If the underlying store query fails
//
// Usage:
//
//	info, err := view.GetAllLocksOnFile(ctx, handle)
//	if err != nil { return err }
//	if len(info.Leases) > 0 {
//	    // Handle SMB leases (may need to break for NFS write)
//	}
//	if len(info.ByteRangeLocks) > 0 {
//	    // Handle byte-range locks
//	}
func (v *UnifiedLockView) GetAllLocksOnFile(ctx context.Context, fileHandle lock.FileHandle) (*FileLocksInfo, error) {
	// Query all locks on this file using the file ID filter
	query := lock.LockQuery{
		FileID: string(fileHandle),
	}

	locks, err := v.lockStore.ListLocks(ctx, query)
	if err != nil {
		return nil, err
	}

	// Separate into byte-range locks and leases
	result := &FileLocksInfo{
		ByteRangeLocks: make([]*lock.EnhancedLock, 0),
		Leases:         make([]*lock.EnhancedLock, 0),
	}

	for _, pl := range locks {
		el := lock.FromPersistedLock(pl)
		if el.IsLease() {
			result.Leases = append(result.Leases, el)
		} else {
			result.ByteRangeLocks = append(result.ByteRangeLocks, el)
		}
	}

	return result, nil
}

// HasConflictingLocks checks if any locks would conflict with a requested lock.
//
// This is a convenience method for conflict detection. It checks all existing
// locks on a file and returns any that would conflict with the requested type.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file handle to check
//   - lockType: The type of lock being requested (shared or exclusive)
//
// Returns:
//   - hasConflict: true if any conflicting locks exist
//   - conflicting: Slice of locks that conflict (empty if no conflict)
//   - error: If the underlying store query fails
//
// Usage:
//
//	// Check if an exclusive lock would conflict
//	hasConflict, conflicts, err := view.HasConflictingLocks(ctx, handle, lock.LockTypeExclusive)
//	if err != nil { return err }
//	if hasConflict {
//	    // Build denial response with holder info from conflicts[0]
//	}
func (v *UnifiedLockView) HasConflictingLocks(
	ctx context.Context,
	fileHandle lock.FileHandle,
	lockType lock.LockType,
) (bool, []*lock.EnhancedLock, error) {
	info, err := v.GetAllLocksOnFile(ctx, fileHandle)
	if err != nil {
		return false, nil, err
	}

	// Create a test lock to check for conflicts
	// We use a whole-file range (offset=0, length=0) for simplicity
	// since we're checking for any conflict, not a specific range
	testLock := &lock.EnhancedLock{
		FileHandle: fileHandle,
		Offset:     0,
		Length:     0, // Whole file (to EOF)
		Type:       lockType,
		Owner: lock.LockOwner{
			OwnerID: "", // Empty owner ID ensures no self-conflict
		},
	}

	var conflicting []*lock.EnhancedLock

	// Check byte-range locks
	for _, existing := range info.ByteRangeLocks {
		if lock.IsEnhancedLockConflicting(existing, testLock) {
			conflicting = append(conflicting, existing)
		}
	}

	// Check leases
	for _, existing := range info.Leases {
		if lock.IsEnhancedLockConflicting(existing, testLock) {
			conflicting = append(conflicting, existing)
		}
	}

	return len(conflicting) > 0, conflicting, nil
}

// GetLeaseByKey finds a lease with the specified lease key on a file.
//
// This is useful for SMB operations that need to find an existing lease
// by its 128-bit key, such as lease upgrade/downgrade requests.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file handle to search
//   - leaseKey: The 128-bit lease key to find
//
// Returns:
//   - *lock.EnhancedLock: The lease if found, nil otherwise
//   - error: If the underlying store query fails
func (v *UnifiedLockView) GetLeaseByKey(
	ctx context.Context,
	fileHandle lock.FileHandle,
	leaseKey [16]byte,
) (*lock.EnhancedLock, error) {
	info, err := v.GetAllLocksOnFile(ctx, fileHandle)
	if err != nil {
		return nil, err
	}

	for _, lease := range info.Leases {
		if lease.Lease != nil && lease.Lease.LeaseKey == leaseKey {
			return lease, nil
		}
	}

	return nil, nil
}

// GetWriteLeases returns all leases with Write caching permission on a file.
//
// This is useful for NFS handlers that need to check if any SMB client
// has cached writes (W lease) that must be flushed before NFS can read/write.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file handle to search
//
// Returns:
//   - []*lock.EnhancedLock: Leases with Write permission (may be empty)
//   - error: If the underlying store query fails
func (v *UnifiedLockView) GetWriteLeases(
	ctx context.Context,
	fileHandle lock.FileHandle,
) ([]*lock.EnhancedLock, error) {
	info, err := v.GetAllLocksOnFile(ctx, fileHandle)
	if err != nil {
		return nil, err
	}

	var writeLeases []*lock.EnhancedLock
	for _, lease := range info.Leases {
		if lease.Lease != nil && lease.Lease.HasWrite() {
			writeLeases = append(writeLeases, lease)
		}
	}

	return writeLeases, nil
}

// GetHandleLeases returns all leases with Handle caching permission on a file.
//
// This is useful for NFS handlers that need to check if any SMB client
// has Handle leases (H lease) that must be broken before delete/rename.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file handle to search
//
// Returns:
//   - []*lock.EnhancedLock: Leases with Handle permission (may be empty)
//   - error: If the underlying store query fails
func (v *UnifiedLockView) GetHandleLeases(
	ctx context.Context,
	fileHandle lock.FileHandle,
) ([]*lock.EnhancedLock, error) {
	info, err := v.GetAllLocksOnFile(ctx, fileHandle)
	if err != nil {
		return nil, err
	}

	var handleLeases []*lock.EnhancedLock
	for _, lease := range info.Leases {
		if lease.Lease != nil && lease.Lease.HasHandle() {
			handleLeases = append(handleLeases, lease)
		}
	}

	return handleLeases, nil
}
