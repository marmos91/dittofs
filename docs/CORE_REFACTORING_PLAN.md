# Core Layer Refactoring Analysis & Plan

## Context

The NFS and SMB refactoring plans (in `docs/`) address protocol adapter restructuring. This plan targets the **core product layers beneath the adapters**. Zero overlap with the NFS/SMB plans.

**Execution order:** Complete this plan **before** starting the NFS/SMB refactoring - it provides a cleaner foundation.

## Summary of Findings

1. **Protocol leakage** - NFS/SMB-specific types, locking, and config pollute generic layers
2. **God objects** - Runtime (1,258 lines), MetadataService (994 lines), TransferManager (1,361 lines)
3. **Interface bloat** - ControlPlane Store (60+ methods in one interface)
4. **Boilerplate duplication** - GORM CRUD, API handlers, API client, transaction tests
5. **Naming** - TransferManager -> Offloader rename opportunity, plus other naming improvements

---

## Phase 1: Purge Protocol-Specific Leaks from Generic Layers

**Why:** Generic packages (`pkg/metadata/`, `pkg/controlplane/`, `pkg/payload/`) should only work with abstract, protocol-agnostic types. Currently NFS and SMB concepts are deeply embedded in these layers.

### Step 1.1: Extract SMB Lock Types from `pkg/metadata/lock/`

**Problem:** The generic lock package contains SMB-only types:
- `ShareReservation` (DenyRead/DenyWrite/DenyAll) in `lock/types.go:49-82` - pure SMB share mode concept
- `LeaseInfo` (LeaseKey, R/W/H state flags) in `lock/lease_types.go` - pure SMB2/3 lease
- `lease_break.go` - entire file is SMB lease break management
- `EnhancedLock` struct embeds both `ShareReservation` and `LeaseInfo`

**Fix:**
- Move `ShareReservation`, `LeaseInfo`, `LeaseBreakScanner`, lease conflict functions to `internal/adapter/smb/lock/` (or `pkg/adapter/smb/lock/`)
- Keep `EnhancedLock` generic: byte-range only (LockType, Offset, Length, exclusive/shared)
- SMB adapter wraps the generic lock with its lease/reservation fields
- Define a `ProtocolLockExtension interface{}` field on `EnhancedLock` for protocol-specific data, or use composition in SMB adapter

**Files:**
- `pkg/metadata/lock/types.go` - remove `ShareReservation`, simplify `EnhancedLock`
- `pkg/metadata/lock/lease_types.go` - move entirely to SMB adapter
- `pkg/metadata/lock/lease_break.go` - move entirely to SMB adapter
- SMB handlers that use these types - update imports

### Step 1.2: Extract SMB Lease Methods from MetadataService

**Problem:** `pkg/metadata/service.go` has SMB-only methods:
- `CheckAndBreakLeasesForWrite/Read/Delete` (lines 845-897)
- `ReclaimLeaseSMB` (lines 925-994) with SMB lease keys and state flags
- `OplockChecker` interface + global `SetOplockChecker`/`GetOplockChecker` (lines 817-828)

**Fix:**
- Move lease-breaking methods to SMB adapter (or a `pkg/adapter/smb/leasemanager/`)
- SMB adapter registers a lease checker callback on MetadataService instead of MetadataService knowing about leases
- Remove `OplockChecker` global; SMB adapter owns oplock management

**Files:**
- `pkg/metadata/service.go` - remove ~200 lines of SMB lease code
- SMB adapter - receives extracted code

### Step 1.3: Extract NLM-Specific Methods from MetadataService

**Problem:** `pkg/metadata/service.go` has NLM-only methods:
- `LockFileNLM`, `TestLockNLM`, `UnlockFileNLM`, `CancelBlockingLock` (lines 549-810)
- `SetNLMUnlockCallback` (line 684)

**Fix:**
- Move to NFS adapter layer (e.g., `pkg/adapter/nfs/nlmservice/` or `internal/adapter/nfs/nlm/`)
- NLM service holds a reference to MetadataService for the generic lock manager
- MetadataService keeps only generic byte-range lock operations

**Files:**
- `pkg/metadata/service.go` - remove ~260 lines of NLM code
- NFS adapter - receives extracted NLM service

### Step 1.4: Extract Grace Period from Generic Lock Package

**Problem:** `pkg/metadata/lock/grace.go` (387 lines) - NFS/NLM-specific grace period recovery mechanism in generic lock layer.

