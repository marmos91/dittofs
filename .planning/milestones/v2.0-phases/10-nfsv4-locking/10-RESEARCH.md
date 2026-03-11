# Phase 10: NFSv4 Locking - Research

**Researched:** 2026-02-14
**Domain:** NFSv4 byte-range locking -- LOCK, LOCKT, LOCKU, RELEASE_LOCKOWNER operations with lock-owner/lock-stateid state management (RFC 7530 Sections 9.3-9.7, 16.10-16.12, 16.34)
**Confidence:** HIGH

## Summary

Phase 10 builds NFSv4 integrated byte-range locking on top of the Phase 9 state management infrastructure. The current codebase has:
- `StateManager` with full client ID, open-owner, open-state, lease, and grace period management (Phase 9)
- `OpenState.LockStates []interface{}` -- an empty placeholder field ready for lock state references
- `StateTypeLock byte = 0x02` -- reserved type tag for lock stateids in the "other" field encoding
- `handleReleaseLockOwner()` -- a no-op stub that consumes XDR args and returns NFS4_OK
- LOCK, LOCKT, LOCKU operations -- not registered in the dispatch table (return NFS4ERR_NOTSUPP for opcodes 12-14)
- `pkg/metadata/lock.Manager` -- a fully implemented unified lock manager with `AddEnhancedLock()`, `RemoveEnhancedLock()`, `ListEnhancedLocks()`, POSIX split/merge, cross-protocol conflict detection, and deadlock detection
- All needed NFS4 error codes already defined: `NFS4ERR_DENIED`, `NFS4ERR_DEADLOCK`, `NFS4ERR_LOCK_RANGE`, `NFS4ERR_LOCKS_HELD`, `NFS4ERR_OPENMODE`

Phase 10 must implement four NFSv4 operations (LOCK, LOCKT, LOCKU, RELEASE_LOCKOWNER) and their backing state management. The LOCK operation is the most complex: it has a `locker4` union that distinguishes between a new lock-owner (transitioning from an open stateid to a lock stateid) and an existing lock-owner (using an existing lock stateid). New lock-owners require the open stateid and open seqid for validation, plus a lock-owner identity and lock_seqid. Existing lock-owners use the lock stateid directly. LOCK returns a `LOCK4denied` struct on conflict with the conflicting lock's owner identity. LOCKT tests for conflicts without creating state. LOCKU releases locks and increments the lock stateid seqid. RELEASE_LOCKOWNER releases all lock state for a lock-owner when the client is done with it.

The key architectural decision is how NFSv4 lock state in `StateManager` bridges to the unified `pkg/metadata/lock.Manager`. The StateManager tracks lock-owners, lock stateids, and seqid validation (protocol-level concerns), while the lock manager handles actual byte-range conflict detection, POSIX splitting, and cross-protocol awareness (storage-level concerns). The bridge is the lock-owner's `OwnerID` string: format `"nfs4:{clientid}:{lock_owner_hex}"` enables cross-protocol conflict detection with NLM and SMB locks.

**Primary recommendation:** Add lock-owner tracking and lock-stateid management to the existing `state/` package with a new `lockowner.go` file. LOCK/LOCKT/LOCKU handlers follow the same pattern as OPEN/CLOSE -- decode XDR, delegate to StateManager for state operations, which in turn delegates to `pkg/metadata/lock.Manager` for conflict detection. The `StateManager` gets a reference to the lock manager for conflict operations. Organize into four plans: (1) LOCK with locker4 union decoding, lock-owner creation, stateid management, and lock manager integration, (2) LOCKT and LOCKU operations, (3) RELEASE_LOCKOWNER with real state cleanup, (4) Integration with unified lock manager including CLOSE check for NFS4ERR_LOCKS_HELD.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `internal/protocol/nfs/v4/state/` | Phase 9 | LockOwner tracking, lock stateid generation/validation, seqid handling | Extends existing StateManager pattern for lock state |
| `internal/protocol/nfs/v4/handlers/` | Phase 6-9 | LOCK, LOCKT, LOCKU, RELEASE_LOCKOWNER handlers | New handler files following existing pattern |
| `internal/protocol/nfs/v4/types/` | Phase 6 | Lock type constants (READ_LT, WRITE_LT, etc.), XDR types | Needs new lock type constants added |
| `pkg/metadata/lock.Manager` | Phase 1-5 | Actual byte-range lock conflict detection, POSIX split/merge | Already fully implemented with EnhancedLock support |
| Go stdlib `sync` | N/A | Thread safety for lock-owner maps | Extends existing StateManager RWMutex |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `pkg/metadata/lock.LockOwner` | Phase 5 | Protocol-agnostic owner identity for cross-protocol locks | Bridge between NFSv4 state and unified lock manager |
| `pkg/metadata/lock.EnhancedLock` | Phase 5 | Lock record with byte-range, type, owner, cross-protocol fields | Lock manager entries created by NFSv4 LOCK operations |
| `internal/protocol/xdr` | Phase 6 | XDR encode/decode for lock operation wire formats | All handler XDR parsing |
| `pkg/metadata/lock.RangesOverlap` | Phase 5 | Byte-range overlap calculation | LOCKT conflict detection |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Unified lock manager via `pkg/metadata/lock.Manager` | Separate NFSv4-only lock tracking | Unified manager enables cross-protocol conflict detection (NLM/SMB vs NFSv4), which is a Phase 10 requirement |
| Lock state in StateManager maps | Lock state in lock.Manager only | StateManager must track lock-owners and lock-stateids for seqid validation and stateid generation; actual byte-range conflicts go to lock.Manager |
| Single lock-owner seqid namespace | Per-file lock seqid | RFC 7530 specifies per-lock-owner seqid tracking with a single lock stateid per (lock-owner, open-state) pair |

