# Phase 18: EXCHANGE_ID and Client Registration - Research

**Researched:** 2026-02-20
**Domain:** NFSv4.1 EXCHANGE_ID (op 42), client record management, REST API client visibility
**Confidence:** HIGH

## Summary

Phase 18 implements the NFSv4.1 EXCHANGE_ID operation (op 42) per RFC 8881 Section 18.35. This is the first operation a v4.1 client sends before CREATE_SESSION. It establishes client identity, negotiates state protection, and provides server identity for trunking detection. The phase also includes REST API endpoints for client visibility and `dfsctl` CLI commands for operational management.

The existing codebase already has: (1) complete XDR types for EXCHANGE_ID args/response in `internal/protocol/nfs/v4/types/exchange_id.go`, (2) all necessary shared types (ClientOwner4, ServerOwner4, NfsImplId4, StateProtect4A/R) in `session_common.go`, (3) all EXCHGID4_FLAG constants in `constants.go`, (4) a v4.1 stub handler in the dispatch table that properly decodes args, and (5) a StateManager with client ID generation, boot epoch, and lease infrastructure. The primary work is: implementing the ExchangeID() method on StateManager with the RFC 8881 multi-case algorithm, writing the handler in `exchange_id_handler.go`, adding a V41ClientRecord struct, adding REST API/CLI endpoints, and replacing the stub.

**Primary recommendation:** Implement ExchangeID on StateManager with separate v4.1 client maps (v41ClientsByID, v41ClientsByOwner), a V41ClientRecord struct with implementation ID tracking, and a server identity singleton. Wire the handler to replace the stub, add REST API client listing, and add `dfsctl client` commands.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Server implementation name: `"dittofs"`, domain: `"dittofs.io"`
- Build date embedded via Go ldflags at compile time
- Hardcoded defaults -- no config.yaml overrides for name/domain
- Log client implementation ID at INFO level on EXCHANGE_ID
- Server scope: hostname-based (simple, no persistence required)
- Separate `V41ClientRecord` struct (not extending v4.0 `ClientRecord`) with shared lease behavior via embedding
- In-memory only -- v4.1 client registrations do not survive server restart
- REST API: `/clients` endpoint showing both v4.0 and v4.1 clients
- Rich client API fields: client ID, address, NFS version, connection time, implementation name/domain, lease status, last renewal time
- Admin eviction: `DELETE /clients/{id}` to force-evict a client
- `dfsctl client list` and `dfsctl client evict` CLI commands
- Server info (server_owner, server_scope, implementation ID) exposed on `/status` endpoint
- major_id: hostname-based recommended
- minor_id: boot epoch timestamp (matches v4.0 client ID generation pattern)
- Flags: `EXCHGID4_FLAG_USE_NON_PNFS` only
- SP4_NONE only -- reject SP4_MACH_CRED and SP4_SSV with proper NFS4 error codes
- Validate SP4 support BEFORE allocating a client record (fail fast)
- v4.1 client registration logic as methods on existing StateManager
- Handler file: `exchange_id_handler.go`
- Testing: unit tests on StateManager + integration tests through dispatch
- E2E testing deferred until more v4.1 ops are ready
- REST API client/server endpoints and dfsctl CLI commands included in scope

