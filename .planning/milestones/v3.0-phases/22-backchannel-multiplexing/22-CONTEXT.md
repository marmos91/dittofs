# Phase 22: Backchannel Multiplexing - Context

**Gathered:** 2026-02-21
**Status:** Ready for planning

<domain>
## Phase Boundary

Server sends callbacks to v4.1 clients over their existing fore-channel TCP connections (no separate dial-out). Implements CB_SEQUENCE over the fore-channel, BACKCHANNEL_CTL for security parameter updates, and a routing layer that selects v4.0 (dial-out) or v4.1 (multiplexed) callback path based on client registration. v4.0 callback path continues working unchanged.

</domain>

<decisions>
## Implementation Decisions

### Callback Routing
- Tag each client as v4.0 or v4.1 at registration time (SETCLIENTID vs EXCHANGE_ID) — callback path is pre-determined, not checked at send time
- Dedicated sender goroutine per session (pulling from a callback queue) — serializes callbacks naturally, avoids blocking the recall triggerer
- Build an extensible dispatch table for callback operations — only CB_RECALL implemented now, but CB_NOTIFY (Phase 24) should be trivial to add
- On callback send failure, retry once on another back-bound connection before proceeding to delegation revocation

### CB_SEQUENCE Behavior
- Retry with exponential backoff: 3 attempts before declaring failure and revoking delegation
- Backchannel callbacks use exponential backoff timing (e.g., 5s, 10s, 20s between retries)

### BACKCHANNEL_CTL
- Strict validation: return error if session has no backchannel (CREATE_SESSION4_FLAG_CONN_BACK_CHAN was not set)
- Store callback security parameters per-session (not per-client) — each session can have different security
- Support all three security flavors: AUTH_NONE, AUTH_SYS, and RPCSEC_GSS
- Backchannel params are per-session, matching how sessions own their backchannel slot table

### Connection Selection
- Pick the back-bound connection with most recent fore-channel activity — likely healthiest and best for NAT traversal
- Lazy dead-connection detection only — discover dead connections when callback send fails, no proactive heartbeat/ping
- Callbacks share connections with fore-channel traffic — callbacks are small and infrequent, no contention avoidance needed

### Code Structure
- New `backchannel.go` file in the state package — clean separation from v4.0 `callback.go`
- Extract shared wire-format code (XDR encoding, record marking, RPC framing) into common helpers used by both v4.0 and v4.1 paths
- No BackchannelSender interface — concrete struct with methods, avoid premature abstraction
- Sender goroutine lifecycle tied to session destruction — shuts down when session is destroyed, no orphan goroutines
- Connection read loop demuxes: fore-channel requests go to handler, backchannel responses routed to sender goroutine — true bidirectional multiplexing on shared connection
- Check if existing connection write path already serializes; if not, add write mutex to prevent interleaving of callback and response writes

### Testing
- Integration tests with real TCP loopback connections — test the full wire format including record marking
- No E2E tests for this phase — E2E delegation recall via backchannel comes in Phase 25
- Prometheus metrics: counters (callback_total, callback_failures) + duration histograms (callback_duration_seconds)

### Claude's Discretion
- No-backchannel-bound-connection behavior (queue and wait vs revoke immediately when v4.1 client has no back-bound connections)
- Callback timeout values for v4.1 backchannel sends
- Backchannel slot table sizing (number of slots)
- CB_SEQUENCE replay/EOS enforcement approach (full vs simplified)
- Callback security credential handling (AUTH_NULL for now vs enforcing CREATE_SESSION params)
- BACKCHANNEL_CTL: whether SEQUENCE is required (per RFC 8881)
- BACKCHANNEL_CTL: immediate vs deferred GSS context verification
- BACKCHANNEL_CTL: security flavor selection order from client's list
- BACKCHANNEL_CTL: metrics approach (follow existing patterns)
- Connection failover: immediate switch vs wait-for-retry on disconnect
- Sender goroutine start timing: eager vs lazy
- Callback queue depth bound

</decisions>

<specifics>
## Specific Ideas

- Read loop must demux incoming data on shared connections: fore-channel RPC requests (client→server) vs backchannel RPC replies (client→server responses to CB_COMPOUND). XID matching or message type can distinguish them.
- The extensible dispatch table for callback ops should make Phase 24 (CB_NOTIFY for directory delegations) a simple addition.
- Shared wire-format helpers between v4.0 and v4.1 should cover: RPC record marking, CB_COMPOUND framing, XDR encoding of callback args/results.

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 22-backchannel-multiplexing*
*Context gathered: 2026-02-21*
