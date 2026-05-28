---
phase: 29-core-layer-decomposition
plan: 06
subsystem: runtime
tags: [refactoring, decomposition, sub-services, dependency-injection]

requires:
  - phase: 29-04
    provides: Store sub-interfaces for narrowed dependency injection
  - phase: 29-05
    provides: PayloadService I/O extraction pattern

provides:
  - 6 focused sub-packages under pkg/controlplane/runtime/
  - adapters.Service for protocol adapter lifecycle management
  - stores.Service for metadata store registry
  - shares.Service for share registration and configuration
  - mounts.Tracker for unified mount tracking
  - lifecycle.Service for server startup/shutdown orchestration
  - identity.Service for share-level identity mapping
  - Runtime reduced to ~500-line composition layer

affects: [runtime-consumers, adapter-integration, api-handlers]

tech-stack:
  added: []
  patterns:
    - "Sub-service composition: Runtime delegates to focused sub-packages via thin forwarding"
    - "Type aliases for backward compat: parent re-exports sub-package types (Share = shares.Share)"
    - "Interface-based dependency injection: sub-services accept narrow interfaces to avoid import cycles"
    - "Testing helpers: InjectShareForTesting bypasses full AddShare flow for unit tests"

key-files:
  created:
    - pkg/controlplane/runtime/adapters/service.go
    - pkg/controlplane/runtime/stores/service.go
    - pkg/controlplane/runtime/shares/service.go
    - pkg/controlplane/runtime/mounts/service.go
    - pkg/controlplane/runtime/lifecycle/service.go
    - pkg/controlplane/runtime/identity/service.go
  modified:
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/share.go
    - pkg/controlplane/runtime/mounts.go
    - pkg/controlplane/runtime/runtime_test.go
    - pkg/controlplane/runtime/netgroups.go

key-decisions:
  - "Share/ShareConfig types moved to shares/ sub-package with type aliases in parent for zero-change consumer migration"
  - "MountTracker renamed to mounts.Tracker in sub-package, parent re-exports as MountTracker alias"
  - "AuxiliaryServer interface defined in lifecycle/ sub-package (lifecycle owns server orchestration)"
  - "Identity mapping uses ShareProvider interface to avoid shares/ import cycle"
  - "Lifecycle.Serve accepts dependency interfaces (SettingsInitializer, AdapterLoader, MetadataFlusher, StoreCloser) rather than importing sibling sub-packages"
  - "adapters.RuntimeSetter uses any-typed runtime parameter to break import cycle with parent package"

patterns-established:
  - "Sub-service composition: god objects decomposed into focused sub-packages with thin forwarding in parent"
  - "Type alias re-export: sub-package owns type, parent creates type alias for backward compatibility"
  - "Interface-based DI: sub-services define narrow local interfaces rather than importing concrete types"

requirements-completed: [REF-05.3, REF-05.4]

duration: 10min
completed: 2026-02-26
---

# Phase 29 Plan 06: Runtime Decomposition Summary

**1203-line Runtime god object decomposed into 6 focused sub-services with 502-line composition layer**

## Performance

- **Duration:** 10 min
- **Started:** 2026-02-26T12:01:46Z
- **Completed:** 2026-02-26T12:12:00Z
- **Tasks:** 2
- **Files modified:** 20

## Accomplishments
- Extracted adapters, stores, shares sub-services (Task 1) with full backward compatibility
- Extracted mounts, lifecycle, identity sub-services (Task 2) completing the decomposition
- Runtime reduced from 1203 lines to 502 lines of pure composition and cross-cutting accessors
- All 20 existing tests pass without modification to test logic (only internal field references updated)
- Zero consumer changes required -- type aliases ensure runtime.Share, runtime.MountTracker etc. still work

## Task Commits

Each task was committed atomically:

1. **Task 1: Extract adapters, stores, shares sub-services** - `c5b4073a` (refactor)
2. **Task 2: Extract mounts, lifecycle, identity sub-services** - `8fb2d928` (refactor)

## Files Created/Modified

**Created:**
- `pkg/controlplane/runtime/adapters/service.go` - Protocol adapter lifecycle (352 lines)
- `pkg/controlplane/runtime/adapters/doc.go` - Package documentation
- `pkg/controlplane/runtime/stores/service.go` - Metadata store registry (111 lines)
- `pkg/controlplane/runtime/stores/doc.go` - Package documentation
- `pkg/controlplane/runtime/shares/service.go` - Share management (363 lines)
- `pkg/controlplane/runtime/shares/testing.go` - Test helper for share injection
- `pkg/controlplane/runtime/shares/doc.go` - Package documentation
- `pkg/controlplane/runtime/mounts/service.go` - Unified mount tracking (150 lines)
- `pkg/controlplane/runtime/mounts/doc.go` - Package documentation
- `pkg/controlplane/runtime/lifecycle/service.go` - Serve/shutdown orchestration (202 lines)
- `pkg/controlplane/runtime/lifecycle/doc.go` - Package documentation
- `pkg/controlplane/runtime/identity/service.go` - Identity mapping (96 lines)
- `pkg/controlplane/runtime/identity/doc.go` - Package documentation

**Modified:**
- `pkg/controlplane/runtime/runtime.go` - Reduced to 502-line composition layer
- `pkg/controlplane/runtime/share.go` - Replaced with type aliases to shares.Share
- `pkg/controlplane/runtime/mounts.go` - Replaced with type aliases to mounts.Tracker
- `pkg/controlplane/runtime/runtime_test.go` - Updated internal field references
- `pkg/controlplane/runtime/netgroups.go` - Updated to use sharesSvc.GetShare

## Decisions Made
- Share/ShareConfig types moved to shares/ sub-package with parent type aliases for zero-change migration
- MountTracker renamed to mounts.Tracker; parent NewMountTracker delegates to mounts.NewTracker
- AuxiliaryServer defined in lifecycle/ (lifecycle owns server orchestration)
- Identity mapping uses ShareProvider interface (avoids shares/ import)
- Lifecycle.Serve accepts narrow interfaces (SettingsInitializer, AdapterLoader, MetadataFlusher, StoreCloser) to avoid importing sibling sub-packages
- adapters.RuntimeSetter uses `any`-typed runtime to break import cycle with parent

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All 6 sub-services extracted and operational
- Runtime is a thin 502-line composition layer
- Ready for Phase 29-07 (final plan in core layer decomposition)
- No blockers or concerns

---
*Phase: 29-core-layer-decomposition*
*Completed: 2026-02-26*
