package lock

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
)

// LockManager provides unified lock management for all protocols.
//
// This is the single interface that both NFS and SMB adapters use for lock
// operations. It unifies byte-range locks, oplocks/leases, grace period
// management, and break callback registration into a single coherent API.
//
// The interface covers:
//   - Unified lock CRUD (AddUnifiedLock, RemoveUnifiedLock, etc.)
//   - Centralized break operations (replaces OplockChecker global)
//   - Legacy byte-range locks (backward compat for existing callers)
//   - Grace period management
//   - Break callback registration
//   - Connection/cleanup operations
type LockManager interface {
	// ========================================================================
	// Unified Lock CRUD
	// ========================================================================

	// AddUnifiedLock adds a unified lock (byte-range or oplock).
	// Returns error if the lock conflicts with existing locks.
	AddUnifiedLock(handleKey string, lock *UnifiedLock) error

	// RemoveUnifiedLock removes a unified lock using POSIX splitting semantics.
	RemoveUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) error

	// ListUnifiedLocks returns all unified locks on a file.
	ListUnifiedLocks(handleKey string) []*UnifiedLock

	// RemoveFileUnifiedLocks removes all unified locks for a file.
	RemoveFileUnifiedLocks(handleKey string)

	// UpgradeLock atomically converts a shared lock to exclusive if no other readers exist.
	UpgradeLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error)

	// GetUnifiedLock retrieves a specific unified lock by owner and range.
	GetUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error)

	// ========================================================================
	// Centralized Break Operations (replaces OplockChecker global)
	// ========================================================================

	// CheckAndBreakOpLocksForWrite checks and breaks oplocks that conflict with a write.
	// Write breaks all Write oplocks to None, Read oplocks to None.
	// excludeOwner can be nil to check all owners.
	CheckAndBreakOpLocksForWrite(handleKey string, excludeOwner *LockOwner) error

	// CheckAndBreakOpLocksForRead checks and breaks oplocks that conflict with a read.
	// Read only breaks Write oplocks (to Read).
	// excludeOwner can be nil to check all owners.
	CheckAndBreakOpLocksForRead(handleKey string, excludeOwner *LockOwner) error

	// CheckAndBreakOpLocksForDelete checks and breaks all oplocks on a file.
	// Delete breaks all oplocks to None.
	// excludeOwner can be nil to check all owners.
	CheckAndBreakOpLocksForDelete(handleKey string, excludeOwner *LockOwner) error

	// ========================================================================
	// Legacy Byte-Range (backward compat for existing callers)
	// ========================================================================

	// Lock attempts to acquire a byte-range lock on a file.
	Lock(handleKey string, lock FileLock) error

	// Unlock releases a specific byte-range lock.
	// openID identifies the open that owns the lock (empty string falls back to sessionID).
	Unlock(handleKey string, openID string, sessionID uint64, offset, length uint64) error

	// UnlockAllForOpen releases all locks held by a specific open on a file.
	UnlockAllForOpen(handleKey string, openID string) int

	// TestLock checks if a lock would succeed without acquiring it.
	TestLock(handleKey string, lock FileLock) (*LockConflict, error)

	// ListLocks returns all active byte-range locks on a file.
	ListLocks(handleKey string) []FileLock

	// ========================================================================
	// Grace Period (part of LockManager per user decision)
	// ========================================================================

	// EnterGracePeriod transitions to grace period state.
	EnterGracePeriod(expectedClients []string)

	// ExitGracePeriod manually exits the grace period.
	ExitGracePeriod()

	// IsOperationAllowed checks if a lock operation is allowed in the current state.
	IsOperationAllowed(op Operation) (bool, error)

	// MarkReclaimed records that a client has reclaimed their locks.
	MarkReclaimed(clientID string)

	// IsInGracePeriod returns true if grace period is currently active.
	IsInGracePeriod() bool

	// ========================================================================
	// Lease Operations
	// ========================================================================

	// RequestLease requests a new or upgraded lease on a file or directory.
	// Returns the granted state (may be less than requested), epoch, and error.
	// isDirectory=true restricts to ValidDirectoryLeaseStates.
	RequestLease(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
		parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
		requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error)

	// AcknowledgeLeaseBreak processes a client's lease break acknowledgment.
	// acknowledgedState is the state the client accepts (must be <= breakToState).
	AcknowledgeLeaseBreak(ctx context.Context, leaseKey [16]byte,
		acknowledgedState uint32, epoch uint16) error

	// ReleaseLease releases ALL lease state for the given lease key across
	// every handleKey bucket. Callers with per-handle scope should prefer
	// ReleaseLeaseForHandle — see its doc for why this matters under
	// smbtorture's fixed LEASE1/LEASE2 key pattern.
	ReleaseLease(ctx context.Context, leaseKey [16]byte) error

	// ReleaseLeaseForHandle removes lease records matching leaseKey from a
	// single handleKey bucket only. Use this on CLOSE so that concurrent
	// opens on OTHER files sharing the same LeaseKey keep their records.
	ReleaseLeaseForHandle(ctx context.Context, handleKey string, leaseKey [16]byte) error

	// ReclaimLease reclaims a lease during grace period (both SMB and NFS).
	// Returns the reclaimed lock or error if lease doesn't exist or directory deleted.
	ReclaimLease(ctx context.Context, leaseKey [16]byte,
		requestedState uint32, isDirectory bool) (*UnifiedLock, error)

	// GetLeaseState returns the current state and epoch for a lease key.
	// found=false if no lease exists with that key.
	GetLeaseState(ctx context.Context, leaseKey [16]byte) (state uint32, epoch uint16, found bool)

	// IsTraditionalOplockForKey returns true if the lease record for this key
	// was granted via RequestLeaseAsOplock (traditional oplock, not SMB2.1+ lease).
	IsTraditionalOplockForKey(leaseKey [16]byte) bool

	// SetLeaseEpoch sets the epoch on an existing lease identified by leaseKey.
	// Per MS-SMB2 3.3.5.9: For V2 leases, the server tracks the client's epoch.
	// Returns false if no lease was found with the given key.
	SetLeaseEpoch(leaseKey [16]byte, epoch uint16) bool

	// ========================================================================
	// Delegation Operations
	// ========================================================================

	// GrantDelegation grants a delegation on a file.
	// Returns error if conflicting leases exist.
	GrantDelegation(handleKey string, delegation *Delegation) error

	// RevokeDelegation force-revokes a delegation, removing it from the lock map.
	RevokeDelegation(handleKey string, delegationID string) error

	// ReturnDelegation handles a client returning a delegation (idempotent).
	ReturnDelegation(handleKey string, delegationID string) error

	// GetDelegation retrieves a specific delegation by ID.
	GetDelegation(handleKey string, delegationID string) *Delegation

	// ListDelegations returns all delegations on a file.
	ListDelegations(handleKey string) []*Delegation

	// ========================================================================
	// Unified Caching Break Operations
	// ========================================================================

	// CheckAndBreakCachingForWrite breaks all leases AND all delegations.
	// Used for write operations.
	CheckAndBreakCachingForWrite(handleKey string, excludeOwner *LockOwner) error

	// CheckAndBreakCachingForRead breaks write leases and write delegations.
	// Read delegations and read leases coexist.
	CheckAndBreakCachingForRead(handleKey string, excludeOwner *LockOwner) error

	// CheckAndBreakCachingForDelete breaks all leases AND all delegations.
	// Used for delete operations.
	CheckAndBreakCachingForDelete(handleKey string, excludeOwner *LockOwner) error

	// CheckAndBreakLeasesForSMBOpen breaks Write leases for an SMB CREATE.
	// Unlike CheckAndBreakCachingForWrite, this strips only the Write bit,
	// preserving Read and Handle (RWH -> RH, RW -> R).
	CheckAndBreakLeasesForSMBOpen(handleKey string, excludeOwner *LockOwner) error

	// BreakLeasesForByteRangeLock breaks every other-key lease that holds
	// Read caching to None. Per MS-SMB2 3.3.5.14 and Samba
	// `source3/smbd/smb2_oplock.c::contend_level2_oplocks_begin_default`,
	// acquiring a byte-range lock invalidates remote read caches because
	// another client may now read different data from the locked range.
	// Unlike SMB CREATE breaks (which strip only the Write bit), this is a
	// full revocation:
	//   - RWH -> None
	//   - RW  -> None
	//   - RH  -> None
	//   - R   -> None
	// Leases without Read caching (None, W-only) are not broken: they cannot
	// be caching reads. The locker's own lease must be excluded via
	// excludeOwner.ExcludeLeaseKey ("nobreakself" per MS-SMB2 3.3.5.9).
	BreakLeasesForByteRangeLock(handleKey string, excludeOwner *LockOwner) error

	// BreakLeasesOnOpenConflict breaks existing leases before an SMB CREATE
	// proceeds, per MS-SMB2 3.3.4.7 and Samba `source3/smbd/open.c::delay_for_oplock_fn`.
	// Per-lease target state is computed via ComputeLeaseBreakTo(state, reason).
	BreakLeasesOnOpenConflict(handleKey string, excludeOwner *LockOwner, reason BreakReason) error

	// BreakReadLeasesForParentDir breaks Read leases on a parent directory
	// when directory content changes (CREATE, RENAME, DELETE on close).
	// Per MS-FSA 2.1.5.14: changes to directory listing invalidate Read
	// caching, so clients holding R or RW leases must be notified.
	// Breaks to None (full revocation of Read caching).
	BreakReadLeasesForParentDir(handleKey string, excludeOwner *LockOwner) error

	// WaitForBreakCompletion blocks until all breaking locks on a file resolve
	// or the context is cancelled.
	WaitForBreakCompletion(ctx context.Context, handleKey string) error

	// WaitForBreakCompletionExceptKey is WaitForBreakCompletion scoped to
	// ignore any breaking lease keyed on exceptKey. The SMB CREATE path uses
	// this on same-key reopens: MS-SMB2 3.3.5.9.8 requires the opener to
	// observe Breaking=true on its own key (to emit
	// SMB2_LEASE_FLAG_BREAK_IN_PROGRESS), which forceCompleteBreaks would
	// otherwise clear, while other-key breaks still need to drain first.
	WaitForBreakCompletionExceptKey(ctx context.Context, handleKey string, exceptKey [16]byte) error

	// HasOtherBreakingLeases reports whether any lease on handleKey (excluding
	// exceptKey) or any delegation is currently Breaking. Non-blocking peek
	// used by the SMB CREATE async-park path: if BreakLeasesOnOpenConflict
	// marked other-key leases as Breaking, the handler emits a STATUS_PENDING
	// interim and resumes from a goroutine. Zero exceptKey means "match any".
	HasOtherBreakingLeases(handleKey string, exceptKey [16]byte) bool

	// AnyHolderHasLeaseBits reports whether any lease on handleKey (excluding
	// exceptKey) currently has any bit in mask set. Non-blocking peek used by
	// the SMB CREATE post-break park decision: per Samba `delay_for_oplock_fn`
	// (source3/smbd/open.c line 2458), a CREATE delays only if the existing
	// holder's lease type intersects the delay_mask, where:
	//   - sharing violation         → mask = SMB2_LEASE_HANDLE
	//   - non-violation (default,
	//     overwrite, destructive)   → mask = SMB2_LEASE_WRITE
	// Zero exceptKey means "match any".
	AnyHolderHasLeaseBits(handleKey string, exceptKey [16]byte, mask uint32) bool

	// SignalParkedCreates wakes any parked CREATE waiter on handleKey so it
	// re-evaluates its post-break gate. Used by the SMB CLOSE path after the
	// open-file table entry has been removed: a parked CREATE that was
	// waiting on a share-mode conflict with the closing holder must re-check
	// share-mode against the now-shrunk table. Idempotent — safe to call
	// even when no waiter exists.
	SignalParkedCreates(handleKey string)

	// ========================================================================
	// Break Callbacks
	// ========================================================================

	// RegisterBreakCallbacks registers typed callbacks for break notifications.
	RegisterBreakCallbacks(callbacks BreakCallbacks)

	// ========================================================================
	// Connection/Cleanup
	// ========================================================================

	// RemoveAllLocks removes all locks (both legacy and unified) for a file.
	RemoveAllLocks(handleKey string)

	// RemoveClientLocks removes all locks held by a specific client.
	RemoveClientLocks(clientID string)

	// GetStats returns current lock manager statistics.
	GetStats() ManagerStats
}

// ManagerStats contains statistics about the lock manager state.
type ManagerStats struct {
	// TotalLegacyLocks is the total number of legacy byte-range locks.
	TotalLegacyLocks int

	// TotalUnifiedLocks is the total number of unified locks.
	TotalUnifiedLocks int

	// TotalFiles is the number of files with any locks.
	TotalFiles int

	// BreakCallbackCount is the number of registered break callbacks.
	BreakCallbackCount int

	// GracePeriodActive indicates if grace period is active.
	GracePeriodActive bool
}

