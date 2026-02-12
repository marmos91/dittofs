# Architecture Research: NFSv4 Server State Management

**Domain:** NFSv4 Protocol Server Implementation
**Researched:** 2026-02-04
**Confidence:** MEDIUM-HIGH

## Executive Summary

NFSv4 fundamentally differs from NFSv3 by being a stateful protocol. The server must track client identities, open files, locks, delegations, and sessions (NFSv4.1+). This research documents the standard architecture patterns for NFSv4 state management components, with specific recommendations for integrating into DittoFS's existing architecture.

## Standard NFSv4 Server Architecture

### System Overview

```
+-----------------------------------------------------------------------------+
|                         Protocol Layer (v4/)                                |
|  +---------------+  +----------------+  +----------------+                  |
|  | COMPOUND      |  | Operation      |  | Callback       |                  |
|  | Processor     |  | Handlers       |  | Client         |                  |
|  | (dispatch)    |  | (nfs4proc)     |  | (CB_* ops)     |                  |
|  +-------+-------+  +-------+--------+  +-------+--------+                  |
|          |                  |                   |                           |
+----------+------------------+-------------------+---------------------------+
           |                  |                   |
+----------+------------------+-------------------+---------------------------+
|                        State Manager                                        |
|  +------------------+  +------------------+  +------------------+           |
|  | Client Manager   |  | Session Manager  |  | Lease Manager    |           |
|  | - clientid table |  | - session table  |  | - lease renewal  |           |
|  | - owner tracking |  | - slot table     |  | - expiration     |           |
|  | - verifier mgmt  |  | - DRC (replay)   |  | - grace period   |           |
|  +--------+---------+  +--------+---------+  +--------+---------+           |
|           |                     |                     |                     |
|  +--------+---------+  +--------+---------+  +--------+---------+           |
|  | State ID Manager |  | Lock Manager     |  | Delegation Mgr   |           |
|  | - open states    |  | - byte-range     |  | - read/write     |           |
|  | - lock states    |  | - conflict check |  | - recall logic   |           |
|  | - deleg states   |  | - cross-protocol |  | - callback queue |           |
|  +--------+---------+  +--------+---------+  +--------+---------+           |
|           |                     |                     |                     |
+----------+----------------------+---------------------+---------------------+
           |                      |                     |
+----------+----------------------+---------------------+---------------------+
|                    Recovery Store (Persistent)                              |
|  +------------------+  +------------------+  +------------------+           |
|  | Client Records   |  | Grace Period     |  | Reclaim Tracker  |           |
|  | (owner names)    |  | (timestamps)     |  | (state history)  |           |
|  +------------------+  +------------------+  +------------------+           |
+-----------------------------------------------------------------------------+
           |
+----------+------------------------------------------------------------------+
|                    Existing DittoFS Services                                |
|  +------------------+  +------------------+  +------------------+           |
|  | MetadataService  |  | BlockService     |  | LockManager      |           |
|  | (file metadata)  |  | (content)        |  | (byte-range)     |           |
|  +------------------+  +------------------+  +------------------+           |
+-----------------------------------------------------------------------------+
```

### Component Responsibilities

| Component | Responsibility | Typical Implementation |
|-----------|----------------|------------------------|
| **Client Manager** | Track NFSv4 client identities, owner strings, verifiers | Hash table keyed by clientid; handles SETCLIENTID/EXCHANGE_ID |
| **Session Manager** | NFSv4.1 session handling, slot tables, duplicate request cache | Per-client session list; handles CREATE_SESSION/DESTROY_SESSION |
| **Lease Manager** | Track lease validity, handle renewals, detect expired clients | Timer-based; background "laundromat" thread for cleanup |
| **State ID Manager** | Generate/validate stateids, track open/lock/delegation states | State tables keyed by stateid; links to client and file |
| **Lock Manager** | Byte-range locking with conflict detection | Existing DittoFS LockManager can be extended |
| **Delegation Manager** | Grant/recall read/write delegations, manage callback queue | Callback client for CB_RECALL; tracks delegation holders |
| **Recovery Store** | Persist client records for grace period recovery | Filesystem or database; tracks "active" clients |

## Recommended Project Structure

Based on analysis of Linux nfsd, NFS-Ganesha, and nfs4j, here is the recommended structure for DittoFS:

