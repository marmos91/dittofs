# Phase 23: Client Lifecycle and Cleanup - Research

**Researched:** 2026-02-22
**Domain:** NFSv4.1 client lifecycle operations (DESTROY_CLIENTID, RECLAIM_COMPLETE, FREE_STATEID, TEST_STATEID, v4.0 rejection)
**Confidence:** HIGH

## Summary

Phase 23 implements five RFC 8881 operations that complete the NFSv4.1 client lifecycle: DESTROY_CLIENTID for graceful client teardown, RECLAIM_COMPLETE for grace period completion signaling, FREE_STATEID and TEST_STATEID for stateid lifecycle management, and rejection of v4.0-only operations in minorversion=1 compounds. All XDR types already exist with Encode/Decode/tests from Phase 16. All state infrastructure exists in the `state/` package (StateManager, GracePeriodState, stateid validation). The work is primarily:

1. **State methods** in `state/` package -- extending existing StateManager with DestroyClientID, ReclaimComplete, FreeStateid, TestStateid
2. **Handler files** in `handlers/` -- replacing stubs with real implementations following the established V41OpHandler pattern
3. **v4.0 rejection** -- dispatch-level enforcement for 5 v4.0-only ops in minorversion=1 compounds
4. **Grace period API/CLI** -- health endpoint enrichment, admin force-end endpoint, `dfsctl grace` commands
5. **Tests** -- real StateManager tests (no mocks), handler tests, race condition coverage

**Primary recommendation:** Implement in dependency order: (1) state methods, (2) handlers + v4.0 rejection, (3) grace API/CLI, (4) tests with race detection.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- **DESTROY_CLIENTID strict RFC compliance**: Reject with NFS4ERR_CLIENTID_BUSY if any sessions remain -- client must destroy all sessions first
- **No API exposure for destroy events**: Client destroy events are NOT exposed via REST API -- logs only
- **Structured logging**: Log destroy events with client ID, session count, state count -- consistent with Phase 21 observability
- **Fixed 90-second grace period**: RFC default, not configurable
- **Grace period visible in health endpoint AND `dfs status`**
- **`dfs status` shows countdown**: "Grace period: 47s remaining (3/5 clients reclaimed)" format
- **Admin API to force-end grace period**: REST endpoint for fast recovery in dev/test
- **`dfsctl grace status` and `dfsctl grace end` commands**: CLI wrappers for the admin API
- **Structured logging for RECLAIM_COMPLETE**: Log events with client ID, reclaim duration, number of states reclaimed
- **TEST_STATEID returns per-stateid error codes**: RFC 5661 compliance, not fail-on-first
- **FREE_STATEID does NOT trigger cache flush**: Trust existing COMMIT/cache/WAL flow for data safety
- **Structured logging for FREE_STATEID and TEST_STATEID**: With stateid details, client ID, and result
- **Debug-level logging for v4.0 rejections**: Not noisy in production
- **Per-operation handler files**: `destroy_clientid.go`, `reclaim_complete.go`, `free_stateid.go`, `test_stateid.go`
- **State logic in existing `state/` package**: Extend `state/client.go`, `state/grace.go`, `state/stateid.go`
- **Extend existing StateManager methods** rather than adding new ones where possible
- **No mocks**: Test against real in-memory StateManager with real state setup
- **Race condition tests required**: Concurrent DESTROY_CLIENTID and FREE_STATEID tests with `-race` flag

### Claude's Discretion
- Sync vs async state purging on DESTROY_CLIENTID
- Delegation recall vs immediate revoke
- Lock release timing
- DESTROY_CLIENTID idempotency
- Prometheus metrics for lifecycle operations
- Per-client grace unlock semantics
- Grace period state persistence strategy
- Auto-end grace when all clients reclaim
- Health endpoint degraded vs healthy during grace
- Freeable stateid types, cascading behavior, batch limits, special stateid handling
- v4.0 rejection scope, dispatch point, and testing approach
- Implementation order
- K8s operator integration specifics