// HandleChecker checks if a file handle still exists in the metadata store.
// Used for lease reclaim validation (reject reclaim on deleted directories).
type HandleChecker interface {
	HandleExists(handle FileHandle) bool
}

// Verify Manager satisfies LockManager at compile time.
var _ LockManager = (*Manager)(nil)

// FileLock represents a byte-range lock on a file.
//
// Byte-range locks control what portions of a file can be read/written while
// locked by other clients. They are used by SMB2 LOCK command and NFS NLM protocol.
//
// Lock Types:
//   - Exclusive (write): No other locks allowed on overlapping range
//   - Shared (read): Multiple shared locks allowed, no exclusive locks
//
// Lock Lifetime:
// Locks are advisory and ephemeral (in-memory only). They persist until:
//   - Explicitly released via UnlockFile
//   - File is closed (UnlockAllForOpen)
//   - Session disconnects (cleanup all session locks)
//   - Server restarts (all locks lost)
type FileLock struct {
	// ID is the lock identifier from the client.
	// For SMB2: derived from lock request (often 0 for simple locks)
	// For NLM: opaque client-provided lock handle
	ID uint64

	// SessionID identifies the session that holds the lock.
	// For SMB2: SessionID from SMB header
	// For NLM: hash of network address + client PID
	// Used for session-level cleanup (UnlockAllForSession) and backward compatibility.
	SessionID uint64

	// OpenID identifies the specific open (file handle) that owns this lock.
	// Per MS-SMB2, byte-range locks are per-open, not per-session. Two opens
	// from the same session to the same file are independent lock owners.
	// For SMB2: hex-encoded FileID (unique per open)
	// For NLM/NFS: empty string (NFS uses session-level locking)
	// When empty, falls back to SessionID for ownership comparison.
	OpenID string

	// Offset is the starting byte offset of the lock.
	Offset uint64

	// Length is the number of bytes locked.
	// 0 means "to end of file" (unbounded).
	Length uint64

	// Exclusive indicates lock type.
	// true = exclusive (write lock, blocks all other locks)
	// false = shared (read lock, allows other shared locks)
	Exclusive bool

	// AcquiredAt is the time the lock was acquired.
	AcquiredAt time.Time

	// ClientAddr is the network address of the client holding the lock.
	// Used for debugging and logging.
	ClientAddr string

	// ClientID is the connection-tracker client identifier (e.g. "smb:7").
	// Used to purge a client's persisted locks on disconnect via
	// RemoveClientLocks → DeleteLocksByClient. The legacy byte-range path
	// (Manager.Lock) is SMB-only; SMB producers stamp "smb:{SessionID}" to
	// match the identity SMB session teardown passes to RemoveClientLocks.
	// Empty for locks that never need per-client cleanup.
	ClientID string

	// IsZeroByte marks this as a zero-byte lock (SMB2 Length=0).
	// Zero-byte locks never conflict with any other lock. They are stored
	// and require explicit unlock, but do not block other lock acquisitions.
	// NFS/NLM never sets this; NFS uses Length=0 for "to EOF" semantics.
	IsZeroByte bool

	// persistID is the manager-assigned persistent identity for this lock.
	// SMB stacks multiple identical (same owner/offset/length) shared locks,
	// each requiring a separate Unlock. A per-entry persistID keeps the
	// persisted record 1:1 with this in-memory entry so a partial unlock does
	// not drop a record while another stacked entry survives. Unexported and
	// never serialized on the wire — only used for lock-store round-trips.
	persistID string
}

// LockConflict describes a conflicting lock for error reporting.
//
// When LockFile or TestLock fails due to a conflict, this structure
// provides information about the conflicting lock. This can be used
// by protocols to report conflict details back to clients.
type LockConflict struct {
	// Offset is the starting byte offset of the conflicting lock.
	Offset uint64

	// Length is the number of bytes of the conflicting lock.
	Length uint64

	// Exclusive indicates type of conflicting lock.
	Exclusive bool

	// OwnerSessionID identifies the client holding the conflicting lock.
	OwnerSessionID uint64

	// OwnerID is the effective owner identifier of the conflicting lock
	// (per-open OpenID for SMB; "session:N" fallback for NFS/NLM). Used by
	// the SMB blocking-lock async-park path to feed deadlock-detection edges
	// into the Wait-For Graph (MS-SMB2 §3.3.5.14, smb2.lock.open-brlock-deadlock).
	OwnerID string
}

// lockOwnerID returns the effective owner identifier for a FileLock.
// If OpenID is set (SMB per-open locking), it is used.
// Otherwise, falls back to SessionID (NFS/NLM session-level locking).
func lockOwnerID(fl *FileLock) string {
	return callerOwnerID(fl.OpenID, fl.SessionID)
}

// callerOwnerID builds an owner identifier from an openID and sessionID pair.
// If openID is non-empty it is used directly; otherwise the sessionID is formatted.
// This is the shared logic behind lockOwnerID, CheckIOConflict, and Unlock.
func callerOwnerID(openID string, sessionID uint64) string {
	if openID != "" {
		return openID
	}
	return fmt.Sprintf("session:%d", sessionID)
}

// IsLockConflicting checks if two locks conflict with each other.
//
// Mirrors Samba brl_conflict (source3/locking/brlock.c):
//
//  1. Zero-byte + zero-byte → never conflict (regardless of offset).
//  2. Zero-byte vs non-zero → conflict iff zero-byte offset is strictly
//     inside the other range (Samba byte_range_overlap with last=ofs+len-1).
//  3. Read + Read → never conflict (multiple readers OK).
//  4. Same owner (same OpenID), existing Write, new Read → no conflict
//     ("a read lock can stack on top of a write lock", Samba comment).
//  5. Everything else → conflict iff ranges overlap.
//
// Per MS-SMB2, lock ownership is per-open (per FileID), not per-session. Two
// different opens from the same session are independent lock owners and MUST
// conflict with each other when acquiring exclusive locks on overlapping ranges.
func IsLockConflicting(existing, requested *FileLock) bool {
	// Compute overlap first, including zero-byte lock semantics.
	if !locksOverlap(existing, requested) {
		return false
	}

	// Read locks never conflict with each other.
	if !existing.Exclusive && !requested.Exclusive {
		return false
	}

	// Same owner handling. NFS/NLM (OpenID empty) uses session-level
	// ownership where same-process re-locking always succeeds (POSIX).
	// SMB (OpenID set) uses per-open ownership with restricted stacking
	// per Samba brl_conflict: only shared-on-exclusive from same open is
	// allowed; all other combos fall through to the overlap check.
	if lockOwnerID(existing) == lockOwnerID(requested) {
		if existing.OpenID == "" || requested.OpenID == "" {
			return false // NFS/NLM: same session, no conflict
		}
		if existing.Exclusive && !requested.Exclusive {
			return false // SMB: read stacks on write from same open
		}
		// SMB same-open: exclusive+exclusive or shared+exclusive conflict
		return true
	}

	// At least one is exclusive and ranges overlap — conflict.
	return true
}

// locksOverlap returns true if two FileLocks have overlapping byte ranges,
// correctly handling SMB2 zero-byte locks (IsZeroByte).
//
// Mirrors Samba byte_range_overlap (source3/locking/brlock.c) with
// inclusive-end semantics: last = offset + length - 1. A zero-byte lock at
// offset N produces an inverted range [N, N-1] that only overlaps with
// ranges spanning strictly across N (i.e., start < N < start+length).
//
// Two zero-byte locks never overlap. A zero-byte lock at offset 0 never
// overlaps anything (MS-FSA {0,0} special case).
func locksOverlap(a, b *FileLock) bool {
	// Two zero-byte locks never overlap each other.
	if a.IsZeroByte && b.IsZeroByte {
		return false
	}

	// One zero-byte, one non-zero: check if zero-byte offset is strictly
	// inside the other range. {0, 0} never overlaps (MS-FSA special case).
	if a.IsZeroByte || b.IsZeroByte {
		var zb, other *FileLock
		if a.IsZeroByte {
			zb, other = a, b
		} else {
			zb, other = b, a
		}

		// {0, 0} never overlaps anything.
		if zb.Offset == 0 {
			return false
		}

		// Samba inclusive-end: last = offset + length - 1.
		// For zero-byte lock: last = zb.Offset - 1 (inverted range).
		// Overlap iff other.Offset < zb.Offset AND otherEnd > zb.Offset.
		otherEnd := rangeEnd(other.Offset, other.Length)
		return other.Offset < zb.Offset && otherEnd > zb.Offset
	}

	// Both non-zero-byte: standard range overlap.
	return RangesOverlap(a.Offset, a.Length, b.Offset, b.Length)
}

// CheckIOConflict checks if an I/O operation conflicts with an existing lock.
//
// This implements SMB2 byte-range lock semantics per MS-FSA 2.1.4.10:
//   - Shared lock: Allows reads from all opens but blocks writes from ALL
//     opens, including the lock holder. This is the key difference from
//     POSIX advisory locks where a process's own locks never block its own I/O.
//   - Exclusive lock: Only the lock holder (same open) can read or write the range.
//
// Conflict rules (using openID for ownership, falling back to sessionID):
//   - READ + same open + any lock type = ALLOW
//   - READ + different open + shared lock = ALLOW
//   - READ + different open + exclusive lock = BLOCK
//   - WRITE + same open + exclusive lock = ALLOW (lock holder can write)
//   - WRITE + same open + shared lock = BLOCK (shared = read-only for everyone)
//   - WRITE + different open + any lock = BLOCK
//
// Parameters:
//   - existing: The lock to check against
//   - openID: The open identifier performing the I/O (empty string falls back to sessionID)
//   - sessionID: The session performing the I/O (used when openID is empty)
//   - offset: Starting byte offset of the I/O
//   - length: Number of bytes in the I/O
//   - isWrite: true for write operations, false for reads
//
// Returns true if the I/O is blocked by the existing lock.
func CheckIOConflict(existing *FileLock, openID string, sessionID uint64, offset, length uint64, isWrite bool) bool {
	// Zero-byte locks never block I/O — they have no actual byte range.
	if existing.IsZeroByte {
		return false
	}

	// Check range overlap first (common case: no overlap)
	if !RangesOverlap(existing.Offset, existing.Length, offset, length) {
		return false
	}

	// Determine if this is the same owner
	sameOwner := lockOwnerID(existing) == callerOwnerID(openID, sessionID)

	// Same owner handling
	if sameOwner {
		// Reads from the same open are always allowed regardless of lock type
		if !isWrite {
			return false
		}
		// Writes from the same open:
		// - Exclusive lock holder CAN write to their own locked range
		// - Non-exclusive (shared) lock holder CANNOT write; shared locks are read-only
		//   and prevent writes from all opens, including the holder.
		return !existing.Exclusive
	}

	// Different owner: writes are blocked by any lock (shared or exclusive)
	if isWrite {
		return true
	}

	// Different owner reads: only exclusive locks block
	return existing.Exclusive
}

// conflictFrom creates a LockConflict from a FileLock.
func conflictFrom(fl *FileLock) *LockConflict {
	return &LockConflict{
		Offset:         fl.Offset,
		Length:         fl.Length,
		Exclusive:      fl.Exclusive,
		OwnerSessionID: fl.SessionID,
		OwnerID:        lockOwnerID(fl),
	}
}

// Manager manages byte-range file locks for SMB/NLM protocols.
//
// This is a shared, in-memory implementation that can be embedded in any
// metadata store. Locks are ephemeral and lost on server restart.
//
// Manager implements the LockManager interface, providing unified lock
// management including byte-range locks, oplocks, grace period, and
// typed break callbacks.
//
// Thread Safety:
// Manager is safe for concurrent use by multiple goroutines.
type Manager struct {
	mu             sync.RWMutex
	locks          map[string][]FileLock     // handle key -> locks (legacy)
	unifiedLocks   map[string][]*UnifiedLock // handle key -> unified locks
	breakCallbacks []BreakCallbacks          // registered break callbacks
	gracePeriod    *GracePeriodManager       // grace period state (may be nil)
	handleChecker  HandleChecker             // checks if file handles still exist (for reclaim)
	lockStore      LockStore                 // persistent lock store (optional)
	epoch          uint64                    // current server epoch (stamped on persisted locks)
	shareName      string                    // share this manager serves (stamped on persisted byte-range locks)
	recentlyBroken *recentlyBrokenCache      // prevents directory lease storms

	// Delegation-related fields
	breakWaitChans          map[string]chan struct{} // per-handleKey channel for break wait
	delegationRecallTimeout time.Duration            // default 90s, configurable
}

