# Phase 19: Session Lifecycle - Research

**Researched:** 2026-02-21
**Domain:** NFSv4.1 CREATE_SESSION (op 43), DESTROY_SESSION (op 44), session management on StateManager, channel attribute negotiation, session reaper, REST API/CLI for sessions, Prometheus metrics
**Confidence:** HIGH

## Summary

Phase 19 implements the NFSv4.1 CREATE_SESSION and DESTROY_SESSION operations per RFC 8881 Sections 18.36-18.37. CREATE_SESSION establishes a session on an existing v4.1 client record (registered by EXCHANGE_ID in Phase 18), allocating fore/back channel slot tables with negotiated channel attributes. DESTROY_SESSION tears down a session, releasing all slot table memory. The phase also includes: CREATE_SESSION replay detection via the client's eir_sequenceid, a background session reaper goroutine for lease-expired clients, Prometheus metrics (session create/destroy counters, active gauge, duration histogram), REST API endpoints for session listing/force-destroy, and dfsctl CLI commands for session management.

The existing codebase already has: (1) complete XDR types for CreateSessionArgs/Res and DestroySessionArgs/Res in `internal/protocol/nfs/v4/types/`, (2) Session struct and NewSession constructor in `internal/protocol/nfs/v4/state/session.go`, (3) SlotTable struct with complete validation logic in `slot_table.go`, (4) V41ClientRecord with SequenceID field for CREATE_SESSION replay detection, (5) StateManager with v41ClientsByID/v41ClientsByOwner maps and ExchangeID method, (6) existing REST API client handler pattern in `internal/controlplane/api/handlers/clients.go`, and (7) stub handlers in the dispatch table for both CREATE_SESSION and DESTROY_SESSION. The primary work is: implementing CreateSession()/DestroySession() methods on StateManager with session maps, channel attribute negotiation logic, the full CREATE_SESSION replay detection algorithm, writing handler files, the session reaper goroutine, Prometheus metrics, extending the REST API and dfsctl CLI, and updating EvictV41Client to clean up sessions.

**Primary recommendation:** Add sessionsByID and sessionsByClientID maps to StateManager. Implement CreateSession() with full RFC 8881 Section 18.36 multi-case replay detection, channel attribute clamping, and per-client session limiting. Implement DestroySession() with session lookup and cleanup. Add StartSessionReaper() as a goroutine on StateManager. Wire handlers to replace stubs. Extend REST API with nested session endpoints under /clients/{id}/sessions. Add dfsctl session commands.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Max slot count: **64 slots** per session fore channel (matches existing DefaultMaxSlots constant)
- Max request/response sizes: **aligned with existing NFS READ/WRITE buffer pool maximums** (1MB tier)
- Max operations per COMPOUND: **unlimited** (no cap) -- Linux NFS client typically sends 2-6 ops
- RDMA: **always 0** for header_pad_size, empty rdma_ird (no RDMA support)
- Configurability: **via NFS adapter config** -- extend existing adapter config with a `v4` subsection; group all existing NFSv4-specific configs there too
- Sessions are **ephemeral (in-memory only)** -- on restart, clients get NFS4ERR_STALE_CLIENTID and re-establish via EXCHANGE_ID + CREATE_SESSION
- **Allocate real back channel SlotTable** during CREATE_SESSION when client requests it -- Phase 22 activates it
- **Set CREATE_SESSION4_FLAG_CONN_BACK_CHAN** in response flags when client requests back channel
- Callback security: accept AUTH_NONE + AUTH_SYS, reject RPCSEC_GSS (stored for Phase 22)
- **Per-client session limit**: enforced (server rejects with appropriate error when exceeded)
- **Lease expiry**: destroy all sessions for a client when its lease expires (clean, predictable)
- **DESTROY_SESSION scope**: only connections bound to the session can destroy it (RFC 8881 Section 18.37)
- **Background session reaper**: goroutine on StateManager with periodic sweep checking lease expiry
- Session storage: **direct maps on StateManager** (sessionsByID, sessionsByClientID) alongside existing v41ClientsBy* maps
- **Full multi-case algorithm** per RFC 8881 Section 18.36 -- all cases implemented
- **Full response cached** on the V41ClientRecord (CachedCreateSessionRes field) for idempotent replay
- Each successful CREATE_SESSION **increments the client's sequence ID**
- Log CREATE_SESSION and DESTROY_SESSION at **INFO level** for observability
- **One handler file per operation**: create_session_handler.go, destroy_session_handler.go (same pattern as EXCHANGE_ID)
- Session reaper: **method on StateManager** (StartSessionReaper), runs as goroutine
- **Comprehensive unit + integration tests**: StateManager methods with all RFC cases (table-driven), integration tests with handler dispatch and XDR round-trip
- **Prometheus metrics**: session create/destroy counters, active sessions gauge, session duration histogram
- **Update protocol CLAUDE.md** with NFSv4.1 session handler conventions
- **Single plan** for the entire phase
- **Nested REST API endpoints**: GET /clients/{id}/sessions (list), DELETE /clients/{id}/sessions/{sid} (force-destroy)
- **Admin force-destroy** via DELETE endpoint for stuck/orphaned sessions
- **dfsctl CLI commands**: `dfsctl client sessions list` and `dfsctl client sessions destroy`
- API + CLI implemented in this phase (not deferred)