**No new external dependencies required.**

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/
├── state/
│   ├── manager.go          # MODIFY: add lock-owner maps, lock state maps, LockManager reference
│   ├── lockowner.go         # NEW: LockOwner, LockState, lock stateid management
│   ├── lockowner_test.go    # NEW: lock-owner seqid, stateid, state tests
│   ├── stateid.go           # EXISTING: StateTypeLock already reserved
│   ├── openowner.go         # MODIFY: change LockStates from []interface{} to []*LockState
│   └── ...
│
├── handlers/
│   ├── handler.go           # MODIFY: register OP_LOCK, OP_LOCKT, OP_LOCKU
│   ├── lock.go              # NEW: handleLock, handleLockT, handleLockU
│   ├── lock_test.go         # NEW: handler-level lock tests
│   ├── stubs.go             # MODIFY: upgrade handleReleaseLockOwner from no-op to real
│   └── close.go             # MODIFY: check NFS4ERR_LOCKS_HELD before closing
│
├── types/
│   └── constants.go         # MODIFY: add lock_type4 constants (READ_LT, WRITE_LT, etc.)
```

### Pattern 1: Lock-Owner and Lock-State Data Model
**What:** Each lock-owner is identified by `(clientid, owner_data)` -- same pattern as open-owners. A lock-owner is associated with exactly one open-state (the open file it locks). The lock stateid is per-(lock-owner, open-state) pair.
**When to use:** LOCK with `new_lock_owner=true` creates a LockOwner and LockState. Subsequent LOCKs with `new_lock_owner=false` use the existing lock stateid.
**Example:**
```go
// state/lockowner.go
type LockOwner struct {
    ClientID     uint64
    OwnerData    []byte
    LastSeqID    uint32
    LastResult   *CachedResult  // For replay detection
    ClientRecord *ClientRecord
}

type LockState struct {
    Stateid     types.Stateid4
    LockOwner   *LockOwner
    OpenState   *OpenState     // The open state this lock is derived from
    FileHandle  []byte
}

// Lock-owner key matches open-owner pattern
type lockOwnerKey string

func makeLockOwnerKey(clientID uint64, ownerData []byte) lockOwnerKey {
    return lockOwnerKey(fmt.Sprintf("%d:%s", clientID, hex.EncodeToString(ownerData)))
}
```

### Pattern 2: Locker4 Union Handling (LOCK's Three Cases)
**What:** The LOCK operation uses a `locker4` union discriminated by `new_lock_owner` boolean. When true, it contains `open_to_lock_owner4` (open stateid + open seqid + lock owner + lock seqid). When false, it contains `exist_lock_owner4` (lock stateid + lock seqid).
**When to use:** LOCK handler XDR decoding and state manager dispatch.
**Example:**
```go
// handlers/lock.go -- decode the locker4 union
newLockOwner, _ := xdr.DecodeUint32(reader) // bool as uint32

if newLockOwner != 0 {
    // open_to_lock_owner4: open the lock-owner's first lock
    openSeqid, _ := xdr.DecodeUint32(reader)
    openStateid, _ := types.DecodeStateid4(reader)
    lockSeqid, _ := xdr.DecodeUint32(reader)
    lockClientID, _ := xdr.DecodeUint64(reader)
    lockOwnerData, _ := xdr.DecodeOpaque(reader)

    result, err := h.StateManager.LockNew(
        lockClientID, lockOwnerData, lockSeqid,
        openStateid, openSeqid,
        ctx.CurrentFH, lockType, offset, length, reclaim,
    )
} else {
    // exist_lock_owner4: use existing lock stateid
    lockStateid, _ := types.DecodeStateid4(reader)
    lockSeqid, _ := xdr.DecodeUint32(reader)

    result, err := h.StateManager.LockExisting(
        lockStateid, lockSeqid,
        ctx.CurrentFH, lockType, offset, length, reclaim,
    )
}
```

### Pattern 3: StateManager-to-LockManager Bridge
**What:** StateManager handles protocol-level state (lock-owners, stateids, seqids). Actual byte-range conflict detection is delegated to `pkg/metadata/lock.Manager` via an `EnhancedLock` with an OwnerID string in the format `"nfs4:{clientid}:{owner_hex}"`.
**When to use:** Every LOCK/LOCKT/LOCKU operation.
**Example:**
```go
// state/manager.go
type StateManager struct {
    // ... existing fields ...
    lockOwners       map[lockOwnerKey]*LockOwner
    lockStateByOther map[[types.NFS4_OTHER_SIZE]byte]*LockState
    lockManager      *lock.Manager  // Reference to unified lock manager
}

