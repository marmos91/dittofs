---
phase: 26-generic-lock-interface-protocol-leak-purge
plan: 02
subsystem: database
tags: [gorm, sqlite, postgres, interface-segregation, adapter-config]

# Dependency graph
requires:
  - phase: 26-01
    provides: "Clean lock type names (no protocol leaks in lock layer)"
provides:
  - "ShareAdapterConfig GORM model with per-adapter per-share typed JSON config"
  - "NFSExportOptions and SMBShareOptions typed config structs"
  - "NetgroupStore and IdentityMappingStore as separate interfaces"
  - "Adapter config CRUD operations (get, set, delete, list)"
  - "Clean Share model without NFS/SMB-specific fields"
affects: [26-03, 26-04, 26-05, adapter-config-api, nfs-adapter, smb-adapter]

# Tech tracking
tech-stack:
  added: []
  patterns: ["adapter config pattern (per-adapter per-share JSON config)", "interface segregation for protocol-specific store operations"]

key-files:
  created:
    - "pkg/controlplane/models/share_adapter_config.go"
    - "pkg/controlplane/store/adapter_configs.go"
  modified:
    - "pkg/controlplane/models/share.go"
    - "pkg/controlplane/store/interface.go"
    - "pkg/controlplane/store/gorm.go"
    - "pkg/controlplane/store/shares.go"
    - "pkg/controlplane/store/netgroups.go"
    - "pkg/controlplane/runtime/init.go"
    - "pkg/controlplane/runtime/netgroups.go"
    - "pkg/controlplane/api/router.go"
    - "internal/controlplane/api/handlers/shares.go"
    - "internal/controlplane/api/handlers/netgroups.go"
    - "internal/controlplane/api/handlers/identity_mappings.go"
    - "pkg/apiclient/shares.go"

key-decisions:
  - "Kept SquashMode in models/permission.go (shared by NFS adapter and runtime identity mapping)"
  - "Runtime Share struct retains NFS-specific fields for fast handler access, populated from adapter config at load time"
  - "Netgroup-in-use check queries adapter config JSON (LIKE search) since netgroup_id moved to NFS config"
  - "Auto-create default NFS and SMB adapter configs when share is created"
  - "Router uses type assertion for NetgroupStore/IdentityMappingStore (routes only registered if store implements them)"

patterns-established:
  - "Adapter config pattern: ShareAdapterConfig with ShareID + AdapterType unique index, typed JSON config via ParseConfig/SetConfig"
  - "Interface segregation: protocol-specific store operations behind separate interfaces with runtime type assertions"
  - "Default config creation: share creation auto-creates default adapter configs for each protocol"

requirements-completed: [REF-02]

# Metrics
duration: 16min
completed: 2026-02-25
---

# Phase 26 Plan 02: Share Adapter Config System Summary

**ShareAdapterConfig model with per-adapter typed JSON config, interface segregation for NetgroupStore/IdentityMappingStore, and clean Share model without protocol-specific fields**

## Performance

- **Duration:** 16 min
- **Started:** 2026-02-25T09:15:51Z
- **Completed:** 2026-02-25T09:31:51Z
- **Tasks:** 3
- **Files modified:** 20

## Accomplishments

- Created ShareAdapterConfig GORM model with per-adapter per-share typed JSON config system
- Removed 11 NFS/SMB-specific fields from Share model (Squash, AnonymousUID/GID, AllowAuthSys, RequireKerberos, MinKerberosLevel, NetgroupID, DisableReaddirplus, GuestEnabled, GuestUID, GuestGID)
- Extracted NetgroupStore (9 methods) and IdentityMappingStore (4 methods) into separate interfaces from main Store
- Updated all consumers: runtime share loading, API handlers, router, tests, and API client

## Task Commits

Each task was committed atomically:

1. **Tasks 1-2: Create ShareAdapterConfig model, clean Share, extract interfaces** - `bc4e1c41` (refactor)
2. **Task 3: Fix all compilation errors, update consumers, run tests** - `a185dafc` (fix)

## Files Created/Modified

