# Phase 21: Connection Management and Trunking - Research

**Researched:** 2026-02-21
**Domain:** NFSv4.1 BIND_CONN_TO_SESSION, connection-session association, trunking
**Confidence:** HIGH

## Summary

Phase 21 implements BIND_CONN_TO_SESSION (RFC 8881 Section 18.34) to associate multiple TCP connections with a single NFSv4.1 session, enabling trunking and reconnection after network disruption. The XDR types already exist (`BindConnToSessionArgs`/`BindConnToSessionRes` in `internal/protocol/nfs/v4/types/bind_conn_to_session.go`), sessions exist (Phase 19), and SEQUENCE gating with session-exempt dispatch exists (Phase 20). BIND_CONN_TO_SESSION is already listed as session-exempt in `isSessionExemptOp()` and has a stub in `v41DispatchTable`.

The implementation requires: (1) a connection ID plumbing mechanism from TCP accept through `CompoundContext` to the handler, (2) connection binding state in `StateManager` tracking which connections are bound to which sessions in which direction, (3) replacing the stub handler with a real implementation, (4) disconnect cleanup, and (5) connection draining support. The existing codebase patterns (session metrics, state management, handler conventions) provide clear templates.

**Primary recommendation:** Follow the existing StateManager pattern for connection binding state, use a separate `sync.RWMutex` for connection bindings (per CONTEXT.md), assign connection IDs via global `atomic.Uint64` at TCP accept time, and thread `ConnectionID` through `CompoundContext`.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- When client requests CDFC4_FORE_OR_BOTH or CDFC4_BACK_OR_BOTH, always grant CDFS4_BOTH (generous policy)
- First connection that creates a session (via CREATE_SESSION) is automatically bound as fore-channel
- Re-binding direction on an already-bound connection is allowed (RFC 8881 conformant)
- A TCP connection can only be bound to one session at a time; binding to a new session silently unbinds from the old one
- Enforce that a session always has at least one fore-channel connection (reject bind that would leave zero fore connections)
- Ownership validation: only the client (by client_id) that created the session can bind connections to it; return NFS4ERR_BADSESSION otherwise
- State protection: SP4_NONE only (consistent with DittoFS AUTH_UNIX model)
- Configurable maximum connections per session (e.g., V4MaxConnectionsPerSession, default 16)
- Max connections setting exposed via NFS adapter REST API (not config file only)
- Connection counts in session detail API response with per-direction breakdown: `{ fore: N, back: N, both: N, total: N }`
- Prometheus metrics: gauge for connections per session, counter for bind/unbind events (follows session_metrics.go nil-safe pattern)
- INFO-level logging for bind/unbind events
- Immediate unbind when a TCP connection drops (no grace period)
- Session survives when all connections are lost (kept alive by lease timer, client can reconnect and BIND new connections)
- Session reaper (existing 30s sweep) also cleans up orphaned/stale connection bindings
- DESTROY_SESSION auto-unbinds all connections (client doesn't need to manually unbind each)
- Graceful server shutdown: reuse existing DittoFS shutdown flow for bound connections (no NFS4-specific shutdown signaling)
- Track last-activity timestamp per connection for diagnostics (exposed in session detail API)
- Connection draining supported: mark a connection as "draining" so no new requests are accepted but in-flight ones complete
- Accept RDMA mode requests but always return `UseConnInRDMAMode=false` in response (client falls back to TCP)
- DEBUG-level log note when client requests RDMA mode
- Add a `ConnectionType` field (TCP/RDMA) to the connection model for future extensibility
- Global atomic `uint64` counter for connection IDs (unique across all adapters)
- Add `ConnectionID uint64` to `CompoundContext` -- assigned at TCP accept() time in NFS adapter, threaded through dispatch
- Auto-bind on CREATE_SESSION records both connection ID and direction in the session
- Connection binding changes protected by a separate `sync.RWMutex` (not the session-level lock) for better concurrency
- Connection binding state lives in StateManager (extends existing session/client ownership)
- Handler follows existing pattern: `bind_conn_to_session_handler.go`
- Replace existing BIND_CONN_TO_SESSION stub in v41DispatchTable with real handler
- Update `internal/protocol/CLAUDE.md` with connection management conventions

### Plan Structure
- **Plan 21-01**: Core binding model -- connection ID plumbing, StateManager connection methods, BIND_CONN_TO_SESSION handler, cleanup/disconnect logic, draining support, unit tests, simulated disconnect tests
- **Plan 21-02**: Observability & API -- Prometheus connection metrics, REST API session detail extension (connection breakdown), CLI updates, integration tests with multi-connection COMPOUND dispatch

### Claude's Discretion
- Exact NFS error code when connection limit is exceeded
- In-flight request handling on disconnect (best-effort cancel vs wait-with-timeout)
- SEQ4_STATUS flag notification when connection drops (RFC-guided decision)
- Whether session destroy force-closes remaining connections or just unbinds them
- CLI command structure for viewing session connections (dfsctl subcommand vs flag)
- Handler file structure details

### Deferred Ideas (OUT OF SCOPE)
- **RDMA support**: Full RDMA transport implementation
- **Backchannel over bound connections**: Phase 22 scope (CB_SEQUENCE, bidirectional I/O)
- **Dynamic connection limit adjustment**: Runtime-configurable limits via API
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| BACK-02 | Server handles BIND_CONN_TO_SESSION to associate connections with sessions for fore/back/both directions | Core of this phase: handler implementation, StateManager binding methods, direction negotiation per generous policy, connection tracking infrastructure |
| TRUNK-01 | Multiple connections can be bound to a single session via BIND_CONN_TO_SESSION | Enabled by connection binding model in StateManager, connection ID plumbing through CompoundContext, limit enforcement, multi-connection COMPOUND dispatch tests |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | 1.21+ | `sync.RWMutex` for connection binding lock, `sync.Map` not needed (bounded cardinality) | Standard Go concurrency primitive |
| Go stdlib `sync/atomic` | 1.21+ | Global connection ID counter (`atomic.Uint64`) | Lock-free monotonic counter |
| Go stdlib `time` | 1.21+ | Last-activity timestamp per connection | Already used throughout state package |
| `prometheus/client_golang` | v1.x | Connection metrics (gauge, counter) | Already in use for session/sequence metrics |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| Go stdlib `testing` | 1.21+ | Unit tests | All test files |
| Go stdlib `net` | 1.21+ | Simulated disconnect tests | Testing only |

### Alternatives Considered
None -- all decisions are locked and the stack follows existing project patterns.

## Architecture Patterns

### Recommended File Structure
```
internal/protocol/nfs/v4/state/
├── manager.go              # Extend with connection binding methods
├── session.go              # No changes (session struct stays lean)
├── connection.go           # NEW: BoundConnection struct, ConnectionDirection type
├── connection_test.go      # NEW: Unit tests for binding logic
├── connection_metrics.go   # NEW: ConnectionMetrics (nil-safe, Plan 21-02)
├── connection_metrics_test.go # NEW: Metrics tests (Plan 21-02)

internal/protocol/nfs/v4/handlers/
├── bind_conn_to_session_handler.go       # NEW: BIND_CONN_TO_SESSION handler
├── bind_conn_to_session_handler_test.go  # NEW: Handler tests
├── handler.go              # Replace stub with real handler in NewHandler()
├── context.go              # Thread ConnectionID (already has ExtractV4HandlerContext)

internal/protocol/nfs/v4/types/
├── types.go                # Add ConnectionID to CompoundContext

pkg/adapter/nfs/
├── nfs_adapter.go          # Add global connection ID counter, assign at accept
├── nfs_connection.go       # Store connection ID on NFSConnection
├── nfs_connection_handlers.go  # Pass connection ID into CompoundContext

pkg/controlplane/models/
├── adapter_settings.go     # Add V4MaxConnectionsPerSession field
```

### Pattern 1: Connection Binding State in StateManager

**What:** Connection bindings are tracked in a dedicated map protected by a separate `sync.RWMutex`, following the Phase 17 per-SlotTable mutex pattern for concurrency.

**When to use:** All connection bind/unbind operations.

**Example:**
```go
// In state/connection.go
type ConnectionDirection uint8

const (
    ConnDirFore ConnectionDirection = iota + 1
    ConnDirBack
    ConnDirBoth
)

type ConnectionType uint8

const (
    ConnTypeTCP ConnectionType = iota
    ConnTypeRDMA
)

type BoundConnection struct {
    ConnectionID uint64
    SessionID    types.SessionId4
    Direction    ConnectionDirection
    ConnType     ConnectionType
    BoundAt      time.Time
    LastActivity time.Time
    Draining     bool
}

// In state/manager.go (additions to StateManager)
type StateManager struct {
    // ... existing fields ...

    // Connection binding state (separate lock per CONTEXT.md)
    connMu        sync.RWMutex
    connByID      map[uint64]*BoundConnection      // connectionID -> binding
    connBySession map[types.SessionId4][]*BoundConnection // sessionID -> bindings
    maxConnsPerSession int                            // default 16
}
```

### Pattern 2: Connection ID Plumbing

**What:** A global `atomic.Uint64` counter in the NFS adapter assigns a unique ID to each TCP connection at accept() time. The ID is stored on `NFSConnection`, then threaded into `CompoundContext` during dispatch.

**When to use:** Every connection accept and COMPOUND dispatch.

**Example:**
```go
// In pkg/adapter/nfs/nfs_adapter.go
type NFSAdapter struct {
    // ... existing fields ...
    nextConnID atomic.Uint64  // global connection ID counter
}

// In Serve() accept loop:
connID := s.nextConnID.Add(1)
conn := s.newConnWithID(tcpConn, connID)

// In types/types.go CompoundContext:
type CompoundContext struct {
    // ... existing fields ...
    ConnectionID uint64 // assigned at TCP accept, threaded through dispatch
}

// In handlers/context.go ExtractV4HandlerContext:
// The ConnectionID is set by the caller (nfs_connection_handlers.go)
// before calling ProcessCompound.
```

### Pattern 3: Direction Negotiation (Generous Policy)

**What:** Per locked decision, when client requests `CDFC4_FORE_OR_BOTH` or `CDFC4_BACK_OR_BOTH`, server always grants `CDFS4_BOTH`. For `CDFC4_FORE` grant `CDFS4_FORE`, for `CDFC4_BACK` grant `CDFS4_BACK`.

**Example:**
```go
func negotiateDirection(clientDir uint32) (ConnectionDirection, uint32) {
    switch clientDir {
    case types.CDFC4_FORE:
        return ConnDirFore, types.CDFS4_FORE
    case types.CDFC4_BACK:
        return ConnDirBack, types.CDFS4_BACK
    case types.CDFC4_FORE_OR_BOTH:
        return ConnDirBoth, types.CDFS4_BOTH
    case types.CDFC4_BACK_OR_BOTH:
        return ConnDirBoth, types.CDFS4_BOTH
    default:
        return ConnDirFore, types.CDFS4_FORE // safe default
    }
}
```

### Pattern 4: Auto-Bind on CREATE_SESSION

**What:** When CREATE_SESSION succeeds, the connection that sent it is automatically bound to the new session as a fore-channel connection. This avoids the need for an explicit BIND_CONN_TO_SESSION after CREATE_SESSION.

**When to use:** CREATE_SESSION handler success path. Requires ConnectionID in CompoundContext.

**Example:**
```go
// In CREATE_SESSION handler, after StateManager.CreateSession() succeeds:
if compCtx.ConnectionID != 0 {
    h.StateManager.BindConnection(compCtx.ConnectionID, result.SessionID, ConnDirFore)
}
```

### Pattern 5: Disconnect Cleanup

**What:** When a TCP connection closes, all bindings for that connection ID are immediately removed from StateManager. The session survives (kept alive by lease timer).

**When to use:** NFS adapter connection close path (the deferred cleanup in `Serve()` accept loop).

**Example:**
```go
// In pkg/adapter/nfs/nfs_adapter.go Serve() goroutine defer:
defer func() {
    // ... existing cleanup ...
    // Unbind connection from any session
    if s.v4Handler != nil && s.v4Handler.StateManager != nil {
        s.v4Handler.StateManager.UnbindConnection(connID)
    }
}()
```

### Anti-Patterns to Avoid

- **Storing net.Conn in StateManager:** The binding model must be transport-agnostic. Store connection IDs, not connection objects. The NFS adapter owns the TCP connection; StateManager only knows about IDs and directions.
- **Global sm.mu for connection operations:** Per CONTEXT.md, use a separate `connMu` RWMutex. Connection binding is a hot path for trunking and must not contend with open state / lock state operations.
- **Session death on last connection drop:** Per CONTEXT.md, sessions survive connection loss. The lease timer keeps the session alive. The client can reconnect and BIND new connections.
- **Blocking on connection limit exceeded:** Return an NFS error immediately, do not queue.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Prometheus metrics | Custom counters | `prometheus/client_golang` with nil-safe wrapper pattern from `session_metrics.go` | Consistency, Prometheus ecosystem |
| Connection ID generation | UUID or random | `atomic.Uint64.Add(1)` | Simpler, faster, unique across adapters, per CONTEXT.md |
| Thread-safe maps | Custom locking per key | `sync.RWMutex` protecting plain maps | Bounded cardinality (max ~16 conns/session, ~16 sessions/client) makes sync.Map unnecessary |

**Key insight:** The connection binding model is conceptually simple (map of connID -> binding, plus reverse index sessionID -> []binding). The complexity is in the lifecycle hooks (accept, bind, rebind, unbind, disconnect, session destroy, reaper).

## Common Pitfalls

### Pitfall 1: Forgetting to Thread ConnectionID
**What goes wrong:** Handler receives ConnectionID=0 and cannot record the binding.
**Why it happens:** CompoundContext is created in `ExtractV4HandlerContext` but the caller (`nfs_connection_handlers.go`) might not set ConnectionID.
**How to avoid:** Set `compCtx.ConnectionID = c.connectionID` in `handleNFSv4Procedure` right after `ExtractV4HandlerContext` returns, before calling `ProcessCompound`. Add a test that verifies ConnectionID is non-zero in the handler.
**Warning signs:** All BIND_CONN_TO_SESSION calls succeed but connection tracking shows zero bindings.

### Pitfall 2: Race Between Unbind and In-Flight Requests
**What goes wrong:** A connection drops while a COMPOUND is mid-flight. The unbind fires from the close handler, removing the binding. The in-flight request finishes and tries to cache the response in a slot that references a session the connection was just unbound from.
**Why it happens:** Disconnect cleanup runs concurrently with request processing.
**How to avoid:** Unbinding is a bookkeeping operation that removes the entry from `connByID`/`connBySession` maps. It does NOT cancel in-flight requests. The in-flight request still has the session and slot references from SEQUENCE validation (stored in `v41ctx`). Slot completion and response caching are independent of connection binding state.
**Warning signs:** Panics or stale data during high-churn disconnect testing.

### Pitfall 3: Deadlock Between connMu and sm.mu
**What goes wrong:** `BindConnection` acquires `connMu` then needs session info (acquires `sm.mu`); meanwhile `DestroySession` acquires `sm.mu` then needs to unbind connections (acquires `connMu`).
**Why it happens:** Two mutexes acquired in different order from different call sites.
**How to avoid:** Establish strict lock ordering: always acquire `sm.mu` before `connMu`. Alternatively, design the API so that `BindConnection` does not need `sm.mu` (pass the clientID/sessionID as parameters validated by the caller under `sm.mu`). The recommended approach: `BindConnection` takes `connMu` only and receives pre-validated session metadata; `DestroySession` first collects connection IDs under `sm.mu`, then calls `UnbindConnection` (which takes `connMu`) after releasing `sm.mu`.
**Warning signs:** Deadlock under concurrent bind + destroy operations.

### Pitfall 4: Fore-Channel Enforcement Edge Case
**What goes wrong:** Client rebinds the last fore-channel connection to back-only. Server should reject this but doesn't check the count correctly (counts the connection being rebound as still fore).
**Why it happens:** The check counts existing fore connections including the one being rebound. If it is the only fore connection and is being rebound to back, the count is 1 (itself) but after rebind it would be 0.
**How to avoid:** When rebinding, temporarily exclude the current connection from the count: `foreCount = countForeExcluding(sessionID, connectionID)`. If the new direction is not fore/both and foreCount == 0, reject with NFS4ERR_INVAL.
**Warning signs:** Sessions left without fore-channel connections, causing SEQUENCE to fail.

### Pitfall 5: Connection Limit Error Code Selection
**What goes wrong:** Using wrong error code causes client to retry indefinitely instead of backing off.
**Why it happens:** RFC 8881 doesn't have a specific "too many connections" error.
**How to avoid:** Use `NFS4ERR_RESOURCE` (10018) for connection limit exceeded. This is the standard "server resource limit" error that clients understand means "try later or reduce usage." Linux nfsd uses `NFS4ERR_RESOURCE` for similar resource exhaustion scenarios.
**Warning signs:** Client enters retry loop after hitting connection limit.

### Pitfall 6: Auto-Bind on CREATE_SESSION Missing
**What goes wrong:** Session created but no connection is bound. First SEQUENCE works (session found by ID) but backchannel tracking shows no connections, and trunking diagnostics show empty connection list.
**Why it happens:** CREATE_SESSION handler doesn't call BindConnection after session creation.
**How to avoid:** Add explicit auto-bind call in CREATE_SESSION handler success path. Requires ConnectionID in CompoundContext (this is a prerequisite for the handler to work).
**Warning signs:** Session detail API shows 0 connections immediately after creation.

## Code Examples

### BIND_CONN_TO_SESSION Handler Pattern

```go
// bind_conn_to_session_handler.go
func (h *Handler) handleBindConnToSession(
    ctx *types.CompoundContext,
    _ *types.V41RequestContext, // nil for session-exempt ops
    reader io.Reader,
) *types.CompoundResult {
    var args types.BindConnToSessionArgs
    if err := args.Decode(reader); err != nil {
        return &types.CompoundResult{
            Status: types.NFS4ERR_BADXDR,
            OpCode: types.OP_BIND_CONN_TO_SESSION,
            Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
        }
    }

    // Log RDMA request at DEBUG level per CONTEXT.md
    if args.UseConnInRDMAMode {
        logger.Debug("BIND_CONN_TO_SESSION: client requested RDMA mode (not supported)",
            "session_id", args.SessionID.String(),
            "client", ctx.ClientAddr)
    }

    // Delegate to StateManager
    result, err := h.StateManager.BindConnToSession(
        ctx.ConnectionID,
        args.SessionID,
        args.Dir,
    )
    if err != nil {
        nfsStatus := mapStateError(err)
        return &types.CompoundResult{
            Status: nfsStatus,
            OpCode: types.OP_BIND_CONN_TO_SESSION,
            Data:   encodeStatusOnly(nfsStatus),
        }
    }

    // Encode success response
    res := &types.BindConnToSessionRes{
        Status:            types.NFS4_OK,
        SessionID:         args.SessionID,
        Dir:               result.ServerDir,
        UseConnInRDMAMode: false, // Always false per CONTEXT.md
    }
    var buf bytes.Buffer
    if err := res.Encode(&buf); err != nil {
        return &types.CompoundResult{
            Status: types.NFS4ERR_SERVERFAULT,
            OpCode: types.OP_BIND_CONN_TO_SESSION,
            Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
        }
    }

    logger.Info("BIND_CONN_TO_SESSION: connection bound",
        "session_id", args.SessionID.String(),
        "connection_id", ctx.ConnectionID,
        "direction", result.ServerDir,
        "client", ctx.ClientAddr)

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_BIND_CONN_TO_SESSION,
        Data:   buf.Bytes(),
    }
}
```

### StateManager BindConnToSession Method

```go
// In state/manager.go
func (sm *StateManager) BindConnToSession(
    connectionID uint64,
    sessionID types.SessionId4,
    clientDir uint32,
) (*BindConnResult, error) {
    // Validate session exists and get client ID (under sm.mu)
    sm.mu.RLock()
    session, exists := sm.sessionsByID[sessionID]
    if !exists {
        sm.mu.RUnlock()
        return nil, ErrBadSession
    }
    clientID := session.ClientID
    sm.mu.RUnlock()

    // Negotiate direction
    direction, serverDir := negotiateDirection(clientDir)

    // Acquire connection lock
    sm.connMu.Lock()
    defer sm.connMu.Unlock()

    // Check if connection is already bound to another session -> silently unbind
    if existing, ok := sm.connByID[connectionID]; ok {
        if existing.SessionID != sessionID {
            sm.unbindConnectionLocked(connectionID)
        }
    }

    // Check connection limit
    bindings := sm.connBySession[sessionID]
    if len(bindings) >= sm.maxConnsPerSession {
        // Check if we're rebinding (not a new connection for this session)
        isRebind := false
        for _, b := range bindings {
            if b.ConnectionID == connectionID {
                isRebind = true
                break
            }
        }
        if !isRebind {
            return nil, &NFS4StateError{
                Status:  types.NFS4ERR_RESOURCE,
                Message: "per-session connection limit exceeded",
            }
        }
    }

    // Fore-channel enforcement: reject if this would leave zero fore connections
    if direction == ConnDirBack {
        foreCount := 0
        for _, b := range bindings {
            if b.ConnectionID == connectionID {
                continue // exclude self
            }
            if b.Direction == ConnDirFore || b.Direction == ConnDirBoth {
                foreCount++
            }
        }
        if foreCount == 0 {
            return nil, &NFS4StateError{
                Status:  types.NFS4ERR_INVAL,
                Message: "cannot leave session with zero fore-channel connections",
            }
        }
    }

    // Create or update binding
    now := time.Now()
    binding := &BoundConnection{
        ConnectionID: connectionID,
        SessionID:    sessionID,
        Direction:    direction,
        ConnType:     ConnTypeTCP,
        BoundAt:      now,
        LastActivity:  now,
    }

    // Remove old binding for this connection (rebind case)
    sm.removeConnFromSession(connectionID, sessionID)

    sm.connByID[connectionID] = binding
    sm.connBySession[sessionID] = append(sm.connBySession[sessionID], binding)

    return &BindConnResult{ServerDir: serverDir}, nil
}
```

### Connection Metrics Pattern (nil-safe)

```go
// connection_metrics.go -- follows session_metrics.go pattern exactly
type ConnectionMetrics struct {
    BindTotal   *prometheus.CounterVec // label: "direction"
    UnbindTotal *prometheus.CounterVec // label: "reason" (explicit, disconnect, session_destroy)
    BoundGauge  *prometheus.GaugeVec   // label: "session_id"
}

func (m *ConnectionMetrics) RecordBind(direction string) {
    if m == nil { return }
    m.BindTotal.WithLabelValues(direction).Inc()
}
func (m *ConnectionMetrics) RecordUnbind(reason string) {
    if m == nil { return }
    m.UnbindTotal.WithLabelValues(reason).Inc()
}
func (m *ConnectionMetrics) SetBoundConnections(sessionID string, count float64) {
    if m == nil { return }
    m.BoundGauge.WithLabelValues(sessionID).Set(count)
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Single connection per session (NFSv4.0) | Multiple connections per session via BIND_CONN_TO_SESSION (NFSv4.1) | RFC 5661 (2010), updated RFC 8881 (2020) | Enables trunking, reconnection, backchannel multiplexing |
| SP4_MACH_CRED for bind security | SP4_NONE sufficient for AUTH_UNIX environments | DittoFS decision (Phase 21) | Simplifies implementation, no Kerberos requirement for binding |

**Deprecated/outdated:**
- RFC 5661 Section 18.34 has been superseded by RFC 8881 Section 18.34 (same operation, clearer specification)

## Open Questions

1. **SEQ4_STATUS flag on connection drop**
   - What we know: RFC 8881 defines `SEQ4_STATUS_CB_PATH_DOWN` when backchannel is unavailable. When a back-channel connection drops, this flag should be set in the next SEQUENCE response.
   - What's unclear: Whether to set a specific flag when a fore-channel connection drops (the session is still alive via other connections or lease).
   - Recommendation: Set `SEQ4_STATUS_CB_PATH_DOWN` if the last back/both connection drops and no backchannel remains. Do not set any flag for fore-only connection drops (the session is still functional via other fore connections, and the client that lost the connection will know because its TCP dropped). The existing `GetStatusFlags` already sets `CB_PATH_DOWN` when `BackChannelSlots == nil`; extend this to also check if any back-channel connection is bound.

2. **Force-close vs unbind on DESTROY_SESSION**
   - What we know: CONTEXT.md says "DESTROY_SESSION auto-unbinds all connections"
   - What's unclear: Whether to also force-close the TCP connections (writing a close on the wire) or just remove the binding metadata
   - Recommendation: Just unbind (remove metadata). The TCP connections remain open and can be used for new sessions or other operations. Force-closing TCP is a network-level concern and shouldn't be mixed with NFS state management. The existing `destroySessionLocked` pattern only removes state, never touches TCP.

3. **Connection draining implementation**
   - What we know: CONTEXT.md says "Connection draining supported: mark as draining so no new requests are accepted but in-flight ones complete"
   - What's unclear: Where the draining check is enforced (COMPOUND dispatch? accept loop?)
   - Recommendation: Add a `Draining` flag to `BoundConnection`. Check it in `dispatchV41` after SEQUENCE validation: if the connection is marked as draining, return `NFS4ERR_DELAY` for new requests. In-flight requests (already past SEQUENCE) complete normally. Expose a drain API endpoint or StateManager method for admin use.

## Sources

### Primary (HIGH confidence)
- RFC 8881 Sections 18.34, 2.10.5 - BIND_CONN_TO_SESSION specification, trunking semantics
- Codebase analysis of existing StateManager, Session, handler patterns (direct code reading)
- `internal/protocol/nfs/v4/types/bind_conn_to_session.go` - XDR types already implemented (Phase 16)
- `internal/protocol/nfs/v4/types/constants.go` - CDFC4_*/CDFS4_* direction constants, NFS4ERR_* error codes
- `internal/protocol/nfs/v4/state/session_metrics.go` - Nil-safe Prometheus pattern reference
- `internal/protocol/nfs/v4/state/sequence_metrics.go` - Nil-safe Prometheus pattern reference
- `internal/protocol/nfs/v4/handlers/handler.go` - v41DispatchTable stub pattern, handler registration
- `internal/protocol/nfs/v4/handlers/create_session_handler.go` - Session-exempt handler pattern reference
- `internal/protocol/nfs/v4/handlers/sequence_handler.go` - isSessionExemptOp includes BIND_CONN_TO_SESSION
- `pkg/adapter/nfs/nfs_adapter.go` - TCP accept loop, connection lifecycle, shutdown flow
- `pkg/adapter/nfs/nfs_connection.go` - NFSConnection struct, Serve() method
- `pkg/adapter/nfs/nfs_connection_handlers.go` - CompoundContext creation via ExtractV4HandlerContext

### Secondary (MEDIUM confidence)
- [Linux nfsd nfs4state.c](https://github.com/torvalds/linux/blob/master/fs/nfsd/nfs4state.c) - `alloc_conn`, `nfsd4_hash_conn` patterns for connection tracking
- [Linux nfsd cleanup patches](https://patchwork.kernel.org/project/linux-nfs/patch/1424374571-4945-4-git-send-email-trond.myklebust@primarydata.com/) - `bind_conn_to_session` cleanup showing direction handling

### Tertiary (LOW confidence)
- [NetApp BIND_CONN_TO_SESSION KB](https://kb.netapp.com/on-prem/ontap/da/NAS/NAS-KBs/Slow_NFSv4.1_with_KRB5P_due_to_excessive_BIND_CONN_TO_SESSION_calls) - Real-world issue with excessive bind calls (informational, confirms operation frequency can be high)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All Go stdlib, all patterns already used in project
- Architecture: HIGH - Follows exact existing StateManager/handler/metrics patterns; XDR types pre-built
- Pitfalls: HIGH - Derived from direct codebase analysis and concurrency patterns already validated in Phases 17-20

**Research date:** 2026-02-21
**Valid until:** Indefinite (RFC 8881 is stable, project patterns are established)
