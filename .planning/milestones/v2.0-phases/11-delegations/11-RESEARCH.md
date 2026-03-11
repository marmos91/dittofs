# Phase 11: Delegations - Research

**Researched:** 2026-02-14
**Domain:** NFSv4.0 open delegations (read/write), callback channel (CB_COMPOUND/CB_RECALL), conflict detection, and delegation lifecycle per RFC 7530 Sections 10.4, 16.4, 16.8
**Confidence:** HIGH

## Summary

Phase 11 implements NFSv4.0 open delegations, which allow the server to delegate file management to a client for improved caching performance. When a delegation is granted, the client can locally service OPEN, CLOSE, LOCK, READ, WRITE without server interaction. The server recalls the delegation via CB_RECALL when another client's access creates a conflict.

The implementation requires four major components: (1) delegation state tracking within the existing StateManager, (2) a server-to-client callback RPC client for sending CB_COMPOUND/CB_RECALL, (3) conflict detection that triggers recall when a second client opens a delegated file, and (4) timeout-based revocation when clients fail to respond. The codebase already has substantial infrastructure to build on: StateManager with type-tagged stateids (StateTypeDeleg = 0x03 already defined), CallbackInfo stored per-client via SETCLIENTID, the NLM callback client pattern (fresh TCP, build RPC call, read reply), and OPEN handler with OPEN_DELEGATE_NONE placeholder.

The most complex new infrastructure is the callback RPC client. Unlike the NLM callback which uses a well-known program number, NFSv4 callbacks use a client-specified program number (from SETCLIENTID) and the NFS4_CALLBACK program at 0x40000000. The callback must encode a CB_COMPOUND message containing a CB_RECALL operation. The universal address format from SETCLIENTID must be parsed to extract the IP and port for TCP connection.

**Primary recommendation:** Build in four plans matching the proposed structure: (1) delegation state model and grant logic in StateManager, (2) callback channel client for CB_COMPOUND/CB_RECALL, (3) conflict detection integrated with OPEN handler, (4) timeout and revocation. The callback client should follow the existing NLM callback pattern (fresh TCP connection per callback, short timeout).

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `internal/protocol/nfs/v4/state` | Phase 9-10 | StateManager, DelegationState, stateid generation | All NFSv4 state lives here, single RWMutex pattern |
| `internal/protocol/nfs/v4/handlers` | Phase 7-10 | OPEN handler, DELEGRETURN handler, compound dispatch | All handlers follow established patterns |
| `internal/protocol/nfs/v4/types` | Phase 6-10 | Constants, error codes, CompoundContext | All NFSv4 constants and types |
| `internal/protocol/xdr` | existing | XDR encode/decode primitives | Used by all NFS handlers |
| `internal/protocol/nfs/rpc` | existing | RPC message building, record marking | Needed for callback RPC client |
| Go stdlib `net`, `time`, `context`, `sync` | N/A | TCP connections, timers, cancellation | Standard patterns |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/protocol/nlm/callback` | Phase 5 | Reference pattern for server-to-client RPC callback | Copy/adapt `buildRPCCallMessage`, `addRecordMark`, `readAndDiscardReply` patterns |
| `internal/protocol/nfs/v4/state/grace.go` | Phase 9 | GracePeriodState for delegation reclaim | CLAIM_DELEGATE_PREV recovery |
| `pkg/metadata` | existing | FileHandle encoding for delegation tracking | Map file handles to delegation state |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Fresh TCP per callback | Persistent callback connection | Persistent is more efficient but complex (connection lifecycle, reconnect, health check); fresh TCP matches NLM pattern and is simpler for v4.0 |
| Immediate delegation grant on every OPEN | Heuristic-based granting (track conflicts, deny after rapid recall cycles) | Heuristics reduce recall storms but add complexity; start simple (grant when no conflict) then add heuristics later |
| Blocking OPEN during recall | NFS4ERR_DELAY during recall | NFS4ERR_DELAY is correct per RFC 7530 -- let the client retry |

**No new external dependencies required.** All packages already exist in the codebase.

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/
├── state/
│   ├── manager.go          # MODIFY: add delegation maps, GrantDelegation, RecallDelegation, RevokeDelegation
│   ├── delegation.go        # NEW: DelegationState struct, DelegationKey, delegation tracking types
│   ├── delegation_test.go   # NEW: delegation state unit tests
│   ├── callback.go          # NEW: CallbackClient, SendCBRecall, ParseUniversalAddr
│   ├── callback_test.go     # NEW: callback client tests
│   ├── stateid.go           # EXISTING: StateTypeDeleg already defined (0x03)
│   ├── client.go            # EXISTING: CallbackInfo already stored
│   └── openowner.go         # EXISTING: OpenState, no changes needed
│
├── handlers/
│   ├── open.go              # MODIFY: delegation grant logic in encodeOpenResult, CLAIM_DELEGATE_CUR/PREV
│   ├── delegreturn.go       # NEW: DELEGRETURN handler
│   ├── delegpurge.go        # NEW: DELEGPURGE handler (or stub)
│   ├── handler.go           # MODIFY: register new ops, add CB_GETATTR support
│   ├── stubs.go             # MODIFY: remove OPEN_DOWNGRADE if already implemented; no changes needed
│   └── delegation_test.go   # NEW: delegation integration tests
│
└── types/
    └── constants.go         # MODIFY: add OP_CB_GETATTR, OP_CB_RECALL, NFS4_CALLBACK_PROG if missing
```

