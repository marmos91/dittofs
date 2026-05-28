# Phase 9: State Management - Research

**Researched:** 2026-02-13
**Domain:** NFSv4 stateful model -- client ID, state ID, open/lock owners, leases, grace period (RFC 7530 Sections 9, 16.28, 16.33, 16.34)
**Confidence:** HIGH

## Summary

Phase 9 replaces the Phase 6/7 stub implementations of SETCLIENTID, SETCLIENTID_CONFIRM, and RENEW with proper stateful client and open-state management. The current codebase has:
- `handleSetClientID()` -- a stub that uses an atomic counter for client IDs and returns timestamp-based confirm verifiers without any state tracking
- `handleSetClientIDConfirm()` -- a stub that always returns NFS4_OK without validating the confirm verifier
- `handleRenew()` -- a stub that always returns NFS4_OK without tracking leases
- `handleOpen()` -- generates random placeholder stateids with no tracking
- `handleClose()` -- returns zeroed stateids without cleaning up state
- `handleOpenConfirm()` -- blindly increments seqid without verifying open-owner state
- `V4ClientState` -- a placeholder struct with only `ClientAddr`

Phase 9 must build a complete state management layer that tracks: (1) client identities with their confirm/unconfirm lifecycle, (2) open-owners with per-owner seqid validation and last-request replay caching, (3) stateids that map to open state (share_access, share_deny, filehandle), (4) lease timers with configurable duration and automatic expiration, and (5) grace period handling for state reclaim after server restart. This layer is also the foundation for Phase 10 (LOCK/LOCKT/LOCKU) and Phase 11 (delegations), both of which add their own state types atop this infrastructure.

The most complex aspect is the SETCLIENTID processing algorithm (RFC 7530 Section 9.1.1), which has five distinct cases depending on whether the client is new, restarting, updating callbacks, or in collision with unconfirmed state. Getting this wrong causes client hangs, state corruption, or inability to recover from network partitions.

**Primary recommendation:** Build a new `internal/protocol/nfs/v4/state/` package containing the state manager, separate from the handlers. The state manager owns all client records, open-owner tables, stateid maps, and lease timers. Handlers call into the state manager for all state operations. This keeps protocol concerns (XDR decode/encode) in handlers and state logic isolated for testing. Organize into five plans: (1) Client ID management with the full SETCLIENTID algorithm, (2) stateid generation/validation with the "other" field encoding scheme, (3) open-owner and lock-owner tracking with seqid validation and replay cache, (4) lease management with RENEW and automatic expiration, (5) grace period and state recovery.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `internal/protocol/nfs/v4/state/` | NEW | Central state manager for all NFSv4 state | Isolates state logic from protocol handlers; testable independently |
| `internal/protocol/nfs/v4/types` | Phase 6 | Stateid4, CompoundContext, V4ClientState, error codes | Already has Stateid4 type and all needed NFS4ERR_* constants |
| `internal/protocol/nfs/v4/handlers` | Phase 6-8 | SETCLIENTID, RENEW, OPEN, CLOSE handlers | Existing stubs to upgrade with real state calls |
| `pkg/metadata/lock` | Phase 1 | GracePeriodManager, LockOwner patterns | Reuse grace period state machine for NFSv4 grace period |
| Go stdlib `sync`, `time`, `crypto/rand` | N/A | Concurrency, timers, random generation | State manager must be thread-safe; lease timers use time.AfterFunc |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `pkg/config` | existing | LockConfig.GracePeriodDuration | Shared grace period config already exists |
| `pkg/controlplane/runtime` | existing | Runtime reference for state persistence hooks | STATE-08 requires metadata store integration |
| `github.com/google/uuid` | existing dep | UUID generation for client record IDs | Already used throughout codebase |
| Go stdlib `encoding/binary` | N/A | Encode client ID boot epoch into verifier | Server boot verifier encoding |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| New `state/` package | Embed state directly in Handler struct | Separate package is testable without protocol overhead; state logic is complex enough to warrant isolation |
| In-memory state only | Persistent state in metadata store | In-memory is simpler for Phase 9; STATE-08 adds persistence hooks but core state can start in-memory |
| Reuse existing GracePeriodManager | Build separate NFSv4 grace period | Existing GracePeriodManager has the right state machine; extend or wrap it for NFSv4 CLAIM_PREVIOUS semantics |
| Global atomic counter for client IDs | Random or epoch-based client IDs | Counter is simpler but not restart-safe; combine boot epoch (high 32 bits) + counter (low 32 bits) for uniqueness across restarts |

