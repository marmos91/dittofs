// Package handlers provides SMB2 command handlers and session management.
//
// This file provides cross-protocol integration helpers for SMB handlers to
// check NLM byte-range locks and return appropriate SMB status codes.
//
// Cross-Protocol Semantics (per CONTEXT.md):
//   - NFS lock vs SMB Write lease: Deny SMB immediately (STATUS_LOCK_NOT_GRANTED)
//   - NLM byte-range locks are explicit and win over opportunistic SMB leases
//   - Cross-protocol conflicts logged at INFO level (working as designed)
//
// Reference: MS-ERREF 2.3 NTSTATUS Values
package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// SMB Status Codes for NLM Conflicts
// ============================================================================

// SMB status code for lock conflicts. Per MS-ERREF 2.3, STATUS_LOCK_NOT_GRANTED
// indicates that a requested lock operation could not be performed because the
// requested range is locked by another party.
//
// This is the appropriate status for NLM lock conflicts, NOT STATUS_SHARING_VIOLATION.
// STATUS_SHARING_VIOLATION (0xC0000043) is for share mode conflicts at open time,
// while STATUS_LOCK_NOT_GRANTED is specifically for byte-range lock conflicts.
const StatusLockNotGranted uint32 = 0xC0000054

// ============================================================================
// Cross-Protocol Logging Helpers
// ============================================================================

// formatNLMLockInfo formats NLM lock information for logging purposes.
//
// This produces a human-readable string containing:
//   - Lock owner (protocol:client identifier)
//   - Byte range (offset-length or "entire file")
//   - Lock type (shared/exclusive)
//
// Used for INFO-level logging when cross-protocol conflicts occur.
// Per CONTEXT.md, these are logged at INFO level since they're working as designed.
//
// Parameters:
//   - nlmLock: The NLM lock to format
//
// Returns:
//   - string: Human-readable lock description
//
// Example outputs:
//
//	"owner=nlm:host1:1234, range=0-1024, type=exclusive"
//	"owner=nlm:host1:1234, range=entire-file, type=shared"
func formatNLMLockInfo(nlmLock *lock.UnifiedLock) string {
	if nlmLock == nil {
		return "nil"
	}

	// Format lock type
	lockType := "shared"
	if nlmLock.IsExclusive() {
		lockType = "exclusive"
	}

	// Format byte range
	var rangeStr string
	if nlmLock.Length == 0 {
		// Length 0 means to end of file
		rangeStr = fmt.Sprintf("%d-EOF", nlmLock.Offset)
	} else if nlmLock.Offset == 0 && nlmLock.Length == ^uint64(0) {
		rangeStr = "entire-file"
	} else {
		rangeStr = fmt.Sprintf("%d-%d", nlmLock.Offset, nlmLock.Offset+nlmLock.Length)
	}

	return fmt.Sprintf("owner=%s, range=%s, type=%s",
		nlmLock.Owner.OwnerID, rangeStr, lockType)
}

// ============================================================================
// NLM Lock Conflict Detection for Leases
// ============================================================================

// checkNLMLocksForLeaseConflict queries the lock store for NLM byte-range locks
// that would conflict with a requested SMB lease.
//
// Per CONTEXT.md:
//   - NFS lock vs SMB Write lease: Deny SMB immediately
//   - NFS byte-range locks are explicit and win over opportunistic SMB leases
//
// Conflict Rules:
//   - Write lease requested: ANY NLM lock conflicts (exclusive access required)
//   - Read lease requested: ONLY exclusive NLM locks conflict
//   - Handle lease (alone): No conflict with NLM locks (H is about delete notification)
//
// Parameters:
//   - ctx: Context for cancellation
//   - lockStore: The lock store to query
//   - fileHandle: The file handle to check
//   - requestedState: The requested lease state (R/W/H flags)
//
// Returns:
//   - []*lock.UnifiedLock: List of conflicting NLM locks (empty if no conflicts)
//   - error: Query error (nil on success)
func checkNLMLocksForLeaseConflict(
	ctx context.Context,
	lockStore lock.LockStore,
	fileHandle lock.FileHandle,
	requestedState uint32,
) ([]*lock.UnifiedLock, error) {
	if lockStore == nil {
		return nil, nil
	}

	// Query byte-range locks only (not leases)
	// IsLease=false filters to NLM locks
	isLease := false
	locks, err := lockStore.ListLocks(ctx, lock.LockQuery{
		FileID:  string(fileHandle),
		IsLease: &isLease,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query NLM locks: %w", err)
	}

	var conflicts []*lock.UnifiedLock

	// Determine what conflicts based on requested lease state
	wantsWrite := requestedState&lock.LeaseStateWrite != 0
	wantsRead := requestedState&lock.LeaseStateRead != 0

	for _, pl := range locks {
		el := lock.FromPersistedLock(pl)

		// Skip if this is somehow a lease (shouldn't happen with IsLease=false)
		if el.IsLease() {
			continue
		}

		// Check for conflict based on lease request type
		if wantsWrite {
			// Write lease requires exclusive access to the file
			// ANY NLM lock (shared or exclusive) conflicts
			conflicts = append(conflicts, el)
		} else if wantsRead {
			// Read lease can coexist with shared NLM locks
			// But exclusive NLM locks conflict (exclusive = client wants exclusive access)
			if el.IsExclusive() {
				conflicts = append(conflicts, el)
			}
		}
		// Handle-only lease (no R or W) does not conflict with NLM locks
		// Handle lease is about delete/rename notification, not data access
	}

	return conflicts, nil
}