**Fix:** Move `GracePeriodManager` to NFS adapter. Generic lock manager should not assume NFS recovery semantics.

**Files:** `pkg/metadata/lock/grace.go` -> NFS adapter

### Step 1.5: Clean Up Share Model - Separate Protocol-Specific Fields

**Problem:** `pkg/controlplane/models/share.go` mixes NFS and SMB fields:
- NFS-specific: `AllowAuthSys`, `RequireKerberos`, `MinKerberosLevel`, `NetgroupID`, `Squash`, `AnonymousUID/GID` (squash modes are NFS terminology per comments)
- SMB-specific: `GuestEnabled`, `GuestUID`, `GuestGID`
- NFS-specific: `DisableReaddirplus` (also in `runtime/share.go`)

**Fix:** Use the existing `Config` JSON blob field for protocol-specific options:
- Move NFS fields into `NFSExportOptions` struct stored in share's `Config` JSON blob
- Move SMB fields into `SMBShareOptions` struct stored in share's `Config` JSON blob
- Keep only generic fields on `Share` model: ID, Name, MetadataStoreID, PayloadStoreID, ReadOnly, DefaultPermission
- NFS adapter deserializes `NFSExportOptions` from config; SMB adapter deserializes `SMBShareOptions`
- `runtime/share.go` `Share` and `ShareConfig` structs are similarly cleaned

**Files:**
- `pkg/controlplane/models/share.go` - remove protocol-specific columns
- `pkg/controlplane/runtime/share.go` - remove protocol-specific fields
- `pkg/controlplane/runtime/runtime.go` - update `AddShare` to handle config
- NFS/SMB adapters - parse their options from config blob
- DB migration needed (move column data to JSON config)

### Step 1.6: Move SquashMode to NFS Adapter

**Problem:** `pkg/controlplane/models/permission.go:103-210` - `SquashMode` (root_squash, all_squash, etc.) is explicitly documented as "NFS shares" and "Synology NAS squash options". It's a pure NFS identity mapping concept.

**Fix:** Move `SquashMode` type and constants to the NFS adapter. Generic identity mapping (if needed cross-protocol) can use a simpler abstraction.

**Files:** `pkg/controlplane/models/permission.go` -> NFS adapter types

### Step 1.7: Extract Netgroup and Identity Mapping from Store Interface

**Problem:** `pkg/controlplane/store/interface.go` includes:
- Netgroup operations (8 methods) - purely NFS concept
- Identity mapping operations (4 methods) - NFSv4 Kerberos principal mapping

**Fix:**
- Define `NetgroupStore` and `IdentityMappingStore` sub-interfaces but move them to NFS-specific code
- Remove from the generic `Store` composition
- NFS adapter type-asserts or uses a separate store reference

**Files:**
- `pkg/controlplane/store/interface.go` - remove 12 methods from `Store`
- NFS adapter - use separate store interfaces

### Step 1.8: Move NFS-Specific Runtime Fields

**Problem:** `pkg/controlplane/runtime/runtime.go` has NFS-only fields:
- `mounts map[string]*MountInfo` (line 99) - NFS mount tracking
- `dnsCache` + `dnsCacheOnce` (lines 120-122) - netgroup hostname resolution
- `shareChangeCallbacks` (line 124) - NFSv4 pseudo-fs rebuild
- `nfsClientProvider any` (line 127) - NFSv4 client state

**Fix:**
- `mounts`: Move to NFS adapter (only NFS mounts are tracked)
- `dnsCache`: Move to NFS adapter (only NFS uses netgroups)
- `nfsClientProvider any`: Replace with typed `NFSClientProvider` interface or move to NFS adapter
- `shareChangeCallbacks`: Generalize as event system or keep but rename to clarify it's a generic hook (SMB might also want share-change notifications)

**Files:** `pkg/controlplane/runtime/runtime.go` - remove NFS-specific fields

### Step 1.9: Move NFS-Specific API Handlers

**Problem:** `internal/controlplane/api/handlers/` contains:
- `clients.go` - NFSv4 client management endpoints
- `grace.go` - NFSv4 grace period endpoints (imports `internal/protocol/nfs/v4/state`)

**Fix:** Move these to NFS adapter-specific API routes. The generic API server can allow adapters to register additional routes.

**Files:**
- `internal/controlplane/api/handlers/clients.go` -> NFS adapter
- `internal/controlplane/api/handlers/grace.go` -> NFS adapter

### Step 1.10: Move `pkg/identity/` to NFS Adapter