### Claude's Discretion
- v4.0/v4.1 client map isolation strategy (shared owner lookup vs completely independent)
- server_owner major_id construction details
- EXCHGID4_FLAG_CONFIRMED_R tracking approach
- V41ClientRecord struct design (which fields to embed vs v4.1-specific)
- Error code selection for SP4_MACH_CRED/SP4_SSV rejection

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SESS-01 | Server handles EXCHANGE_ID to register v4.1 clients with owner/implementation ID tracking | Core deliverable: ExchangeID() on StateManager, V41ClientRecord, exchange_id_handler.go, REST API/CLI |
| TRUNK-02 | Server reports consistent server_owner in EXCHANGE_ID for trunking detection | ServerIdentity singleton with hostname-based major_id + boot epoch minor_id, consistent across all EXCHANGE_ID responses |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `os` | 1.21+ | `os.Hostname()` for server_owner major_id | Standard library, no dependency |
| Go stdlib `crypto/rand` | 1.21+ | Secure random generation (follows existing v4.0 pattern) | Used by existing StateManager |
| Go stdlib `sync` | 1.21+ | RWMutex for client map thread safety | Existing pattern in StateManager |
| Go stdlib `sync/atomic` | 1.21+ | Atomic counters for v4.1 client ID sequence | Existing pattern in StateManager |
| `github.com/go-chi/chi/v5` | existing | REST API routing for client endpoints | Already used by all API routes |
| `github.com/spf13/cobra` | existing | CLI commands for `dfsctl client` | Already used by all CLI commands |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/cli/output` | existing | Table/JSON/YAML output for client list | CLI output formatting |
| `internal/cli/prompt` | existing | Confirmation prompt for client eviction | Interactive eviction |
| `pkg/apiclient` | existing | REST client methods for client endpoints | dfsctl HTTP calls |

## Architecture Patterns

### Recommended Code Structure

```
internal/protocol/nfs/v4/
├── state/
│   ├── manager.go          # Add v4.1 client maps + ExchangeID() method
│   ├── client.go           # Existing v4.0 ClientRecord (unchanged)
│   └── v41_client.go       # NEW: V41ClientRecord, ExchangeIDResult, ServerIdentity
├── handlers/
│   ├── handler.go          # Replace EXCHANGE_ID stub with real handler
│   └── exchange_id_handler.go  # NEW: handleExchangeID handler
└── types/
    └── exchange_id.go      # Already complete (Phase 16)

internal/controlplane/api/handlers/
└── clients.go              # NEW: ClientHandler for /clients endpoints

pkg/apiclient/
└── clients.go              # NEW: Client API methods (ListClients, EvictClient)

