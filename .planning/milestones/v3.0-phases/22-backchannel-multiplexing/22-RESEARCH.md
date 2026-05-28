# Phase 22: Backchannel Multiplexing - Research

**Researched:** 2026-02-21
**Domain:** NFSv4.1 backchannel callbacks over multiplexed TCP connections (RFC 8881)
**Confidence:** HIGH

## Summary

Phase 22 implements the NFSv4.1 backchannel: the server sends CB_COMPOUND callbacks (specifically CB_SEQUENCE + CB_RECALL) over the client's existing fore-channel TCP connection instead of dialing out to a separate client-hosted callback port. This is the v4.1 replacement for the v4.0 SETCLIENTID callback mechanism.

The existing codebase has strong foundations: v4.0 callback wire-format code in `callback.go` (RPC framing, record marking, CB_COMPOUND encoding, reply parsing), connection binding infrastructure in the StateManager (`connByID`, `connBySession`, `BoundConnection` with direction tracking), session slot tables including `BackChannelSlots`, and all required XDR types (`CbSequenceArgs/Res`, `BackchannelCtlArgs/Res`). The primary work is: (1) a backchannel sender that writes CB_COMPOUND messages to back-bound connections and reads replies from them, (2) a read-loop demux so the connection can handle both fore-channel RPC requests and backchannel RPC replies on the same TCP stream, (3) a BACKCHANNEL_CTL handler to update session callback security, and (4) a callback routing layer that selects v4.0 (dial-out) vs v4.1 (multiplexed) path based on client registration.

**Primary recommendation:** Build a per-session `BackchannelSender` goroutine that pulls from a callback queue, acquires a back-bound connection, sends CB_COMPOUND (CB_SEQUENCE + CB_RECALL) using the existing wire-format helpers, and awaits the reply via an XID-keyed response channel populated by the connection's read loop demux.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Tag each client as v4.0 or v4.1 at registration time (SETCLIENTID vs EXCHANGE_ID) -- callback path is pre-determined, not checked at send time
- Dedicated sender goroutine per session (pulling from a callback queue) -- serializes callbacks naturally, avoids blocking the recall triggerer
- Build an extensible dispatch table for callback operations -- only CB_RECALL implemented now, but CB_NOTIFY (Phase 24) should be trivial to add
- On callback send failure, retry once on another back-bound connection before proceeding to delegation revocation
- Retry with exponential backoff: 3 attempts before declaring failure and revoking delegation
- Backchannel callbacks use exponential backoff timing (e.g., 5s, 10s, 20s between retries)
- Strict validation: return error if session has no backchannel (CREATE_SESSION4_FLAG_CONN_BACK_CHAN was not set)
- Store callback security parameters per-session (not per-client) -- each session can have different security
- Support all three security flavors: AUTH_NONE, AUTH_SYS, and RPCSEC_GSS
- Backchannel params are per-session, matching how sessions own their backchannel slot table
- Pick the back-bound connection with most recent fore-channel activity -- likely healthiest and best for NAT traversal
- Lazy dead-connection detection only -- discover dead connections when callback send fails, no proactive heartbeat/ping
- Callbacks share connections with fore-channel traffic -- callbacks are small and infrequent, no contention avoidance needed
- New `backchannel.go` file in the state package -- clean separation from v4.0 `callback.go`
- Extract shared wire-format code (XDR encoding, record marking, RPC framing) into common helpers used by both v4.0 and v4.1 paths
- No BackchannelSender interface -- concrete struct with methods, avoid premature abstraction
- Sender goroutine lifecycle tied to session destruction -- shuts down when session is destroyed, no orphan goroutines
- Connection read loop demuxes: fore-channel requests go to handler, backchannel responses routed to sender goroutine -- true bidirectional multiplexing on shared connection
- Check if existing connection write path already serializes; if not, add write mutex to prevent interleaving of callback and response writes
- Integration tests with real TCP loopback connections -- test the full wire format including record marking
- No E2E tests for this phase -- E2E delegation recall via backchannel comes in Phase 25
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

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| BACK-01 | Server sends callbacks via CB_SEQUENCE over the client's existing TCP connection (no separate dial) | BackchannelSender goroutine writes CB_COMPOUND (CB_SEQUENCE + CB_RECALL) to back-bound connections; connection read loop demuxes replies via XID matching |
| BACK-03 | Server handles BACKCHANNEL_CTL to update backchannel security and attributes | Replace v41 stub handler with real handler that validates session has backchannel, stores CbProgram and SecParms per-session |
| BACK-04 | Existing CB_RECALL works over backchannel for v4.1 clients (fallback to separate TCP for v4.0) | Callback routing layer in delegation recall checks client version: v4.1 uses BackchannelSender queue, v4.0 uses existing SendCBRecall dial-out |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | 1.24 | Mutex, WaitGroup, channels for sender goroutine | Already used throughout codebase for connection/state management |
| Go stdlib `net` | 1.24 | TCP connection I/O for backchannel writes/reads | Already used in callback.go and nfs_connection.go |
| Go stdlib `context` | 1.24 | Sender goroutine lifecycle, timeout management | Already used for all async operations |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `prometheus/client_golang` | existing | Backchannel metrics | callback_total, callback_failures, callback_duration_seconds |
| `internal/protocol/xdr` | existing | XDR encoding for CB_COMPOUND wire format | Used by all NFS protocol code |
| `internal/protocol/nfs/rpc` | existing | RPC message framing constants | Already used in callback.go |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Per-session goroutine | Channel pool with worker goroutines | Simpler per-session model matches session lifecycle; goroutine overhead is minimal for typical session counts (<100) |
| XID-keyed channel map | Single response channel with demux | XID map enables concurrent callbacks if needed in future; single channel would block |

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/state/
├── callback.go           # v4.0 callback (EXISTING - extract shared helpers)
├── callback_common.go    # Shared wire-format helpers (NEW - extracted from callback.go)
├── backchannel.go        # v4.1 BackchannelSender, CB_COMPOUND over multiplexed conn (NEW)
├── backchannel_test.go   # Integration tests with real TCP loopback (NEW)
├── backchannel_metrics.go # Prometheus metrics for backchannel (NEW)
├── connection.go         # BoundConnection types (EXISTING)
├── manager.go            # StateManager with new backchannel state (MODIFY)
├── delegation.go         # Delegation recall routing v4.0 vs v4.1 (MODIFY)
└── session.go            # Session with backchannel security params (MODIFY)

