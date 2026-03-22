---
phase: 71-operational-visibility
plan: 01
subsystem: runtime
tags: [client-tracking, registry, ttl, runtime-service]

# Dependency graph
requires: []
provides:
  - "ClientRecord data model with NFS/SMB protocol detail sub-structs"
  - "ClientRegistry runtime sub-service with Register/Deregister/Get/List/filter/sweep"
  - "Runtime.Clients() accessor for adapter integration"
affects: [71-02-PLAN, client-api, dfsctl-client-list]

# Tech tracking
tech-stack:
  added: []
  patterns: [runtime-sub-service, copy-on-read, ttl-sweeper]

key-files:
  created:
    - pkg/controlplane/runtime/clients/service.go
    - pkg/controlplane/runtime/clients/service_test.go
    - pkg/controlplane/runtime/clients.go
  modified:
    - pkg/controlplane/runtime/runtime.go

key-decisions:
  - "Default TTL 5 min for stale client cleanup"
  - "Sweep interval = TTL/2 for responsive cleanup without excessive overhead"
  - "Deep copy of Shares slice and protocol detail structs for copy-on-read safety"

patterns-established:
  - "ClientRegistry sub-service: same pattern as MountTracker (RWMutex, map, copy-on-read, filter func)"
  - "Type aliases in clients.go for backward-compat re-export"

requirements-completed: [CLIENT-01, CLIENT-04]

# Metrics
duration: 3min
completed: 2026-03-22
---

# Phase 71 Plan 01: ClientRecord Model and Registry Summary

**Thread-safe ClientRegistry sub-service with NFS/SMB client tracking, TTL-based stale cleanup, and protocol/share filtering**

## Performance

- **Duration:** 3 min
- **Started:** 2026-03-22T21:25:54Z
- **Completed:** 2026-03-22T21:29:03Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- ClientRecord model with NfsDetails (version, auth, UID/GID) and SmbDetails (session, dialect, signing, encryption) sub-structs
- Registry service with Register/Deregister/Get/List/ListByProtocol/ListByShare/UpdateActivity/AddShare/RemoveShare
- Background TTL sweeper with configurable interval that cleans stale clients
- Copy-on-read for thread-safe snapshot access (deep copies Shares slice and protocol details)
- ClientRegistry wired into Runtime with Clients() accessor and sweeper auto-start in Serve()

## Task Commits

Each task was committed atomically:

1. **Task 1: Create ClientRecord model and Registry service with TTL sweeper** - `cc480823` (feat)
2. **Task 2: Wire ClientRegistry into Runtime as sub-service** - `267caff9` (feat)

## Files Created/Modified
- `pkg/controlplane/runtime/clients/service.go` - ClientRecord model, Registry service with TTL sweeper
- `pkg/controlplane/runtime/clients/service_test.go` - 13 unit tests covering all behaviors
- `pkg/controlplane/runtime/clients.go` - Type aliases for backward-compat re-export
- `pkg/controlplane/runtime/runtime.go` - clientRegistry field, initialization, Clients() accessor, sweeper start

## Decisions Made
- Default TTL of 5 minutes for stale client cleanup, matching typical NFS/SMB session timeouts
- Sweep interval at TTL/2 for responsive cleanup without excessive lock contention
- Deep copy of Shares slice and protocol detail structs ensures copy-on-read safety
- Followed MountTracker sub-service pattern exactly for consistency

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- ClientRegistry is ready for NFS/SMB adapter integration (Plan 02)
- Runtime.Clients() accessor available for API handlers and CLI commands
- Sweeper automatically starts with server context

---
*Phase: 71-operational-visibility*
*Completed: 2026-03-22*
