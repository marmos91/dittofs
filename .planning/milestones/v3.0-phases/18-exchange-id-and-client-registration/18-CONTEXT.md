# Phase 18: EXCHANGE_ID and Client Registration - Context

**Gathered:** 2026-02-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement the NFSv4.1 EXCHANGE_ID operation (op 42) so v4.1 clients can register with the server and receive a client ID for session creation. Includes v4.1 client record management, server identity reporting, and REST API/CLI for client visibility. CREATE_SESSION and actual session establishment are Phase 19.

</domain>

<decisions>
## Implementation Decisions

### Server Identity
- Server implementation name: `"dittofs"`
- Domain: `"dittofs.io"`
- Build date embedded via Go ldflags at compile time (standard Go pattern)
- Hardcoded defaults -- no config.yaml overrides for name/domain
- Log client implementation ID (name, domain, build date) at INFO level on EXCHANGE_ID
- Server scope: hostname-based (simple, no persistence required, changes naturally with server identity)

### Client Tracking
- Separate `V41ClientRecord` struct (not extending v4.0 `ClientRecord`) with shared lease behavior via embedding
- v4.0 and v4.1 client state isolation: Claude's discretion on whether to share owner lookup or keep completely independent (decide based on RFC 8881 guidance)
- In-memory only -- v4.1 client registrations do not survive server restart (clients re-register)
- REST API: expose client listing via `/clients` endpoint showing both v4.0 and v4.1 clients
- Rich client API fields: client ID, address, NFS version (v4.0/v4.1), connection time, implementation name/domain (v4.1), lease status, last renewal time
- Admin eviction: `DELETE /clients/{id}` to force-evict a client and clean up all its state
- `dfsctl client list` and `dfsctl client evict` CLI commands following existing table/JSON output pattern
- Server info (server_owner, server_scope, implementation ID) exposed on `/status` endpoint

### Trunking & server_owner
- major_id: Claude's discretion (hostname-based recommended for simplicity)
- minor_id: boot epoch timestamp (matches existing v4.0 client ID generation pattern)
- Flags: `EXCHGID4_FLAG_USE_NON_PNFS` only (DittoFS doesn't support pNFS)
- Phase 18 scope: report consistent server_owner for trunking detection only; actual multi-connection binding is Phase 21
- EXCHGID4_FLAG_CONFIRMED_R handling: Claude's discretion based on RFC correctness

### State Protection
- SP4_NONE only -- reject SP4_MACH_CRED and SP4_SSV with proper NFS4 error codes
- Bare minimum state_protect4_r response: just encode sp_how = SP4_NONE
- Validate SP4 support BEFORE allocating a client record (fail fast, no cleanup needed)

### Code Structure & Testing
- v4.1 client registration logic: methods on existing `StateManager` (ExchangeID(), etc.) with separate v4.1 client maps
- Handler file: `exchange_id_handler.go` in `internal/protocol/nfs/v4/handlers/` (consistent with existing pattern)
- Testing: unit tests on StateManager + integration tests through full dispatch path
- E2E testing: deferred until more v4.1 ops are ready (CREATE_SESSION needed for full mount)
- REST API client/server endpoints and dfsctl CLI commands included in this phase's scope

### Claude's Discretion
- v4.0/v4.1 client map isolation strategy (shared owner lookup vs completely independent)
- server_owner major_id construction details
- EXCHGID4_FLAG_CONFIRMED_R tracking approach
- V41ClientRecord struct design (which fields to embed vs v4.1-specific)
- Error code selection for SP4_MACH_CRED/SP4_SSV rejection

</decisions>

<specifics>
## Specific Ideas

- Server should identify as "dittofs" with domain "dittofs.io" -- consistent with project branding
- Client listing API should be useful for debugging: show implementation info, lease status, NFS version
- Admin eviction is important for operational control (force-disconnect misbehaving clients)
- Follow the `dfsctl` table/JSON output convention for new client commands

</specifics>

<deferred>
## Deferred Ideas

None -- discussion stayed within phase scope

</deferred>

---

*Phase: 18-exchange-id-and-client-registration*
*Context gathered: 2026-02-20*