cmd/dfsctl/commands/client/
├── list.go                 # NEW: dfsctl client list
└── evict.go                # NEW: dfsctl client evict
```

### Pattern 1: V41ClientRecord with Embedded Lease

**What:** Separate struct for v4.1 client records, sharing lease behavior via LeaseState embedding
**When to use:** Always for v4.1 client tracking

```go
// Source: internal/protocol/nfs/v4/state/v41_client.go
type V41ClientRecord struct {
    // Client identity (from EXCHANGE_ID)
    ClientID       uint64
    OwnerID        []byte   // co_ownerid from client_owner4
    Verifier       [8]byte  // co_verifier from client_owner4

    // Implementation info (from eia_client_impl_id)
    ImplDomain     string
    ImplName       string
    ImplDate       time.Time

    // Server-assigned state
    SequenceID     uint32    // eir_sequenceid, starts at 1
    Confirmed      bool      // true after CREATE_SESSION confirms

    // Connection info
    ClientAddr     string
    CreatedAt      time.Time
    LastRenewal    time.Time

    // Shared lease behavior
    Lease          *LeaseState
}
```

**Design rationale:** v4.0 ClientRecord has v4.0-specific fields (ConfirmVerifier, Callback, OpenOwners, CBPathUp) that don't apply to v4.1. v4.1 has session-based callbacks and implementation ID tracking that v4.0 lacks. Separate structs avoid confusion and wasted fields. The LeaseState pointer is the shared behavior (both v4.0 and v4.1 clients have leases).

### Pattern 2: Server Identity Singleton

**What:** Single ServerIdentity instance created at StateManager construction time
**When to use:** Returned in every EXCHANGE_ID response for trunking consistency

```go
// Source: internal/protocol/nfs/v4/state/v41_client.go
type ServerIdentity struct {
    ServerOwner  types.ServerOwner4   // major_id=hostname, minor_id=bootEpoch
    ServerScope  []byte               // hostname bytes
    ImplID       types.NfsImplId4     // "dittofs" / "dittofs.io" / build date
}
```

The ServerIdentity is computed once at construction and returned by reference for every EXCHANGE_ID call. This guarantees consistency (TRUNK-02 requirement).

### Pattern 3: EXCHANGE_ID Multi-Case Algorithm (RFC 8881 Section 18.35)

**What:** The algorithm for processing EXCHANGE_ID has multiple cases based on existing client state
**When to use:** In StateManager.ExchangeID()

The v4.1 EXCHANGE_ID algorithm is simpler than the v4.0 five-case SETCLIENTID algorithm because v4.1 does not have a separate CONFIRM step -- confirmation happens implicitly via CREATE_SESSION. The key cases:

1. **New client (no existing record for this owner):**
   - Generate new clientID via generateClientID()
   - Create V41ClientRecord with Confirmed=false, SequenceID=1
   - Store in v41ClientsByOwner and v41ClientsByID maps
   - Return clientID, sequenceID=1, flags without CONFIRMED_R

2. **Same owner, same verifier (update or trunking):**
   - If UPD_CONFIRMED_REC_A flag set and client is confirmed: update client properties
   - If UPD_CONFIRMED_REC_A not set: return existing clientID (trunking detection)
   - Return flags with CONFIRMED_R set (if confirmed)
   - Preserve existing sequenceID

3. **Same owner, different verifier (client reboot):**
   - Client has restarted -- generate NEW clientID
   - Replace the old V41ClientRecord
   - New SequenceID=1, Confirmed=false
   - Return flags without CONFIRMED_R
   - Note: actual state cleanup happens in CREATE_SESSION (Phase 19)

4. **Same owner, unconfirmed record exists:**
   - Replace the unconfirmed record with new one
   - Fresh clientID and SequenceID=1

**Key difference from v4.0:** No two-phase SETCLIENTID/SETCLIENTID_CONFIRM. The client record is created immediately and becomes confirmed by CREATE_SESSION.

### Pattern 4: REST API Client Listing

**What:** Unified client endpoint showing both v4.0 and v4.1 clients
**When to use:** Admin debugging and operational visibility

```go
// Source: internal/controlplane/api/handlers/clients.go
type ClientInfo struct {
    ClientID       string    `json:"client_id"`       // hex-encoded
    Address        string    `json:"address"`
    NFSVersion     string    `json:"nfs_version"`     // "4.0" or "4.1"
    ConnectedAt    time.Time `json:"connected_at"`
    LastRenewal    time.Time `json:"last_renewal"`
    LeaseStatus    string    `json:"lease_status"`    // "active", "expired", "unknown"
    ImplName       string    `json:"impl_name,omitempty"`   // v4.1 only
    ImplDomain     string    `json:"impl_domain,omitempty"` // v4.1 only
    Confirmed      bool      `json:"confirmed"`
}
```

### Anti-Patterns to Avoid

- **Sharing v4.0 client maps for v4.1:** v4.0 and v4.1 have fundamentally different lifecycles (SETCLIENTID+CONFIRM vs EXCHANGE_ID+CREATE_SESSION). Mixing them in the same maps causes confusion in state transitions.
- **Mutable ServerIdentity:** server_owner must be identical across ALL EXCHANGE_ID responses within a server instance for trunking detection to work. Never recompute it.
- **Allocating client records before SP4 validation:** Per locked decision, validate SP4_NONE before creating any records. This avoids cleanup on rejection.
- **Using hostname for server_scope differently than major_id:** The scope and major_id should use the same hostname value to maintain consistency.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Client ID generation | Custom random scheme | Existing `generateClientID()` | Already correct: bootEpoch<<32 + atomic sequence |
| Hostname resolution | Manual network interface inspection | `os.Hostname()` | Standard, simple, per locked decision |
| XDR encode/decode for EXCHANGE_ID | Manual byte manipulation | Existing `ExchangeIdArgs.Decode()` / `ExchangeIdRes.Encode()` | Complete and tested in Phase 16 |
| Table/JSON output in CLI | Custom formatting | `internal/cli/output` package | Existing pattern used by all dfsctl commands |
| REST API error responses | Inline error formatting | `handlers.writeJSON()`, `handlers.problemResponse()` | Existing pattern in all API handlers |
| JWT auth middleware | Custom auth | `apiMiddleware.JWTAuth(jwtService)` + `RequireAdmin()` | Existing pattern for admin endpoints |

## Common Pitfalls

### Pitfall 1: CONFIRMED_R Flag in Response
**What goes wrong:** Setting CONFIRMED_R incorrectly causes clients to skip CREATE_SESSION or fail trunking detection.
**Why it happens:** The CONFIRMED_R flag has server-only (response) semantics; it's not an echo of the client's flags.
**How to avoid:** Set CONFIRMED_R in the response flags if and only if the client record was already confirmed by a prior CREATE_SESSION. New or rebooted clients should NOT have CONFIRMED_R set.
**Warning signs:** Linux NFS client sends CREATE_SESSION but gets NFS4ERR_STALE_CLIENTID.

### Pitfall 2: SequenceID Semantics
**What goes wrong:** The eir_sequenceid in EXCHANGE_ID response is misunderstood as a slot sequence ID.
**Why it happens:** The name "sequenceid" is overloaded in NFSv4.1.
**How to avoid:** eir_sequenceid is a CREATE_SESSION sequence counter -- the client must use this value as the csa_sequence in its next CREATE_SESSION request. It starts at 1 for new clients and is incremented each time the client record is confirmed. It is NOT related to slot table sequence IDs.
**Warning signs:** CREATE_SESSION fails with NFS4ERR_SEQ_MISORDERED.

### Pitfall 3: Owner String Comparison for Idempotency
**What goes wrong:** Two different clients get the same client ID because owner comparison is case-sensitive vs case-insensitive.
**Why it happens:** The co_ownerid is an opaque byte array, not a string. Comparison must be byte-exact.
**How to avoid:** Use `bytes.Equal()` for co_ownerid comparison, not string comparison. Store as `[]byte`, not `string`.
**Warning signs:** Client A's EXCHANGE_ID returns client B's client ID.

### Pitfall 4: SP4 Validation Before Allocation
**What goes wrong:** A client record is allocated, then SP4_MACH_CRED is rejected, leaving orphaned state.
**Why it happens:** Natural code flow decodes args then processes -- but SP4 check must happen before any allocation.
**How to avoid:** Per locked decision: check `args.StateProtect.How != SP4_NONE` immediately after decode, return error BEFORE calling StateManager.ExchangeID().
**Warning signs:** Memory leak from orphaned V41ClientRecord objects.

### Pitfall 5: Handler Stub Replacement
**What goes wrong:** Both the stub and real handler exist, or the stub's arg decoder is removed breaking other stubs.
**Why it happens:** Stubs auto-decode args and the real handler must replace the stub entry, not add alongside it.
**How to avoid:** In `handler.go` NewHandler(), replace `h.v41DispatchTable[types.OP_EXCHANGE_ID] = ...` assignment with the real handler. Do NOT remove the stub definition -- other stubs still use v41StubHandler.
**Warning signs:** EXCHANGE_ID returns NFS4ERR_NOTSUPP (stub is still active).

### Pitfall 6: Hostname Resolution Failure
**What goes wrong:** `os.Hostname()` fails on some systems, leaving server_owner empty.
**Why it happens:** Container or sandboxed environments may not have a hostname set.
**How to avoid:** Fallback to `"dittofs-unknown"` if `os.Hostname()` returns an error. Log at WARN level.
**Warning signs:** Empty server_owner breaks client trunking detection.

### Pitfall 7: Client Eviction State Cleanup
**What goes wrong:** Evicting a v4.1 client leaves dangling sessions (Phase 19+).
**Why it happens:** DELETE /clients/{id} only removes the client record but not sessions or state.
**How to avoid:** In Phase 18, eviction only cleans up the client record since sessions don't exist yet. Document that Phase 19 must extend eviction to destroy sessions. Add a TODO comment.
**Warning signs:** Stale session after client eviction.

## Code Examples

### ExchangeID Handler Pattern

```go
// Source: Based on existing setclientid.go handler pattern
func (h *Handler) handleExchangeID(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
    // 1. Decode args (XDR already implemented in Phase 16)
    var args types.ExchangeIdArgs
    if err := args.Decode(reader); err != nil {
        return &types.CompoundResult{
            Status: types.NFS4ERR_BADXDR,
            OpCode: types.OP_EXCHANGE_ID,
            Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
        }
    }

    // 2. Validate SP4 BEFORE any state allocation
    if args.StateProtect.How != types.SP4_NONE {
        // NFS4ERR_ENCR_ALG_UNSUPP for SP4_SSV per RFC 8881
        // NFS4ERR_NOTSUPP for SP4_MACH_CRED (we don't support RPCSEC_GSS machine cred)
        status := types.NFS4ERR_ENCR_ALG_UNSUPP
        if args.StateProtect.How == types.SP4_MACH_CRED {
            status = types.NFS4ERR_NOTSUPP
        }
        return &types.CompoundResult{
            Status: status,
            OpCode: types.OP_EXCHANGE_ID,
            Data:   encodeStatusOnly(status),
        }
    }

    // 3. Log client implementation ID at INFO level (per locked decision)
    if len(args.ClientImplId) > 0 {
        impl := args.ClientImplId[0]
        logger.Info("NFSv4.1 EXCHANGE_ID client",
            "impl_name", impl.Name,
            "impl_domain", impl.Domain,
            "client", ctx.ClientAddr)
    }

    // 4. Delegate to StateManager
    result, err := h.StateManager.ExchangeID(
        args.ClientOwner.OwnerID,
        args.ClientOwner.Verifier,
        args.Flags,
        args.ClientImplId,
        ctx.ClientAddr,
    )
    if err != nil {
        nfsStatus := mapStateError(err)
        return &types.CompoundResult{
            Status: nfsStatus,
            OpCode: types.OP_EXCHANGE_ID,
            Data:   encodeStatusOnly(nfsStatus),
        }
    }

    // 5. Encode response
    res := &types.ExchangeIdRes{
        Status:       types.NFS4_OK,
        ClientID:     result.ClientID,
        SequenceID:   result.SequenceID,
        Flags:        result.Flags,
        StateProtect: types.StateProtect4R{How: types.SP4_NONE},
        ServerOwner:  result.ServerOwner,
        ServerScope:  result.ServerScope,
        ServerImplId: result.ServerImplId,
    }

    var buf bytes.Buffer
    if err := res.Encode(&buf); err != nil {
        return &types.CompoundResult{
            Status: types.NFS4ERR_SERVERFAULT,
            OpCode: types.OP_EXCHANGE_ID,
            Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
        }
    }

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_EXCHANGE_ID,
        Data:   buf.Bytes(),
    }
}
```

### StateManager ExchangeID Method

```go
// Source: Based on existing SetClientID pattern in manager.go
type ExchangeIDResult struct {
    ClientID    uint64
    SequenceID  uint32
    Flags       uint32
    ServerOwner types.ServerOwner4
    ServerScope []byte
    ServerImplId []types.NfsImplId4
}