internal/protocol/nfs/v4/handlers/
├── backchannel_ctl_handler.go   # BACKCHANNEL_CTL handler (NEW)
├── handler.go                    # Wire up handler to dispatch table (MODIFY)
└── sequence_handler.go           # isSessionExemptOp update if needed (CHECK)

pkg/adapter/nfs/
├── nfs_connection.go     # Read loop demux for backchannel replies (MODIFY)
└── nfs_adapter.go        # Pass net.Conn reference for backchannel writes (MODIFY)
```

### Pattern 1: Backchannel Sender Goroutine
**What:** Dedicated goroutine per session that pulls callback requests from a channel, acquires a back-bound connection, sends CB_COMPOUND (CB_SEQUENCE + payload), and waits for the reply.
**When to use:** Whenever the server needs to send a callback to a v4.1 client.
**Example:**
```go
// BackchannelSender manages callback delivery for a single session.
type BackchannelSender struct {
    sessionID  types.SessionId4
    clientID   uint64
    cbProgram  uint32
    queue      chan CallbackRequest
    slotTable  *SlotTable           // backchannel slot table
    sm         *StateManager        // for connection lookup
    stopCh     chan struct{}
    metrics    *BackchannelMetrics
}

// CallbackRequest is a generic callback to send via backchannel.
type CallbackRequest struct {
    OpCode   uint32         // OP_CB_RECALL, OP_CB_NOTIFY, etc.
    Payload  []byte         // Pre-encoded callback operation args
    ResultCh chan error      // Completion signal
}