### Deferred Ideas (OUT OF SCOPE)
- K8s operator grace-aware rolling updates -- evaluate during operator sync, may be out of scope for this phase
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| LIFE-01 | Server handles DESTROY_CLIENTID for graceful client cleanup (all sessions destroyed first) | Existing `purgeV41Client()` handles session teardown, `EvictV41Client()` provides admin-level pattern. New method needs NFS4ERR_CLIENTID_BUSY check (sessions must be zero). XDR types exist in `types/destroy_clientid.go`. |
| LIFE-02 | Server handles RECLAIM_COMPLETE to signal end of grace period reclaim for a client | Existing `GracePeriodState.ClientReclaimed()` handles per-client tracking with auto-end-on-all-reclaimed. New handler calls this, existing state infrastructure is complete. XDR types exist in `types/reclaim_complete.go`. |
| LIFE-03 | Server handles FREE_STATEID to release individual stateids | Existing stateid validation (`ValidateStateid`), `openStateByOther`/`lockStateByOther`/`delegByOther` maps provide lookup. New method removes stateid from maps and cleans up associated state. XDR types exist in `types/free_stateid.go`. |
| LIFE-04 | Server handles TEST_STATEID to batch-validate stateid liveness | Existing `ValidateStateid` can be adapted for per-stateid validation. New method iterates array, returns per-stateid status codes. XDR types exist in `types/test_stateid.go` with 1024-entry limit. |
| LIFE-05 | v4.0-only operations return NFS4ERR_NOTSUPP for minorversion=1 | Existing `dispatchV41()` fallback to `opDispatchTable` for v4.0 ops needs a pre-check. Five ops: SETCLIENTID(35), SETCLIENTID_CONFIRM(36), RENEW(30), OPEN_CONFIRM(20), RELEASE_LOCKOWNER(39). Constants and dispatch infrastructure exist. |
</phase_requirements>

## Standard Stack

### Core (all existing -- no new dependencies)
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go standard library (`sync`, `time`) | Go 1.22+ | Concurrency primitives, timers | Already used throughout state/ |
| `github.com/go-chi/chi/v5` | v5.x | HTTP router for grace API endpoints | Already used in API router |
| `github.com/spf13/cobra` | v1.x | CLI framework for `dfsctl grace` commands | Already used in all CLI commands |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `net/http` | stdlib | Grace period REST endpoints | Health enrichment + admin force-end |
| `encoding/json` | stdlib | API request/response encoding | Grace status endpoint |

### No New Dependencies
This phase adds no new external dependencies. All work extends existing packages.

## Architecture Patterns

### Recommended File Structure

```
internal/protocol/nfs/v4/
├── state/
│   ├── client.go         # EXTEND: add DestroyClientID method (locked decision)
│   ├── v41_client.go     # EXTEND: add DestroyV41ClientID method
│   ├── grace.go          # EXTEND: add GraceStatus(), ForceEndGrace(), per-v4.1 reclaim tracking
│   ├── stateid.go        # EXTEND: add FreeStateid(), TestStateid() methods
│   ├── client_test.go    # EXTEND: add DESTROY_CLIENTID tests
│   ├── grace_test.go     # EXTEND: add RECLAIM_COMPLETE + grace status tests
│   └── stateid_test.go   # EXTEND: add FREE_STATEID + TEST_STATEID tests
├── handlers/
│   ├── destroy_clientid_handler.go      # NEW: DESTROY_CLIENTID handler
│   ├── destroy_clientid_handler_test.go # NEW: handler tests
│   ├── reclaim_complete_handler.go      # NEW: RECLAIM_COMPLETE handler
│   ├── reclaim_complete_handler_test.go # NEW: handler tests
│   ├── free_stateid_handler.go          # NEW: FREE_STATEID handler
│   ├── free_stateid_handler_test.go     # NEW: handler tests
│   ├── test_stateid_handler.go          # NEW: TEST_STATEID handler
│   ├── test_stateid_handler_test.go     # NEW: handler tests
│   ├── handler.go                       # MODIFY: replace stubs with real handlers
│   └── compound.go                      # MODIFY: add v4.0-rejection check in dispatchV41
├── types/
│   ├── destroy_clientid.go  # EXISTS: XDR types already defined
│   ├── reclaim_complete.go  # EXISTS: XDR types already defined
│   ├── free_stateid.go      # EXISTS: XDR types already defined
│   └── test_stateid.go      # EXISTS: XDR types already defined

pkg/controlplane/api/
├── router.go               # MODIFY: add grace period endpoints

internal/controlplane/api/handlers/
├── health.go               # MODIFY: add grace period info to health/readiness

cmd/dfsctl/commands/
├── grace/                  # NEW: grace period CLI commands
│   ├── grace.go           # Parent command
│   ├── status.go          # `dfsctl grace status`
│   └── end.go             # `dfsctl grace end`

cmd/dfs/commands/
├── status.go              # MODIFY: add grace period countdown to status output
```

### Pattern 1: DESTROY_CLIENTID State Method

