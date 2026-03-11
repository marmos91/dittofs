---
phase: 29-core-layer-decomposition
plan: 01
subsystem: core
tags: [generics, errors, gorm, apiclient, payload, adapter]

# Dependency graph
requires: []
provides:
  - PayloadError structured type wrapping sentinel errors with Unwrap()
  - ProtocolError interface for adapter-level error translation
  - MapError method on Adapter interface (stubs on BaseAdapter)
  - Generic GORM helpers (getByField, listAll, createWithID, deleteByField)
  - Generic API client helpers (getResource, listResources, createResource, updateResource, deleteResource)
affects: [29-02, 29-03, 29-04, 29-05, 29-06, 29-07]

# Tech tracking
tech-stack:
  added: []
  patterns: [generic-crud-helpers, structured-error-wrapping, protocol-error-interface]

key-files:
  created:
    - pkg/payload/errors.go (PayloadError type)
    - pkg/adapter/errors.go (ProtocolError interface)
    - pkg/controlplane/store/helpers.go (generic GORM helpers)
    - pkg/apiclient/helpers.go (generic API client helpers)
  modified:
    - pkg/adapter/adapter.go (MapError added to Adapter interface)
    - pkg/adapter/base.go (MapError stub)
    - pkg/controlplane/store/users.go
    - pkg/controlplane/store/groups.go
    - pkg/controlplane/store/shares.go
    - pkg/controlplane/store/adapters.go
    - pkg/controlplane/store/metadata_stores.go
    - pkg/controlplane/store/payload_stores.go
    - pkg/controlplane/store/settings.go
    - pkg/controlplane/store/identity_mappings.go
    - pkg/controlplane/store/netgroups.go
    - pkg/apiclient/users.go
    - pkg/apiclient/groups.go
    - pkg/apiclient/adapters.go
    - pkg/apiclient/stores.go
    - pkg/apiclient/settings.go
    - pkg/apiclient/shares.go
    - pkg/apiclient/netgroups.go
    - pkg/apiclient/identity_mappings.go
    - pkg/apiclient/clients.go

key-decisions:
  - "MapError stub on BaseAdapter rather than NFS/SMB — both embed BaseAdapter so they inherit the stub; override later"
  - "createWithID accepts currentID and idSetter callback rather than interface constraint — avoids forcing SetID on all models"
  - "API client helpers return value types (not pointers to slices) for listResources to match existing return signatures"

patterns-established:
  - "Generic GORM helper pattern: getByField[T](db, ctx, field, value, notFoundErr, preloads...) for all single-record lookups"
  - "Generic API client helper pattern: getResource[T](c, path) / listResources[T](c, path) for all REST resource access"
  - "PayloadError wrapping pattern: NewPayloadError(op, share, payloadID, blockIdx, backend, err) with Unwrap() for errors.Is()"

requirements-completed: [REF-06.1, REF-06.2, REF-06.3, REF-06.5]

# Metrics
duration: 15min
completed: 2026-02-26
---

# Phase 29 Plan 01: Foundational Error Types and Generic Helpers Summary

**PayloadError/ProtocolError types with generic GORM and API client helpers eliminating 185 lines of CRUD boilerplate**

## Performance

- **Duration:** 15 min
- **Started:** 2026-02-26T09:42:55Z
- **Completed:** 2026-02-26T09:57:55Z
- **Tasks:** 2
- **Files modified:** 24

## Accomplishments
- PayloadError struct wraps existing sentinel errors with Op/Share/PayloadID/BlockIdx/Backend context while preserving errors.Is() via Unwrap()
- ProtocolError interface defined in pkg/adapter/ with Code()/Message()/Unwrap(), MapError added to Adapter interface
- Generic GORM helpers (getByField, listAll, createWithID, deleteByField) reduce ~30 repetitive get/list/create/delete patterns across 8 store files
- Generic API client helpers (getResource, listResources, createResource, updateResource, deleteResource) reduce boilerplate across 9 client files
- Net reduction: 185 lines removed (262 added including new helper files, 447 removed from refactored files)

## Task Commits

Each task was committed atomically:

1. **Task 1: Create PayloadError and ProtocolError types** - `33b45379` (feat)
2. **Task 2: Create generic GORM helpers and refactor store methods** - `e2629ab0` (refactor)

## Files Created/Modified
- `pkg/payload/errors.go` - PayloadError struct with Op/Share/PayloadID/BlockIdx/Size/Duration/Retries/Backend/Err fields
- `pkg/adapter/errors.go` - ProtocolError interface with Code()/Message()/Unwrap()
- `pkg/adapter/adapter.go` - MapError(err) ProtocolError added to Adapter interface
- `pkg/adapter/base.go` - Stub MapError returning nil on BaseAdapter
- `pkg/controlplane/store/helpers.go` - Generic GORM helpers: getByField[T], listAll[T], createWithID[T], deleteByField[T]
- `pkg/apiclient/helpers.go` - Generic API client helpers: getResource[T], listResources[T], createResource[T], updateResource[T], deleteResource, resourcePath
- 8 store files refactored to use generic helpers (users, groups, shares, adapters, metadata_stores, payload_stores, settings, identity_mappings, netgroups)
- 9 API client files refactored to use generic helpers (users, groups, adapters, stores, settings, shares, netgroups, identity_mappings, clients)

## Decisions Made
- MapError stub placed on BaseAdapter rather than individually on NFS/SMB adapters, since both embed *BaseAdapter
- createWithID helper accepts currentID parameter and idSetter callback instead of requiring a SetID interface on models
- API client listResources returns []T (value slice) matching existing method signatures, while GORM listAll returns []*T (pointer slice) matching existing store patterns

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All foundational types and helpers in place for subsequent plans
- PayloadError ready for use in payload/offloader decomposition (Plan 04-05)
- ProtocolError ready for NFS/SMB adapter implementations (Plan 06-07)
- Generic GORM helpers ready for ControlPlane Store interface decomposition (Plan 02)

---
*Phase: 29-core-layer-decomposition*
*Completed: 2026-02-26*