**Problem:** `pkg/identity/` is NFSv4 Kerberos principal resolution. Public `pkg/` should be protocol-agnostic.

**Fix:** Move to `internal/adapter/nfs/v4/identity/` (or after NFS refactoring: `internal/adapter/nfs/v4/identity/`)

**Files:** `pkg/identity/` -> NFS internal

---

## Phase 2: Decompose the ControlPlane Store Interface

**Why:** `pkg/controlplane/store/interface.go` (484 lines, 60+ methods) is a single monolithic interface. After Phase 1 removes ~12 NFS-specific methods, the remaining ~48 methods should be decomposed into sub-interfaces.

### Step 2.1: Define Sub-Interfaces

```go
type UserStore interface { /* 11 methods */ }
type GroupStore interface { /* 8 methods */ }
type ShareStore interface { /* 4 methods */ }
type PermissionStore interface { /* 9 methods */ }
type StoreConfigStore interface { /* 12 methods */ }
type AdapterStore interface { /* 6 methods */ }
type AdapterSettingsStore interface { /* 7 methods */ }
type SettingsStore interface { /* 4 methods */ }
type HealthStore interface { /* 2 methods */ }

type Store interface {
    UserStore; GroupStore; ShareStore; PermissionStore
    StoreConfigStore; AdapterStore; AdapterSettingsStore
    SettingsStore; HealthStore
}
```

**Files:** `pkg/controlplane/store/interface.go`

### Step 2.2: Narrow Handler Dependencies

Update API handlers to accept narrowest interface needed.

**Files:** `internal/controlplane/api/handlers/*.go`

---

## Phase 3: Split Runtime into Focused Managers

**Why:** After Phase 1 removes NFS-specific fields, Runtime is still ~900 lines with 40+ methods managing metadata stores, shares, adapters, settings, and lifecycle.

### Step 3.1: Extract AdapterManager
Move adapter lifecycle (~300 lines, already uses its own `adaptersMu`).

### Step 3.2: Extract MetadataStoreManager
Move store registry (~70 lines).

### Step 3.3: Replace `nfsClientProvider any` with Typed Interface
(If not fully removed in Phase 1.8, at minimum type it properly.)

**Result:** `runtime.go` shrinks from 1,258 to ~500 lines.

---

## Phase 4: Rename TransferManager -> Offloader (+ Related Naming)

**Why:** "TransferManager" is generic and doesn't convey what the component does. "Offloader" clearly communicates its purpose: offloading bytes from cache to block stores.

### Naming Evaluation

| Current | Proposed | Rationale |
|---------|----------|-----------|
| `TransferManager` | `Offloader` | Conveys the "cache -> block store" direction. The main job is offloading cached data. |
| `TransferQueue` | `OffloadQueue` | Consistent with parent rename. |
| `TransferRequest` | `OffloadRequest` | Consistent. However, downloads are *not* offloads. See note below. |
| `TransferType` | `OffloadPriority` | `Download/Upload/Prefetch` are really priority levels. |
| `pkg/payload/transfer/` | `pkg/payload/offloader/` | Package rename. |

**Nuance:** The component handles both uploads (offloading) AND downloads (loading). "Offloader" emphasizes the primary direction. Alternatives considered:
- `BlockSyncer` - accurate but sounds like rsync
- `CacheBridge` - too abstract
- `BlockOrchestrator` - too generic
- `StorageOffloader` - redundant
- **`Offloader`** - clean, memorable, conveys primary purpose

For the download path specifically, methods like `EnsureAvailable` and `downloadBlock` remain clear on their own. The download functionality is secondary (cache miss recovery) while offloading is the core value proposition.

### Additional Naming Improvements

| Current | Proposed | Rationale |
|---------|----------|-----------|
| `PayloadService` | `ContentService` or keep `PayloadService` | "Payload" is unusual; "Content" is more standard. But this is a larger rename - evaluate separately. |
| `BlockStore` (in payload) | Keep as-is | Clear and accurate. |
| `fileUploadState` | `fileOffloadState` | Consistent with Offloader rename. |
| `FinalizationCallback` | `OffloadCompleteCallback` | Clearer intent. |

### Execution

1. `git mv pkg/payload/transfer/ pkg/payload/offloader/`
2. Rename types: `TransferManager` -> `Offloader`, `TransferQueue` -> `OffloadQueue`, etc.
3. Update all imports (~22 files reference the transfer package)
4. Update CLAUDE.md docs

**Validation:** `go build ./...` && `go test ./...`