func (sm *StateManager) acquireLock(lockState *LockState, lockType byte,
    offset, length uint64, reclaim bool) (*LOCK4denied, error) {
    // Build EnhancedLock for the unified manager
    owner := lock.LockOwner{
        OwnerID:   fmt.Sprintf("nfs4:%d:%s", lockState.LockOwner.ClientID,
            hex.EncodeToString(lockState.LockOwner.OwnerData)),
        ClientID:  fmt.Sprintf("nfs4:%d", lockState.LockOwner.ClientID),
        ShareName: extractShareFromHandle(lockState.FileHandle),
    }

    enhLock := lock.NewEnhancedLock(owner, lock.FileHandle(lockState.FileHandle),
        offset, length, mapNFS4LockType(lockType))
    enhLock.Reclaim = reclaim

    err := sm.lockManager.AddEnhancedLock(string(lockState.FileHandle), enhLock)
    if err != nil {
        // Map conflict to LOCK4denied
        return translateConflict(err), nil
    }
    return nil, nil
}
```

### Pattern 4: CLOSE Must Check for Held Locks
**What:** Per RFC 7530, CLOSE MUST fail with NFS4ERR_LOCKS_HELD if byte-range locks are still held on the open state. The client must LOCKU all locks before CLOSE.
**When to use:** CloseFile in StateManager.
**Example:**
```go
func (sm *StateManager) CloseFile(stateid *types.Stateid4, seqid uint32) (*types.Stateid4, error) {
    // ... existing validation ...

    // Check for held locks (Phase 10 addition)
    if len(openState.LockStates) > 0 {
        return nil, &NFS4StateError{
            Status:  types.NFS4ERR_LOCKS_HELD,
            Message: "cannot close: byte-range locks still held",
        }
    }
    // ... rest of existing close logic ...
}
```

### Anti-Patterns to Avoid
- **Separate lock tracking from StateManager:** Lock-owners and lock-stateids MUST be in StateManager (not a separate manager) because they participate in the same lease renewal, seqid validation, and client cleanup flow as open-owners.
- **Bypassing the unified lock manager:** NFSv4 locks must go through `pkg/metadata/lock.Manager.AddEnhancedLock()` so that cross-protocol conflict detection works. Do NOT maintain a separate NFSv4-only lock table.
- **Forgetting open-owner seqid validation on new lock-owner:** When `new_lock_owner=true`, the `open_seqid` in `open_to_lock_owner4` must be validated against the open-owner's LastSeqID. This is the mechanism that prevents duplicate lock acquisitions from retransmits.
- **Lock stateid per lock range:** RFC 7530 specifies ONE lock stateid per (lock-owner, open-state) pair, covering ALL byte ranges locked by that owner on that file. The seqid increments with each LOCK/LOCKU. Do NOT create a separate stateid per byte range.
- **Ignoring lock cleanup on lease expiry:** When `onLeaseExpired` fires, ALL lock state for the client must be cleaned up (lock owners, lock stateids, and actual locks from the lock manager).

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Byte-range conflict detection | Custom conflict checker in state/ | `lock.IsEnhancedLockConflicting()` | Already handles shared/exclusive, range overlap, same-owner bypass, cross-protocol |
| Lock splitting on LOCKU | Custom range arithmetic | `lock.SplitLock()` and `lock.RemoveEnhancedLock()` | POSIX semantics for partial unlock (0, 1, or 2 resulting locks) already implemented |
| Lock merging | Custom range coalescing | `lock.MergeLocks()` | Adjacent/overlapping lock coalescing already implemented |
| Deadlock detection | Custom cycle detection | `lock.Manager` deadlock support | Already has wait-for graph via `lock.deadlock.go` |
| Cross-protocol translation | Custom NFS4-to-NLM mapping | `lock.TranslateToNLMHolder()` | Already translates EnhancedLock to NLM holder info format |
| Seqid validation | New validation function | Reuse `nextSeqID()` and `OpenOwner.ValidateSeqID()` pattern | Same algorithm applies to lock-owners |

**Key insight:** The heavy lifting (conflict detection, POSIX splitting, cross-protocol awareness) is already done in `pkg/metadata/lock`. Phase 10's complexity is in the NFSv4-specific state management (lock-owner lifecycle, lock stateid tracking, locker4 union decoding) and the bridge between protocol state and the lock manager.

## Common Pitfalls

### Pitfall 1: Lock Stateid Is Per (Lock-Owner, Open-State), Not Per Range
**What goes wrong:** Creating a new lock stateid for each byte range leads to seqid tracking confusion, incorrect LOCKU behavior, and violates the RFC.
**Why it happens:** It's intuitive to think each lock has its own stateid, but NFSv4 bundles all locks from the same lock-owner on the same open file under a single stateid.
**How to avoid:** The `LockState` struct maps 1:1 with the (lock-owner, open-state) pair. Multiple LOCK operations from the same lock-owner increment the same stateid's seqid. The actual byte ranges are tracked in the lock manager, not in the stateid.
**Warning signs:** Multiple lock stateids for the same owner on the same file; LOCKU failing to find the stateid.

### Pitfall 2: Open-Owner Seqid Must Be Validated on New Lock-Owner
**What goes wrong:** When `new_lock_owner=true`, the `open_seqid` in the `open_to_lock_owner4` struct must be validated against the open-owner. Skipping this allows retransmitted LOCK requests to create duplicate lock-owners.
**Why it happens:** It seems like only lock_seqid matters for LOCK, but the open_seqid provides cross-owner replay protection.
**How to avoid:** When processing `open_to_lock_owner4`: (1) validate `open_stateid` via `ValidateStateid`, (2) validate `open_seqid` against the open-owner's `LastSeqID`, (3) create the lock-owner and lock-state, (4) update the open-owner's `LastSeqID` to `open_seqid`.
**Warning signs:** Duplicate lock-owners created on retransmit; NFS4ERR_BAD_SEQID returned incorrectly for valid new lock requests.

### Pitfall 3: LOCKT Does Not Create State
**What goes wrong:** Implementing LOCKT like LOCK (creating lock-owners or stateids) wastes resources and creates orphaned state.
**Why it happens:** LOCKT shares many fields with LOCK, making it tempting to reuse the same code path.
**How to avoid:** LOCKT only checks for conflicts. It uses `lock_owner4` (not `locker4`) because the client may not have an open stateid. LOCKT queries the lock manager for conflicts without creating any state in the StateManager.
**Warning signs:** Lock-owners created by LOCKT that are never cleaned up; state growth from frequent LOCKT calls.

### Pitfall 4: NFS4ERR_OPENMODE When Lock Type Mismatches Open Access
**What goes wrong:** A client with OPEN4_SHARE_ACCESS_READ tries to acquire a WRITE_LT lock. The server must return NFS4ERR_OPENMODE.
**Why it happens:** The lock handler doesn't check that the lock type is compatible with the open state's share_access.
**How to avoid:** Before calling the lock manager, verify: WRITE_LT/WRITEW_LT requires `share_access & OPEN4_SHARE_ACCESS_WRITE != 0`. READ_LT/READW_LT requires `share_access & OPEN4_SHARE_ACCESS_READ != 0`.
**Warning signs:** Write locks granted on read-only opens; clients confused by lock-then-write failures.

### Pitfall 5: Blocking Lock Types (READW_LT, WRITEW_LT) Are Non-Blocking on Server
**What goes wrong:** Treating READW_LT/WRITEW_LT as blocking server-side (waiting for lock release) creates resource exhaustion from held connections.
**Why it happens:** The "W" suffix suggests "wait," but per RFC 7530 Section 9.4, the server does NOT block. It returns NFS4ERR_DENIED immediately, and the "W" is a hint for the client to retry.
**How to avoid:** Map READW_LT to READ_LT and WRITEW_LT to WRITE_LT for conflict detection purposes. Return NFS4ERR_DENIED (not NFS4ERR_DELAY) on conflict. The blocking behavior is client-side only.
**Warning signs:** Server goroutines blocked waiting for lock release; connection pool exhaustion under lock contention.

### Pitfall 6: Lock Cleanup on Lease Expiry Must Remove from Lock Manager
**What goes wrong:** `onLeaseExpired` removes lock-owners and lock-stateids from StateManager maps but forgets to call `lockManager.RemoveEnhancedLock()` for each lock. The byte-range locks persist as "orphaned" entries blocking other clients.
**Why it happens:** Phase 9's lease expiry only needed to clean up open state (maps in StateManager). Locks add a second cleanup target (the lock manager).
**How to avoid:** In `onLeaseExpired`, iterate over all lock states for the expired client. For each lock state, call `lockManager.RemoveEnhancedFileLocks()` using the file handle key. Then remove from StateManager maps.
**Warning signs:** After client disconnect, other clients cannot acquire locks on files previously locked by the expired client.

### Pitfall 7: CLOSE Check for Locks Must Come Before State Cleanup
**What goes wrong:** The existing `CloseFile` removes state first and then checks for locks, meaning the check never finds locks because the state is already gone.
**Why it happens:** The existing code has no lock state to check.
**How to avoid:** Add the `NFS4ERR_LOCKS_HELD` check BEFORE removing any open state. Check `len(openState.LockStates) > 0`.
**Warning signs:** CLOSE always succeeds even with held locks; clients skip LOCKU because CLOSE doesn't enforce it.

### Pitfall 8: Reclaim Locks During Grace Period
**What goes wrong:** New lock requests during grace period should return NFS4ERR_GRACE. Reclaim locks (`reclaim=true` in LOCK4args) should be allowed during grace period. Not distinguishing these causes either all locks to fail during grace or no grace protection.
**Why it happens:** The existing `CheckGraceForNewState()` only handles OPEN claims.
**How to avoid:** LOCK with `reclaim=true` bypasses grace period check (same as OPEN CLAIM_PREVIOUS). LOCK with `reclaim=false` calls `CheckGraceForNewState()` and gets NFS4ERR_GRACE during grace.
**Warning signs:** Reclaim locks rejected during grace; new locks allowed during grace period.

## Code Examples

### Example 1: Lock Type Constants (to add to types/constants.go)
```go
// Source: RFC 7531 (XDR specification), enum nfs_lock_type4
const (
    READ_LT  = 1 // Read lock (shared)
    WRITE_LT = 2 // Write lock (exclusive)
    READW_LT = 3 // Blocking read lock (hint only, server does NOT block)
    WRITEW_LT = 4 // Blocking write lock (hint only, server does NOT block)
)
```

### Example 2: LockOwner and LockState Structures
```go
// Source: RFC 7530 Section 9.4 + Linux nfsd nfs4_lockowner patterns
type LockOwner struct {
    ClientID     uint64
    OwnerData    []byte
    LastSeqID    uint32
    LastResult   *CachedResult
    ClientRecord *ClientRecord
}