// DefaultDelegationRecallTimeout is the default delegation recall timeout.
// NFS uses a longer timeout than SMB leases (90s vs 35s).
const DefaultDelegationRecallTimeout = 90 * time.Second

// persistTimeout bounds every synchronous lock-store call made under lm.mu.
// Persistence runs inline (mutex order == store order, see putLockLocked) so a
// hung backend would otherwise wedge the lock manager indefinitely; the timeout
// turns that into a bounded best-effort failure that logs and proceeds.
const persistTimeout = 3 * time.Second

// newBaseManager creates a Manager with all common fields initialized.
// Callers customize the returned Manager before use.
func newBaseManager(recentlyBrokenTTL time.Duration) *Manager {
	return &Manager{
		locks:                   make(map[string][]FileLock),
		unifiedLocks:            make(map[string][]*UnifiedLock),
		recentlyBroken:          newRecentlyBrokenCache(recentlyBrokenTTL),
		breakWaitChans:          make(map[string]chan struct{}),
		delegationRecallTimeout: DefaultDelegationRecallTimeout,
	}
}

// NewManager creates a new lock manager.
func NewManager() *Manager {
	return newBaseManager(defaultRecentlyBrokenTTL)
}

// NewManagerWithTTL creates a new lock manager with a custom recently-broken TTL.
// Primarily used in tests to avoid waiting for the default 5-second TTL.
func NewManagerWithTTL(recentlyBrokenTTL time.Duration) *Manager {
	return newBaseManager(recentlyBrokenTTL)
}

// NewManagerWithGracePeriod creates a new lock manager with a grace period manager.
func NewManagerWithGracePeriod(gracePeriod *GracePeriodManager) *Manager {
	m := newBaseManager(defaultRecentlyBrokenTTL)
	m.gracePeriod = gracePeriod
	return m
}

// Lock attempts to acquire a byte-range lock on a file.
//
// This is a low-level CRUD operation with no permission checking.
// Business logic (permission checks, file type validation) should be
// performed by the caller.
//
// Returns nil on success, or ErrLocked if a conflict exists.
//
// Persistence is synchronous under lm.mu: the in-memory mutation and the
// PutLock run while the mutex is held so the store sees mutations in the same
// order the mutex serializes them. Two concurrent ops on the same persistID can
// no longer reach the store out of order (the reorder/resurrection bug class).
func (lm *Manager) Lock(handleKey string, lock FileLock) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handleKey]

	// Check for conflicts with existing locks
	for i := range existing {
		if IsLockConflicting(&existing[i], &lock) {
			return NewLockedError("", conflictFrom(&existing[i]))
		}
	}

	// NFS/NLM (OpenID empty): POSIX semantics — re-locking the same range
	// from the same session replaces the existing lock in place.
	// SMB (OpenID set): Windows semantics — every Lock call stacks a new
	// entry even when (owner, offset, length, type) match. Each entry
	// requires a separate Unlock call. Per MS-SMB2 §3.3.5.14 and Samba
	// brl_lock_windows (source3/locking/brlock.c).
	if lock.OpenID == "" {
		for i := range existing {
			if lockOwnerID(&existing[i]) == lockOwnerID(&lock) &&
				existing[i].Offset == lock.Offset &&
				existing[i].Length == lock.Length {
				// Update existing lock in place (NFS/POSIX re-lock)
				existing[i].Exclusive = lock.Exclusive
				existing[i].AcquiredAt = time.Now()
				existing[i].ID = lock.ID
				lm.assignPersistIDLocked(&existing[i])
				lm.persistFileLockLocked(handleKey, &existing[i])
				return nil
			}
		}
	}

	// Set acquisition time if not set
	if lock.AcquiredAt.IsZero() {
		lock.AcquiredAt = time.Now()
	}

	// Add new lock. A distinct persistID per stacked entry keeps the persisted
	// record 1:1 with this slice entry (SMB shared-lock stacking).
	lm.assignPersistIDLocked(&lock)
	lm.locks[handleKey] = append(existing, lock)
	lm.persistFileLockLocked(handleKey, &lock)
	return nil
}

// Unlock releases a specific byte-range lock.
//
// The lock is identified by openID (or sessionID if openID is empty), offset,
// and length - all must match exactly.
//
// Returns nil on success, or ErrLockNotFound if the lock wasn't found.
func (lm *Manager) Unlock(handleKey string, openID string, sessionID uint64, offset, length uint64) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return NewLockNotFoundError("")
	}

	// Find and remove the matching lock. For stacked identical SMB shared
	// locks the first match is removed; its distinct persistID ensures only
	// that one persisted record is dropped, leaving the rest of the stack.
	owner := callerOwnerID(openID, sessionID)
	for i := range existing {
		if lockOwnerID(&existing[i]) == owner &&
			existing[i].Offset == offset &&
			existing[i].Length == length {
			lm.deleteFileLockLocked(&existing[i])
			// Remove this lock
			lm.locks[handleKey] = append(existing[:i], existing[i+1:]...)

			// Clean up empty entries to prevent memory leak
			if len(lm.locks[handleKey]) == 0 {
				delete(lm.locks, handleKey)
			}
			return nil
		}
	}

	return NewLockNotFoundError("")
}

// UnlockAllForOpen releases all locks held by a specific open on a file.
//
// Returns the number of locks released.
func (lm *Manager) UnlockAllForOpen(handleKey string, openID string) int {
	if openID == "" {
		return 0 // empty openID would match all unset locks — guard against misuse
	}
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return 0
	}

	// Filter out locks belonging to this open
	remaining := make([]FileLock, 0, len(existing))
	removed := 0
	for i := range existing {
		if existing[i].OpenID == openID {
			lm.deleteFileLockLocked(&existing[i])
			removed++
		} else {
			remaining = append(remaining, existing[i])
		}
	}

	// Update or clean up
	if len(remaining) == 0 {
		delete(lm.locks, handleKey)
	} else {
		lm.locks[handleKey] = remaining
	}

	return removed
}

// UnlockAllForSession releases all locks held by a session on a file.
//
// Returns the number of locks released.
func (lm *Manager) UnlockAllForSession(handleKey string, sessionID uint64) int {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return 0
	}

	// Filter out locks belonging to this session
	remaining := make([]FileLock, 0, len(existing))
	removed := 0
	for i := range existing {
		if existing[i].SessionID == sessionID {
			lm.deleteFileLockLocked(&existing[i])
			removed++
		} else {
			remaining = append(remaining, existing[i])
		}
	}

	// Update or clean up
	if len(remaining) == 0 {
		delete(lm.locks, handleKey)
	} else {
		lm.locks[handleKey] = remaining
	}

	return removed
}

// TestLock checks if a lock would succeed without acquiring it.
//
// Returns (*LockConflict, nil) if conflict exists, or (nil, nil) if lock would succeed.
func (lm *Manager) TestLock(handleKey string, lock FileLock) (*LockConflict, error) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handleKey]

	for i := range existing {
		if IsLockConflicting(&existing[i], &lock) {
			return conflictFrom(&existing[i]), nil
		}
	}

	return nil, nil
}

// TestLockByParams checks if a lock would succeed without acquiring it (legacy params).
//
// Returns (true, nil) if lock would succeed, (false, conflict) if conflict exists.
func (lm *Manager) TestLockByParams(handleKey string, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict) {
	testLock := FileLock{
		SessionID: sessionID,
		Offset:    offset,
		Length:    length,
		Exclusive: exclusive,
	}

	conflict, _ := lm.TestLock(handleKey, testLock)
	if conflict != nil {
		return false, conflict
	}
	return true, nil
}

// CheckForIO checks if an I/O operation would conflict with existing locks.
//
// Returns nil if I/O is allowed, or conflict details if blocked.
func (lm *Manager) CheckForIO(handleKey string, openID string, sessionID uint64, offset, length uint64, isWrite bool) *LockConflict {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handleKey]

	for i := range existing {
		if CheckIOConflict(&existing[i], openID, sessionID, offset, length, isWrite) {
			return conflictFrom(&existing[i])
		}
	}

	return nil
}

// ListLocks returns all active locks on a file.
//
// Returns nil if no locks exist.
func (lm *Manager) ListLocks(handleKey string) []FileLock {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return nil
	}

	// Return a copy to avoid race conditions
	result := make([]FileLock, len(existing))
	copy(result, existing)
	return result
}

// RemoveFileLocks removes all locks for a file.
//
// Called when a file is deleted to clean up any stale lock entries.
func (lm *Manager) RemoveFileLocks(handleKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.locks, handleKey)
}

// SetDelegationRecallTimeout sets the delegation recall timeout (thread-safe).
func (lm *Manager) SetDelegationRecallTimeout(d time.Duration) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.delegationRecallTimeout = d
}

// DelegationRecallTimeout returns the current delegation recall timeout (thread-safe).
func (lm *Manager) DelegationRecallTimeout() time.Duration {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.delegationRecallTimeout
}

// SetHandleChecker sets the handle checker used for lease reclaim validation.
func (lm *Manager) SetHandleChecker(hc HandleChecker) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.handleChecker = hc
}

// SetLockStore sets the persistent lock store for lease persistence.
func (lm *Manager) SetLockStore(store LockStore) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.lockStore = store
}

// SetEpoch records the current server epoch stamped on persisted locks.
func (lm *Manager) SetEpoch(epoch uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.epoch = epoch
}

// SetShareName records the share this manager serves. The share name is
// stamped on persisted byte-range locks so they can be recovered by the
// per-share ListLocks query at startup.
func (lm *Manager) SetShareName(shareName string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.shareName = shareName
}

// assignPersistIDLocked stamps a fresh UUID persistent ID on a byte-range lock
// if it does not already have one. A UUID (rather than a deterministic
// handleKey:owner:range#seq format backed by an in-memory counter) is required
// for restart safety: the counter reset to 0 on a fresh Manager and was never
// restored, so a new stacked lock after a restart regenerated a persistID
// identical to a restored one — the id-keyed PutLock upsert then overwrote the
// restored record, resurfacing the stacked-unlock data-loss bug (R3-2). A UUID
// has no collision surface across restarts. The id round-trips through
// PersistedLock.ID (fileLockFromPersisted restores it), so a later Unlock still
// deletes exactly the matching record. Caller must hold lm.mu.
func (lm *Manager) assignPersistIDLocked(fl *FileLock) {
	if fl.persistID != "" {
		return
	}
	fl.persistID = uuid.New().String()
}

// withPersistTimeout returns a context bounded by persistTimeout so a hung
// backend can never wedge the lock manager: the store call runs synchronously
// under lm.mu (mutex order == store order), but the timeout caps how long it
// can block the critical section.
func withPersistTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), persistTimeout)
}

// fileLockToPersisted builds a PersistedLock from a byte-range FileLock.
// fl.persistID must already be assigned (see assignPersistIDLocked).
func (lm *Manager) fileLockToPersisted(handleKey string, fl *FileLock) *PersistedLock {
	lockType := LockTypeShared
	if fl.Exclusive {
		lockType = LockTypeExclusive
	}
	return &PersistedLock{
		ID:                fl.persistID,
		ShareName:         lm.shareName,
		FileID:            handleKey,
		OwnerID:           lockOwnerID(fl),
		ClientID:          fl.ClientID,
		LockType:          int(lockType),
		Offset:            fl.Offset,
		Length:            fl.Length,
		IsZeroByte:        fl.IsZeroByte,
		IsLegacyByteRange: true,
		AcquiredAt:        fl.AcquiredAt,
		ServerEpoch:       lm.epoch,
	}
}

// persistFileLockLocked synchronously persists a byte-range lock. No-op if
// persistence is disabled. Caller must hold lm.mu.
func (lm *Manager) persistFileLockLocked(handleKey string, fl *FileLock) {
	if lm.lockStore == nil {
		return
	}
	lm.putLockLocked(lm.fileLockToPersisted(handleKey, fl))
}

// deleteFileLockLocked synchronously removes a persisted byte-range lock. No-op
// if persistence is disabled or the lock was never persisted. Caller must hold
// lm.mu.
func (lm *Manager) deleteFileLockLocked(fl *FileLock) {
	if lm.lockStore == nil || fl.persistID == "" {
		return
	}
	lm.deletePersistedLocked(fl.persistID)
}