### Pattern 1: Delegation State in StateManager
**What:** DelegationState tracks a granted delegation per (clientID, fileHandle) pair.
**When to use:** Every delegation grant, recall, and return.

```go
// DelegationState represents a granted delegation for a file.
type DelegationState struct {
    Stateid      types.Stateid4       // Delegation stateid (type tag = 0x03)
    ClientID     uint64               // Client that holds the delegation
    FileHandle   []byte               // The delegated file
    DelegType    uint32               // OPEN_DELEGATE_READ or OPEN_DELEGATE_WRITE
    RecallSent   bool                 // Whether CB_RECALL has been sent
    RecallTime   time.Time            // When CB_RECALL was sent
    Revoked      bool                 // Whether delegation has been revoked
}

// StateManager additions
type StateManager struct {
    // ... existing fields ...

    // delegByOther maps delegation stateid "other" -> DelegationState
    delegByOther map[[types.NFS4_OTHER_SIZE]byte]*DelegationState

    // delegByFile maps fileHandle (string key) -> list of DelegationState
    // Used for conflict detection: "does any client hold a delegation for this file?"
    delegByFile map[string][]*DelegationState
}
```

### Pattern 2: Callback Client (follows NLM callback pattern)
**What:** Server-to-client RPC client for sending CB_COMPOUND with CB_RECALL.
**When to use:** When a delegation must be recalled.

```go
// SendCBRecall sends a CB_RECALL to the client.
// Follows the NLM callback pattern: fresh TCP connection, short timeout.
func SendCBRecall(ctx context.Context, callback CallbackInfo,
    stateid *types.Stateid4, truncate bool, fh []byte) error {

    // 1. Parse universal address to get host:port
    host, port, err := ParseUniversalAddr(callback.NetID, callback.Addr)

    // 2. Create TCP connection with timeout
    addr := net.JoinHostPort(host, strconv.Itoa(port))
    conn, err := dialer.DialContext(ctx, "tcp", addr)

    // 3. Build CB_COMPOUND containing CB_RECALL
    args := encodeCBCompound(callback.Program, stateid, truncate, fh)

    // 4. Send with record marking, wait for reply
    framedMsg := addRecordMark(args, true)
    conn.Write(framedMsg)
    readAndValidateReply(conn) // Parse reply, check for NFS4_OK
}
```

### Pattern 3: Conflict Detection in OPEN
**What:** Before completing OPEN, check if a conflicting delegation exists.
**When to use:** Every OPEN that accesses a file.

```go
// In handleOpenClaimNull, after file lookup but before returning:
func (h *Handler) checkDelegationConflict(fileHandle []byte, clientID uint64, shareAccess uint32) uint32 {
    // Check if any OTHER client holds a delegation on this file
    delegs := h.StateManager.GetDelegationsForFile(fileHandle)
    for _, d := range delegs {
        if d.ClientID == clientID {
            continue // Same client, no conflict
        }
        if d.DelegType == OPEN_DELEGATE_WRITE {
            // Any access by another client conflicts with write delegation
            // Trigger recall, return NFS4ERR_DELAY
            go h.StateManager.RecallDelegation(d)
            return types.NFS4ERR_DELAY
        }
        if d.DelegType == OPEN_DELEGATE_READ && shareAccess & OPEN4_SHARE_ACCESS_WRITE != 0 {
            // Write access conflicts with read delegation
            go h.StateManager.RecallDelegation(d)
            return types.NFS4ERR_DELAY
        }
    }
    return types.NFS4_OK // No conflict
}
```

### Pattern 4: Delegation Grant Decision
**What:** Decide whether to grant a delegation when responding to OPEN.
**When to use:** In the OPEN handler, when encoding the response.

