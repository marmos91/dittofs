# Architecture Research: NFSv4.1 Session Integration

**Domain:** NFSv4.1 Protocol Server — Session Infrastructure
**Researched:** 2026-02-20
**Confidence:** HIGH (RFC 8881 is definitive, existing codebase well-understood)

## Executive Summary

NFSv4.1 sessions replace the NFSv4.0 per-owner seqid replay model with a per-session slot table that provides exactly-once semantics (EOS) for all operations. This is not an incremental add — it is a fundamental change to how requests are sequenced, replayed, and associated with connections. The existing StateManager, COMPOUND dispatcher, callback client, and connection model all need significant modifications. However, the existing single-RWMutex StateManager pattern, streaming XDR decoder, and per-connection goroutine model provide a solid foundation.

The core challenge is that NFSv4.1 sessions sit between the connection layer and the operation handlers, mediating every single request. The SEQUENCE operation must be the first operation in every NFSv4.1 COMPOUND (except session-creation operations), and it drives slot-based replay detection, lease renewal, and status flag propagation. This means the COMPOUND dispatcher must be split into v4.0 and v4.1 paths.

## Existing Architecture (v4.0 Baseline)

### What We Have Today

```
                    NFS Adapter (per-connection goroutine)
                              |
                    NFSConnection.handleRPCCall()
                              |
                    [GSS Interception if RPCSEC_GSS]
                              |
                    dispatch by program/version
                              |
              +---------------+---------------+
              |                               |
        MOUNT program               NFS4 program
              |                               |
        mount handlers         Handler.ProcessCompound()
                                              |
                               [minor version check: ==0]
                                              |
                               sequential op dispatch loop
                                              |
                               opDispatchTable[opCode](ctx, reader)
                                              |
                               CompoundContext carries:
                                 - CurrentFH, SavedFH
                                 - Auth (UID/GID/GIDs)
                                 - ClientState (ClientID)
```

### Key Existing Components and Their Roles

| Component | Location | Current Role | v4.1 Impact |
|-----------|----------|-------------|-------------|
| `StateManager` | `state/manager.go` | Client records, open/lock/deleg state, lease timers | Add session maps, slot tables, EOS cache |
| `ClientRecord` | `state/client.go` | SETCLIENTID state, callback info, open owners | Add sessions list, EXCHANGE_ID fields |
| `CompoundContext` | `types/types.go` | Per-COMPOUND mutable state (FH, auth) | Add Session pointer, slot reference |
| `ProcessCompound()` | `handlers/compound.go` | Sequential op dispatch, minor version check | Branch on minorversion=1, enforce SEQUENCE-first |
| `OpHandler` | `handlers/handler.go` | `func(ctx, reader) *CompoundResult` | Unchanged signature, new op registrations |
| `Handler` | `handlers/handler.go` | Dispatch table, Registry, StateManager, PseudoFS | Add session-aware operations |
| `CallbackInfo` | `state/client.go` | Program, NetID, Addr for v4.0 separate TCP callbacks | Replaced by backchannel on fore-channel connection |
| `SendCBRecall()` | `state/callback.go` | Dial separate TCP, send CB_COMPOUND | Rewrite to use bound backchannel connection |
| `NFSConnection` | `nfs_connection.go` | Per-connection goroutine, request read loop | Track bound session(s), support backchannel writes |
| `types/constants.go` | `types/constants.go` | v4.0 op numbers (3-39, 10044) | Add ops 40-58, CB ops 5-14 |

## New Components Required

### 1. Session Manager (within StateManager)

**File:** `state/session.go`

The session is the central new abstraction. Each session has:
- A unique 16-byte session ID
- A fore-channel slot table (client-to-server request sequencing)
- A back-channel slot table (server-to-client callback sequencing)
- A set of bound connections
- A reference to the owning ClientRecord
- Channel attributes (max request/response sizes, max ops, max requests)

```go
// SessionRecord represents an NFSv4.1 session.
type SessionRecord struct {
    SessionID     [16]byte
    Client        *ClientRecord     // owning client
    ForeChannel   *ChannelAttrs
    BackChannel   *ChannelAttrs
    ForeSlotTable *SlotTable        // request sequencing + replay cache
    BackSlotTable *SlotTable        // callback sequencing
    Connections   map[net.Conn]ConnBinding  // bound connections with direction flags
    CreatedAt     time.Time
    Flags         uint32            // CREATE_SESSION flags (persist, backchannel, RDMA)
}

type ChannelAttrs struct {
    HeaderPadSize      uint32
    MaxRequestSize     uint32
    MaxResponseSize    uint32
    MaxResponseSizeCached  uint32  // max size of cached reply for EOS
    MaxOps             uint32      // max operations per COMPOUND
    MaxRequests        uint32      // = number of slots
}

type ConnBinding struct {
    Direction  uint32  // CDFC4_FORE, CDFC4_BACK, CDFC4_FORE_OR_BOTH
    UseRDMA    bool
}
```

