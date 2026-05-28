# Phase 20: SEQUENCE and COMPOUND Bifurcation - Research

**Researched:** 2026-02-21
**Domain:** NFSv4.1 SEQUENCE operation, COMPOUND dispatcher bifurcation, exactly-once semantics
**Confidence:** HIGH

## Summary

Phase 20 is the critical integration point where all v4.1 session infrastructure (Phases 16-19) becomes functional. The SEQUENCE operation must gate every v4.1 COMPOUND, providing exactly-once semantics via the slot table built in Phase 17, while v4.0 clients continue working unchanged through the same dispatcher.

The existing codebase already has: (1) the v4.0/v4.1 COMPOUND bifurcation in `compound.go` with `dispatchV40`/`dispatchV41` paths, (2) the `SlotTable` with `ValidateSequence` and `CompleteSlotRequest` methods, (3) `V41RequestContext` and `V41OpHandler` types, (4) a stub SEQUENCE handler that only decodes args and returns NFS4ERR_NOTSUPP, and (5) `StateManager.GetSession()` for session lookup. The primary work is replacing the stub with real SEQUENCE validation, wiring the slot table into the dispatch loop, implementing replay cache at the COMPOUND level, and ensuring per-owner seqid validation is bypassed for v4.1 operations.

**Primary recommendation:** Implement SEQUENCE as a special first-op handler in `dispatchV41()` that validates session/slot/seqid, populates `V41RequestContext`, and wraps the remaining COMPOUND in slot lifecycle management (mark in-use, cache reply, release slot). Keep all handler code in `internal/protocol/nfs/v4/handlers/` co-located with existing compound dispatch. Add Prometheus metrics to `state/` following the `SessionMetrics` pattern.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Strict RFC slot limits: slot count negotiated at CREATE_SESSION and fixed. Excess requests get NFS4ERR_DELAY
- Retention until slot reused: cached response lives until client advances seqid on that slot. No timer-based eviction
- Full encoded XDR response cached per slot: byte-identical replay, no re-encoding. O(1) replay performance
- No global memory cap: each session gets its negotiated slots. Total memory bounded by (sessions x slots x max_response). Session reaper handles cleanup
- Return cached response on replay regardless of argument differences: match by slot+seqid only per RFC
- Lock-free atomics for slot table operations (acquire, release, replay hit) -- NOTE: existing SlotTable uses mutex, not atomics. Claude's discretion whether to refactor or keep mutex
- Reject immediately (NFS4ERR_DELAY) when SEQUENCE arrives for an in-flight slot. No queuing
- Cache key is (sessionID, slotID) only. Any connection bound to the session can get the replay
- RFC error codes to client + detailed server-side logging (session ID, expected vs actual seqid, slot state)
- Full partial results in COMPOUND: return results for all ops up to and including the failing one
- SEQUENCE is pure gating: only validates slot/seqid/session. Semantic conflicts caught by individual operation handlers
- Unified logging: SEQUENCE errors follow the global log level. No separate session-specific log level
- Always include sa_status_flags in SEQUENCE response, even on errors (lease/callback/recallable status)
- All sa_status_flags reported from this phase: lease expiry, backchannel fault, recallable locks, devid notifications (even if some subsystems not yet active)
- No operation count limit on COMPOUNDs: max request size from session negotiation already caps payload
- Dedicated types for v4.0 and v4.1 with shared operation handlers: separate COMPOUND/dispatch types per minor version, common ops imported from v4.0
- Dedicated v4.1 COMPOUND response type: wraps SEQUENCE result + operation results. Compile-time enforcement of SEQUENCE-first
- Handler wrapper pattern for seqid bypass: v4.1 dispatch strips per-owner seqid before calling shared handler. Handler never sees v4.0 seqid concerns
- Mixed minorversion per connection: a single TCP connection can carry both v4.0 and v4.1 COMPOUNDs interleaved
- Echo COMPOUND tag verbatim: copy request tag to response as-is, no validation
- Existing v4.0 tests + new coexistence tests: run all v4.0 COMPOUND scenarios through new dispatcher AND add interleaved v4.0/v4.1 tests
- Concurrent mixed traffic tests: goroutines sending v4.0 and v4.1 COMPOUNDs simultaneously to catch race conditions
- Configurable minor versions via min/max range in NFS adapter config (control plane, per-adapter): nfs_min_minor_version / nfs_max_minor_version. Full stack: model + REST API + dfsctl commands
- Minimal Prometheus metrics: sequence_total, sequence_errors_total, replay_hits_total counters
- Per-session slot utilization gauge: slots_in_use / total_slots per session
- Replay cache memory gauge: total bytes consumed by cached responses across all sessions
- Per-error-type counters for SEQUENCE failures: bad_session, seq_misordered, replay_hit, slot_busy
- No OpenTelemetry tracing for now
- Successful SEQUENCE: DEBUG level logging
- Replay cache hits: INFO level logging (noteworthy production events)
- Always log bifurcation routing at DEBUG: which path each COMPOUND takes (v4.0 or v4.1)
- Log full COMPOUND operation list at DEBUG with XDR-encoded size per operation
- Log operation list as [SEQUENCE, PUTFH, OPEN, GETATTR] format at DEBUG for troubleshooting
- v4.1 COMPOUND processor in internal/protocol/nfs/v41/ (parallel to existing v3/) -- NOTE: CONTEXT says v41/ but existing code already has v4.1 handlers in v4/handlers/ with v41DispatchTable. Research recommends keeping in v4/handlers/ per existing structure
- SEQUENCE handler and slot table logic co-located in v41 package
- Static switch for v4.1 operation dispatch (compile-time checked)
- Import v4.0 handlers directly for shared operations (READ, WRITE, GETATTR, etc.)
- Testing with real in-memory components: real SessionManager, real SlotTable, real dispatch. No mocks
- Table-driven unit tests for SEQUENCE validation edge cases + integration tests for end-to-end COMPOUND flows
- Go benchmark test for SEQUENCE validation + COMPOUND dispatch throughput
- Session-aware request context (SessionContext) flows through all v4.1 operation handlers after SEQUENCE succeeds, carrying session info, slot reference, client ID

