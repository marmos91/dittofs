# Phase 26: Generic Lock Interface & Protocol Leak Purge - Research

**Researched:** 2026-02-25
**Domain:** Go refactoring -- type renaming, interface extraction, DB schema migration, code relocation
**Confidence:** HIGH

## Summary

Phase 26 is a pure refactoring phase with no new capabilities. It has two pillars: (1) rename lock types to protocol-agnostic names (`EnhancedLock` -> `UnifiedLock`, `LeaseInfo` -> `OpLock`, `ShareReservation` -> `AccessMode`) with composed struct design and centralized `ConflictsWith()`, and (2) purge NFS/SMB-specific code from generic layers (`pkg/metadata/`, `pkg/controlplane/`) by moving protocol types to adapter packages and extracting per-adapter config into a `share_adapter_configs` table.

The codebase is well-structured for this change. The lock package (`pkg/metadata/lock/`) is 8,803 lines across 22 files with clear separation. The `GENERIC_LOCK_INTERFACE_PLAN.md` and `CORE_REFACTORING_PLAN.md` provide detailed blueprints. Key risk is the breadth of the rename -- `EnhancedLock` appears in 460 occurrences across 71 files, `LeaseInfo`/`LeaseState` in 583 occurrences across 57 files. The backward-compatibility re-export layer in `pkg/metadata/lock_exports.go` needs to be updated in sync.

**Primary recommendation:** Execute in dependency order: lock type renames first (compile errors guide completion), then method moves (NLM from MetadataService, SMB lease methods), then share model cleanup (DB migration), then runtime/API handler relocation. Each step should be independently compilable and testable.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- `EnhancedLock` renamed to `UnifiedLock` -- composed struct embedding `OpLock`, `AccessMode`, and `ByteRangeLock` as separate sub-types (not flat)
- `LeaseInfo` renamed to `OpLock` -- truly opaque, no protocol field. Adapters register break callbacks; LockManager never knows the protocol
- `ShareReservation` renamed to `AccessMode` -- bitmask implementation with `ACCESS_READ`, `ACCESS_WRITE`, `DENY_READ`, `DENY_WRITE`, `DENY_DELETE`
- OpLock levels: union of both protocols -- `None`, `Read`, `Write`, `ReadWrite`, `Handle`, `ReadHandle`, `ReadWriteHandle`
- Conflict detection via `lock.ConflictsWith(other)` method on UnifiedLock (not a standalone detector)
- Break notifications via typed callbacks: `OnOpLockBreak()`, `OnByteRangeRevoke()`, `OnAccessConflict()` -- compile-safe, adapter implements only what it uses
- All types stay in `pkg/metadata/lock/` (not elevated to top-level pkg/lock/)
- Single `LockManager` interface (not split into sub-interfaces) -- lean, both adapters use all lock types
- GracePeriodManager included as part of LockManager interface (grace periods are tied to lock recovery)
- NFSv4 state registers grace period needs through LockManager
- Adapters receive LockManager through Runtime (keep `SetRuntime()` for now, Phase 29 will decompose)
- Explore separate LockManager injection path as alternative to MetadataService getter; fall back to getter if simpler
- NFS lock handlers (LOCK/LOCKT/LOCKU) call LockManager directly -- no adapter-local coordinator
- SMB adapter uses generic UnifiedLock interface only -- no SMB-specific lease wrapper
- All SMB lease methods (CheckAndBreakLeases*, ReclaimLeaseSMB, OplockChecker) removed from MetadataService
- NLM methods moved from MetadataService to NFS adapter with simplified signatures (not just moved as-is)
- Generic identity layer: DittoFS users + UID/GID + name resolution stays in metadata
- Protocol-specific transforms: SquashMode/Netgroup to NFS adapter, SID mapping to SMB adapter
- `pkg/identity/` dissolved entirely -- generic parts merge into metadata, NFS parts to NFS adapter
- Mount tracking: unified "mounts" concept across both protocols
- NFS/SMB-specific fields removed from Share model, moved to separate `share_adapter_configs` table
- Table: `share_id`, `adapter_type` (string), `config` (typed JSON)
- Each adapter registers typed config schema -- validated at API level
- API pattern: `/api/adapters/nfs/settings` -- global NFS defaults; `/api/shares/{id}/adapters/nfs/settings` -- per-share NFS overrides
- Layered config: adapter defaults apply unless share has explicit override
- Share creation uses adapter defaults automatically -- separate `dfsctl share adapter-config set` for overrides
- GORM auto-migration for schema changes
- Old protocol-specific columns dropped from Share table (clean break, no deprecation)
- Clean API break -- v3.5 dfsctl only works with v3.5+ servers, no backward-compat code
- K8s operator updated alongside each phase (not deferred)
- Auto-migrate DB on startup by default, `--no-auto-migrate` flag for manual control
- config.yaml unchanged -- all adapter/share settings managed through API
- Lock test suites rewritten against new LockManager interface (not just renamed)
- Full E2E test suite run as validation after refactoring