**StateManager additions:**
```go
// New maps in StateManager
sessionsByID    map[[16]byte]*SessionRecord
sessionsByConn  map[net.Conn]*SessionRecord  // fast lookup from connection

// New methods
func (sm *StateManager) CreateSession(...) (*SessionRecord, error)
func (sm *StateManager) DestroySession(sessionID [16]byte) error
func (sm *StateManager) BindConnToSession(sessionID [16]byte, conn net.Conn, dir uint32) error
func (sm *StateManager) LookupSession(sessionID [16]byte) *SessionRecord
func (sm *StateManager) GetSessionForConn(conn net.Conn) *SessionRecord
```

### 2. Slot Table with EOS Replay Cache

**File:** `state/slot.go`

The slot table is the heart of exactly-once semantics. Each slot holds:
- A sequence ID (incremented per request on that slot)
- A cached reply (the complete COMPOUND response for replay)
- An in-use flag (prevents parallel use of the same slot)

```go
type SlotTable struct {
    mu    sync.Mutex
    slots []*Slot
}

type Slot struct {
    SequenceID   uint32
    InUse        bool
    CachedReply  []byte   // complete COMPOUND4res for replay
    CachedStatus uint32   // top-level status of cached reply
}

// ProcessSequence validates and claims a slot for a new request.
// Returns: (isReplay, cachedReply, error)
func (st *SlotTable) ProcessSequence(slotID, seqID uint32) (bool, []byte, error)

// CacheReply stores the reply for exactly-once replay.
func (st *SlotTable) CacheReply(slotID uint32, reply []byte, status uint32)

// ReleaseSlot marks a slot as no longer in use.
func (st *SlotTable) ReleaseSlot(slotID uint32)

// HighestSlot returns the current highest slot in use (for server flow control).
func (st *SlotTable) HighestSlot() uint32
```

**EOS sequence validation rules (from RFC 8881 Section 2.10.6.1):**
- `sa_sequenceid == slot.SequenceID + 1`: New request, proceed
- `sa_sequenceid == slot.SequenceID`: Replay, return cached reply
- Difference >= 2 or less than cached (with wraparound): NFS4ERR_SEQ_MISORDERED
- `sa_slotid >= len(slots)`: NFS4ERR_BADSLOT

### 3. EXCHANGE_ID Handler

**File:** `handlers/exchangeid.go`

EXCHANGE_ID replaces SETCLIENTID for NFSv4.1. It establishes or confirms a client ID but does NOT create a session — that is a separate step.

Key differences from SETCLIENTID:
- No callback info (backchannel is established via CREATE_SESSION)
- Returns server owner info (for trunking detection)
- Returns implementation ID and capabilities
- Uses `eir_clientid` (same uint64 as v4.0 ClientID)
- Supports multiple sessions per client

**Integration:** Reuses existing `ClientRecord` but adds:
- `OwnerID` field (the client-provided opaque identifier, replaces `ClientIDString`)
- `ServerOwnerMajorID` / `ServerOwnerMinorID` for trunking
- `Sessions` list on `ClientRecord`

### 4. CREATE_SESSION / DESTROY_SESSION Handlers

**Files:** `handlers/createsession.go`, `handlers/destroysession.go`

CREATE_SESSION:
1. Validates the client ID from EXCHANGE_ID
2. Validates the CREATE_SESSION sequence (separate from slot sequences)
3. Negotiates channel attributes (fore and back)
4. Creates the SlotTable with negotiated number of slots
5. Optionally establishes the backchannel on the calling connection
6. Returns the session ID

DESTROY_SESSION:
1. Looks up the session
2. Drains active slots (waits for in-flight requests)
3. Removes session from all maps
4. Unbinds all connections

### 5. SEQUENCE Handler

**File:** `handlers/sequence.go`

SEQUENCE must be the first operation in every NFSv4.1 COMPOUND. It:
1. Looks up the session by ID
2. Validates and claims the slot (slot ID + sequence ID)
3. Detects replays and returns cached replies
4. Implicitly renews the client's lease
5. Returns status flags (callback path state, lease time, etc.)
6. Sets highest slot ID for server-driven flow control