### Claude's Discretion
- Back channel slot table sizing (smaller limits than fore channel are fine given CB_RECALL/CB_SEQUENCE usage patterns)
- Minimum request/response size floor enforcement (pick based on RFC guidance and practical NFS behavior)
- Exact per-client session limit number
- Behavior when DESTROY_SESSION is called with in-flight requests (pick based on RFC guidance)
- Unconfirmed client timeout policy (pick based on RFC and existing lease model)

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SESS-02 | Server handles CREATE_SESSION to establish sessions with negotiated channel attributes and slot tables | Core deliverable: CreateSession() on StateManager, channel attribute negotiation with server-imposed limits, slot table allocation, create_session_handler.go, replay detection algorithm, Prometheus metrics, REST API/CLI |
| SESS-03 | Server handles DESTROY_SESSION to tear down sessions and release slot table memory | DestroySession() on StateManager, session lookup by ID, slot table cleanup, destroy_session_handler.go, connection binding check, background reaper integration |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | 1.21+ | RWMutex for session maps on StateManager | Existing pattern, same mutex as v4.1 client maps |
| Go stdlib `crypto/rand` | 1.21+ | Session ID generation (already used by NewSession) | Secure random, existing pattern |
| Go stdlib `time` | 1.21+ | Session creation timestamps, reaper ticker | Standard library |
| Go stdlib `context` | 1.21+ | Reaper goroutine shutdown via context cancellation | Standard pattern for goroutine lifecycle |
| `github.com/prometheus/client_golang` | existing | Session metrics (counters, gauge, histogram) | Already used by NFS adapter metrics |
| `github.com/go-chi/chi/v5` | existing | REST API nested routing /clients/{id}/sessions | Already used by all API routes |
| `github.com/spf13/cobra` | existing | CLI commands for `dfsctl client sessions` | Already used by all CLI commands |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/cli/output` | existing | Table/JSON/YAML output for session list | CLI output formatting |
| `pkg/apiclient` | existing | REST client methods for session endpoints | dfsctl HTTP calls |
| Existing `state.NewSession` | Phase 17 | Session constructor with slot table allocation | Called by CreateSession() |
| Existing `state.SlotTable` | Phase 17 | Slot table with sequence validation | Created per session |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Maps on StateManager | Separate SessionManager struct | Unnecessary complexity; sessions are tightly coupled with client state and leases |
| Periodic reaper goroutine | Check on every SEQUENCE | Reaper is cleaner, avoids adding overhead to the hot path |
| sync.Map for session maps | Regular map + mutex | Sessions have low churn (create/destroy) vs high read (SEQUENCE), regular map under RWMutex is appropriate |

## Architecture Patterns

### Recommended Code Structure

```
internal/protocol/nfs/v4/
├── state/
│   ├── manager.go              # EXTEND: add session maps, CreateSession(), DestroySession(),
│   │                           #   ListSessionsForClient(), StartSessionReaper(), GetSession()
│   ├── v41_client.go           # EXTEND: add CachedCreateSessionRes field
│   ├── session.go              # EXISTING: Session struct, NewSession (Phase 17)
│   └── slot_table.go           # EXISTING: SlotTable (Phase 17)
├── handlers/
│   ├── handler.go              # MODIFY: replace CREATE_SESSION and DESTROY_SESSION stubs
│   ├── create_session_handler.go     # NEW: handleCreateSession handler
│   ├── create_session_handler_test.go # NEW: handler unit + integration tests
│   ├── destroy_session_handler.go    # NEW: handleDestroySession handler
│   └── destroy_session_handler_test.go # NEW: handler tests
└── types/
    └── create_session.go       # EXISTING: XDR types (Phase 16)