**No new external dependencies required.** All packages already exist in the codebase.

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/
├── state/                        # NEW: State management package
│   ├── manager.go                # StateManager: central coordinator
│   ├── client.go                 # ClientRecord, SETCLIENTID algorithm
│   ├── openowner.go              # OpenOwner tracking, seqid validation
│   ├── lockowner.go              # LockOwner tracking (foundation for Phase 10)
│   ├── stateid.go                # Stateid generation, validation, lookup
│   ├── lease.go                  # Lease timer, renewal, expiration
│   ├── grace.go                  # NFSv4 grace period integration
│   ├── manager_test.go           # StateManager unit tests
│   ├── client_test.go            # Client record tests
│   ├── openowner_test.go         # Open-owner seqid tests
│   ├── stateid_test.go           # Stateid tests
│   └── lease_test.go             # Lease timer tests
│
├── handlers/
│   ├── handler.go                # MODIFY: add StateManager field
│   ├── setclientid.go            # MODIFY: replace stub with real state management
│   ├── renew.go                  # MODIFY: replace stub with real lease renewal
│   ├── open.go                   # MODIFY: use StateManager for stateid generation
│   ├── close.go                  # MODIFY: use StateManager for state cleanup
│   ├── read.go                   # MODIFY: validate stateids via StateManager
│   ├── write.go                  # MODIFY: validate stateids via StateManager
│   ├── stubs.go                  # MODIFY: OPEN_DOWNGRADE uses StateManager
│   └── compound.go               # MODIFY: pass client state to CompoundContext
│
├── types/
│   ├── types.go                  # MODIFY: extend V4ClientState with client record ref
│   └── constants.go              # EXISTING: all needed error codes already defined
```

### Pattern 1: State Manager as Central Authority
**What:** A single `StateManager` struct owns all NFSv4 state. Handlers never directly modify state tables -- they call StateManager methods.
**When to use:** Every state-related operation (SETCLIENTID, OPEN, CLOSE, RENEW, READ/WRITE stateid checks).
**Example:**
```go
// state/manager.go
type StateManager struct {
    mu sync.RWMutex

    // Client records indexed by clientid4
    clientsByID map[uint64]*ClientRecord

    // Client records indexed by nfs_client_id4.id (opaque string)
    clientsByName map[string]*ClientRecord

    // Unconfirmed client records (pending SETCLIENTID_CONFIRM)
    unconfirmedByName map[string]*ClientRecord

    // Open state indexed by stateid "other" field
    openStateByOther map[[NFS4_OTHER_SIZE]byte]*OpenState

    // Open-owners indexed by (clientid, owner) tuple
    openOwners map[openOwnerKey]*OpenOwner

    // Lease configuration
    leaseDuration time.Duration

    // Grace period manager
    gracePeriod *GracePeriodState

    // Boot epoch for client ID generation
    bootEpoch uint32

    // Counter for client ID generation
    nextClientSeq uint32

    // Counter for stateid "other" field generation
    nextStateSeq uint64
}
```

### Pattern 2: Client ID Generation (Boot Epoch + Counter)
**What:** Combine server boot epoch (high 32 bits) with monotonic counter (low 32 bits) to produce 64-bit client IDs that are unique across server restarts.
**When to use:** SETCLIENTID when creating a new client record.
**Example:**
```go
func (sm *StateManager) generateClientID() uint64 {
    seq := atomic.AddUint32(&sm.nextClientSeq, 1)
    return (uint64(sm.bootEpoch) << 32) | uint64(seq)
}
```
This ensures that client IDs from before a restart never collide with new ones, which is critical for clients detecting server reboots via NFS4ERR_STALE_CLIENTID.

### Pattern 3: SETCLIENTID Five-Case Algorithm
**What:** RFC 7530 Section 9.1.1 requires five distinct cases for SETCLIENTID processing based on whether the client's `nfs_client_id4.id` and `verifier` match existing records.
**When to use:** handleSetClientID handler.
**Example:**
```go
func (sm *StateManager) SetClientID(clientIDStr string, verifier [8]byte, callback CallbackInfo) (*SetClientIDResult, error) {
    sm.mu.Lock()
    defer sm.mu.Unlock()

    confirmed := sm.clientsByName[clientIDStr]
    unconfirmed := sm.unconfirmedByName[clientIDStr]

    switch {
    case confirmed == nil && unconfirmed == nil:
        // Case 1: Completely new client
        return sm.createNewClient(clientIDStr, verifier, callback)

    case confirmed != nil && confirmed.VerifierMatches(verifier):
        // Case 5: Same client, same verifier (re-SETCLIENTID, maybe callback update)
        return sm.reuseConfirmedClient(confirmed, callback)

    case confirmed != nil && !confirmed.VerifierMatches(verifier):
        // Case 3: Same client ID string, different verifier (client reboot)
        return sm.handleClientReboot(confirmed, clientIDStr, verifier, callback)

    case confirmed == nil && unconfirmed != nil:
        // Case 4: Replace unconfirmed record
        return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback)

    default:
        // Case 2: Confirmed exists + unconfirmed exists (replace unconfirmed)
        return sm.replaceUnconfirmed(unconfirmed, clientIDStr, verifier, callback)
    }
}
```

### Pattern 4: Stateid Encoding Scheme
**What:** The 12-byte `other` field in stateid4 encodes enough information to look up the associated state. A common approach: 4-byte type tag + 8-byte unique counter.
**When to use:** Generating stateids for OPEN, LOCK operations.
**Example:**
```go
const (
    StateTypeOpen = 0x01
    StateTypeLock = 0x02
    StateTypeDeleg = 0x03
)