**Critical integration point:** SEQUENCE result includes `sr_status_flags` that communicate server state to the client:
- `SEQ4_STATUS_CB_PATH_DOWN` — backchannel is down
- `SEQ4_STATUS_CB_GSS_CONTEXTS_EXPIRING` — GSS contexts need refresh
- `SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED` — all state revoked
- `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` — delegations revoked
- etc.

### 6. BIND_CONN_TO_SESSION Handler

**File:** `handlers/bindconn.go`

Associates additional TCP connections with an existing session. Critical for:
- Trunking (multiple connections for throughput)
- Reconnection after network failure without losing session state
- Backchannel binding (marking a connection as usable for callbacks)

### 7. Backchannel Multiplexer

**File:** `state/backchannel.go` (replaces parts of `callback.go`)

NFSv4.1 callbacks use the backchannel of an existing connection rather than a separate TCP connection. This is NAT-friendly and container-friendly.

```go
type BackchannelManager struct {
    mu       sync.RWMutex
    sessions map[[16]byte]*BackchannelState
}

type BackchannelState struct {
    Session     *SessionRecord
    SlotTable   *SlotTable       // back-channel slot table
    Connections []net.Conn       // connections bound for back direction
    NextXID     uint32
}

// SendCallback sends a CB_COMPOUND over the backchannel of a session.
// It uses CB_SEQUENCE as the first operation (v4.1 requirement).
// Selects an available back-channel connection, acquires a slot,
// sends the request, and waits for the reply.
func (bm *BackchannelManager) SendCallback(sessionID [16]byte, ops []byte) error
```

**Key difference from v4.0:** CB_SEQUENCE must be the first operation in every v4.1 CB_COMPOUND, just like SEQUENCE is first in fore-channel COMPOUNDs. The backchannel has its own slot table.

### 8. Directory Delegation Components

**Files:** `handlers/getdirdeleg.go`, `state/dirdeleg.go`

GET_DIR_DELEGATION is a new operation (op 46) that grants a read-only delegation on a directory. The client can then cache directory listings without server round-trips.

```go
type DirDelegationState struct {
    Stateid     types.Stateid4
    ClientID    uint64
    DirHandle   []byte
    Notifications uint32  // bitmask of requested notification types
    Cookieverf  [8]byte
}
```

CB_NOTIFY (callback op 6) sends directory change notifications:
- Entry added/removed/renamed
- Attribute changes on directory entries
- Cookie verifier changes

The notification model allows the client to maintain its cache incrementally rather than recalling the entire delegation.

### 9. DESTROY_CLIENTID Handler

**File:** `handlers/destroyclientid.go`

Graceful client cleanup (op 57). Must destroy all sessions first. Removes the ClientRecord entirely.

## Modified Components

### 1. ProcessCompound() — Bifurcated Dispatch

The COMPOUND dispatcher must branch on `minorversion`:

```go
func (h *Handler) ProcessCompound(compCtx *types.CompoundContext, data []byte) ([]byte, error) {
    // ... decode tag, minorversion, numops (same as today) ...

    switch minorVersion {
    case types.NFS4_MINOR_VERSION_0:
        return h.processCompoundV40(compCtx, tag, numOps, reader)
    case types.NFS4_MINOR_VERSION_1:
        return h.processCompoundV41(compCtx, tag, numOps, reader)
    default:
        return encodeCompoundResponse(types.NFS4ERR_MINOR_VERS_MISMATCH, tag, nil)
    }
}

func (h *Handler) processCompoundV41(compCtx *types.CompoundContext, tag []byte, numOps uint32, reader *bytes.Reader) ([]byte, error) {
    // First op MUST be SEQUENCE (or session-creation op: EXCHANGE_ID, CREATE_SESSION, etc.)
    // Process SEQUENCE: validate slot, check replay, set session on compCtx
    // If replay: return cached COMPOUND response immediately
    // Continue dispatching remaining ops
    // After all ops: cache the response in the slot
}
```

**Session-creation operations that do NOT require SEQUENCE first:**
- EXCHANGE_ID (op 42)
- CREATE_SESSION (op 43)
- DESTROY_SESSION (op 44) — allowed without SEQUENCE per RFC
- DESTROY_CLIENTID (op 57)
- BIND_CONN_TO_SESSION (op 41)

### 2. CompoundContext — Session Awareness