```go
// Grant decision: simple policy for initial implementation
func (sm *StateManager) ShouldGrantDelegation(
    clientID uint64, fileHandle []byte, shareAccess uint32,
) (uint32, bool) {
    // 1. Check callback path is available
    client := sm.clientsByID[clientID]
    if client == nil || client.Callback.Addr == "" {
        return OPEN_DELEGATE_NONE, false
    }

    // 2. Check no other client has opens on this file
    // (If other clients have opens, conflict is imminent)
    opensOnFile := sm.countOpensOnFile(fileHandle, clientID)
    if opensOnFile > 0 {
        return OPEN_DELEGATE_NONE, false
    }

    // 3. Check no existing delegation (already granted to another client)
    existingDelegs := sm.delegByFile[string(fileHandle)]
    if len(existingDelegs) > 0 {
        return OPEN_DELEGATE_NONE, false
    }

    // 4. Grant based on access mode
    if shareAccess & OPEN4_SHARE_ACCESS_WRITE != 0 {
        return OPEN_DELEGATE_WRITE, true
    }
    return OPEN_DELEGATE_READ, true
}
```

### Anti-Patterns to Avoid
- **Granting delegations without callback path verification:** Server must verify CB_NULL works before granting delegations. If callback path is down, never grant.
- **Blocking OPEN synchronously on recall:** Return NFS4ERR_DELAY immediately and let the client retry. Never block the OPEN handler waiting for CB_RECALL response.
- **Revoking before lease timeout:** Per RFC 7530 Section 10.4.6, the server MUST NOT revoke a delegation before the lease period has expired since the recall attempt.
- **Holding StateManager lock during callback send:** Sending CB_RECALL over TCP can block. Always release the lock before making network calls.
- **Forgetting to clean up delegations on lease expiry:** When `onLeaseExpired` fires, all delegations for that client must also be revoked.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| RPC message framing | Custom TCP framing | Reuse `buildRPCCallMessage` + `addRecordMark` from NLM callback | Proven pattern, handles XDR alignment, record marking |
| Universal address parsing | Regex-based parser | Dedicated `ParseUniversalAddr(netid, uaddr)` function with IPv4/IPv6 handling | NFSv4 uaddr format is specific: `h1.h2.h3.h4.p1.p2` for tcp, different for tcp6 |
| Timer-based delegation recall timeout | Custom goroutine + sleep | `time.AfterFunc` (matches LeaseState pattern from Phase 9) | Clean, testable, matches existing patterns |
| Stateid generation for delegations | New ID scheme | Existing `generateStateidOther(StateTypeDeleg)` | Already defined in Phase 9: type tag 0x03 |
| ACE encoding in delegation response | Full ACL implementation | Minimal nfsace4 with ALLOW + GENERIC_READ/WRITE for "EVERYONE@" | Clients don't typically use the ACE; keep it simple |

**Key insight:** The NLM callback client (`internal/protocol/nlm/callback/client.go`) is an excellent template. The NFSv4 callback client is structurally identical: build RPC CALL message with AUTH_NULL, frame it, send over TCP, read reply. The difference is the program number (from SETCLIENTID), version (1), and procedure (CB_COMPOUND = 1), plus the XDR encoding of CB_COMPOUND args.

## Common Pitfalls

### Pitfall 1: Callback Channel Connectivity
**What goes wrong:** Server grants delegation but callback path is down (NAT, firewall, client not listening). When recall is needed, CB_RECALL fails and the conflicting OPEN is blocked indefinitely.
**Why it happens:** SETCLIENTID provides callback info but doesn't guarantee connectivity.
**How to avoid:** Test callback path with CB_NULL after SETCLIENTID_CONFIRM. Track callback status per client (`cbPathUp bool`). Never grant delegations if callback path is unknown or down.
**Warning signs:** Delegation recalls timing out repeatedly for the same client.

### Pitfall 2: Deadlock on Recall During OPEN
**What goes wrong:** OPEN handler detects conflict, calls RecallDelegation which tries to acquire StateManager lock, but OPEN already holds it.
**Why it happens:** Conflict detection runs inside the OPEN handler which holds sm.mu.
**How to avoid:** Mark the delegation as "recall needed" under the lock, then trigger the actual CB_RECALL asynchronously (goroutine) without holding sm.mu. Return NFS4ERR_DELAY to the conflicting client.
**Warning signs:** Server hangs on OPEN when delegations are active.

### Pitfall 3: Race Between DELEGRETURN and CB_RECALL
**What goes wrong:** Client returns delegation via DELEGRETURN just as server sends CB_RECALL. Server marks delegation as recalled but the DELEGRETURN arrives and tries to remove it.
**Why it happens:** Network latency means CB_RECALL and DELEGRETURN can cross in transit.
**How to avoid:** DELEGRETURN should succeed regardless of recall state. If the delegation exists, remove it. If already removed (race), return NFS4_OK. Linux nfsd handles this by checking if delegation was already returned.
**Warning signs:** Spurious NFS4ERR_BAD_STATEID errors on DELEGRETURN.