### Claude's Discretion
- Dynamic slot table resizing (highest_slotid / target_highest_slotid): Claude decides based on RFC compliance vs complexity tradeoff
- Specific RFC error for missing SEQUENCE in v4.1 COMPOUND: Claude picks NFS4ERR_OP_NOT_IN_SESSION vs NFS4ERR_SEQUENCE_POS
- Stale seqid handling (old vs future): Claude picks based on RFC 8881 Section 18.46
- SessionContext design: whether to extend AuthContext or wrap it as separate struct

### Deferred Ideas (OUT OF SCOPE)
- Refactor NFS operations into shared ops package (internal/protocol/nfs/ops/) grouping common handlers for v4.0/v4.1/future -- create GH issue
- OpenTelemetry tracing for SEQUENCE operations -- may add in a later phase
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SESS-04 | Server handles SEQUENCE as first operation in every v4.1 COMPOUND with slot validation and lease renewal | SlotTable.ValidateSequence already implements RFC 8881 Section 2.10.6.1 algorithm. SEQUENCE handler must: decode args, look up session via StateManager.GetSession(), call ValidateSequence(), populate V41RequestContext, renew lease. All building blocks exist |
| COEX-01 | COMPOUND dispatcher routes minorversion=0 to existing v4.0 path and minorversion=1 to v4.1 path | Already implemented in compound.go: dispatchV40/dispatchV41 switch on minorVersion. Phase 20 adds SEQUENCE enforcement to dispatchV41 and version range gating |
| COEX-02 | v4.0 clients continue working unchanged when v4.1 is enabled | dispatchV40 path unchanged. Need regression tests proving v4.0 operations still work through same dispatcher. Mixed v4.0/v4.1 on same TCP connection already supported by the switch statement |
| COEX-03 | Per-owner seqid validation bypassed for v4.1 operations (slot table provides replay protection) | Per-owner seqid validation lives in StateManager.OpenFile/ConfirmOpen/CloseFile/Lock etc. v4.1 operations go through the same handlers via v4.0 fallback in dispatchV41. Need wrapper pattern to skip seqid for v4.1 callers |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | 1.22+ | Mutex for slot table (existing pattern) | SlotTable already uses sync.Mutex; consistent with codebase |
| Go stdlib `sync/atomic` | 1.22+ | Atomic counters for metrics hot path | Lock-free counters per CONTEXT decision |
| `prometheus/client_golang` | (existing) | Prometheus metrics for SEQUENCE operations | Follows SessionMetrics pattern already in codebase |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| Go stdlib `bytes` | - | XDR encoding/decoding of SEQUENCE args/res | All NFS handler encoding |
| Go stdlib `fmt` | - | Logging session/slot IDs | Standard debug logging |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| sync.Mutex in SlotTable | sync/atomic CAS | CONTEXT says "lock-free atomics" but existing SlotTable uses mutex with complex multi-field updates (InUse + SeqID + CachedReply). Atomic CAS works for single counters but not for the slot acquire/release pattern. Keep mutex for correctness; use atomics only for metric counters |

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/
├── handlers/
│   ├── compound.go           # MODIFY: dispatchV41 adds SEQUENCE gating, replay cache
│   ├── handler.go            # MODIFY: replace SEQUENCE stub with real handler
│   ├── sequence_handler.go   # NEW: SEQUENCE handler implementation
│   ├── compound_test.go      # MODIFY: add v4.1 SEQUENCE+COMPOUND integration tests
│   ├── sequence_handler_test.go  # NEW: table-driven SEQUENCE edge case tests
│   └── context.go            # MODIFY or keep: V41RequestContext populated by SEQUENCE
├── state/
│   ├── slot_table.go         # MODIFY: add CachedReplySize() or similar for metrics
│   ├── session.go            # No change needed
│   ├── manager.go            # MODIFY: add RenewV41Lease() and GetStatusFlags() methods
│   ├── sequence_metrics.go   # NEW: Prometheus metrics for SEQUENCE operations
│   └── v41_client.go         # MODIFY: lease renewal support for v4.1 clients
└── types/
    ├── sequence.go           # Already exists: SequenceArgs, SequenceRes
    ├── constants.go          # Already has SEQ4_STATUS_* flags and NFS4ERR_* codes
    └── types.go              # Already has V41RequestContext