### Claude's Discretion
- Config validation: API-level vs both API+startup (defense in depth assessment)
- Cross-protocol conflict test coverage: Phase 26 vs defer to Phase 29 (risk-based decision)
- Exact LockManager injection path: separate injection vs MetadataService getter (based on code analysis)

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| REF-01 | Generic Lock Interface -- unify lock types, centralize conflict detection, rename to protocol-agnostic names | Lock package analysis (types.go, lease_types.go, manager.go, store.go, lease_break.go) confirms all types to rename, conflict detection to centralize, and persistence layer fields to update. GENERIC_LOCK_INTERFACE_PLAN.md provides complete mapping. |
| REF-02 | Protocol Leak Purge -- remove protocol-specific code from generic layers | MetadataService analysis (994 lines, NLM methods lines 549-810, SMB lease methods lines 817-994), Share model analysis (NFS: Squash/Kerberos/Netgroup/AnonymousUID fields, SMB: GuestEnabled/GuestUID/GuestGID), Store interface analysis (8 netgroup + 4 identity mapping methods), Runtime analysis (mounts, dnsCache, nfsClientProvider, shareChangeCallbacks), API handler analysis (clients.go, grace.go import NFS v4 state). |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go standard library | 1.22+ | Type definitions, interfaces, sync primitives | All changes are pure Go refactoring |
| GORM | v2.x | Schema migration for share_adapter_configs table | Already used for all control plane persistence |
| `encoding/json` | stdlib | Typed JSON config blobs in share_adapter_configs | Standard Go JSON for config serialization |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/google/uuid` | v1.x | UUID generation for new lock IDs | Already imported in lock/types.go |
| `github.com/go-chi/chi/v5` | v5.x | API route registration for new adapter config endpoints | Already used for all API routing |

### Alternatives Considered
None -- this is a refactoring phase using existing libraries.

## Architecture Patterns

### Recommended File Organization for Lock Renames

The GENERIC_LOCK_INTERFACE_PLAN.md specifies the exact file mapping:

```
pkg/metadata/lock/
├── types.go          # UnifiedLock (was EnhancedLock), AccessMode (was ShareReservation), LockOwner
├── oplock.go         # OpLock (was LeaseInfo in lease_types.go), OpLockState bitmask, validation
├── oplock_break.go   # OpLockBreakScanner (was LeaseBreakScanner in lease_break.go)
├── manager.go        # Manager with unifiedLocks map (was enhancedLocks), AddUnifiedLock etc.
├── store.go          # PersistedLock updated fields, LockQuery.IsOpLock, ReclaimOpLock
├── grace.go          # GracePeriodManager (UNCHANGED - already generic)
├── errors.go         # Updated param types
├── cross_protocol.go # Translation helpers (update type references)
├── deadlock.go       # UNCHANGED
├── connection.go     # UNCHANGED
├── config.go         # UNCHANGED
├── metrics.go        # UNCHANGED
└── client_store.go   # UNCHANGED
```

### Pattern 1: Composed UnifiedLock (User Decision)

**What:** UnifiedLock embeds sub-types instead of flat fields
**When to use:** All lock creation and conflict detection

```go
// pkg/metadata/lock/types.go
type UnifiedLock struct {
    ID         string
    Owner      LockOwner
    FileHandle FileHandle
    Offset     uint64
    Length     uint64
    Type       LockType
    AccessMode AccessMode     // Bitmask: ACCESS_READ, ACCESS_WRITE, DENY_READ, DENY_WRITE, DENY_DELETE
    OpLock     *OpLock        // nil for byte-range locks
    AcquiredAt time.Time
    Blocking   bool
    Reclaim    bool
}