// persistUnifiedLockLocked synchronously persists a unified lock. No-op if
// persistence is disabled. Caller must hold lm.mu.
//
// The share name is stamped from lm.shareName rather than trusting the
// producer's Owner.ShareName: NFSv4/NLM byte-range producers build LockOwner
// with ShareName="" (the byte-range path never carries it), which would make
// the lock invisible to the per-share recovery query (ListLocks{ShareName})
// and silently drop it on restart. Since each Manager serves exactly one
// share, lm.shareName is authoritative for every lock it holds; this matches
// how the legacy byte-range path (fileLockToPersisted) already stamps it. The
// override is skipped when lm.shareName is empty so a directly-constructed
// manager preserves a producer-set Owner.ShareName.
func (lm *Manager) persistUnifiedLockLocked(ul *UnifiedLock) {
	if lm.lockStore == nil {
		return
	}
	pl := ToPersistedLock(ul, lm.epoch)
	if lm.shareName != "" {
		pl.ShareName = lm.shareName
	}
	lm.putLockLocked(pl)
}

// deleteUnifiedLockLocked synchronously removes a persisted unified lock. No-op
// if persistence is disabled. Caller must hold lm.mu.
func (lm *Manager) deleteUnifiedLockLocked(ul *UnifiedLock) {
	if lm.lockStore == nil {
		return
	}
	lm.deletePersistedLocked(ul.ID)
}

// putLockLocked persists one record synchronously under lm.mu, bounded by
// persistTimeout. Caller must hold lm.mu and must have already applied the
// in-memory mutation, so the store observes mutations in mutex order — this is
// what eliminates the reorder/resurrection bug class (R3-1).
//
// Persistence is BEST-EFFORT. The in-memory lock map is authoritative for the
// running server, so a failed PutLock must NOT fail the lock op — the client is
// told the (advisory) lock is held and it is, in this process. The only
// consequence of a failed persist is durability across restart: the lock
// survives in memory but is lost on restart, after which a conflicting lock
// could be granted. The operator MUST treat these ERROR logs as a durability
// alarm. Errors are logged with file/owner context so they are observable.
func (lm *Manager) putLockLocked(pl *PersistedLock) {
	ctx, cancel := withPersistTimeout()
	defer cancel()
	if err := lm.lockStore.PutLock(ctx, pl); err != nil {
		logger.Error("lock persistence failed: lock held in memory but NOT durable across restart",
			"lockID", pl.ID,
			"share", pl.ShareName,
			"fileID", pl.FileID,
			"ownerID", pl.OwnerID,
			"error", err)
	}
}

// deletePersistedLocked removes one record by ID synchronously under lm.mu,
// bounded by persistTimeout. Caller must hold lm.mu and must have already
// applied the in-memory removal (mutex order == store order).
//
// Best-effort with the same contract as putLockLocked: a failed DeleteLock
// means a released lock may resurrect on restart until the next successful
// overwrite/cleanup. ErrLockNotFound is ignored — the record is already gone.
func (lm *Manager) deletePersistedLocked(id string) {
	ctx, cancel := withPersistTimeout()
	defer cancel()
	if err := lm.lockStore.DeleteLock(ctx, id); err != nil && !isLockNotFound(err) {
		logger.Error("lock-delete persistence failed: released lock may resurrect on restart",
			"lockID", id,
			"error", err)
	}
}

// RestoreLocks loads previously-persisted locks back into the in-memory lock
// maps after a restart. Records are routed by shape: lease/delegation records
// (LeaseKey or DelegationID present) repopulate unifiedLocks; plain byte-range
// records repopulate the legacy locks map so the byte-range ops (Lock/Unlock/
// TestLock/CheckForIO) — which consult lm.locks, not lm.unifiedLocks — enforce
// them after restart. Locks are inserted without conflict checking: prior-run
// locks are by definition conflict-free with each other.
func (lm *Manager) RestoreLocks(persisted []*PersistedLock) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	for _, pl := range persisted {
		// pl.FileID is the handle key used when persisting (see persist helpers).
		// Legacy SMB byte-range records belong in lm.locks (consulted by
		// Lock/Unlock/TestLock/CheckForIO); leases, delegations and NLM/NFSv4
		// unified locks belong in lm.unifiedLocks.
		if pl.IsLegacyByteRange {
			lm.locks[pl.FileID] = append(lm.locks[pl.FileID], fileLockFromPersisted(pl))
			continue
		}
		lm.unifiedLocks[pl.FileID] = append(lm.unifiedLocks[pl.FileID], FromPersistedLock(pl))
	}
	return nil
}

// fileLockFromPersisted reconstructs a byte-range FileLock from a persisted
// record. The owner identity is recovered from OwnerID: SMB locks store the
// per-open OpenID directly; NFS/NLM locks store "session:N" (see
// callerOwnerID), from which the SessionID is recovered. The persistID is
// restored so a later Unlock deletes the correct record.
func fileLockFromPersisted(pl *PersistedLock) FileLock {
	fl := FileLock{
		Offset:     pl.Offset,
		Length:     pl.Length,
		Exclusive:  LockType(pl.LockType) == LockTypeExclusive,
		IsZeroByte: pl.IsZeroByte,
		AcquiredAt: pl.AcquiredAt,
		ClientID:   pl.ClientID,
		persistID:  pl.ID,
	}
	if sid, ok := sessionIDFromOwnerID(pl.OwnerID); ok {
		fl.SessionID = sid
	} else {
		// SMB per-open lock: OpenID is recovered from OwnerID. SessionID is
		// NOT restored (it is not a PersistedLock field) and stays 0 (R3-6).
		// This is latent, not a live bug: SMB lock teardown keys on OpenID
		// (UnlockAllForOpen) and ClientID (RemoveClientLocks), both of which
		// ARE preserved. SessionID-keyed cleanup (UnlockAllForSession) is an
		// NFS/NLM path and those locks restore their SessionID above. Adding a
		// session_id column purely to round-trip an unused field is not worth
		// the schema churn at this stage.
		fl.OpenID = pl.OwnerID
	}
	return fl
}

// sessionIDFromOwnerID parses a "session:N" owner identifier (NFS/NLM) back
// into its numeric SessionID. Returns false for SMB per-open owner IDs.
func sessionIDFromOwnerID(ownerID string) (uint64, bool) {
	const prefix = "session:"
	if !strings.HasPrefix(ownerID, prefix) {
		return 0, false
	}
	sid, err := strconv.ParseUint(ownerID[len(prefix):], 10, 64)
	if err != nil {
		return 0, false
	}
	return sid, true
}

// ============================================================================
// Lease Operations (implementations in leases.go and reclaim.go)
// ============================================================================

// RequestLease requests a new or upgraded lease on a file or directory.
func (lm *Manager) RequestLease(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
	parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
	requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error) {
	return lm.requestLeaseImpl(ctx, fileHandle, leaseKey, parentLeaseKey, ownerID, clientID, shareName, requestedState, isDirectory)
}

// RequestLeaseAsOplock is the traditional-oplock variant of RequestLease.
// The SMB adapter calls this when a CREATE arrives with a non-Lease
// OplockLevel (LEVEL_II / Exclusive / Batch); the new record is tagged
// `IsTraditionalOplock=true` so subsequent grants observe the cross-tier
// rules described in `bestGrantableState`. All other parameters and
// semantics match `RequestLease`.
//
// Reference: MS-SMB2 §3.3.5.9 / Samba `source3/smbd/open.c::grant_fsp_oplock_type`.
func (lm *Manager) RequestLeaseAsOplock(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
	parentLeaseKey [16]byte, ownerID string, clientID string, shareName string,
	requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error) {
	return lm.requestLeaseImplWithMode(ctx, fileHandle, leaseKey, parentLeaseKey,
		ownerID, clientID, shareName, requestedState, isDirectory, true)
}

// AcknowledgeLeaseBreak processes a client's lease break acknowledgment.
func (lm *Manager) AcknowledgeLeaseBreak(ctx context.Context, leaseKey [16]byte,
	acknowledgedState uint32, epoch uint16) error {
	return lm.acknowledgeLeaseBreakImpl(ctx, leaseKey, acknowledgedState, epoch)
}

// ReleaseLease releases all lease state for the given lease key.
func (lm *Manager) ReleaseLease(ctx context.Context, leaseKey [16]byte) error {
	return lm.releaseLeaseImpl(ctx, leaseKey)
}

// ReclaimLease reclaims a lease during grace period.
func (lm *Manager) ReclaimLease(ctx context.Context, leaseKey [16]byte,
	requestedState uint32, isDirectory bool) (*UnifiedLock, error) {
	return lm.reclaimLeaseImpl(ctx, leaseKey, requestedState, isDirectory)
}

// ReleaseLeaseForHandle removes lease records matching leaseKey from a
// single handleKey bucket only. See releaseLeaseForHandleImpl for details.
func (lm *Manager) ReleaseLeaseForHandle(ctx context.Context, handleKey string, leaseKey [16]byte) error {
	return lm.releaseLeaseForHandleImpl(ctx, handleKey, leaseKey)
}

// GetLeaseState returns the current state and epoch for a lease key.
func (lm *Manager) GetLeaseState(ctx context.Context, leaseKey [16]byte) (state uint32, epoch uint16, found bool) {
	return lm.getLeaseStateImpl(ctx, leaseKey)
}

// SetLeaseEpoch sets the epoch on an existing lease identified by leaseKey.
// Per MS-SMB2 3.3.5.9: For V2 leases, the server should track the client's
// epoch from the RqLs create context. SetLeaseEpoch is called after RequestLease
// to initialize the epoch to the client's requested value.
// Returns false if no lease was found with the given key.
func (lm *Manager) SetLeaseEpoch(leaseKey [16]byte, epoch uint16) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Update every lease record matching leaseKey, not just the first found.
	// Stale records from prior tests (same LEASE1 constant across smbtorture
	// tests) or from multiple opens by different clients can coexist in
	// lm.unifiedLocks under different handleKey buckets. findLeaseByKey's
	// map-iteration order is non-deterministic, so scoping to the first
	// match can miss the lease that RequestLease just granted — leaving it
	// at Epoch=1 (createAndGrantLease default) while the response to the
	// client carries the higher requested epoch. Subsequent break
	// notifications then dispatch with Epoch=2 instead of requestedEpoch+2,
	// regressing smbtorture V2 tests (break_twice, breaking*, v2_breaking3).
	found := false
	for _, locks := range lm.unifiedLocks {
		for _, lock := range locks {
			if lock.Lease == nil || lock.Lease.LeaseKey != leaseKey {
				continue
			}
			if epoch >= lock.Lease.Epoch {
				lock.Lease.Epoch = epoch
			}
			found = true
		}
	}
	return found
}

// ============================================================================
// POSIX Lock Splitting
// ============================================================================

// SplitLock splits an existing lock when a portion is unlocked.
//
// POSIX semantics require that unlocking a portion of a locked range results in:
//   - 0 locks: if the unlock range covers the entire lock
//   - 1 lock: if the unlock range covers the start or end
//   - 2 locks: if the unlock range is in the middle (creates a "hole")
//
// Parameters:
//   - existing: The lock to split
//   - unlockOffset: Starting byte offset of the unlock range
//   - unlockLength: Number of bytes to unlock (0 = to EOF)
//
// Returns:
//   - []UnifiedLock: The resulting locks after the split (0, 1, or 2 locks)
//
// Examples:
//   - Lock [0-100], Unlock [0-100] -> [] (exact match)
//   - Lock [0-100], Unlock [0-50] -> [[50-100]] (unlock at start)
//   - Lock [0-100], Unlock [50-100] -> [[0-50]] (unlock at end)
//   - Lock [0-100], Unlock [25-75] -> [[0-25], [75-100]] (unlock in middle)
func SplitLock(existing *UnifiedLock, unlockOffset, unlockLength uint64) []*UnifiedLock {
	// Check if ranges overlap at all
	if !RangesOverlap(existing.Offset, existing.Length, unlockOffset, unlockLength) {
		// No overlap - return existing lock unchanged
		return []*UnifiedLock{existing.Clone()}
	}

	// Calculate lock end
	lockEnd := existing.End()
	if existing.Length == 0 {
		// Unbounded lock - treat as very large for calculation purposes
		lockEnd = ^uint64(0) // Max uint64
	}

	// Calculate unlock end
	unlockEnd := unlockOffset + unlockLength
	if unlockLength == 0 {
		// Unbounded unlock - goes to EOF
		unlockEnd = ^uint64(0)
	}

	// Check for exact match or complete coverage
	if unlockOffset <= existing.Offset && unlockEnd >= lockEnd {
		// Unlock completely covers the lock - remove it
		return []*UnifiedLock{}
	}

	var result []*UnifiedLock

	// Each fragment is a distinct lock and MUST get a fresh ID. Clone() copies
	// the original's ID verbatim, so two fragments would otherwise share one
	// persist identity — the second PutLock would overwrite the first (the
	// store is keyed by ID), silently losing one byte-range across a restart.
	// Check if there's a portion before the unlock range
	if unlockOffset > existing.Offset {
		beforeLock := existing.Clone()
		beforeLock.ID = uuid.New().String()
		beforeLock.Length = unlockOffset - existing.Offset
		result = append(result, beforeLock)
	}

	// Check if there's a portion after the unlock range
	if unlockEnd < lockEnd {
		afterLock := existing.Clone()
		afterLock.ID = uuid.New().String()
		afterLock.Offset = unlockEnd
		if existing.Length == 0 {
			// Original was unbounded, after portion is also unbounded
			afterLock.Length = 0
		} else {
			afterLock.Length = lockEnd - unlockEnd
		}
		result = append(result, afterLock)
	}

	return result
}