func (s *BackchannelSender) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case <-s.stopCh:
            return
        case req := <-s.queue:
            err := s.sendCallback(ctx, req)
            if req.ResultCh != nil {
                req.ResultCh <- err
            }
        }
    }
}
```

### Pattern 2: Connection Read Loop Demux
**What:** The connection read loop reads an RPC message, checks `msg_type` (0=CALL, 1=REPLY). CALL messages are fore-channel requests dispatched to handlers. REPLY messages are backchannel responses routed to the sender goroutine via XID-keyed channels.
**When to use:** On every TCP connection that is bound for backchannel traffic.
**Example:**
```go
// In NFSConnection.Serve(), after reading a fragment:
// Parse just the XID and msg_type (first 8 bytes)
xid := binary.BigEndian.Uint32(message[0:4])
msgType := binary.BigEndian.Uint32(message[4:8])

if msgType == rpc.RPCReply {
    // This is a backchannel response (client replying to our CB_COMPOUND)
    c.routeBackchannelReply(xid, message)
    continue  // Don't process as a fore-channel request
}
// Otherwise it's a fore-channel CALL -- process normally
call, err := rpc.ReadCall(message)
```

### Pattern 3: Callback Routing (v4.0 vs v4.1)
**What:** Delegation recall checks whether the delegation owner is a v4.0 or v4.1 client and routes to the appropriate callback mechanism.
**When to use:** In the delegation recall path (currently `sendCBRecallAsync` in `delegation.go`).
**Example:**
```go
func (sm *StateManager) sendCBRecallAsync(deleg *DelegationState) {
    // Check if v4.1 client with backchannel
    if sender := sm.getBackchannelSender(deleg.ClientID); sender != nil {
        // v4.1 path: enqueue CB_RECALL to backchannel sender
        recallOp := encodeCBRecallOp(&deleg.Stateid, false, deleg.FileHandle)
        req := CallbackRequest{
            OpCode:  types.OP_CB_RECALL,
            Payload: recallOp,
        }
        sender.Enqueue(req)
        return
    }
    // v4.0 path: existing dial-out SendCBRecall
    // ... (existing code unchanged)
}
```

### Pattern 4: XID-Keyed Response Routing
**What:** Before sending a CB_COMPOUND, the sender registers an XID in a pending-responses map with a channel. The read loop demux writes the raw reply bytes to that channel when a REPLY with that XID arrives.
**When to use:** Correlating backchannel responses with outstanding callback requests.
**Example:**
```go
// pendingCBReplies is on the NFSConnection (or a shared registry)
type pendingCBReplies struct {
    mu      sync.Mutex
    waiters map[uint32]chan []byte  // XID -> reply channel
}

func (p *pendingCBReplies) Register(xid uint32) chan []byte {
    ch := make(chan []byte, 1)
    p.mu.Lock()
    p.waiters[xid] = ch
    p.mu.Unlock()
    return ch
}

