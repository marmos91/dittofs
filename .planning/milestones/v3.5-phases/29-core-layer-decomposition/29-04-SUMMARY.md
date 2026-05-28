---
phase: 29-core-layer-decomposition
plan: 04
subsystem: api
tags: [go-interfaces, dependency-injection, interface-segregation, gorm, chi]

# Dependency graph
requires:
  - phase: 29-01
    provides: "Generic GORM helpers (createWithID, listAll, etc.) used by store implementations"
provides:
  - "12 named sub-interfaces for Store (UserStore, GroupStore, ShareStore, etc.)"
  - "Composite Store interface embedding all sub-interfaces"
  - "Narrowed API handlers accepting minimal sub-interfaces"
  - "Compile-time assertions for all sub-interfaces"
affects: [29-05, 29-06, 29-07, future-test-mocking]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Interface Segregation Principle for Go store layer"
    - "Composite interface via embedding for backward compatibility"
    - "Handler-local composite interfaces for multi-store handlers (ShareHandlerStore)"

key-files:
  created: []
  modified:
    - "pkg/controlplane/store/interface.go"
    - "pkg/controlplane/store/shares.go"
    - "pkg/controlplane/store/adapters.go"
    - "pkg/controlplane/store/health.go"
    - "internal/controlplane/api/handlers/shares.go"
    - "internal/controlplane/api/handlers/auth.go"
    - "internal/controlplane/api/handlers/users.go"
    - "internal/controlplane/api/handlers/groups.go"
    - "internal/controlplane/api/handlers/adapter_settings.go"
    - "internal/controlplane/api/handlers/metadata_stores.go"
    - "internal/controlplane/api/handlers/payload_stores.go"
    - "internal/controlplane/api/handlers/settings.go"

key-decisions:
  - "GuestUser/IsGuestEnabled folded into UserStore (returns *User, per research)"
  - "ShareHandler gets custom composite ShareHandlerStore (6 sub-interfaces) since it needs cross-entity queries"
  - "NetgroupStore and IdentityMappingStore kept outside composite Store (accessed via type assertion)"
  - "Compile-time assertions for all 12 sub-interfaces plus models.UserStore and models.IdentityStore"
  - "Router unchanged -- Go implicit interface satisfaction narrows full Store to sub-interfaces automatically"

patterns-established:
  - "Interface Segregation: each handler declares exactly the store methods it needs"
  - "Handler-local composite: when a handler needs multiple sub-interfaces, define a local composite (ShareHandlerStore)"
  - "Compile-time assertion block: var _ SubInterface = (*GORMStore)(nil) for every sub-interface in health.go"

requirements-completed: [REF-05.1, REF-05.2]

# Metrics
duration: 18min
completed: 2026-02-26
---

# Phase 29 Plan 04: Store Sub-Interfaces + Handler Narrowing Summary

**Decomposed monolithic Store (~70 methods) into 12 named sub-interfaces and narrowed all API handlers to accept only the specific sub-interface(s) they use**

## Performance

- **Duration:** 18 min
- **Started:** 2026-02-26T10:38:00Z
- **Completed:** 2026-02-26T10:56:00Z
- **Tasks:** 2
- **Files modified:** 17

## Accomplishments
- Decomposed single Store interface into 12 named sub-interfaces (UserStore, GroupStore, ShareStore, PermissionStore, MetadataStoreConfigStore, PayloadStoreConfigStore, AdapterStore, SettingsStore, AdminStore, HealthStore, NetgroupStore, IdentityMappingStore)
- Created composite Store interface embedding 10 core sub-interfaces (NetgroupStore/IdentityMappingStore remain separate for type-assertion access)
- Narrowed 8 API handlers from full Store to specific sub-interfaces (AuthHandler->UserStore, UserHandler->UserStore, GroupHandler->GroupStore, ShareHandler->ShareHandlerStore, AdapterSettingsHandler->AdapterStore, MetadataStoreHandler->MetadataStoreConfigStore, PayloadStoreHandler->PayloadStoreConfigStore, SettingsHandler->SettingsStore)
- Renamed/absorbed 5 underscore files per CONTEXT.md (adapter_configs->shares, adapter_settings->adapters, metadata_stores->metadata, payload_stores->payload, identity_mappings->identity)
- Added 15 compile-time assertions verifying GORMStore satisfies all sub-interfaces