```

### Pattern 1: SEQUENCE-Gated COMPOUND Dispatch (Core Pattern)

**What:** The `dispatchV41` method enforces SEQUENCE as the first operation, validates the session/slot/seqid, and wraps the remaining operations in slot lifecycle management.

**When to use:** Every v4.1 COMPOUND request.

**Example:**
```go
// Source: compound.go dispatchV41 (modified)
func (h *Handler) dispatchV41(compCtx *types.CompoundContext, tag []byte, numOps uint32, reader io.Reader) ([]byte, error) {
    // Step 1: First op MUST be SEQUENCE (or one of the exempt ops)
    firstOpCode, err := xdr.DecodeUint32(reader)
    if err != nil {
        return nil, fmt.Errorf("decode v4.1 first op: %w", err)
    }

    // Per RFC 8881: EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION,
    // BIND_CONN_TO_SESSION do NOT require SEQUENCE first
    if isSessionExemptOp(firstOpCode) {
        // Dispatch exempt op without SEQUENCE context
        return h.dispatchV41Exempt(compCtx, tag, firstOpCode, numOps, reader)
    }

    // SEQUENCE must be first
    if firstOpCode != types.OP_SEQUENCE {
        // Return NFS4ERR_OP_NOT_IN_SESSION for non-exempt ops
        return encodeCompoundResponse(types.NFS4ERR_OP_NOT_IN_SESSION, tag, nil)
    }

    // Step 2: Handle SEQUENCE - validates session, slot, seqid
    seqResult, v41ctx, session, slot := h.handleSequenceOp(compCtx, reader)
    results := []types.CompoundResult{seqResult}

    if seqResult.Status != types.NFS4_OK {
        // SEQUENCE failed - return only SEQUENCE result
        // If it was a replay, seqResult.Data contains the full cached COMPOUND
        if v41ctx != nil && slot != nil && slot.CachedReply != nil {
            // Replay: return cached COMPOUND response directly
            return slot.CachedReply, nil
        }
        return encodeCompoundResponse(seqResult.Status, tag, results)
    }

    // Step 3: Dispatch remaining ops with V41RequestContext
    // ... (remaining ops use v41ctx for session awareness)

    // Step 4: After all ops complete, cache the response and release slot
    response, err := encodeCompoundResponse(lastStatus, tag, results)
    if err != nil {
        return nil, err
    }
    session.ForeChannelSlots.CompleteSlotRequest(
        v41ctx.SlotID, v41ctx.SequenceID,
        v41ctx.CacheThis, response,
    )
    return response, nil
}
```

### Pattern 2: SEQUENCE Handler (Session/Slot Validation)

**What:** Decodes SEQUENCE args, looks up session, validates slot/seqid, builds V41RequestContext.

**When to use:** Called as first operation in every v4.1 COMPOUND.

**Example:**
```go
// Source: sequence_handler.go (new file)
func (h *Handler) handleSequenceOp(
    compCtx *types.CompoundContext, reader io.Reader,
) (types.CompoundResult, *types.V41RequestContext, *state.Session, *state.Slot) {
    var args types.SequenceArgs
    if err := args.Decode(reader); err != nil {
        return errorResult(types.NFS4ERR_BADXDR, types.OP_SEQUENCE), nil, nil, nil
    }

    // Look up session
    session := h.StateManager.GetSession(args.SessionID)
    if session == nil {
        return errorResult(types.NFS4ERR_BADSESSION, types.OP_SEQUENCE), nil, nil, nil
    }

    // Validate slot + seqid via SlotTable
    validation, slot, err := session.ForeChannelSlots.ValidateSequence(
        args.SlotID, args.SequenceID,
    )

    switch validation {
    case state.SeqRetry:
        // Return cached reply from slot
        // ...
    case state.SeqMisordered:
        // Return error from err (*NFS4StateError)
        // ...
    case state.SeqNew:
        // Build V41RequestContext, renew lease
        v41ctx := &types.V41RequestContext{
            SessionID:   args.SessionID,
            SlotID:      args.SlotID,
            SequenceID:  args.SequenceID,
            HighestSlot: args.HighestSlotID,
            CacheThis:   args.CacheThis,
        }
        // Renew v4.1 client lease
        h.StateManager.RenewV41Lease(session.ClientID)
        // Build SEQUENCE response
        res := buildSequenceRes(session, args, h.StateManager.GetStatusFlags(session))
        return successResult(res), v41ctx, session, slot
    }
}
```

### Pattern 3: Per-Owner Seqid Bypass for v4.1

**What:** When a v4.1 COMPOUND calls a shared v4.0 handler (like OPEN, CLOSE, LOCK), the per-owner seqid validation must be skipped because the slot table already provides replay protection.

**When to use:** Any v4.0 handler called from v4.1 dispatch path that uses per-owner seqid.

**Example:**
```go
// Option A: Flag in CompoundContext (simpler)
// Add IsV41 bool to CompoundContext. v4.0 handlers check this flag
// and skip seqid validation when true.