### Pitfall 4: Lease Expiry Without Delegation Cleanup
**What goes wrong:** Client's lease expires, onLeaseExpired cleans up opens and locks but forgets delegations. Stale delegation state blocks future OPEN requests for those files.
**Why it happens:** Delegation cleanup not added to the existing onLeaseExpired cascade.
**How to avoid:** In onLeaseExpired, also iterate delegByOther and remove all delegations for the expired client. Remove from delegByFile too.
**Warning signs:** Files become permanently undelegatable after client timeout.

### Pitfall 5: Delegation State Leaks on CLOSE
**What goes wrong:** Client CLOSEs all opens on a file but delegation state persists. The delegation is never returned because the client considers it abandoned.
**Why it happens:** CLOSE removes open state but delegation is a separate state with its own stateid. Delegation persists until DELEGRETURN.
**How to avoid:** This is actually correct behavior per RFC 7530. A delegation can outlive the open that created it. The client uses DELEGRETURN to explicitly return it. Do NOT auto-revoke on CLOSE.
**Warning signs:** None -- this is the expected lifecycle.

### Pitfall 6: CB_GETATTR for Modified Size/Mtime
**What goes wrong:** Server needs current file attributes but client holds a write delegation and has cached modifications. Server reads stale attributes from the metadata store.
**Why it happens:** With a write delegation, the client may have modified the file locally without syncing to server.
**How to avoid:** Implement CB_GETATTR in the callback channel. Before returning GETATTR results for a file with a write delegation, send CB_GETATTR to the delegation holder to get fresh size/modify_time. For the initial implementation, only grant read delegations (simpler, no CB_GETATTR needed).
**Warning signs:** GETATTR returns stale size/mtime for write-delegated files.

### Pitfall 7: Bloom Filter for Recent Recalls (Linux nfsd Pattern)
**What goes wrong:** Server recalls a delegation, client returns it, then immediately re-opens the same file and gets a delegation again, causing a recall loop.
**Why it happens:** Without remembering recent recalls, the server eagerly re-grants.
**How to avoid:** Implement a simple "recently recalled" set (bloom filter or TTL cache) that temporarily prevents re-granting delegations on recently-recalled files. Linux nfsd uses a pair of bloom filters for this.
**Warning signs:** Rapid grant-recall-grant-recall cycles on the same file.

## Code Examples

### Example 1: Parse Universal Address
```go
// ParseUniversalAddr converts an NFSv4 universal address to host:port.
//
// For netid "tcp" (IPv4): "10.1.3.7.2.15" -> host "10.1.3.7", port 527
// For netid "tcp6" (IPv6): "fe80::1.2.15" -> host "fe80::1", port 527
//
// Source: RFC 7530 Section 3.3 (cb_client4), RFC 3530 Appendix D
func ParseUniversalAddr(netid, uaddr string) (string, int, error) {
    // Find last two dot-separated components (port bytes)
    lastDot := strings.LastIndex(uaddr, ".")
    if lastDot < 0 {
        return "", 0, fmt.Errorf("invalid uaddr: no dots: %s", uaddr)
    }
    p2Str := uaddr[lastDot+1:]
    rest := uaddr[:lastDot]

    secondLastDot := strings.LastIndex(rest, ".")
    if secondLastDot < 0 {
        return "", 0, fmt.Errorf("invalid uaddr: need at least two dot components: %s", uaddr)
    }
    p1Str := rest[secondLastDot+1:]
    host := rest[:secondLastDot]

    p1, err := strconv.Atoi(p1Str)
    if err != nil { return "", 0, fmt.Errorf("invalid port p1: %s", p1Str) }
    p2, err := strconv.Atoi(p2Str)
    if err != nil { return "", 0, fmt.Errorf("invalid port p2: %s", p2Str) }

    port := p1*256 + p2
    return host, port, nil
}
```

### Example 2: Encode CB_COMPOUND with CB_RECALL
```go
// Source: RFC 7531 (XDR), RFC 7530 Section 16.4 (CB_COMPOUND)
func encodeCBCompound(callbackIdent uint32, recallStateid *types.Stateid4,
    truncate bool, fh []byte) []byte {
    var buf bytes.Buffer

    // CB_COMPOUND4args:
    //   tag: utf8str_cs (empty string)
    xdr.WriteXDROpaque(&buf, []byte{}) // tag = ""
    //   minorversion: uint32
    xdr.WriteUint32(&buf, 0)           // minorversion = 0
    //   callback_ident: uint32
    xdr.WriteUint32(&buf, callbackIdent)
    //   argarray: nfs_cb_argop4<>
    xdr.WriteUint32(&buf, 1)           // array count = 1 operation

    // nfs_cb_argop4 (CB_RECALL):
    //   argop: uint32
    xdr.WriteUint32(&buf, OP_CB_RECALL) // = 4
    //   CB_RECALL4args:
    //     stateid: stateid4
    types.EncodeStateid4(&buf, recallStateid)
    //     truncate: bool
    if truncate {
        xdr.WriteUint32(&buf, 1)
    } else {
        xdr.WriteUint32(&buf, 0)
    }
    //     fh: nfs_fh4 (opaque<NFS4_FHSIZE>)
    xdr.WriteXDROpaque(&buf, fh)

    return buf.Bytes()
}
```