- `pkg/controlplane/models/share_adapter_config.go` - ShareAdapterConfig GORM model, NFSExportOptions, SMBShareOptions
- `pkg/controlplane/store/adapter_configs.go` - CRUD operations for ShareAdapterConfig on GORMStore
- `pkg/controlplane/models/share.go` - Clean Share model without protocol fields
- `pkg/controlplane/store/interface.go` - Store without netgroup/identity methods, separate NetgroupStore/IdentityMappingStore
- `pkg/controlplane/store/gorm.go` - Post-migration column drops for removed Share fields
- `pkg/controlplane/store/shares.go` - Guest access via SMB adapter config, adapter config cleanup on share delete
- `pkg/controlplane/store/netgroups.go` - Netgroup-in-use check via adapter config JSON search
- `pkg/controlplane/runtime/init.go` - LoadSharesFromStore loads NFS options from adapter config
- `pkg/controlplane/runtime/netgroups.go` - Type-assert store to NetgroupStore for member lookup
- `pkg/controlplane/api/router.go` - Type-assert for netgroup/identity routes registration
- `internal/controlplane/api/handlers/shares.go` - Remove protocol fields from create/update/response, auto-create adapter configs
- `internal/controlplane/api/handlers/netgroups.go` - Accept NetgroupStore interface
- `internal/controlplane/api/handlers/identity_mappings.go` - Accept IdentityMappingStore interface
- `pkg/apiclient/shares.go` - Remove protocol-specific fields from Share/CreateShareRequest/UpdateShareRequest

## Decisions Made

1. **SquashMode stays in models/permission.go** - Used by both NFS adapter and runtime identity mapping, so cannot move to adapter package without circular dependency
2. **Runtime Share struct keeps NFS-specific fields** - Protocol handlers need fast access; fields populated from adapter config at share load time (no DB query per NFS operation)
3. **Netgroup-in-use check uses JSON LIKE search** - Since netgroup_id is now inside adapter config JSON, DeleteNetgroup searches share_adapter_configs.config with LIKE pattern
4. **Router conditionally registers routes** - Netgroup and identity mapping routes only registered if store implements the respective interface (always true for GORMStore, but architecturally clean)
5. **Default adapter configs auto-created** - When a share is created via API, default NFS and SMB adapter configs are automatically created

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Netgroup-in-use check queries wrong table**
- **Found during:** Task 3 (compilation fix)
- **Issue:** DeleteNetgroup and GetSharesByNetgroup queried shares.netgroup_id column which was dropped
- **Fix:** Updated to search share_adapter_configs.config JSON blob for netgroup ID references
- **Files modified:** pkg/controlplane/store/netgroups.go
- **Verification:** Integration tests pass for netgroup-in-use scenario
- **Committed in:** bc4e1c41

**2. [Rule 3 - Blocking] Router type mismatch for handler constructors**
- **Found during:** Task 3 (compilation fix)
- **Issue:** Router passes store.Store to NewNetgroupHandler/NewIdentityMappingHandler which now accept narrower interfaces
- **Fix:** Added runtime type assertion with conditional route registration
- **Files modified:** pkg/controlplane/api/router.go
- **Verification:** Build passes, all tests pass
- **Committed in:** a185dafc

---

**Total deviations:** 2 auto-fixed (1 bug, 1 blocking)
**Impact on plan:** Both fixes necessary for correctness. No scope creep.

## Issues Encountered

- Plan suggested cleaning runtime Share struct of NFS fields, but runtime Share struct must retain them for fast protocol handler access (populated from adapter config at load time, not per-operation DB queries)
- E2e tests for netgroup-share association now need adapter config API to set netgroup on share (marked as t.Skip with TODO)

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Share model is clean of protocol-specific fields
- Adapter config system is in place and tested
- Ready for plan 03 (adapter lifecycle cleanup) and beyond
- Future adapter config API (REST endpoint for managing per-share adapter configs) will enable e2e testing of netgroup-share association

---
*Phase: 26-generic-lock-interface-protocol-leak-purge*
*Completed: 2026-02-25*