**What:** Synchronous state purging with NFS4ERR_CLIENTID_BUSY pre-check.
**When to use:** DESTROY_CLIENTID handler calls StateManager method.
**Rationale for sync over async:** The RFC requires that after DESTROY_CLIENTID returns NFS4_OK, the client ID is no longer valid. Async purging would create a window where the client ID appears valid but is being torn down. Sync is simpler and correct.

```go
// In state/v41_client.go or state/client.go -- extend StateManager

// DestroyV41ClientID implements DESTROY_CLIENTID per RFC 8881 Section 18.50.
// Returns NFS4ERR_CLIENTID_BUSY if any sessions remain.
// Returns NFS4ERR_STALE_CLIENTID if client ID not found.
func (sm *StateManager) DestroyV41ClientID(clientID uint64) error {
    sm.mu.Lock()
    defer sm.mu.Unlock()

    record, exists := sm.v41ClientsByID[clientID]
    if !exists {
        return &NFS4StateError{Status: types.NFS4ERR_STALE_CLIENTID, Message: "client ID not found"}
    }

    // RFC 8881: MUST return NFS4ERR_CLIENTID_BUSY if any sessions exist
    if sessions := sm.sessionsByClientID[clientID]; len(sessions) > 0 {
        return &NFS4StateError{Status: types.NFS4ERR_CLIENTID_BUSY,
            Message: fmt.Sprintf("client has %d active sessions", len(sessions))}
    }

    // Purge all state: delegations, open state, lock state, backchannel state
    sm.purgeV41Client(record)

    // Log structured event (locked decision)
    logger.Info("DESTROY_CLIENTID: client destroyed",
        "client_id", clientID,
        "client_addr", record.ClientAddr)

    return nil
}
```

### Pattern 2: v4.0 Rejection in v4.1 Dispatch

**What:** Pre-dispatch check in `dispatchV41` for v4.0-only operations.
**When to use:** Before the v4.0 fallback lookup in the v4.1 dispatch loop.
**Dispatch point:** Best placement is in the dispatch loop of `dispatchV41()` and `dispatchV41Ops()`, at the point where v4.0 fallback happens. Check a static set before dispatching.

```go
// In handlers/compound.go

// v40OnlyOps lists operations that are v4.0-only and MUST return
// NFS4ERR_NOTSUPP when used in a minorversion=1 COMPOUND.
// Per RFC 8881 Section 18.
var v40OnlyOps = map[uint32]bool{
    types.OP_SETCLIENTID:         true,  // op 35
    types.OP_SETCLIENTID_CONFIRM: true,  // op 36
    types.OP_RENEW:               true,  // op 30
    types.OP_OPEN_CONFIRM:        true,  // op 20
    types.OP_RELEASE_LOCKOWNER:   true,  // op 39
}

// In the dispatch loop, before v4.0 fallback:
} else if v40Handler, ok := h.opDispatchTable[opCode]; ok {
    // Check if this is a v4.0-only operation forbidden in v4.1
    if v40OnlyOps[opCode] {
        logger.Debug("NFSv4.1 COMPOUND rejected v4.0-only operation",
            "opcode", opCode,
            "op_name", types.OpName(opCode),
            "client", compCtx.ClientAddr)
        // Must still consume XDR args to prevent desync
        v40Handler(compCtx, reader) // execute handler to consume args
        result = notSuppHandler(opCode)
    } else {
        result = v40Handler(compCtx, reader)
    }
}
```

**Important:** The v4.0 handler must still execute to consume XDR args from the reader stream. An alternative is to consume args then return NOTSUPP. The existing v4.0 handlers already consume their args, so calling them and discarding the result (replacing with NOTSUPP) is simpler. However, some handlers have side effects (e.g., SETCLIENTID creates state). A cleaner approach: create a thin wrapper that decodes args without side effects, or simply return NOTSUPP from the dispatch point and rely on the fact that these ops have known arg sizes. But the safest approach per the existing pattern is: decode args, then return NOTSUPP. Since these ops already exist as handlers in the v4.0 table, the cleanest solution is to intercept BEFORE dispatching and consume args manually or use a dedicated decoder.

**Recommended approach:** Add the check in the dispatch loop. For arg consumption, since all 5 ops have simple fixed-size args, create a small helper that reads and discards them. Or better: the v4.0-only ops are in `opDispatchTable` which has handlers that consume args. We can call the handler (which consumes args) but then override the result with NOTSUPP. This avoids creating side effects if the handlers do state mutations. Wait -- SETCLIENTID/RENEW etc. DO have side effects. So we need to NOT call the handler. Instead, create arg-consuming stubs specific to these 5 ops.

