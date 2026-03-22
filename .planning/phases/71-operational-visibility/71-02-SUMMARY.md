---
phase: 71-operational-visibility
plan: 02
subsystem: adapter, api, cli
tags: [client-tracking, disconnect, api, cli, nfs, smb]

# Dependency graph
requires: [71-01]
provides:
  - "NFS adapter registers/deregisters clients on connect/disconnect"
  - "SMB adapter registers/deregisters clients on session setup/cleanup"
  - "DisconnectClient with protocol-specific TCP teardown"
  - "Unified GET /api/v1/clients with protocol/share filters"
  - "DELETE /api/v1/clients/{id} with real protocol teardown"
  - "dfsctl client list with --protocol and --share flags"
  - "dfsctl client disconnect with confirmation prompt"
affects: [operational-visibility, admin-tooling]

# Tech tracking
tech-stack:
  added: []
  patterns: [force-close-by-address, unified-endpoint, backward-compat-alias]

key-files:
  created:
    - internal/controlplane/api/handlers/nfs_clients.go
  modified:
    - pkg/adapter/base.go
    - pkg/adapter/nfs/connection.go
    - pkg/adapter/smb/connection.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/adapters/service.go
    - internal/controlplane/api/handlers/clients.go
    - pkg/controlplane/api/router.go
    - pkg/apiclient/clients.go
    - cmd/dfsctl/commands/client/client.go
    - cmd/dfsctl/commands/client/list.go
    - cmd/dfsctl/commands/client/evict.go

key-decisions:
  - "Local clientDisconnecter interface in adapters package to avoid import cycle"
  - "ForceCloseByAddress on BaseAdapter leverages existing ActiveConnections sync.Map"
  - "NFS-specific session handlers split to nfs_clients.go, kept under /adapters/nfs/"
  - "ClientInfo type alias and EvictClient method kept for backward compat"

patterns-established:
  - "Unified API endpoint pattern: protocol-agnostic at /api/v1/clients, protocol-specific at /api/v1/adapters/{type}/"

requirements-completed: [CLIENT-02, CLIENT-03]

# Metrics
duration: 8min
completed: 2026-03-22
---

# Phase 71 Plan 02: Adapter Integration, API, and CLI Summary

**NFS/SMB adapter registration hooks, unified REST API with protocol-specific disconnect, and CLI commands with filtering**

## Performance

- **Duration:** 8 min
- **Started:** 2026-03-22T21:31:42Z
- **Completed:** 2026-03-22T21:39:53Z
- **Tasks:** 3
- **Files modified:** 11

## Accomplishments
- NFS adapter registers ClientRecord on connection accept, deregisters on close, updates activity per request
- SMB adapter registers ClientRecord on SESSION_SETUP, deregisters on LOGOFF and connection cleanup
- ForceCloseByAddress on BaseAdapter closes TCP by address, triggering existing cleanup chains
- DisconnectClient on Runtime bridges registry lookup + adapter TCP close for real protocol teardown
- ForceCloseClientConnection on adapters.Service routes disconnect to correct protocol adapter
- Unified ClientHandler replaces NFS-only handler with List (GET) and Disconnect (DELETE)
- Query filters: ?protocol=nfs|smb and ?share=/export
- NFS-specific session handlers preserved under /adapters/nfs/clients/{id}/sessions
- apiclient updated: ClientRecord model, ListClients with options, DisconnectClient
- CLI list shows PROTOCOL, ADDRESS, USER, SHARES, CONNECTED columns with --protocol and --share flags
- CLI disconnect command with confirmation prompt and --force flag (evict kept as alias)

## Task Commits

Each task was committed atomically:

1. **Task 1: Integrate NFS/SMB adapters with ClientRegistry and add protocol-specific disconnect** - `b2ddd456` (feat)
2. **Task 2: Rewrite REST API handler and apiclient for unified client endpoint** - `9bc036e0` (feat)
3. **Task 3: Update CLI commands for unified client management** - `83114053` (feat)

## Files Created/Modified
- `pkg/adapter/base.go` - ForceCloseByAddress for protocol-specific TCP teardown
- `pkg/adapter/nfs/connection.go` - Register/Deregister/UpdateActivity hooks
- `pkg/adapter/smb/connection.go` - Register/Deregister hooks on session lifecycle
- `pkg/controlplane/runtime/runtime.go` - DisconnectClient method
- `pkg/controlplane/runtime/adapters/service.go` - ForceCloseClientConnection with local interface
- `internal/controlplane/api/handlers/clients.go` - Rewritten unified ClientHandler
- `internal/controlplane/api/handlers/nfs_clients.go` - NFS-specific session/identity handlers
- `pkg/controlplane/api/router.go` - Unified /clients route, NFS sessions under /adapters/nfs/
- `pkg/apiclient/clients.go` - ClientRecord model, ListClients options, DisconnectClient
- `cmd/dfsctl/commands/client/client.go` - Updated root command
- `cmd/dfsctl/commands/client/list.go` - Unified table with protocol/share filters
- `cmd/dfsctl/commands/client/evict.go` - Renamed to disconnect with evict alias

## Decisions Made
- Local `clientDisconnecter` interface defined in adapters package to avoid import cycle between pkg/adapter and pkg/controlplane/runtime
- ForceCloseByAddress leverages existing ActiveConnections sync.Map rather than adding new tracking
- NFS-specific session handlers split to separate file rather than deleted (still useful for deep NFS inspection)
- Backward compatibility aliases (ClientInfo type, EvictClient method) kept for external consumers

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Import cycle between pkg/adapter and pkg/controlplane/runtime**
- **Found during:** Task 1
- **Issue:** ClientDisconnecter interface defined in pkg/adapter created import cycle since it's imported by runtime/adapters which also imports pkg/adapter
- **Fix:** Defined local clientDisconnecter interface in runtime/adapters package instead
- **Files modified:** pkg/controlplane/runtime/adapters/service.go

**2. [Rule 3 - Blocking] CLI compile failure due to removed ClientInfo type**
- **Found during:** Task 2
- **Issue:** Removing old ClientInfo and EvictClient from apiclient broke CLI compilation, preventing Task 2 build verification
- **Fix:** Added ClientInfo type alias and EvictClient compat method; also updated CLI list.go in Task 2 to compile with new model
- **Files modified:** pkg/apiclient/clients.go, cmd/dfsctl/commands/client/list.go

## Issues Encountered

None beyond the auto-fixed deviations above.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Full end-to-end client tracking operational: adapter -> registry -> API -> CLI
- Admin disconnect performs real protocol teardown via TCP close
- Phase 71 operational visibility complete

---
*Phase: 71-operational-visibility*
*Completed: 2026-03-22*
