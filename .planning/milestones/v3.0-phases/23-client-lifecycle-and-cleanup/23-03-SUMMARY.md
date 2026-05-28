---
phase: 23-client-lifecycle-and-cleanup
plan: 03
subsystem: api
tags: [nfs, grace-period, rest-api, cli, cobra]

# Dependency graph
requires:
  - phase: 23-01
    provides: GraceStatusInfo, GraceStatus(), ForceEndGrace() on StateManager
provides:
  - GET /api/v1/grace endpoint (unauthenticated grace period status)
  - POST /api/v1/grace/end endpoint (admin-only force-end)
  - Health readiness endpoint enriched with grace_period info
  - dfs status grace countdown display
  - dfsctl grace status command (table/JSON/YAML)
  - dfsctl grace end command
  - API client methods for grace period management
affects: [testing, monitoring, operator]

# Tech tracking
tech-stack:
  added: []
  patterns: [unauthenticated-status-endpoint, admin-force-action-pattern]

key-files:
  created:
    - internal/controlplane/api/handlers/grace.go
    - pkg/apiclient/grace.go
    - cmd/dfsctl/commands/grace/grace.go
    - cmd/dfsctl/commands/grace/status.go
    - cmd/dfsctl/commands/grace/end.go
  modified:
    - internal/controlplane/api/handlers/health.go
    - pkg/controlplane/api/router.go
    - cmd/dfs/commands/status.go
    - cmd/dfsctl/commands/root.go

key-decisions:
  - "Grace status endpoint unauthenticated (like health probes) for K8s and monitoring tool access"
  - "Force-end endpoint requires admin auth (consistent with client evict pattern)"
  - "Grace period info only shown in dfs status when active (clean output by default)"

patterns-established:
  - "Unauthenticated status endpoint pattern: register outside protected group, same level as /health"
  - "Admin force-action pattern: POST endpoint in protected group with RequireAdmin middleware"

requirements-completed: [LIFE-02]

# Metrics
duration: 6min
completed: 2026-02-22
---

# Phase 23 Plan 03: Grace Period API and CLI Summary

**Grace period REST API with GET /api/v1/grace (unauthenticated), POST /api/v1/grace/end (admin), enriched health/status outputs, and dfsctl grace CLI commands**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-22T12:04:47Z
- **Completed:** 2026-02-22T12:10:44Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Grace period REST API with status endpoint (unauthenticated) and force-end endpoint (admin-only)
- Health readiness endpoint enriched with grace_period field when NFS adapter is configured
- dfs status shows yellow grace countdown when grace period is active
- dfsctl grace status and grace end CLI commands with full output format support
- API client methods for both grace endpoints

## Task Commits

Each task was committed atomically:

1. **Task 1: Grace period REST API endpoints and health endpoint enrichment** - `97507d22` (feat)
2. **Task 2: dfs status grace countdown and dfsctl grace CLI commands** - `89a00b65` (feat)

## Files Created/Modified
- `internal/controlplane/api/handlers/grace.go` - GraceHandler with Status and ForceEnd handlers
- `pkg/apiclient/grace.go` - GraceStatus and ForceEndGrace API client methods
- `internal/controlplane/api/handlers/health.go` - Enriched readiness with grace_period info
- `pkg/controlplane/api/router.go` - Registered grace routes (GET unauthenticated, POST admin)
- `cmd/dfs/commands/status.go` - Added GracePeriodInfo and grace countdown display
- `cmd/dfsctl/commands/grace/grace.go` - Parent grace command
- `cmd/dfsctl/commands/grace/status.go` - Grace status subcommand with table/JSON/YAML
- `cmd/dfsctl/commands/grace/end.go` - Grace end subcommand
- `cmd/dfsctl/commands/root.go` - Registered grace command

## Decisions Made
- Grace status endpoint registered as unauthenticated (same pattern as health probes) so K8s liveness/readiness probes and monitoring tools can access it
- Force-end endpoint placed inside the authenticated admin group (consistent with client evict pattern)
- Grace period info only displayed in dfs status table output when active (keeps output clean by default)
- GraceHandler uses NewGraceHandlerFromProvider pattern (same as ClientHandler) to avoid pkg/ -> internal/ import cycle

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 23 (Client Lifecycle and Cleanup) fully complete
- All 3 plans executed: client lifecycle ops, session-exempt ops, and grace period API/CLI
- Ready for Phase 24

---
*Phase: 23-client-lifecycle-and-cleanup*
*Completed: 2026-02-22*