### Example 3: Encode open_delegation4 in OPEN Response
```go
// Source: RFC 7531 (open_read_delegation4, open_write_delegation4)
func encodeDelegation(buf *bytes.Buffer, deleg *DelegationState) {
    if deleg == nil {
        xdr.WriteUint32(buf, OPEN_DELEGATE_NONE) // 0
        return
    }

    xdr.WriteUint32(buf, deleg.DelegType)

    // stateid4 (delegation stateid)
    types.EncodeStateid4(buf, &deleg.Stateid)

    // recall: bool (false - not being recalled at grant time)
    xdr.WriteUint32(buf, 0) // false

    if deleg.DelegType == OPEN_DELEGATE_WRITE {
        // nfs_space_limit4: limit_by4 = NFS_LIMIT_SIZE (1), filesize = maxuint64
        xdr.WriteUint32(buf, 1)                    // NFS_LIMIT_SIZE
        xdr.WriteUint64(buf, 0xFFFFFFFFFFFFFFFF)   // unlimited
    }

    // nfsace4 permissions: ALLOW EVERYONE@ generic read/write
    xdr.WriteUint32(buf, 0)           // acetype4: ACE4_ACCESS_ALLOWED_ACE_TYPE = 0
    xdr.WriteUint32(buf, 0)           // aceflag4: no flags
    if deleg.DelegType == OPEN_DELEGATE_READ {
        xdr.WriteUint32(buf, 0x00120081) // ACE4_GENERIC_READ
    } else {
        xdr.WriteUint32(buf, 0x001601BF) // ACE4_GENERIC_READ | ACE4_GENERIC_WRITE
    }
    xdr.WriteString(buf, "EVERYONE@")  // who: utf8str_mixed
}
```

### Example 4: DELEGRETURN Handler
```go
// Source: RFC 7530 Section 16.8
func (h *Handler) handleDelegReturn(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
    // Require current filehandle
    if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
        return &types.CompoundResult{
            Status: status,
            OpCode: types.OP_DELEGRETURN,
            Data:   encodeStatusOnly(status),
        }
    }

    // Decode DELEGRETURN4args: stateid4
    stateid, err := types.DecodeStateid4(reader)
    if err != nil {
        return &types.CompoundResult{
            Status: types.NFS4ERR_BADXDR,
            OpCode: types.OP_DELEGRETURN,
            Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
        }
    }

    // Remove delegation state
    err = h.StateManager.ReturnDelegation(stateid)
    if err != nil {
        nfsStatus := mapOpenStateError(err)
        return &types.CompoundResult{
            Status: nfsStatus,
            OpCode: types.OP_DELEGRETURN,
            Data:   encodeStatusOnly(nfsStatus),
        }
    }

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_DELEGRETURN,
        Data:   encodeStatusOnly(types.NFS4_OK),
    }
}
```

## Wire Format Reference

### CB_COMPOUND4args (Server -> Client)
```
CB_COMPOUND4args:
    utf8str_cs   tag;              // opaque string (typically empty)
    uint32       minorversion;     // 0 for NFSv4.0
    uint32       callback_ident;   // from SETCLIENTID args
    nfs_cb_argop4 argarray<>;      // array of callback operations

nfs_cb_argop4 (for CB_RECALL):
    uint32       argop;            // OP_CB_RECALL = 4
    CB_RECALL4args:
        stateid4 stateid;          // delegation stateid being recalled
        bool     truncate;         // hint: file about to be truncated to zero
        nfs_fh4  fh;               // file handle

CB_COMPOUND4res:
    nfsstat4     status;           // overall status
    utf8str_cs   tag;              // echo tag from args
    nfs_cb_resop4 resarray<>;      // per-op results
```

### NFS4_CALLBACK Program
```
program NFS4_CALLBACK {
    version NFS_CB {
        void      CB_NULL(void) = 0;
        CB_COMPOUND4res CB_COMPOUND(CB_COMPOUND4args) = 1;
    } = 1;
} = 0x40000000;
```
Note: The actual program number used is the one provided by the client in SETCLIENTID (`cb_program` field), NOT 0x40000000. The 0x40000000 value is the base of the transient range. Clients typically use 0x40000000 but may choose any value in the transient range.

