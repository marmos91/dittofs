# Generic Lock Interface Design (Phase 1.1-1.4 of CORE_REFACTORING_PLAN)

## Context

The `pkg/metadata/lock/` package currently has protocol-specific types (SMB leases, NFS grace period) mixed into the generic layer. But SMB leases, NFSv4 delegations, SMB share modes, and NFSv4 OPEN deny modes are all instances of **the same generic concepts** with different wire formats:

| Generic Concept | SMB | NFSv4 | NLM |
|----------------|-----|-------|-----|
| **OpLock** | Lease (R/W/H flags) | Delegation (READ/WRITE) | n/a |
| **AccessMode** | Share reservation (DenyRead/Write/All) | OPEN4_SHARE_DENY_* | n/a |
| **Break/recall** | LEASE_BREAK notification | CB_RECALL callback | n/a |
| **Grace period** | Reconnect + reclaim | Lock reclaim window | Lock reclaim window |
| **Byte-range lock** | SMB2 LOCK | NFSv4 LOCK | NLM LOCK |

**Design principle:** All locking logic is centralized. Fix a bug once, fix it for all protocols. Protocol adapters are thin translators between wire format and generic types.

---

## Naming Convention

| Old name | New name | Rationale |
|----------|----------|-----------|
| `EnhancedLock` | **`UnifiedLock`** | Conveys cross-protocol unification |
| `LeaseInfo` | **`OpLock`** | Well-known filesystem term; SMB leases and NFSv4 delegations both evolved from it |
| `LeaseState*` constants | **`OpLockRead/Write/Handle`** | Consistent prefix |
| `ShareReservation` | **`AccessMode`** | Generic term for deny-others semantics |
| `LeaseBreakScanner` | **`OpLockBreakScanner`** | Follows OpLock naming |
| `LeaseBreakCallback` | **`OpLockBreakCallback`** | Follows OpLock naming |
| `CachingGrantsConflict` | **`OpLocksConflict`** | Follows OpLock naming |
| `IsLease` (query filter) | **`IsOpLock`** | Follows OpLock naming |

---

## Generic Types

### `OpLockState` -- bitmask for caching permissions

```go
// pkg/metadata/lock/types.go

type OpLockState uint8

const (
    OpLockRead   OpLockState = 0x01 // Cache reads locally
    OpLockWrite  OpLockState = 0x02 // Cache writes locally (dirty data)
    OpLockHandle OpLockState = 0x04 // Cache open handles (delay close)
)

func (s OpLockState) HasRead() bool   { return s&OpLockRead != 0 }
func (s OpLockState) HasWrite() bool  { return s&OpLockWrite != 0 }
func (s OpLockState) HasHandle() bool { return s&OpLockHandle != 0 }
func (s OpLockState) String() string  // "None", "R", "RW", "RH", "RWH"
```

Maps directly to:
- SMB: `LeaseStateRead=0x01, Write=0x02, Handle=0x04` (same bit layout)
- NFSv4: `OPEN_DELEGATE_READ` -> `OpLockRead`, `OPEN_DELEGATE_WRITE` -> `OpLockRead|OpLockWrite`

### `OpLock` -- replaces `LeaseInfo`

```go
// pkg/metadata/lock/types.go

// OpLock represents a server-granted opportunistic lock on a file.
// SMB calls this a "lease", NFSv4 calls it a "delegation".
// The semantics are identical: the server allows the client to cache
// operations locally until the oplock is broken/recalled.
type OpLock struct {
    // GroupKey identifies the caching unit (opaque string).
    // Locks with the same GroupKey belong to the same caching unit and
    // don't conflict with each other.
    //   SMB:   hex(128-bit LeaseKey)
    //   NFSv4: fmt.Sprintf("nfs4:%d", clientID)
    GroupKey string

    // State is the current caching permission flags.
    State OpLockState

    // Breaking indicates a break/recall is in progress.
    Breaking bool

    // BreakTarget is the target state after the break completes.
    BreakTarget OpLockState

    // BreakStarted records when the break was initiated (for timeout enforcement).
    BreakStarted time.Time

    // StateVersion is incremented on every state change.
    // Maps to SMB3 epoch or NFSv4 delegation seqid.
    StateVersion uint32

    // Reclaim indicates this oplock was reclaimed during grace period.
    Reclaim bool
}
```

