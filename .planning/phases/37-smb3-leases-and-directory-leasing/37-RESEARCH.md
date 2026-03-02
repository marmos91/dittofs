# Phase 37: SMB3 Leases and Directory Leasing - Research

**Researched:** 2026-03-02
**Domain:** SMB3 Lease V2, directory leasing, unified lock management, cross-protocol lease coordination
**Confidence:** HIGH

## Summary

Phase 37 consolidates all SMB lease management from the adapter-level `OplockManager` into the shared `pkg/metadata/lock/Manager`, adds Lease V2 support (ParentLeaseKey, epoch tracking), and introduces directory leasing with a `DirChangeNotifier` callback pattern that unifies notification dispatch for both SMB and NFS protocols.

The existing codebase provides strong foundations: `OplockManager` already implements `RequestLease`, `AcknowledgeLeaseBreak`, `ReleaseLease` with full V2 context parsing, `OpLock` struct already has `Epoch` field, `ValidDirectoryLeaseStates` is already defined, `BreakCallbacks` interface exists with `OnOpLockBreak` method, and `OpLockBreakScanner` handles timeout enforcement. The NFS4 `StateManager` provides the reference architecture (thin protocol wrapper over shared infrastructure) with its `dir_delegation.go` and `recentlyRecalled` cache pattern.

The core migration is: move lease CRUD + NLM conflict checking from `internal/adapter/smb/v2/handlers/` into `pkg/metadata/lock/`, create a thin `internal/adapter/smb/lease/` wrapper (parallel to NFS4 StateManager), add `DirChangeNotifier` to MetadataService mutation methods, and refactor NFS4 handlers to use the unified notification path.