func (lo *LockOwner) ValidateSeqID(seqid uint32) SeqIDValidation {
    expected := nextSeqID(lo.LastSeqID)
    if seqid == expected {
        return SeqIDOK
    }
    if seqid == lo.LastSeqID {
        return SeqIDReplay
    }
    return SeqIDBad
}

type LockState struct {
    Stateid    types.Stateid4
    LockOwner  *LockOwner
    OpenState  *OpenState
    FileHandle []byte
}
```

### Example 3: LOCK4denied Response Encoding
```go
// Source: RFC 7531 LOCK4denied struct
func encodeLOCK4denied(buf *bytes.Buffer, denied *LOCK4denied) {
    _ = xdr.WriteUint64(buf, denied.Offset)     // offset4
    _ = xdr.WriteUint64(buf, denied.Length)      // length4
    _ = xdr.WriteUint32(buf, denied.LockType)    // nfs_lock_type4
    _ = xdr.WriteUint64(buf, denied.Owner.ClientID) // clientid4
    _ = xdr.WriteXDROpaque(buf, denied.Owner.OwnerData) // owner opaque
}

type LOCK4denied struct {
    Offset   uint64
    Length   uint64
    LockType uint32
    Owner    struct {
        ClientID  uint64
        OwnerData []byte
    }
}
```

### Example 4: Open-Mode Validation Before Lock
```go
// Source: RFC 7530 Section 16.10 NFS4ERR_OPENMODE rules
func validateOpenModeForLock(openState *OpenState, lockType uint32) error {
    switch lockType {
    case types.WRITE_LT, types.WRITEW_LT:
        if openState.ShareAccess & types.OPEN4_SHARE_ACCESS_WRITE == 0 {
            return &NFS4StateError{
                Status:  types.NFS4ERR_OPENMODE,
                Message: "write lock requires OPEN4_SHARE_ACCESS_WRITE",
            }
        }
    case types.READ_LT, types.READW_LT:
        if openState.ShareAccess & types.OPEN4_SHARE_ACCESS_READ == 0 {
            return &NFS4StateError{
                Status:  types.NFS4ERR_OPENMODE,
                Message: "read lock requires OPEN4_SHARE_ACCESS_READ",
            }
        }
    }
    return nil
}
```

### Example 5: StateManager.LockNew (New Lock-Owner Path)
```go
// Source: RFC 7530 Section 16.10 Case 3 (new lock-owner)
func (sm *StateManager) LockNew(
    lockClientID uint64, lockOwnerData []byte, lockSeqid uint32,
    openStateid *types.Stateid4, openSeqid uint32,
    fileHandle []byte, lockType uint32, offset, length uint64, reclaim bool,
) (*LockResult, error) {
    // Grace period check
    if !reclaim {
        if err := sm.CheckGraceForNewState(); err != nil {
            return nil, err
        }
    }

    sm.mu.Lock()
    defer sm.mu.Unlock()

    // 1. Validate open stateid
    openState, exists := sm.openStateByOther[openStateid.Other]
    if !exists {
        return nil, ErrBadStateid
    }

    // 2. Validate open-owner seqid
    openOwner := openState.Owner
    validation := openOwner.ValidateSeqID(openSeqid)
    if validation == SeqIDBad {
        return nil, ErrBadSeqid
    }
    // Note: SeqIDReplay needs special handling for the transition case

    // 3. Check open mode compatibility
    if err := validateOpenModeForLock(openState, lockType); err != nil {
        return nil, err
    }

    // 4. Create or find lock-owner
    lockKey := makeLockOwnerKey(lockClientID, lockOwnerData)
    lockOwner, ownerExists := sm.lockOwners[lockKey]
    if !ownerExists {
        lockOwner = &LockOwner{
            ClientID:     lockClientID,
            OwnerData:    append([]byte(nil), lockOwnerData...),
            LastSeqID:    0,
            ClientRecord: sm.clientsByID[lockClientID],
        }
        sm.lockOwners[lockKey] = lockOwner
    }

    // 5. Create or find lock state for (lock-owner, open-state)
    lockState := sm.findOrCreateLockState(lockOwner, openState, fileHandle)

    // 6. Validate lock seqid
    lockValidation := lockOwner.ValidateSeqID(lockSeqid)
    if lockValidation == SeqIDBad {
        return nil, ErrBadSeqid
    }

    // 7. Acquire lock via unified lock manager
    denied, err := sm.acquireLock(lockState, lockType, offset, length, reclaim)
    if denied != nil {
        return &LockResult{Denied: denied}, nil
    }
    if err != nil {
        return nil, err
    }

    // 8. Update seqids
    lockState.Stateid.Seqid = nextSeqID(lockState.Stateid.Seqid)
    lockOwner.LastSeqID = lockSeqid
    openOwner.LastSeqID = openSeqid

    return &LockResult{Stateid: lockState.Stateid}, nil
}
```

## State of the Art

| Old Approach (Phase 9) | Current Approach (Phase 10) | When Changed | Impact |
|-------------------------|----------------------------|--------------|--------|
| `OpenState.LockStates []interface{}` empty | Typed `[]*LockState` with real lock tracking | Phase 10 | Lock state tied to open state lifecycle |
| No LOCK/LOCKT/LOCKU handlers | Full implementations with locker4 union | Phase 10 | Byte-range locking operational |
| RELEASE_LOCKOWNER no-op | Real lock-owner cleanup | Phase 10 | Clients can release lock resources |
| CLOSE always succeeds | CLOSE checks NFS4ERR_LOCKS_HELD | Phase 10 | Enforces lock-before-close cleanup |
| No lock type constants | READ_LT/WRITE_LT/READW_LT/WRITEW_LT | Phase 10 | Wire format lock type support |
| Lock manager not connected to NFSv4 | StateManager delegates to lock.Manager | Phase 10 | Cross-protocol lock awareness |
| Lease expiry cleans only open state | Lease expiry cleans lock state and lock manager entries | Phase 10 | No orphaned locks after client timeout |

**Deprecated/outdated:**
- `handleReleaseLockOwner()` no-op in stubs.go -- replace with real implementation
- `OpenState.LockStates []interface{}` -- change to `[]*LockState`

## XDR Wire Format Summary

### LOCK4args
```
nfs_lock_type4  locktype     (uint32: 1=READ_LT, 2=WRITE_LT, 3=READW_LT, 4=WRITEW_LT)
bool            reclaim      (uint32: 0 or 1)
offset4         offset       (uint64)
length4         length       (uint64, 0 = to EOF, 0xFFFFFFFFFFFFFFFF = full file)
locker4:
  bool new_lock_owner (uint32)
  if TRUE (open_to_lock_owner4):
    seqid4     open_seqid    (uint32)
    stateid4   open_stateid  (16 bytes: seqid + other)
    seqid4     lock_seqid    (uint32)
    lock_owner4:
      clientid4  clientid    (uint64)
      opaque     owner       (XDR opaque: uint32 len + bytes + padding)
  if FALSE (exist_lock_owner4):
    stateid4   lock_stateid  (16 bytes)
    seqid4     lock_seqid    (uint32)