// ============================================================================
// Lock Merging
// ============================================================================

// MergeLocks coalesces adjacent or overlapping locks from the same owner.
//
// This is used when upgrading or extending locks to avoid fragmentation.
// Only locks with the same owner, type, and file handle can be merged.
//
// Parameters:
//   - locks: Slice of locks to potentially merge
//
// Returns:
//   - []UnifiedLock: Merged locks (may have fewer elements than input)
func MergeLocks(locks []*UnifiedLock) []*UnifiedLock {
	if len(locks) == 0 {
		return nil
	}
	if len(locks) == 1 {
		return []*UnifiedLock{locks[0].Clone()}
	}

	// Group locks by owner+type+filehandle
	type groupKey struct {
		ownerID    string
		lockType   LockType
		fileHandle string
	}

	groups := make(map[groupKey][]*UnifiedLock)
	for _, lock := range locks {
		key := groupKey{
			ownerID:    lock.Owner.OwnerID,
			lockType:   lock.Type,
			fileHandle: string(lock.FileHandle),
		}
		groups[key] = append(groups[key], lock)
	}

	var result []*UnifiedLock

	for _, group := range groups {
		merged := mergeRanges(group)
		result = append(result, merged...)
	}

	return result
}

// mergeRanges merges locks that have the same owner/type/file.
// It combines overlapping or adjacent ranges into single locks.
func mergeRanges(locks []*UnifiedLock) []*UnifiedLock {
	if len(locks) == 0 {
		return nil
	}
	if len(locks) == 1 {
		return []*UnifiedLock{locks[0].Clone()}
	}

	// Sort by offset
	sorted := make([]*UnifiedLock, len(locks))
	for i, l := range locks {
		sorted[i] = l.Clone()
	}
	slices.SortFunc(sorted, func(a, b *UnifiedLock) int {
		return cmp.Compare(a.Offset, b.Offset)
	})

	var result []*UnifiedLock
	current := sorted[0]

	for i := 1; i < len(sorted); i++ {
		next := sorted[i]

		// Check if current and next can be merged
		if canMerge(current, next) {
			// Merge into current
			current = mergeTwoLocks(current, next)
		} else {
			// Can't merge - finalize current and move to next
			result = append(result, current)
			current = next
		}
	}

	// Don't forget the last one
	result = append(result, current)

	return result
}

// canMerge checks if two locks can be merged (adjacent or overlapping).
func canMerge(a, b *UnifiedLock) bool {
	// Must be same owner, type, and file (assumed by caller grouping)

	// Handle unbounded locks
	if a.Length == 0 {
		// a is unbounded - can merge with anything at or after a.Offset
		return b.Offset >= a.Offset
	}
	if b.Length == 0 {
		// b is unbounded - can merge if a overlaps or is adjacent to b.Offset
		return a.End() >= b.Offset
	}

	// Both bounded - check if adjacent or overlapping
	aEnd := a.End()
	return aEnd >= b.Offset // Adjacent (aEnd == b.Offset) or overlapping
}

// mergeTwoLocks combines two locks into one.
func mergeTwoLocks(a, b *UnifiedLock) *UnifiedLock {
	result := a.Clone()

	// Start is the minimum offset
	result.Offset = min(a.Offset, b.Offset)

	// Handle unbounded locks
	if a.Length == 0 || b.Length == 0 {
		result.Length = 0 // Result is unbounded
		return result
	}

	// Both bounded - end is the maximum
	maxEnd := max(a.End(), b.End())

	result.Length = maxEnd - result.Offset
	return result
}

// ============================================================================
// Atomic Lock Upgrade
// ============================================================================

// UpgradeLock atomically converts a shared lock to exclusive if no other readers exist.
//
// This implements the user decision: "Lock upgrade: Atomic upgrade supported
// (read -> write if no other readers)".
//
// Steps:
//  1. Find existing shared lock owned by `owner` covering the range
//  2. Check if any OTHER owners hold shared locks on overlapping range
//  3. If other readers exist: return ErrLockConflict
//  4. If no other readers: atomically change lock type to Exclusive
//
// Parameters:
//   - handleKey: The file handle key
//   - owner: The lock owner requesting the upgrade
//   - offset: Starting byte offset of the range to upgrade
//   - length: Number of bytes (0 = to EOF)
//
// Returns:
//   - *UnifiedLock: The upgraded lock on success
//   - error: ErrLockConflict if other readers exist, ErrLockNotFound if no lock to upgrade
//
// PRECONDITION — whole-lock upgrade only. Step 3 flips the ENTIRE matched
// shared lock to exclusive, not just the [offset,length) sub-range, and that
// whole-range promotion is what gets persisted. This is correct ONLY because
// every caller upgrades a lock at exactly the range it was granted at: the
// NFSv4 LOCK upgrade path promotes the lock-owner's existing lock as a whole,
// never a strict sub-range of a larger shared lock. If a caller ever passes a
// sub-range of a wider shared lock, the surrounding bytes would be silently
// promoted to exclusive (and persisted that way) — split the matched lock into
// the upgraded sub-range plus shared remainder(s) before changing this. There
// are currently no production callers (NFSv4/NLM upgrade is not yet wired); the
// invariant is enforced by TestUpgradeLock_WholeLockOnly.
func (lm *Manager) UpgradeLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	unifiedLocks := lm.getUnifiedLocksLocked(handleKey)

	// Step 1: Find existing shared lock owned by this owner covering the range
	var ownLock *UnifiedLock
	var ownLockIndex = -1

	for i, lock := range unifiedLocks {
		if lock.Owner.OwnerID == owner.OwnerID &&
			lock.Type == LockTypeShared &&
			lock.Overlaps(offset, length) {
			// Found our shared lock
			ownLock = lock
			ownLockIndex = i
			break
		}
	}

	if ownLock == nil {
		// Check if we already have an exclusive lock (no-op case)
		for _, lock := range unifiedLocks {
			if lock.Owner.OwnerID == owner.OwnerID &&
				lock.Type == LockTypeExclusive &&
				lock.Overlaps(offset, length) {
				// Already exclusive - return it as-is
				return lock.Clone(), nil
			}
		}
		return nil, NewLockNotFoundError("")
	}

	// Step 2: Check if any OTHER owners hold shared locks on overlapping range
	for _, lock := range unifiedLocks {
		if lock.Owner.OwnerID == owner.OwnerID {
			continue // Skip our own locks
		}
		if lock.Overlaps(offset, length) {
			// Another owner has a lock on this range - cannot upgrade
			return nil, NewLockConflictError("", &UnifiedLockConflict{
				Lock:   lock,
				Reason: "other reader exists on range",
			})
		}
	}

	// Step 3: Atomically upgrade the lock
	unifiedLocks[ownLockIndex].Type = LockTypeExclusive

	// Persist the upgraded type under lm.mu so the change survives a restart.
	// Without this the in-memory lock reverted to shared on restart and a
	// reader could be wrongly granted against an intended-exclusive lock (R3-3).
	lm.persistUnifiedLockLocked(unifiedLocks[ownLockIndex])

	return unifiedLocks[ownLockIndex].Clone(), nil
}

// getUnifiedLocksLocked returns unified locks for a file (must hold lm.mu).
func (lm *Manager) getUnifiedLocksLocked(handleKey string) []*UnifiedLock {
	return lm.unifiedLocks[handleKey]
}

// AddUnifiedLock adds a unified lock to the storage.
//
// Checks for conflicts using the ConflictsWith method which handles all 4
// conflict cases: access modes, oplock-oplock, oplock-byterange, byterange-byterange.
func (lm *Manager) AddUnifiedLock(handleKey string, lock *UnifiedLock) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.unifiedLocks[handleKey]

	// Check for conflicts with existing locks using ConflictsWith
	for _, el := range existing {
		if lock.ConflictsWith(el) {
			return NewLockConflictError("", &UnifiedLockConflict{
				Lock:   el,
				Reason: "lock conflict",
			})
		}
	}

	// Check if this exact lock already exists (same owner, offset, length)
	// If so, update it (allows changing lock type)
	for i, el := range existing {
		if el.Owner.OwnerID == lock.Owner.OwnerID &&
			el.Offset == lock.Offset &&
			el.Length == lock.Length {
			// Update existing lock in place
			existing[i].Type = lock.Type
			existing[i].AcquiredAt = time.Now()
			lm.persistUnifiedLockLocked(existing[i])
			return nil
		}
	}

	// Set acquisition time if not set
	if lock.AcquiredAt.IsZero() {
		lock.AcquiredAt = time.Now()
	}

	// Add new lock
	lm.unifiedLocks[handleKey] = append(existing, lock)
	lm.persistUnifiedLockLocked(lock)
	return nil
}

// RemoveUnifiedLock removes a unified lock using POSIX splitting semantics.
func (lm *Manager) RemoveUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.unifiedLocks[handleKey]
	if len(existing) == 0 {
		return NewLockNotFoundError("")
	}

	var newLocks []*UnifiedLock
	found := false

	for _, lock := range existing {
		if lock.Owner.OwnerID != owner.OwnerID {
			// Not our lock - keep it
			newLocks = append(newLocks, lock)
			continue
		}

		// Our lock - check if it overlaps with the unlock range
		if !lock.Overlaps(offset, length) {
			// Doesn't overlap - keep it unchanged
			newLocks = append(newLocks, lock)
			continue
		}

		// Overlaps - split the lock. Delete the original record, then persist
		// each fragment (each carries a fresh UUID from SplitLock). Done under
		// lm.mu so the store sees delete-then-puts in mutation order.
		found = true
		lm.deleteUnifiedLockLocked(lock)
		splitResult := SplitLock(lock, offset, length)
		for _, frag := range splitResult {
			lm.persistUnifiedLockLocked(frag)
		}
		newLocks = append(newLocks, splitResult...)
	}

	if !found {
		return NewLockNotFoundError("")
	}

	// Update or clean up
	if len(newLocks) == 0 {
		delete(lm.unifiedLocks, handleKey)
	} else {
		lm.unifiedLocks[handleKey] = newLocks
	}

	return nil
}

// ListUnifiedLocks returns all unified locks on a file.
func (lm *Manager) ListUnifiedLocks(handleKey string) []*UnifiedLock {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.unifiedLocks[handleKey]
	if len(existing) == 0 {
		return nil
	}

	// Return a copy to avoid race conditions
	result := make([]*UnifiedLock, len(existing))
	for i, el := range existing {
		result[i] = el.Clone()
	}
	return result
}

// RemoveFileUnifiedLocks removes all unified locks, delegations, and break
// wait channels for a file.
func (lm *Manager) RemoveFileUnifiedLocks(handleKey string) {
	lm.mu.Lock()
	delete(lm.unifiedLocks, handleKey)
	delete(lm.breakWaitChans, handleKey)
	lm.mu.Unlock()
}

// GetUnifiedLock retrieves a specific unified lock by owner and range.
//
// Returns the matching lock or ErrLockNotFound if no matching lock exists.
func (lm *Manager) GetUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	for _, lock := range lm.unifiedLocks[handleKey] {
		if lock.Owner.OwnerID == owner.OwnerID &&
			lock.Offset == offset &&
			lock.Length == length {
			return lock.Clone(), nil
		}
	}

	return nil, NewLockNotFoundError("")
}

// CheckAndBreakOpLocksForWrite checks and initiates breaks for oplocks that
// conflict with a write operation. Backward-compatible wrapper for CheckAndBreakCachingForWrite.
func (lm *Manager) CheckAndBreakOpLocksForWrite(handleKey string, excludeOwner *LockOwner) error {
	return lm.CheckAndBreakCachingForWrite(handleKey, excludeOwner)
}

// CheckAndBreakOpLocksForRead checks and initiates breaks for oplocks that
// conflict with a read operation. Backward-compatible wrapper for CheckAndBreakCachingForRead.
func (lm *Manager) CheckAndBreakOpLocksForRead(handleKey string, excludeOwner *LockOwner) error {
	return lm.CheckAndBreakCachingForRead(handleKey, excludeOwner)
}

