---
phase: 28-smb-adapter-restructuring
plan: 02
subsystem: adapter
tags: [base-adapter, tcp-lifecycle, refactoring, connection-factory]

# Dependency graph
requires:
  - phase: 28-smb-adapter-restructuring
    provides: Renamed SMB adapter types matching NFS conventions (Plan 01)
  - phase: 27-nfs-adapter-restructuring
    provides: NFS adapter structure to align with for shared extraction
provides:
  - BaseAdapter struct in pkg/adapter/base.go with shared TCP lifecycle
  - ConnectionFactory and ConnectionHandler interfaces
  - MetricsRecorder interface for optional per-protocol metrics
  - Both NFS and SMB adapters embedding BaseAdapter
affects: [28-03, 28-04, 28-05]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "BaseAdapter embedded struct pattern for shared TCP lifecycle across protocol adapters"
    - "ConnectionFactory interface enables per-protocol connection creation in shared accept loop"
    - "PreAccept hook for protocol-specific connection acceptance checks (live settings)"
    - "OnConnectionClose callback for protocol-specific cleanup in accept goroutine"
    - "nfsMetricsRecorder adapter type bridges NFS metrics to BaseAdapter MetricsRecorder"

key-files:
  created:
    - pkg/adapter/base.go
  modified:
    - pkg/adapter/nfs/adapter.go
    - pkg/adapter/nfs/shutdown.go
    - pkg/adapter/nfs/connection.go
    - pkg/adapter/nfs/dispatch.go
    - pkg/adapter/nfs/handlers.go
    - pkg/adapter/nfs/nlm.go
    - pkg/adapter/smb/adapter.go
    - pkg/adapter/smb/connection.go

key-decisions:
  - "BaseAdapter uses pointer embedding (*adapter.BaseAdapter) to avoid go vet warnings about copying sync primitives"
  - "NFS Serve() does protocol-specific startup (portmapper, NSM, v4.1 reaper) before delegating to ServeWithFactory"
  - "NFS onConnectionClose callback left as no-op; v4 backchannel cleanup handled in connection-level defer"
  - "NFS metrics bridged via nfsMetricsRecorder adapter to satisfy generic MetricsRecorder interface"
  - "Shared fields promoted from BaseAdapter: Registry, Shutdown, ShutdownCtx, CancelRequests, ConnCount, etc."

patterns-established:
  - "Protocol adapters embed *adapter.BaseAdapter and override SetRuntime, Serve, Stop"
  - "Protocol adapters implement adapter.ConnectionFactory to create connections"
  - "PreAccept hooks handle live settings max_connections per-protocol"

requirements-completed: [REF-04]

# Metrics
duration: 8min
completed: 2026-02-25
---

# Phase 28 Plan 02: BaseAdapter Extraction Summary

**Extracted shared TCP lifecycle (accept loop, graceful shutdown, force-close, connection tracking, semaphore) into BaseAdapter embedded struct, eliminating ~760 lines of duplicated code across NFS and SMB adapters**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-25T20:45:17Z
- **Completed:** 2026-02-25T20:53:17Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Created pkg/adapter/base.go with BaseAdapter, ConnectionFactory, ConnectionHandler, MetricsRecorder, BaseConfig
- Refactored NFS adapter to embed *adapter.BaseAdapter, removing all duplicated lifecycle code
- Refactored SMB adapter to embed *adapter.BaseAdapter, removing all duplicated lifecycle code
- NFS shutdown.go reduced from 262 lines to 42 lines (NFS-specific cleanup only)
- Net change: +162 lines added, -928 lines removed (766 lines eliminated)

## Task Commits

Each task was committed atomically:

1. **Task 1: Create BaseAdapter with shared lifecycle** - `67be2601` (feat)
2. **Task 2: Refactor both adapters to embed BaseAdapter** - `7558c60a` (refactor)

## Files Created/Modified
- `pkg/adapter/base.go` - BaseAdapter struct with shared TCP lifecycle, interfaces, accept loop
- `pkg/adapter/nfs/adapter.go` - NFSAdapter embeds *adapter.BaseAdapter, Serve delegates to ServeWithFactory
- `pkg/adapter/nfs/shutdown.go` - Reduced to NFS-specific cleanup (portmapper, GSS, Kerberos) + BaseAdapter.Stop()
- `pkg/adapter/nfs/connection.go` - Updated field access (shutdown -> Shutdown)
- `pkg/adapter/nfs/dispatch.go` - Updated field access (registry -> Registry)
- `pkg/adapter/nfs/handlers.go` - Updated field access (registry -> Registry)
- `pkg/adapter/nfs/nlm.go` - Updated field access (registry -> Registry, shutdownCtx -> ShutdownCtx)
- `pkg/adapter/smb/adapter.go` - Adapter embeds *adapter.BaseAdapter, Serve delegates to ServeWithFactory
- `pkg/adapter/smb/connection.go` - Updated field access (shutdown -> Shutdown, registry -> Registry)

## Decisions Made
- Used pointer embedding (*adapter.BaseAdapter) instead of value embedding to satisfy go vet (sync primitives cannot be copied)
- NFS onConnectionClose left as no-op because v4 backchannel unbinding needs connection ID which is not available from the address-based callback -- cleanup remains in connection-level defer
- NFS-specific startup (portmapper, NSM, v4.1 session reaper) happens in NFS Serve() before delegating to ServeWithFactory, keeping protocol-specific concerns out of BaseAdapter
- nfsMetricsRecorder adapter type bridges the NFS-specific metrics.NFSMetrics interface to BaseAdapter's generic MetricsRecorder interface

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed go vet sync.WaitGroup copy warning**
- **Found during:** Task 2
- **Issue:** BaseAdapter returned by value from NewBaseAdapter contained sync.WaitGroup, sync.Once, sync.Map, sync.RWMutex -- go vet flagged literal copy
- **Fix:** Changed NewBaseAdapter to return *BaseAdapter and switched both adapters to pointer embedding (*adapter.BaseAdapter)
- **Files modified:** pkg/adapter/base.go, pkg/adapter/nfs/adapter.go, pkg/adapter/smb/adapter.go
- **Verification:** go vet ./... passes cleanly
- **Committed in:** 7558c60a (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Auto-fix necessary for correctness. No scope creep.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- BaseAdapter extraction complete, both adapters delegate lifecycle to shared code
- Single place for shutdown bugs going forward
- Ready for Plan 03 (connection slimming / dispatch extraction)
- ConnectionFactory pattern proven working with both protocols

---
*Phase: 28-smb-adapter-restructuring*
*Completed: 2026-02-25*

## Self-Check: PASSED

All 5 key files verified present. Both commit hashes (67be2601, 7558c60a) verified in git log. BaseAdapter struct confirmed in base.go. Both NFS and SMB adapters confirmed embedding *adapter.BaseAdapter. ConnectionFactory interface confirmed present.