```go
type CompoundContext struct {
    // ... existing fields (CurrentFH, SavedFH, Auth, etc.) ...

    // v4.1 Session Fields
    Session     *SessionRecord  // nil for v4.0
    SlotID      uint32          // current slot (set by SEQUENCE)
    SequenceID  uint32          // current sequence (set by SEQUENCE)
    HighestSlot uint32          // server's current highest slot
    StatusFlags uint32          // SEQ4_STATUS_* flags to return
}
```

### 3. ClientRecord — Multiple Sessions

```go
type ClientRecord struct {
    // ... existing fields ...

    // v4.1 additions
    Sessions         map[[16]byte]*SessionRecord  // multiple sessions per client
    ExchangeIDSeq    uint32          // sequence for EXCHANGE_ID confirm
    ServerOwnerMajor []byte          // for trunking detection
    ImplementationID string          // implementation name
    MinorVersion     uint32          // 0 or 1, set at EXCHANGE_ID/SETCLIENTID
}
```

### 4. NFSConnection — Backchannel Support

```go
type NFSConnection struct {
    // ... existing fields ...

    // v4.1 additions
    boundSession *state.SessionRecord  // session bound to this connection
    bcDirection  uint32                // CDFC4_FORE, CDFC4_BACK, CDFC4_FORE_OR_BOTH
    bcWriteMu    sync.Mutex            // protects backchannel writes
}
```

The connection's Serve loop must be modified to:
1. Support interleaved fore/back channel messages on the same TCP connection
2. Route incoming messages: RPC CALL = fore channel request, RPC REPLY = backchannel response
3. Allow the server to write CB_COMPOUND messages on connections bound for backchannel

### 5. Lease Renewal — Session-Driven

In v4.0, lease renewal is explicit (RENEW) or implicit (any operation using a stateid). In v4.1, SEQUENCE implicitly renews the lease for the session's client. The RENEW operation is removed in v4.1.

### 6. Open-Owner Seqid — Eliminated in v4.1

NFSv4.1 eliminates per-owner seqid validation. The SEQUENCE slot table provides all replay protection. Operations like OPEN, CLOSE, LOCK no longer carry owner seqids. However, the existing v4.0 seqid logic must be preserved for v4.0 clients.

This means the open-owner `ValidateSeqID()` path is bypassed when `compCtx.Session != nil`.

### 7. OPEN_CONFIRM — Removed in v4.1

OPEN_CONFIRM is not used in NFSv4.1. New open-owners do not need confirmation because sessions provide the sequencing guarantee. The handler should return NFS4ERR_NOTSUPP for minorversion 1.

## Data Flow: NFSv4.1 Request Processing

```
Client sends COMPOUND(minorversion=1):
  SEQUENCE(sessionID, slotID, seqID, highestSlot, cachethis)
  PUTFH(fh)
  READ(stateid, offset, count)

Server processing:
  1. ProcessCompound() -> minorversion=1 -> processCompoundV41()
  2. Decode first op (must be SEQUENCE)
  3. Look up SessionRecord by sessionID
  4. SlotTable.ProcessSequence(slotID, seqID):
     a. If seqID == slot.SequenceID: REPLAY -> return slot.CachedReply
     b. If seqID == slot.SequenceID + 1: NEW -> claim slot, proceed
     c. Otherwise: NFS4ERR_SEQ_MISORDERED
  5. Renew client lease (implicit via SEQUENCE)
  6. Set compCtx.Session, compCtx.SlotID
  7. Build SEQUENCE result (sr_sessionid, sr_sequenceid, sr_slotid,
     sr_highest_slotid, sr_target_highest_slotid, sr_status_flags)
  8. Continue dispatching PUTFH, READ as normal
  9. Encode full COMPOUND4res
  10. Cache response in slot: SlotTable.CacheReply(slotID, response)
  11. Return response to client
```

## Data Flow: NFSv4.1 Backchannel Callback

```
Server needs to recall a delegation:
  1. BackchannelManager.SendCallback(sessionID, cbOps)
  2. Look up BackchannelState for the session
  3. Select a connection bound for CDFC4_BACK
  4. Acquire a slot from the backchannel SlotTable
  5. Build CB_COMPOUND(minorversion=1):
     CB_SEQUENCE(sessionID, slotID, seqID, ...)
     CB_RECALL(stateid, truncate, fh)
  6. Frame as RPC CALL and write to the connection
  7. Read RPC REPLY from the connection (requires bidirectional I/O)
  8. Validate CB_SEQUENCE result and CB_RECALL status
  9. Release backchannel slot
```

