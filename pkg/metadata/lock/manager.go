package lock

import (
	"context"
	"fmt"
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
	// TestUnifiedLock previews whether a prospective NLM/NFSv4 byte-range lock
	// would conflict, checking BOTH the unified and SMB byte-range maps.
	TestUnifiedLock(handleKey string, want *UnifiedLock) *UnifiedLockConflict

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
	// isDirectory=true restricts to validDirectoryLeaseStates.
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
	// clientID must match the owner recorded on the persisted lease; a
	// mismatch is rejected to prevent lease-stealing. Pass "" to skip the
	// owner check (callers without a stable client identity).
	// Returns the reclaimed lock or error if lease doesn't exist or directory deleted.
	ReclaimLease(ctx context.Context, leaseKey [16]byte,
		requestedState uint32, isDirectory bool, clientID string) (*UnifiedLock, error)

	// GetLeaseState returns the current state and epoch of the lease that
	// leaseKey holds on handleKey. found=false if that file holds no lease
	// under that key.
	//
	// The file is a parameter rather than resolved from the key because the
	// key alone does not identify a lease: the cross-file uniqueness rule is
	// scoped per (client, file), so another client may hold the same 16-byte
	// value on a different file of this share. The state and epoch returned
	// here go back out on the wire on a durable disconnect and on a replayed
	// CREATE.
	GetLeaseState(ctx context.Context, handleKey string, leaseKey [16]byte) (state uint32, epoch uint16, found bool)

	// IsTraditionalOplockForKey returns true if the lease record this key holds
	// on handleKey was granted via RequestLeaseAsOplock (traditional oplock,
	// not SMB2.1+ lease).
	IsTraditionalOplockForKey(handleKey string, leaseKey [16]byte) bool

	// HasLeaseOnHandle reports whether a lease record with this key already
	// exists on this file. Unlike GetLeaseState it does not search across
	// files, so it cannot be answered by another client's lease that happens
	// to reuse the same key value on a different file. It asks exactly what
	// RequestLease asks when it decides whether to reuse a record or create
	// one, which is what makes it usable as a "would this grant be the first
	// for this key on this file" test.
	HasLeaseOnHandle(handleKey string, leaseKey [16]byte) bool

	// SetLeaseEpoch sets the epoch on the lease that leaseKey holds on
	// handleKey. Per MS-SMB2 3.3.5.9: For V2 leases, the server tracks the
	// client's epoch. Returns false if that file holds no lease under that key.
	SetLeaseEpoch(handleKey string, leaseKey [16]byte, epoch uint16) bool

	// ========================================================================
	// Delegation Operations
	// ========================================================================

	// GrantDelegation grants a delegation on a file.
	// Returns error if a conflicting lease or byte-range lock exists.
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
	// Outstanding NFSv4 delegations on the file are recalled as well: a
	// delegated client answers byte-range locks locally, so it can neither
	// see this lock nor have its own locks seen.
	BreakLeasesForByteRangeLock(handleKey string, excludeOwner *LockOwner) error

	// BreakLeasesOnOpenConflict breaks existing leases before an SMB CREATE
	// proceeds, per MS-SMB2 3.3.4.7 and Samba `source3/smbd/open.c::delay_for_oplock_fn`.
	// Per-lease target state is computed via ComputeLeaseBreakTo(state, reason).
	BreakLeasesOnOpenConflict(handleKey string, excludeOwner *LockOwner, reason BreakReason) error

	// PrepareBreakLeasesOnOpenConflict records the same breaks but returns the
	// function that sends their notifications, so a caller can order the wire
	// notification after its own response without letting a lease granted in
	// between be broken by a change that predates it.
	PrepareBreakLeasesOnOpenConflict(handleKey string, excludeOwner *LockOwner, reason BreakReason) func()

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

	// WaitForByteRangeLeaseBreak blocks until every breaking SMB lease on
	// handleKey resolves, ignoring in-flight NFSv4 delegation breaks. Used by the
	// byte-range-lock acquisition path, which holds the NFSv4 StateManager mutex
	// and must not wait on delegation breaks (DELEGRETURN needs that same mutex).
	WaitForByteRangeLeaseBreak(ctx context.Context, handleKey string) error

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

	// WaitForShareConflictClear blocks until the parked CREATE should stop
	// waiting and recheck the share mode, returning when ANY of: conflictPresent()
	// reports false (holder CLOSEd → conflict cleared), the holder's break drained
	// while the conflict persists (holder ACKed but kept its open), or ctx is
	// cancelled. It re-evaluates conflictPresent on every break-wait signal and on
	// a short poll. A nil return means "recheck", NOT "conflict cleared" — the
	// caller MUST re-run the share-mode check. Unlike WaitForBreakCompletion it
	// NEVER force-completes breaking leases, so the holder's deferred ACK still
	// succeeds (smbtorture replay dhv2-pending1n-vs-violation-lease-{close,ack}-sane,
	// MS-SMB2 §3.3.5.9 deferred open / Samba defer_open→retry_open).
	WaitForShareConflictClear(ctx context.Context, handleKey string, conflictPresent func() bool) error

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

// Byte-range locks live in two maps that the protocol adapters populate
// separately: SMB byte-range locks in lm.locks (Manager.Lock) and NLM/NFSv4
// byte-range locks in lm.unifiedLocks (Manager.AddUnifiedLock). The helpers
// below let each acquisition/IO path cross-check the OTHER map so a lock taken
// via one protocol blocks a conflicting lock or write via the other
// (MS-FSA §2.1.5 cross-protocol byte-range conflict). Without this an NFS lock
// and an SMB lock on the same overlapping range could both be granted.

// fileLockConflictsWithUnified reports whether an SMB byte-range FileLock and a
// byte-range UnifiedLock on the same file conflict. Cross-protocol byte-range
// locks are always different owners, so the test reduces to range overlap plus
// exclusivity. Whole-file leases and delegations are caching primitives
// resolved through the break path, not byte-range conflicts, so they never
// participate; SMB2 zero-byte locks never conflict.
func fileLockConflictsWithUnified(fl *FileLock, ul *UnifiedLock) bool {
	if ul.IsLease() || ul.IsDelegation() {
		return false
	}
	if fl.IsZeroByte {
		return false
	}
	if !RangesOverlap(fl.Offset, fl.Length, ul.Offset, ul.Length) {
		return false
	}
	// Two shared (read) locks coexist; an exclusive on either side conflicts.
	return fl.Exclusive || ul.IsExclusive()
}

// unifiedLockBlocksIO reports whether a byte-range UnifiedLock held by another
// protocol blocks an SMB I/O at [offset, length). Cross-protocol I/O is always
// a different owner: a write is blocked by any overlapping lock; a read is
// blocked only by an overlapping exclusive lock (MS-FSA §2.1.4.10).
func unifiedLockBlocksIO(ul *UnifiedLock, offset, length uint64, isWrite bool) bool {
	if ul.IsLease() || ul.IsDelegation() {
		return false
	}
	if !ul.Overlaps(offset, length) {
		return false
	}
	if isWrite {
		return true
	}
	return ul.IsExclusive()
}

// conflictFromUnified renders a byte-range UnifiedLock as the LockConflict the
// SMB byte-range path reports.
func conflictFromUnified(ul *UnifiedLock) *LockConflict {
	return &LockConflict{
		Offset:    ul.Offset,
		Length:    ul.Length,
		Exclusive: ul.IsExclusive(),
		OwnerID:   ul.Owner.OwnerID,
	}
}

// unifiedConflictFromFileLock renders a conflicting SMB FileLock as the
// UnifiedLockConflict the NLM/NFSv4 byte-range path reports.
func unifiedConflictFromFileLock(fl *FileLock) *UnifiedLockConflict {
	lt := LockTypeShared
	if fl.Exclusive {
		lt = LockTypeExclusive
	}
	return &UnifiedLockConflict{
		Lock: &UnifiedLock{
			Owner:  LockOwner{OwnerID: lockOwnerID(fl)},
			Offset: fl.Offset,
			Length: fl.Length,
			Type:   lt,
		},
		Reason: "cross-protocol byte-range conflict",
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

	// Reverse indexes over unifiedLocks, maintained by reindexHandleLocked
	// (see indexes.go). leaseKeyIndex maps a lease key to the set of handleKey
	// buckets (ref-counted per bucket) that hold a record for it — the same
	// numeric lease key may be bound on multiple files (distinct buckets), so
	// the index must track every holder, giving findLeaseByKey a candidate set
	// to probe instead of a full scan. clientHandleIndex maps a clientID to the
	// set of handleKeys (ref-counted per bucket) that hold at least one of its
	// locks, bounding RemoveClientLocks to affected files. Both are derived
	// state and require lm.mu (write to mutate, read to look up).
	leaseKeyIndex     map[[16]byte]map[string]int
	clientHandleIndex map[string]map[string]int

	gracePeriod   *GracePeriodManager // grace period state (may be nil)
	handleChecker HandleChecker       // checks if file handles still exist (for reclaim)
	lockStore     LockStore           // persistent lock store (optional)

	// clientRecoveryStore is the NFSv4 client recovery store used for
	// principal verification during lease reclaim. Optional — nil disables
	// the NFSv4 principal check (SMB-only deployments).
	clientRecoveryStore ClientRecoveryStore
	epoch               uint64               // current server epoch (stamped on persisted locks)
	shareName           string               // share this manager serves (stamped on persisted byte-range locks)
	recentlyBroken      *recentlyBrokenCache // prevents directory lease storms

	// Delegation-related fields
	breakWaitChans          map[string]chan struct{} // per-handleKey channel for break wait
	delegationRecallTimeout time.Duration            // default 90s, configurable

	// onByteRangeRelease is a protocol-agnostic notification fired after a
	// byte-range lock is released on handleKey, so a different protocol's
	// blocked waiters can be re-driven. It exists because NLM uses a
	// server-driven GRANTED-callback model (it does not poll): when an SMB
	// UNLOCK frees a range an NLM waiter is blocked on, that waiter must be
	// actively woken. The NFS adapter wires this to its processNLMWaiters
	// drain, mirroring the NLM-side onUnlock callback. May be nil. Fired
	// outside lm.mu so the subscriber can call back into the Manager.
	onByteRangeRelease func(handleKey string)

	// persistLanes orders lock-store writes per file, and pendingPersist holds
	// the writes the current critical section has queued but not yet run. See
	// persistqueue.go: mutations take a lane ticket under lm.mu and make their
	// store call after lm.mu is released, so per file the store still observes
	// them in mutex order while different files' round-trips overlap.
	persistLanes   [persistLaneCount]persistLane
	pendingPersist []persistOp
}

// DefaultDelegationRecallTimeout is the default delegation recall timeout.
// NFS uses a longer timeout than SMB leases (90s vs 35s).
const DefaultDelegationRecallTimeout = 90 * time.Second

// persistTimeout bounds every lock-store call the manager makes. The call runs
// before the operation that queued it returns, and it holds its file's persist
// lane while it runs, so a hung backend would otherwise wedge that file (and
// its caller) indefinitely; the timeout turns that into a bounded best-effort
// failure that logs and proceeds.
const persistTimeout = 3 * time.Second

// newBaseManager creates a Manager with all common fields initialized.
// Callers customize the returned Manager before use.
func newBaseManager(recentlyBrokenTTL time.Duration) *Manager {
	m := &Manager{
		locks:                   make(map[string][]FileLock),
		unifiedLocks:            make(map[string][]*UnifiedLock),
		leaseKeyIndex:           make(map[[16]byte]map[string]int),
		clientHandleIndex:       make(map[string]map[string]int),
		recentlyBroken:          newRecentlyBrokenCache(recentlyBrokenTTL),
		breakWaitChans:          make(map[string]chan struct{}),
		delegationRecallTimeout: DefaultDelegationRecallTimeout,
	}
	for i := range m.persistLanes {
		m.persistLanes[i].cond = sync.NewCond(&m.persistLanes[i].mu)
	}
	return m
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

// SetByteRangeReleaseCallback registers a protocol-agnostic notification that
// fires after a byte-range lock is released on a handle (see the field doc on
// onByteRangeRelease). Used by the NFS adapter to re-drive blocked NLM waiters
// when an SMB UNLOCK frees a contended range. Safe to call once at wiring time.
func (lm *Manager) SetByteRangeReleaseCallback(fn func(handleKey string)) {
	lm.mu.Lock()
	lm.onByteRangeRelease = fn
	lm.unlock()
}

// notifyByteRangeReleased fires the release callback (if registered) for
// handleKey. MUST be called WITHOUT lm.mu held: the subscriber (e.g. the NLM
// waiter drain) calls back into the Manager to re-attempt blocked locks.
func (lm *Manager) notifyByteRangeReleased(handleKey string) {
	lm.mu.RLock()
	fn := lm.onByteRangeRelease
	lm.mu.RUnlock()
	if fn != nil {
		fn(handleKey)
	}
}

// Lock attempts to acquire a byte-range lock on a file.
//
// This is a low-level CRUD operation with no permission checking.
// Business logic (permission checks, file type validation) should be
// performed by the caller.
//
// Returns nil on success, or ErrLocked if a conflict exists.
//
// Persistence is synchronous with respect to the caller: the in-memory
// mutation happens under lm.mu, the PutLock is queued on the file's persist
// lane and runs once lm.mu is released, and this call does not return until it
// has run. Per file the store still sees mutations in the order the mutex
// serialized them, so two concurrent ops on the same persistID cannot reach the
// store out of order (the reorder/resurrection bug class).
func (lm *Manager) Lock(handleKey string, lock FileLock) error {
	lm.mu.Lock()
	defer lm.unlock()

	existing := lm.locks[handleKey]

	// Check for conflicts with existing locks
	for i := range existing {
		if IsLockConflicting(&existing[i], &lock) {
			return NewLockedError("", conflictFrom(&existing[i]))
		}
	}

	// Cross-protocol: an overlapping NLM/NFSv4 byte-range lock must also block
	// this SMB lock (area-5 H-3 / xproto H1).
	for _, ul := range lm.unifiedLocks[handleKey] {
		if fileLockConflictsWithUnified(&lock, ul) {
			return NewLockedError("", conflictFromUnified(ul))
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
	released, err := lm.doUnlock(handleKey, openID, sessionID, offset, length)
	if released {
		// A freed byte-range may unblock a cross-protocol waiter (e.g. an NLM
		// F_SETLKW blocked on this SMB lock). Notify outside lm.mu.
		lm.notifyByteRangeReleased(handleKey)
	}
	return err
}

func (lm *Manager) doUnlock(handleKey string, openID string, sessionID uint64, offset, length uint64) (bool, error) {
	lm.mu.Lock()
	defer lm.unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return false, NewLockNotFoundError("")
	}

	// Find and remove the matching lock. For stacked identical SMB shared
	// locks the first match is removed; its distinct persistID ensures only
	// that one persisted record is dropped, leaving the rest of the stack.
	owner := callerOwnerID(openID, sessionID)
	for i := range existing {
		if lockOwnerID(&existing[i]) == owner &&
			existing[i].Offset == offset &&
			existing[i].Length == length {
			lm.deleteFileLockLocked(handleKey, &existing[i])
			// Remove this lock
			lm.locks[handleKey] = append(existing[:i], existing[i+1:]...)

			// Clean up empty entries to prevent memory leak
			if len(lm.locks[handleKey]) == 0 {
				delete(lm.locks, handleKey)
			}
			return true, nil
		}
	}

	return false, NewLockNotFoundError("")
}

// UnlockAllForOpen releases all locks held by a specific open on a file.
//
// Returns the number of locks released.
func (lm *Manager) UnlockAllForOpen(handleKey string, openID string) int {
	if openID == "" {
		return 0 // empty openID would match all unset locks — guard against misuse
	}
	removed := lm.doUnlockAllForOpen(handleKey, openID)
	if removed > 0 {
		// Freed ranges may unblock cross-protocol waiters; notify outside lm.mu.
		lm.notifyByteRangeReleased(handleKey)
	}
	return removed
}

func (lm *Manager) doUnlockAllForOpen(handleKey string, openID string) int {
	lm.mu.Lock()
	defer lm.unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return 0
	}

	// Filter out locks belonging to this open
	remaining := make([]FileLock, 0, len(existing))
	removed := 0
	for i := range existing {
		if existing[i].OpenID == openID {
			lm.deleteFileLockLocked(handleKey, &existing[i])
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
	removed := lm.doUnlockAllForSession(handleKey, sessionID)
	if removed > 0 {
		// Freed ranges may unblock cross-protocol waiters; notify outside lm.mu.
		lm.notifyByteRangeReleased(handleKey)
	}
	return removed
}

func (lm *Manager) doUnlockAllForSession(handleKey string, sessionID uint64) int {
	lm.mu.Lock()
	defer lm.unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return 0
	}

	// Filter out locks belonging to this session
	remaining := make([]FileLock, 0, len(existing))
	removed := 0
	for i := range existing {
		if existing[i].SessionID == sessionID {
			lm.deleteFileLockLocked(handleKey, &existing[i])
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

	// Mirror Lock()'s cross-protocol check so the preview agrees with acquire.
	for _, ul := range lm.unifiedLocks[handleKey] {
		if fileLockConflictsWithUnified(&lock, ul) {
			return conflictFromUnified(ul), nil
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

	// Cross-protocol: a byte-range lock held via NLM/NFSv4 must also gate SMB
	// I/O (xproto H2). Without this an NFS exclusive lock never blocks an SMB
	// write to the same range.
	for _, ul := range lm.unifiedLocks[handleKey] {
		if unifiedLockBlocksIO(ul, offset, length, isWrite) {
			return conflictFromUnified(ul)
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
	defer lm.unlock()
	delete(lm.locks, handleKey)
}

// SetDelegationRecallTimeout sets the delegation recall timeout (thread-safe).
func (lm *Manager) SetDelegationRecallTimeout(d time.Duration) {
	lm.mu.Lock()
	defer lm.unlock()
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
	defer lm.unlock()
	lm.handleChecker = hc
}

// SetLockStore sets the persistent lock store for lease persistence.
func (lm *Manager) SetLockStore(store LockStore) {
	lm.mu.Lock()
	defer lm.unlock()
	lm.lockStore = store
}

// SetClientRecoveryStore wires the NFSv4 client recovery store used for
// principal verification during lease reclaim. Optional — when nil the
// NFSv4 principal check is skipped (SMB-only deployments).
func (lm *Manager) SetClientRecoveryStore(store ClientRecoveryStore) {
	lm.mu.Lock()
	defer lm.unlock()
	lm.clientRecoveryStore = store
}

// SetEpoch records the current server epoch stamped on persisted locks.
func (lm *Manager) SetEpoch(epoch uint64) {
	lm.mu.Lock()
	defer lm.unlock()
	lm.epoch = epoch
}

// SetShareName records the share this manager serves. The share name is
// stamped on persisted byte-range locks so they can be recovered by the
// per-share ListLocks query at startup.
func (lm *Manager) SetShareName(shareName string) {
	lm.mu.Lock()
	defer lm.unlock()
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
// backend can never wedge the lock manager: the store call holds its file's
// persist lane and blocks the operation that queued it, and the timeout caps
// how long either can last.
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

// persistFileLockLocked queues a byte-range lock for persistence. No-op if
// persistence is disabled. Caller must hold lm.mu.
func (lm *Manager) persistFileLockLocked(handleKey string, fl *FileLock) {
	if lm.lockStore == nil {
		return
	}
	lm.putLockLocked(lm.fileLockToPersisted(handleKey, fl))
}

// deleteFileLockLocked queues removal of a persisted byte-range lock. No-op if
// persistence is disabled or the lock was never persisted. Caller must hold
// lm.mu.
func (lm *Manager) deleteFileLockLocked(handleKey string, fl *FileLock) {
	if lm.lockStore == nil || fl.persistID == "" {
		return
	}
	lm.deletePersistedLocked(handleKey, fl.persistID)
}

// persistUnifiedLockLocked queues a unified lock for persistence. No-op if
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
	// For a lease record Type is a projection of LeaseState, not an independent
	// field: re-derive it here so a mutation that changes LeaseState without
	// touching Type cannot write out a record whose Type contradicts the lease
	// it carries. Byte-range and delegation records own their Type outright and
	// are left alone.
	if ul.Lease != nil {
		ul.Type = lockTypeForLeaseState(ul.Lease.LeaseState)
	}

	if lm.lockStore == nil {
		return
	}
	pl := ToPersistedLock(ul, lm.epoch)
	if lm.shareName != "" {
		pl.ShareName = lm.shareName
	}
	lm.putLockLocked(pl)
}

// deleteUnifiedLockLocked queues removal of a persisted unified lock. No-op if
// persistence is disabled. Caller must hold lm.mu.
func (lm *Manager) deleteUnifiedLockLocked(ul *UnifiedLock) {
	if lm.lockStore == nil {
		return
	}
	lm.deletePersistedLocked(string(ul.FileHandle), ul.ID)
}

// putLockLocked queues one record for persistence on its file's lane, bounded
// by persistTimeout when it runs. Caller must hold lm.mu and must have already
// applied the in-memory mutation, so the store observes mutations in mutex
// order — this is what eliminates the reorder/resurrection bug class (R3-1).
// The call itself is made after lm.mu is released and before the acquiring
// operation returns to its client (see persistqueue.go).
//
// Persistence is BEST-EFFORT. The in-memory lock map is authoritative for the
// running server, so a failed PutLock must NOT fail the lock op — the client is
// told the (advisory) lock is held and it is, in this process. The only
// consequence of a failed persist is durability across restart: the lock
// survives in memory but is lost on restart, after which a conflicting lock
// could be granted. The operator MUST treat these ERROR logs as a durability
// alarm. Errors are logged with file/owner context so they are observable.
func (lm *Manager) putLockLocked(pl *PersistedLock) {
	store := lm.lockStore
	lm.enqueuePersistLocked(pl.FileID, func() {
		ctx, cancel := withPersistTimeout()
		defer cancel()
		if err := store.PutLock(ctx, pl); err != nil {
			logger.Error("lock persistence failed: lock held in memory but NOT durable across restart",
				"lockID", pl.ID,
				"share", pl.ShareName,
				"fileID", pl.FileID,
				"ownerID", pl.OwnerID,
				"error", err)
		}
	})
}

// deletePersistedLocked queues removal of one record by ID on fileKey's lane,
// bounded by persistTimeout when it runs. Caller must hold lm.mu and must have
// already applied the in-memory removal (mutex order == store order). fileKey
// must be the same key the record was persisted under, so the delete stays
// ordered behind its own put.
//
// Best-effort with the same contract as putLockLocked: a failed DeleteLock
// means a released lock may resurrect on restart until the next successful
// overwrite/cleanup. ErrLockNotFound is ignored — the record is already gone.
func (lm *Manager) deletePersistedLocked(fileKey, id string) {
	store := lm.lockStore
	lm.enqueuePersistLocked(fileKey, func() {
		ctx, cancel := withPersistTimeout()
		defer cancel()
		if err := store.DeleteLock(ctx, id); err != nil && !isLockNotFound(err) {
			logger.Error("lock-delete persistence failed: released lock may resurrect on restart",
				"lockID", id,
				"error", err)
		}
	})
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
	defer lm.unlock()

	for _, pl := range persisted {
		// pl.FileID is the handle key used when persisting (see persist helpers).
		// Legacy SMB byte-range records belong in lm.locks (consulted by
		// Lock/Unlock/TestLock/CheckForIO); leases, delegations and NLM/NFSv4
		// unified locks belong in lm.unifiedLocks.
		if pl.IsLegacyByteRange {
			lm.locks[pl.FileID] = append(lm.locks[pl.FileID], fileLockFromPersisted(pl))
			continue
		}
		ul := FromPersistedLock(pl)
		lm.unifiedLocks[pl.FileID] = append(lm.unifiedLocks[pl.FileID], ul)
		lm.indexAddLockLocked(pl.FileID, ul)
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
