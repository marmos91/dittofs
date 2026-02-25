# Phase 19: Session Lifecycle - Context

**Gathered:** 2026-02-21
**Status:** Ready for planning

<domain>
## Phase Boundary

NFSv4.1 clients can create and destroy sessions with negotiated channel attributes. Implements CREATE_SESSION and DESTROY_SESSION handlers (RFC 8881 Sections 18.36-18.37), session storage on StateManager, channel attribute negotiation with server-imposed limits, replay detection for CREATE_SESSION, a background session reaper, REST API endpoints for session monitoring/management, and corresponding dfsctl CLI commands.

Does NOT include: SEQUENCE validation (Phase 20), connection binding/trunking (Phase 21), or backchannel callback sending (Phase 22).

</domain>

<decisions>
## Implementation Decisions

### Channel Attribute Limits
- Max slot count: **64 slots** per session fore channel (matches existing DefaultMaxSlots constant)
- Max request/response sizes: **aligned with existing NFS READ/WRITE buffer pool maximums** (1MB tier)
- Max operations per COMPOUND: **unlimited** (no cap) — Linux NFS client typically sends 2-6 ops
- RDMA: **always 0** for header_pad_size, empty rdma_ird (no RDMA support)
- Configurability: **via NFS adapter config** — extend existing adapter config with a `v4` subsection; group all existing NFSv4-specific configs there too
- Sessions are **ephemeral (in-memory only)** — on restart, clients get NFS4ERR_STALE_CLIENTID and re-establish via EXCHANGE_ID + CREATE_SESSION

### Back Channel Policy
- **Allocate real back channel SlotTable** during CREATE_SESSION when client requests it — Phase 22 activates it
- **Set CREATE_SESSION4_FLAG_CONN_BACK_CHAN** in response flags when client requests back channel
- Callback security: accept AUTH_NONE + AUTH_SYS, reject RPCSEC_GSS (stored for Phase 22)

### Claude's Discretion — Back Channel
- Back channel slot table sizing (smaller limits than fore channel are fine given CB_RECALL/CB_SEQUENCE usage patterns)
- Minimum request/response size floor enforcement (pick based on RFC guidance and practical NFS behavior)

### Session Limits & Cleanup
- **Per-client session limit**: enforced (server rejects with appropriate error when exceeded)
- **Lease expiry**: destroy all sessions for a client when its lease expires (clean, predictable)
- **DESTROY_SESSION scope**: only connections bound to the session can destroy it (RFC 8881 Section 18.37)
- **Background session reaper**: goroutine on StateManager with periodic sweep checking lease expiry
- Session storage: **direct maps on StateManager** (sessionsByID, sessionsByClientID) alongside existing v41ClientsBy* maps

### Claude's Discretion — Session Limits
- Exact per-client session limit number
- Behavior when DESTROY_SESSION is called with in-flight requests (pick based on RFC guidance)
- Unconfirmed client timeout policy (pick based on RFC and existing lease model)

### CREATE_SESSION Replay Detection
- **Full multi-case algorithm** per RFC 8881 Section 18.36 — all cases implemented
- **Full response cached** on the V41ClientRecord (CachedCreateSessionRes field) for idempotent replay
- Each successful CREATE_SESSION **increments the client's sequence ID**
- Log CREATE_SESSION and DESTROY_SESSION at **INFO level** for observability

### Code Structure & Testing
- **One handler file per operation**: create_session_handler.go, destroy_session_handler.go (same pattern as EXCHANGE_ID)
- Session reaper: **method on StateManager** (StartSessionReaper), runs as goroutine
- **Comprehensive unit + integration tests**: StateManager methods with all RFC cases (table-driven), integration tests with handler dispatch and XDR round-trip
- **Prometheus metrics**: session create/destroy counters, active sessions gauge, session duration histogram
- **Update protocol CLAUDE.md** with NFSv4.1 session handler conventions
- **Single plan** for the entire phase

### REST API & CLI
- **Nested endpoints**: GET /clients/{id}/sessions (list), DELETE /clients/{id}/sessions/{sid} (force-destroy)
- **Admin force-destroy** via DELETE endpoint for stuck/orphaned sessions
- **dfsctl CLI commands**: `dfsctl client sessions list` and `dfsctl client sessions destroy`
- API + CLI implemented in this phase (not deferred)

</decisions>

<specifics>
## Specific Ideas

- Channel attribute limits should be part of the NFS adapter config under a new `v4` subsection — group any existing NFSv4-specific configs there for consistency
- The user prefers the term "sessions" over "clients" for observability — both exist but sessions are the primary user-facing concept
- Session events (create/destroy) should be visible at INFO log level without enabling DEBUG

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 19-session-lifecycle*
*Context gathered: 2026-02-21*