## Trunking Architecture

NFSv4.1 defines two levels of trunking:

### Client ID Trunking
Multiple sessions per client. Already naturally supported by the multi-session model. When two connections present different sessions but the same client ID, the server knows they are the same client.

### Session Trunking
Multiple connections per session. BIND_CONN_TO_SESSION associates additional connections. The server must handle:
- Any bound connection can carry any slot's request
- The server returns responses on the same connection that sent the request
- Backchannel can be bound to any connection

**Server owner identity** determines trunking scope. The server reports `so_major_id` and `so_minor_id` in EXCHANGE_ID results. Two server addresses with the same server owner can be trunked.

For DittoFS (single-instance), all connections have the same server owner. Trunking support is straightforward: just allow BIND_CONN_TO_SESSION to work.

## Component Dependency Graph

```
EXCHANGE_ID ─────────────────────────────────┐
  (creates/confirms ClientRecord with v4.1   │
   fields, no session yet)                   │
                                             ▼
CREATE_SESSION ──────────────────────> SessionRecord
  (creates session, slot tables,        │    │
   binds calling connection)            │    │
                                        │    ▼
BIND_CONN_TO_SESSION ──────────> Connection binding
  (adds connections to session,    (multi-conn per session)
   enables trunking + backchannel)
                                        │
SEQUENCE ◄──────────────────────────────┘
  (MUST be first in every v4.1 COMPOUND,
   drives EOS, lease renewal, flow control)
                                        │
                                        ▼
[All existing ops: PUTFH, READ, WRITE, OPEN, CLOSE, LOCK, etc.]
  (unchanged logic, but skip owner-seqid validation when session present)
                                        │
                                        ▼
GET_DIR_DELEGATION ──────────> DirDelegationState
  (new op, grants dir deleg)       │
                                   ▼
CB_NOTIFY ◄────────────────── BackchannelManager
  (sends dir changes via backchannel,
   uses CB_SEQUENCE for sequencing)

DESTROY_SESSION ──────────────> removes SessionRecord
DESTROY_CLIENTID ─────────────> removes ClientRecord + all sessions
```

## Suggested Build Order

Based on the dependency graph and integration complexity:

### Phase 1: Types and Constants Foundation
- Add NFSv4.1 operation numbers (40-58) to `types/constants.go`
- Add callback operation numbers (5-14) to `types/constants.go`
- Add NFSv4.1 error codes (NFS4ERR_SEQ_MISORDERED, NFS4ERR_BADSLOT, etc.)
- Add `NFS4_MINOR_VERSION_1 = 1` constant
- Add session-related XDR structures to types package
- **No existing code modified, pure additions**

### Phase 2: Slot Table and Session Data Structures
- Implement `SlotTable` with sequence validation and replay cache (`state/slot.go`)
- Implement `SessionRecord` struct (`state/session.go`)
- Add session maps to `StateManager`
- Add `ChannelAttrs` negotiation logic
- **Testable in isolation, no handler changes yet**

### Phase 3: EXCHANGE_ID
- Implement handler for op 42
- Extend `ClientRecord` with v4.1 fields
- Add `MinorVersion` tracking on ClientRecord
- Register in dispatch table
- **First v4.1 wire-level operation**

### Phase 4: CREATE_SESSION / DESTROY_SESSION
- Implement handler for op 43 (CREATE_SESSION)
- Implement handler for op 44 (DESTROY_SESSION)
- Session creation with slot table allocation
- Connection binding (initial connection gets fore+back)
- **Sessions now exist but are not yet used for requests**

### Phase 5: SEQUENCE and COMPOUND Bifurcation
- Implement handler for op 53 (SEQUENCE)
- Split `ProcessCompound()` into v4.0 and v4.1 paths
- Enforce SEQUENCE-first rule for v4.1 COMPOUNDs
- Implement EOS: cache replies in slot, detect replays
- Skip owner-seqid validation when session is present
- Suppress OPEN_CONFIRM for v4.1
- **This is the critical integration point — after this, v4.1 sessions work end-to-end**

### Phase 6: BIND_CONN_TO_SESSION
- Implement handler for op 41
- Track connections per session in StateManager
- Support fore/back/both direction binding
- Handle connection disconnect (unbind from session)
- **Enables trunking and backchannel binding**

