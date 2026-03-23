# Phase 71: Operational Visibility - Context

**Gathered:** 2026-03-22
**Status:** Ready for planning

<domain>
## Phase Boundary

Protocol-agnostic client tracking — unified view of all connected NFS and SMB clients with connection metadata and automatic stale cleanup. Operators can see all connected clients in a single view via REST API and CLI. Includes admin disconnect capability.

</domain>

<decisions>
## Implementation Decisions

### Client Identity Model
- **D-01:** One ClientRecord per session/mount — NFS: one record per mount (v3) or per stateful client (v4). SMB: one record per authenticated session. A single IP with both NFS+SMB appears as 2+ records.
- **D-02:** ClientRecord is a runtime-only in-memory struct (not persisted in GORM). Lost on restart, rebuilt as clients reconnect. Matches transient nature of connections.

### Data Model Design
- **D-03:** Common fields as struct fields: ClientID (string), Protocol (string), Address (string), User (string), ConnectedAt (time.Time), LastActivity (time.Time), Shares ([]string).
- **D-04:** Protocol-specific details via typed sub-structs: `NfsDetails` and `SmbDetails` (not map[string]any). Enables json:"-" tags on sensitive fields (signing keys, preauth hash) and self-documenting API schema.

### Staleness and Cleanup
- **D-05:** Hybrid approach — TCP disconnect is the primary removal signal. When a connection closes cleanly, the client record is removed immediately.
- **D-06:** Background TTL sweeper (configurable, default 5 min) handles ghost entries from unclean disconnects (no FIN). Sweeper targets entries where TCP connection is dead AND LastActivity exceeds TTL.
- **D-07:** Idle NFS mounts stay listed as long as their TCP connection is open — inactivity alone does not trigger removal.

### API Design
- **D-08:** Replace existing `/api/v1/adapters/{type}/clients` with unified `GET /api/v1/clients`. Breaking change accepted — no backwards compatibility shim.
- **D-09:** Unified endpoint supports query filters: `?protocol=nfs|smb`, `?share=/export`.
- **D-10:** `DELETE /api/v1/clients/{id}` for admin disconnect — admin-role-only, enforced by API middleware.

### Disconnect Semantics
- **D-11:** `dfsctl client disconnect CLIENT_ID` supported — admin-only destructive operation with interactive confirmation prompt.
- **D-12:** Protocol-specific teardown: NFS = revoke state + close TCP; SMB = LOGOFF + close connection. Implementation delegates to adapter-specific cleanup.

### Code Organization
- **D-13:** ClientRegistry service in `pkg/controlplane/runtime/clients/service.go` — new runtime sub-service, same pattern as `mounts/service.go`.
- **D-14:** Adapters register/deregister via callback interface on Runtime: `RegisterClient(record)` / `DeregisterClient(clientID)`. Called in connection accept/close handlers. Minimal coupling.
- **D-15:** CLI commands in `cmd/dfsctl/commands/client/` — `list` (with --protocol, --share filters, table/JSON/YAML output) and `disconnect` (admin-only with confirmation).

### Claude's Discretion
- ClientID format (UUID vs hex-encoded counter vs composite key)
- Exact NfsDetails and SmbDetails field selection (which protocol-specific fields to expose)
- TTL sweeper implementation (timer-based vs ticker-based goroutine)
- CLI table column selection and formatting
- REST API response structure (envelope vs flat)

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Client tracking patterns
- `pkg/controlplane/runtime/mounts/service.go` — MountTracker pattern (RWMutex, composite keys, copy-on-read enumeration). The ClientRegistry should follow this established pattern.
- `internal/adapter/nfs/v4/state/` — NFS StateManager with ListV40Clients()/ListV41Clients() for client enumeration
- `internal/adapter/smb/session/manager.go` — SMB SessionManager with sync.Map, atomic ID generation, per-session tracking

### Existing client API (to be replaced)
- `internal/controlplane/api/handlers/clients.go` — Current NFS-only ClientHandler with ClientInfo/SessionInfo response types
- `pkg/controlplane/api/router.go` — Route registration pattern, existing client endpoints to replace

### Adapter connection lifecycle
- `pkg/adapter/nfs/connection.go` — NFS connection model, handleConnectionClose() cleanup
- `pkg/adapter/smb/connection.go` — SMB connection model, cleanupSessions() on disconnect
- `pkg/adapter/adapter.go` — BaseAdapter with ConnCount, connection accept loop

### CLI patterns
- `cmd/dfsctl/commands/user/list.go` — Standard list command pattern (TableRenderer, cmdutil.PrintOutput)
- `cmd/dfsctl/cmdutil/util.go` — GetAuthenticatedClient, PrintOutput, formatting helpers

### API patterns
- `internal/controlplane/api/handlers/` — Handler patterns, WriteJSONOK, error helpers
- `internal/controlplane/api/middleware/` — Auth middleware for role-based access control

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `mounts/service.go` MountTracker: Direct pattern for ClientRegistry (RWMutex, map, List/Record/Remove methods)
- `session/manager.go` SessionManager: SMB session data already tracked (username, domain, CreatedAt, credits)
- `state/` StateManager: NFS client data already tracked (ClientAddr, CreatedAt, LastRenewal, sessions)
- `handlers/clients.go` ClientHandler: Response types (ClientInfo, SessionInfo) can inform unified model
- `cmdutil/` package: PrintOutput with table/JSON/YAML, GetAuthenticatedClient for CLI

### Established Patterns
- Runtime sub-service pattern: mounts/, stores/, shares/, adapters/, lifecycle/, identity/ — ClientRegistry fits naturally as clients/
- Adapter callback pattern: Adapters call Runtime methods (e.g., mount Record/Remove) during lifecycle events
- sync.Map for high-churn concurrent tracking (used in BaseAdapter, SMB SessionManager)
- Copy-on-read for thread-safe enumeration (MountTracker.List() copies values under RLock)

### Integration Points
- `pkg/adapter/nfs/connection.go` — Add RegisterClient in NewNFSConnection, DeregisterClient in handleConnectionClose
- `pkg/adapter/smb/connection.go` — Add RegisterClient in session creation, DeregisterClient in cleanupSessions
- `pkg/controlplane/runtime/runtime.go` — Add ClientRegistry as new sub-service, wire in Serve()
- `pkg/controlplane/api/router.go` — Replace adapter-specific client routes with unified /api/v1/clients
- `internal/controlplane/api/handlers/clients.go` — Rewrite handler to use ClientRegistry instead of NFS StateManager directly

</code_context>

<specifics>
## Specific Ideas

No specific requirements — open to standard approaches following established codebase patterns.

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 71-operational-visibility*
*Context gathered: 2026-03-22*