```
internal/protocol/nfs/
├── v4/                           # NFSv4 specific implementation
│   ├── doc.go                    # Package documentation
│   ├── handlers/                 # NFSv4 operation handlers
│   │   ├── compound.go           # COMPOUND processor
│   │   ├── exchange_id.go        # EXCHANGE_ID (clientid establishment)
│   │   ├── create_session.go     # CREATE_SESSION
│   │   ├── sequence.go           # SEQUENCE (session validation)
│   │   ├── open.go               # OPEN (creates open stateid)
│   │   ├── close.go              # CLOSE
│   │   ├── lock.go               # LOCK/LOCKT/LOCKU
│   │   ├── read.go               # READ (stateid validation)
│   │   ├── write.go              # WRITE (stateid validation)
│   │   ├── commit.go             # COMMIT
│   │   ├── delegreturn.go        # DELEGRETURN
│   │   ├── reclaim_complete.go   # RECLAIM_COMPLETE
│   │   └── ... (40+ operations)
│   ├── callback/                 # Callback client implementation
│   │   ├── client.go             # CB_COMPOUND RPC client
│   │   ├── recall.go             # CB_RECALL implementation
│   │   └── notify.go             # CB_NOTIFY (optional)
│   └── xdr/                      # NFSv4 XDR types
│       ├── types.go              # NFSv4 type definitions
│       ├── stateid.go            # Stateid encoding/decoding
│       └── compound.go           # COMPOUND request/response

pkg/nfs4state/                    # NFSv4 state management (new package)
├── doc.go                        # Package documentation
├── manager.go                    # StateManager interface + implementation
├── client.go                     # NFS4Client struct and clientid handling
├── session.go                    # NFS4Session struct (NFSv4.1)
├── stateid.go                    # StateID generation and types
├── open_state.go                 # Open state tracking
├── lock_state.go                 # Lock state (delegates to metadata.LockManager)
├── delegation.go                 # Delegation state and recall logic
├── lease.go                      # Lease management and expiration
├── grace.go                      # Grace period handling
├── recovery.go                   # State recovery coordination
└── store/                        # Persistent recovery store
    ├── interface.go              # RecoveryStore interface
    ├── memory.go                 # In-memory (ephemeral, testing)
    └── file.go                   # File-based persistence
```

### Structure Rationale

- **`internal/protocol/nfs/v4/`**: Keeps NFSv4 handlers separate from v3, following existing pattern
- **`pkg/nfs4state/`**: State management as a public package allows:
  - Sharing state between NFS and SMB adapters (cross-protocol locking)
  - Testing state logic independently of protocol handlers
  - Future API exposure for state inspection/management
- **Recovery store abstraction**: Enables different persistence backends (file, database)

## Architectural Patterns

### Pattern 1: Compound Operation Processing

**What:** NFSv4 uses a single COMPOUND RPC that contains multiple operations executed sequentially. Each operation can reference results from previous operations via the "current filehandle" concept.