### open_delegation4 (in OPEN4resok)
```
union open_delegation4 switch (open_delegation_type4 delegation_type) {
    case OPEN_DELEGATE_NONE:   void;
    case OPEN_DELEGATE_READ:   open_read_delegation4 read;
    case OPEN_DELEGATE_WRITE:  open_write_delegation4 write;
};

struct open_read_delegation4 {
    stateid4 stateid;          // delegation stateid
    bool     recall;           // true if being recalled at grant time
    nfsace4  permissions;      // ACE describing who can access without ACCESS
};

struct open_write_delegation4 {
    stateid4 stateid;
    bool     recall;
    nfs_space_limit4 space_limit;  // space guarantee
    nfsace4  permissions;
};
```

### DELEGRETURN4args/res
```
struct DELEGRETURN4args {
    stateid4 deleg_stateid;    // delegation stateid to return
};
struct DELEGRETURN4res {
    nfsstat4 status;
};
```

### DELEGPURGE4args/res
```
struct DELEGPURGE4args {
    clientid4 clientid;        // client whose delegations to purge
};
struct DELEGPURGE4res {
    nfsstat4 status;
};
```

### nfsace4 Structure
```
struct nfsace4 {
    acetype4     type;           // uint32: 0=ALLOW, 1=DENY, 2=AUDIT, 3=ALARM
    aceflag4     flag;           // uint32: ACE flags
    acemask4     access_mask;    // uint32: access rights bitmask
    utf8str_mixed who;           // string: principal ("EVERYONE@", "user@domain")
};
```

### nfs_space_limit4 Union
```
enum limit_by4 {
    NFS_LIMIT_SIZE   = 1,
    NFS_LIMIT_BLOCKS = 2
};
union nfs_space_limit4 switch (limit_by4 limitby) {
    case NFS_LIMIT_SIZE:   uint64 filesize;
    case NFS_LIMIT_BLOCKS: nfs_modified_limit4 mod_blocks;
};
```

## Delegation Lifecycle (RFC 7530 Section 10.4)