// Method on UnifiedLock, not standalone function
func (ul *UnifiedLock) ConflictsWith(other *UnifiedLock) bool {
    // Same owner = no conflict
    if ul.Owner.OwnerID == other.Owner.OwnerID {
        return false
    }
    // Delegate to sub-type conflict methods
    // OpLock vs OpLock, OpLock vs byte-range, byte-range vs byte-range, AccessMode
    ...
}
```

### Pattern 2: AccessMode as Bitmask (User Decision)

**What:** AccessMode uses bitmask flags instead of enum
**When to use:** Share deny semantics for both SMB and NFSv4

```go
type AccessMode uint32

const (
    AccessRead   AccessMode = 0x01
    AccessWrite  AccessMode = 0x02
    DenyRead     AccessMode = 0x04
    DenyWrite    AccessMode = 0x08
    DenyDelete   AccessMode = 0x10  // Extended for SMB
)
```

**Note:** This is broader than the old `ShareReservation` enum (DenyNone/DenyRead/DenyWrite/DenyAll) -- it includes `ACCESS_READ`, `ACCESS_WRITE`, and `DENY_DELETE`. SMB `FILE_SHARE_DELETE` maps to `DenyDelete`.

### Pattern 3: Typed Break Callbacks (User Decision)

**What:** Adapters register typed callbacks for break notifications
**When to use:** Cross-protocol oplock/lock break coordination

```go
type BreakCallbacks interface {
    OnOpLockBreak(groupKey string, targetState OpLockState)
    OnByteRangeRevoke(handle FileHandle, offset, length uint64)
    OnAccessConflict(handle FileHandle, conflictingMode AccessMode)
}
```

Adapters implement only the methods they use. NFS adapter implements `OnOpLockBreak` (for delegation recall). SMB adapter implements all three.

### Pattern 4: share_adapter_configs Table

**What:** Separate GORM table for per-adapter per-share configuration
**When to use:** Replacing protocol-specific columns on Share model

```go
// pkg/controlplane/models/share_adapter_config.go
type ShareAdapterConfig struct {
    ID          string `gorm:"primaryKey;size:36"`
    ShareID     string `gorm:"not null;size:36;uniqueIndex:idx_share_adapter"`
    AdapterType string `gorm:"not null;size:50;uniqueIndex:idx_share_adapter"`
    Config      string `gorm:"type:text"` // JSON blob
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

NFS adapter defines typed config:
```go
// In NFS adapter package
type NFSExportOptions struct {
    Squash           string   `json:"squash"`
    AnonymousUID     *uint32  `json:"anonymous_uid,omitempty"`
    AnonymousGID     *uint32  `json:"anonymous_gid,omitempty"`
    AllowAuthSys     bool     `json:"allow_auth_sys"`
    RequireKerberos  bool     `json:"require_kerberos"`
    MinKerberosLevel string   `json:"min_kerberos_level"`
    NetgroupID       *string  `json:"netgroup_id,omitempty"`
    DisableReaddirplus bool   `json:"disable_readdirplus"`
}
```

### Pattern 5: LockManager Interface (User Decision)

**What:** Single unified interface for all lock operations, including grace period
**When to use:** Runtime injects into adapters

```go
type LockManager interface {
    // Unified lock CRUD
    AddUnifiedLock(handleKey string, lock *UnifiedLock) error
    RemoveUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) error
    ListUnifiedLocks(handleKey string) []*UnifiedLock
    RemoveFileUnifiedLocks(handleKey string)
    UpgradeLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error)

    // Legacy byte-range (backward compat)
    Lock(handleKey string, lock FileLock) error
    Unlock(handleKey string, sessionID, offset, length uint64) error
    // ... other legacy methods

    // Grace period
    EnterGracePeriod(expectedClients []string)
    ExitGracePeriod()
    IsOperationAllowed(op Operation) (bool, error)
    MarkReclaimed(clientID string)

    // Break callbacks
    RegisterBreakCallbacks(callbacks BreakCallbacks)
}
```

### Anti-Patterns to Avoid
- **Moving SMB types to SMB adapter verbatim:** The CONTEXT.md explicitly says to rename to generic types, NOT to move SMB types. `LeaseInfo` becomes `OpLock`, not "SMB's lease in pkg/adapter/smb/".
- **Creating new pkg/lock/ top-level package:** User explicitly decided types stay in `pkg/metadata/lock/`.
- **Splitting LockManager into sub-interfaces:** User explicitly decided single interface.
- **Wrapping generic types in protocol-specific wrappers:** SMB adapter uses `UnifiedLock` directly, NOT an `SMBLock` that wraps it.
- **Removing the legacy FileLock API:** The manager retains legacy methods alongside unified ones. Deprecation is future work.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| DB schema migration | Manual SQL ALTER TABLE | GORM AutoMigrate | Project already uses GORM auto-migration; adding new table + dropping old columns is standard GORM pattern |
| JSON config serialization | Custom binary format | `encoding/json` + Go struct tags | Typed JSON is the established pattern (Share.Config field already exists) |
| Bitmask operations | Custom bit manipulation library | Go bitwise operators on uint32 | AccessMode bitmask is simple enough for native Go operators |
| Cross-package type aliasing | Manual re-implementation | Go type aliases (`type X = lock.X`) | Already used in `lock_exports.go` for backward compatibility |

**Key insight:** This phase is pure refactoring. No new external dependencies needed. The complexity is in correctly updating all references across 70+ files, not in building new abstractions.

## Common Pitfalls

### Pitfall 1: Breaking Compile by Renaming Before Updating References
**What goes wrong:** Renaming `EnhancedLock` to `UnifiedLock` in `types.go` breaks 460 references across 71 files immediately.
**Why it happens:** Large-scale renames in Go require updating all consumers simultaneously.
**How to avoid:** Use `sed` or IDE refactoring for mechanical renames. Add type aliases temporarily: `type EnhancedLock = UnifiedLock` for intermediate compilability. Remove aliases in final cleanup.
**Warning signs:** Build fails after partial rename.

### Pitfall 2: Forgetting the Re-Export Layer
**What goes wrong:** `pkg/metadata/lock_exports.go` re-exports `EnhancedLock`, `ShareReservation`, etc. for backward compatibility. Renaming the source types without updating exports breaks external consumers.
**Why it happens:** The re-export file is easy to overlook.
**How to avoid:** Update `lock_exports.go` immediately after renaming source types. This file has 256 lines of re-exports that must track the new names.
**Warning signs:** Tests in `pkg/metadata/` package fail after lock package rename.

### Pitfall 3: PersistedLock JSON Field Names Must Stay Backward-Compatible
**What goes wrong:** Changing `json:"lease_key"` to `json:"oplock_group_key"` in `PersistedLock` breaks existing persisted lock data.
**Why it happens:** PersistedLock is serialized to disk/DB by LockStore implementations.
**How to avoid:** The CONTEXT decision is "clean break, no deprecation". Since this is v3.5, old persisted locks from v3.0 won't exist (locks are ephemeral). BUT: the JSON tags still need to match what `GENERIC_LOCK_INTERFACE_PLAN.md` specifies. If any LockStore implementation uses GORM (check postgres/locks.go), column names need migration.
**Warning signs:** Lock reclaim fails after server restart.

### Pitfall 4: GORM AutoMigrate Column Drops
**What goes wrong:** GORM's `AutoMigrate` adds new columns and creates new tables, but does NOT drop old columns. The old NFS/SMB fields on the Share table persist as ghost columns.
**Why it happens:** GORM is conservative about schema changes.
**How to avoid:** After AutoMigrate for the new `share_adapter_configs` table, run explicit `Migrator().DropColumn()` calls for each removed field. Do this in a dedicated migration function, not in AutoMigrate.
**Warning signs:** Old columns remain in production DB, code reads zero values from them.

### Pitfall 5: MetadataService NLM Method Move Breaks Import Cycles
**What goes wrong:** Moving NLM methods from `pkg/metadata/service.go` to NFS adapter creates import cycles because the NFS adapter already imports `pkg/metadata`.
**Why it happens:** Bidirectional dependency between adapter and metadata packages.
**How to avoid:** The NLM service in the NFS adapter should hold a reference to `*lock.Manager` (from `pkg/metadata/lock/`), NOT to `*metadata.MetadataService`. The NLM service does file existence checks via a minimal interface, not the full MetadataService.
**Warning signs:** `import cycle not allowed` compile error.

### Pitfall 6: Netgroup/IdentityMapping Store Method Removal Breaks Tests
**What goes wrong:** Removing 12 methods from the Store interface breaks all mock implementations and test code.
**Why it happens:** Tests often implement the full Store interface in mocks.
**How to avoid:** Define `NetgroupStore` and `IdentityMappingStore` as separate interfaces. The main `Store` interface no longer embeds them. NFS adapter type-asserts `store.(NetgroupStore)` at setup time. Update mocks to implement only needed interfaces.
**Warning signs:** 20+ test files fail to compile.

### Pitfall 7: AccessMode Bitmask vs Old ShareReservation Enum Mismatch
**What goes wrong:** The old `ShareReservation` was an enum (0=None, 1=DenyRead, 2=DenyWrite, 3=DenyAll). The new `AccessMode` is a bitmask with 5 flags. Direct cast `AccessMode(oldShareReservation)` produces wrong values.
**Why it happens:** Different encoding schemes.
**How to avoid:** Create explicit `migrateShareReservation(old ShareReservation) AccessMode` converter. Map: None=0, DenyRead=`DenyRead`, DenyWrite=`DenyWrite`, DenyAll=`DenyRead|DenyWrite`. Used in `FromPersistedLock()`.
**Warning signs:** Existing SMB share reservations silently misinterpreted.

### Pitfall 8: OplockChecker Global Removal Breaks NFS-SMB Cross-Protocol
**What goes wrong:** NFS v3 WRITE/REMOVE handlers call `CheckAndBreakLeasesForWrite()` on MetadataService, which uses the global `OplockChecker`. Removing this without replacement breaks cross-protocol lease breaks.
**Why it happens:** The current global variable pattern is the coordination mechanism.
**How to avoid:** Replace with `LockManager.CheckAndBreakOpLocksForWrite()` that uses the centralized break logic and registered callbacks. NFS handlers call LockManager directly instead of MetadataService.
**Warning signs:** NFS writes proceed without breaking SMB Write leases.

## Code Examples

### Example 1: UnifiedLock Type Definition

```go
// pkg/metadata/lock/types.go

type OpLockState uint8

const (
    OpLockRead   OpLockState = 0x01
    OpLockWrite  OpLockState = 0x02
    OpLockHandle OpLockState = 0x04
)

type OpLock struct {
    GroupKey     string      // Opaque caching unit ID
    State        OpLockState
    Breaking     bool
    BreakTarget  OpLockState
    BreakStarted time.Time
    StateVersion uint32
    Reclaim      bool
}

type AccessMode uint32

const (
    AccessRead  AccessMode = 0x01
    AccessWrite AccessMode = 0x02
    DenyRead    AccessMode = 0x04
    DenyWrite   AccessMode = 0x08
    DenyDelete  AccessMode = 0x10
)

type UnifiedLock struct {
    ID         string
    Owner      LockOwner
    FileHandle FileHandle
    Offset     uint64
    Length     uint64
    Type       LockType
    AccessMode AccessMode
    OpLock     *OpLock      // nil for byte-range locks
    AcquiredAt time.Time
    Blocking   bool
    Reclaim    bool
}

func (ul *UnifiedLock) HasOpLock() bool { return ul.OpLock != nil }

func (ul *UnifiedLock) ConflictsWith(other *UnifiedLock) bool {
    if ul.Owner.OwnerID == other.Owner.OwnerID {
        return false
    }
    // AccessMode conflicts
    if accessModesConflict(ul.AccessMode, other.AccessMode) {
        return true
    }
    // OpLock vs OpLock
    if ul.HasOpLock() && other.HasOpLock() {
        return OpLocksConflict(ul.OpLock, other.OpLock)
    }
    // OpLock vs byte-range
    if ul.HasOpLock() != other.HasOpLock() {
        return opLockConflictsWithByteLock(ul, other)
    }
    // Byte-range vs byte-range
    if !RangesOverlap(ul.Offset, ul.Length, other.Offset, other.Length) {
        return false
    }
    if ul.Type == LockTypeShared && other.Type == LockTypeShared {
        return false
    }
    return true
}
```

### Example 2: share_adapter_configs GORM Model

```go
// pkg/controlplane/models/share_adapter_config.go

type ShareAdapterConfig struct {
    ID          string    `gorm:"primaryKey;size:36" json:"id"`
    ShareID     string    `gorm:"not null;size:36;uniqueIndex:idx_share_adapter" json:"share_id"`
    AdapterType string    `gorm:"not null;size:50;uniqueIndex:idx_share_adapter" json:"adapter_type"`
    Config      string    `gorm:"type:text" json:"config"`
    CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`
    UpdatedAt   time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func (ShareAdapterConfig) TableName() string {
    return "share_adapter_configs"
}
```

### Example 3: PersistedLock Field Renames

```go
// pkg/metadata/lock/store.go -- updated PersistedLock

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

    // Renamed: was ShareReservation
    AccessMode int `json:"access_mode"`

    // Renamed: was LeaseKey/LeaseState/LeaseEpoch/BreakToState/Breaking
    OpLockGroupKey     string `json:"oplock_group_key,omitempty"`
    OpLockState        uint8  `json:"oplock_state,omitempty"`
    OpLockStateVersion uint32 `json:"oplock_state_version,omitempty"`
    OpLockBreakTarget  uint8  `json:"oplock_break_target,omitempty"`
    OpLockBreaking     bool   `json:"oplock_breaking,omitempty"`
}