```

### LOCK4res
```
switch (nfsstat4 status):
  case NFS4_OK:
    stateid4   lock_stateid  (16 bytes)
  case NFS4ERR_DENIED:
    LOCK4denied:
      offset4         offset     (uint64)
      length4         length     (uint64)
      nfs_lock_type4  locktype   (uint32)
      lock_owner4:
        clientid4  clientid     (uint64)
        opaque     owner        (XDR opaque)
  default:
    void
```

### LOCKT4args
```
nfs_lock_type4  locktype     (uint32)
offset4         offset       (uint64)
length4         length       (uint64)
lock_owner4:
  clientid4     clientid     (uint64)
  opaque        owner        (XDR opaque)
```

### LOCKT4res
```
switch (nfsstat4 status):
  case NFS4_OK:
    void
  case NFS4ERR_DENIED:
    LOCK4denied    (same as LOCK)
  default:
    void
```

### LOCKU4args
```
nfs_lock_type4  locktype       (uint32)
seqid4          seqid          (uint32)
stateid4        lock_stateid   (16 bytes)
offset4         offset         (uint64)
length4         length         (uint64)
```

### LOCKU4res
```
switch (nfsstat4 status):
  case NFS4_OK:
    stateid4   lock_stateid   (16 bytes, seqid incremented)
  default:
    void