**When to use:** All NFSv4 requests (unlike NFSv3's single-operation RPCs)

**Trade-offs:**
- Pro: Reduces round trips, enables atomic multi-operation sequences
- Con: More complex error handling, partial success states

**Example:**
```go
// Compound processor maintains current/saved filehandle state
type CompoundContext struct {
    CurrentFH   metadata.FileHandle
    SavedFH     metadata.FileHandle
    Client      *NFS4Client
    Session     *NFS4Session  // nil for NFSv4.0
    StateID     *StateID      // current operation's stateid
    MinorVer    uint32
}

func ProcessCompound(ctx context.Context, args *CompoundArgs) *CompoundRes {
    cctx := &CompoundContext{MinorVer: args.MinorVersion}
    results := make([]OperationResult, 0, len(args.Operations))

    for _, op := range args.Operations {
        result := dispatchOperation(ctx, cctx, op)
        results = append(results, result)

        // Stop on first error
        if result.Status != NFS4_OK {
            break
        }
    }

    return &CompoundRes{Status: results[len(results)-1].Status, Results: results}
}
```

### Pattern 2: Stateid Validation Chain

**What:** Every stateful operation (READ, WRITE, LOCK, etc.) must validate the provided stateid against the server's state tables.

**When to use:** Any operation that requires open/lock state

**Trade-offs:**
- Pro: Ensures state consistency, detects stale/invalid state
- Con: Additional lookup overhead on every I/O operation

**Example:**
```go
type StateID struct {
    Seqid  uint32   // Increments on state changes
    Other  [12]byte // Server-generated identifier
}

func (sm *StateManager) ValidateStateID(ctx context.Context,
    fh metadata.FileHandle, stateid StateID, op Operation) (*OpenState, error) {

    // Check for special stateids (anonymous, READ bypass)
    if isSpecialStateID(stateid) {
        return sm.handleSpecialStateID(ctx, fh, stateid, op)
    }

    // Look up state by stateid.Other
    state, err := sm.states.Get(stateid.Other)
    if err != nil {
        return nil, NFS4ERR_BAD_STATEID
    }

    // Verify seqid (detect replay or old state)
    if stateid.Seqid != state.Seqid && stateid.Seqid != 0 {
        if stateid.Seqid < state.Seqid {
            return nil, NFS4ERR_OLD_STATEID
        }
        return nil, NFS4ERR_BAD_STATEID
    }

    // Verify file handle matches
    if !bytes.Equal(state.FileHandle, fh) {
        return nil, NFS4ERR_BAD_STATEID
    }

    // Renew lease on successful validation
    sm.leaseManager.Renew(state.ClientID)

    return state, nil
}
```

### Pattern 3: Lease-Based Expiration (Laundromat)

**What:** Background goroutine periodically scans for expired clients and revokes their state. Named after Linux nfsd's "laundromat" thread.

**When to use:** Required for any NFSv4 server

**Trade-offs:**
- Pro: Automatic cleanup, predictable resource bounds
- Con: Must carefully handle concurrent access, callback delays

**Example:**
```go
type LeaseManager struct {
    leaseTime time.Duration
    clients   *ClientTable
    ticker    *time.Ticker
    stopCh    chan struct{}
}

func (lm *LeaseManager) Start() {
    lm.ticker = time.NewTicker(lm.leaseTime / 2)
    go lm.laundromat()
}

func (lm *LeaseManager) laundromat() {
    for {
        select {
        case <-lm.ticker.C:
            lm.expireClients()
        case <-lm.stopCh:
            return
        }
    }
}

func (lm *LeaseManager) expireClients() {
    now := time.Now()
    expired := lm.clients.FindExpired(now)

    for _, client := range expired {
        // Check for "courtesy" extension (no conflicting requests)
        if lm.canExtendCourtesy(client) {
            continue
        }

        // Recall any delegations first
        if err := lm.recallDelegations(client); err != nil {
            // Client unresponsive, force revoke
            lm.revokeClientState(client)
        }

        lm.clients.Remove(client.ID)
    }
}
```

### Pattern 4: Delegation Callback

**What:** Server can grant read/write delegations to clients for caching. When conflicts arise, server recalls delegations via callback RPC.

**When to use:** Optimization for exclusive access patterns

**Trade-offs:**
- Pro: Major performance improvement for single-client workloads
- Con: Complex callback handling, unresponsive client issues

**Example:**
```go
type DelegationManager struct {
    delegations *DelegationTable
    callbacks   *CallbackClient
    recallWait  time.Duration
}

func (dm *DelegationManager) RecallDelegation(ctx context.Context,
    deleg *Delegation, reason RecallReason) error {

    // Send CB_RECALL to client
    err := dm.callbacks.Recall(ctx, deleg.ClientID, deleg.StateID, deleg.FileHandle)
    if err != nil {
        // Callback failed - mark for forced revocation
        deleg.Status = DelegationRevoking
        return err
    }

    // Wait for DELEGRETURN with timeout
    select {
    case <-deleg.ReturnedCh:
        return nil
    case <-time.After(dm.recallWait):
        // Client didn't return delegation, force revoke
        dm.revokeDelegation(deleg)
        return ErrDelegationRevoked
    }
}
```

## Data Flow

### Client Establishment Flow (NFSv4.1)

```
Client                    Server
  |                         |
  | EXCHANGE_ID            |
  |------------------------>|
  |                         | - Generate clientid (timestamp | instance | counter)
  |                         | - Store client record with owner/verifier
  |   clientid, seqid      |
  |<------------------------|
  |                         |
  | CREATE_SESSION         |
  |------------------------>|
  |                         | - Validate clientid
  |                         | - Generate sessionid
  |                         | - Initialize slot table (DRC)
  |   sessionid, slot info |
  |<------------------------|
  |                         |
  | SEQUENCE + RECLAIM_COMPLETE (if recovering)
  |------------------------>|
  |                         | - Validate session
  |                         | - Mark client as "active"
  |   OK                   |
  |<------------------------|
```

### Open/Read/Write Flow

```
Client                    Server
  |                         |
  | COMPOUND: SEQUENCE + PUTFH + OPEN
  |------------------------>|
  |                         | - Validate session (SEQUENCE)
  |                         | - Set current FH (PUTFH)
  |                         | - Create open state
  |                         | - Generate open stateid
  |                         | - Optionally grant delegation
  |   open_stateid, delegation_stateid (optional)
  |<------------------------|
  |                         |
  | COMPOUND: SEQUENCE + PUTFH + READ(stateid)
  |------------------------>|
  |                         | - Validate session
  |                         | - Validate stateid (renews lease)
  |                         | - Check access mode vs open state
  |                         | - Perform read
  |   data                 |
  |<------------------------|
```

### State Management Flow

```
                          StateManager
                               |
         +--------------------+--------------------+
         |                    |                    |
    ClientManager        SessionManager      LeaseManager
         |                    |                    |
    +----+----+          +----+----+          +----+
    |         |          |         |          |
 clients   owners     sessions   slots     timers
 (hash)    (hash)      (hash)   (array)   (heap)
         |                    |                    |
         +--------------------+--------------------+
                               |
                         StateIDManager
                               |
         +--------------------+--------------------+
         |                    |                    |
    OpenStates           LockStates         Delegations
    (by stateid)       (by stateid)       (by stateid)
         |                    |                    |
         +--------------------+--------------------+
                               |
                       RecoveryStore
                       (persistent)
```

## Scaling Considerations

| Scale | Architecture Adjustments |
|-------|--------------------------|
| 0-1k clients | Single StateManager instance, in-memory state, file-based recovery |
| 1k-100k clients | Sharded state tables by clientid hash, async lease processing |
| 100k+ clients | Distributed state (not recommended for NFSv4.0), consider NFSv4.1 session affinity |

### Scaling Priorities

1. **First bottleneck: State table locking** - Use RWMutex for state tables, separate locks for different state types (open, lock, delegation)
2. **Second bottleneck: Lease expiration scanning** - Use heap/priority queue sorted by expiration time instead of full table scans
3. **Third bottleneck: Recovery store I/O** - Batch writes, async persistence with WAL

## Anti-Patterns

### Anti-Pattern 1: Monolithic State Table

**What people do:** Single map with global lock for all state types
**Why it's wrong:** Contention between unrelated operations (e.g., lock check blocks open)
**Do this instead:** Separate tables for clients, sessions, opens, locks, delegations with independent locks

### Anti-Pattern 2: Synchronous Delegation Recall

**What people do:** Block on delegation recall during conflicting OPEN
**Why it's wrong:** Unresponsive client blocks all other clients trying to access file
**Do this instead:** Async recall with timeout, fail conflicting request if recall doesn't complete in time

### Anti-Pattern 3: Embedding Stateid Validation in Handlers

**What people do:** Each handler (READ, WRITE, LOCK) implements its own stateid validation
**Why it's wrong:** Inconsistent validation, duplicated code, harder to audit
**Do this instead:** Centralized ValidateStateID() called before dispatching to handler

### Anti-Pattern 4: Ignoring Grace Period

**What people do:** Accept new state immediately after restart
**Why it's wrong:** Clients may have locks from before crash that conflict with new requests
**Do this instead:** Implement grace period where only RECLAIM operations are allowed

## Integration Points

### Integration with Existing DittoFS Components

| Boundary | Communication | Notes |
|----------|---------------|-------|
| StateManager <-> MetadataService | Direct method calls | StateManager delegates file operations to MetadataService |
| StateManager <-> LockManager | Interface abstraction | Extend existing LockManager to understand NFSv4 lock owners |
| NFS4Adapter <-> StateManager | Dependency injection | Adapter creates/owns StateManager instance |
| StateManager <-> RecoveryStore | Interface abstraction | Allow pluggable persistence (file, BadgerDB, PostgreSQL) |

### Cross-Protocol Locking (NFS + SMB)

| Protocol | Lock Semantics | Integration Approach |
|----------|----------------|---------------------|
| NFSv4 | Byte-range, advisory | Use existing LockManager with NFSv4 owner types |
| SMB | Share mode + byte-range, mandatory | Same LockManager, different owner type |
| Cross-protocol | Both check same lock table | Unified LockManager, protocol-specific owner identity |

**Key decision:** The existing `metadata.LockManager` already supports session-based locking. NFSv4 state adds:
- Owner concept (clientid + owner string)
- Stateid tracking (lock state generates stateid)
- Lock upgrade/downgrade semantics

**Recommendation:** Extend LockManager interface to support:
```go
type NFSv4LockOwner struct {
    ClientID  uint64
    OwnerStr  []byte
}

// Add to LockManager interface
LockWithOwner(handleKey string, owner NFSv4LockOwner, lock FileLock) (*LockState, error)
```

## Build Order (Dependencies)

Based on the architecture analysis, here is the recommended build order:

### Phase 1: Core State Infrastructure
1. **StateID types and generation** - Foundation for all state tracking
2. **Client Manager** - Handles SETCLIENTID/EXCHANGE_ID
3. **Lease Manager** - Laundromat thread for expiration
4. **Basic Recovery Store** - File-based client persistence

### Phase 2: Open State and Basic Operations
1. **Open State Manager** - OPEN/CLOSE operations
2. **Stateid Validation** - Shared validation logic
3. **NFSv4 handlers for stateful READ/WRITE** - Uses open stateids

### Phase 3: Locking
1. **Lock State integration** - Connect to existing LockManager
2. **LOCK/LOCKT/LOCKU handlers** - Generate lock stateids
3. **Cross-protocol lock awareness** - SMB integration

### Phase 4: Sessions (NFSv4.1)
1. **Session Manager** - CREATE_SESSION/DESTROY_SESSION
2. **Slot table and DRC** - Duplicate request cache
3. **SEQUENCE operation** - Session validation

### Phase 5: Delegations
1. **Callback client** - RPC client for CB_* operations
2. **Delegation Manager** - Grant/recall logic
3. **Delegation stateids** - Track delegation state

### Phase 6: Recovery
1. **Grace period handling** - RECLAIM operations
2. **Full recovery flow** - Server restart recovery
3. **Edge cases** - Revocation, courtesy clients

## Sources

### RFC Standards (HIGH confidence)
- [RFC 7530 - NFSv4.0 Protocol](https://datatracker.ietf.org/doc/html/rfc7530)
- [RFC 5661 - NFSv4.1 Protocol](https://www.rfc-editor.org/rfc/rfc5661.html)
- [RFC 8881 - NFSv4.1 (updated)](https://www.rfc-editor.org/rfc/rfc8881.pdf)

### Linux Kernel Implementation (HIGH confidence)
- [Linux nfsd source - nfs4state.c](https://elixir.bootlin.com/linux/latest/source/fs/nfsd/nfs4state.c)
- [Linux NFSv4.1 Server Documentation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html)
- [Nfsd4 Server Recovery Design](https://client.linux-nfs.org/wiki/index.php/Nfsd4_server_recovery)

### Reference Implementations (MEDIUM confidence)
- [nfs4j - Java NFSv4 Implementation](https://github.com/dCache/nfs4j)
- [nfs4j StateHandler Source](https://github.com/dCache/nfs4j/blob/master/core/src/main/java/org/dcache/nfs/v4/NFSv4StateHandler.java)
- [NFS-Ganesha Project](https://github.com/nfs-ganesha/nfs-ganesha)
- [NFS-Ganesha Lock Management](https://deepwiki.com/nfs-ganesha/nfs-ganesha/6.1-lock-management)

### Cross-Protocol Locking (MEDIUM confidence)
- [Multiprotocol NAS and Locking](https://whyistheinternetbroken.wordpress.com/2015/05/20/techmultiprotocol-nas-locking-and-you/)
- [NetApp NFS/SMB Locking](https://kb.netapp.com/on-prem/ontap/da/NAS/NAS-KBs/How_does_file_locking_work_between_NFS_and_SMB_protocols)
- [Linux Dual-Protocol Support](https://linux-nfs.org/wiki/index.php?title=Dual-protocol_support)

---
*Architecture research for: NFSv4 Server State Management*
*Researched: 2026-02-04*