internal/controlplane/api/handlers/
└── clients.go                  # EXTEND: add ListSessions, ForceDestroySession methods

pkg/apiclient/
└── clients.go                  # EXTEND: add ListSessions, ForceDestroySession methods

cmd/dfsctl/commands/client/
├── client.go                   # MODIFY: add sessions subcommand
├── sessions_list.go            # NEW: dfsctl client sessions list
└── sessions_destroy.go         # NEW: dfsctl client sessions destroy

internal/protocol/
└── CLAUDE.md                   # UPDATE: v4.1 session handler conventions
```

### Pattern 1: CREATE_SESSION Multi-Case Algorithm (RFC 8881 Section 18.36)

**What:** The server validates the client ID, checks the sequence ID against the cached value on the V41ClientRecord, and either creates a new session, replays a cached response, or rejects the request.

**When to use:** In StateManager.CreateSession()

The algorithm cases (per RFC 8881 Section 18.36.4):

1. **csa_clientid unknown:** Return NFS4ERR_STALE_CLIENTID -- the client must re-issue EXCHANGE_ID.

2. **csa_sequence == V41ClientRecord.SequenceID (replay):** Return the cached CreateSessionRes from the V41ClientRecord. This is idempotent -- the same session is not created again.

3. **csa_sequence == V41ClientRecord.SequenceID + 1 (new request):** Create a new session, store it, cache the full response on V41ClientRecord.CachedCreateSessionRes, increment V41ClientRecord.SequenceID.

4. **csa_sequence != SequenceID and csa_sequence != SequenceID + 1:** Return NFS4ERR_SEQ_MISORDERED.

5. **Client not yet confirmed and this is the first CREATE_SESSION:** Set V41ClientRecord.Confirmed = true, create lease timer.

```go
// Source: RFC 8881 Section 18.36.4
type CreateSessionResult struct {
    SessionID        types.SessionId4
    SequenceID       uint32
    Flags            uint32
    ForeChannelAttrs types.ChannelAttrs
    BackChannelAttrs types.ChannelAttrs
}

func (sm *StateManager) CreateSession(
    clientID uint64,
    sequenceID uint32,
    flags uint32,
    foreAttrs, backAttrs types.ChannelAttrs,
    cbProgram uint32,
    cbSecParms []types.CallbackSecParms4,
) (*CreateSessionResult, []byte, error) {
    sm.mu.Lock()
    defer sm.mu.Unlock()

    // Case 1: Unknown client
    record, exists := sm.v41ClientsByID[clientID]
    if !exists {
        return nil, nil, ErrStaleClientID
    }

    // Case 2: Replay (same sequence ID)
    if sequenceID == record.SequenceID {
        if record.CachedCreateSessionRes == nil {
            return nil, nil, ErrSeqMisordered
        }
        return nil, record.CachedCreateSessionRes, nil
    }

    // Case 4: Misordered
    expectedSeqID := record.SequenceID + 1
    if sequenceID != expectedSeqID {
        return nil, nil, ErrSeqMisordered
    }

    // Case 3: New request (sequence ID = cached + 1)
    // Check per-client session limit
    // Negotiate channel attributes
    // Create session
    // Cache response, increment sequence ID
    // Confirm client if first session
    // ...
}
```

### Pattern 2: Channel Attribute Negotiation

**What:** The server clamps client-requested channel attributes to server-imposed limits without rejecting.

**When to use:** During CREATE_SESSION, before creating the session.

Per RFC 8881, the server MUST NOT increase any value beyond the client's request, but MAY reduce values to server limits. The server MUST support at least 1 slot.

```go
// Server-imposed limits for channel negotiation
type ChannelLimits struct {
    MaxSlots              uint32 // DefaultMaxSlots = 64
    MaxRequestSize        uint32 // 1MB (aligned with buffer pools)
    MaxResponseSize       uint32 // 1MB
    MaxResponseSizeCached uint32 // 64KB (replay cache per slot)
    MaxOperations         uint32 // 0 = unlimited
    MinRequestSize        uint32 // 8KB floor (minimum for useful RPC)
    MinResponseSize       uint32 // 8KB floor
}