func (sm *StateManager) ExchangeID(ownerID []byte, verifier [8]byte, flags uint32, clientImplId []types.NfsImplId4, clientAddr string) (*ExchangeIDResult, error) {
    sm.mu.Lock()
    defer sm.mu.Unlock()

    ownerKey := string(ownerID) // byte-exact comparison as map key
    existing := sm.v41ClientsByOwner[ownerKey]

    var record *V41ClientRecord
    var responseFlags uint32

    if existing == nil {
        // Case 1: New client
        record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr)
        responseFlags = types.EXCHGID4_FLAG_USE_NON_PNFS
    } else if existing.Verifier == verifier {
        // Case 2: Same owner, same verifier (update or trunking)
        record = existing
        responseFlags = types.EXCHGID4_FLAG_USE_NON_PNFS
        if existing.Confirmed {
            responseFlags |= types.EXCHGID4_FLAG_CONFIRMED_R
        }
        // Update impl info and address
        if len(clientImplId) > 0 {
            record.ImplDomain = clientImplId[0].Domain
            record.ImplName = clientImplId[0].Name
        }
        record.ClientAddr = clientAddr
        record.LastRenewal = time.Now()
    } else {
        // Case 3: Same owner, different verifier (client reboot)
        sm.purgeV41Client(existing)
        record = sm.createV41Client(ownerID, verifier, clientImplId, clientAddr)
        responseFlags = types.EXCHGID4_FLAG_USE_NON_PNFS
    }

    return &ExchangeIDResult{
        ClientID:     record.ClientID,
        SequenceID:   record.SequenceID,
        Flags:        responseFlags,
        ServerOwner:  sm.serverIdentity.ServerOwner,
        ServerScope:  sm.serverIdentity.ServerScope,
        ServerImplId: []types.NfsImplId4{sm.serverIdentity.ImplID},
    }, nil
}
```

### REST API Client Handler

```go
// Source: Based on existing handler patterns (adapters.go, health.go)
type ClientHandler struct {
    stateManager *state.StateManager
}