// CheckAndBreakOpLocksForDelete checks and initiates breaks for all oplocks
// on a file being deleted. Backward-compatible wrapper for CheckAndBreakCachingForDelete.
func (lm *Manager) CheckAndBreakOpLocksForDelete(handleKey string, excludeOwner *LockOwner) error {
	return lm.CheckAndBreakCachingForDelete(handleKey, excludeOwner)
}

// ============================================================================
// Unified Caching Break Operations
// ============================================================================

// CheckAndBreakCachingForWrite breaks all leases AND all delegations.
// Used for cross-protocol writes (e.g., NFS write breaking SMB leases).
func (lm *Manager) CheckAndBreakCachingForWrite(handleKey string, excludeOwner *LockOwner) error {
	if err := lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, func(lease *OpLock) bool {
		return lease.HasRead() || lease.HasWrite()
	}); err != nil {
		return err
	}
	lm.breakDelegations(handleKey, excludeOwner, func(deleg *Delegation) bool {
		return true
	})

	return nil
}

// CheckAndBreakCachingForRead breaks write leases (to Read) and write delegations.
// Read delegations and read leases coexist with reads.
func (lm *Manager) CheckAndBreakCachingForRead(handleKey string, excludeOwner *LockOwner) error {
	if err := lm.breakOpLocks(handleKey, excludeOwner, LeaseStateRead, func(lease *OpLock) bool {
		return lease.HasWrite()
	}); err != nil {
		return err
	}
	lm.breakDelegations(handleKey, excludeOwner, func(deleg *Delegation) bool {
		return deleg.DelegType == DelegTypeWrite
	})

	return nil
}

// CheckAndBreakLeasesForSMBOpen breaks conflicting leases for an SMB CREATE.
//
// Per MS-SMB2 3.3.5.9 / MS-FSA 2.1.5.17.1: When a new SMB open arrives,
// existing leases that hold Write caching must be broken. Unlike cross-protocol
// breaks (CheckAndBreakCachingForWrite), the break strips only the Write bit,
// preserving Read and Handle caching. This allows clients to continue read
// and handle caching while flushing dirty data.
//
//   - RWH -> RH (strip W, keep Read + Handle)
//   - RW  -> R  (strip W, keep Read)
//   - R   -> not broken (no Write to strip)
//   - RH  -> not broken (no Write to strip)
func (lm *Manager) CheckAndBreakLeasesForSMBOpen(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, BreakToStripWrite, func(lease *OpLock) bool {
		return lease.HasWrite()
	})
}

// BreakLeasesForByteRangeLock breaks every other-key lease that holds Read
// caching to None when an SMB byte-range lock is acquired.
//
// Per MS-SMB2 3.3.5.14 (Receiving an SMB2 LOCK Request) and Samba
// `source3/smbd/smb2_oplock.c::contend_level2_oplocks_begin_default`
// (lines 1391-1467) + `do_break_lease_to_none` (lines 1155-1206):
// when a byte-range lock is granted on an open, every other lease holder
// whose state has Read caching must be broken to None — Read caching
// becomes invalid because the locking client may now write data the
// remote cache can no longer observe.
//
// The locker's own lease (typically same lease key, possibly a same-key
// secondary handle) is excluded via excludeOwner.ExcludeLeaseKey, mirroring
// Samba's `smb2_lease_equal` no-self-break check.
//
// Leases without Read caching (None, or Write-only, which the protocol
// disallows in practice) are skipped: there is no read cache to flush.
// The break target is None — full revocation — not "strip W" or "strip H".
func (lm *Manager) BreakLeasesForByteRangeLock(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, func(lease *OpLock) bool {
		return lease.HasRead()
	})
}

// BreakLeasesOnOpenConflict breaks leases held by other clients when an SMB
// CREATE arrives. Per MS-SMB2 3.3.4.7 and Samba
// `source3/smbd/open.c::delay_for_oplock_fn`. Per-lease target state is
// computed via ComputeLeaseBreakTo(state, reason); a lease is broken only
// when the computed target differs from its current state.
func (lm *Manager) BreakLeasesOnOpenConflict(handleKey string, excludeOwner *LockOwner, reason BreakReason) error {
	return lm.breakOpLocks(handleKey, excludeOwner, breakSentinelForReason(reason), func(lease *OpLock) bool {
		return ComputeLeaseBreakTo(lease.LeaseState, reason) != lease.LeaseState
	})
}

// BreakReadLeasesForParentDir breaks Read leases on a parent directory when
// a child file is modified (SET_INFO, WRITE, DELETE). Per MS-FSA 2.1.5.14:
// changes to directory contents or child metadata invalidate Read caching,
// so clients holding R or RW leases on the directory must be notified.
//
// The break goes to None (full revocation):
//   - R  -> None
//   - RW -> None
func (lm *Manager) BreakReadLeasesForParentDir(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, func(lease *OpLock) bool {
		return lease.IsDirectory && lease.HasRead()
	})
}

// CheckAndBreakCachingForDelete breaks all leases AND all delegations.
func (lm *Manager) CheckAndBreakCachingForDelete(handleKey string, excludeOwner *LockOwner) error {
	if err := lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, func(lease *OpLock) bool {
		return lease.LeaseState != LeaseStateNone
	}); err != nil {
		return err
	}
	lm.breakDelegations(handleKey, excludeOwner, func(deleg *Delegation) bool {
		return true
	})

	return nil
}

// WaitForBreakCompletionExceptKey is WaitForBreakCompletion scoped to ignore
// any breaking lease whose LeaseKey matches exceptKey. Used by the SMB CREATE
// path on a same-key reopen: MS-SMB2 3.3.5.9.8 requires the opener to observe
// Breaking=true on its own key (to emit SMB2_LEASE_FLAG_BREAK_IN_PROGRESS),
// which forceCompleteBreaks would otherwise clear — but any other-key breaks
// still need to drain before the CREATE proceeds (MS-SMB2 3.3.4.7). On
// timeout, own-key is preserved and only other-key leases are force-completed.
func (lm *Manager) WaitForBreakCompletionExceptKey(ctx context.Context, handleKey string, exceptKey [16]byte) error {
	for {
		lm.mu.Lock()
		hasOther := false
		for _, l := range lm.unifiedLocks[handleKey] {
			if l.Lease != nil && l.Lease.Breaking && l.Lease.LeaseKey != exceptKey {
				hasOther = true
				break
			}
			if l.Delegation != nil && l.Delegation.Breaking {
				hasOther = true
				break
			}
		}
		if !hasOther {
			lm.mu.Unlock()
			return nil
		}

		ch, ok := lm.breakWaitChans[handleKey]
		if !ok {
			ch = make(chan struct{})
			lm.breakWaitChans[handleKey] = ch
		}
		lm.mu.Unlock()

		select {
		case <-ctx.Done():
			lm.forceCompleteBreaksExceptKey(handleKey, exceptKey)
			return ctx.Err()
		case <-ch:
			continue
		}
	}
}

// AnyHolderHasLeaseBits reports whether any lease on handleKey (other than
// exceptKey) currently has any bit in mask set. Non-blocking. Used by the SMB
// CREATE post-break park decision per Samba `delay_for_oplock_fn`: a new opener
// only needs to wait for the in-flight break ACK when the existing holder's
// lease type intersects the delay_mask. Zero exceptKey means "no exclusion".
func (lm *Manager) AnyHolderHasLeaseBits(handleKey string, exceptKey [16]byte, mask uint32) bool {
	if mask == 0 {
		return false
	}
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	hasExclusion := exceptKey != ([16]byte{})
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil {
			continue
		}
		if hasExclusion && l.Lease.LeaseKey == exceptKey {
			continue
		}
		if l.Lease.LeaseState&mask != 0 {
			return true
		}
	}
	return false
}

// HasActiveLeaseRecord reports whether handleKey has any lease record (other
// than one keyed on excludeKey) that is not a timeout tombstone. A holder
// kept alive at LeaseState=None after ack-to-None still counts as active —
// Samba's `disallow_write_lease` predicate (source3/smbd/open.c lines
// 2397-2403) gates on `op_type != NO_OPLOCK`, not on lease state. Timeout
// tombstones (BrokenViaTimeout=true) are excluded so a new opener after the
// abandoned holder's timeout is not constrained by the dead record.
func (lm *Manager) HasActiveLeaseRecord(handleKey string, excludeKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil {
			continue
		}
		if l.Lease.LeaseKey == excludeKey {
			continue
		}
		if l.Lease.BrokenViaTimeout {
			continue
		}
		return true
	}
	return false
}

// AnyHolderIsTraditionalOplock reports whether any record on handleKey is a
// traditional oplock (IsTraditionalOplock=true). Used by the SMB CREATE path
// to apply the narrower oplock stat-open mask when a traditional holder is
// present (MS-SMB2 §3.3.5.9 / Samba `is_oplock_stat_open`).
func (lm *Manager) AnyHolderIsTraditionalOplock(handleKey string) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.IsTraditionalOplock {
			return true
		}
	}
	return false
}

// OnlyTimeoutTombstoneRecords reports whether handleKey has at least one
// lease record AND every present lease record is a timeout tombstone
// (BrokenViaTimeout=true). Returns false when no records exist at all, or
// when at least one record is not a timeout tombstone.
//
// Used by the CREATE-grant LEVEL_II coercion to distinguish "holder timed
// out and the server moved on" (only timeout tombstones present → don't
// constrain the new grant by the abandoned holder) from "holder normally
// acked or is still active" (at least one live record → defer to
// bestGrantableState or fall back to non-stat-open coercion).
//
// Covers the contrast between smbtorture batch22b (timeout → tree2 BATCH
// expected) and exclusive9 SUPERSEDE iteration (ack → tree2 LEVEL_II
// expected) which both leave LeaseState=None records but originate from
// different paths.
func (lm *Manager) OnlyTimeoutTombstoneRecords(handleKey string) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	any := false
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil {
			continue
		}
		any = true
		if !l.Lease.BrokenViaTimeout {
			return false
		}
	}
	return any
}

// HasOtherBreakingLeases reports whether any lease (other than exceptKey) or
// any delegation on handleKey is currently Breaking. Non-blocking. Used by the
// SMB CREATE async-park path to decide whether to emit STATUS_PENDING and
// resume the CREATE from a goroutine. A zero exceptKey means "no exclusion" —
// any Breaking lease matches.
func (lm *Manager) HasOtherBreakingLeases(handleKey string, exceptKey [16]byte) bool {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	hasExclusion := exceptKey != ([16]byte{})
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease != nil && l.Lease.Breaking {
			if !hasExclusion || l.Lease.LeaseKey != exceptKey {
				return true
			}
		}
		if l.Delegation != nil && l.Delegation.Breaking {
			return true
		}
	}
	return false
}

// WaitForBreakCompletion blocks until all breaking locks on a file resolve
// or the context is cancelled. Multiple goroutines may wait concurrently;
// signalBreakWait uses close() to broadcast to all waiters.
//
// On timeout (context cancellation), all leases still in Breaking state are
// automatically downgraded to their BreakToState, as if the client had
// acknowledged. Per MS-SMB2 3.3.5.22.2: if the client fails to acknowledge
// within the timeout, the server completes the break.
func (lm *Manager) WaitForBreakCompletion(ctx context.Context, handleKey string) error {
	for {
		lm.mu.Lock()
		hasBreaking := false
		for _, lock := range lm.unifiedLocks[handleKey] {
			if lock.Lease != nil && lock.Lease.Breaking {
				hasBreaking = true
				break
			}
			if lock.Delegation != nil && lock.Delegation.Breaking {
				hasBreaking = true
				break
			}
		}

		if !hasBreaking {
			lm.mu.Unlock()
			return nil
		}

		// Get or create the wait channel while still holding the lock,
		// so no signal from signalBreakWait can be missed.
		ch, ok := lm.breakWaitChans[handleKey]
		if !ok {
			ch = make(chan struct{})
			lm.breakWaitChans[handleKey] = ch
		}
		lm.mu.Unlock()

		select {
		case <-ctx.Done():
			// Timeout: auto-downgrade all breaking leases to their break-to state.
			lm.forceCompleteBreaks(handleKey)
			return ctx.Err()
		case <-ch:
			continue
		}
	}
}

// forceCompleteBreaks force-revokes all breaking leases on a file to None when
// the break wait times out. Records are kept alive at LeaseState=None
// (handle-bound lifetime) so a later unsolicited ack surfaces as
// ErrLeaseAckNotBreaking → STATUS_UNSUCCESSFUL.
func (lm *Manager) forceCompleteBreaks(handleKey string) {
	lm.forceCompleteBreaksExceptKey(handleKey, [16]byte{})
}