func negotiateChannelAttrs(requested types.ChannelAttrs, limits ChannelLimits) types.ChannelAttrs {
    negotiated := requested

    // header_pad_size: always 0 (no RDMA)
    negotiated.HeaderPadSize = 0

    // MaxRequests (slot count): clamp to [1, MaxSlots]
    if negotiated.MaxRequests > limits.MaxSlots {
        negotiated.MaxRequests = limits.MaxSlots
    }
    if negotiated.MaxRequests < 1 {
        negotiated.MaxRequests = 1
    }

    // MaxRequestSize: clamp to [floor, limit]
    if negotiated.MaxRequestSize > limits.MaxRequestSize {
        negotiated.MaxRequestSize = limits.MaxRequestSize
    }
    if negotiated.MaxRequestSize < limits.MinRequestSize {
        negotiated.MaxRequestSize = limits.MinRequestSize
    }

    // MaxResponseSize: same pattern
    // MaxResponseSizeCached: important for replay cache sizing
    // MaxOperations: 0 = unlimited (per locked decision)
    // RdmaIrd: always empty (no RDMA)
    negotiated.RdmaIrd = nil

    return negotiated
}
```

**Discretion decisions for channel attributes:**
- **Back channel MaxSlots:** 8 (sufficient for CB_RECALL/CB_SEQUENCE; back channel is low-volume)
- **Back channel MaxRequestSize:** 64KB (callback operations are small)
- **Minimum request/response size floor:** 8KB (covers COMPOUND headers + at least one small operation)
- **MaxResponseSizeCached:** 64KB (per-slot replay cache limit; keeps memory bounded)

### Pattern 3: Session Maps on StateManager

**What:** Direct maps on StateManager for session lookup by ID and by client ID.

**When to use:** Session creation, lookup, and cleanup.

```go
// Added to StateManager struct:
// sessionsByID maps SessionId4 -> *Session for O(1) lookup by session ID.
sessionsByID map[types.SessionId4]*state.Session

// sessionsByClientID maps clientID -> []*Session for per-client session management.
sessionsByClientID map[uint64][]*state.Session
```

### Pattern 4: Background Session Reaper

**What:** A goroutine on StateManager that periodically sweeps for expired client leases and destroys their sessions.

**When to use:** Started when the StateManager is initialized, stopped on Shutdown().

```go
func (sm *StateManager) StartSessionReaper(ctx context.Context) {
    go func() {
        ticker := time.NewTicker(30 * time.Second) // sweep every 30 seconds
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                sm.reapExpiredSessions()
            }
        }
    }()
}