### `AccessMode` -- replaces `ShareReservation`

```go
// pkg/metadata/lock/types.go

// AccessMode controls what other clients are denied while a file is open.
// SMB calls this "share reservation", NFSv4 calls it "share deny".
type AccessMode uint32

const (
    DenyNone  AccessMode = 0 // Allow all (default)
    DenyRead  AccessMode = 1 // Deny reads to others
    DenyWrite AccessMode = 2 // Deny writes to others
    DenyAll   AccessMode = 3 // Deny all access to others
)
```

Maps directly to:
- SMB: `ShareReservationNone/DenyRead/DenyWrite/DenyAll`
- NFSv4: `OPEN4_SHARE_DENY_NONE/READ/WRITE/BOTH`

### `UnifiedLock` -- replaces `EnhancedLock`

```go
type UnifiedLock struct {
    ID         string
    Owner      LockOwner
    FileHandle FileHandle
    Offset     uint64       // Byte range start; 0 for oplocks (whole-file)
    Length     uint64       // 0 = to EOF; 0 for oplocks (whole-file)
    Type       LockType     // Shared or Exclusive
    AccessMode AccessMode   // Deny others access
    OpLock     *OpLock      // nil for byte-range locks, non-nil for oplocks
    AcquiredAt time.Time
    Blocking   bool
    Reclaim    bool
}

func (ul *UnifiedLock) HasOpLock() bool { return ul.OpLock != nil }
```

---

## Centralized Conflict Detection

All conflict logic stays in `pkg/metadata/lock/`. The generic layer handles all cases:

```go
// IsConflicting handles ALL conflict cases centrally:
//   1. OpLock vs OpLock (same GroupKey = no conflict)
//   2. OpLock vs byte-range lock (Write oplock vs exclusive lock)
//   3. Byte-range vs byte-range (overlap + shared/exclusive rules)
//   4. AccessMode conflicts
func IsConflicting(existing, requested *UnifiedLock) bool {
    // Same owner - no conflict
    if existing.Owner.OwnerID == requested.Owner.OwnerID {
        return false
    }

    existingHasOpLock := existing.HasOpLock()
    requestedHasOpLock := requested.HasOpLock()

    // Case 1: Both have oplocks
    if existingHasOpLock && requestedHasOpLock {
        return OpLocksConflict(existing.OpLock, requested.OpLock)
    }

    // Case 2: One has oplock, one is byte-range
    if existingHasOpLock != requestedHasOpLock {
        return OpLockConflictsWithByteLock(...)
    }

    // Case 3: Both are byte-range locks
    if !RangesOverlap(existing.Offset, existing.Length, requested.Offset, requested.Length) {
        return false
    }
    if existing.Type == LockTypeShared && requested.Type == LockTypeShared {
        return false
    }
    return true
}

// OpLocksConflict: same GroupKey = no conflict,
// Write oplock conflicts with any other oplock, Read oplocks coexist.
func OpLocksConflict(a, b *OpLock) bool

// OpLockConflictsWithByteLock: Write oplock + exclusive byte lock = conflict.
func OpLockConflictsWithByteLock(oplock *OpLock, oplockOwner string, byteLock *UnifiedLock) bool
```

These are **renamed versions** of the existing `LeasesConflict` and `LeaseConflictsWithByteRangeLock` -- logic is identical.

---

## Centralized Break/Recall

### `OpLockBreakScanner` -- replaces `LeaseBreakScanner`

```go
// pkg/metadata/lock/oplock_break.go (renamed from lease_break.go)

// OpLockBreakCallback is called when an oplock break times out.
type OpLockBreakCallback interface {
    OnBreakTimeout(groupKey string)
}

// OpLockBreakScanner monitors breaking oplocks and force-revokes on timeout.
// Used by both SMB (lease break timeout) and NFSv4 (delegation recall timeout).
type OpLockBreakScanner struct { /* same internals as LeaseBreakScanner */ }
```

### Break trigger functions -- centralized

```go
// CheckAndBreakOpLocksForWrite triggers oplock breaks needed before a write.
// Returns ErrOpLockBreakPending if a break was initiated and caller should retry.
func CheckAndBreakOpLocksForWrite(oplocks []*UnifiedLock) error

// CheckAndBreakOpLocksForRead triggers breaks needed before a read.
// Only Write oplocks need breaking (flush dirty data).
func CheckAndBreakOpLocksForRead(oplocks []*UnifiedLock) error

// CheckAndBreakOpLocksForDelete triggers breaks before deleting a file.
// Handle oplocks must be broken.
func CheckAndBreakOpLocksForDelete(oplocks []*UnifiedLock) error
```

