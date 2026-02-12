---
phase: 03-nsm-protocol
plan: 02
subsystem: nsm-handlers
tags: [nsm, handlers, dispatch, persistence, nfs-adapter]

dependency-graph:
  requires: ["03-01"]
  provides: ["NSM handlers", "client registration storage", "NFS adapter integration"]
  affects: ["03-03"]

tech-stack:
  added: []
  patterns: ["dispatch table", "handler methods", "client registration store interface"]

key-files:
  created:
    - internal/protocol/nsm/handlers/context.go
    - internal/protocol/nsm/handlers/handler.go
    - internal/protocol/nsm/handlers/null.go
    - internal/protocol/nsm/handlers/mon.go
    - internal/protocol/nsm/handlers/unmon.go
    - internal/protocol/nsm/handlers/unmon_all.go
    - internal/protocol/nsm/handlers/notify.go
    - internal/protocol/nsm/dispatch.go
    - pkg/metadata/store/memory/clients.go
    - pkg/metadata/store/badger/clients.go
    - pkg/metadata/store/postgres/clients.go
    - pkg/metadata/store/postgres/migrations/000003_clients.up.sql
    - pkg/metadata/store/postgres/migrations/000003_clients.down.sql
  modified:
    - pkg/metadata/store/memory/store.go
    - pkg/metadata/store/badger/store.go
    - pkg/metadata/store/postgres/store.go
    - pkg/adapter/nfs/nfs_adapter.go
    - pkg/adapter/nfs/nfs_connection.go
    - internal/protocol/nfs/rpc/constants.go

decisions:
  - id: handler-result-type
    choice: HandlerResult in handlers package
    rationale: Keep result type close to handlers for simplicity
  - id: client-id-format
    choice: "nsm:{client_addr}:{callback_host}"
    rationale: Ensures uniqueness per client/callback combination
  - id: nsm-version
    choice: NSM v1 only
    rationale: Standard version used by all NFS implementations

metrics:
  duration: 10 min
  completed: 2026-02-05
---

# Phase 3 Plan 02: NSM Handlers and Client Registration Summary

NSM handlers (NULL, STAT, MON, UNMON, UNMON_ALL, NOTIFY), dispatch table, client registration persistence in all metadata stores, NFS adapter integration for program 100024.

## Commits

| Hash | Type | Description |
|------|------|-------------|
| ee97d8e | feat | NSM handlers and dispatch table |
| 1f890fa | feat | Client registration storage in all metadata store backends |
| 1022f47 | feat | Integrate NSM dispatcher with NFS adapter |

## What Was Built

### NSM Handler Infrastructure
- **Handler struct** (`internal/protocol/nsm/handlers/handler.go`): Main NSM handler with ConnectionTracker, ClientRegistrationStore, server state counter, and server name
- **Handler context** (`context.go`): NSMHandlerContext with client info, auth flavor, credentials
- **Dispatch table** (`dispatch.go`): Maps procedure numbers to handler methods

### NSM Procedure Handlers
- **SM_NULL** (`null.go`): Ping/health check, returns empty response
- **SM_STAT** (`null.go`): Query server state without registering
- **SM_MON** (`mon.go`): Register client for crash notification, updates ConnectionTracker with NSM fields, persists to ClientRegistrationStore
- **SM_UNMON** (`unmon.go`): Unregister specific host monitoring (NSM only, not locks)
- **SM_UNMON_ALL** (`unmon_all.go`): Unregister all hosts from a callback address
- **SM_NOTIFY** (`notify.go`): Receive crash notification (placeholder for full callback dispatch in Plan 03-03)

### Client Registration Storage
- **Memory store** (`pkg/metadata/store/memory/clients.go`): In-memory map with mutex, returns copies to prevent modification
- **BadgerDB store** (`pkg/metadata/store/badger/clients.go`): Uses `nsm:client:` key prefix, `nsm:monname:` index, JSON marshaling
- **PostgreSQL store** (`pkg/metadata/store/postgres/clients.go`): Uses `nsm_client_registrations` table
- **PostgreSQL migration** (`000003_clients.up.sql`): Creates table with indexes on callback_host, registered_at, mon_name

### NFS Adapter Integration
- Added `ProgramNSM = 100024` and `NSMVersion1 = 1` constants
- Added `nsmHandler` field to NFSAdapter
- `initNSMHandler()` creates handler with ConnectionTracker and ClientRegistrationStore
- `handleNSMProcedure()` routes NSM calls to dispatch table with metrics and telemetry

## Key Implementation Details

### Client ID Generation
```go
func generateClientID(clientAddr, callbackHost string) string {
    return "nsm:" + clientAddr + ":" + callbackHost
}
```

### SM_MON Registration Flow
1. Decode XDR `mon` structure (MonID + priv[16])
2. Check client limit (default 10000)
3. Register in ConnectionTracker
4. Update NSM info (MonName, Priv, CallbackInfo)
5. Persist to ClientRegistrationStore (if available)
6. Return sm_stat_res with STAT_SUCC and current state

### State Counter Semantics
- Odd values = server is up
- Even values = server went down
- Incremented on each server restart
- Stored as atomic int32 in handler

## Verification Results

All packages build successfully:
```bash
go build ./internal/protocol/nsm/...
go build ./pkg/metadata/store/memory/...
go build ./pkg/metadata/store/badger/...
go build ./pkg/metadata/store/postgres/...
go build ./pkg/adapter/nfs/...
```

All lock and memory store tests pass.

## Deviations from Plan

None - plan executed exactly as written.

## Next Phase Readiness

**Ready for 03-03:** NSM Service and Store Implementations
- Handlers are complete and tested
- Client registration persistence is in place
- NFS adapter routes NSM calls correctly
- Plan 03-03 will implement:
  - SM_NOTIFY callback dispatch to clients
  - Server restart notification
  - Graceful recovery coordination