func (sm *StateManager) generateStateidOther(stateType byte) [NFS4_OTHER_SIZE]byte {
    seq := atomic.AddUint64(&sm.nextStateSeq, 1)
    var other [NFS4_OTHER_SIZE]byte
    other[0] = stateType
    // Bytes 1-3: reserved/boot epoch fragment for cross-restart detection
    other[1] = byte(sm.bootEpoch >> 16)
    other[2] = byte(sm.bootEpoch >> 8)
    other[3] = byte(sm.bootEpoch)
    // Bytes 4-11: unique sequence
    binary.BigEndian.PutUint64(other[4:], seq)
    return other
}
```

### Pattern 5: Open-Owner Seqid Validation with Replay Cache
**What:** Each open-owner maintains a seqid counter. The server validates that each new request has seqid = previous + 1. If seqid matches the previous value exactly, return the cached last response (replay detection).
**When to use:** OPEN, CLOSE, OPEN_CONFIRM, OPEN_DOWNGRADE operations.
**Example:**
```go
type OpenOwner struct {
    ClientID    uint64
    OwnerData   []byte
    LastSeqID   uint32
    LastResult  *CachedResult  // For replay detection
    Confirmed   bool
    OpenStates  []*OpenState   // All open stateids for this owner
}

func (oo *OpenOwner) ValidateSeqID(seqid uint32) SeqIDValidation {
    expected := oo.LastSeqID + 1
    if seqid == expected {
        return SeqIDOK
    }
    if seqid == oo.LastSeqID {
        return SeqIDReplay  // Return cached result
    }
    return SeqIDBad  // NFS4ERR_BAD_SEQID
}
```

### Pattern 6: Lease Timer with Implicit Renewal
**What:** Each confirmed client has a lease timer. Any operation that touches state implicitly renews the lease. RENEW explicitly renews. When the timer fires, all client state is cleaned up.
**When to use:** Lease management.
**Example:**
```go
type LeaseState struct {
    ClientID   uint64
    Duration   time.Duration
    LastRenew  time.Time
    Timer      *time.Timer
    OnExpire   func(clientID uint64)
}

func (ls *LeaseState) Renew() {
    ls.LastRenew = time.Now()
    ls.Timer.Reset(ls.Duration)
}