// Option B: Wrapper handler (CONTEXT decision)
// v4.1 dispatch wraps v4.0 handlers to strip seqid concerns:
func v41WrapHandler(v40handler OpHandler) V41OpHandler {
    return func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
        // Set flag indicating v4.1 context (seqid bypass)
        ctx.SkipOwnerSeqid = true
        return v40handler(ctx, reader)
    }
}
```

### Pattern 4: Prometheus Metrics (SequenceMetrics)

**What:** Follows the exact same nil-safe receiver pattern as SessionMetrics.

**When to use:** Recording SEQUENCE outcomes in the hot path.

**Example:**
```go
// Source: state/sequence_metrics.go (new file, follows session_metrics.go pattern)
type SequenceMetrics struct {
    SequenceTotal      prometheus.Counter
    SequenceErrors     *prometheus.CounterVec  // label: error_type
    ReplayHitsTotal    prometheus.Counter
    SlotsInUse         *prometheus.GaugeVec    // label: session_id
    ReplayCacheBytes   prometheus.Gauge
}

func (m *SequenceMetrics) recordSequence() {
    if m == nil { return }
    m.SequenceTotal.Inc()
}

func (m *SequenceMetrics) recordError(errType string) {
    if m == nil { return }
    m.SequenceErrors.WithLabelValues(errType).Inc()
}
```

### Anti-Patterns to Avoid

- **Do NOT re-encode responses on replay:** The cached bytes are the complete COMPOUND response. Return them verbatim.
- **Do NOT validate SEQUENCE args beyond slot/seqid/session:** SEQUENCE is pure gating. No semantic validation of the operations that follow.
- **Do NOT queue requests for busy slots:** Return NFS4ERR_DELAY immediately.
- **Do NOT evict cached replies on a timer:** Retention is until the slot is reused (client advances seqid).
- **Do NOT create a separate v41/ package:** The existing code structure places all v4 handlers in `v4/handlers/` with `v41DispatchTable`. Follow this pattern.
- **Do NOT put business logic in the SEQUENCE handler:** SEQUENCE only validates session membership and slot ownership.
- **Do NOT lock the global StateManager.mu for SEQUENCE validation:** Use the per-SlotTable mutex (already implemented in Phase 17).

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Slot validation algorithm | Custom seqid checks | `SlotTable.ValidateSequence()` | Already implements RFC 8881 Section 2.10.6.1 exactly; handles new/retry/misordered/delay/uncached |
| Session lookup | Manual map traversal | `StateManager.GetSession()` | Thread-safe RLock, O(1) by SessionId4 key |
| Slot completion + cache | Manual slot state management | `SlotTable.CompleteSlotRequest()` | Atomically updates SeqID, clears InUse, copies reply bytes |
| Session ID generation | Custom ID schemes | `NewSession()` using crypto/rand | Already generates 16-byte random session IDs |
| XDR encode/decode | Custom byte manipulation | `SequenceArgs.Decode()` / `SequenceRes.Encode()` | Already implemented and tested in Phase 16 |
| Error mapping | Manual NFS error switch | `mapStateError()` + `NFS4StateError` | Existing pattern extracts Status from typed errors |
| Prometheus metrics | Custom counters | `prometheus/client_golang` with nil-safe receivers | SessionMetrics pattern already proven in codebase |

**Key insight:** Almost all building blocks exist from Phases 16-19. This phase is integration and wiring, not new algorithm design.

## Common Pitfalls

### Pitfall 1: Replay Cache Returns Wrong Level of Response
**What goes wrong:** Returning just the SEQUENCE result on replay instead of the full COMPOUND response.
**Why it happens:** The cached reply must be the entire COMPOUND4res (status + tag + all results), not just the SEQUENCE result. A replay of slot+seqid returns the byte-identical full response.
**How to avoid:** Cache `encodeCompoundResponse(...)` output in `CompleteSlotRequest()`. On replay, return `slot.CachedReply` directly to the caller (bypass the entire dispatch loop).
**Warning signs:** Client re-sends a COMPOUND and gets back only a SEQUENCE result with no operation results.

### Pitfall 2: Slot Not Released on Error/Panic
**What goes wrong:** If a handler panics or an error causes early return, the slot stays InUse=true forever. All subsequent requests to that slot get NFS4ERR_DELAY.
**Why it happens:** The `CompleteSlotRequest()` call is only reached on the happy path.
**How to avoid:** Use `defer` to ensure slot release. Set up a deferred function immediately after SEQUENCE succeeds that calls `CompleteSlotRequest(slotID, seqID, cacheThis, response)` with a sentinel empty response if the real response was never built.
**Warning signs:** Clients get persistent NFS4ERR_DELAY on a specific slot after a server error.

### Pitfall 3: v4.0 Regression from Seqid Bypass
**What goes wrong:** Adding a `SkipOwnerSeqid` flag to CompoundContext and accidentally skipping seqid validation for v4.0 operations.
**Why it happens:** The same handler code is shared between v4.0 and v4.1 dispatch paths.
**How to avoid:** The flag must ONLY be set in the v4.1 dispatch path. The v4.0 dispatch path never touches this flag. Add explicit tests for v4.0 OPEN/CLOSE/LOCK seqid validation continuing to work.
**Warning signs:** v4.0 OPEN replay detection stops working after Phase 20 is merged.

### Pitfall 4: SEQUENCE Error Response Missing sa_status_flags
**What goes wrong:** SEQUENCE error responses return only the status code without sa_status_flags.
**Why it happens:** The SequenceRes XDR encoding skips all fields when Status != NFS4_OK.
**How to avoid:** Per the CONTEXT decision, always include sa_status_flags even on errors. However, the RFC says the union discriminant means only NFS4_OK includes the full response. Resolution: for error responses, encode only status (this IS the RFC behavior). The CONTEXT decision about "always include sa_status_flags even on errors" may conflict with the XDR wire format. The planner should clarify: the status flags are only in the NFS4_OK branch of the union. Errors return void.
**Warning signs:** XDR decode errors on the client when parsing error responses.

### Pitfall 5: Exempt Operations Not Handled Correctly
**What goes wrong:** EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, and BIND_CONN_TO_SESSION must work WITHOUT SEQUENCE as the first operation. If the dispatcher always requires SEQUENCE first, these operations break.
**Why it happens:** RFC 8881 explicitly exempts these operations from the SEQUENCE-first requirement.
**How to avoid:** Before checking for SEQUENCE, check if the first opcode is one of the exempt ops. If so, dispatch it through the v4.1 table without a V41RequestContext (v41ctx=nil, as the stubs already handle).
**Warning signs:** CREATE_SESSION stops working after SEQUENCE enforcement is added.

### Pitfall 6: Mixed v4.0/v4.1 Connection State Collision
**What goes wrong:** A connection carries both v4.0 and v4.1 COMPOUNDs. If connection-level state (like V4ClientState) is shared, v4.1 session context could corrupt v4.0 state or vice versa.
**Why it happens:** Both minor versions share the same TCP connection and the same CompoundContext creation path.
**How to avoid:** CompoundContext is created fresh per COMPOUND RPC call. V41RequestContext is created per COMPOUND (from SEQUENCE). No connection-level state leaks between COMPOUNDs.
**Warning signs:** Stale session IDs appearing in v4.0 operations.

### Pitfall 7: Lease Renewal Not Triggered by SEQUENCE
**What goes wrong:** v4.1 clients' leases expire despite active SEQUENCE operations.
**Why it happens:** v4.0 uses explicit RENEW operations. v4.1 uses SEQUENCE as implicit lease renewal (RFC 8881 Section 8.1.3: "the server MUST update the lease on the client's state").
**How to avoid:** After successful SEQUENCE validation, call a new `StateManager.RenewV41Lease(clientID)` method that updates `V41ClientRecord.LastRenewal` and resets the lease timer.
**Warning signs:** Session reaper destroys sessions of active v4.1 clients.

## Code Examples

### SEQUENCE Response Encoding (Verified from existing types)

```go
// Source: internal/protocol/nfs/v4/types/sequence.go (already implemented)
res := &types.SequenceRes{
    Status:              types.NFS4_OK,
    SessionID:           args.SessionID,
    SequenceID:          args.SequenceID,
    SlotID:              args.SlotID,
    HighestSlotID:       session.ForeChannelSlots.GetHighestSlotID(),
    TargetHighestSlotID: session.ForeChannelSlots.GetTargetHighestSlotID(),
    StatusFlags:         statusFlags, // SEQ4_STATUS_* bitmask
}
var buf bytes.Buffer
res.Encode(&buf)
```

### Slot Validation (Verified from existing slot_table.go)

```go
// Source: internal/protocol/nfs/v4/state/slot_table.go (already implemented)
validation, slot, err := session.ForeChannelSlots.ValidateSequence(slotID, seqID)
switch validation {
case state.SeqNew:
    // slot.InUse is already set to true by ValidateSequence
    // Proceed with COMPOUND dispatch
case state.SeqRetry:
    // slot.CachedReply contains the full response bytes
    // Return directly: slot.CachedReply
case state.SeqMisordered:
    // err is *NFS4StateError with Status field:
    //   NFS4ERR_BADSLOT, NFS4ERR_SEQ_MISORDERED,
    //   NFS4ERR_DELAY, NFS4ERR_RETRY_UNCACHED_REP
}
```

### Session Lookup (Verified from existing manager.go)

```go
// Source: internal/protocol/nfs/v4/state/manager.go (already implemented)
session := h.StateManager.GetSession(args.SessionID) // Thread-safe RLock
if session == nil {
    // Return NFS4ERR_BADSESSION
}
```

### Exempt Operation Check (RFC 8881 Section 18.46)

```go
// Operations that may appear as first op WITHOUT SEQUENCE:
func isSessionExemptOp(opCode uint32) bool {
    switch opCode {
    case types.OP_EXCHANGE_ID,
         types.OP_CREATE_SESSION,
         types.OP_DESTROY_SESSION,
         types.OP_BIND_CONN_TO_SESSION:
        return true
    }
    return false
}
```

### Version Range Gating

```go
// In ProcessCompound, before routing:
if minorVersion < h.minMinorVersion || minorVersion > h.maxMinorVersion {
    return encodeCompoundResponse(types.NFS4ERR_MINOR_VERS_MISMATCH, tag, nil)
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Per-owner seqid (v4.0) | Slot table + SEQUENCE (v4.1) | RFC 5661/8881 | Exactly-once semantics for all operations, not just stateful ones |
| Explicit RENEW (v4.0) | Implicit via SEQUENCE (v4.1) | RFC 5661/8881 | Every COMPOUND renews the lease automatically |
| SETCLIENTID flow (v4.0) | EXCHANGE_ID + CREATE_SESSION (v4.1) | RFC 5661/8881 | Session-based with negotiated channels |

**Key RFC 8881 rules verified:**
- SEQUENCE MUST be first operation in v4.1 COMPOUND (Section 18.46)
- NFS4ERR_SEQUENCE_POS: SEQUENCE appears in any position other than first
- NFS4ERR_OP_NOT_IN_SESSION: Non-exempt op appears as first op in v4.1 COMPOUND
- Exempt ops (no SEQUENCE required): EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, BIND_CONN_TO_SESSION
- SEQUENCE implicitly renews client lease (Section 8.1.3)

## Open Questions

1. **sa_status_flags on error responses**
   - What we know: CONTEXT says "always include sa_status_flags even on errors". RFC XDR union says only NFS4_OK includes resok fields (including status_flags). Error returns void.
   - What's unclear: Whether to violate XDR wire format to include flags on errors
   - Recommendation: Follow RFC XDR union semantics. Status flags are only in NFS4_OK response. The CONTEXT decision may have been aspirational. Log status flags at DEBUG level on errors for server-side observability.

2. **v41/ package vs v4/handlers/ location**
   - What we know: CONTEXT says "v4.1 COMPOUND processor in internal/protocol/nfs/v41/". Existing code has all v4.1 handlers in `internal/protocol/nfs/v4/handlers/` with `v41DispatchTable`.
   - What's unclear: Whether to create a new v41/ package or keep in v4/handlers/
   - Recommendation: Keep in `v4/handlers/` to avoid a massive refactor of existing code (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION handlers are already there). The v41/ split can be the deferred GH issue for shared ops package.

3. **Lock-free atomics vs mutex for slot table**
   - What we know: CONTEXT says "lock-free atomics for slot table operations". Existing SlotTable uses sync.Mutex. Slot operations update multiple fields atomically (InUse, SeqID, CachedReply).
   - What's unclear: Whether to refactor SlotTable to use atomics
   - Recommendation: Keep sync.Mutex for ValidateSequence/CompleteSlotRequest (multi-field atomic update). Use sync/atomic only for metric counters. The mutex scope is per-SlotTable (not global), so contention is bounded by concurrent operations per session.

4. **Dynamic slot resizing**
   - What we know: SlotTable already has SetTargetHighestSlotID(), GetTargetHighestSlotID(). EOS-03 (dynamic slot count adjustment) is marked complete in Phase 17.
   - What's unclear: Whether to implement actual slot reduction based on target_highest_slotid feedback
   - Recommendation: Report target_highest_slotid in SEQUENCE response (already supported). Actual dynamic resizing is a future enhancement -- just report the values for now.

## Sources

### Primary (HIGH confidence)
- **Codebase inspection** (HIGH): `internal/protocol/nfs/v4/handlers/compound.go` -- existing v4.0/v4.1 bifurcation with `dispatchV40`/`dispatchV41`
- **Codebase inspection** (HIGH): `internal/protocol/nfs/v4/state/slot_table.go` -- complete SlotTable with ValidateSequence, CompleteSlotRequest
- **Codebase inspection** (HIGH): `internal/protocol/nfs/v4/types/sequence.go` -- SequenceArgs/SequenceRes with full XDR encode/decode
- **Codebase inspection** (HIGH): `internal/protocol/nfs/v4/handlers/handler.go` -- V41OpHandler type, v41DispatchTable, stub SEQUENCE handler
- **Codebase inspection** (HIGH): `internal/protocol/nfs/v4/state/manager.go` -- GetSession(), sessionsByID, V41ClientRecord
- **Codebase inspection** (HIGH): `internal/protocol/nfs/v4/types/constants.go` -- All SEQ4_STATUS_* flags, NFS4ERR_* codes
- **Codebase inspection** (HIGH): `internal/protocol/nfs/v4/state/openowner.go` -- Per-owner SeqIDValidation (the bypass target)

### Secondary (MEDIUM confidence)
- **RFC 8881** (MEDIUM - verified via web search): SEQUENCE must be first op, exempt ops list (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, BIND_CONN_TO_SESSION), NFS4ERR_OP_NOT_IN_SESSION for non-exempt first ops, NFS4ERR_SEQUENCE_POS for SEQUENCE not first
- **RFC 8881 Section 8.1.3** (MEDIUM): SEQUENCE implicitly renews lease

### Tertiary (LOW confidence)
- [Linux kernel nfsd4_sequence implementation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html) -- referenced for pattern comparison but details not extractable from docs page

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - all libraries already in use in the codebase
- Architecture: HIGH - existing compound.go bifurcation, SlotTable, V41RequestContext, and handler patterns provide clear path
- Pitfalls: HIGH - well-documented RFC rules and existing codebase patterns make pitfalls identifiable
- Seqid bypass: MEDIUM - the interaction between v4.1 dispatch and v4.0 handler seqid validation needs careful implementation

**Research date:** 2026-02-21
**Valid until:** 2026-03-21 (stable domain; NFS RFCs don't change)