### Phase 7: Backchannel Multiplexing
- Implement `BackchannelManager` with CB_SEQUENCE support
- Modify `NFSConnection` for bidirectional I/O (fore + back on same TCP)
- Rewrite CB_RECALL to use backchannel instead of separate TCP
- Implement CB_SEQUENCE encoding/decoding
- **Replaces v4.0 callback path for v4.1 clients**

### Phase 8: Directory Delegations
- Implement GET_DIR_DELEGATION handler (op 46)
- Add `DirDelegationState` to StateManager
- Implement CB_NOTIFY encoding (callback op 6)
- Hook directory modifications to trigger CB_NOTIFY
- **Uses backchannel infrastructure from Phase 7**

### Phase 9: DESTROY_CLIENTID and Cleanup
- Implement DESTROY_CLIENTID handler (op 57)
- Implement FREE_STATEID handler (op 45)
- Implement RECLAIM_COMPLETE handler (op 58)
- Implement TEST_STATEID handler (op 55)
- Grace period updates for v4.1 reclaim
- **Completeness operations for production readiness**

### Phase 10: E2E Testing and Integration
- NFSv4.1 mount tests (Linux client with `vers=4.1`)
- Session establishment and teardown tests
- EOS replay tests (kill client mid-write, reconnect)
- Backchannel recall tests
- Directory delegation tests
- Trunking tests (nconnect mount option)

## Scalability Considerations

| Concern | 100 Clients | 10K Clients | 1M Clients |
|---------|-------------|-------------|-------------|
| Session memory | ~50KB each (slot table) | ~500MB total | Out of scope (single-instance) |
| Slot table slots | 64 per session (default) | 64K total slots | N/A |
| Replay cache | ~4KB per slot avg | ~256MB total | N/A |
| Connection tracking | Map lookup O(1) | Map lookup O(1) | N/A |
| Lease timers | 100 timers | 10K timers (manageable) | N/A |

The single-RWMutex pattern in StateManager will face contention at scale. At 10K+ clients, consider sharding by client ID or using per-session locks for slot table operations. For the v3.0 milestone (single-instance), the current pattern is sufficient.

## Anti-Patterns to Avoid

### Anti-Pattern 1: Shared Slot Table Lock with StateManager
**What:** Using StateManager's single RWMutex for slot table operations
**Why bad:** SEQUENCE runs on every single request. Contending with lease/delegation/open operations would serialize everything.
**Instead:** Use a separate `sync.Mutex` per SlotTable. The StateManager lock is only needed for session lookup; once you have the SessionRecord, slot operations use the slot's own lock.

### Anti-Pattern 2: Caching Only Status in Replay Cache
**What:** Caching just the NFS status code in slots
**Why bad:** EOS requires the complete COMPOUND response to be returned on replay. If only the status is cached, the client gets an inconsistent response.
**Instead:** Cache the entire `COMPOUND4res` byte slice in the slot. This is what Linux nfsd does.

### Anti-Pattern 3: Blocking on Backchannel with StateManager Lock
**What:** Holding StateManager lock while writing to backchannel connection
**Why bad:** Network I/O can block indefinitely, deadlocking the entire server.
**Instead:** Look up session/connection under RLock, release lock, then do network I/O. Same pattern as existing `sendRecall()`.

### Anti-Pattern 4: Mixed v4.0/v4.1 State on Same Client
**What:** Allowing a client to use SETCLIENTID (v4.0) and EXCHANGE_ID (v4.1) with the same identity
**Why bad:** State model incompatibility. v4.0 uses per-owner seqids, v4.1 uses session slots. Mixing them creates impossible replay semantics.
**Instead:** Each ClientRecord is either v4.0 or v4.1 (tracked by `MinorVersion` field). A "rebooted" client can switch versions via a new EXCHANGE_ID.

## Sources

- [RFC 8881: NFSv4.1 Protocol (current)](https://www.rfc-editor.org/rfc/rfc8881.html) — HIGH confidence
- [RFC 5661: NFSv4.1 Protocol (original, obsoleted by 8881)](https://datatracker.ietf.org/doc/rfc5661/) — HIGH confidence
- [Linux nfsd NFSv4.1 Server Implementation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html) — HIGH confidence
- [nfs4j: Pure Java NFSv4.2 implementation](https://github.com/dCache/nfs4j) — MEDIUM confidence (reference implementation)
- [NFSv4.1 Session Trunking and MPTCP](https://www.ietf.org/proceedings/96/slides/slides-96-mptcp-8.pdf) — MEDIUM confidence
- Existing DittoFS source code: `internal/protocol/nfs/v4/` — verified by direct reading