**Revised approach:** In the v4.1 dispatch loop, before the v4.0 fallback, check `v40OnlyOps[opCode]`. If matched, consume the op's XDR args (each has known structure) and return NOTSUPP. This matches the existing stub pattern used for unimplemented v4.1 ops.

### Pattern 3: Grace Period Enrichment

**What:** Expose grace period state through health endpoint and `dfs status`.
**When to use:** On server startup during grace period.

The existing `GracePeriodState` tracks active/duration/expected/reclaimed clients. We need to add:

1. **GraceStatus() method** on StateManager -- returns structured status info
2. **ForceEndGrace() method** on StateManager -- admin API to end grace early
3. **Health endpoint enrichment** -- add grace fields to health response
4. **`dfs status` enrichment** -- add grace countdown to status output
5. **REST API endpoints** -- `GET /api/v1/grace` and `POST /api/v1/grace/end`
6. **`dfsctl grace status` and `dfsctl grace end`** CLI commands

```go
// In state/grace.go -- extend GracePeriodState

type GraceStatusInfo struct {
    Active            bool          `json:"active"`
    RemainingSeconds  float64       `json:"remaining_seconds,omitempty"`
    TotalDuration     time.Duration `json:"total_duration,omitempty"`
    ExpectedClients   int           `json:"expected_clients"`
    ReclaimedClients  int           `json:"reclaimed_clients"`
    StartedAt         time.Time     `json:"started_at,omitempty"`
}

func (g *GracePeriodState) Status() GraceStatusInfo { ... }
```

### Pattern 4: RECLAIM_COMPLETE for v4.1

**What:** RECLAIM_COMPLETE signals that a v4.1 client has finished reclaiming state.
**RFC 8881 Section 18.51:** After a client sends RECLAIM_COMPLETE, the server knows this client will not send any more reclaim requests. The server can then free reclaim-tracking resources for that client.

Key behaviors:
- `OneFS=false` (rca_one_fs=false): Client has finished reclaiming across ALL filesystems
- `OneFS=true` (rca_one_fs=true): Client has finished reclaiming on the current filesystem (per-FS migration). Since DittoFS is single-FS, both behave the same.
- Duplicate RECLAIM_COMPLETE: Return NFS4ERR_COMPLETE_ALREADY
- RECLAIM_COMPLETE outside grace: Return NFS4_OK (idempotent, client may not know grace ended)

The existing `GracePeriodState.ClientReclaimed()` already handles the per-client tracking and auto-end. For v4.1, we need to:
1. Track which v4.1 clients have sent RECLAIM_COMPLETE (per-client flag)
2. Call `ClientReclaimed()` on the grace state
3. Return NFS4ERR_COMPLETE_ALREADY on duplicate

### Pattern 5: FREE_STATEID Implementation

**What:** FREE_STATEID releases a single stateid that the client no longer needs.
**RFC 8881 Section 18.38:**

Key behaviors:
- Can free: lock stateids, open stateids (after CLOSE), delegation stateids (after DELEGRETURN)
- Cannot free: special stateids (all-zeros, all-ones)
- Cannot free: stateid with active locks on it (NFS4ERR_LOCKS_HELD)
- Stale/bad stateid: Return appropriate error
- Lock stateids: Can be freed directly (releases the lock state record)
- Open stateids: Only if no lock stateids reference it (i.e., after all locks released)
- Delegation stateids: Can be freed after DELEGRETURN has been processed

```go
// In state/stateid.go

func (sm *StateManager) FreeStateid(stateid *types.Stateid4) error {
    // Check special stateids -- cannot free
    if stateid.IsSpecialStateid() {
        return &NFS4StateError{Status: types.NFS4ERR_BAD_STATEID, Message: "cannot free special stateid"}
    }

    sm.mu.Lock()
    defer sm.mu.Unlock()

    stateType := stateid.Other[0]

    switch stateType {
    case StateTypeLock:
        return sm.freeLockStateid(stateid)
    case StateTypeOpen:
        return sm.freeOpenStateid(stateid)
    case StateTypeDeleg:
        return sm.freeDelegStateid(stateid)
    default:
        return &NFS4StateError{Status: types.NFS4ERR_BAD_STATEID, Message: "unknown stateid type"}
    }
}
```

### Pattern 6: TEST_STATEID Implementation