func (h *ClientHandler) List(w http.ResponseWriter, r *http.Request) {
    clients := h.stateManager.ListAllClients() // returns []ClientInfo
    writeJSON(w, http.StatusOK, clients)
}

func (h *ClientHandler) Evict(w http.ResponseWriter, r *http.Request) {
    idStr := chi.URLParam(r, "id")
    clientID, err := strconv.ParseUint(idStr, 16, 64)
    if err != nil {
        writeJSON(w, http.StatusBadRequest, problemResponse("invalid client ID"))
        return
    }
    if err := h.stateManager.EvictClient(clientID); err != nil {
        writeJSON(w, http.StatusNotFound, problemResponse("client not found"))
        return
    }
    w.WriteHeader(http.StatusNoContent)
}
```

## State of the Art

| Old Approach (v4.0) | Current Approach (v4.1) | When Changed | Impact |
|---------------------|------------------------|--------------|--------|
| SETCLIENTID + SETCLIENTID_CONFIRM (two-phase) | EXCHANGE_ID + CREATE_SESSION (single-phase client reg) | NFSv4.1 / RFC 5661 | Simpler client registration, eliminates confirm verifier |
| Separate callback address per client | Session-based backchannel (shared TCP connection) | NFSv4.1 | No separate callback ports needed |
| No implementation tracking | nfs_impl_id4 in EXCHANGE_ID | NFSv4.1 | Server can identify client implementations for debugging |
| No trunking support | server_owner for trunking detection | NFSv4.1 | Clients can detect multi-path to same server |

## Open Questions

1. **SP4_MACH_CRED Error Code**
   - What we know: SP4_SSV rejection uses NFS4ERR_ENCR_ALG_UNSUPP per RFC 8881. SP4_MACH_CRED is rejected because DittoFS doesn't enforce RPCSEC_GSS machine credentials.
   - What's unclear: The exact error code for SP4_MACH_CRED rejection. NFS4ERR_NOTSUPP is a safe catch-all. Some implementations use NFS4ERR_INVAL.
   - Recommendation: Use NFS4ERR_ENCR_ALG_UNSUPP for both SP4_SSV and SP4_MACH_CRED as done by Linux nfsd. This is the most compatible choice.

2. **UPD_CONFIRMED_REC_A Flag Behavior**
   - What we know: This flag is set when a client wants to update properties of an already-confirmed client record without creating a new one.
   - What's unclear: Whether to support this flag in Phase 18 or defer to Phase 19 when CREATE_SESSION creates confirmed records.
   - Recommendation: Implement basic UPD_CONFIRMED_REC_A handling now (update impl info and address), but since no records can be confirmed yet (CREATE_SESSION is Phase 19), the flag effectively has no impact until Phase 19. Just ensure the flag bit is properly masked.

3. **StateManager Thread Access for REST API**
   - What we know: REST API handlers run in different goroutines than NFS protocol handlers. StateManager already uses RWMutex.
   - What's unclear: Whether to expose StateManager directly to API handlers or use Runtime as intermediary.
   - Recommendation: Add a `ListAllClients()` and `EvictClient()` method to StateManager that the Runtime proxies. API handler accesses Runtime (consistent with existing adapter/share handlers), Runtime delegates to StateManager.

## Sources

### Primary (HIGH confidence)
- RFC 8881 Section 18.35 - EXCHANGE_ID operation specification (authoritative)
- Existing codebase: `internal/protocol/nfs/v4/types/exchange_id.go` - Complete XDR types (verified, tested)
- Existing codebase: `internal/protocol/nfs/v4/state/manager.go` - StateManager patterns, SetClientID algorithm (verified)
- Existing codebase: `internal/protocol/nfs/v4/handlers/handler.go` - Dispatch table, stub mechanism (verified)
- Existing codebase: `internal/protocol/nfs/v4/types/constants.go` - All EXCHGID4_FLAG, SP4_*, error codes (verified)
- Existing codebase: `internal/protocol/nfs/v4/state/session.go` - Session struct (Phase 17, verified)
- Existing codebase: `internal/protocol/nfs/v4/state/slot_table.go` - SlotTable (Phase 17, verified)
- Existing codebase: `pkg/controlplane/api/router.go` - API routing pattern (verified)
- Existing codebase: `cmd/dfsctl/commands/status.go` - CLI command pattern (verified)

### Secondary (MEDIUM confidence)
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) - Web search confirmed trunking semantics (server_owner major_id/minor_id equality rules)
- [Linux kernel NFSv4.1 server docs](https://www.kernel.org/doc/html/v5.10/filesystems/nfs/nfs41-server.html) - Reference implementation patterns

### Tertiary (LOW confidence)
- SP4_MACH_CRED error code selection (NFS4ERR_ENCR_ALG_UNSUPP vs NFS4ERR_NOTSUPP) - based on training data of Linux nfsd behavior, not directly verified

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use, no new dependencies
- Architecture: HIGH - Follows established StateManager + handler + API patterns exactly
- Pitfalls: HIGH - Based on direct RFC reading and existing v4.0 implementation experience
- REST API/CLI: HIGH - Follows existing adapter/user/share handler patterns exactly

**Research date:** 2026-02-20
**Valid until:** 2026-04-20 (stable domain, RFC is finalized)
