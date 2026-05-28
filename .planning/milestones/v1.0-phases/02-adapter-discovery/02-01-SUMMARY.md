---
phase: 02-adapter-discovery
plan: 01
subsystem: infra
tags: [kubernetes, operator, polling, adapter-discovery, controller-runtime]

# Dependency graph
requires:
  - phase: 01-auth-foundation
    provides: DittoFSClient, operator credentials Secret, Authenticated condition
provides:
  - AdapterDiscoverySpec CRD field with configurable PollingInterval
  - DittoFSClient.ListAdapters() method returning []AdapterInfo
  - adapter_reconciler.go with DISC-03 safety (preserve state on error)
  - mergeRequeueAfter for minimum-RequeueAfter across sub-reconcilers
  - getLastKnownAdapters() for Phase 3 Service reconciliation consumption
affects: [03-service-reconciler, adapter-lifecycle]

# Tech tracking
tech-stack:
  added: []
  patterns: [sub-reconciler pattern with safety guards, min-RequeueAfter merge, mutex-protected in-memory state]

key-files:
  created:
    - k8s/dittofs-operator/internal/controller/adapter_reconciler.go
    - k8s/dittofs-operator/internal/controller/adapter_reconciler_test.go
  modified:
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_types_builder.go
    - k8s/dittofs-operator/internal/controller/dittofs_client.go
    - k8s/dittofs-operator/internal/controller/dittoserver_controller.go

key-decisions:
  - "AdapterInfo uses minimal subset struct (type, enabled, running, port) -- Go JSON decoder ignores unknown fields"
  - "Polling interval read fresh from spec on every reconcile, never cached"
  - "Empty adapter list stored as valid state (not nil) to distinguish from 'never polled'"
  - "Re-fetch DittoServer after auth reconciliation to get updated Authenticated condition"

patterns-established:
  - "Sub-reconciler safety: on API error, preserve existing state and log at Info level"
  - "mergeRequeueAfter: minimum positive RequeueAfter from all sub-reconcilers drives cadence"
  - "Mutex-protected in-memory cache keyed by namespace/name for cross-reconcile state"

# Metrics
duration: 4min
completed: 2026-02-10
---

# Phase 02 Plan 01: Adapter Discovery Summary

**Adapter discovery polling with CRD-configurable interval, DISC-03 safety guards, and min-RequeueAfter merge across sub-reconcilers**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-10T20:42:05Z
- **Completed:** 2026-02-10T20:46:28Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- AdapterDiscoverySpec CRD field with PollingInterval defaulting to 30s
- DittoFSClient.ListAdapters() returning minimal AdapterInfo subset
- adapter_reconciler.go implementing DISC-03 safety: API errors never delete or modify existing adapter state
- Adapter polling gated behind Authenticated condition check with re-fetch after auth
- mergeRequeueAfter logic ensuring fastest sub-reconciler drives reconcile cadence
- 12 tests covering success, API error preservation, empty response, no credentials, custom intervals, and merge logic

## Task Commits

Each task was committed atomically:

1. **Task 1: Add CRD spec field, builder option, and ListAdapters to DittoFSClient** - `9c7d612` (feat)
2. **Task 2: Implement adapter reconciler with safety guards, integrate into Reconcile loop, and add tests** - `aa3a27d` (feat)

## Files Created/Modified
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` - Added AdapterDiscoverySpec struct and field on DittoServerSpec
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types_builder.go` - Added WithAdapterDiscovery builder option
- `k8s/dittofs-operator/internal/controller/dittofs_client.go` - Added AdapterInfo type and ListAdapters method
- `k8s/dittofs-operator/internal/controller/adapter_reconciler.go` - Core adapter polling logic with safety guards
- `k8s/dittofs-operator/internal/controller/adapter_reconciler_test.go` - 12 tests for adapter reconciler
- `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` - Integrated adapter polling, added mergeRequeueAfter, adaptersMu/lastKnownAdapters fields

## Decisions Made
- AdapterInfo uses minimal 4-field subset struct (type, enabled, running, port) -- Go JSON silently ignores extra API fields
- Polling interval read fresh from CRD spec every reconcile (never cached), supporting runtime changes
- Empty adapter list stored as valid state (empty slice, not nil) to distinguish from "never polled"
- Re-fetch DittoServer after auth reconciliation to get updated Authenticated condition before adapter check

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- getLastKnownAdapters() is ready for Phase 3 Service reconciler consumption
- Adapter state is safely populated and preserved across errors
- mergeRequeueAfter ensures Phase 3 sub-reconcilers can participate in cadence

---
*Phase: 02-adapter-discovery*
*Completed: 2026-02-10*