// forceCompleteBreaksExceptKey is forceCompleteBreaks that leaves any lease
// keyed on exceptKey untouched. Zero exceptKey means "no exclusion" (same
// semantics as forceCompleteBreaks).
//
// Forces breaking leases to None, mirroring Samba's lease_timeout_handler
// (source3/smbd/smb2_oplock.c) which calls
// `downgrade_lease(..., SMB2_LEASE_NONE)` regardless of the in-flight or
// cumulative break target. A non-acking client must not be allowed to retain
// any lease bits past the timeout — otherwise a later opener observes stale
// state (smb2.lease.timeout: probe of original lease key returns RH instead
// of the spec-mandated empty state) and stale R/H rights would generate
// spurious break notifications on subsequent IO.
func (lm *Manager) forceCompleteBreaksExceptKey(handleKey string, exceptKey [16]byte) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	modified := false
	for _, l := range lm.unifiedLocks[handleKey] {
		if l.Lease == nil || !l.Lease.Breaking || l.Lease.LeaseKey == exceptKey {
			continue
		}
		modified = true
		// Force-revoke to None and keep the record alive at LeaseState=None
		// (handle-bound lifetime) so a later unsolicited or duplicate ack
		// surfaces as ErrLeaseAckNotBreaking → STATUS_UNSUCCESSFUL. Same
		// rationale as applyBreakStageLocked.
		//
		// Do NOT advance Epoch: this is the timeout/internal completion
		// path for an already-dispatched break. Per MS-SMB2 §3.3.4.7 the
		// epoch advances only when a break notification is dispatched (and
		// was already advanced when the in-flight break started). Bumping
		// it here would invalidate any straggling client ack still echoing
		// the original epoch.
		l.Lease.LeaseState = LeaseStateNone
		l.Lease.BreakingToRequired = LeaseStateNone
		l.Lease.Breaking = false
		l.Lease.BreakToState = 0
		l.Lease.BreakStarted = time.Time{}
		l.Lease.BrokenViaTimeout = true
		l.Type = lockTypeForLeaseState(l.Lease.LeaseState)

		if lm.lockStore != nil {
			pl := ToPersistedLock(l, 0)
			_ = lm.lockStore.PutLock(context.Background(), pl)
		}
		logger.Debug("forceCompleteBreaks: auto-downgraded lease",
			"handleKey", handleKey,
			"leaseKey", fmt.Sprintf("%x", l.Lease.LeaseKey),
			"newState", LeaseStateToString(l.Lease.LeaseState))
	}

	if modified {
		lm.signalBreakWaitLocked(handleKey)
	}
}

// signalBreakWait broadcasts to all waiters by closing the wait channel and
// removing it from the map. The next WaitForBreakCompletion call will create
// a fresh channel if needed. Acquires lm.mu internally.
func (lm *Manager) signalBreakWait(handleKey string) {
	lm.mu.Lock()
	lm.signalBreakWaitLocked(handleKey)
	lm.mu.Unlock()
}

// SignalParkedCreates is the LockManager-interface entry point for
// signalBreakWait, exposed so the SMB CLOSE path can wake a parked CREATE
// waiter after the open-file table entry has been removed. See interface doc.
func (lm *Manager) SignalParkedCreates(handleKey string) {
	lm.signalBreakWait(handleKey)
}

// signalBreakWaitLocked is the lock-held variant of signalBreakWait.
// Caller must hold lm.mu.
func (lm *Manager) signalBreakWaitLocked(handleKey string) {
	if ch, ok := lm.breakWaitChans[handleKey]; ok {
		close(ch)
		delete(lm.breakWaitChans, handleKey)
	}
}

// breakOpLocks marks matching oplocks as breaking and dispatches break
// notifications. Releases mutex before dispatching to avoid deadlock.
//
// breakToState is the target state for the break. Pass BreakToStripWrite
// to compute the per-lease break-to state by stripping the Write bit from
// each lease's current state (preserving Read and Handle).
//
// Concurrent-break behavior: when a lease is already Breaking, the new
// target is AND-merged into BreakingToRequired (cumulative final target)
// without dispatching a new notification or advancing the epoch. This
// mirrors Samba `process_oplock_break_message` lines 956-965; the next
// progressive stage is dispatched from acknowledgeLeaseBreakImpl after the
// in-flight ACK arrives.
func (lm *Manager) breakOpLocks(
	handleKey string,
	excludeOwner *LockOwner,
	breakToState uint32,
	shouldBreak func(lease *OpLock) bool,
) error {
	lm.mu.Lock()
	locks := lm.unifiedLocks[handleKey]

	type breakEntry struct {
		lock         *UnifiedLock
		breakToState uint32
	}
	var toBreak []breakEntry
	for _, lock := range locks {
		if lock.Lease == nil {
			continue
		}
		if excludeOwner != nil {
			if lock.Owner.OwnerID == excludeOwner.OwnerID ||
				(excludeOwner.ClientID != "" && lock.Owner.ClientID == excludeOwner.ClientID) {
				continue
			}
			// Per MS-SMB2 3.3.5.9: opens with the same lease key must not
			// break each other's leases ("If Open.Lease.LeaseKey == the new
			// open's LeaseKey, no break is required").
			if excludeOwner.ExcludeLeaseKey != ([16]byte{}) &&
				lock.Lease.LeaseKey == excludeOwner.ExcludeLeaseKey {
				continue
			}
			// Per MS-SMB2 §3.3.4.20 / Samba dirlease_should_break: when a
			// child CREATE or SET_INFO carries an RqLs whose ParentLeaseKey
			// matches the parent dir lease's key, suppress that dir-lease
			// break. Scoped to dir leases to avoid suppressing an unrelated
			// file-lease break that happens to share the key value (#470 C2).
			if excludeOwner.HasExcludeParentDirLeaseKey &&
				lock.Lease.IsDirectory &&
				lock.Lease.LeaseKey == excludeOwner.ExcludeParentDirLeaseKey {
				continue
			}
		}
		if !shouldBreak(lock.Lease) {
			continue
		}

		targetState := computeFreshTarget(lock.Lease.LeaseState, breakToState)

		// Per Samba `delay_for_oplock_fn` (source3/smbd/open.c lines 2439-2444):
		// traditional oplocks only support breaking to R or NONE — any Handle
		// or Write residue in the strip-W/strip-H target must be cleared so the
		// holder lands at R (LEVEL_II) or 0 (NONE). Lease holders retain the
		// fine-grained break-to bits; this mask only applies to traditional
		// oplocks tagged at grant time. Required by smbtorture
		// smb2.oplock.batch9a (BATCH attrs-only holder must break to R so the
		// subsequent normal-open BATCH request can be granted LEVEL_II via
		// bestGrantableState).
		if lock.Lease.IsTraditionalOplock {
			targetState &^= LeaseStateHandle | LeaseStateWrite
		}

		if lock.Lease.Breaking {
			// Concurrent break: AND-merge the new opener's target into the
			// cumulative final target. No notification, no epoch bump
			// (Samba intentionally skips the bump per its inline comment).
			// The next progressive stage will be dispatched on ACK.
			lock.Lease.BreakingToRequired &= targetState
			if lm.lockStore != nil {
				pl := ToPersistedLock(lock, 0)
				_ = lm.lockStore.PutLock(context.Background(), pl)
			}
			continue
		}

		// Fresh dispatch: BreakingToRequired starts at this opener's target.
		// Subsequent concurrent breaks may tighten it via the AND-merge above.
		// Advance the epoch here so the dispatched notification's NewEpoch is
		// pre-bumped (per MS-SMB2 2.2.23.2). Post-ACK progressive stages do
		// NOT advance — the multi-stage progression is one logical break.
		lock.Lease.BreakingToRequired = targetState
		advanceEpoch(lock.Lease)
		snapshot := lm.applyBreakStageLocked(lock, targetState)
		// Persist the in-flight Breaking state so a crash/restart preserves
		// the break-in-progress and parked CREATEs aren't stranded waiting
		// for a notification that was already sent over the wire.
		// applyBreakStageLocked only persists the fire-and-forget downgrade
		// path; the ack-required path (which is the common case) is
		// persisted here.
		if lm.lockStore != nil {
			pl := ToPersistedLock(lock, 0)
			_ = lm.lockStore.PutLock(context.Background(), pl)
		}
		toBreak = append(toBreak, breakEntry{lock: snapshot, breakToState: targetState})
	}
	lm.mu.Unlock()

	for _, entry := range toBreak {
		lm.dispatchOpLockBreak(handleKey, entry.lock, entry.breakToState)
	}

	return nil
}

// computeFreshTarget resolves a breakOpLocks sentinel against the current
// lease state, returning the actual per-lease target. Direct state values
// pass through unchanged.
func computeFreshTarget(currentState, sentinel uint32) uint32 {
	switch sentinel {
	case BreakToStripWrite:
		// Per MS-SMB2 3.3.5.9: RWH -> RH, RW -> R.
		return currentState &^ LeaseStateWrite
	case BreakToStripHandle:
		// Per MS-SMB2 3.3.5.9 Step 10: RWH -> RW, RH -> R.
		return currentState &^ LeaseStateHandle
	}
	return sentinel
}

// applyBreakStageLocked performs a single break stage on lock targeting
// `target`. Caller must hold lm.mu, must have already set
// lock.Lease.BreakingToRequired appropriately, and is responsible for
// dispatching the returned snapshot via dispatchOpLockBreak after releasing
// lm.mu.
//
// Per MS-SMB2 3.3.4.7, a break is ack-required iff the current state is NOT
// pure Read. Without ACK_REQUIRED the client never responds, so leaving
// Breaking=true would block same-key reopens — instead we resolve inline.
//
// For target=None on the inline (fire-and-forget) path we keep the record
// alive at LeaseState=None (handle-bound lifetime) so a later unsolicited
// ack from the client surfaces as ErrLeaseAckNotBreaking →
// STATUS_UNSUCCESSFUL per MS-SMB2 3.3.5.22.2 (smbtorture breaking5). The
// record is removed when the holding handle CLOSEs (ReleaseLeaseForHandle).
func (lm *Manager) applyBreakStageLocked(lock *UnifiedLock, target uint32) *UnifiedLock {
	// Snapshot while LeaseState still holds the pre-break value for
	// CurrentLeaseState in the notification. Caller is responsible for
	// advancing the epoch on fresh dispatch (per MS-SMB2 2.2.23.2). Progressive
	// next-stage dispatch from a post-ACK re-eval does NOT advance epoch — the
	// multi-stage break is one continuous progression and Samba's
	// `downgrade_lease` (source3/smbd/smb2_oplock.c line 607) reuses the
	// existing epoch unchanged for each intermediate stage.
	snapshot := lock.Clone()

	ackRequired := lock.Lease.LeaseState != LeaseStateRead
	if ackRequired {
		lock.Lease.Breaking = true
		lock.Lease.BreakToState = target
		lock.Lease.BreakStarted = time.Now()
		return snapshot
	}

	// Fire-and-forget downgrade: client won't ACK (current state is pure R).
	lock.Lease.Breaking = false
	lock.Lease.BreakToState = 0
	lock.Lease.BreakStarted = time.Time{}
	lock.Lease.LeaseState = target
	lock.Lease.BreakingToRequired = target
	lock.Type = lockTypeForLeaseState(target)
	if lm.lockStore != nil {
		pl := ToPersistedLock(lock, 0)
		_ = lm.lockStore.PutLock(context.Background(), pl)
	}
	return snapshot
}

// ============================================================================
// Delegation CRUD Operations
// ============================================================================

// GrantDelegation grants a delegation on a file.
// Returns error if conflicting leases exist or the file was recently broken.
func (lm *Manager) GrantDelegation(handleKey string, delegation *Delegation) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Check anti-storm cache inside the lock to be atomic with lease conflict check.
	if lm.recentlyBroken != nil && lm.recentlyBroken.IsRecentlyBroken(handleKey) {
		return fmt.Errorf("delegation denied: file recently had caching broken")
	}

	locks := lm.unifiedLocks[handleKey]

	// Check lease conflicts. Delegation-vs-delegation conflicts (e.g., at most
	// one write delegation per file) are enforced by the protocol layer (NFS
	// state manager, SMB handler) before calling GrantDelegation.
	for _, lock := range locks {
		if lock.Lease != nil {
			if DelegationConflictsWithLease(delegation, lock.Lease) {
				return fmt.Errorf("delegation conflicts with existing lease (state=%s)",
					LeaseStateToString(lock.Lease.LeaseState))
			}
		}
	}

	newLock := &UnifiedLock{
		ID: delegation.DelegationID,
		Owner: LockOwner{
			OwnerID:   DelegationOwnerID(delegation.ClientID, delegation.DelegationID),
			ClientID:  delegation.ClientID,
			ShareName: delegation.ShareName,
		},
		FileHandle: FileHandle(handleKey),
		Offset:     0,
		Length:     0, // Whole file
		Type:       delegationToLockType(delegation.DelegType),
		AcquiredAt: time.Now(),
		Delegation: delegation,
	}

	lm.unifiedLocks[handleKey] = append(locks, newLock)
	return nil
}