**What:** TEST_STATEID validates an array of stateids and returns per-stateid status codes.
**RFC 8881 Section 18.48:**

Key behaviors:
- Always returns NFS4_OK at the top level (the operation itself succeeds)
- Each stateid gets its own status code in the result array
- Valid stateid: NFS4_OK
- Invalid stateid: Appropriate error code (BAD_STATEID, OLD_STATEID, STALE_STATEID, EXPIRED)
- The existing `ValidateStateid` method can be reused but needs a version that doesn't do lease renewal (TEST is read-only)

```go
func (sm *StateManager) TestStateids(stateids []types.Stateid4) []uint32 {
    sm.mu.RLock()
    defer sm.mu.RUnlock()

    results := make([]uint32, len(stateids))
    for i, sid := range stateids {
        results[i] = sm.testSingleStateid(&sid)
    }
    return results
}
```

### Anti-Patterns to Avoid

- **Async state purging for DESTROY_CLIENTID:** The RFC requires synchronous completion. After NFS4_OK, the clientID must be invalid immediately.
- **Global grace period unlock on RECLAIM_COMPLETE:** Per RFC 8881, RECLAIM_COMPLETE is per-client, not global. The server tracks per-client reclaim completion.
- **Calling full ValidateStateid from TEST_STATEID:** ValidateStateid does implicit lease renewal and has side effects. TEST_STATEID needs a read-only validation path.
- **Executing v4.0 handlers for rejected ops:** SETCLIENTID etc. have side effects (state creation). Must consume XDR args without executing business logic.
- **Cache flush on FREE_STATEID:** Locked decision says NO. Trust existing COMMIT/cache/WAL flow.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Grace period timer management | Custom timer logic | Existing `GracePeriodState` with `time.AfterFunc` | Already handles early exit, timer cleanup, callback-outside-lock pattern |
| Stateid validation | New validation logic | Extend existing `ValidateStateid` pattern | Boot epoch check, type routing, seqid comparison already implemented |
| XDR encoding/decoding | Custom parsers | Existing `types/*.go` Encode/Decode methods | All 4 operation types already have complete XDR implementations with tests |
| Structured logging | Custom format | Existing `logger.Info/Debug` with key-value pairs | Consistent with Phase 21 observability patterns |
| CLI command structure | Custom CLI | Existing Cobra pattern from `cmd/dfsctl/commands/` | Follow `client/client.go` parent + subcommand file pattern |
| API endpoints | Custom HTTP handlers | chi router + existing handler patterns | Follow `handlers/health.go` pattern for unauthenticated, `handlers/client.go` for admin |

**Key insight:** Nearly all infrastructure exists. This phase is about wiring existing pieces together with new state methods and handlers.

## Common Pitfalls

### Pitfall 1: XDR Stream Desync in v4.0 Rejection
**What goes wrong:** Returning NOTSUPP for a v4.0-only op without consuming its XDR args causes the reader to be mispositioned for the next op in the COMPOUND.
**Why it happens:** The v4.0 ops have variable-size args (SETCLIENTID has opaque fields). If you skip the handler, the reader still points at the args.
**How to avoid:** Create arg-consuming stubs for the 5 v4.0-only ops, or call the handler but discard the result. Prefer dedicated decoders to avoid side effects.
**Warning signs:** Tests pass individually but fail in multi-op COMPOUND tests.

### Pitfall 2: Lock Ordering Violation in DESTROY_CLIENTID
**What goes wrong:** DESTROY_CLIENTID needs `sm.mu` (for client state) and potentially `connMu` (for connection cleanup). Wrong ordering causes deadlock.
**Why it happens:** The existing `purgeV41Client` acquires `sm.mu` but connection cleanup needs `connMu`.
**How to avoid:** Follow existing lock ordering: `sm.mu` before `connMu`. The existing `destroySessionLocked` already demonstrates this pattern (acquires connMu while holding sm.mu).
**Warning signs:** Deadlock in concurrent destroy tests.

### Pitfall 3: RECLAIM_COMPLETE Outside Grace Period
**What goes wrong:** Client sends RECLAIM_COMPLETE when server is not in grace period. Returning error confuses the client.
**Why it happens:** Server may have already exited grace (timer expired) before client sends RECLAIM_COMPLETE.
**How to avoid:** Per RFC 8881, return NFS4_OK even outside grace period. The operation is effectively a no-op. Only return NFS4ERR_COMPLETE_ALREADY if the client already sent it during the current grace period.
**Warning signs:** Linux NFS client mount failures after server restart.