---

## Grace Period -- stays generic, stays in place

```go
// pkg/metadata/lock/grace.go (unchanged)

// GracePeriodManager manages the grace period state machine.
// After server restart, clients reclaim their locks/oplocks.
// Used by NLM, NFSv4, and SMB reconnect flows.
type GracePeriodManager struct { /* unchanged */ }
```

---

## Persistence

### `PersistedLock` -- native fields with new names

```go
type PersistedLock struct {
    // Core fields (unchanged)
    ID          string    `json:"id"`
    ShareName   string    `json:"share_name"`
    FileID      string    `json:"file_id"`
    OwnerID     string    `json:"owner_id"`
    ClientID    string    `json:"client_id"`
    LockType    int       `json:"lock_type"`
    Offset      uint64    `json:"offset"`
    Length      uint64    `json:"length"`
    AcquiredAt  time.Time `json:"acquired_at"`
    ServerEpoch uint64    `json:"server_epoch"`

    // Renamed fields
    AccessMode int `json:"access_mode"` // was ShareReservation

    // OpLock fields (renamed from lease fields)
    OpLockGroupKey      string `json:"oplock_group_key,omitempty"`      // was LeaseKey
    OpLockState         uint8  `json:"oplock_state,omitempty"`          // was LeaseState
    OpLockStateVersion  uint32 `json:"oplock_state_version,omitempty"`  // was LeaseEpoch
    OpLockBreakTarget   uint8  `json:"oplock_break_target,omitempty"`   // was BreakToState
    OpLockBreaking      bool   `json:"oplock_breaking,omitempty"`       // was Breaking
}

func (pl *PersistedLock) IsOpLock() bool {
    return pl.OpLockGroupKey != ""
}
```

### `LockStore` -- `ReclaimLease` -> `ReclaimOpLock`

```go
type LockStore interface {
    // CRUD (unchanged)
    PutLock(ctx context.Context, lock *PersistedLock) error
    GetLock(ctx context.Context, lockID string) (*PersistedLock, error)
    DeleteLock(ctx context.Context, lockID string) error
    ListLocks(ctx context.Context, query LockQuery) ([]*PersistedLock, error)

    // Bulk (unchanged)
    DeleteLocksByClient(ctx context.Context, clientID string) (int, error)
    DeleteLocksByFile(ctx context.Context, fileID string) (int, error)

    // Server epoch (unchanged)
    GetServerEpoch(ctx context.Context) (uint64, error)
    IncrementServerEpoch(ctx context.Context) (uint64, error)

    // OpLock reclaim (renamed from ReclaimLease - works for SMB + NFSv4)
    ReclaimOpLock(ctx context.Context, fileHandle FileHandle, groupKey string, clientID string) (*UnifiedLock, error)
}
```

### `LockQuery` -- `IsLease` -> `IsOpLock`

```go
type LockQuery struct {
    FileID    string
    OwnerID   string
    ClientID  string
    ShareName string
    IsOpLock  *bool  // was IsLease; nil=all, true=oplocks only, false=byte-range only
}
```

---

## Manager API

```go
type Manager struct {
    mu            sync.RWMutex
    locks         map[string][]FileLock      // legacy (eventually deprecated)
    unifiedLocks  map[string][]*UnifiedLock  // was enhancedLocks
}

// Legacy FileLock API (unchanged):
// Lock, Unlock, UnlockAllForSession, TestLock, CheckForIO, ListLocks, RemoveFileLocks

// UnifiedLock API (renamed from Enhanced*):
func (lm *Manager) AddUnifiedLock(handleKey string, lock *UnifiedLock) error
func (lm *Manager) RemoveUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) error
func (lm *Manager) ListUnifiedLocks(handleKey string) []*UnifiedLock
func (lm *Manager) RemoveFileUnifiedLocks(handleKey string)
func (lm *Manager) UpgradeLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error)
```

---

## Validation -- centralized