**Primary recommendation:** Follow the incremental migration strategy from CONTEXT.md -- add lease methods to LockManager, update call sites, then delete OplockManager. Each step independently testable.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- **Full merge into LockManager** -- move ALL lease CRUD (RequestLease, AcknowledgeBreak, ReleaseLease, conflict checks, NLM cross-protocol checks) from OplockManager into `pkg/metadata/lock/Manager`. LockManager becomes the single source of truth for lease state.
- **Delete OplockManager entirely** -- don't gut/refactor, delete. Create a new thin SMB `LeaseManager` wrapper in `internal/adapter/smb/lease/` (parallel to `internal/adapter/nfs/v4/state/`). The wrapper only handles: sessionID-to-leaseKey mapping and SendLeaseBreak notification dispatch.
- **No SMB-side caching** -- all lease lookups go through LockManager. If performance matters, add caching inside LockManager itself, not in the adapter wrapper.
- **Cross-protocol NLM checks move into LockManager** -- `checkNLMLocksForLeaseConflict()` logic joins `CheckAndBreakOpLocksForWrite/Read/Delete` and all cross-protocol translation in `pkg/metadata/lock/`. One location for all protocol interop.
- **LockManager tracks break state** -- activeBreaks map (lease break timeout tracking) moves into LockManager alongside the existing `OpLockBreakScanner`. SMB LeaseManager only keeps sessionMap (SMB-specific session->lease binding).
- **Protocol-agnostic reclaim** -- `LockManager.ReclaimLease(leaseKey, requestedState, isDirectory)` replaces both `ReclaimLeaseSMB()` and `ReclaimDelegation()`. Single method for both protocols.
- **Restricted lease upgrade transitions** -- LockManager validates upgrade transitions against a whitelist (R->RW, R->RH, RH->RWH, etc.). Rejects invalid transitions (downgrades, invalid states).
- **Unified persistence** -- LockManager uses its existing LockStore for all lease persistence. Remove OplockManager's separate persistence path.
- **Incremental migration** -- 1) Add lease methods to LockManager, 2) Update call sites to use LockManager, 3) Delete OplockManager. Each step testable independently.
- **Per-share isolation** -- lease state remains per-share (each share has its own LockManager). No global lease registry.
- **Single LockManager interface** -- no sub-interfaces. Add lease methods (RequestLease, AcknowledgeBreak, ReleaseLease, ReclaimLease) directly to the existing LockManager interface.
- **Strict spec triggers for directory lease breaks** -- only create, delete, and rename within the directory trigger directory lease breaks (per MS-SMB2 3.3.4.7). SetInfo/attribute changes do NOT break.
- **Immediate dispatch** -- no batching for SMB directory breaks (unlike NFS4 batched notifications). Break the directory lease synchronously when the mutation happens.
- **DirChangeNotifier callback** -- MetadataService gets a `DirChangeNotifier` interface. After successful create/delete/rename, calls `notifier.OnDirChange(parentHandle, changeType, originClient)`. LockManager implements this interface.
- **Self-notification exclusion** -- pass originating client identity in the DirChangeNotifier callback. LockManager skips breaking leases owned by the originating client.
- **Non-blocking cross-protocol** -- NFS operations that trigger SMB directory lease breaks proceed immediately. Break is dispatched asynchronously. If SMB client doesn't acknowledge within 35s, lease is force-revoked.
- **Recently-broken cache** -- after breaking a directory lease, don't grant one for the same directory for N seconds. Prevents grant-break storms on busy directories.
- **Single notification path** -- MetadataService notifies LockManager only. LockManager fans out to both NFS4 StateManager and SMB LeaseManager via the existing `BreakCallbacks` interface.
- **Reuse OnOpLockBreak** -- directory leases stored as `UnifiedLock` with `Lease` field. `OnOpLockBreak` already dispatches for these. No new BreakCallbacks method needed.
- **NFS4 handlers refactored** -- NFS handlers no longer call `StateManager.NotifyDirChange()` directly. All directory change notifications flow through: NFS handlers -> MetadataService -> LockManager -> BreakCallbacks -> StateManager.
- **Allow dual directory leases** -- both NFS directory delegation and SMB directory lease can co-exist on the same directory.
- **V2 only for directory leases** -- directory leases require Lease V2 context (ParentLeaseKey + epoch). V1 clients (SMB 2.1) can still get file leases.
- **ParentLeaseKey: store but don't validate** -- accept and store ParentLeaseKey. Echo it in break notifications. Don't require a matching directory lease to exist.
- **Size-based V1/V2 detection** -- keep current approach: 52 bytes = V2, 32 bytes = V1. Already implemented.
- **Directory leases are reclaimable** -- persisted and reclaimable during grace period, same as file leases.
- **Single ReclaimLease method** -- `LockManager.ReclaimLease(leaseKey, requestedState, isDirectory)` for both SMB and NFS.
- **Reject reclaim on deleted directory** -- LockManager checks if directory handle still exists in MetadataStore.
- **Single-word filenames in lock/** -- `leases.go`, `directory.go`, `reclaim.go`. No underscores.

### Claude's Discretion
- Exact smbtorture/MSVP test mapping to expected-pass
- Unsolicited message dispatch mechanism for LEASE_BREAK_Notification
- Conformance test organization (locktest/ package vs inline tests)
- Recently-broken cache duration (suggested 5s but Claude can adjust)
- Sequence diagram format (Mermaid vs ASCII)

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| LEASE-01 | Server grants Lease V2 with ParentLeaseKey and epoch tracking in CREATE responses | Existing `DecodeLeaseCreateContext` already parses V2 (52-byte detection). `OpLock.Epoch` already exists. Need to add `ParentLeaseKey` to `PersistedLock` and `OpLock`, update `EncodeLeaseResponseContext` to include ParentLeaseKey with `SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET`. LockManager's `RequestLease` sets initial epoch=1, increments on state change per MS-SMB2 3.3.5.9.11. |
| LEASE-02 | Server grants directory leases (Read-caching) for SMB 3.0+ clients | `ValidDirectoryLeaseStates` already defined (None, R, RH). `IsValidDirectoryLeaseState` validation exists. LockManager's `RequestLease` must accept `isDirectory` parameter and validate against directory state whitelist. V2 context required (V1 clients cannot get directory leases). |
| LEASE-03 | Server breaks directory leases when directory contents change (file create/delete/rename) | `DirChangeNotifier` interface in `pkg/metadata/lock/directory.go`. MetadataService calls `OnDirChange(parentHandle, changeType, originClient)` after successful create/delete/rename. LockManager finds directory leases for parentHandle, dispatches break via `BreakCallbacks.OnOpLockBreak`. Self-notification excluded. Recently-broken cache prevents storms. |
| LEASE-04 | Lease management logic lives in metadata service layer, not SMB internal package | All lease CRUD moves from `internal/adapter/smb/v2/handlers/` to `pkg/metadata/lock/`. New `internal/adapter/smb/lease/` is thin wrapper with only session-to-lease mapping and notification dispatch. `cross_protocol.go` NLM checks move to `pkg/metadata/lock/`. |
| ARCH-01 | Business logic (leases, durable handles, state) lives in metadata service layer following NFS v3/v4 pattern | Same as LEASE-04. LockManager in `pkg/metadata/lock/` owns all lease business logic. Protocol adapters (NFS StateManager, SMB LeaseManager) are thin wrappers. |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `pkg/metadata/lock` | existing | Shared LockManager for all lock/lease operations | Already the unified lock management layer; natural home for lease CRUD |
| `pkg/metadata/lock/LockStore` | existing | Lease persistence interface | Already supports PutLock/GetLock/DeleteLock/ListLocks with lease fields |
| `internal/adapter/smb/smbenc` | existing | SMB binary codec (Reader/Writer) | ARCH-02 mandates all wire format encoding through smbenc |
| `pkg/metadata/lock/OpLockBreakScanner` | existing | Lease break timeout enforcement | 35s timeout scanner already implemented, reusable for directory leases |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/adapter/nfs/v4/state` | existing | NFS4 StateManager (reference pattern) | Pattern reference for thin protocol wrapper design |
| `pkg/metadata/lock/BreakCallbacks` | existing | Cross-protocol break dispatch | Fan-out directory changes to both NFS4 StateManager and SMB LeaseManager |
| `sync.Map` / `sync.RWMutex` | stdlib | Thread-safe state | LockManager uses RWMutex, adapter wrappers use appropriate concurrency primitives |

### No New Dependencies
This phase uses only existing packages. No external libraries needed.

## Architecture Patterns

### Recommended File Structure
```
pkg/metadata/lock/
├── manager.go          # EXTEND: Add RequestLease, AcknowledgeBreak, ReleaseLease, ReclaimLease to LockManager interface + Manager impl
├── leases.go           # NEW: Lease CRUD implementation (RequestLease, AcknowledgeBreak, ReleaseLease, ReclaimLease)
├── directory.go        # NEW: DirChangeNotifier interface, DirChangeType enum, recently-broken cache, directory break dispatch
├── reclaim.go          # NEW: Unified reclaim logic (ReclaimLease for both SMB and NFS)
├── oplock.go           # EXTEND: Add ParentLeaseKey to OpLock, add IsDirectory flag
├── store.go            # EXTEND: Add ParentLeaseKey, IsDirectory to PersistedLock
├── cross_protocol.go   # EXTEND: Move checkNLMLocksForLeaseConflict here
└── oplock_break.go     # EXISTING: OpLockBreakScanner (reused as-is for directory leases)

internal/adapter/smb/lease/
├── manager.go          # NEW: Thin SMB LeaseManager wrapper (sessionMap + notification dispatch)
└── notifier.go         # NEW: LeaseBreakNotifier implementation (SendLeaseBreak)

pkg/metadata/
├── service.go          # EXTEND: Add DirChangeNotifier field, wire to LockManager
├── file_create.go      # EXTEND: Call DirChangeNotifier.OnDirChange after successful create
├── file_remove.go      # EXTEND: Call DirChangeNotifier.OnDirChange after successful remove
├── file_modify.go      # EXTEND: Call DirChangeNotifier.OnDirChange after successful rename/move
├── directory.go        # EXTEND: Call DirChangeNotifier.OnDirChange after successful mkdir/rmdir
└── lock_exports.go     # EXTEND: Re-export new types if needed

internal/adapter/smb/v2/handlers/
├── oplock.go           # DELETE (OplockManager)
├── lease.go            # DELETE (OplockManager lease methods) -- wire encoding stays or moves to smbenc
├── lease_context.go    # REFACTOR: Update ProcessLeaseCreateContext to call new SMB LeaseManager
├── cross_protocol.go   # DELETE (moves to pkg/metadata/lock/)
├── create.go           # REFACTOR: Use new SMB LeaseManager instead of OplockManager
├── close.go            # REFACTOR: Use new SMB LeaseManager instead of OplockManager
├── stub_handlers.go    # REFACTOR: OplockBreak handler routes to LockManager or SMB LeaseManager
└── handler.go          # REFACTOR: Replace OplockManager field with LeaseManager reference

internal/adapter/nfs/v4/handlers/
├── create.go           # REFACTOR: Remove direct StateManager.NotifyDirChange calls
├── remove.go           # REFACTOR: Remove direct StateManager.NotifyDirChange calls
├── open.go             # REFACTOR: Remove direct StateManager.NotifyDirChange calls
├── rename.go           # REFACTOR: Remove direct StateManager.NotifyDirChange calls
├── link.go             # REFACTOR: Remove direct StateManager.NotifyDirChange calls
└── setattr.go          # REFACTOR: Remove direct StateManager.NotifyDirChange calls (attr changes don't trigger SMB breaks, but NFS still needs notification via unified path)
```

### Pattern 1: Lease Methods on LockManager
**What:** Add lease CRUD methods directly to the existing LockManager interface (no sub-interfaces).
**When to use:** All lease operations (request, acknowledge, release, reclaim).
**Key insight:** LockManager already has break dispatch (`breakOpLocks`), BreakCallbacks registration, grace period management, and per-share isolation. Lease methods plug in naturally.

```go
// Added to LockManager interface in manager.go
type LockManager interface {
    // ... existing methods ...

    // Lease Operations
    RequestLease(ctx context.Context, fileHandle FileHandle, leaseKey [16]byte,
        ownerID string, clientID string, shareName string,
        requestedState uint32, isDirectory bool) (grantedState uint32, epoch uint16, err error)

    AcknowledgeLeaseBreak(ctx context.Context, leaseKey [16]byte,
        acknowledgedState uint32) error

    ReleaseLease(ctx context.Context, leaseKey [16]byte) error

    ReclaimLease(ctx context.Context, leaseKey [16]byte,
        requestedState uint32, isDirectory bool) (*UnifiedLock, error)

    GetLeaseState(ctx context.Context, leaseKey [16]byte) (state uint32, epoch uint16, found bool)
}
```

### Pattern 2: DirChangeNotifier Interface
**What:** Callback interface for directory content changes, implemented by LockManager.
**When to use:** MetadataService calls after successful create/delete/rename operations.
**Key insight:** MetadataService does NOT import lease types. It only knows about `DirChangeNotifier` and `DirChangeType`. LockManager implements the interface and handles protocol-specific dispatch.

```go
// In pkg/metadata/lock/directory.go

// DirChangeType identifies the type of directory change.
type DirChangeType int

const (
    DirChangeAddEntry    DirChangeType = iota // File/dir created in directory
    DirChangeRemoveEntry                       // File/dir removed from directory
    DirChangeRenameEntry                       // File/dir renamed within/across directory
)

// DirChangeNotifier is called when directory contents change.
// Implementations handle lease break dispatch (SMB) and delegation recall (NFS).
type DirChangeNotifier interface {
    OnDirChange(parentHandle FileHandle, changeType DirChangeType, originClientID string)
}
```

### Pattern 3: Thin SMB LeaseManager Wrapper
**What:** Protocol-specific wrapper that bridges LockManager with SMB transport.
**When to use:** SMB handlers call this instead of directly calling LockManager for lease operations that need SMB-specific context (session tracking, notification dispatch).
**Key insight:** Parallel to NFS4 StateManager. Holds only sessionID-to-leaseKey mapping and LeaseBreakNotifier. All business logic delegates to LockManager.

```go
// In internal/adapter/smb/lease/manager.go

type LeaseManager struct {
    lockManager lock.LockManager
    lockStore   lock.LockStore
    notifier    LeaseBreakNotifier  // For sending LEASE_BREAK_NOTIFICATION
    sessionMap  map[string]uint64   // leaseKeyHex -> sessionID (SMB-specific)
    mu          sync.RWMutex
}

func (m *LeaseManager) RequestLease(ctx context.Context, ...) (uint32, uint16, error) {
    // Track session mapping
    // Delegate to m.lockManager.RequestLease(...)
}
```

### Pattern 4: Recently-Broken Cache for Directory Leases
**What:** Time-based cache that prevents granting directory leases on recently-broken directories.
**When to use:** In LockManager.RequestLease when isDirectory=true.
**Key insight:** Mirrors NFS4 StateManager's `recentlyRecalled` pattern. Prevents grant-break storms on busy directories. 5-second default TTL.

```go
// In pkg/metadata/lock/directory.go

type recentlyBrokenCache struct {
    mu      sync.RWMutex
    entries map[string]time.Time // handleKey -> breakTime
    ttl     time.Duration        // Default 5s
}

func (c *recentlyBrokenCache) IsRecentlyBroken(handleKey string) bool {
    c.mu.RLock()
    defer c.mu.RUnlock()
    breakTime, ok := c.entries[handleKey]
    return ok && time.Since(breakTime) < c.ttl
}
```

### Pattern 5: Unified Notification Flow
**What:** All directory change notifications flow through a single path: MetadataService -> LockManager -> BreakCallbacks.
**When to use:** Any file/directory mutation that affects directory contents.
**Key insight:** NFS4 handlers STOP calling StateManager.NotifyDirChange() directly. Instead, MetadataService's create/remove/rename operations call DirChangeNotifier after success. LockManager handles directory lease breaks (SMB) and fans out to BreakCallbacks (which reaches NFS4 StateManager for delegation recalls). This creates a single authoritative notification path for both protocols.

```
NFS CREATE handler
    -> MetadataService.CreateFile()
        -> [success]
        -> DirChangeNotifier.OnDirChange(parentHandle, AddEntry, originClientID)
            -> LockManager: break SMB directory leases on parentHandle (exclude originClient)
            -> LockManager: dispatch via BreakCallbacks.OnOpLockBreak (reaches NFS4 StateManager)
```

### Anti-Patterns to Avoid
- **Duplicating lease state in adapter wrapper:** All state lives in LockManager. SMB LeaseManager holds only sessionMap (mapping, not state).
- **Adding new BreakCallbacks methods:** Reuse `OnOpLockBreak` for directory lease breaks. Receivers check if the lock is a directory lease.
- **Batching directory breaks for SMB:** Unlike NFS4, SMB directory breaks dispatch immediately (per MS-SMB2). Only NFS4 uses batched notifications.
- **Calling StateManager.NotifyDirChange from NFS handlers:** All notifications go through MetadataService -> LockManager -> BreakCallbacks.
- **Separate V1/V2 code paths:** Single code path handles both. V1 has no ParentLeaseKey/epoch. V2 adds them. Store both in same structures.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Lease break timeout | Custom timeout tracking | `OpLockBreakScanner` (existing) | Already handles 35s timeout with configurable interval, persisted lock scanning |
| Lease key conflict detection | Manual iteration | `OpLocksConflict()` (existing) | Already handles same-key no-conflict, Write exclusivity, breaking state |
| V1/V2 context parsing | New parser | `DecodeLeaseCreateContext` (existing) | Already detects 52-byte V2 vs 32-byte V1, parses ParentLeaseKey |
| Grace period management | Separate grace period | `GracePeriodManager` (existing) | Embedded in LockManager, handles reclaim windows |
| Cross-protocol translation | New translation layer | `TranslateToNLMHolder`, `TranslateSMBConflictReason` (existing) | Already in `pkg/metadata/lock/cross_protocol.go` |
| Wire format encoding | Raw byte manipulation | `smbenc.NewWriter`/`smbenc.NewReader` (existing) | ARCH-02 mandates all encoding through smbenc |

**Key insight:** Most of the building blocks exist. This phase is primarily a _reorganization_ with targeted additions (directory lease breaks, DirChangeNotifier, ParentLeaseKey storage). Very little genuinely new protocol logic needed.

## Common Pitfalls

### Pitfall 1: Circular Import Between LockManager and MetadataStore
**What goes wrong:** LockManager needs to check if a directory handle exists (for reclaim validation), but MetadataStore is in `pkg/metadata` which imports `pkg/metadata/lock`.
**Why it happens:** Reclaim validation requires checking if the directory still exists in the metadata store.
**How to avoid:** Pass a `HandleExistsFunc func(FileHandle) bool` to the reclaim method rather than importing MetadataStore directly. Or define a minimal interface in the lock package (`type HandleChecker interface { HandleExists(FileHandle) bool }`).
**Warning signs:** Import cycle errors during compilation.

### Pitfall 2: Lock Ordering Violation Between LockManager.mu and SMB LeaseManager.mu
**What goes wrong:** Deadlock when LockManager dispatches a break callback that tries to acquire SMB LeaseManager's lock (to look up sessionID), while another goroutine holds LeaseManager.mu and calls into LockManager.
**Why it happens:** Break dispatch is synchronous (per `BreakCallbacks` contract), and callback implementations may need their own locks.
**How to avoid:** BreakCallbacks implementations should be lightweight. SMB LeaseManager should use a separate lock for the sessionMap lookup that doesn't nest with LockManager.mu. Alternatively, dispatch break notifications asynchronously from LockManager (goroutine per break).
**Warning signs:** Intermittent hangs during cross-protocol operations.

### Pitfall 3: Forgetting Epoch Increment on Lease State Changes
**What goes wrong:** SMB3 clients use epoch to detect stale notifications. Missing epoch increments cause clients to ignore valid break notifications or accept stale ones.
**Why it happens:** Epoch must be incremented on EVERY state change (grant, break initiate, break acknowledge, upgrade).
**How to avoid:** Centralize epoch management in a single method (e.g., `advanceEpoch`) called from all lease state transitions. Per MS-SMB2, server sets `NewEpoch = Lease.Epoch + 1` during break, then sets `Lease.Epoch = NewEpoch`.
**Warning signs:** smbtorture lease-v2 epoch tests fail.

### Pitfall 4: Breaking Directory Leases on Attribute Changes
**What goes wrong:** Directory lease broken on SetAttr/chmod operations, causing excessive breaks.
**Why it happens:** Misunderstanding which operations trigger directory breaks.
**How to avoid:** Per CONTEXT.md and MS-SMB2 3.3.4.7: ONLY create, delete, and rename trigger directory lease breaks. SetInfo/attribute changes do NOT break directory leases. The DirChangeType enum has exactly three values: AddEntry, RemoveEntry, RenameEntry. SetAttr notifications still flow through for NFS4 (via BreakCallbacks) but do NOT trigger SMB directory lease breaks.
**Warning signs:** Excessive directory lease break notifications in logs.

### Pitfall 5: V2 Lease Break Notification Missing ParentLeaseKey
**What goes wrong:** V2 clients receive break notifications without ParentLeaseKey, breaking their cache tree management.
**Why it happens:** Using V1 break notification format for V2 leases.
**How to avoid:** Per CONTEXT.md: "if the lease was created with V2 context, send V2 LEASE_BREAK_Notification (includes ParentLeaseKey + epoch)". Track lease version (V1 vs V2) in PersistedLock/OpLock. Send V2 break format when lease.Version == 2.
**Warning signs:** Windows client cache invalidation issues after lease breaks.

### Pitfall 6: Not Excluding Self from Directory Lease Breaks
**What goes wrong:** Client creates a file, then immediately gets its own directory lease broken, causing unnecessary cache invalidation.
**Why it happens:** DirChangeNotifier not passing or checking originClientID.
**How to avoid:** Always pass originClientID in DirChangeNotifier.OnDirChange(). LockManager skips breaking leases whose owner matches originClientID. Per MS-SMB2: "a client's own operations don't break its own leases."
**Warning signs:** Clients getting their own leases broken immediately after operations.

### Pitfall 7: Race Between Lease Grant and Directory Mutation
**What goes wrong:** A directory lease is granted between the mutation check and the DirChangeNotifier callback, missing the break.
**Why it happens:** Non-atomic sequence: mutation succeeds, then notification fires, but lease was granted in between.
**How to avoid:** The recently-broken cache handles this by preventing grants on recently-mutated directories. Additionally, LockManager should check for pending mutations before granting directory leases.
**Warning signs:** Stale directory listings on SMB clients.

## Code Examples

### Example 1: LockManager.RequestLease Implementation Sketch

```go
// In pkg/metadata/lock/leases.go
func (lm *Manager) RequestLease(
    ctx context.Context,
    fileHandle FileHandle,
    leaseKey [16]byte,
    ownerID string,
    clientID string,
    shareName string,
    requestedState uint32,
    isDirectory bool,
) (grantedState uint32, epoch uint16, err error) {
    lm.mu.Lock()
    defer lm.mu.Unlock()

    // Validate requested state
    if isDirectory {
        if !IsValidDirectoryLeaseState(requestedState) {
            return LeaseStateNone, 0, nil
        }
        // Check recently-broken cache
        handleKey := string(fileHandle)
        if lm.recentlyBroken.IsRecentlyBroken(handleKey) {
            return LeaseStateNone, 0, nil
        }
    } else {
        if !IsValidFileLeaseState(requestedState) {
            return LeaseStateNone, 0, nil
        }
    }

    // Cross-protocol NLM check (moved from OplockManager)
    if lm.lockStore != nil {
        if conflicts := lm.checkNLMConflicts(ctx, fileHandle, requestedState); len(conflicts) > 0 {
            return LeaseStateNone, 0, nil
        }
    }

    // Check existing lease with same key (upgrade/maintain)
    existing := lm.findLeaseByKey(ctx, fileHandle, leaseKey)
    if existing != nil {
        return lm.upgradeLeaseState(ctx, existing, requestedState)
    }

    // Check conflicting leases (different key)
    if conflict := lm.checkLeaseConflict(ctx, fileHandle, requestedState, leaseKey); conflict != nil {
        breakToState := lm.calculateBreakToState(requestedState)
        lm.initiateLeaseBreak(string(fileHandle), conflict, breakToState)
        return LeaseStateNone, 0, nil
    }

    // Grant new lease
    lock := NewUnifiedLock(
        LockOwner{OwnerID: ownerID, ClientID: clientID, ShareName: shareName},
        fileHandle, 0, 0, LockTypeShared,
    )
    lock.Lease = &OpLock{
        LeaseKey:   leaseKey,
        LeaseState: requestedState,
        Epoch:      1,
        IsDirectory: isDirectory,
    }

    // Persist
    pl := ToPersistedLock(lock, 0)
    if err := lm.lockStore.PutLock(ctx, pl); err != nil {
        return LeaseStateNone, 0, err
    }

    return requestedState, 1, nil
}
```

### Example 2: DirChangeNotifier in MetadataService

```go
// In pkg/metadata/service.go
type MetadataService struct {
    // ... existing fields ...
    dirChangeNotifier lock.DirChangeNotifier // Set via SetDirChangeNotifier()
}

func (s *MetadataService) SetDirChangeNotifier(n lock.DirChangeNotifier) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.dirChangeNotifier = n
}

// In pkg/metadata/file_create.go - after successful createEntry
func (s *MetadataService) CreateFile(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error) {
    file, err := s.createEntry(ctx, parentHandle, name, attr, FileTypeRegular, "", 0, 0)
    if err != nil {
        return nil, err
    }
    // Notify directory change (fire-and-forget)
    if s.dirChangeNotifier != nil {
        originClient := ""
        if ctx.Identity != nil {
            originClient = ctx.Identity.ClientID()
        }
        s.dirChangeNotifier.OnDirChange(
            lock.FileHandle(parentHandle),
            lock.DirChangeAddEntry,
            originClient,
        )
    }
    return file, nil
}
```

### Example 3: LockManager as DirChangeNotifier

```go
// In pkg/metadata/lock/directory.go
func (lm *Manager) OnDirChange(parentHandle FileHandle, changeType DirChangeType, originClientID string) {
    handleKey := string(parentHandle)

    lm.mu.RLock()
    locks := lm.unifiedLocks[handleKey]
    var dirLeases []*UnifiedLock
    for _, l := range locks {
        if l.Lease != nil && l.Lease.IsDirectory {
            // Skip originating client's own leases
            if originClientID != "" && l.Owner.ClientID == originClientID {
                continue
            }
            dirLeases = append(dirLeases, l)
        }
    }
    lm.mu.RUnlock()

    // Break each directory lease
    for _, lease := range dirLeases {
        lm.dispatchOpLockBreak(handleKey, lease, LeaseStateNone)
    }

    // Mark as recently broken to prevent grant storms
    if len(dirLeases) > 0 {
        lm.recentlyBroken.Mark(handleKey)
    }
}
```

### Example 4: V2 Response Context with ParentLeaseKey

```go
// Updated EncodeLeaseResponseContext with ParentLeaseKey support
func EncodeLeaseV2ResponseContext(leaseKey [16]byte, leaseState uint32,
    parentLeaseKey [16]byte, hasParent bool, epoch uint16) []byte {
    w := smbenc.NewWriter(LeaseV2ContextSize)
    w.WriteBytes(leaseKey[:])     // LeaseKey (16 bytes)
    w.WriteUint32(leaseState)     // LeaseState
    var flags uint32
    if hasParent {
        flags |= 0x04 // SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET
    }
    w.WriteUint32(flags)          // Flags
    w.WriteUint64(0)              // LeaseDuration
    w.WriteBytes(parentLeaseKey[:]) // ParentLeaseKey (16 bytes)
    w.WriteUint16(epoch)          // Epoch
    w.WriteUint16(0)              // Reserved
    return w.Bytes()
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| OplockManager in handlers/ owns lease state | LockManager in pkg/metadata/lock/ owns ALL lease state | Phase 37 (this phase) | Single source of truth, cross-protocol coordination |
| NFS4 handlers call StateManager.NotifyDirChange directly | MetadataService -> DirChangeNotifier -> LockManager -> BreakCallbacks | Phase 37 (this phase) | Unified notification path for both protocols |
| Separate OplockManager persistence | LockStore unified persistence | Phase 37 (this phase) | One persistence path for all lease types |
| V1-only lease response encoding | V2 response with ParentLeaseKey + epoch | Phase 37 (this phase) | SMB3 client cache tree management |

## Open Questions

1. **originClientID format for cross-protocol self-exclusion**
   - What we know: SMB uses "smb:lease:{leaseKeyHex}" as ownerID, NFS uses "nfs4:clientid:stateid". The DirChangeNotifier passes originClientID as a string.
   - What's unclear: Whether to match on ClientID field or OwnerID field for self-exclusion. ClientID is connection-level, OwnerID is operation-level.
   - Recommendation: Use `LockOwner.ClientID` for self-exclusion matching, since that identifies the connection/session that originated the operation. This works for both NFS (client address) and SMB (connection tracker ID).

2. **NFS4 SETATTR notifications through unified path**
   - What we know: Per CONTEXT.md, SetInfo/attribute changes do NOT trigger SMB directory lease breaks. But NFS4 currently sends NOTIFY4_CHANGE_DIR_ATTRS via NotifyDirChange for setattr.
   - What's unclear: Should SETATTR still flow through the unified notification path, or should it remain a direct NFS4 StateManager call?
   - Recommendation: Route through unified path but add a DirChangeType value (e.g., `DirChangeAttributes`) that LockManager ignores for SMB breaks but forwards to BreakCallbacks for NFS4 StateManager to handle. This maintains the "single notification path" decision while preserving NFS4 attr notification behavior.

3. **Lease V2 break notification wire format**
   - What we know: Existing `LeaseBreakNotification.Encode()` produces 44-byte V1 format with NewEpoch field. The V2 break notification format per MS-SMB2 2.2.23.2 is the SAME structure -- the NewEpoch field just becomes meaningful for 3.x dialects.
   - What's unclear: Whether additional V2-specific break notification fields (AccessMaskHint, ShareMaskHint) need to be populated.
   - Recommendation: Per MS-SMB2, BreakReason/AccessMaskHint/ShareMaskHint are reserved and MUST be 0. The existing 44-byte format is correct for V2. Just ensure NewEpoch is set to `Lease.Epoch + 1` for 3.x dialect leases (already in existing LeaseBreakNotification struct).

## Validation Architecture

> Skipped -- workflow.nyquist_validation not enabled in config.json

## Sources

### Primary (HIGH confidence)
- [MS-SMB2 3.3.5.9.11: Handling SMB2_CREATE_REQUEST_LEASE_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fc4f8879-f295-4995-b71e-21f309d8d7c8) - Complete server-side algorithm for V2 lease handling, ParentLeaseKey storage, epoch management
- [MS-SMB2 2.2.23.2: Lease Break Notification](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/9abe6f73-f32f-4a23-998d-ee9da2b90e2e) - Wire format for lease break notifications, NewEpoch for 3.x dialects
- [MS-SMB2 3.3.4.7: Object Store Indicates a Lease Break](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/c367fad4-c00f-4778-913d-c0560ead1360) - Break dispatch algorithm, R-only lease breaks without ACK, epoch increment on break
- [MS-SMB2 3.3.5.22.2: Processing a Lease Acknowledgment](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/0ccd85cd-5e1d-4dc2-9698-71b4c87c0cec) - Lease break ack processing, state validation, error codes
- [MS-SMB2 Per Lease](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/212eb853-7e50-4608-877e-22d42e0664f3) - Lease state model: LeaseKey, LeaseState, Epoch, Breaking, BreakToLeaseState, ParentLeaseKey, Version
- [MS-SMB2 SMB2_CREATE_RESPONSE_LEASE_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/1bccd8d3-a13e-4288-9c7b-26e498052a25) - V2 response format with ParentLeaseKey and SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET

### Existing Codebase (HIGH confidence)
- `pkg/metadata/lock/manager.go` - LockManager interface and Manager implementation (1168 lines)
- `pkg/metadata/lock/oplock.go` - OpLock struct, ValidDirectoryLeaseStates, LeaseStateToString, OpLocksConflict
- `pkg/metadata/lock/oplock_break.go` - OpLockBreakScanner with 35s timeout, BreakCallbacks interface
- `pkg/metadata/lock/types.go` - UnifiedLock, LockOwner, ConflictsWith, FileHandle
- `pkg/metadata/lock/store.go` - PersistedLock, LockStore interface, ToPersistedLock/FromPersistedLock
- `pkg/metadata/lock/cross_protocol.go` - TranslateToNLMHolder, TranslateSMBConflictReason
- `internal/adapter/smb/v2/handlers/oplock.go` - OplockManager (source for migration)
- `internal/adapter/smb/v2/handlers/lease.go` - OplockManager lease methods (source for migration)
- `internal/adapter/smb/v2/handlers/lease_context.go` - V2 context parsing, ProcessLeaseCreateContext
- `internal/adapter/smb/v2/handlers/cross_protocol.go` - checkNLMLocksForLeaseConflict (source for migration)
- `internal/adapter/nfs/v4/state/dir_delegation.go` - NFS4 directory delegation pattern (reference architecture)
- `internal/adapter/nfs/v4/state/manager.go` - NFS4 StateManager pattern (reference architecture)
- `pkg/metadata/service.go` - MetadataService structure, per-share LockManager
- `pkg/metadata/file_create.go` - createEntry method (DirChangeNotifier integration point)
- `pkg/metadata/file_remove.go` - RemoveFile method (DirChangeNotifier integration point)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries/packages already exist in the codebase; no new dependencies
- Architecture: HIGH - CONTEXT.md provides extremely detailed decisions; NFS4 StateManager pattern is proven
- Pitfalls: HIGH - Well-understood from existing OplockManager code and MS-SMB2 spec
- Wire format: HIGH - V2 parsing already implemented (52-byte detection), V1/V2 break notification format identical (NewEpoch field already in struct)

**Research date:** 2026-03-02
**Valid until:** 2026-04-01 (stable domain, no external dependencies)