## Task Commits

Each task was committed atomically:

1. **Task 1: Decompose Store interface into sub-interfaces + rename files** - `00580c86` (refactor)
2. **Task 2: Narrow API handlers to specific sub-interfaces** - `f7804d69` (refactor)

## Files Created/Modified
- `pkg/controlplane/store/interface.go` - Rewritten: 12 sub-interfaces + composite Store
- `pkg/controlplane/store/shares.go` - Absorbed adapter_configs.go methods (ShareAdapterConfig CRUD)
- `pkg/controlplane/store/adapters.go` - Absorbed adapter_settings.go methods (NFS/SMB settings)
- `pkg/controlplane/store/health.go` - 15 compile-time assertions for all sub-interfaces
- `pkg/controlplane/store/metadata.go` - Renamed from metadata_stores.go
- `pkg/controlplane/store/payload.go` - Renamed from payload_stores.go
- `pkg/controlplane/store/identity.go` - Renamed from identity_mappings.go
- `pkg/controlplane/store/adapter_configs.go` - Deleted (methods moved to shares.go)
- `pkg/controlplane/store/adapter_settings.go` - Deleted (methods moved to adapters.go)
- `internal/controlplane/api/handlers/auth.go` - store.Store -> store.UserStore
- `internal/controlplane/api/handlers/users.go` - store.Store -> store.UserStore
- `internal/controlplane/api/handlers/groups.go` - store.Store -> store.GroupStore
- `internal/controlplane/api/handlers/shares.go` - store.Store -> ShareHandlerStore (6 sub-interfaces)
- `internal/controlplane/api/handlers/adapter_settings.go` - store.Store -> store.AdapterStore
- `internal/controlplane/api/handlers/metadata_stores.go` - store.Store -> store.MetadataStoreConfigStore
- `internal/controlplane/api/handlers/payload_stores.go` - store.Store -> store.PayloadStoreConfigStore
- `internal/controlplane/api/handlers/settings.go` - store.Store -> store.SettingsStore

## Decisions Made
- **GuestUser/IsGuestEnabled in UserStore**: Placed in UserStore per research recommendation (returns *User). Implementation stays in shares.go since GORMStore satisfies all interfaces.
- **ShareHandler composite interface**: ShareHandler calls methods from 6 sub-interfaces (ShareStore, PermissionStore, MetadataStoreConfigStore, PayloadStoreConfigStore, UserStore, GroupStore). Created handler-local ShareHandlerStore composite rather than accepting full Store.
- **NetgroupStore/IdentityMappingStore outside composite**: These remain outside the main Store composite because they are accessed via type assertion in runtime code, not directly by all consumers.
- **Router unchanged**: Go's implicit interface satisfaction means the router passes its full store.Store to handler constructors, which automatically narrows to sub-interfaces. No explicit casting needed.
- **Test files unchanged**: Tests use integration build tags and construct full GORMStore, which satisfies all sub-interfaces. No test modifications required.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Sub-interfaces enable future test mocking with minimal implementations
- Plans 29-05 through 29-07 can proceed with narrowed interfaces in place
- All existing tests continue to pass with no modifications

## Self-Check: PASSED

- All 7 modified/renamed files exist
- Both deleted files (adapter_configs.go, adapter_settings.go) confirmed removed
- Commit 00580c86 found in git log
- Commit f7804d69 found in git log
- 29-04-SUMMARY.md exists

---
*Phase: 29-core-layer-decomposition*
*Completed: 2026-02-26*
