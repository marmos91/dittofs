# Phase 21: Connection Management and Trunking - Context

**Gathered:** 2026-02-21
**Status:** Ready for planning

<domain>
## Phase Boundary

BIND_CONN_TO_SESSION operation implementation: multiple TCP connections can be bound to a single NFSv4.1 session, enabling trunking and reconnection after network disruption. The XDR types already exist (Phase 16), sessions exist (Phase 19), and SEQUENCE gating with session-exempt dispatch exists (Phase 20). This phase replaces the existing BIND_CONN_TO_SESSION stub with a real handler and adds connection-to-session tracking infrastructure.

Backchannel callbacks over bound connections are Phase 22 scope.

</domain>

<decisions>
## Implementation Decisions

### Direction Negotiation Policy
- When client requests CDFC4_FORE_OR_BOTH or CDFC4_BACK_OR_BOTH, always grant CDFS4_BOTH (generous policy)
- First connection that creates a session (via CREATE_SESSION) is automatically bound as fore-channel
- Re-binding direction on an already-bound connection is allowed (RFC 8881 conformant)
- A TCP connection can only be bound to one session at a time; binding to a new session silently unbinds from the old one
- Enforce that a session always has at least one fore-channel connection (reject bind that would leave zero fore connections)
- Ownership validation: only the client (by client_id) that created the session can bind connections to it; return NFS4ERR_BADSESSION otherwise
- State protection: SP4_NONE only (consistent with DittoFS AUTH_UNIX model)

### Connection Limits and Binding Rules
- Configurable maximum connections per session (e.g., V4MaxConnectionsPerSession, default 16)
- Error code when limit exceeded: Claude's discretion (pick most appropriate NFS error per RFC)
- Max connections setting exposed via NFS adapter REST API (not config file only)
- Connection counts in session detail API response with per-direction breakdown: `{ fore: N, back: N, both: N, total: N }`
- Prometheus metrics: gauge for connections per session, counter for bind/unbind events (follows session_metrics.go nil-safe pattern)
- INFO-level logging for bind/unbind events

### Disconnect and Cleanup Behavior
- Immediate unbind when a TCP connection drops (no grace period)
- Session survives when all connections are lost (kept alive by lease timer, client can reconnect and BIND new connections)
- In-flight request handling on disconnect: Claude's discretion (follow existing graceful shutdown patterns)
- Session reaper (existing 30s sweep) also cleans up orphaned/stale connection bindings
- Connection draining supported: mark a connection as "draining" so no new requests are accepted but in-flight ones complete
- DESTROY_SESSION auto-unbinds all connections (client doesn't need to manually unbind each)
- Graceful server shutdown: reuse existing DittoFS shutdown flow for bound connections (no NFS4-specific shutdown signaling)
- Track last-activity timestamp per connection for diagnostics (exposed in session detail API)

### RDMA Mode
- Accept RDMA mode requests but always return `UseConnInRDMAMode=false` in response (client falls back to TCP)
- Consistent behavior: CREATE_SESSION back-channel RDMA also accepted and returns false
- DEBUG-level log note when client requests RDMA mode
- Add a `ConnectionType` field (TCP/RDMA) to the connection model for future extensibility

### Connection Identity
- Global atomic `uint64` counter for connection IDs (unique across all adapters)
- Add `ConnectionID uint64` to `CompoundContext` — assigned at TCP accept() time in NFS adapter, threaded through dispatch
- Auto-bind on CREATE_SESSION records both connection ID and direction in the session
- Connection binding changes protected by a separate `sync.RWMutex` (not the session-level lock) for better concurrency

### Code Structure
- Connection binding state lives in StateManager (extends existing session/client ownership)
- Handler follows existing pattern: `bind_conn_to_session_handler.go` (Claude's discretion on exact structure)
- Replace existing BIND_CONN_TO_SESSION stub in v41DispatchTable with real handler
- Update `internal/protocol/CLAUDE.md` with connection management conventions

### Plan Structure
- **Plan 21-01**: Core binding model — connection ID plumbing, StateManager connection methods, BIND_CONN_TO_SESSION handler, cleanup/disconnect logic, draining support, unit tests, simulated disconnect tests
- **Plan 21-02**: Observability & API — Prometheus connection metrics, REST API session detail extension (connection breakdown), CLI updates (Claude's discretion), integration tests with multi-connection COMPOUND dispatch

### Test Strategy
- Unit tests for StateManager binding logic (bind, rebind, unbind, limits, ownership validation, direction enforcement)
- Integration tests with full COMPOUND dispatch (bind, rebind, disconnect, multi-connection scenarios)
- Simulated network failure tests (drop net.Conn, verify cleanup, unbinding, metric updates)
- No benchmarks in this phase

### RFC References
- RFC 8881 Section 18.34 (BIND_CONN_TO_SESSION operation specification)
- RFC 8881 Section 2.10.5 (trunking and connection association overview)
- Both sections should be deeply referenced during research

### Claude's Discretion
- Exact NFS error code when connection limit is exceeded
- In-flight request handling on disconnect (best-effort cancel vs wait-with-timeout)
- SEQ4_STATUS flag notification when connection drops (RFC-guided decision)
- Whether session destroy force-closes remaining connections or just unbinds them
- CLI command structure for viewing session connections (dfsctl subcommand vs flag)
- Handler file structure details

</decisions>

<specifics>
## Specific Ideas

- Session detail API should show per-direction connection counts: `{ fore: 2, back: 1, both: 3, total: 6 }` — useful for trunking diagnostics
- Connection draining is important for planned maintenance scenarios
- Last-activity timestamp per connection helps debug stale connections
- Connection type field (TCP/RDMA) is minimal cost now, avoids refactor if RDMA ever materializes

</specifics>

<deferred>
## Deferred Ideas

- **RDMA support**: Full RDMA transport implementation — track as a future phase or GitHub issue
- **Backchannel over bound connections**: Phase 22 scope (CB_SEQUENCE, bidirectional I/O)
- **Dynamic connection limit adjustment**: Runtime-configurable limits via API (currently config-time only for the default)

</deferred>

---

*Phase: 21-connection-management-and-trunking*
*Context gathered: 2026-02-21*