func (p *pendingCBReplies) Deliver(xid uint32, reply []byte) {
    p.mu.Lock()
    ch, ok := p.waiters[xid]
    if ok {
        delete(p.waiters, xid)
    }
    p.mu.Unlock()
    if ok {
        ch <- reply
    }
}
```

### Anti-Patterns to Avoid
- **Blocking the delegation recall caller:** The recall trigger (e.g., OPEN handler detecting conflict) must not block waiting for CB_RECALL to complete. Use async enqueue with fire-and-forget + timer-based revocation.
- **Holding StateManager.mu during network I/O:** The v4.0 path already correctly drops mu before calling SendCBRecall. The v4.1 path must follow the same pattern -- enqueue to the sender, never send on the network while holding any lock.
- **Parsing msg_type deep in the stack:** The demux decision (CALL vs REPLY) must happen at the top of the read loop, before attempting to parse an RPC CALL header. Trying to parse a REPLY as a CALL will fail and drop the message.
- **Using a single global XID counter:** XIDs must not collide between concurrent callbacks on different sessions. Use per-sender atomic counter or crypto/rand, similar to existing `time.Now().UnixNano() & 0xFFFFFFFF` pattern.
- **Writing to connection without writeMu:** The existing `writeMu` on `NFSConnection` already serializes writes. Backchannel writes must also acquire this mutex to prevent interleaving callback messages with fore-channel replies on the same TCP stream.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| RPC record marking | Custom framing code | `addCBRecordMark()` from callback.go | Already correct, well-tested |
| CB_COMPOUND encoding | New encoder | `encodeCBCompound()` from callback.go (adapt for minorversion=1) | Wire format is identical, just change minorversion and omit callback_ident |
| RPC CALL message building | New builder | `buildCBRPCCallMessage()` from callback.go | Reusable for both v4.0 and v4.1 |
| CB_RECALL op encoding | New encoder | `encodeCBRecallOp()` from callback.go | Identical for both versions |
| Reply validation | New parser | `readAndValidateCBReply()` from callback.go | Adapt for reading from XID-routed channel instead of direct conn.Read |
| XDR types | New structs | `CbSequenceArgs`, `CbSequenceRes`, `BackchannelCtlArgs`, `BackchannelCtlRes` already in types/ | Already defined and tested in Phase 16 |
| Slot table | New slot management | `SlotTable` from session.go | BackChannelSlots already allocated in Session when CONN_BACK_CHAN flag set |
| Prometheus metrics pattern | Custom registration | Follow `SessionMetrics` nil-safe receiver pattern | Established project convention (Phase 19+) |

**Key insight:** ~70% of the wire-format code already exists in `callback.go`. The main new logic is the sender goroutine, the read-loop demux, the XID routing, and the BACKCHANNEL_CTL handler.

## Common Pitfalls

### Pitfall 1: TCP Stream Interleaving
**What goes wrong:** Callback messages and fore-channel replies written concurrently corrupt the TCP stream -- partial writes from one interleave with another.
**Why it happens:** Two goroutines (request handler sending reply, backchannel sender sending CB_COMPOUND) write to the same `net.Conn` simultaneously.
**How to avoid:** Always acquire `NFSConnection.writeMu` before writing. The existing code already does this for all reply writes.
**Warning signs:** Corrupt RPC messages, protocol errors on the client side, garbled fragment headers.

### Pitfall 2: Backchannel Reply vs Fore-Channel Request Confusion
**What goes wrong:** The read loop tries to parse a backchannel REPLY (msg_type=1) as a fore-channel CALL (msg_type=0), causing an "invalid RPC call" error and dropping the reply.
**Why it happens:** The read loop was only designed for CALL messages before backchannel multiplexing.
**How to avoid:** Check `msg_type` field (bytes 4-7 of the RPC message) immediately after reading the fragment. Route REPLY messages to the XID-keyed response map, CALL messages to the existing handler path.
**Warning signs:** "Error parsing RPC call" log messages when callbacks are in flight.

### Pitfall 3: Orphaned Sender Goroutines
**What goes wrong:** Sender goroutine continues running after the session is destroyed, leaking a goroutine per destroyed session.
**Why it happens:** Session destruction doesn't signal the sender goroutine to stop.
**How to avoid:** Close the sender's `stopCh` in `destroySessionLocked()` (or the Phase 21 session cleanup path). Use `context.WithCancel` derived from session lifecycle.
**Warning signs:** Goroutine count grows monotonically with session churn.

### Pitfall 4: Deadlock from Lock Ordering Violation
**What goes wrong:** Sender goroutine holds connMu to find a back-bound connection, then needs sm.mu for slot table validation -- but another goroutine holds sm.mu and calls connection code.
**Why it happens:** The established lock ordering is `sm.mu before connMu` (Phase 21 decision). Reversing this causes deadlock.
**How to avoid:** The sender goroutine must acquire connMu.RLock to find a connection, release it, then proceed with the network I/O (no lock held during I/O). Never acquire sm.mu while holding connMu.
**Warning signs:** Server hangs under callback load, goroutine dump shows two goroutines each waiting for the other's lock.

### Pitfall 5: CB_SEQUENCE Slot Mismatch
**What goes wrong:** Server sends CB_SEQUENCE with a slot ID that exceeds the back channel slot table size, causing the client to reject the callback.
**Why it happens:** Back channel slot tables are smaller (default 8 slots vs 64 for fore channel). Using the wrong slot table or hardcoded slot count.
**How to avoid:** Always use `session.BackChannelSlots.GetHighestSlotID()` to determine valid slot range. Start simple: use slot 0 with monotonic seqid, single outstanding callback at a time.
**Warning signs:** Client returns NFS4ERR_BADSLOT or NFS4ERR_SEQ_MISORDERED for callbacks.

### Pitfall 6: Stale Connection Selection
**What goes wrong:** Sender picks a connection that was recently closed (e.g., client dropped it) and the write fails, but the sender doesn't try an alternative.
**Why it happens:** The connection is still in `connBySession` because the server hasn't detected the disconnect yet (lazy detection).
**How to avoid:** On write failure, remove the dead connection from `connBySession`, try the next back-bound connection (per the "retry once on another" decision). Only revoke after all connections exhausted.
**Warning signs:** Callback failures despite the client being connected and healthy.

### Pitfall 7: BACKCHANNEL_CTL on Session Without Backchannel
**What goes wrong:** Client sends BACKCHANNEL_CTL for a session that was created without CREATE_SESSION4_FLAG_CONN_BACK_CHAN, which means no back channel slot table exists.
**Why it happens:** Not validating that the session actually has a backchannel before accepting the update.
**How to avoid:** Check `session.BackChannelSlots != nil` in the BACKCHANNEL_CTL handler. Return `NFS4ERR_INVAL` if no backchannel exists.
**Warning signs:** Nil pointer dereference when accessing backchannel slot table, or silently accepting params that are never used.

## Code Examples

### CB_COMPOUND for v4.1 (CB_SEQUENCE + CB_RECALL)
```go
// encodeCBCompoundV41 encodes CB_COMPOUND4args for minorversion=1.
// Per RFC 8881, v4.1 CB_COMPOUND uses minorversion=1 and callback_ident=0.
func encodeCBCompoundV41(ops [][]byte) []byte {
    var buf bytes.Buffer
    _ = xdr.WriteXDROpaque(&buf, nil)  // tag: empty
    _ = xdr.WriteUint32(&buf, 1)       // minorversion: 1 (key difference from v4.0)
    _ = xdr.WriteUint32(&buf, 0)       // callback_ident: 0 (unused in v4.1)
    _ = xdr.WriteUint32(&buf, uint32(len(ops)))  // operation count
    for _, op := range ops {
        _, _ = buf.Write(op)
    }
    return buf.Bytes()
}
```

### CB_SEQUENCE Encoding (server side)
```go
// encodeCBSequenceOp encodes one CB_SEQUENCE operation for the callback channel.
func encodeCBSequenceOp(sessionID types.SessionId4, seqID, slotID, highestSlotID uint32) []byte {
    var buf bytes.Buffer
    _ = xdr.WriteUint32(&buf, types.OP_CB_SEQUENCE)
    args := &types.CbSequenceArgs{
        SessionID:     sessionID,
        SequenceID:    seqID,
        SlotID:        slotID,
        HighestSlotID: highestSlotID,
        CacheThis:     false,
        ReferringCallLists: nil,
    }
    _ = args.Encode(&buf)
    return buf.Bytes()
}
```

### Sending a v4.1 Callback
```go
func (s *BackchannelSender) sendCallback(ctx context.Context, req CallbackRequest) error {
    // 1. Allocate backchannel slot
    slot, seqID := s.slotTable.AllocateSlot()
    defer s.slotTable.ReleaseSlot(slot)

    // 2. Build CB_COMPOUND: CB_SEQUENCE + payload op
    cbSeqOp := encodeCBSequenceOp(s.sessionID, seqID, slot, s.slotTable.GetHighestSlotID())
    compound := encodeCBCompoundV41([][]byte{cbSeqOp, req.Payload})

    // 3. Build RPC CALL message
    xid := atomic.AddUint32(&s.nextXID, 1)
    callMsg := buildCBRPCCallMessage(xid, s.cbProgram,
        types.NFS4_CALLBACK_VERSION, types.CB_PROC_COMPOUND, compound)
    framedMsg := addCBRecordMark(callMsg, true)

    // 4. Find back-bound connection, register XID, write, wait for reply
    conn, replyCh, err := s.acquireConnectionAndRegister(xid)
    if err != nil {
        return err
    }
    if _, err := conn.WriteCallback(framedMsg); err != nil {
        return s.handleSendFailure(ctx, req, err)
    }

    // 5. Wait for reply with timeout
    select {
    case replyBytes := <-replyCh:
        return validateCBCompoundReply(replyBytes)
    case <-time.After(s.callbackTimeout):
        return fmt.Errorf("callback timeout after %v", s.callbackTimeout)
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

### BACKCHANNEL_CTL Handler
```go
func (h *Handler) handleBackchannelCtl(
    ctx *types.CompoundContext,
    v41ctx *types.V41RequestContext,
    reader io.Reader,
) *types.CompoundResult {
    var args types.BackchannelCtlArgs
    if err := args.Decode(reader); err != nil {
        return errResult(types.OP_BACKCHANNEL_CTL, types.NFS4ERR_BADXDR)
    }

    // Validate session has a backchannel
    sess := v41ctx.Session
    if sess.BackChannelSlots == nil {
        return errResult(types.OP_BACKCHANNEL_CTL, types.NFS4ERR_INVAL)
    }

    // Validate at least one acceptable security flavor
    if !HasAcceptableCallbackSecurity(args.SecParms) {
        return errResult(types.OP_BACKCHANNEL_CTL, types.NFS4ERR_ENCR_ALG_UNSUPP)
    }

    // Update session backchannel parameters
    h.StateManager.UpdateBackchannelParams(sess.SessionID, args.CbProgram, args.SecParms)

    // Encode success response
    res := &types.BackchannelCtlRes{Status: types.NFS4_OK}
    var buf bytes.Buffer
    _ = res.Encode(&buf)
    return okResult(types.OP_BACKCHANNEL_CTL, buf.Bytes())
}
```

## State of the Art

| Old Approach (v4.0) | Current Approach (v4.1) | When Changed | Impact |
|---------------------|------------------------|--------------|--------|
| Server dials out to client's callback address | Server sends callbacks over client's existing TCP connection | NFSv4.1 (RFC 8881) | Works through NAT/firewalls, no client-side listener needed |
| SETCLIENTID provides callback address | CREATE_SESSION + BIND_CONN_TO_SESSION establishes backchannel | NFSv4.1 | Connection binding replaces address-based callbacks |
| No sequence validation on callbacks | CB_SEQUENCE provides exactly-once semantics | NFSv4.1 | Prevents duplicate callback processing |
| Global callback program from SETCLIENTID | Per-session callback program from CREATE_SESSION / BACKCHANNEL_CTL | NFSv4.1 | Per-session isolation of callback parameters |

**Note on Linux nfsd:** The Linux kernel nfsd marks CB_SEQUENCE as an optional/unimplemented feature. This means DittoFS is implementing a feature not yet available in the reference implementation. The specification is clear, but there is limited reference implementation to compare against. This increases the importance of integration testing.

## Discretion Recommendations

Based on research, here are recommendations for the areas left to Claude's discretion:

| Area | Recommendation | Rationale |
|------|---------------|-----------|
| No back-bound connection behavior | Queue for 5s, then revoke | Matches the v4.0 pattern of short revocation timer when callback path is unavailable |
| Callback timeout | 10s per attempt | Longer than v4.0's 5s CBCallbackTimeout because backchannel shares a busy connection; reduces false timeouts |
| Backchannel slot table sizing | Use negotiated count from CREATE_SESSION (default max 8) | Already implemented in `NewSession()` |
| CB_SEQUENCE EOS | Simplified: monotonic seqid per slot, no replay cache | Server controls the seqid; duplicates only possible on retry which the server controls |
| Callback security | AUTH_NULL initially | Matches Linux client behavior; RPCSEC_GSS on backchannel is widely unimplemented per kernel docs |
| BACKCHANNEL_CTL: SEQUENCE required | Yes, requires SEQUENCE (non-exempt) | Per RFC 8881, only EXCHANGE_ID/CREATE_SESSION/DESTROY_SESSION/BIND_CONN_TO_SESSION are exempt |
| BACKCHANNEL_CTL: GSS verification | Deferred (store raw bytes, validate when actually used) | Avoids complexity; GSS on backchannel is not commonly used |
| BACKCHANNEL_CTL: security flavor selection | First acceptable flavor from client's list | Simple, matches CREATE_SESSION handler pattern |
| BACKCHANNEL_CTL: metrics | Follow SessionMetrics nil-safe pattern | Consistency with existing codebase |
| Connection failover | Immediate switch on failure | No reason to wait; the dead connection won't recover |
| Sender goroutine start timing | Lazy (start on first callback enqueue or on first BIND_CONN_TO_SESSION with back-channel direction) | Avoids goroutine overhead for sessions that never receive callbacks |
| Callback queue depth | 64 (buffered channel) | Matches max fore-channel slot count; unlikely to have more outstanding callbacks than that |

## Open Questions

1. **Connection reference for backchannel writes**
   - What we know: The `NFSConnection` struct holds `conn net.Conn` and `writeMu`. Backchannel writes need access to both.
   - What's unclear: How the `BackchannelSender` gets a reference to the `net.Conn` and `writeMu` for a back-bound connection. The `BoundConnection` in StateManager only has `ConnectionID`, not the actual `net.Conn`.
   - Recommendation: Add a `ConnWriter` interface or store `*NFSConnection` references in a registry on the adapter, keyed by connectionID. The sender looks up the NFSConnection by ID and calls `WriteCallback()` (new method using existing `writeMu`). Alternatively, add a `net.Conn` field to `BoundConnection` or a parallel map.

2. **Read loop demux placement**
   - What we know: The demux must happen in `NFSConnection.Serve()` before `rpc.ReadCall()`.
   - What's unclear: Whether demux should be in `readRequest()` or in the `Serve()` loop itself.
   - Recommendation: Modify `readRequest()` to return an additional flag indicating message type (CALL vs REPLY), or handle REPLY routing inside `readRequest()` before it tries to parse an RPC CALL. The REPLY raw bytes get routed to a `pendingCBReplies` map on the NFSConnection.

3. **GetStatusFlags update for backchannel health**
   - What we know: Current code sets CB_PATH_DOWN and BACKCHANNEL_FAULT when `session.BackChannelSlots == nil`. Phase 22 needs to clear these when a backchannel connection is bound.
   - What's unclear: Exact condition for "backchannel is healthy" vs "backchannel has fault".
   - Recommendation: CB_PATH_DOWN cleared when at least one back-bound connection exists for any session of this client. BACKCHANNEL_FAULT set only when a callback send actually fails (not just because no back connection exists yet).

## Sources

### Primary (HIGH confidence)
- RFC 8881 Sections 2.10.3 (backchannel), 18.33 (BACKCHANNEL_CTL), 20.9 (CB_SEQUENCE) - [Full RFC](https://datatracker.ietf.org/doc/html/rfc8881)
- Existing codebase: `callback.go` (v4.0 wire format), `connection.go` (BoundConnection types), `session.go` (BackChannelSlots), `manager.go` (connByID/connBySession maps)
- Existing types: `cb_sequence.go`, `backchannel_ctl.go` (XDR encode/decode already implemented in Phase 16)

### Secondary (MEDIUM confidence)
- [Linux kernel nfsd v4.1 server docs](https://docs.kernel.org/next/filesystems/nfs/nfs41-server.html) - Notes CB_SEQUENCE as unimplemented, GSS on backchannel not widely used
- [Linux NFS client session implementation issues](https://linux-nfs.org/wiki/index.php/Client_sessions_Implementation_Issues) - Backchannel binding to fore-channel connection

### Tertiary (LOW confidence)
- None -- all critical claims verified against RFC and codebase

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use, no new dependencies
- Architecture: HIGH - Patterns well-established by Phase 21 connection management and existing callback code
- Pitfalls: HIGH - Identified from direct codebase analysis (lock ordering, write serialization, read loop structure)
- Wire format: HIGH - Existing callback.go provides verified patterns; types already defined in Phase 16
- BACKCHANNEL_CTL handler: HIGH - XDR types exist, stub handler exists, pattern matches BIND_CONN_TO_SESSION handler

**Research date:** 2026-02-21
**Valid until:** 2026-03-21 (stable - RFC-based protocol work)