```go
// pkg/metadata/lock/oplock.go

// Valid oplock states for files: None, R, RW, RH, RWH
var ValidFileOpLockStates = []OpLockState{
    0, OpLockRead, OpLockRead|OpLockWrite,
    OpLockRead|OpLockHandle, OpLockRead|OpLockWrite|OpLockHandle,
}

// Valid oplock states for directories: None, R, RH
var ValidDirOpLockStates = []OpLockState{
    0, OpLockRead, OpLockRead|OpLockHandle,
}

func IsValidFileOpLockState(s OpLockState) bool
func IsValidDirOpLockState(s OpLockState) bool
```

---

## Protocol Adapter Translation (thin layers)

### SMB adapter

```go
func ToOpLock(leaseKey [16]byte, state uint32, epoch uint16) *lock.OpLock {
    return &lock.OpLock{
        GroupKey:      hex.EncodeToString(leaseKey[:]),
        State:         lock.OpLockState(state), // Same bit layout
        StateVersion:  uint32(epoch),
    }
}

func ToAccessMode(sr ShareReservation) lock.AccessMode {
    return lock.AccessMode(sr) // Same values
}
```

### NFSv4 adapter

```go
func ToOpLock(clientID uint64, delegType uint32) *lock.OpLock {
    var flags lock.OpLockState
    switch delegType {
    case types.OPEN_DELEGATE_READ:
        flags = lock.OpLockRead
    case types.OPEN_DELEGATE_WRITE:
        flags = lock.OpLockRead | lock.OpLockWrite
    }
    return &lock.OpLock{
        GroupKey: fmt.Sprintf("nfs4:%d", clientID),
        State:   flags,
    }
}

func ToAccessMode(shareDeny uint32) lock.AccessMode {
    switch shareDeny {
    case types.OPEN4_SHARE_DENY_READ:  return lock.DenyRead
    case types.OPEN4_SHARE_DENY_WRITE: return lock.DenyWrite
    case types.OPEN4_SHARE_DENY_BOTH:  return lock.DenyAll
    default:                           return lock.DenyNone
    }
}
```

### NLM adapter

No oplocks or access modes. Only byte-range locks. `OpLock` stays nil, `AccessMode` stays `DenyNone`.

---

## MetadataService changes

**Renamed** (logic stays centralized):
- `CheckAndBreakLeasesForWrite/Read/Delete` -> `CheckAndBreakOpLocksForWrite/Read/Delete`
- `ReclaimLeaseSMB` -> `ReclaimOpLock` (now works for both SMB + NFSv4)

**Moved to NFS adapter** (NLM-specific orchestration, not core lock logic):
- `LockFileNLM`, `TestLockNLM`, `UnlockFileNLM`, `CancelBlockingLock`, `SetNLMUnlockCallback`
- These are NLM-specific orchestration (blocking queues, grant callbacks) around generic lock operations

**Removed:**
- `OplockChecker` interface + globals (oplock break logic is now centralized)

---

## File Rename Summary

| Current file | New file | Changes |
|-------------|----------|---------|
| `lock/types.go` | `lock/types.go` | `EnhancedLock`->`UnifiedLock`, `ShareReservation`->`AccessMode`, add `OpLockState` type |
| `lock/lease_types.go` | `lock/oplock.go` | `LeaseInfo`->`OpLock`, `LeaseState*`->`OpLock*`, validation functions renamed |
| `lock/lease_break.go` | `lock/oplock_break.go` | `LeaseBreakScanner`->`OpLockBreakScanner`, `LeaseBreakCallback`->`OpLockBreakCallback` |
| `lock/grace.go` | `lock/grace.go` | **No change** -- already generic |
| `lock/manager.go` | `lock/manager.go` | `enhancedLocks`->`unifiedLocks`, method renames `*Enhanced*`->`*Unified*` |
| `lock/store.go` | `lock/store.go` | `PersistedLock` field renames, `ReclaimLease`->`ReclaimOpLock`, `IsLease`->`IsOpLock` |
| `lock/errors.go` | `lock/errors.go` | `NewLockConflictError` param type update |

No files move to protocol adapters (except NLM orchestration methods from `service.go`).

---

## Verification

1. `go build ./...`
2. `go vet ./...`
3. `go test ./...`
4. `go test -race ./...`
5. Verify SMB oplock/lease tests still pass
6. Verify NFSv4 delegation tests still pass
7. Verify NLM lock tests still pass
8. Verify cross-protocol conflict tests (NFS write -> SMB oplock break)