func (sm *StateManager) reapExpiredSessions() {
    sm.mu.Lock()
    defer sm.mu.Unlock()

    for clientID, record := range sm.v41ClientsByID {
        if record.Lease != nil && record.Lease.IsExpired() {
            // Destroy all sessions for this client
            sessions := sm.sessionsByClientID[clientID]
            for _, sess := range sessions {
                delete(sm.sessionsByID, sess.SessionID)
                // Metrics: record session destroy + duration
            }
            delete(sm.sessionsByClientID, clientID)
            sm.purgeV41Client(record)
            logger.Info("Session reaper: expired client cleaned up",
                "client_id", clientID,
                "sessions_destroyed", len(sessions))
        }
    }
}
```

### Pattern 5: REST API Session Endpoints

**What:** Nested endpoints under /clients/{id}/sessions for session management.

**When to use:** Admin operational visibility and emergency session cleanup.

```go
// Extended routes in router.go:
r.Route("/clients", func(r chi.Router) {
    r.Use(apiMiddleware.RequireAdmin())
    r.Get("/", clientHandler.List)
    r.Delete("/{id}", clientHandler.Evict)
    r.Route("/{id}/sessions", func(r chi.Router) {
        r.Get("/", clientHandler.ListSessions)
        r.Delete("/{sid}", clientHandler.ForceDestroySession)
    })
})
```

### Pattern 6: Prometheus Session Metrics

**What:** Metrics for session lifecycle observability.

**When to use:** Always (zero overhead when metrics disabled via nil check pattern).

```go
// Session metrics (registered if Prometheus is enabled)
var (
    sessionCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "dittofs_nfs_sessions_created_total",
        Help: "Total number of NFSv4.1 sessions created",
    })
    sessionDestroyedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
        Name: "dittofs_nfs_sessions_destroyed_total",
        Help: "Total number of NFSv4.1 sessions destroyed",
    }, []string{"reason"}) // reason: "client_request", "admin_evict", "lease_expired"
    sessionActiveGauge = prometheus.NewGauge(prometheus.GaugeOpts{
        Name: "dittofs_nfs_sessions_active",
        Help: "Current number of active NFSv4.1 sessions",
    })
    sessionDurationHistogram = prometheus.NewHistogram(prometheus.HistogramOpts{
        Name:    "dittofs_nfs_session_duration_seconds",
        Help:    "Duration of NFSv4.1 sessions from creation to destruction",
        Buckets: prometheus.ExponentialBuckets(1, 2, 20), // 1s to ~145 hours
    })
)
```

### Anti-Patterns to Avoid

- **Creating a session without clamping channel attributes:** The server MUST negotiate by clamping, never exceeding client's requested values. Returning unclamped values from the client request would allocate excessive memory.
- **Not caching the full CREATE_SESSION response:** The replay detection algorithm requires returning the exact cached response on sequence ID replay. Reconstructing it is fragile; cache the XDR bytes.
- **Incrementing SequenceID before the session is successfully created:** If session creation fails (e.g., too many sessions), the sequence ID must NOT be incremented. Only increment on success.
- **Forgetting to confirm the client on first CREATE_SESSION:** The first successful CREATE_SESSION must set V41ClientRecord.Confirmed = true and start the lease timer.
- **Destroying sessions in the reaper without holding sm.mu:** The reaper modifies session maps and client records -- it must hold the write lock.
- **Allowing any connection to destroy any session:** Per RFC 8881 Section 18.37 and locked decision, only connections bound to the session can destroy it. However, since we don't implement connection binding until Phase 21, for now the creating connection (identified by client identity) is sufficient.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Session creation with slot tables | Custom session struct + manual slot allocation | Existing `state.NewSession()` from Phase 17 | Already creates SessionID, fore/back slot tables with proper clamping |
| Session ID generation | Custom UUID or sequential IDs | Existing `crypto/rand.Read()` in NewSession | Already implemented, cryptographically random |
| XDR encode/decode for CREATE_SESSION | Manual byte manipulation | Existing `CreateSessionArgs.Decode()` / `CreateSessionRes.Encode()` | Complete and tested in Phase 16 |
| XDR encode/decode for DESTROY_SESSION | Manual byte manipulation | Existing `DestroySessionArgs.Decode()` / `DestroySessionRes.Encode()` | Complete and tested in Phase 16 |
| Slot table clamping | Manual min/max logic per field | Helper function `negotiateChannelAttrs()` | Centralizes all negotiation rules in one testable function |
| Handler error mapping | Inline NFS4 status conversion | Existing `mapStateError()` in helpers.go | Already handles NFS4StateError -> nfsstat4 |
| REST API JSON responses | Custom marshal | Existing `WriteJSONOK()`, `BadRequest()`, `NotFound()` | Already used by all API handlers |
| CLI table output | Custom formatting | Existing `internal/cli/output` package | Already used by client list command |

## Common Pitfalls

### Pitfall 1: CREATE_SESSION Sequence ID is NOT a Slot Sequence ID
**What goes wrong:** Confusing the per-client CREATE_SESSION sequence ID (eir_sequenceid/csa_sequence) with the per-slot COMPOUND sequence ID (sa_sequenceid).
**Why it happens:** Both are called "sequence ID" in the RFC.
**How to avoid:** The CREATE_SESSION sequence ID lives on V41ClientRecord.SequenceID. It starts at 1 (set by ExchangeID), and the client's first CREATE_SESSION must use csa_sequence = 1. On success, the server increments to 2. It is a single counter per client, not per slot. Slot sequence IDs are per-slot within a session's SlotTable.
**Warning signs:** CREATE_SESSION fails with NFS4ERR_SEQ_MISORDERED when it should succeed.

### Pitfall 2: CREATE_SESSION Confirmation Side Effect
**What goes wrong:** Client stays unconfirmed after CREATE_SESSION, causing EXCHANGE_ID to not set EXCHGID4_FLAG_CONFIRMED_R.
**Why it happens:** Forgetting to set V41ClientRecord.Confirmed = true and start the lease timer on the first successful CREATE_SESSION.
**How to avoid:** After successful session creation, check `if !record.Confirmed` and if true, set `record.Confirmed = true`, create lease timer via `NewLeaseState()`, set `record.LastRenewal`. This is the v4.1 equivalent of SETCLIENTID_CONFIRM.
**Warning signs:** Subsequent EXCHANGE_ID responses lack CONFIRMED_R flag; client keeps re-sending CREATE_SESSION.

### Pitfall 3: Session Reaper vs Lease Timer Callback Conflict
**What goes wrong:** Both the session reaper goroutine and the existing onLeaseExpired callback try to clean up the same client, causing double-free or panics.
**Why it happens:** V4.0 clients use onLeaseExpired callback from LeaseState. If v4.1 clients also get a lease timer with the same callback, the reaper and callback race.
**How to avoid:** For v4.1 clients, do NOT use the v4.0 onLeaseExpired callback. Use the session reaper goroutine exclusively for v4.1 lease cleanup. The v4.1 lease timer should use a separate callback (or nil callback with manual expiry checking in the reaper).
**Warning signs:** Panic on nil map access during concurrent cleanup; sessions partially destroyed.

### Pitfall 4: Not Handling the PERSIST Flag
**What goes wrong:** Client requests CREATE_SESSION4_FLAG_PERSIST but server persists nothing, causing confusion on restart.
**Why it happens:** The PERSIST flag asks the server to survive restarts.
**How to avoid:** Per locked decision, sessions are ephemeral. The server MUST clear the PERSIST flag from the response (set response flags WITHOUT PERSIST). On restart, clients get NFS4ERR_STALE_CLIENTID and re-establish.
**Warning signs:** Client expects session to survive restart; gets stale errors.

### Pitfall 5: Forgetting RPCSEC_GSS Rejection in Callback Security
**What goes wrong:** Client sends RPCSEC_GSS callback security parameters, server allocates backchannel with GSS, Phase 22 can't handle it.
**Why it happens:** Not validating csa_sec_parms before allocating.
**How to avoid:** Per locked decision, accept AUTH_NONE (flavor 0) and AUTH_SYS (flavor 1). Reject RPCSEC_GSS (flavor 6) with NFS4ERR_ENCR_ALG_UNSUPP. If ALL provided flavors are RPCSEC_GSS and none are AUTH_NONE/AUTH_SYS, reject the entire CREATE_SESSION.
**Warning signs:** Server stores GSS context it can't use; Phase 22 callback fails.

### Pitfall 6: Per-Client Session Limit Error Code
**What goes wrong:** Using a generic error code when the per-client session limit is exceeded.
**Why it happens:** RFC 8881 doesn't define a specific "too many sessions" error.
**How to avoid:** Use NFS4ERR_TOO_MANY_OPS (10070) or NFS4ERR_SERVERFAULT. Linux nfsd doesn't enforce a per-client limit (it has a global limit via DRC). For DittoFS, NFS4ERR_RESOURCE is the most appropriate -- it indicates the server cannot allocate resources for the operation.
**Warning signs:** Client gets an unexpected error code and doesn't know what to do.

### Pitfall 7: DESTROY_SESSION with In-Flight Requests
**What goes wrong:** Session is destroyed while COMPOUND operations are executing on its slot table, causing panics or corrupt state.
**Why it happens:** DESTROY_SESSION races with concurrent SEQUENCE+COMPOUND processing.
**How to avoid:** Per RFC 8881 Section 18.37, if there are in-flight operations on the session's slot table, the server SHOULD return NFS4ERR_BACK_CHAN_BUSY (if back channel is busy) or process the destroy after in-flight ops complete. For simplicity, check if any slot has InUse=true; if so, return NFS4ERR_DELAY and let the client retry.
**Warning signs:** Nil pointer panic accessing destroyed slot table from in-flight COMPOUND.

### Pitfall 8: Extending EvictV41Client Without Session Cleanup
**What goes wrong:** Admin evicts a v4.1 client via REST API, but its sessions remain in sessionsByID, causing stale session lookups.
**Why it happens:** The existing EvictV41Client() in v41_client.go has a TODO comment about session cleanup but doesn't implement it.
**How to avoid:** This phase MUST extend EvictV41Client() to destroy all sessions for the evicted client (remove from sessionsByID and sessionsByClientID).
**Warning signs:** Stale session remains accessible after client eviction.

## Code Examples

### CreateSession Handler Pattern

```go
// Source: Based on existing exchange_id_handler.go pattern
func (h *Handler) handleCreateSession(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
    var args types.CreateSessionArgs
    if err := args.Decode(reader); err != nil {
        logger.Debug("CREATE_SESSION: decode error", "error", err, "client", ctx.ClientAddr)
        return &types.CompoundResult{
            Status: types.NFS4ERR_BADXDR,
            OpCode: types.OP_CREATE_SESSION,
            Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
        }
    }

    // Validate callback security params (accept AUTH_NONE + AUTH_SYS only)
    if !hasAcceptableCallbackSecurity(args.CbSecParms) {
        return &types.CompoundResult{
            Status: types.NFS4ERR_ENCR_ALG_UNSUPP,
            OpCode: types.OP_CREATE_SESSION,
            Data:   encodeStatusOnly(types.NFS4ERR_ENCR_ALG_UNSUPP),
        }
    }

    result, cachedReply, err := h.StateManager.CreateSession(
        args.ClientID,
        args.SequenceID,
        args.Flags,
        args.ForeChannelAttrs,
        args.BackChannelAttrs,
        args.CbProgram,
        args.CbSecParms,
    )

    // If cachedReply is non-nil, this is a replay -- return cached response
    if cachedReply != nil {
        return &types.CompoundResult{
            Status: types.NFS4_OK,
            OpCode: types.OP_CREATE_SESSION,
            Data:   cachedReply,
        }
    }

    if err != nil {
        nfsStatus := mapStateError(err)
        return &types.CompoundResult{
            Status: nfsStatus,
            OpCode: types.OP_CREATE_SESSION,
            Data:   encodeStatusOnly(nfsStatus),
        }
    }

    // Encode success response
    res := &types.CreateSessionRes{
        Status:           types.NFS4_OK,
        SessionID:        result.SessionID,
        SequenceID:       result.SequenceID,
        Flags:            result.Flags,
        ForeChannelAttrs: result.ForeChannelAttrs,
        BackChannelAttrs: result.BackChannelAttrs,
    }

    var buf bytes.Buffer
    if err := res.Encode(&buf); err != nil {
        return &types.CompoundResult{
            Status: types.NFS4ERR_SERVERFAULT,
            OpCode: types.OP_CREATE_SESSION,
            Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
        }
    }

    // Cache the encoded response for replay detection
    h.StateManager.CacheCreateSessionResponse(args.ClientID, buf.Bytes())

    logger.Info("CREATE_SESSION: session created",
        "client_id", args.ClientID,
        "session_id", result.SessionID.String(),
        "fore_slots", result.ForeChannelAttrs.MaxRequests,
        "client", ctx.ClientAddr)

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_CREATE_SESSION,
        Data:   buf.Bytes(),
    }
}
```

### DestroySession Handler Pattern

```go
func (h *Handler) handleDestroySession(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
    var args types.DestroySessionArgs
    if err := args.Decode(reader); err != nil {
        return &types.CompoundResult{
            Status: types.NFS4ERR_BADXDR,
            OpCode: types.OP_DESTROY_SESSION,
            Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
        }
    }

    if err := h.StateManager.DestroySession(args.SessionID); err != nil {
        nfsStatus := mapStateError(err)
        return &types.CompoundResult{
            Status: nfsStatus,
            OpCode: types.OP_DESTROY_SESSION,
            Data:   encodeStatusOnly(nfsStatus),
        }
    }

    logger.Info("DESTROY_SESSION: session destroyed",
        "session_id", args.SessionID.String(),
        "client", ctx.ClientAddr)

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_DESTROY_SESSION,
        Data:   encodeStatusOnly(types.NFS4_OK),
    }
}
```

### Session API Response Type

```go
// SessionInfo is the response type for session list endpoints.
type SessionInfo struct {
    SessionID    string    `json:"session_id"`
    ClientID     string    `json:"client_id"`
    CreatedAt    time.Time `json:"created_at"`
    ForeSlots    uint32    `json:"fore_channel_slots"`
    BackSlots    uint32    `json:"back_channel_slots"`
    Flags        uint32    `json:"flags"`
    BackChannel  bool      `json:"back_channel"`
}
```

## State of the Art

| Old Approach (v4.0) | Current Approach (v4.1) | When Changed | Impact |
|---------------------|------------------------|--------------|--------|
| SETCLIENTID_CONFIRM creates client state | CREATE_SESSION creates session + confirms client | NFSv4.1 / RFC 5661/8881 | Session-based state management; client identity separate from session |
| Single callback TCP connection per client | Back channel bound to session, shared TCP | NFSv4.1 | No separate callback port; back channel resources per-session |
| Lease renewal via RENEW operation | Lease renewal via SEQUENCE (in every COMPOUND) | NFSv4.1 | Implicit renewal on every operation; RENEW not needed |
| Per-client state protection via SETCLIENTID | Per-session channel attribute negotiation | NFSv4.1 | Fine-grained resource allocation per session |

**Deprecated/outdated:**
- CREATE_SESSION4_FLAG_PERSIST (RFC 8881 marks this as non-essential; Linux nfsd ignores it)
- RDMA channel attributes (ca_rdma_ird) for non-RDMA transports -- always empty

## Open Questions

1. **Per-Client Session Limit Number**
   - What we know: Linux nfsd has a global DRC limit (typically 1024 sessions total) but no per-client limit. Most clients create 1-2 sessions.
   - Recommendation: **16 sessions per client**. This is generous (most clients use 1-3) while preventing a single misbehaving client from consuming all resources. Configurable if needed in the future.

2. **DESTROY_SESSION with In-Flight Requests**
   - What we know: RFC 8881 Section 18.37 says "If the session has any remaining requests that are still in progress, the server MAY refuse to destroy the session."
   - Recommendation: Check if any fore channel slot has InUse=true. If so, return NFS4ERR_DELAY. The client will retry. This is the simplest correct behavior.

3. **Unconfirmed Client Timeout**
   - What we know: Unconfirmed v4.1 clients (after EXCHANGE_ID but before CREATE_SESSION) consume memory but have no lease timer yet.
   - Recommendation: The session reaper should also sweep unconfirmed clients older than 2x the lease duration. This prevents memory leaks from clients that EXCHANGE_ID but never CREATE_SESSION.

4. **Connection Binding for DESTROY_SESSION**
   - What we know: Per RFC 8881 Section 18.37, only connections bound to the session can destroy it. Connection binding is Phase 21 (BIND_CONN_TO_SESSION).
   - Recommendation: Until Phase 21, allow any connection from the same client (verified by session ownership via clientID match) to destroy a session. Add a TODO for Phase 21 connection binding enforcement.

## Sources

### Primary (HIGH confidence)
- RFC 8881 Section 18.36 - CREATE_SESSION operation specification (authoritative)
- RFC 8881 Section 18.37 - DESTROY_SESSION operation specification (authoritative)
- RFC 8881 Section 2.10 - Session model overview (authoritative)
- Existing codebase: `internal/protocol/nfs/v4/types/create_session.go` - Complete XDR types (verified, tested in Phase 16)
- Existing codebase: `internal/protocol/nfs/v4/types/destroy_session.go` - Complete XDR types (verified, tested in Phase 16)
- Existing codebase: `internal/protocol/nfs/v4/state/session.go` - Session struct, NewSession (verified, Phase 17)
- Existing codebase: `internal/protocol/nfs/v4/state/slot_table.go` - SlotTable with validation (verified, Phase 17)
- Existing codebase: `internal/protocol/nfs/v4/state/v41_client.go` - V41ClientRecord with SequenceID (verified, Phase 18)
- Existing codebase: `internal/protocol/nfs/v4/state/manager.go` - StateManager patterns (verified)
- Existing codebase: `internal/protocol/nfs/v4/handlers/exchange_id_handler.go` - Handler pattern template (verified, Phase 18)
- Existing codebase: `internal/controlplane/api/handlers/clients.go` - Client REST API pattern (verified, Phase 18)
- Existing codebase: `pkg/apiclient/clients.go` - Client API methods pattern (verified, Phase 18)
- Existing codebase: `cmd/dfsctl/commands/client/` - CLI command pattern (verified, Phase 18)
- Existing codebase: `internal/protocol/nfs/v4/types/constants.go` - CREATE_SESSION4_FLAG_*, NFS4ERR_* codes (verified)

### Secondary (MEDIUM confidence)
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) - Full specification
- [Linux kernel nfsd documentation](https://docs.kernel.org/next/filesystems/nfs/nfs41-server.html) - Reference implementation behavior (session limits, channel negotiation)
- [Linux NFS Client sessions issues](http://wiki.linux-nfs.org/wiki/index.php/Client_sessions_Implementation_Issues) - Client perspective on session edge cases

### Tertiary (LOW confidence)
- Exact NFS4ERR code for per-client session limit exceeded -- not specified in RFC, NFS4ERR_RESOURCE recommended based on training data

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use, no new dependencies
- Architecture: HIGH - Follows established StateManager + handler + API patterns exactly from Phase 18
- Pitfalls: HIGH - Based on direct RFC reading, existing codebase analysis, and Phase 17/18 implementation experience
- REST API/CLI: HIGH - Follows existing client handler pattern (Phase 18) with nested routes
- Channel negotiation: HIGH - RFC is prescriptive; server MUST clamp, never exceed client values
- Replay detection: HIGH - RFC 8881 Section 18.36.4 describes the exact algorithm

**Research date:** 2026-02-21
**Valid until:** 2026-04-21 (stable domain, RFC 8881 is finalized)