```

### RELEASE_LOCKOWNER4args
```
lock_owner4:
  clientid4     clientid     (uint64)
  opaque        owner        (XDR opaque)
```

### RELEASE_LOCKOWNER4res
```
nfsstat4       status        (uint32)
```

## Recommended Plan Structure

### Plan 10-01: LOCK Operation with Stateid Management
**Scope:** LockOwner struct, LockState struct, lock-owner maps in StateManager, lock stateid generation (type=0x02), LockNew() for new lock-owner path, LockExisting() for existing lock-owner path, lock type constants, locker4 union XDR decoding in handler, LOCK4denied response encoding, open-mode validation (NFS4ERR_OPENMODE), grace period check for reclaim=true vs false, register OP_LOCK in dispatch table.
**Files:** `state/lockowner.go` (new), `state/manager.go` (extend), `handlers/lock.go` (new), `types/constants.go` (add lock constants), `handlers/handler.go` (register OP_LOCK)
**Complexity:** High -- locker4 union decoding, three LOCK cases, open-owner seqid validation on new lock-owner transition
**Tests:** ~30 tests: new lock-owner creates lock state, existing lock-owner uses lock stateid, seqid validation and replay, NFS4ERR_OPENMODE for write-lock on read-open, NFS4ERR_DENIED on conflict, NFS4ERR_GRACE for new locks during grace, reclaim allowed during grace, READW_LT/WRITEW_LT treated as non-blocking

### Plan 10-02: LOCKT, LOCKU Operations
**Scope:** LOCKT handler (conflict test without state creation), LOCKU handler (unlock with seqid validation and stateid update), register OP_LOCKT and OP_LOCKU in dispatch table, POSIX split semantics via lock manager, LOCKT uses lock_owner4 (not locker4).
**Files:** `handlers/lock.go` (extend), `state/manager.go` (add TestLock and Unlock methods), `handlers/handler.go` (register OP_LOCKT, OP_LOCKU)
**Complexity:** Medium -- LOCKU needs seqid validation and stateid seqid increment; LOCKT is simpler (no state)
**Tests:** ~20 tests: LOCKT detects conflict, LOCKT returns ok when no conflict, LOCKU releases lock and increments seqid, LOCKU with bad stateid returns NFS4ERR_BAD_STATEID, LOCKU with bad seqid returns NFS4ERR_BAD_SEQID, partial LOCKU splits lock

### Plan 10-03: RELEASE_LOCKOWNER Operation
**Scope:** Upgrade handleReleaseLockOwner from no-op to real implementation. Remove all lock state for the specified lock-owner from StateManager maps and from lock manager. Return NFS4ERR_LOCKS_HELD if locks are still held (per RFC, RELEASE_LOCKOWNER must fail if lock_owner has locks associated with it).
**Files:** `handlers/stubs.go` (upgrade), `state/manager.go` (add ReleaseLockOwner method)
**Complexity:** Low-Medium -- straightforward state cleanup
**Tests:** ~10 tests: release unknown lock-owner returns NFS4_OK, release lock-owner with no locks succeeds, release lock-owner with held locks returns NFS4ERR_LOCKS_HELD, release cleans up lock-owner from all maps

### Plan 10-04: Integration with Unified Lock Manager
**Scope:** Connect StateManager to `pkg/metadata/lock.Manager`, CLOSE check for NFS4ERR_LOCKS_HELD, lease expiry cleanup of lock state from lock manager, cross-protocol lock owner format ("nfs4:{clientid}:{owner}"), update `OpenState.LockStates` from `[]interface{}` to `[]*LockState`, integration tests showing NFSv4 locks conflict with pre-existing enhanced locks.
**Files:** `state/manager.go` (add lockManager field, update constructors), `state/openowner.go` (change LockStates type), `handlers/close.go` (add locks-held check), `state/lockowner_test.go` (integration tests), `handlers/lock_test.go` (integration tests)
**Complexity:** Medium -- bridging two subsystems, ensuring cleanup is complete
**Tests:** ~15 tests: CLOSE with held locks returns NFS4ERR_LOCKS_HELD, CLOSE after LOCKU succeeds, lease expiry removes locks from lock manager, cross-protocol conflict detection, lock manager cleanup on client removal

## Open Questions

1. **Lock manager reference: passed in or discovered?**
   - What we know: StateManager is created in `NewHandler` with no lock manager. The lock manager lives in `MetadataService` (or per-share).
   - What's unclear: Should StateManager receive a lock manager reference at construction, or discover it per-operation via the runtime?
   - Recommendation: Add an optional `lockManager *lock.Manager` parameter to `NewStateManager()` (variadic, like `graceDuration`). For tests, pass nil (locks managed internally). For production, the NFS adapter passes the shared lock manager. This keeps the existing test call sites unchanged.

2. **Lock owner identity format for cross-protocol**
   - What we know: The lock manager uses `LockOwner.OwnerID` string for conflict detection. NLM uses `"nlm:{caller}:{svid}:{oh}"`, SMB uses `"smb:{clientid}"`.
   - What's unclear: Exact format for NFSv4 lock owners.
   - Recommendation: Use `"nfs4:{clientid}:{hex(owner_data)}"` matching the existing open-owner key pattern. This is unique per client and per lock-owner identity.

3. **Blocking locks (READW_LT/WRITEW_LT) behavior**
   - What we know: RFC 7530 Section 9.4 says the server returns NFS4ERR_DENIED immediately for blocking locks. The blocking behavior is client-side.
   - What's unclear: Whether any special handling is needed beyond stripping the "blocking" flag.
   - Recommendation: Map READW_LT -> READ_LT and WRITEW_LT -> WRITE_LT for conflict detection. Return NFS4ERR_DENIED on conflict, same as non-blocking. No server-side blocking.

4. **RELEASE_LOCKOWNER with held locks**
   - What we know: RFC 7530 says RELEASE_LOCKOWNER should fail if the lock_owner has locks associated with it. However, some implementations are more lenient.
   - What's unclear: Should we strictly follow the RFC or be lenient?
   - Recommendation: Strictly follow RFC -- return NFS4ERR_LOCKS_HELD if the lock-owner has any associated lock state. This matches Linux nfsd behavior.

## Sources

### Primary (HIGH confidence)
- **RFC 7530** (https://www.rfc-editor.org/rfc/rfc7530.html) - NFSv4.0 protocol specification: Sections 9.3-9.7 (locking model, blocking locks, lease renewal, crash recovery), 16.10 (LOCK), 16.11 (LOCKT), 16.12 (LOCKU), 16.34 (RELEASE_LOCKOWNER)
- **RFC 7531** (https://www.rfc-editor.org/rfc/rfc7531.html) - NFSv4.0 XDR definitions: lock_type4, locker4, open_to_lock_owner4, exist_lock_owner4, LOCK4args/res, LOCKT4args/res, LOCKU4args/res, RELEASE_LOCKOWNER4args/res
- **Existing codebase** - Phase 9 state manager (`state/manager.go`, `state/stateid.go`, `state/openowner.go`), lock manager (`pkg/metadata/lock/manager.go`, `lock/types.go`), handler patterns (`handlers/stubs.go`, `handlers/close.go`, `handlers/open.go`), error codes (`types/constants.go`)
- **Linux kernel nfsd** (https://github.com/torvalds/linux/blob/master/fs/nfsd/nfs4state.c) - Reference implementation: `nfsd4_lock()`, `nfsd4_lockt()`, `nfsd4_locku()`, `nfs4_lockowner`, `nfs4_ol_stateid`, lockstateid_hashtbl

### Secondary (MEDIUM confidence)
- **WebSearch findings on lock-owner seqid management** - Verified against RFC 7530 Section 9.1.7 and Phase 9 existing implementation
- **Linux nfsd patch series on lock state revocation** (https://lore.kernel.org/all/20220128162340.GF14908@fieldses.org/T/) - Admin revocation of lock stateids, cleanup patterns

### Tertiary (LOW confidence)
- **Blocking lock behavior** - RFC 7530 Section 9.4 text not directly fetched due to document size; inferred from multiple secondary sources and RFC table of contents. Recommendation verified against Linux nfsd behavior (returns NFS4ERR_DENIED, does not block server-side).

## Metadata

**Confidence breakdown:**
- Lock-owner/lock-state data model: HIGH - directly from RFC 7531 XDR types and Phase 9 existing patterns
- LOCK operation (locker4 union, three cases): HIGH - XDR types confirmed from RFC 7531, implementation rules from RFC 7530 Section 16.10
- LOCKT/LOCKU operations: HIGH - straightforward XDR from RFC 7531, standard handler pattern
- RELEASE_LOCKOWNER: HIGH - simple XDR, clear semantics
- Integration with unified lock manager: HIGH - lock.Manager API is fully documented in codebase, EnhancedLock supports NFSv4 pattern
- NFS4ERR_OPENMODE validation: HIGH - standard check from RFC 7530
- Blocking lock semantics (READW_LT/WRITEW_LT): MEDIUM - RFC Section 9.4 text not directly verified but multiple sources agree on non-blocking server behavior
- Cross-protocol lock format: MEDIUM - owner ID format is a design choice, not RFC-specified

**Research date:** 2026-02-14
**Valid until:** 2026-03-16 (stable; RFC-based, well-established patterns)