func (ls *LeaseState) IsExpired() bool {
    return time.Since(ls.LastRenew) > ls.Duration
}
```

### Anti-Patterns to Avoid
- **State in handlers:** Handlers should not have `map[uint64]*ClientRecord` fields. All state belongs in the StateManager. Handlers call StateManager methods.
- **Global variables for state:** The current `nextClientID` atomic counter in `setclientid.go` is a Phase 6 stopgap. Replace with StateManager-owned state.
- **Skipping the confirm step:** SETCLIENTID creates unconfirmed state, SETCLIENTID_CONFIRM promotes to confirmed. Skipping this allows stale clients to corrupt fresh state.
- **Ignoring seqid validation:** Returning NFS4_OK for any seqid breaks replay detection and allows duplicate operations. This causes data corruption under retransmit scenarios.
- **Separate locks for client table vs stateid table:** Use a single RWMutex for the entire StateManager to avoid deadlocks between interdependent lookups (client -> open-owner -> stateid -> lease).
- **Modifying state outside StateManager:** If the grace period or lease timer calls back into state, it must go through StateManager methods, not directly modify maps.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Grace period state machine | Custom NFSv4 grace period FSM | Wrap/adapt `pkg/metadata/lock.GracePeriodManager` | Already handles Normal/Active states, timer-based exit, expected client tracking, early exit on all reclaims |
| UUID generation | Custom random IDs | `github.com/google/uuid` | Already a project dependency, cryptographically sound |
| Timer management | Manual goroutine + sleep | `time.AfterFunc` / `time.Timer` | Standard library, properly handles cancellation and reset |
| Concurrent map access | Unsynchronized maps | `sync.RWMutex` on StateManager | Must handle concurrent COMPOUND operations from multiple connections |
| Stateid XDR encode/decode | New encode/decode functions | Existing `types.DecodeStateid4()` / `types.EncodeStateid4()` | Already implemented in Phase 6 |

**Key insight:** The state manager is infrastructure -- it is about data structures, lookups, timers, and concurrency. It does NOT handle XDR encoding, RPC framing, or protocol dispatch. Those remain in handlers.

## Common Pitfalls

### Pitfall 1: SETCLIENTID Algorithm Incorrectness
**What goes wrong:** Treating SETCLIENTID as simple "create client" instead of implementing the five-case algorithm causes: (a) client reboots not detected (stale state persists), (b) callback updates not applied, (c) network partition recovery fails.
**Why it happens:** The RFC section 9.1.1 algorithm is complex with five distinct cases based on confirmed/unconfirmed state and verifier matching.
**How to avoid:** Implement all five cases as a switch statement with explicit case comments. Write a unit test for each case. Test sequence: new client, same client re-SETCLIENTID, client reboot (new verifier), replace unconfirmed, confirmed+unconfirmed collision.
**Warning signs:** Client hangs after reboot, "stale clientid" errors that never resolve, callbacks pointing to wrong client address.

### Pitfall 2: Seqid Wrap-Around at MAX_UINT32
**What goes wrong:** When seqid reaches 0xFFFFFFFF, next value should be 1 (not 0). Seqid 0 is reserved for special stateids. Wrapping to 0 breaks stateid validation.
**Why it happens:** Simple `seqid + 1` overflows uint32 to 0.
**How to avoid:** Use `nextSeqid := (seqid % 0xFFFFFFFF) + 1` or explicit check: `if seqid == 0xFFFFFFFF { return 1 }`.
**Warning signs:** Sporadic NFS4ERR_BAD_STATEID errors after long server uptime.

### Pitfall 3: Lease Renewal on State-Modifying Operations Only
**What goes wrong:** RFC 7530 says READ operations implicitly renew leases. If only RENEW and state-modifying operations (OPEN, CLOSE, LOCK) renew, a client doing only READs will have its lease expire.
**Why it happens:** Easy to forget that READ and other non-state operations also serve as implicit lease renewal.
**How to avoid:** The COMPOUND dispatcher should call `StateManager.TouchLease(clientID)` for every operation that carries a valid (non-special) stateid. Since stateids encode the client ID (via open-state lookup), this is straightforward.
**Warning signs:** Active clients suddenly getting NFS4ERR_EXPIRED while doing heavy reads.

### Pitfall 4: Special Stateids Must Still Work
**What goes wrong:** After adding stateid validation, READ/WRITE with special stateids (all-zeros anonymous, all-ones read-bypass) start returning NFS4ERR_BAD_STATEID because the validator doesn't recognize them.
**Why it happens:** Special stateids are not in the state table. Validation code checks the table and fails.
**How to avoid:** Always check `IsSpecialStateid()` BEFORE looking up in the state table. Special stateids bypass all state validation (no lease renewal, no open check).
**Warning signs:** Mounting fails because initial READ operations use anonymous stateids.

### Pitfall 5: Race Between Lease Expiry and In-Flight Operations
**What goes wrong:** Lease timer fires and cleans up state while a COMPOUND operation is mid-execution using that state. This causes the operation to fail with NFS4ERR_EXPIRED mid-compound or crash due to nil pointer.
**Why it happens:** Lease timer runs on a separate goroutine from request handlers.
**How to avoid:** Two approaches: (a) Lease timer marks state as "expiring" but defers cleanup until no operations are in-flight (complex), or (b) check lease validity at start of each operation and extend briefly during execution (simpler, recommended). The second approach means expired state might linger for one more operation but is much safer.
**Warning signs:** Intermittent panics or NFS4ERR_EXPIRED in the middle of compound operations.

### Pitfall 6: Confirm Verifier Must Be Unpredictable
**What goes wrong:** Using a simple counter or timestamp for the confirm verifier allows a malicious or stale client to guess the verifier and confirm someone else's SETCLIENTID.
**Why it happens:** Phase 6 stub uses timestamp nanoseconds, which are predictable.
**How to avoid:** Use `crypto/rand.Read()` for the 8-byte confirm verifier. This is what the Linux kernel nfsd does.
**Warning signs:** State corruption when multiple clients are active, especially during failover.

### Pitfall 7: Open State Must Track share_access and share_deny
**What goes wrong:** Without tracking share_access/share_deny per open, OPEN_DOWNGRADE has no baseline to downgrade from, and share mode conflicts are not detected.
**Why it happens:** Phase 7 ignores share_access/share_deny completely.
**How to avoid:** Each OpenState record must store the accumulated share_access and share_deny bits. Multiple OPENs from the same open-owner on the same file produce a single stateid with OR'd access/deny bits.
**Warning signs:** OPEN_DOWNGRADE always fails, share_deny has no effect.

## Code Examples

### Example 1: ClientRecord Structure
```go
// Source: RFC 7530 Section 9.1.1 + Linux nfsd state.h patterns
type ClientRecord struct {
    // Server-assigned 64-bit client ID
    ClientID uint64

    // Client-provided opaque identifier (nfs_client_id4.id)
    ClientIDString string

    // Client-provided verifier (detects client restarts)
    Verifier [8]byte

    // Server-generated confirm verifier
    ConfirmVerifier [8]byte

    // Whether SETCLIENTID_CONFIRM has been called
    Confirmed bool

    // Callback information for delegations (Phase 11)
    Callback CallbackInfo

    // Lease state
    Lease *LeaseState

    // All open-owners for this client
    OpenOwners map[string]*OpenOwner // keyed by owner opaque string

    // Creation time (for debugging)
    CreatedAt time.Time

    // Client network address (for logging)
    ClientAddr string
}
```

### Example 2: OpenState Structure
```go
// Source: RFC 7530 Section 9.1.4 + Linux nfsd nfs4_ol_stateid
type OpenState struct {
    // The stateid for this open
    Stateid types.Stateid4

    // The open-owner that created this state
    Owner *OpenOwner

    // The file handle this open is for
    FileHandle []byte

    // Share access bits (OPEN4_SHARE_ACCESS_READ/WRITE/BOTH)
    ShareAccess uint32

    // Share deny bits (OPEN4_SHARE_DENY_NONE/READ/WRITE/BOTH)
    ShareDeny uint32

    // Whether OPEN_CONFIRM has been called for this state
    Confirmed bool

    // Associated lock states (populated by Phase 10)
    LockStates []*LockState
}
```

### Example 3: Stateid Validation Flow
```go
// Source: RFC 7530 Section 9.1.4
func (sm *StateManager) ValidateStateid(stateid *types.Stateid4, operation string) (*OpenState, error) {
    // 1. Check for special stateids
    if stateid.IsSpecialStateid() {
        return nil, nil // Special stateids bypass validation
    }

    // 2. Look up by "other" field
    sm.mu.RLock()
    openState, exists := sm.openStateByOther[stateid.Other]
    sm.mu.RUnlock()

    if !exists {
        return nil, NFS4Error(types.NFS4ERR_BAD_STATEID)
    }

    // 3. Check boot epoch in "other" field (detects server restart)
    if !sm.isCurrentEpoch(stateid.Other) {
        return nil, NFS4Error(types.NFS4ERR_STALE_STATEID)
    }

    // 4. Validate seqid (must be current or current-1 for some ops)
    if stateid.Seqid < openState.Stateid.Seqid {
        return nil, NFS4Error(types.NFS4ERR_OLD_STATEID)
    }
    if stateid.Seqid > openState.Stateid.Seqid {
        return nil, NFS4Error(types.NFS4ERR_BAD_STATEID)
    }

    // 5. Check client lease is still valid
    if openState.Owner != nil && openState.Owner.ClientRecord != nil {
        if openState.Owner.ClientRecord.Lease.IsExpired() {
            return nil, NFS4Error(types.NFS4ERR_EXPIRED)
        }
        // Implicit lease renewal
        openState.Owner.ClientRecord.Lease.Renew()
    }

    return openState, nil
}
```

### Example 4: Handler Integration Pattern
```go
// Source: Existing handler patterns adapted for state manager
func (h *Handler) handleSetClientID(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
    // ... decode XDR args (existing code) ...

    // Call state manager instead of atomic counter
    result, err := h.StateManager.SetClientID(clientIDStr, verifier, CallbackInfo{
        Program: callbackProgram,
        NetID:   callbackNetID,
        Addr:    callbackAddr,
    })
    if err != nil {
        // Map state error to NFS4 error code
        return &types.CompoundResult{
            Status: mapStateError(err),
            OpCode: types.OP_SETCLIENTID,
            Data:   encodeStatusOnly(mapStateError(err)),
        }
    }

    // Encode response
    var buf bytes.Buffer
    _ = xdr.WriteUint32(&buf, types.NFS4_OK)
    _ = xdr.WriteUint64(&buf, result.ClientID)
    buf.Write(result.ConfirmVerifier[:])

    return &types.CompoundResult{
        Status: types.NFS4_OK,
        OpCode: types.OP_SETCLIENTID,
        Data:   buf.Bytes(),
    }
}
```

### Example 5: Lease Expiration Callback
```go
// Source: RFC 7530 Section 9.5 + existing GracePeriodManager pattern
func (sm *StateManager) onLeaseExpired(clientID uint64) {
    sm.mu.Lock()
    defer sm.mu.Unlock()

    client, exists := sm.clientsByID[clientID]
    if !exists {
        return
    }

    logger.Info("NFSv4 client lease expired, cleaning up state",
        "client_id", clientID,
        "client_addr", client.ClientAddr,
        "open_owners", len(client.OpenOwners))

    // Clean up all open state for this client
    for _, owner := range client.OpenOwners {
        for _, openState := range owner.OpenStates {
            delete(sm.openStateByOther, openState.Stateid.Other)
        }
        delete(sm.openOwners, makeOwnerKey(clientID, owner.OwnerData))
    }

    // Remove client record
    delete(sm.clientsByID, clientID)
    delete(sm.clientsByName, client.ClientIDString)
}
```

## State of the Art

| Old Approach (Phase 6-8) | Current Approach (Phase 9) | When Changed | Impact |
|--------------------------|---------------------------|--------------|--------|
| Atomic counter for client IDs | Boot epoch + counter, tracked in ClientRecord | Phase 9 | Clients can detect server restarts via NFS4ERR_STALE_CLIENTID |
| No confirm verification | Full SETCLIENTID/SETCLIENTID_CONFIRM two-step with verifier | Phase 9 | Prevents stale client state corruption |
| Random placeholder stateids | Tracked stateids with type-tagged "other" field | Phase 9 | Stateids can be validated, expired, looked up |
| No seqid validation | Per-open-owner seqid tracking with replay cache | Phase 9 | Prevents duplicate operations under network retransmit |
| No lease management | Configurable lease timers with auto-expiry | Phase 9 | Server reclaims resources from disconnected clients |
| No grace period for NFSv4 | Grace period with CLAIM_PREVIOUS support | Phase 9 | Clients can reclaim state after server restart |
| OPEN accepts all, CLOSE discards | OPEN tracks share_access/deny, CLOSE cleans up state | Phase 9 | Share mode enforcement possible |
| READ/WRITE accept all stateids | Validate stateids, renew leases implicitly | Phase 9 | Detects stale/expired state, keeps leases alive |

**Deprecated/outdated:**
- `nextClientID` global atomic counter (setclientid.go:16) -- replace with StateManager
- `V4ClientState` placeholder struct (types.go:124) -- extend with ClientRecord reference
- Phase 7 "accept ALL stateids" comments in read.go/write.go -- replace with validation + special stateid bypass

## Data Structures Summary

The following structures form the NFSv4 state hierarchy:

```
StateManager
├── ClientRecord (keyed by clientid4)
│   ├── ClientID (uint64)
│   ├── Verifier (8 bytes, from client)
│   ├── ConfirmVerifier (8 bytes, server-generated)
│   ├── Confirmed (bool)
│   ├── Callback (for Phase 11 delegations)
│   ├── Lease (timer + last renewal time)
│   └── OpenOwners[]
│       ├── OpenOwner
│       │   ├── OwnerData (opaque bytes)
│       │   ├── LastSeqID (uint32)
│       │   ├── LastResult (cached for replay)
│       │   ├── Confirmed (after OPEN_CONFIRM)
│       │   └── OpenStates[]
│       │       └── OpenState
│       │           ├── Stateid4 (seqid + other[12])
│       │           ├── FileHandle ([]byte)
│       │           ├── ShareAccess (uint32)
│       │           ├── ShareDeny (uint32)
│       │           └── LockStates[] (Phase 10)
│       └── ...
├── openStateByOther (map[[12]byte]*OpenState)  -- fast stateid lookup
├── openOwners (map[ownerKey]*OpenOwner)        -- fast owner lookup
├── GracePeriodState
│   ├── Active (bool)
│   ├── Duration (time.Duration)
│   └── Timer (*time.Timer)
└── Boot epoch (uint32) + counters
```

## Relationship to Phase 10 and Phase 11

Phase 9 builds the foundation that Phases 10 and 11 extend:

| Phase 9 Provides | Phase 10 Adds | Phase 11 Adds |
|-------------------|---------------|---------------|
| ClientRecord with lease | Lock-owner tracking | Delegation state type |
| Stateid generation (type=Open) | Stateid generation (type=Lock) | Stateid generation (type=Deleg) |
| Open-owner seqid validation | Lock-owner seqid validation | Delegation recall callbacks |
| Lease renewal mechanism | Lock state tied to lease | Delegation tied to lease |
| Grace period for OPEN reclaim | Grace period for LOCK reclaim | Delegation recall on conflict |
| StateManager.ValidateStateid() | LOCK/LOCKT/LOCKU handlers | CB_RECALL callback channel |
| share_access/share_deny tracking | Byte-range lock conflict detection | Read/Write delegation grants |

**Design the StateManager to accommodate Phase 10/11 from the start:**
- The stateid "other" field type tag should have reserved values for Lock and Deleg states
- The OpenState struct should have a `LockStates []*LockState` slice (empty in Phase 9)
- The ClientRecord should have a `Delegations` field (nil in Phase 9)
- Lock-owner tracking data structures should be defined but not implemented until Phase 10

## Configuration

The following configuration values are relevant:

```yaml
# Existing config (pkg/config/config.go)
lock:
  grace_period: 90s   # Reuse for NFSv4 grace period duration