### Pitfall 4: FREE_STATEID on Open Stateid with Active Locks
**What goes wrong:** Freeing an open stateid while lock stateids reference it leaves orphaned lock state.
**Why it happens:** Open stateids own lock stateids. Freeing the open without freeing locks first creates dangling references.
**How to avoid:** Return NFS4ERR_LOCKS_HELD if the open state has any associated lock states. Client must free lock stateids first.
**Warning signs:** Lock state map grows unboundedly.

### Pitfall 5: TEST_STATEID with Implicit Lease Renewal
**What goes wrong:** TEST_STATEID is a read-only probe but existing ValidateStateid does implicit lease renewal.
**Why it happens:** RFC 7530 says stateid-using operations renew leases. TEST_STATEID is explicitly NOT a lease-renewing operation.
**How to avoid:** Create a separate `testSingleStateid` method that does validation without lease renewal. Do not call `ValidateStateid` directly.
**Warning signs:** Clients that only TEST their stateids never have leases expire.

### Pitfall 6: Grace Period Race with DESTROY_CLIENTID
**What goes wrong:** Client sends DESTROY_CLIENTID while grace period is tracking it as expected client. Grace period never ends because it waits for a client that was destroyed.
**Why it happens:** DESTROY_CLIENTID removes client state but doesn't update grace period tracking.
**How to avoid:** When destroying a v4.1 client during grace period, call `ClientReclaimed()` (or equivalent) to mark it as resolved.
**Warning signs:** Grace period hangs for full 90 seconds even when all live clients have reclaimed.

## Code Examples

### DESTROY_CLIENTID Handler Pattern (follows destroy_session_handler.go)

```go
// In handlers/destroy_clientid_handler.go

func (h *Handler) handleDestroyClientID(
    ctx *types.CompoundContext,
    v41ctx *types.V41RequestContext,
    reader io.Reader,
) *types.CompoundResult {
    var args types.DestroyClientidArgs
    if err := args.Decode(reader); err != nil {
        logger.Debug("DESTROY_CLIENTID: decode error", "error", err, "client", ctx.ClientAddr)
        return &types.CompoundResult{
            Status: types.NFS4ERR_BADXDR,
            OpCode: types.OP_DESTROY_CLIENTID,
            Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
        }
    }

    err := h.StateManager.DestroyV41ClientID(args.ClientID)
    if err != nil {
        nfsStatus := mapStateError(err)
        logger.Debug("DESTROY_CLIENTID: state error",
            "error", err,
            "nfs_status", nfsStatus,
            "client_id", fmt.Sprintf("0x%x", args.ClientID),
            "client", ctx.ClientAddr)
        return &types.CompoundResult{
            Status: nfsStatus,
            OpCode: types.OP_DESTROY_CLIENTID,
            Data:   encodeStatusOnly(nfsStatus),
        }
    }

    logger.Info("DESTROY_CLIENTID: client destroyed",
        "client_id", fmt.Sprintf("0x%x", args.ClientID),
        "client", ctx.ClientAddr)

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_DESTROY_CLIENTID,
        Data:   encodeStatusOnly(types.NFS4_OK),
    }
}
```

### TEST_STATEID Handler Pattern

```go
// In handlers/test_stateid_handler.go

func (h *Handler) handleTestStateid(
    ctx *types.CompoundContext,
    v41ctx *types.V41RequestContext,
    reader io.Reader,
) *types.CompoundResult {
    var args types.TestStateidArgs
    if err := args.Decode(reader); err != nil {
        logger.Debug("TEST_STATEID: decode error", "error", err, "client", ctx.ClientAddr)
        return &types.CompoundResult{
            Status: types.NFS4ERR_BADXDR,
            OpCode: types.OP_TEST_STATEID,
            Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
        }
    }

    statusCodes := h.StateManager.TestStateids(args.Stateids)

    res := &types.TestStateidRes{
        Status:      types.NFS4_OK,
        StatusCodes: statusCodes,
    }
    var buf bytes.Buffer
    if err := res.Encode(&buf); err != nil {
        logger.Error("TEST_STATEID: encode error", "error", err)
        return &types.CompoundResult{
            Status: types.NFS4ERR_SERVERFAULT,
            OpCode: types.OP_TEST_STATEID,
            Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
        }
    }

    logger.Debug("TEST_STATEID: validated stateids",
        "count", len(args.Stateids),
        "client", ctx.ClientAddr)

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_TEST_STATEID,
        Data:   buf.Bytes(),
    }
}
```

