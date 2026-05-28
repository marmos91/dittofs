# Phase 39: Cross-Protocol Integration and Documentation - Research

**Researched:** 2026-03-02
**Domain:** Cross-protocol lock/delegation coordination, NFS/SMB bidirectional caching coherency
**Confidence:** HIGH

## Summary

Phase 39 implements bidirectional coordination between SMB3 leases and NFS delegations through the shared LockManager in `pkg/metadata/lock/`, plus comprehensive SMB3 documentation. The codebase already has strong infrastructure for this: the `LockManager` interface in `pkg/metadata/lock/manager.go` already manages unified locks, leases, and break callbacks; the `BreakCallbacks` interface dispatches break notifications to protocol adapters; the `DirChangeNotifier` triggers directory lease breaks on directory mutations; and the `MetadataService` already calls `notifyDirChange()` from `CreateFile`, `RemoveFile`, `CreateHardLink`, and `Move`.

The primary technical challenge is extracting NFS delegation state from the NFS-specific `internal/adapter/nfs/v4/state/DelegationState` into a protocol-neutral `Delegation` struct in `pkg/metadata/lock/`, refactoring the NFS v4 `StateManager` to delegate delegation management to the shared `LockManager`, and extending `BreakCallbacks` with `OnDelegationRecall`. The notification queue (for directory change notifications consumed by both NFS CB_NOTIFY and SMB CHANGE_NOTIFY) is a new bounded data structure in the lock package. The documentation work expands the existing 741-line `docs/SMB.md` to cover SMB3 features and updates `SECURITY.md`, `CONFIGURATION.md`, `TROUBLESHOOTING.md`, and `README.md`.