# New NFSv4-specific config (to add)
adapters:
  nfs:
    v4_lease_duration: 90s    # NFSv4 lease period (default: 90s, same as Linux nfsd)
    v4_max_open_files: 65536  # Maximum tracked open stateids
    v4_max_clients: 1024      # Maximum simultaneous client records
```

The lease duration MUST be reported to clients via the `lease_time` attribute (FATTR4_LEASE_TIME, bit 10). This is already in the supported attributes list from Phase 7.

## Open Questions

1. **State persistence depth for STATE-08**
   - What we know: Requirement STATE-08 says "State recovery via metadata store." The current metadata store interface has no state-specific methods.
   - What's unclear: How much state to persist? Full open stateids? Just client records? Just enough for grace period reclaim?
   - Recommendation: For Phase 9, persist only client records (clientid, verifier, callback) to the control plane store. This enables grace period reclaim detection. Full stateid persistence is complex and can be deferred to a later phase. On restart, enter grace period, let clients reclaim via CLAIM_PREVIOUS in OPEN.

2. **OPEN_CONFIRM deprecation path**
   - What we know: OPEN_CONFIRM is only needed for NFSv4.0 (not 4.1+). The current implementation always sets OPEN4_RESULT_CONFIRM in rflags.
   - What's unclear: Should Phase 9 stop requiring OPEN_CONFIRM for already-confirmed open-owners?
   - Recommendation: Per RFC 7530, OPEN_CONFIRM is only required for the FIRST use of an open-owner. Once confirmed, subsequent OPENs from the same owner should NOT set OPEN4_RESULT_CONFIRM. Phase 9 should implement this correctly: check `openOwner.Confirmed`, only set flag if false.

3. **Integration with existing lock manager**
   - What we know: The existing `pkg/metadata/lock` manager handles NLM and SMB locks. Phase 10 will add NFSv4 locks through this manager.
   - What's unclear: Should Phase 9's StateManager reference the lock manager, or should that coupling wait until Phase 10?
   - Recommendation: Phase 9 should NOT couple to the lock manager. The StateManager is purely NFSv4 protocol state. Phase 10 will bridge StateManager lock-owners to the unified lock manager via a new adapter layer.

4. **Lease duration in GETATTR response**
   - What we know: FATTR4_LEASE_TIME (bit 10) should return the server's lease duration. The attrs encoder currently returns a hardcoded value or skips it.
   - What's unclear: Is the lease_time attribute already encoded correctly?
   - Recommendation: Verify and update the lease_time encoding in `attrs/encode.go` to read from the StateManager's configured lease duration. This is a small change but important for client behavior.

## Recommended Plan Structure

### Plan 09-01: Client ID Management (SETCLIENTID, SETCLIENTID_CONFIRM)
**Scope:** StateManager creation, ClientRecord, five-case SETCLIENTID algorithm, SETCLIENTID_CONFIRM with verifier validation, client record lifecycle.
**Files:** `state/manager.go`, `state/client.go`, `state/client_test.go`, `handlers/setclientid.go` (upgrade), `handlers/handler.go` (add StateManager field), `types/types.go` (extend V4ClientState)
**Complexity:** High -- the five-case algorithm is the most complex part of the phase
**Tests:** ~25 tests: new client, client reboot, callback update, unconfirmed replace, confirm with correct/wrong verifier, confirm after timeout, NFS4ERR_CLID_INUSE for different principal, concurrent SETCLIENTID from same client

### Plan 09-02: State ID Generation and Validation
**Scope:** Stateid generation with type-tagged "other" field, stateid lookup, validation (seqid check, epoch check, expired check), special stateid bypass. Integration into OPEN (generate real stateids), CLOSE (clean up state), READ/WRITE (validate stateids).
**Files:** `state/stateid.go`, `state/stateid_test.go`, `state/openowner.go` (OpenState struct), `handlers/open.go` (upgrade), `handlers/close.go` (upgrade), `handlers/read.go` (add validation), `handlers/write.go` (add validation)
**Complexity:** Medium -- clear validation rules, but must carefully preserve special stateid bypass
**Tests:** ~20 tests: generate unique stateids, validate current seqid, reject old seqid, reject bad stateid, accept special stateids, stale after restart, lookup by "other" field

### Plan 09-03: Open-Owner and Lock-Owner Tracking
**Scope:** OpenOwner struct with per-owner seqid tracking, replay cache (last request + result), OPEN_CONFIRM confirmation flag, share_access/share_deny accumulation, OPEN_DOWNGRADE integration. Define LockOwner struct (empty implementation, Phase 10 fills in).
**Files:** `state/openowner.go` (extend), `state/openowner_test.go`, `state/lockowner.go` (define types only), `handlers/open.go` (seqid validation), `handlers/close.go` (seqid validation), `handlers/stubs.go` (OPEN_DOWNGRADE upgrade)
**Complexity:** High -- seqid validation + replay cache + OPEN_CONFIRM conditional flag
**Tests:** ~25 tests: seqid increment, seqid replay returns cached result, NFS4ERR_BAD_SEQID for gap, OPEN_CONFIRM only for new owners, share_access accumulation across multiple OPENs, CLOSE cleans up owner state

### Plan 09-04: Lease Management (RENEW, Expiration)
**Scope:** LeaseState with configurable timer, explicit RENEW handler upgrade, implicit renewal on stateid-carrying operations, lease expiration callback that cleans up all client state, lease_time attribute encoding in GETATTR.
**Files:** `state/lease.go`, `state/lease_test.go`, `handlers/renew.go` (upgrade), `handlers/compound.go` (implicit renewal hook), `attrs/encode.go` (lease_time from config), `pkg/config/config.go` (v4 lease config)
**Complexity:** Medium -- timer management is straightforward, integration with compound dispatcher needs care
**Tests:** ~15 tests: explicit renew, implicit renew via READ, lease expiry cleans state, renew with bad clientid returns NFS4ERR_STALE_CLIENTID, configurable duration, timer reset on renew

### Plan 09-05: State Recovery and Grace Period
**Scope:** NFSv4 grace period integration (wrap GracePeriodManager or build NFSv4-specific), CLAIM_PREVIOUS support in OPEN, NFS4ERR_GRACE for non-reclaim operations, client record persistence to control plane store (basic), recovery on startup.
**Files:** `state/grace.go`, `state/grace_test.go`, `state/manager.go` (startup/shutdown hooks), `handlers/open.go` (CLAIM_PREVIOUS support), `pkg/controlplane/store/` (client record persistence schema)
**Complexity:** Medium-High -- grace period reuses existing pattern but CLAIM_PREVIOUS adds OPEN complexity
**Tests:** ~15 tests: enter grace period, reject new OPEN during grace, allow CLAIM_PREVIOUS, grace period auto-exit, early exit when all clients reclaim, persist client records, load on restart

## Sources

### Primary (HIGH confidence)
- **RFC 7530** (https://www.rfc-editor.org/rfc/rfc7530.html) - NFSv4.0 protocol specification: Sections 9 (state management), 9.1 (client/state ID), 9.5 (leases), 9.6 (recovery), 16.28 (RENEW), 16.33 (SETCLIENTID), 16.34 (SETCLIENTID_CONFIRM)
- **Existing codebase** - Phase 6-8 handlers (setclientid.go, renew.go, open.go, close.go, stubs.go), types (types.go, constants.go), lock infrastructure (pkg/metadata/lock/), config (pkg/config/)
- **Linux kernel nfsd** (https://github.com/torvalds/linux/blob/master/fs/nfsd/nfs4state.c, state.h) - Reference implementation for data structures: nfs4_client, nfs4_stateowner, nfs4_openowner, nfs4_ol_stateid

### Secondary (MEDIUM confidence)
- **RFC 7931** (https://datatracker.ietf.org/doc/rfc7931/) - NFSv4.0 Migration specification update: SETCLIENTID principal checking rules
- **Linux kernel client identifier docs** (https://docs.kernel.org/filesystems/nfs/client-identifier.html) - How Linux NFS clients generate nfs_client_id4
- **LWN.net "NFS: the new millennium"** (https://lwn.net/Articles/898262/) - Overview of NFSv4 state management concepts

### Tertiary (LOW confidence)
- **libnfs-go** (https://github.com/smallfz/libnfs-go) - Go NFSv4 server implementation (experimental, limited state management visible)
- **NetApp KB** (https://kb.netapp.com/Legacy/ONTAP/7Mode/What_are_stateid_in-use_owners_free_owners_client_count_and_lease_count_in_NFSv4_implementation_of_NetApp_shared_storage) - NetApp NFSv4 state concepts (vendor-specific)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - all packages exist, no new dependencies, clear separation of concerns
- Architecture (state manager design): HIGH - follows Linux nfsd patterns, RFC-specified algorithms, DittoFS existing patterns
- Data structures: HIGH - directly derived from RFC 7530 XDR definitions and Linux kernel state.h
- SETCLIENTID algorithm: HIGH - RFC 7530 Section 9.1.1 is explicit about the five cases
- Lease management: HIGH - well-understood timer-based approach, existing GracePeriodManager as reference
- Grace period integration: MEDIUM - need to verify CLAIM_PREVIOUS interaction with existing OPEN handler
- State persistence (STATE-08): MEDIUM - basic approach clear, depth of persistence is a design choice
- Pitfalls: HIGH - derived from RFC error conditions, Linux kernel edge cases, and DittoFS codebase analysis

**Research date:** 2026-02-13
**Valid until:** 2026-03-15 (stable; RFC-based, well-established patterns)