### v4.0 Rejection Args Consumption

```go
// In handlers/compound.go -- helper for consuming v4.0-only op args in v4.1 context

// consumeV40OnlyArgs consumes XDR args for v4.0-only operations that are
// rejected in v4.1 COMPOUNDs. This prevents stream desync without executing
// the handler's business logic.
func consumeV40OnlyArgs(opCode uint32, reader io.Reader) error {
    switch opCode {
    case types.OP_SETCLIENTID:
        // client (opaque) + verifier (8 bytes) + callback (cb_program uint32 + cb_location netaddr4)
        // netaddr4 = na_r_netid (opaque) + na_r_addr (opaque)
        // Use existing decode
        _, _ = xdr.DecodeOpaque(reader) // client id string
        _, _ = xdr.DecodeUint64(reader) // verifier (8 bytes as uint64)
        // callback_ident
        _, _ = xdr.DecodeUint32(reader) // cb_program
        _, _ = xdr.DecodeOpaque(reader) // r_netid
        _, _ = xdr.DecodeOpaque(reader) // r_addr
        _, _ = xdr.DecodeUint32(reader) // callback_ident
        return nil
    case types.OP_SETCLIENTID_CONFIRM:
        _, _ = xdr.DecodeUint64(reader) // clientid
        _, _ = xdr.DecodeUint64(reader) // verifier (8 bytes)
        return nil
    case types.OP_RENEW:
        _, _ = xdr.DecodeUint64(reader) // clientid
        return nil
    case types.OP_OPEN_CONFIRM:
        // stateid4 (4 + 12 bytes) + seqid (4 bytes)
        _, _ = xdr.DecodeUint32(reader)   // seqid of stateid
        buf := make([]byte, 12)
        _, _ = io.ReadFull(reader, buf)   // other field
        _, _ = xdr.DecodeUint32(reader)   // seqid arg
        return nil
    case types.OP_RELEASE_LOCKOWNER:
        _, _ = xdr.DecodeUint64(reader)  // clientid
        _, _ = xdr.DecodeOpaque(reader)  // owner
        return nil
    }
    return fmt.Errorf("unknown v4.0-only op: %d", opCode)
}
```

**Note:** A cleaner approach is to reuse the existing handlers to consume args (since they already do this correctly) but wrap them to suppress side effects. However, given that SETCLIENTID/RENEW have state side effects, the dedicated arg-consumer is safer. An even cleaner approach: since these ops have existing XDR types or handler patterns, extract just the decode logic. Actually, looking at the codebase again, the simplest approach is: define a v4.1-specific dispatch table entry for each of these 5 ops that decodes args and returns NOTSUPP, similar to the existing `v41StubHandler` pattern.

### Grace Status API Response