### Grant Rules
1. Server decides at OPEN time whether to grant a delegation (entirely server's discretion)
2. Callback path must be established and operational
3. OPEN_DELEGATE_READ: no other client has WRITE access to the file
4. OPEN_DELEGATE_WRITE: no other client has ANY access to the file
5. Multiple clients may hold READ delegations simultaneously
6. Only ONE client may hold a WRITE delegation (exclusive)

### Conflict Detection Rules
| Existing Delegation | Second Client Action | Result |
|---------------------|---------------------|--------|
| READ delegation (Client A) | OPEN for READ (Client B) | No conflict, B may also get READ delegation |
| READ delegation (Client A) | OPEN for WRITE (Client B) | CONFLICT: recall A's delegation |
| WRITE delegation (Client A) | OPEN for READ (Client B) | CONFLICT: recall A's delegation |
| WRITE delegation (Client A) | OPEN for WRITE (Client B) | CONFLICT: recall A's delegation |

### Recall Sequence
1. Server detects conflict (second client OPEN)
2. Server sends CB_RECALL to delegation holder
3. Server returns NFS4ERR_DELAY to conflicting client
4. Delegation holder receives CB_RECALL, responds with NFS4_OK (acknowledges recall)
5. Delegation holder flushes dirty data (for write delegations)
6. Delegation holder sends DELEGRETURN to server
7. Server removes delegation state
8. Conflicting client retries OPEN -- succeeds

### Revocation Rules (RFC 7530 Section 10.4.6)
1. If client does not respond to CB_RECALL within lease period, server may revoke
2. Server MUST NOT revoke before lease period has elapsed since recall attempt
3. If client IS responding (flushing data), server should be lenient
4. Revoked delegations: operations using that stateid return NFS4ERR_BAD_STATEID (v4.0)
5. On lease expiry, all delegations for the client are revoked

### CLAIM_DELEGATE_CUR and CLAIM_DELEGATE_PREV
- **CLAIM_DELEGATE_CUR:** Client opens a file using an existing delegation (during recall). The client provides the delegation stateid. This allows the client to perform a new OPEN on a file it already has delegated, avoiding the need for server round-trips.
- **CLAIM_DELEGATE_PREV:** After server restart, client reclaims a delegation held before. Only available during grace period. Server may optionally support this (paired with DELEGPURGE support).

## Existing Code to Reuse

| What | Where | How to Use |
|------|-------|-----------|
| `StateTypeDeleg = 0x03` | `state/stateid.go:27` | Already defined for delegation stateids |
| `CallbackInfo` struct | `state/client.go:98` | Already stores program, netid, addr from SETCLIENTID |
| `generateStateidOther(StateTypeDeleg)` | `state/stateid.go:67` | Generate delegation stateids |
| `isCurrentEpoch()` | `state/stateid.go:94` | Validate delegation stateids |
| NLM callback `buildRPCCallMessage` | `nlm/callback/client.go:125` | Copy/adapt for CB_COMPOUND building |
| NLM callback `addRecordMark` | `nlm/callback/client.go:191` | Reuse directly for TCP framing |
| NLM callback `readAndDiscardReply` | `nlm/callback/client.go:209` | Adapt to parse CB_COMPOUND4res |
| `EncodeStateid4` | `types/types.go` | Encode delegation stateid in responses |
| `DecodeStateid4` | `types/types.go` | Decode stateid in DELEGRETURN args |
| `encodeStatusOnly` | `handlers/handler.go:115` | Error-only responses |
| `mapOpenStateError` | `handlers/open.go:637` | Map state errors to NFS4 status |
| `onLeaseExpired` | `state/manager.go:470` | Add delegation cleanup to cascade |
| `OPEN_DELEGATE_NONE/READ/WRITE` | `types/constants.go:265-267` | Already defined |
| `CLAIM_DELEGATE_CUR/PREV` | `types/constants.go:253-254` | Already defined |
| `OP_DELEGPURGE/OP_DELEGRETURN` | `types/constants.go:81-82` | Already defined |
| `NFS4ERR_DELAY` | `types/constants.go:156` | Return to conflicting client during recall |
| `NFS4ERR_CB_PATH_DOWN` | `types/constants.go:196` | Callback path unavailable |

## New Constants Needed

```go
// Callback operation numbers (for CB_COMPOUND)
const (
    OP_CB_GETATTR  = 3
    OP_CB_RECALL   = 4
    OP_CB_ILLEGAL  = 10044
)

// Callback program and version
const (
    NFS4_CALLBACK_VERSION = 1   // NFS_CB version 1
    CB_PROC_NULL     = 0        // CB_NULL procedure
    CB_PROC_COMPOUND = 1        // CB_COMPOUND procedure
)

// ACE constants for delegation permissions
const (
    ACE4_ACCESS_ALLOWED_ACE_TYPE = 0
    ACE4_GENERIC_READ            = 0x00120081
    ACE4_GENERIC_WRITE           = 0x00160106
    ACE4_GENERIC_EXECUTE         = 0x001200A0
)

// Space limit
const (
    NFS_LIMIT_SIZE   = 1
    NFS_LIMIT_BLOCKS = 2
)
```

## Recommended Plan Structure

### Plan 11-01: Delegation State Tracking and Grant Logic
**Scope:** DelegationState type, StateManager delegation maps, GrantDelegation, ReturnDelegation, delegation cleanup in lease expiry, DELEGRETURN handler
**Files:** `state/delegation.go`, `state/manager.go` (modify), `handlers/delegreturn.go`, `handlers/handler.go` (dispatch), `types/constants.go` (new CB constants)
**Complexity:** Medium -- follows established OpenState/LockState patterns
**Tests:** ~20 tests covering: grant delegation, return delegation, double return (idempotent), bad stateid, stale stateid, lease expiry cleanup, DELEGRETURN handler, DELEGRETURN bad stateid

### Plan 11-02: Callback Channel (CB_COMPOUND, CB_RECALL)
**Scope:** CallbackClient, ParseUniversalAddr, CB_COMPOUND encoding, CB_RECALL encoding, CB_COMPOUND response parsing, CB_NULL for path testing
**Files:** `state/callback.go`, `state/callback_test.go`
**Complexity:** High -- new RPC client (building on NLM pattern), universal address parsing, XDR encoding
**Tests:** ~15 tests covering: ParseUniversalAddr IPv4/IPv6, CB_COMPOUND encoding, CB_RECALL encoding, CB_COMPOUND response parsing, timeout handling, connection failure

### Plan 11-03: Conflict Detection and Recall Triggering
**Scope:** Conflict detection in OPEN handler, async recall trigger, NFS4ERR_DELAY response, OPEN with CLAIM_DELEGATE_CUR/CLAIM_DELEGATE_PREV, delegation grant decision in OPEN, DELEGPURGE handler
**Files:** `handlers/open.go` (modify), `handlers/delegpurge.go`, `state/manager.go` (RecallDelegation, ShouldGrantDelegation, GetDelegationsForFile)
**Complexity:** High -- touches OPEN handler (most complex handler), async recall, claim types
**Tests:** ~20 tests covering: grant on solo open, no grant with other opens, conflict detection, NFS4ERR_DELAY on conflict, CLAIM_DELEGATE_CUR open, CLAIM_DELEGATE_PREV reclaim, DELEGPURGE, read-read no conflict, read-write conflict, write-any conflict

### Plan 11-04: Delegation Timeout and Revocation
**Scope:** Recall timeout timer, delegation revocation, callback path health tracking, bloom filter/TTL cache for recently recalled files, CB_GETATTR stub (returns NFS4ERR_NOTSUPP for write delegations initially)
**Files:** `state/delegation.go` (modify: add recall timer, revocation), `state/callback.go` (add CB_NULL path check, CB_GETATTR), `state/manager.go` (revocation cascade)
**Complexity:** Medium -- timer-based (follows LeaseState pattern), bloom filter for anti-recall-storm
**Tests:** ~15 tests covering: recall timeout fires revocation, revocation cleans state, revoked delegation returns BAD_STATEID, callback path down prevents grants, recently-recalled file not re-granted, CB_NULL success/failure

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| NFSv4.0 separate TCP callback | NFSv4.1 backchannel on same connection | RFC 5661 (2010) | v4.1 eliminated firewall/NAT issues; v4.0 still needs separate TCP |
| No delegation (just opens/locks) | Full delegation support | RFC 3530 (2003) | Major performance improvement for single-client workloads |
| Simple grant-on-first-open | Bloom filter anti-storm + heuristics | Linux 2.6.29+ (2009) | Prevents grant-recall-grant loops |

**Note:** We are implementing NFSv4.0 delegations per RFC 7530. NFSv4.1 (RFC 8881) introduced significant improvements (backchannel sessions, CB_SEQUENCE, referring identifiers) that we do NOT need to implement.

## Open Questions

1. **CB_GETATTR for write delegations**
   - What we know: Write delegations require CB_GETATTR to get fresh size/mtime from the client
   - What's unclear: Whether to implement CB_GETATTR in Phase 11 or defer
   - Recommendation: Start with read delegations only in Plan 11-01. Add write delegation support with CB_GETATTR in Plan 11-03 or defer to a follow-up phase. Read delegations provide most of the caching benefit with less complexity.

2. **DELEGPURGE support**
   - What we know: DELEGPURGE is optional and paired with CLAIM_DELEGATE_PREV support
   - What's unclear: Whether our grace period implementation needs modification
   - Recommendation: Implement DELEGPURGE as NFS4ERR_NOTSUPP initially (matching our CLAIM_DELEGATE_PREV support level). If CLAIM_DELEGATE_PREV is fully supported, implement DELEGPURGE as well.

3. **Callback path verification timing**
   - What we know: CB_NULL should be sent to verify callback path before granting
   - What's unclear: Whether to verify on every OPEN or cache the result per client
   - Recommendation: Verify once on SETCLIENTID_CONFIRM, cache result on ClientRecord. Re-verify if a CB_RECALL fails. This avoids per-OPEN overhead.

4. **Write delegation completeness**
   - What we know: Write delegations are more complex (CB_GETATTR, dirty data flush)
   - What's unclear: Whether full write delegation support is needed for the success criteria
   - Recommendation: The success criteria say "Write delegation granted when client has exclusive write access" -- implement both read and write, but CB_GETATTR can return the server's (potentially stale) attributes initially. Full CB_GETATTR correctness can be a follow-up.

## Sources

### Primary (HIGH confidence)
- [RFC 7530 - NFSv4 Protocol](https://www.rfc-editor.org/rfc/rfc7530.html) - Sections 10.4, 10.5, 16.4, 16.7, 16.8 (delegation semantics, recall, return)
- [RFC 7531 - NFSv4 XDR Description](https://www.rfc-editor.org/rfc/rfc7531.html) - Authoritative XDR definitions for all delegation types
- DittoFS codebase: `internal/protocol/nfs/v4/state/` (StateManager, existing infrastructure)
- DittoFS codebase: `internal/protocol/nlm/callback/` (callback client pattern)

### Secondary (MEDIUM confidence)
- [Linux NFSD nfs4state.c](https://github.com/torvalds/linux/blob/master/fs/nfsd/nfs4state.c) - Delegation bloom filter, revocation, recall implementation
- [Linux NFSD nfs4callback.c](https://github.com/torvalds/linux/blob/master/fs/nfsd/nfs4callback.c) - CB_RECALL encoding, callback channel management
- [NFS-Ganesha NFSv4 Delegations Wiki](https://github.com/nfs-ganesha/nfs-ganesha/wiki/NFSv4-Delegations) - Practical implementation patterns

### Tertiary (LOW confidence)
- [Linux NFS callback issues](http://www.fieldses.org/~bfields/callbacks.html) - Known callback path problems in Linux
- [Oracle NFS Delegation Docs](https://docs.oracle.com/cd/E19253-01/816-4555/rfsrefer-140/index.html) - Delegation overview

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - all libraries exist in codebase, patterns established
- Architecture: HIGH - follows established StateManager pattern, NLM callback precedent
- Wire format: HIGH - verified against RFC 7531 XDR definitions
- Conflict detection rules: HIGH - clearly defined in RFC 7530 Section 10.4
- Callback implementation: MEDIUM - no existing Go NFSv4 callback server reference, but NLM callback pattern is well-proven
- Pitfalls: HIGH - informed by Linux nfsd source code and NFS-Ganesha wiki

**Research date:** 2026-02-14
**Valid until:** 2026-03-14 (stable -- NFSv4.0 spec is frozen, RFC 7530 unchanged since 2015)