func (pl *PersistedLock) IsOpLock() bool {
    return pl.OpLockGroupKey != ""
}
```

### Example 4: NLM Service Extraction Interface

```go
// In NFS adapter package, minimal interface to avoid import cycle

type FileChecker interface {
    GetFile(ctx context.Context, handle []byte) (exists bool, isDir bool, err error)
}

type NLMService struct {
    lockMgr     *lock.Manager
    fileChecker FileChecker
    onUnlock    func(handle []byte)
}

func (s *NLMService) LockFileNLM(ctx context.Context, handle []byte, owner lock.LockOwner,
    offset, length uint64, exclusive, reclaim bool) (*lock.LockResult, error) {
    // Verify file exists
    exists, isDir, err := s.fileChecker.GetFile(ctx, handle)
    if err != nil { return nil, err }
    if !exists { return nil, errors.ErrNoEntity }
    if isDir { return nil, errors.ErrIsDirectory }

    // Create and add lock
    lockType := lock.LockTypeShared
    if exclusive { lockType = lock.LockTypeExclusive }
    ul := lock.NewUnifiedLock(owner, lock.FileHandle(handle), offset, length, lockType)
    ul.Reclaim = reclaim

    handleKey := string(handle)
    err = s.lockMgr.AddUnifiedLock(handleKey, ul)
    // ... conflict handling
}
```

## Current Codebase Analysis

### Lock Package (pkg/metadata/lock/)
- **22 files, 8,803 lines total** (including tests)
- `types.go` (389 lines): `EnhancedLock`, `ShareReservation`, `LockOwner`, `IsEnhancedLockConflicting()`, `RangesOverlap()`
- `lease_types.go` (270 lines): `LeaseInfo`, `LeaseState*` constants, `LeasesConflict()`, `LeaseConflictsWithByteRangeLock()`
- `lease_break.go` (242 lines): `LeaseBreakScanner`, `LeaseBreakCallback`
- `manager.go` (769 lines): `Manager` with dual maps (`locks` for legacy, `enhancedLocks` for unified), `AddEnhancedLock`, `RemoveEnhancedLock`, `SplitLock`, `MergeLocks`, `UpgradeLock`
- `store.go` (311 lines): `PersistedLock`, `LockStore`, `LockQuery`, `ToPersistedLock()`, `FromPersistedLock()`
- `grace.go` (387 lines): `GracePeriodManager` -- **already generic, no changes needed**
- `cross_protocol.go` (295 lines): `TranslateToNLMHolder()`, `TranslateSMBConflictReason()` -- update type references
- `errors.go` (73 lines): Error factory functions -- update `EnhancedLockConflict` param type

### MetadataService (pkg/metadata/service.go)
- **994 lines total**
- Lines 522-772: NLM-specific methods (`LockFileNLM`, `TestLockNLM`, `UnlockFileNLM`, `CancelBlockingLock`, `SetNLMUnlockCallback`) -- ~250 lines to move to NFS adapter
- Lines 774-897: SMB cross-protocol lease methods (`OplockChecker` interface, `CheckAndBreakLeasesFor*`, global `SetOplockChecker/GetOplockChecker`) -- ~120 lines to remove/replace with centralized LockManager
- Lines 899-994: `ReclaimLeaseSMB` -- ~95 lines to generalize as `ReclaimOpLock`

### Share Model (pkg/controlplane/models/share.go)
**Fields to move to `share_adapter_configs`:**
- NFS-specific: `Squash`, `AnonymousUID`, `AnonymousGID`, `AllowAuthSys`, `RequireKerberos`, `MinKerberosLevel`, `NetgroupID`, `DisableReaddirplus` (from runtime/share.go)
- SMB-specific: `GuestEnabled`, `GuestUID`, `GuestGID`
- Total: 11 protocol-specific columns to remove from Share table

### Runtime Share (pkg/controlplane/runtime/share.go)
**Fields to move:**
- `Squash models.SquashMode`, `AnonymousUID`, `AnonymousGID` -- NFS
- `DisableReaddirplus` -- NFS
- `AllowAuthSys`, `RequireKerberos`, `MinKerberosLevel`, `NetgroupName` -- NFS
- `BlockedOperations` -- Generic (keep, both adapters might use it)

### Store Interface (pkg/controlplane/store/interface.go)
- **484 lines, 60+ methods**
- Netgroup methods to remove: `GetNetgroup`, `GetNetgroupByID`, `ListNetgroups`, `CreateNetgroup`, `DeleteNetgroup`, `AddNetgroupMember`, `RemoveNetgroupMember`, `GetNetgroupMembers`, `GetSharesByNetgroup` -- 9 methods
- Identity mapping methods to remove: `GetIdentityMapping`, `ListIdentityMappings`, `CreateIdentityMapping`, `DeleteIdentityMapping` -- 4 methods
- Total: 13 methods to extract to NFS-specific sub-interfaces

### Runtime (pkg/controlplane/runtime/runtime.go)
- **1,259 lines**
- NFS-specific fields: `mounts map[string]*MountInfo` (line 99), `dnsCache` + `dnsCacheOnce` (lines 121-122), `shareChangeCallbacks` (line 125), `nfsClientProvider any` (line 128)
- NFS-specific methods: `RecordMount`, `RemoveMount`, `RemoveAllMounts`, `ListMounts`, `ApplyIdentityMapping`, `SetNFSClientProvider`, `NFSClientProvider`
- `netgroups.go`: 200+ lines of DNS cache and netgroup matching -- NFS-specific

### NFS-specific API Handlers
- `internal/controlplane/api/handlers/clients.go` (298 lines): Imports `internal/protocol/nfs/v4/state` -- purely NFS
- `internal/controlplane/api/handlers/grace.go` (84 lines): Imports `internal/protocol/nfs/v4/state` -- purely NFS
- `internal/controlplane/api/handlers/netgroups.go`: NFS-specific
- `internal/controlplane/api/handlers/identity_mappings.go`: NFS-specific

### pkg/identity/ Package
- 5 source files + 5 test files: `mapper.go`, `convention.go`, `table.go`, `static.go`, `cache.go`
- Pure NFSv4 Kerberos principal resolution
- To be dissolved: generic parts (if any) to metadata, NFS parts to NFS adapter internal

### Backward Compatibility Layer (pkg/metadata/lock_exports.go)
- **256 lines** of type aliases and re-exported functions
- Must be updated to track new names: `EnhancedLock` -> `UnifiedLock`, `ShareReservation` -> `AccessMode`, etc.
- Consider keeping old aliases as deprecated for one release

### Reference Count Summary

| Type/Symbol | Occurrences | Files |
|-------------|------------|-------|
| EnhancedLock | 460 | 71 |
| LeaseInfo/LeaseState | 583 | 57 |
| ShareReservation | 77 | 18 |
| LeaseBreakScanner/Callback | 113 | 11 |
| OplockChecker/Set/Get | 78 | 15 |
| SquashMode | 39 | 11 |

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `EnhancedLock` with flat fields | `UnifiedLock` with composed `OpLock` sub-type | This phase | All lock consumers |
| `LeaseInfo` with SMB-specific [16]byte key | `OpLock` with opaque string GroupKey | This phase | SMB+NFSv4 adapters |
| `ShareReservation` as enum | `AccessMode` as bitmask with 5 flags | This phase | SMB+NFSv4 adapters |
| Protocol fields on Share model | `share_adapter_configs` table with typed JSON | This phase | Share CRUD, adapter config |
| NLM methods on MetadataService | NLM service in NFS adapter | This phase | NFS adapter, MetadataService |
| OplockChecker global | Centralized break logic in LockManager | This phase | NFS-SMB cross-protocol |

## Open Questions

1. **LockManager injection path**
   - What we know: User wants to explore separate LockManager injection vs MetadataService getter, falling back to getter if simpler
   - What's unclear: Whether a separate `SetLockManager()` on adapters creates too much ceremony vs `runtime.GetMetadataService().GetLockManagerForShare()`
   - Recommendation: The current `GetLockManagerForShare(shareName)` getter on MetadataService is simple and works. The LockManager interface (new) can be obtained via `runtime.GetMetadataService().GetLockManager(share)` which returns the new unified interface. No separate injection needed unless analysis shows runtime dependency tangle. **Recommend: getter approach** (simpler, no new API surface).

2. **Config validation: API-level only vs defense-in-depth**
   - What we know: Share adapter configs are typed JSON validated at API level. Question is whether to also validate on startup load.
   - What's unclear: Whether invalid JSON could be written directly to DB (bypassing API).
   - Recommendation: **Both API + startup** (defense in depth). Startup validation logs warnings but doesn't block (graceful degradation). API validation returns 400. Cost is one small validation function per adapter -- trivial.

3. **Cross-protocol conflict tests: Phase 26 vs Phase 29**
   - What we know: The existing cross-protocol tests (`cross_protocol_test.go`, 411 lines) test the old type names. They need rewriting regardless.
   - Recommendation: **Phase 26** -- rewrite tests against new LockManager interface as part of this phase. The tests verify core conflict detection which is central to REF-01. Deferring would leave the new conflict logic untested.

4. **OpLock GroupKey format change: [16]byte vs string**
   - What we know: Current `LeaseInfo.LeaseKey` is `[16]byte`. New `OpLock.GroupKey` is `string` per the plan. SMB passes `hex.EncodeToString(leaseKey[:])`, NFSv4 passes `fmt.Sprintf("nfs4:%d", clientID)`.
   - What's unclear: The `PersistedLock.LeaseKey` is currently `[]byte`. Changing to string changes the serialization format.
   - Recommendation: Change `PersistedLock.OpLockGroupKey` to `string`. Since the CONTEXT decision is "clean break, no deprecation", and locks are ephemeral (lost on restart), there is no migration concern for persisted lock data. Memory and BadgerDB LockStores are ephemeral. PostgreSQL LockStore may need a column type change from `bytea` to `text`.

## Sources

### Primary (HIGH confidence)
- Codebase analysis: All source files read directly from `/Users/marmos91/Projects/dittofs/`
- `docs/GENERIC_LOCK_INTERFACE_PLAN.md` -- detailed type mapping and code examples
- `docs/CORE_REFACTORING_PLAN.md` -- Phase 1 protocol leak purge specification
- `.planning/phases/26-generic-lock-interface-protocol-leak-purge/26-CONTEXT.md` -- user decisions

### Secondary (MEDIUM confidence)
- `.planning/REQUIREMENTS.md` -- REF-01 and REF-02 requirement specifications

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- pure Go refactoring, no new dependencies
- Architecture: HIGH -- detailed plans exist in docs/, codebase structure well-understood
- Pitfalls: HIGH -- derived from direct codebase analysis, cross-reference counts verified

**Research date:** 2026-02-25
**Valid until:** 2026-03-25 (stable domain, internal refactoring)