```go
// GET /api/v1/grace -> returns grace period status
// POST /api/v1/grace/end -> force-end grace period (admin only)

type GraceStatusResponse struct {
    Active           bool    `json:"active"`
    RemainingSeconds float64 `json:"remaining_seconds,omitempty"`
    TotalDuration    string  `json:"total_duration,omitempty"`
    ExpectedClients  int     `json:"expected_clients"`
    ReclaimedClients int     `json:"reclaimed_clients"`
    Message          string  `json:"message"`
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| v4.0 SETCLIENTID flow | v4.1 EXCHANGE_ID + CREATE_SESSION | NFSv4.1 (RFC 5661, 2010) | v4.0-only ops must be rejected in v4.1 |
| v4.0 RENEW for lease maintenance | v4.1 SEQUENCE implicit renewal | NFSv4.1 (RFC 5661, 2010) | RENEW is v4.0-only |
| No DESTROY_CLIENTID in v4.0 | v4.1 DESTROY_CLIENTID | NFSv4.1 (RFC 5661, 2010) | Explicit client teardown instead of lease expiry |
| No FREE_STATEID in v4.0 | v4.1 FREE_STATEID | NFSv4.1 (RFC 5661, 2010) | Explicit stateid release |
| DELEGPURGE for reclaim signaling | RECLAIM_COMPLETE in v4.1 | NFSv4.1 (RFC 5661, 2010) | Per-client grace period tracking |

## Open Questions

1. **DESTROY_CLIENTID and v4.0 state**
   - What we know: DESTROY_CLIENTID targets v4.1 clients (registered via EXCHANGE_ID). v4.0 clients use lease expiry.
   - What's unclear: Should DESTROY_CLIENTID also clean up any v4.0 state that might be associated with the same client identity? (Probably not -- v4.0 and v4.1 are separate client records.)
   - Recommendation: Only clean up v4.1 state. v4.0 clients are a separate namespace.

2. **mapStateError coverage for new error codes**
   - What we know: `mapStateError` in `setclientid.go` handles NFS4StateError, ErrStaleClientID, ErrClientIDInUse.
   - What's unclear: Does it handle NFS4ERR_CLIENTID_BUSY and NFS4ERR_COMPLETE_ALREADY?
   - Recommendation: It handles them -- they come through as `NFS4StateError` which is the first check. No change needed.

3. **DESTROY_CLIENTID session exemption**
   - What we know: DESTROY_SESSION is session-exempt. DESTROY_CLIENTID is NOT listed as exempt in `isSessionExemptOp()`.
   - What's unclear: Per RFC 8881 Section 18.50, DESTROY_CLIENTID "MUST NOT be used when the operations have a session." It operates on the client, not a session. It appears in SEQUENCE-gated compounds.
   - Recommendation: DESTROY_CLIENTID should be session-exempt per RFC 8881, since it needs to work even when the client's last session was just destroyed. Check the RFC text. Actually RFC 8881 Section 18.50.3 says "DESTROY_CLIENTID MAY be the only operation in the COMPOUND, or it MAY be preceded by a SEQUENCE operation." So it can work both ways. Since we already have it as a v4.1 stub (not session-exempt), it works in both modes: as an exempt op (single-op compound) or in a SEQUENCE-gated compound.
   - Resolution: Add DESTROY_CLIENTID to `isSessionExemptOp()` so it works both ways.

4. **Grace period for v4.1 clients**
   - What we know: Existing grace period tracks v4.0 client IDs. v4.1 clients have separate records.
   - What's unclear: Should the grace period track both v4.0 and v4.1 clients?
   - Recommendation: Yes. Extend `StartGracePeriod` to accept both v4.0 and v4.1 client IDs. The `GracePeriodState` already uses uint64 client IDs which work for both.

## Implementation Order Recommendation

1. **State methods first** (no handler dependencies):
   - `DestroyV41ClientID()` in `v41_client.go`
   - `FreeStateid()` and `TestStateids()` in `stateid.go`
   - `GraceStatus()` and `ForceEndGrace()` in `grace.go`
   - Per-client RECLAIM_COMPLETE tracking in grace.go
   - State tests for all new methods

2. **Handlers + v4.0 rejection** (depends on state methods):
   - 4 handler files: destroy_clientid, reclaim_complete, free_stateid, test_stateid
   - v4.0 rejection in compound.go dispatch loop
   - Wire handlers into handler.go dispatch table (replace stubs)
   - Handler tests

3. **Grace API/CLI** (depends on grace state methods):
   - Grace status + force-end REST endpoints
   - Health endpoint enrichment
   - `dfs status` grace countdown
   - `dfsctl grace status` and `dfsctl grace end` commands

## Sources

### Primary (HIGH confidence)
- RFC 8881 Section 18.50 (DESTROY_CLIENTID) -- operation semantics, NFS4ERR_CLIENTID_BUSY
- RFC 8881 Section 18.51 (RECLAIM_COMPLETE) -- grace period signaling, NFS4ERR_COMPLETE_ALREADY
- RFC 8881 Section 18.38 (FREE_STATEID) -- stateid release, NFS4ERR_LOCKS_HELD
- RFC 8881 Section 18.48 (TEST_STATEID) -- batch validation, per-stateid status codes
- Existing codebase: `internal/protocol/nfs/v4/state/` -- all state infrastructure
- Existing codebase: `internal/protocol/nfs/v4/types/` -- all XDR types with tests
- Existing codebase: `internal/protocol/nfs/v4/handlers/` -- handler patterns

### Secondary (MEDIUM confidence)
- Linux nfsd implementation (fs/nfsd/nfs4state.c) -- reference for FREE_STATEID cascading behavior
- Existing handler patterns (destroy_session_handler.go, backchannel_ctl_handler.go) -- code conventions

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- no new dependencies, all existing infrastructure
- Architecture: HIGH -- patterns directly match existing codebase (handler files, state methods, dispatch table)
- Pitfalls: HIGH -- identified from RFC requirements and existing codebase analysis
- XDR types: HIGH -- all types already exist with tests from Phase 16

**Research date:** 2026-02-22
**Valid until:** 2026-03-22 (stable -- RFC and codebase patterns well-established)