---

## Phase 5: Split Offloader (formerly TransferManager) into Components

**Why:** At 1,361 lines with 34 methods, the Offloader mixes upload orchestration, download coordination, deduplication, and prefetch.

### Step 5.1: Extract Upload Orchestrator (`upload.go`)
Move: upload state tracking, eager upload, block upload, flush, finalization (~450 lines)

### Step 5.2: Extract Download Coordinator (`download.go`)
Move: downloadBlock, EnsureAvailable, enqueueDownload, prefetch, in-flight dedup (~250 lines)

### Step 5.3: Extract Dedup Handler (`dedup.go`)
Move: hash computation, FindBlockByHash integration, ref counting (~150 lines)

**Result:** `offloader.go` shrinks from 1,361 to ~400 lines.

---

## Phase 6: Unify Error Handling

**Why:** Metadata uses structured `StoreError` codes. Payload uses sentinel `errors.New()`. Protocol handlers switch between paradigms.

### Step 6.1: Add Structured Payload Errors
`PayloadError` struct wrapping sentinels with context. Maintains `errors.Is()` compatibility.

### Step 6.2: Create Shared Error-to-Status Mapping
Unified mapping for both error types to NFS/SMB status codes.

---

## Phase 7: Reduce Boilerplate

### Step 7.1: Generic GORM Helpers
`pkg/controlplane/store/generic.go` with `getByField[T]`, `listAll[T]`, `createWithID[T]`.

### Step 7.2: API Error Mapping Helper
`internal/controlplane/api/handlers/error_mapper.go` - centralized store-error-to-HTTP mapping.

### Step 7.3: Generic API Client Helpers
`pkg/apiclient/generic.go` with `getResource[T]`, `listResources[T]`, etc.

---

## Phase 8: Reduce Transaction & Test Duplication

### Step 8.1: Common Transaction Helpers
`pkg/metadata/store/txutil/helpers.go` - shared validation/wrapping logic.

### Step 8.2: Shared Transaction Test Suite
`pkg/metadata/store/storetest/` - parameterized tests all backends call.

---

## Phase 9: File Organization in pkg/metadata

### Step 9.1: Split file.go (1,217 lines)
-> `file_create.go`, `file_modify.go`, `file_remove.go`, `file_helpers.go`

### Step 9.2: Split authentication.go (796 lines)
-> `identity.go`, `permissions.go`

---

## Execution Order

```
Phase 1 (Protocol Leak Purge)     <-- HIGHEST PRIORITY, DO FIRST
  1.1-1.4 (lock/lease/grace)      ← parallelizable
  1.5-1.6 (share model/squash)    ← parallelizable
  1.7-1.10 (store/runtime/API)    ← after 1.5
      |
      v
Phase 2 (Store Interface)     Phase 9 (File Splits)
  2.1 -> 2.2                    9.1, 9.2
      |                        (independent)
      v
Phase 3 (Runtime Split)
  3.1 -> 3.2 -> 3.3
      |
      v
Phase 4 (Offloader Rename)    Phase 6 (Errors)
      |                          6.1 -> 6.2
      v
Phase 5 (Offloader Split)     Phase 7 (Boilerplate)
  5.1 -> 5.2 -> 5.3             7.1, 7.2, 7.3
                                    |
                                    v
                               Phase 8 (Transactions)
                                 8.1, 8.2
```

---

## Verification Protocol (every step)

1. `go build ./...`
2. `go vet ./...`
3. `go test ./...`
4. `go test -race ./...`

After all phases: E2E tests + manual NFS/SMB mount tests.

---

## Impact Summary

| Metric | Before | After |
|--------|--------|-------|
| Protocol-specific types in generic layers | ~15 types/methods | 0 |
| `runtime.go` | 1,258 lines | ~500 lines |
| `service.go` | 994 lines | ~400 lines (after removing NLM+SMB+lease code) |
| `lock/` package | Mixed NFS/SMB/generic | Pure generic byte-range locking |
| `Share` model | 12 NFS/SMB-specific fields | Protocol fields in JSON config blob |
| Store interface | 60+ methods (1 interface) | ~48 methods in 9 sub-interfaces |
| `transfer/manager.go` | 1,361 lines ("TransferManager") | ~400 lines ("Offloader") + 3 components |
| `file.go` | 1,217 lines | 4 focused files |
| GORM boilerplate | ~30 identical patterns | Generic helpers |
| API error handling | Repeated per handler | Centralized mapper |