**Primary recommendation:** Follow the locked decisions exactly -- all delegation state moves to LockManager via a new `Delegation` struct on `UnifiedLock`, the `BreakCallbacks` interface gets `OnDelegationRecall`, and the `CheckAndBreakOpLocksFor*` methods become `CheckAndBreakCachingFor*` that break both leases AND delegations in unified calls.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- All delegation state (file + directory) moves from NFS adapter (`internal/adapter/nfs/v4/state/`) to the shared LockManager in `pkg/metadata/lock/`
- Delegations become a first-class lock type in `UnifiedLock` via a new `*Delegation` field (alongside existing `*OpLock` for leases)
- New protocol-neutral `Delegation` struct in `pkg/metadata/lock/` -- no NFS-specific fields (Stateid, RecallTimer). NFS adapter maintains its own `map[DelegationID]Stateid4` mapping
- LockManager supports multiple BreakCallbacks (one per adapter). Both NFS and SMB register theirs
- Extend the existing `BreakCallbacks` interface with `OnDelegationRecall()`. Both adapters implement it (no-ops for irrelevant methods). Same interface, extended
- `CheckAndBreakCachingForWrite/Read/Delete` calls break BOTH leases AND delegations in a single unified call. Callers don't need to know which protocols are active
- Break operations are fire-and-forget with timeout, consistent with current SMB lease break flow
- The existing `OpLockBreakScanner` is extended to also handle delegation recall timeouts (replacing the NFS adapter's per-delegation RecallTimer)
- Single `LockStore`, extended to handle delegation entries alongside leases and byte-range locks
- Unified anti-storm mechanism: `RecentlyRecalledTTL` applies cross-protocol (NFS delegation recalled due to SMB activity prevents re-grant for 30s, and vice versa)
- Per-share LockManager (keep current pattern). Adapters register BreakCallbacks once, applied to all shares
- Adapters register BreakCallbacks during `adapter.SetRuntime()` lifecycle hook
- MetadataService is the hook point: `CreateFile/RemoveFile/Rename` calls `LockManager.CheckAndBreakDirectoryCaching()` for the parent directory. Protocol-agnostic
- Bidirectional: SMB directory ops trigger NFS directory delegation recalls (CB_NOTIFY), and NFS directory ops trigger SMB3 directory lease breaks
- RENAME across directories breaks both source and target directory leases/delegations
- Directory delegations use the same `Delegation` struct with an `IsDirectory` bool flag
- LockManager owns a generic, bounded directory change notification queue (both NFS and SMB adapters consume)
- Bounded capacity (e.g., 1024 events/directory) with overflow collapsing to 'full rescan needed' event
- Flush strategy: time + size threshold (e.g., 100 events or 500ms)
- Typed events with filename: ChangeType (Add|Remove|Rename|Modify), OldName, NewName
- NFS adapter drains queue into CB_NOTIFY. SMB adapter drains into CHANGE_NOTIFY
- NFS CB_NOTIFY batching specifics (wire format optimization) stays in NFS adapter
- Parallel breaks: when both NFS delegation and SMB lease exist, initiate both simultaneously
- Force-revoke and proceed on timeout (consistent with current behavior)
- Reuse per-protocol timeouts: SMB lease 35s (MS-SMB2 default), NFS delegation uses configurable `delegation_recall_timeout` (90s default)
- Cross-protocol conflicts logged at INFO level (consistent with existing `cross_protocol.go`)
- No Prometheus metrics for cross-protocol breaks in this phase
- Auto-grant on break acknowledgment: when a break is acknowledged, check if pending requests can now proceed
- Condition variable / channel pattern for waiting goroutines (goroutine blocks on per-file channel, signaled on break acknowledgment)
- Waiting goroutine uses request's `context.Context` for timeout (no separate wait timeout)
- Allow coexistence for reads: NFS read delegation + SMB Read lease can coexist. Write delegation and Write lease are mutually exclusive
- NFS v4 state manager refactored to delegate delegation state to LockManager. It becomes a thin adapter between NFS wire types and shared lock layer
- NFS adapter maintains its own stateid mapping (`map[DelegationID]Stateid4`)
- NFS adapter's delegation code (grant, recall, revoke) modified in this phase -- not deferred
- Cross-protocol translation helpers (`cross_protocol.go`) extended for SMB3 Lease V2 (ParentLeaseKey, epoch)
- No feature flag for disabling cross-protocol coordination
- Unit tests for new LockManager delegation logic, cross-protocol break coordination, notification queue
- New test files: `delegation_test.go`, `cross_protocol_break_test.go` plus extend existing `cross_protocol_test.go`
- No integration tests with mock adapters -- rely on E2E tests with full adapters
- Existing NFS delegation E2E tests (`nfsv4_delegation_test.go`) updated to work with LockManager-based delegation
- Existing cross-protocol E2E tests (`cross_protocol_test.go`, `cross_protocol_lock_test.go`) updated and extended with SMB3 lease vs NFS delegation scenarios
- Expand existing `docs/SMB.md` in-place to cover all v3.8 features
- Both operational (mounting, configuration) and wire format details (for maintainers), following current SMB.md style
- Cross-protocol behavior matrix table
- Update `docs/SECURITY.md` with SMB3 security model
- Update `docs/CONFIGURATION.md` with SMB3 adapter configuration options
- Add cross-protocol troubleshooting section to `docs/TROUBLESHOOTING.md`
- Brief SMB3 mention in `README.md`

### Claude's Discretion
- Exact notification queue capacity and flush thresholds
- Whether LINK and SETATTR on directory trigger directory lease breaks (per MS-SMB2 spec analysis)
- Whether notifications carry typed events or 'changed' flag (per protocol spec comparison)
- Internal implementation details of the channel-based waiting mechanism
- Exact delegation recall timeout scanning interval
- Delegation struct field design details

### Deferred Ideas (OUT OF SCOPE)
- Prometheus metrics for cross-protocol breaks -- add later when observability is prioritized
- Cross-protocol coordination feature flag -- not needed, no measurable overhead when only one protocol active
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| XPROT-01 | SMB3 lease breaks coordinate bidirectionally with NFS delegations via Unified Lock Manager | Core research: `Delegation` struct in LockManager, `BreakCallbacks.OnDelegationRecall`, unified `CheckAndBreakCachingFor*` methods, coexistence rules (R deleg + R lease OK, W exclusive) |
| XPROT-02 | NFS directory operations trigger SMB3 directory lease breaks | Already partially implemented: `MetadataService.notifyDirChange()` calls `OnDirChange()` on LockManager which breaks directory leases. Needs extension for delegations via `CheckAndBreakDirectoryCaching` |
| XPROT-03 | Cross-protocol coordination logic lives in metadata service (shared abstract layer) | Architecture: all delegation state in `pkg/metadata/lock/`, MetadataService as hook point, NFS adapter is thin stateid-mapping wrapper |
| DOC-01 | Update docs/ with comprehensive SMB3 protocol documentation (configuration, capabilities, security) | Expand existing `docs/SMB.md` (741 lines), update `SECURITY.md`, `CONFIGURATION.md`, `TROUBLESHOOTING.md`, `README.md` |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `pkg/metadata/lock` | Internal | Shared LockManager with delegation, lease, byte-range support | Already established as the unified lock layer -- delegations extend existing patterns |
| `pkg/metadata` | Internal | MetadataService as hook point for directory change notifications | Already wired with `notifyDirChange()` on CreateFile/RemoveFile/Move |
| `internal/adapter/nfs/v4/state` | Internal | NFS-specific stateid mapping and CB_RECALL/CB_NOTIFY wire encoding | Existing NFS state manager becomes thin adapter over shared LockManager |
| `internal/adapter/smb/lease` | Internal | SMB-specific session-to-lease mapping and break notification dispatch | Existing `SMBBreakHandler` and `LeaseManager` patterns extended |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `sync` | stdlib | `sync.Map`/`sync.RWMutex` for concurrent state, channels for waiter notification | Coordination primitives throughout lock package |
| `context` | stdlib | Request-scoped cancellation for break waiter timeout | Waiting goroutines use request's `context.Context` |
| `time` | stdlib | TTL caches, timer-based flush, recall timeout scanning | Anti-storm cache, notification queue flush timer |
| `github.com/google/uuid` | v1.x | Delegation ID generation | Same pattern as existing `UnifiedLock.ID` |

### Alternatives Considered
None -- all decisions are locked. The architecture uses existing in-tree packages exclusively.

## Architecture Patterns

### Recommended Project Structure
```
pkg/metadata/lock/
├── manager.go              # Extended: CheckAndBreakCachingFor*, delegation CRUD
├── types.go                # Extended: UnifiedLock.Delegation field, Delegation struct
├── delegation.go           # NEW: Delegation struct, grant/recall/revoke logic
├── delegation_test.go      # NEW: Unit tests for delegation management
├── oplock_break.go         # Extended: OpLockBreakScanner scans delegations too
├── directory.go            # Extended: OnDirChange breaks both leases AND delegations
├── notification_queue.go   # NEW: Bounded directory change notification queue
├── notification_queue_test.go # NEW: Tests for notification queue
├── cross_protocol.go       # Extended: Lease V2 translation, delegation conflict reasons
├── cross_protocol_break.go # NEW: Unified break coordination (parallel lease+delegation)
├── cross_protocol_break_test.go # NEW: Tests for cross-protocol break coordination
├── leases.go               # Extended: requestLeaseImpl checks delegation coexistence
├── store.go                # Extended: PersistedLock delegation fields
└── callbacks.go            # NEW or in oplock_break.go: OnDelegationRecall added to BreakCallbacks

internal/adapter/nfs/v4/state/
├── delegation.go           # REFACTORED: Thin wrapper, delegates to LockManager
├── manager.go              # REFACTORED: delegByOther becomes map[DelegationID]Stateid4
└── dir_delegation.go       # REFACTORED: GrantDirDelegation delegates to LockManager

internal/adapter/smb/lease/
└── notifier.go             # EXTENDED: OnDelegationRecall no-op implementation

docs/
├── SMB.md                  # EXPANDED: All v3.8 features, cross-protocol matrix
├── SECURITY.md             # UPDATED: SMB3 security model section
├── CONFIGURATION.md        # UPDATED: SMB3 adapter config options
└── TROUBLESHOOTING.md      # UPDATED: Cross-protocol troubleshooting section
```

### Pattern 1: Protocol-Neutral Delegation Struct
**What:** A new `Delegation` struct in `pkg/metadata/lock/` that captures delegation semantics without any NFS-specific fields
**When to use:** Any delegation operation (grant, recall, revoke, query)
**Example:**
```go
// Source: pkg/metadata/lock/delegation.go (to be created)
// Delegation represents a granted file or directory delegation.
// Protocol-neutral: NFS-specific fields (Stateid4, RecallTimer) live in the NFS adapter.
type Delegation struct {
    // DelegationID is the unique identifier for this delegation.
    DelegationID string

    // DelegType indicates read or write delegation.
    // Uses protocol-neutral constants: DelegTypeRead, DelegTypeWrite.
    DelegType DelegationType

    // IsDirectory is true for directory delegations.
    IsDirectory bool

    // ClientID identifies the client holding the delegation.
    ClientID string

    // ShareName is the share this delegation belongs to.
    ShareName string

    // Breaking indicates a recall is in progress.
    Breaking bool

    // BreakStarted records when the recall was initiated.
    BreakStarted time.Time

    // Recalled indicates CB_RECALL has been sent.
    Recalled bool

    // Revoked indicates the delegation was force-revoked.
    Revoked bool

    // NotificationMask is the notification bitmask (directory delegations).
    NotificationMask uint32
}
```

### Pattern 2: Extended UnifiedLock with Delegation
**What:** `UnifiedLock` gains a `*Delegation` field alongside existing `*OpLock`
**When to use:** When storing delegation state in the LockManager's unified lock map
**Example:**
```go
// Source: pkg/metadata/lock/types.go (existing, to be extended)
type UnifiedLock struct {
    // ... existing fields ...
    Lease      *OpLock      // SMB2/3 lease state (nil for byte-range/delegation)
    Delegation *Delegation  // NFS delegation state (nil for byte-range/lease)
}

// IsDelegation returns true if this is an NFS delegation.
func (ul *UnifiedLock) IsDelegation() bool {
    return ul.Delegation != nil
}
```

### Pattern 3: Unified CheckAndBreakCaching Methods
**What:** Replace `CheckAndBreakOpLocksForWrite/Read/Delete` with `CheckAndBreakCachingForWrite/Read/Delete` that break both leases AND delegations
**When to use:** Any operation that conflicts with cached state (read, write, delete)
**Example:**
```go
// Source: pkg/metadata/lock/manager.go (existing, to be extended)
func (lm *Manager) CheckAndBreakCachingForWrite(handleKey string, excludeOwner *LockOwner) error {
    // Break leases (existing logic)
    lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, writeBreakPredicate)
    // Break delegations (new logic)
    lm.breakDelegations(handleKey, excludeOwner, func(d *Delegation) bool {
        return d.DelegType == DelegTypeWrite || d.DelegType == DelegTypeRead
    })
    return nil
}
```

### Pattern 4: Extended BreakCallbacks Interface
**What:** Add `OnDelegationRecall` to the existing `BreakCallbacks` interface
**When to use:** When a delegation needs to be recalled across protocols
**Example:**
```go
// Source: pkg/metadata/lock/oplock_break.go (existing, to be extended)
type BreakCallbacks interface {
    OnOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32)
    OnByteRangeRevoke(handleKey string, lock *UnifiedLock, reason string)
    OnAccessConflict(handleKey string, existingLock *UnifiedLock, requestedMode AccessMode)
    // NEW: Called when a delegation needs to be recalled
    OnDelegationRecall(handleKey string, lock *UnifiedLock)
}
```

### Pattern 5: NFS Adapter as Thin Stateid Mapper
**What:** NFS v4 StateManager maintains only NFS wire-format mappings, delegates all delegation logic to LockManager
**When to use:** The NFS adapter's delegation code
**Example:**
```go
// Source: internal/adapter/nfs/v4/state/delegation.go (existing, to be refactored)
// StateManager maintains NFS-specific state only:
//   - map[DelegationID]Stateid4 (wire-format stateid mapping)
//   - backchannel sender for CB_RECALL (NFS-specific wire format)
//
// All delegation lifecycle (grant decision, conflict check, revoke) delegates to LockManager.

func (sm *StateManager) GrantDelegation(clientID uint64, fileHandle []byte, delegType uint32) *DelegationState {
    // 1. Call LockManager to create delegation in shared state
    delegation := sm.lockManager.GrantDelegation(...)
    // 2. Generate NFS stateid and store mapping
    stateid := sm.generateStateidOther(StateTypeDeleg)
    sm.delegStateidMap[delegation.DelegationID] = stateid
    // 3. Return NFS-specific DelegationState wrapping the shared Delegation
    return &DelegationState{Stateid: stateid, Delegation: delegation}
}
```

### Pattern 6: Bounded Notification Queue
**What:** A generic, bounded queue of directory change events owned by LockManager, consumed by protocol adapters
**When to use:** Directory change notification batching for both CB_NOTIFY and CHANGE_NOTIFY
**Example:**
```go
// Source: pkg/metadata/lock/notification_queue.go (new)
type DirNotification struct {
    ChangeType DirChangeType  // Add, Remove, Rename, Modify
    EntryName  string
    OldName    string         // For rename
    NewName    string         // For rename
}

type NotificationQueue struct {
    mu       sync.Mutex
    events   []DirNotification
    capacity int              // e.g., 1024
    overflow bool             // true = "full rescan needed"
    flushCh  chan struct{}    // signals consumer on flush threshold
}
```

### Pattern 7: Channel-Based Break Waiter
**What:** Goroutines waiting for break acknowledgment block on a per-file channel, signaled when the break is acknowledged
**When to use:** When a conflicting operation needs to wait for a lease/delegation break
**Example:**
```go
// Source: pkg/metadata/lock/manager.go (new addition)
// WaitForBreakCompletion blocks until all breaks on handleKey are resolved
// or ctx is cancelled. Uses the request's context for timeout.
func (lm *Manager) WaitForBreakCompletion(ctx context.Context, handleKey string) error {
    lm.mu.Lock()
    ch := lm.getOrCreateBreakWaitCh(handleKey)
    lm.mu.Unlock()

    select {
    case <-ch:
        return nil // Break completed
    case <-ctx.Done():
        return ctx.Err() // Caller's timeout
    }
}
```

### Anti-Patterns to Avoid
- **Importing NFS types in lock package:** The `Delegation` struct must NOT contain `types.Stateid4`, `*time.Timer` for recall, or any NFS-specific wire types. These stay in `internal/adapter/nfs/v4/state/`.
- **Bidirectional dispatch in adapters:** The `BreakCallbacks` interface is the single dispatch mechanism. Do NOT have NFS adapter directly call SMB code or vice versa. The LockManager dispatches to all registered callbacks.
- **Holding LockManager mutex during TCP sends:** Break callbacks must NOT block on network I/O under the LockManager lock. The SMB `SMBBreakHandler.OnOpLockBreak` already dispatches asynchronously via `go func()`. The NFS delegation recall must follow the same pattern.
- **Re-implementing anti-storm logic per adapter:** The shared `recentlyBrokenCache` (existing in `directory.go`) and `RecentlyRecalledTTL` (existing in NFS state) should be unified into a single cross-protocol anti-storm cache in the LockManager.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Delegation ID generation | Custom incrementing IDs | `github.com/google/uuid` | Same pattern as `UnifiedLock.ID`, avoids collision risk |
| Notification queue | Channel-only queue | Bounded slice + overflow flag | Channels don't support overflow collapse to "rescan needed" |
| Break timeout scanning | Per-delegation timers | Extended `OpLockBreakScanner` | Consolidates all timeout scanning into a single goroutine |
| Anti-storm cache | Per-adapter recently-recalled maps | Extended `recentlyBrokenCache` in LockManager | Cross-protocol anti-storm requires shared state |

**Key insight:** The existing `OpLockBreakScanner`, `recentlyBrokenCache`, and `BreakCallbacks` patterns are production-proven. Extending them is far safer than building parallel delegation-specific mechanisms.

## Common Pitfalls

### Pitfall 1: Breaking Interface Backward Compatibility
**What goes wrong:** Adding `OnDelegationRecall` to `BreakCallbacks` interface breaks all existing implementations
**Why it happens:** Go interfaces are implicit -- adding a method requires all implementations to add it
**How to avoid:** Both existing implementations (`SMBBreakHandler` in `internal/adapter/smb/lease/notifier.go` and any NFS callback) must add the new method. The SMB implementation should be a no-op for `OnDelegationRecall` (SMB has no delegations). The NFS implementation sends CB_RECALL.
**Warning signs:** Compile errors in `pkg/adapter/smb/adapter.go` or `pkg/adapter/nfs/adapter.go` when building after interface change

### Pitfall 2: Deadlock Between LockManager and NFS StateManager
**What goes wrong:** LockManager dispatches `OnDelegationRecall`, NFS callback tries to acquire NFS StateManager mutex, but something already holds StateManager mutex and is waiting on LockManager
**Why it happens:** Lock ordering violation between `LockManager.mu` and `StateManager.mu`
**How to avoid:** LockManager releases its mutex BEFORE dispatching break callbacks (already the pattern in `breakOpLocks()`). NFS callback must not call back into LockManager methods that acquire the mutex. Use the existing pattern: collect locks to break under lock, release lock, then dispatch.
**Warning signs:** Test hangs, goroutine dump shows both goroutines waiting for each other's mutex

### Pitfall 3: Double-Recall Race
**What goes wrong:** NFS client sends DELEGRETURN at the same time server sends CB_RECALL. Both complete, but server tries to revoke an already-returned delegation.
**Why it happens:** Delegation removal and recall are not atomic across the network
**How to avoid:** Make delegation return idempotent (already the case in existing NFS code -- `ReturnDelegation` returns nil for not-found). Use the `Breaking` flag to skip already-breaking delegations.
**Warning signs:** Log messages about "delegation not found" during recall

### Pitfall 4: Notification Queue Memory Leak
**What goes wrong:** Notification queue grows unbounded for directories with many delegations but no consumers
**Why it happens:** Queue events accumulate when no adapter is consuming them (e.g., single-protocol deployment)
**How to avoid:** Bounded capacity with overflow flag. When capacity is reached, collapse to a single "full rescan needed" event. Additionally, clean up queues when the last delegation/lease on a directory is released.
**Warning signs:** Memory profile shows growing `[]DirNotification` slices

### Pitfall 5: Stale Delegation After File Delete
**What goes wrong:** File is deleted, but delegation still exists in LockManager because `RemoveAllLocks(handleKey)` doesn't clean up delegations
**Why it happens:** `RemoveAllLocks` was written before delegations were added to LockManager
**How to avoid:** Extend `RemoveAllLocks` to also clean up delegation entries. Or ensure `CheckAndBreakCachingForDelete` handles delegation cleanup before file removal.
**Warning signs:** Stale delegation entries in lock manager stats after file deletion

### Pitfall 6: Cross-Protocol Anti-Storm Gap
**What goes wrong:** NFS delegation recalled due to SMB write, then immediately re-granted by NFS because the NFS `recentlyRecalled` map is separate from LockManager's `recentlyBrokenCache`
**Why it happens:** Two separate anti-storm caches: one in NFS StateManager, one in LockManager
**How to avoid:** Unify into a single `recentlyBrokenCache` in LockManager. When a delegation is recalled (for any reason), mark the file handle in LockManager's cache. Both `ShouldGrantDelegation` (NFS) and `RequestLease` (SMB) check this cache.
**Warning signs:** Rapid grant-recall-grant-recall cycles visible in logs

## Code Examples

### Example 1: Delegation Struct (Protocol-Neutral)
```go
// Source: Based on existing OpLock struct pattern in pkg/metadata/lock/oplock.go
type DelegationType int

const (
    DelegTypeRead  DelegationType = iota
    DelegTypeWrite
)

type Delegation struct {
    DelegationID     string
    DelegType        DelegationType
    IsDirectory      bool
    ClientID         string
    ShareName        string
    Breaking         bool
    BreakStarted     time.Time
    Recalled         bool
    Revoked          bool
    NotificationMask uint32  // For directory delegations (NOTIFY4_* bitmask)
}

func (d *Delegation) Clone() *Delegation {
    if d == nil { return nil }
    clone := *d
    return &clone
}
```

### Example 2: Unified Break Method
```go
// Source: Based on existing breakOpLocks pattern in pkg/metadata/lock/manager.go
func (lm *Manager) CheckAndBreakCachingForWrite(handleKey string, excludeOwner *LockOwner) error {
    lm.mu.Lock()
    locks := lm.unifiedLocks[handleKey]

    var toBreakLeases []*UnifiedLock
    var toBreakDelegations []*UnifiedLock

    for _, lock := range locks {
        if excludeOwner != nil && lock.Owner.OwnerID == excludeOwner.OwnerID {
            continue
        }

        // Check leases (existing pattern)
        if lock.Lease != nil && !lock.Lease.Breaking {
            if lock.Lease.HasRead() || lock.Lease.HasWrite() {
                lock.Lease.Breaking = true
                lock.Lease.BreakToState = LeaseStateNone
                lock.Lease.BreakStarted = time.Now()
                advanceEpoch(lock.Lease)
                toBreakLeases = append(toBreakLeases, lock)
            }
        }

        // Check delegations (new)
        if lock.Delegation != nil && !lock.Delegation.Breaking {
            lock.Delegation.Breaking = true
            lock.Delegation.BreakStarted = time.Now()
            toBreakDelegations = append(toBreakDelegations, lock)
        }
    }
    lm.mu.Unlock()

    // Dispatch in parallel (both lease breaks and delegation recalls)
    for _, lock := range toBreakLeases {
        lm.dispatchOpLockBreak(handleKey, lock, LeaseStateNone)
    }
    for _, lock := range toBreakDelegations {
        lm.dispatchDelegationRecall(handleKey, lock)
    }

    return nil
}
```

### Example 3: NFS BreakCallbacks Implementation
```go
// Source: Based on existing SMBBreakHandler pattern in internal/adapter/smb/lease/notifier.go
type NFSBreakHandler struct {
    stateManager *StateManager  // NFS-specific state for CB_RECALL
}

func (h *NFSBreakHandler) OnDelegationRecall(handleKey string, lock *lock.UnifiedLock) {
    if lock == nil || lock.Delegation == nil {
        return
    }
    // Look up NFS stateid from delegation ID
    stateid, found := h.stateManager.GetStateidForDelegation(lock.Delegation.DelegationID)
    if !found {
        return  // Not an NFS delegation
    }
    // Send CB_RECALL via backchannel (async, never block)
    go h.stateManager.sendRecall(stateid, lock.Owner.ClientID)
}

func (h *NFSBreakHandler) OnOpLockBreak(_ string, _ *lock.UnifiedLock, _ uint32) {
    // No-op: NFS doesn't have SMB-style leases
}

func (h *NFSBreakHandler) OnByteRangeRevoke(_ string, _ *lock.UnifiedLock, _ string) {
    // No-op for NFS
}

func (h *NFSBreakHandler) OnAccessConflict(_ string, _ *lock.UnifiedLock, _ lock.AccessMode) {
    // No-op for NFS
}
```

### Example 4: Coexistence Rules
```go
// Source: Based on existing OpLocksConflict in pkg/metadata/lock/oplock.go
// Read delegation + Read lease = OK (multiple readers)
// Write delegation + any lease = CONFLICT (exclusive)
// Any delegation + Write lease = CONFLICT (exclusive)
func DelegationConflictsWithLease(deleg *Delegation, lease *OpLock) bool {
    if deleg.DelegType == DelegTypeWrite {
        // Write delegation conflicts with any lease
        return lease.LeaseState != LeaseStateNone
    }
    // Read delegation only conflicts with Write leases
    return lease.HasWrite()
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| NFS delegations in adapter-specific state | Delegations as first-class lock type in shared LockManager | Phase 39 (this phase) | Enables bidirectional cross-protocol coordination |
| Separate anti-storm caches (NFS vs LockManager) | Unified `RecentlyRecalledTTL` in LockManager | Phase 39 (this phase) | Cross-protocol anti-storm prevention |
| `CheckAndBreakOpLocksFor*` (leases only) | `CheckAndBreakCachingFor*` (leases + delegations) | Phase 39 (this phase) | Callers don't need to know which protocols are active |
| Per-delegation `RecallTimer` in NFS adapter | `OpLockBreakScanner` scans delegations too | Phase 39 (this phase) | Single background goroutine for all timeout management |
| NFS-specific `NotifyDirChange` batching | Generic notification queue in LockManager | Phase 39 (this phase) | Both NFS CB_NOTIFY and SMB CHANGE_NOTIFY consume from same queue |

## Open Questions

1. **LINK and SETATTR triggering directory lease breaks**
   - What we know: MS-SMB2 Section 3.3.4.7 says "the object store indicates that the directory has been modified" without listing exact operations. CREATE, REMOVE, RENAME are clearly specified.
   - What's unclear: Whether LINK (creating a hard link into a directory) and SETATTR (changing directory metadata) should trigger breaks
   - Recommendation: LINK should trigger breaks (it adds a visible entry to the directory listing). SETATTR on directory metadata (mtime, permissions) should NOT trigger directory lease breaks since directory listing content doesn't change. This matches Linux kernel NFS behavior where only entry additions/removals trigger CB_NOTIFY.
   - Confidence: MEDIUM (based on reasoning, not explicit spec language)

2. **Notification queue flush timing details**
   - What we know: User decision specifies "100 events or 500ms" as thresholds
   - What's unclear: Exact timer management -- is the timer per-directory or global?
   - Recommendation: Per-directory timers to avoid coupling unrelated directories. Start timer on first event, reset on flush. This matches the existing NFS `BatchTimer` pattern in `DelegationState`.
   - Confidence: HIGH (follows existing pattern)

3. **OpLockBreakScanner scanning interval for delegations**
   - What we know: Current scanner uses 1-second interval (`OpLockBreakScanInterval`). NFS delegation recall timeout is 90s.
   - What's unclear: Whether a 1-second scan interval is appropriate for 90s timeouts
   - Recommendation: Keep 1-second interval. The scan is lightweight (iterate persisted leases), and the 90s timeout means a few extra iterations are negligible. Having a separate longer interval adds complexity for no benefit.
   - Confidence: HIGH

4. **Backward compatibility of CheckAndBreakOpLocksFor* rename**
   - What we know: These methods are called from NFS handlers, SMB handlers, and MetadataService
   - What's unclear: Whether to keep old methods as aliases or require callers to update
   - Recommendation: Rename to `CheckAndBreakCachingFor*` and update all callers. The old names reference "OpLocks" which is now a subset of the functionality. Keep the old method signatures as deprecated wrappers if needed for compilation during migration.
   - Confidence: HIGH

## Sources

### Primary (HIGH confidence)
- `pkg/metadata/lock/manager.go` -- LockManager interface and implementation (1261 lines)
- `pkg/metadata/lock/types.go` -- UnifiedLock, OpLock, LockOwner types (358 lines)
- `pkg/metadata/lock/oplock.go` -- Lease state management, conflict detection (256 lines)
- `pkg/metadata/lock/oplock_break.go` -- BreakCallbacks interface, OpLockBreakScanner (265 lines)
- `pkg/metadata/lock/directory.go` -- DirChangeNotifier, recentlyBrokenCache (178 lines)
- `pkg/metadata/lock/cross_protocol.go` -- NLM/SMB translation helpers (371 lines)
- `pkg/metadata/lock/leases.go` -- RequestLease, AcknowledgeLeaseBreak (379 lines)
- `pkg/metadata/lock/store.go` -- PersistedLock, LockStore interface (333 lines)
- `internal/adapter/nfs/v4/state/delegation.go` -- NFS DelegationState, grant/recall/revoke (671 lines)
- `internal/adapter/nfs/v4/state/dir_delegation.go` -- Directory delegation management
- `internal/adapter/nfs/v4/state/manager.go` -- NFS StateManager with delegation maps
- `internal/adapter/smb/lease/manager.go` -- SMB LeaseManager thin wrapper (259 lines)
- `internal/adapter/smb/lease/notifier.go` -- SMBBreakHandler, SMBOplockBreaker (177 lines)
- `pkg/adapter/adapter.go` -- OplockBreaker interface
- `pkg/adapter/smb/adapter.go` -- SMB SetRuntime with BreakCallbacks registration
- `pkg/adapter/nfs/adapter.go` -- NFS SetRuntime with StateManager initialization
- `pkg/metadata/file_create.go` -- CreateFile with notifyDirChange hook
- `pkg/metadata/file_remove.go` -- RemoveFile with notifyDirChange hook
- `pkg/metadata/file_modify.go` -- Move with notifyDirChange for both source and target dirs
- `pkg/metadata/service.go` -- MetadataService, dirChangeNotifiers map
- `pkg/metadata/unified_view.go` -- UnifiedLockView for cross-protocol visibility
- `docs/SMB.md` -- Existing SMB documentation (741 lines, covers SMB2 only)
- `docs/SECURITY.md` -- Existing security documentation
- `docs/CONFIGURATION.md` -- Existing configuration documentation
- `docs/TROUBLESHOOTING.md` -- Existing troubleshooting documentation

### Secondary (MEDIUM confidence)
- MS-SMB2 Section 3.3.4.7 -- Object Store Indicates a Lease Break (directory lease break triggers)
- RFC 7530 Section 10.4 -- NFSv4 delegation semantics
- RFC 8881 Section 18.39 -- GET_DIR_DELEGATION (directory delegations)
- RFC 8881 Section 20.4 -- CB_NOTIFY (directory change notification)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all in-tree, well-understood packages with established patterns
- Architecture: HIGH -- user locked all major decisions, patterns follow existing codebase conventions
- Pitfalls: HIGH -- identified from actual codebase analysis (lock ordering, race conditions, anti-storm gaps)

**Research date:** 2026-03-02
**Valid until:** 2026-04-01 (stable, internal codebase, no external dependencies changing)
