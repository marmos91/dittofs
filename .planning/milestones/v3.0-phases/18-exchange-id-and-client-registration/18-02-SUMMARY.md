---
phase: 18-exchange-id-and-client-registration
plan: 02
subsystem: api
tags: [rest-api, nfsv4, cli, cobra, chi-router, client-management]

# Dependency graph
requires:
  - phase: 18-01
    provides: "StateManager with ListV40Clients, ListV41Clients, EvictV41Client, ServerInfo"
provides:
  - "GET /api/v1/clients REST endpoint for unified v4.0+v4.1 client listing"
  - "DELETE /api/v1/clients/{id} REST endpoint for client eviction"
  - "/health server identity info (server_owner, server_impl, server_scope)"
  - "dfsctl client list and dfsctl client evict CLI commands"
  - "pkg/apiclient ListClients and EvictClient methods"
  - "Runtime.SetNFSClientProvider/NFSClientProvider for cross-package access"
affects: [19-create-session, 20-sequence-compound]

# Tech tracking
tech-stack:
  added: []
  patterns: ["any-typed provider for cross-package state access (pkg/ -> internal/)"]

key-files:
  created:
    - internal/controlplane/api/handlers/clients.go
    - pkg/apiclient/clients.go
    - cmd/dfsctl/commands/client/client.go
    - cmd/dfsctl/commands/client/list.go
    - cmd/dfsctl/commands/client/evict.go
  modified:
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/api/router.go
    - internal/controlplane/api/handlers/health.go
    - internal/protocol/nfs/v4/state/v41_client.go
    - pkg/adapter/nfs/nfs_adapter.go
    - cmd/dfsctl/commands/root.go

key-decisions:
  - "NFSClientProvider stored as any on Runtime to avoid import cycle between pkg/ and internal/"
  - "Type assertion done in handlers package (internal/) which can import state package"
  - "EvictV40Client performs full cleanup (open states, lock states, delegations, lease timer)"
  - "Client routes only registered if NFSClientProvider is non-nil (graceful degradation)"

patterns-established:
  - "any-typed provider pattern: Runtime stores cross-boundary references as any, handlers type-assert"
  - "NewXHandlerFromProvider: handler constructor accepting any for router use from pkg/"

requirements-completed: [SESS-01, TRUNK-02]

# Metrics
duration: 8min
completed: 2026-02-20
---

# Phase 18 Plan 02: Client REST API and CLI Summary

**REST API endpoints for NFS client visibility (list/evict) with server identity on /health, plus dfsctl client commands**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-20T22:29:31Z
- **Completed:** 2026-02-20T22:37:49Z
- **Tasks:** 2
- **Files modified:** 12

## Accomplishments
- GET /api/v1/clients returns unified list of v4.0 and v4.1 clients with rich fields (client_id, address, nfs_version, lease_status, confirmed, impl_name)
- DELETE /api/v1/clients/{id} evicts clients by hex ID (tries v4.1 first, then v4.0) with full state cleanup
- GET /health includes server_owner, server_impl, and server_scope when NFS adapter is active
- dfsctl client list renders table with CLIENT_ID, VERSION, ADDRESS, LEASE, CONFIRMED, IMPL_NAME columns
- dfsctl client evict prompts for confirmation (supports --force flag)
- No import cycles between pkg/ and internal/ packages

## Task Commits

Each task was committed atomically:

1. **Task 1: REST API client endpoints and /health server info** - `bdeb8964` (feat)
2. **Task 2: dfsctl client commands and apiclient methods** - `5ac850d0` (feat)

## Files Created/Modified
- `internal/controlplane/api/handlers/clients.go` - ClientHandler with List, Evict methods and server identity helpers
- `pkg/apiclient/clients.go` - ListClients() and EvictClient() API client methods
- `cmd/dfsctl/commands/client/client.go` - Parent client Cobra command
- `cmd/dfsctl/commands/client/list.go` - dfsctl client list with TableRenderer
- `cmd/dfsctl/commands/client/evict.go` - dfsctl client evict with confirmation
- `pkg/controlplane/runtime/runtime.go` - NFSClientProvider getter/setter (any type)
- `pkg/controlplane/api/router.go` - /clients route registration with nil guard
- `internal/controlplane/api/handlers/health.go` - Server identity info on /health
- `internal/protocol/nfs/v4/state/v41_client.go` - EvictV40Client method
- `pkg/adapter/nfs/nfs_adapter.go` - Wire StateManager to Runtime via SetNFSClientProvider
- `cmd/dfsctl/commands/root.go` - Register client command

## Decisions Made
- Used `any` type on Runtime for NFSClientProvider to avoid import cycles (pkg/ cannot import internal/). Handlers do type assertion since they are in internal/.
- EvictV40Client performs comprehensive cleanup: stops lease timer, removes open states, lock states, lock owners, delegations, and client maps -- matching the existing onLeaseExpired cleanup pattern.
- Client routes wrapped in nil guard so they are only registered when NFS adapter is configured, preventing nil pointer panics.
- Router nil-checks Runtime before calling NFSClientProvider() to handle test scenarios where Runtime is nil.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed nil Runtime panic in router test**
- **Found during:** Task 2 (verification)
- **Issue:** TestAPIServer_Lifecycle passes nil Runtime, causing panic on rt.NFSClientProvider()
- **Fix:** Added nil guard for Runtime before calling NFSClientProvider() in router
- **Files modified:** pkg/controlplane/api/router.go
- **Verification:** All tests pass including TestAPIServer_Lifecycle
- **Committed in:** 5ac850d0 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Essential nil guard for test compatibility. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 18 complete: EXCHANGE_ID handler and client management endpoints all operational
- Ready for Phase 19 (CREATE_SESSION) which will use the v4.1 client records established here
- ServerIdentity exposed on /health enables trunking verification (TRUNK-02)

---
*Phase: 18-exchange-id-and-client-registration*
*Completed: 2026-02-20*