// DelegationOwnerID returns the OwnerID that GrantDelegation assigns to a
// delegation. This is useful for constructing an excludeOwner that matches
// the delegation's LockOwner.
func DelegationOwnerID(clientID, delegationID string) string {
	return fmt.Sprintf("deleg:%s:%s", clientID, delegationID)
}

// delegationToLockType converts a DelegationType to a LockType.
func delegationToLockType(dt DelegationType) LockType {
	if dt == DelegTypeWrite {
		return LockTypeExclusive
	}
	return LockTypeShared
}

// RevokeDelegation force-revokes a delegation, removing it from the lock map.
func (lm *Manager) RevokeDelegation(handleKey string, delegationID string) error {
	lm.mu.Lock()

	locks := lm.unifiedLocks[handleKey]
	found := false
	var remaining []*UnifiedLock
	for _, l := range locks {
		if l.Delegation != nil && l.Delegation.DelegationID == delegationID {
			found = true
			continue // Drop from remaining (removed from map)
		}
		remaining = append(remaining, l)
	}

	if !found {
		lm.mu.Unlock()
		return fmt.Errorf("delegation %s not found on handle %s", delegationID, handleKey)
	}

	if len(remaining) == 0 {
		delete(lm.unifiedLocks, handleKey)
	} else {
		lm.unifiedLocks[handleKey] = remaining
	}
	lm.mu.Unlock()

	lm.signalBreakWait(handleKey)
	return nil
}

// ReturnDelegation handles a client returning a delegation. Idempotent:
// returns nil even if the delegation was not found.
func (lm *Manager) ReturnDelegation(handleKey string, delegationID string) error {
	lm.mu.Lock()

	locks := lm.unifiedLocks[handleKey]
	var remaining []*UnifiedLock
	for _, l := range locks {
		if l.Delegation != nil && l.Delegation.DelegationID == delegationID {
			continue
		}
		remaining = append(remaining, l)
	}

	if len(remaining) == 0 {
		delete(lm.unifiedLocks, handleKey)
	} else {
		lm.unifiedLocks[handleKey] = remaining
	}
	lm.mu.Unlock()

	lm.signalBreakWait(handleKey)
	return nil
}

// GetDelegation retrieves a specific delegation by ID.
// Returns nil if not found.
func (lm *Manager) GetDelegation(handleKey string, delegationID string) *Delegation {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	for _, lock := range lm.unifiedLocks[handleKey] {
		if lock.Delegation != nil && lock.Delegation.DelegationID == delegationID {
			return lock.Delegation.Clone()
		}
	}
	return nil
}

// ListDelegations returns all delegations on a file.
func (lm *Manager) ListDelegations(handleKey string) []*Delegation {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	var result []*Delegation
	for _, lock := range lm.unifiedLocks[handleKey] {
		if lock.Delegation != nil {
			result = append(result, lock.Delegation.Clone())
		}
	}
	return result
}

// ExpiredDelegation holds info about a delegation whose recall has timed out.
type ExpiredDelegation struct {
	HandleKey    string
	DelegationID string
}

// CollectExpiredDelegationRecalls returns delegations that are in the breaking
// state and have exceeded the given timeout. This allows external scanners to
// query for expired recalls without accessing internal fields.
func (lm *Manager) CollectExpiredDelegationRecalls(now time.Time, timeout time.Duration) []ExpiredDelegation {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	var expired []ExpiredDelegation
	for handleKey, locks := range lm.unifiedLocks {
		for _, lock := range locks {
			if lock.Delegation == nil || !lock.Delegation.Breaking {
				continue
			}
			if now.After(lock.Delegation.BreakStarted.Add(timeout)) {
				expired = append(expired, ExpiredDelegation{
					HandleKey:    handleKey,
					DelegationID: lock.Delegation.DelegationID,
				})
			}
		}
	}
	return expired
}

// breakDelegations collects delegations matching the predicate and dispatches
// recall notifications. Releases mutex before dispatching to avoid deadlock.
//
// excludeOwner skips delegations whose Owner.OwnerID matches. Delegation
// OwnerIDs use the format "deleg:{clientID}:{delegationID}". Callers that
// want to exclude by client identity should match on Owner.ClientID instead,
// or construct the OwnerID in the same format.
func (lm *Manager) breakDelegations(
	handleKey string,
	excludeOwner *LockOwner,
	shouldBreak func(deleg *Delegation) bool,
) {
	lm.mu.Lock()
	locks := lm.unifiedLocks[handleKey]

	var toBreak []*UnifiedLock
	for _, lock := range locks {
		if lock.Delegation == nil {
			continue
		}
		if excludeOwner != nil &&
			(lock.Owner.OwnerID == excludeOwner.OwnerID ||
				(excludeOwner.ClientID != "" && lock.Owner.ClientID == excludeOwner.ClientID)) {
			continue
		}
		if lock.Delegation.Breaking {
			continue
		}
		if shouldBreak(lock.Delegation) {
			lock.Delegation.Breaking = true
			lock.Delegation.BreakStarted = time.Now()
			// Clone before dispatch to prevent race with concurrent ack/release.
			toBreak = append(toBreak, lock.Clone())
		}
	}
	lm.mu.Unlock()

	if len(toBreak) > 0 && lm.recentlyBroken != nil {
		lm.recentlyBroken.Mark(handleKey)
	}

	for _, lock := range toBreak {
		lm.dispatchDelegationRecall(handleKey, lock)
	}
}

// dispatchDelegationRecall notifies all registered break callbacks about a delegation recall.
func (lm *Manager) dispatchDelegationRecall(handleKey string, lock *UnifiedLock) {
	lm.mu.RLock()
	callbacks := make([]BreakCallbacks, len(lm.breakCallbacks))
	copy(callbacks, lm.breakCallbacks)
	lm.mu.RUnlock()

	if len(callbacks) == 0 {
		logger.Debug("delegation recall with no callbacks registered",
			"handleKey", handleKey,
			"delegationID", lock.Delegation.DelegationID)
		return
	}

	for _, cb := range callbacks {
		cb.OnDelegationRecall(handleKey, lock)
	}
}

// dispatchOpLockBreak notifies all registered break callbacks about an oplock break.
func (lm *Manager) dispatchOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	lm.mu.RLock()
	callbacks := make([]BreakCallbacks, len(lm.breakCallbacks))
	copy(callbacks, lm.breakCallbacks)
	lm.mu.RUnlock()

	if len(callbacks) == 0 {
		logger.Debug("oplock break with no callbacks registered",
			"handleKey", handleKey,
			"owner", lock.Owner.OwnerID,
			"breakToState", LeaseStateToString(breakToState))
		return
	}

	for _, cb := range callbacks {
		cb.OnOpLockBreak(handleKey, lock, breakToState)
	}
}

// ============================================================================
// Grace Period Delegation
// ============================================================================

// EnterGracePeriod transitions to grace period state.
// If no grace period manager is configured, this is a no-op.
func (lm *Manager) EnterGracePeriod(expectedClients []string) {
	if lm.gracePeriod != nil {
		lm.gracePeriod.EnterGracePeriod(expectedClients)
	}
}

// ExitGracePeriod manually exits the grace period.
// If no grace period manager is configured, this is a no-op.
func (lm *Manager) ExitGracePeriod() {
	if lm.gracePeriod != nil {
		lm.gracePeriod.ExitGracePeriod()
	}
}

// AbortGracePeriod cancels a pending grace timer WITHOUT firing the onGraceEnd
// callback. Used to discard a manager that lost a registration race: its
// orphaned timer must never run, because onGraceEnd sweeps the shared lock
// store (RemoveClientLocks) and ends the surviving NFSv4 grace machine — both
// would corrupt the published manager's state. If no grace period manager is
// configured, this is a no-op.
func (lm *Manager) AbortGracePeriod() {
	if lm.gracePeriod != nil {
		lm.gracePeriod.Close()
	}
}

// IsOperationAllowed checks if a lock operation is allowed in the current state.
// If no grace period manager is configured, all operations are allowed.
func (lm *Manager) IsOperationAllowed(op Operation) (bool, error) {
	if lm.gracePeriod != nil {
		return lm.gracePeriod.IsOperationAllowed(op)
	}
	return true, nil
}

// MarkReclaimed records that a client has reclaimed their locks.
// If no grace period manager is configured, this is a no-op.
func (lm *Manager) MarkReclaimed(clientID string) {
	if lm.gracePeriod != nil {
		lm.gracePeriod.MarkReclaimed(clientID)
	}
}

// IsInGracePeriod returns true if grace period is currently active.
func (lm *Manager) IsInGracePeriod() bool {
	if lm.gracePeriod != nil {
		return lm.gracePeriod.GetState() == GraceStateActive
	}
	return false
}

// GetExpectedClients returns the client IDs the grace period is waiting on to
// reclaim. Returns nil if no grace period manager is configured.
func (lm *Manager) GetExpectedClients() []string {
	if lm.gracePeriod != nil {
		return lm.gracePeriod.GetExpectedClients()
	}
	return nil
}

// GetReclaimedClients returns the client IDs that have reclaimed during the
// current grace period. Returns nil if no grace period manager is configured.
func (lm *Manager) GetReclaimedClients() []string {
	if lm.gracePeriod != nil {
		return lm.gracePeriod.GetReclaimedClients()
	}
	return nil
}

// ============================================================================
// Break Callback Registration
// ============================================================================

// RegisterBreakCallbacks registers typed callbacks for break notifications.
//
// Multiple callbacks can be registered (one per protocol adapter).
// Callbacks are invoked in registration order during break operations.
func (lm *Manager) RegisterBreakCallbacks(callbacks BreakCallbacks) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.breakCallbacks = append(lm.breakCallbacks, callbacks)
}

// ============================================================================
// Connection/Cleanup Operations
// ============================================================================

// RemoveAllLocks removes all locks (legacy, unified, and delegations) for a file.
//
// The persisted bulk-delete runs synchronously under lm.mu (handleKey is the
// FileID used when persisting): keeping it ordered with single-record PutLock/
// DeleteLock prevents a concurrent acquire on the same file from racing this
// delete to the store and leaving an orphaned record behind (R3-1 class).
func (lm *Manager) RemoveAllLocks(handleKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.locks, handleKey)
	delete(lm.unifiedLocks, handleKey)
	delete(lm.breakWaitChans, handleKey)

	if lm.lockStore != nil {
		ctx, cancel := withPersistTimeout()
		defer cancel()
		if _, err := lm.lockStore.DeleteLocksByFile(ctx, handleKey); err != nil {
			logger.Error("RemoveAllLocks: failed to delete persisted locks", "handleKey", handleKey, "error", err)
		}
	}
}

// RemoveClientLocks removes all unified locks held by a specific client. The
// persisted bulk-delete runs synchronously under lm.mu for the same ordering
// reason as RemoveAllLocks.
func (lm *Manager) RemoveClientLocks(clientID string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	for handleKey, locks := range lm.unifiedLocks {
		var remaining []*UnifiedLock
		for _, lock := range locks {
			if lock.Owner.ClientID != clientID {
				remaining = append(remaining, lock)
			}
		}
		if len(remaining) == 0 {
			delete(lm.unifiedLocks, handleKey)
		} else {
			lm.unifiedLocks[handleKey] = remaining
		}
	}

	if lm.lockStore != nil {
		ctx, cancel := withPersistTimeout()
		defer cancel()
		if _, err := lm.lockStore.DeleteLocksByClient(ctx, clientID); err != nil {
			logger.Error("RemoveClientLocks: failed to delete persisted locks", "clientID", clientID, "error", err)
		}
	}
}

// GetStats returns current lock manager statistics.
func (lm *Manager) GetStats() ManagerStats {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	totalLegacy := 0
	for _, locks := range lm.locks {
		totalLegacy += len(locks)
	}

	totalUnified := 0
	for _, locks := range lm.unifiedLocks {
		totalUnified += len(locks)
	}

	fileSet := make(map[string]struct{})
	for key := range lm.locks {
		fileSet[key] = struct{}{}
	}
	for key := range lm.unifiedLocks {
		fileSet[key] = struct{}{}
	}

	return ManagerStats{
		TotalLegacyLocks:   totalLegacy,
		TotalUnifiedLocks:  totalUnified,
		TotalFiles:         len(fileSet),
		BreakCallbackCount: len(lm.breakCallbacks),
		GracePeriodActive:  lm.gracePeriod != nil && lm.gracePeriod.GetState() == GraceStateActive,
	}
}
