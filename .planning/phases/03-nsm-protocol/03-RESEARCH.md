# Phase 3: NSM Protocol - Research

**Researched:** 2026-02-05
**Domain:** Network Status Monitor protocol (RPC 100024) for NFS crash recovery
**Confidence:** HIGH

## Summary

This phase implements the NSM (Network Status Monitor) protocol to enable crash recovery for NFS file locking. The research examined: the NSM protocol specification from the Open Group XNFS documentation, the existing ConnectionTracker and GracePeriodManager infrastructure from Phase 1, the NLM callback client pattern from Phase 2, and the Linux rpc.statd/sm-notify implementation documentation.

**Key findings:**
- NSM is RPC program 100024 version 1 (not v4 like NLM). It is protocol-independent infrastructure used by NLM for crash recovery.
- The existing ConnectionTracker from Phase 1 provides most of the client tracking infrastructure. NSM handlers extend it with: persistent client registrations, sm_state tracking, and crash notification callbacks.
- SM_NOTIFY uses a callback mechanism similar to NLM_GRANTED: server sends notification to registered client's callback address with the 16-byte private data (priv field) provided during SM_MON.
- FREE_ALL (NLM procedure 17/23) ties NSM to NLM: when NSM detects client crash, it calls NLM FREE_ALL to release that client's locks.
- Server restart flow: increment server epoch, send SM_NOTIFY to all registered clients in parallel, enter grace period, allow only reclaims.

**Primary recommendation:** Extend ConnectionTracker with NSM-specific fields (mon_name, callback info, priv, sm_state). Create internal/protocol/nsm/ mirroring the nlm/ structure. Implement SM_MON, SM_UNMON, SM_UNMON_ALL, SM_STAT, and SM_NOTIFY procedures. Add FREE_ALL handler to NLM. Use parallel notification pattern from Phase 2 for SM_NOTIFY on server restart.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `net` | 1.22+ | TCP connections for SM_NOTIFY callbacks | Standard network I/O |
| Go stdlib `sync` | 1.22+ | Thread-safe client registration map | Concurrent access control |
| Go stdlib `context` | 1.22+ | Timeout control for callbacks | 5s callback timeout (consistent with NLM) |
| Existing `internal/protocol/xdr` | - | Shared XDR encoding/decoding | Already refactored for NLM |
| Existing `internal/protocol/nfs/rpc` | - | RPC message handling | Reuse for NSM RPC |
| Existing `pkg/metadata/lock` | - | ConnectionTracker from Phase 1 | Extend for NSM |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/prometheus/client_golang` | 1.19+ | NSM metrics (nsm_* prefix) | Already in project |
| Existing `pkg/metadata/lock/store.go` | - | LockStore.DeleteLocksByClient | FREE_ALL implementation |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Extend ConnectionTracker | New ClientMonitor type | User decision: extend existing, enables SMB reuse |
| Callback-based crash detection | Heartbeat polling | User decision: callbacks only (matches NLM pattern) |
| Parallel SM_NOTIFY | Sequential notification | User decision: parallel for fastest recovery |

**No new dependencies required** - extend existing patterns.

## Architecture Patterns

### Recommended Project Structure

```
internal/protocol/
├── nsm/                              # NEW: NSM protocol implementation
│   ├── types/
│   │   ├── constants.go              # SM_PROG (100024), SM_VERS (1), procedure numbers
│   │   └── types.go                  # sm_name, my_id, mon_id, mon, stat_chge, status
│   ├── xdr/
│   │   ├── decode.go                 # Decode NSM requests
│   │   └── encode.go                 # Encode NSM responses
│   ├── handlers/
│   │   ├── handler.go                # Handler struct with ConnectionTracker ref
│   │   ├── null.go                   # SM_NULL (0)
│   │   ├── stat.go                   # SM_STAT (1)
│   │   ├── mon.go                    # SM_MON (2)
│   │   ├── unmon.go                  # SM_UNMON (3)
│   │   ├── unmon_all.go              # SM_UNMON_ALL (4)
│   │   └── notify.go                 # SM_NOTIFY (6) - incoming notifications
│   ├── callback/
│   │   ├── client.go                 # TCP callback client for status notifications
│   │   └── notify.go                 # SM_NOTIFY callback sending logic
│   ├── dispatch.go                   # NSM procedure dispatch table
│   └── metrics.go                    # nsm_* Prometheus metrics
│
├── nlm/
│   └── handlers/
│       └── free_all.go               # NEW: NLMPROC4_FREE_ALL (23)

pkg/metadata/lock/
├── connection.go                     # EXTEND: Add NSM-specific fields to ClientRegistration
├── store.go                          # EXTEND: Add client registration persistence
└── grace.go                          # Already has grace period logic

pkg/metadata/store/
├── memory/
│   └── clients.go                    # NEW: In-memory client registration storage
├── badger/
│   └── clients.go                    # NEW: BadgerDB client registration storage
└── postgres/
    └── clients.go                    # NEW: PostgreSQL client registration storage
```

### Pattern 1: Extended ClientRegistration for NSM

**What:** Add NSM-specific fields to ConnectionTracker's ClientRegistration.
**When to use:** SM_MON stores callback info, priv data, and sm_state.

**Example:**
```go
// Source: Per CONTEXT.md - extend existing ConnectionTracker

// ClientRegistration extended for NSM support
type ClientRegistration struct {
    // Existing fields from Phase 1
    ClientID     string
    AdapterType  string
    TTL          time.Duration
    RegisteredAt time.Time
    LastSeen     time.Time
    RemoteAddr   string
    LockCount    int

    // NSM-specific fields (new)
    MonName      string         // Monitored hostname (mon_id.mon_name)
    Priv         [16]byte       // Private data returned in callbacks
    SMState      int32          // Client's NSM state counter
    CallbackInfo *NSMCallback   // RPC callback details from my_id
}

// NSMCallback holds callback RPC details from SM_MON my_id field
type NSMCallback struct {
    Hostname string   // my_id.my_name
    Program  uint32   // my_id.my_prog (usually NLM program 100021)
    Version  uint32   // my_id.my_vers
    Proc     uint32   // my_id.my_proc (usually NLM_SM_NOTIFY or custom)
}
```

### Pattern 2: NSM Dispatcher Integration

**What:** Extend RPC dispatcher to route NSM program (100024) to NSM handlers.
**When to use:** NSM requests come through same port as NFS/NLM.

**Example:**
```go
// Source: Extend dispatch.go pattern from Phase 2

case ProgramNSM: // 100024
    if call.Version != SMVersion1 {
        return c.handleUnsupportedVersion(call, SMVersion1, "NSM", clientAddr)
    }
    replyData, err = c.handleNSMProcedure(ctx, call, procedureData, clientAddr)
```

### Pattern 3: SM_MON Handler with Idempotent Update

**What:** Register client for monitoring, update if already registered.
**When to use:** Client calls SM_MON to request crash notification.

**Example:**
```go
// Source: Per CONTEXT.md - update existing on duplicate SM_MON

func (h *Handler) MonHandler(ctx context.Context, args *types.SMMonArgs) (*types.SMStatRes, error) {
    // Check client limit
    if h.tracker.GetClientCount("") >= h.maxClients {
        return &types.SMStatRes{Result: types.StatFail, State: h.getServerState()}, nil
    }

    // Build registration from SM_MON args
    reg := &lock.ClientRegistration{
        ClientID:     args.Mon.MonID.MonName,
        AdapterType:  "nsm",
        TTL:          0, // NSM doesn't use TTL
        RegisteredAt: time.Now(),
        LastSeen:     time.Now(),
        RemoteAddr:   ctx.Value(remoteAddrKey).(string),
        MonName:      args.Mon.MonID.MonName,
        Priv:         args.Mon.Priv,
        SMState:      0, // Will be updated when client state changes
        CallbackInfo: &lock.NSMCallback{
            Hostname: args.Mon.MonID.MyID.MyName,
            Program:  args.Mon.MonID.MyID.MyProg,
            Version:  args.Mon.MonID.MyID.MyVers,
            Proc:     args.Mon.MonID.MyID.MyProc,
        },
    }

    // RegisterClient handles both new registration and idempotent update
    err := h.tracker.RegisterClient(reg.ClientID, reg.AdapterType, reg.RemoteAddr, reg.TTL)
    if err != nil {
        return &types.SMStatRes{Result: types.StatFail, State: h.getServerState()}, nil
    }

    // Store NSM-specific fields
    h.tracker.UpdateNSMInfo(reg.ClientID, reg.MonName, reg.Priv, reg.CallbackInfo)

    // Persist registration to metadata store
    if err := h.persistRegistration(ctx, reg); err != nil {
        logger.Warn("Failed to persist NSM registration", "client", reg.ClientID, "error", err)
        // Continue - registration is in memory
    }

    return &types.SMStatRes{
        Result: types.StatSucc,
        State:  h.getServerState(),
    }, nil
}
```

### Pattern 4: Parallel SM_NOTIFY on Server Restart

**What:** Send SM_NOTIFY to all registered clients in parallel.
**When to use:** Server restart triggers notification to all monitored clients.

**Example:**
```go
// Source: Per CONTEXT.md - parallel notification (mirrors Phase 2 waiter processing)

func (n *Notifier) NotifyAllClients(ctx context.Context) {
    clients := n.tracker.ListClients("")

    var wg sync.WaitGroup
    results := make(chan notifyResult, len(clients))

    for _, client := range clients {
        if client.CallbackInfo == nil {
            continue
        }

        wg.Add(1)
        go func(c *lock.ClientRegistration) {
            defer wg.Done()

            err := n.sendNotify(ctx, c)
            results <- notifyResult{
                clientID: c.ClientID,
                err:      err,
            }
        }(client)
    }

    // Wait for all notifications to complete
    go func() {
        wg.Wait()
        close(results)
    }()

    // Process results: failed notifications trigger lock cleanup
    for result := range results {
        if result.err != nil {
            logger.Warn("SM_NOTIFY failed, cleaning up client locks",
                "client", result.clientID,
                "error", result.err)
            // Per CONTEXT.md: mark as crashed and cleanup their locks
            n.handleClientCrash(ctx, result.clientID)
        }
    }
}

func (n *Notifier) sendNotify(ctx context.Context, client *lock.ClientRegistration) error {
    // 5 second total timeout (consistent with NLM_GRANTED)
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    // Build callback address
    addr := fmt.Sprintf("%s:%d", client.CallbackInfo.Hostname, client.CallbackInfo.Program)

    // Build status struct with mon_name, state, and priv
    status := &types.SMStatus{
        MonName: n.serverName,  // This server's name
        State:   n.getServerState(),
        Priv:    client.Priv,   // Return client's private data
    }

    // Send RPC callback
    return sendStatusCallback(ctx, addr, client.CallbackInfo, status)
}
```

### Pattern 5: FREE_ALL Handler Integration

**What:** NLM FREE_ALL releases all locks for a crashed client.
**When to use:** Called by NSM when client crash detected.

**Example:**
```go
// Source: Per CONTEXT.md - NLM procedure 17/23 for bulk lock release

// In internal/protocol/nlm/handlers/free_all.go
func (h *Handler) FreeAll(ctx *NLMHandlerContext, req *FreeAllRequest) (*FreeAllResponse, error) {
    // req.Name is the crashed client's hostname
    // Build client ID pattern: nlm:{hostname}:*
    clientIDPrefix := fmt.Sprintf("nlm:%s:", req.Name)

    // Get lock manager
    lm := h.runtime.LockManager()

    // Delete all locks matching this client
    count, err := lm.DeleteLocksByClientPrefix(clientIDPrefix)
    if err != nil {
        logger.Error("FREE_ALL failed", "client", req.Name, "error", err)
        // Per CONTEXT.md: best effort - continue even on errors
    }

    logger.Info("FREE_ALL completed", "client", req.Name, "locks_released", count)

    // Process blocking queue waiters for affected files
    // Released locks may unblock waiting requests
    h.processWaitersForReleasedLocks(ctx)

    // FREE_ALL has no response body per specification
    return &FreeAllResponse{}, nil
}
```

### Pattern 6: Client Registration Persistence

**What:** Store client registrations in metadata store.
**When to use:** Registrations must survive server restart for SM_NOTIFY.

**Example:**
```go
// Source: Per CONTEXT.md - reuse server epoch, persist in metadata store

// PersistedClientRegistration for storage
type PersistedClientRegistration struct {
    ClientID     string    `json:"client_id"`
    MonName      string    `json:"mon_name"`
    Priv         [16]byte  `json:"priv"`
    CallbackHost string    `json:"callback_host"`
    CallbackProg uint32    `json:"callback_prog"`
    CallbackVers uint32    `json:"callback_vers"`
    CallbackProc uint32    `json:"callback_proc"`
    RegisteredAt time.Time `json:"registered_at"`
    ServerEpoch  uint64    `json:"server_epoch"`
}

// ClientRegistrationStore interface for persistence
type ClientRegistrationStore interface {
    PutRegistration(ctx context.Context, reg *PersistedClientRegistration) error
    GetRegistration(ctx context.Context, clientID string) (*PersistedClientRegistration, error)
    DeleteRegistration(ctx context.Context, clientID string) error
    ListRegistrations(ctx context.Context) ([]*PersistedClientRegistration, error)
    DeleteAllRegistrations(ctx context.Context) (int, error)
}
```

### Anti-Patterns to Avoid

- **Creating new ClientMonitor type:** User decided to extend ConnectionTracker.
- **Proactive heartbeat polling:** User decided callback-based detection only.
- **Sequential SM_NOTIFY:** User decided parallel for fastest recovery.
- **Delaying lock cleanup:** User decided immediate cleanup when crash detected.
- **Ignoring FREE_ALL:** Must implement NLM procedure 17/23 for proper NSM integration.

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Client tracking | NSM-specific tracker | Extend `ConnectionTracker` | User decision, enables SMB reuse |
| Grace period management | NSM-specific grace | Phase 1 `GracePeriodManager` | Already tested, 90s default |
| Lock cleanup | NSM-specific unlock loop | `LockStore.DeleteLocksByClient()` | Atomic bulk delete |
| RPC callback | Custom callback code | Extend NLM callback client pattern | Same 5s timeout, same framing |
| Server epoch | NSM-specific state | `LockStore.GetServerEpoch()` | Already persisted, Phase 1 |
| XDR encoding | NSM-specific XDR | `internal/protocol/xdr` shared utils | Proven patterns |

**Key insight:** NSM is the crash recovery infrastructure that ties NLM and SMB together. Extending ConnectionTracker creates a unified client monitoring abstraction reusable across protocols.

## Common Pitfalls

### Pitfall 1: SM_NOTIFY Callback Address Resolution

**What goes wrong:** SM_NOTIFY sent to wrong address, client never receives notification.
**Why it happens:** my_id.my_name may be hostname needing DNS resolution; port derived from program number.
**How to avoid:** Use portmapper/rpcbind to resolve callback port, or assume standard NLM port.
**Warning signs:** SM_NOTIFY callbacks fail with connection refused; clients timeout waiting for notification.

### Pitfall 2: Stale Registrations After Server Restart

**What goes wrong:** Old registrations persist but clients have new sm_state.
**Why it happens:** Registrations persisted with old epoch; client already rebooted.
**How to avoid:** Compare sm_state in SM_MON with stored value; update if different.
**Warning signs:** Duplicate notifications; clients ignore notifications with wrong state.

### Pitfall 3: Race Between SM_NOTIFY and Client Reconnect

**What goes wrong:** Client reconnects and reclaims locks before receiving SM_NOTIFY.
**Why it happens:** Parallel SM_NOTIFY and client lock reclaim race.
**How to avoid:** Grace period blocks new locks; only reclaims allowed. NSM and NLM coordinate via grace period.
**Warning signs:** Client's reclaimed locks get wiped by FREE_ALL meant for old session.

### Pitfall 4: priv Field Handling

**What goes wrong:** Private data corrupted or lost in callback.
**Why it happens:** priv is 16-byte fixed array; improper copy or encoding.
**How to avoid:** Use `[16]byte` type (not slice); XDR encodes as opaque fixed array.
**Warning signs:** Clients fail to match callbacks to their requests; lock recovery fails.

### Pitfall 5: SM_UNMON_ALL Without Proper Cleanup

**What goes wrong:** SM_UNMON_ALL removes registration but locks remain.
**Why it happens:** Unmonitor doesn't mean crash; don't release locks on unmonitor.
**How to avoid:** SM_UNMON_ALL only removes NSM registration, not locks. Locks released only on crash (FREE_ALL) or explicit unlock.
**Warning signs:** Lock state inconsistent after clean client shutdown.

### Pitfall 6: Client Limit Enforcement Race

**What goes wrong:** Client count exceeds limit due to concurrent SM_MON requests.
**Why it happens:** Check and register not atomic.
**How to avoid:** Hold lock during count check and registration; or use atomic increment.
**Warning signs:** Memory growth under load; more registrations than configured limit.

## Code Examples

Verified patterns from official sources:

### NSM XDR Data Structures

```go
// Source: Open Group NSM specification
// https://pubs.opengroup.org/onlinepubs/9629799/chap11.htm

// SM_MAXSTRLEN is the maximum length for NSM strings
const SMMaxStrLen = 1024

// SMRes result enumeration
const (
    StatSucc uint32 = 0  // Monitoring established
    StatFail uint32 = 1  // Unable to monitor
)

// SMName identifies a host to monitor
type SMName struct {
    Name string  // mon_name<SM_MAXSTRLEN>
}

// MyID contains callback RPC information
type MyID struct {
    MyName string  // Callback hostname
    MyProg uint32  // RPC program number (e.g., NLM 100021)
    MyVers uint32  // Program version
    MyProc uint32  // Procedure number
}

// MonID combines monitored host and callback info
type MonID struct {
    MonName string  // Host to monitor
    MyID    MyID    // Callback details
}

// Mon is the SM_MON argument structure
type Mon struct {
    MonID MonID     // Monitor and callback info
    Priv  [16]byte  // Private data returned in notifications
}

// SMStatRes is returned by SM_STAT and SM_MON
type SMStatRes struct {
    Result uint32  // StatSucc or StatFail
    State  int32   // Current NSM state (odd=up, even=down)
}

// SMStat holds just the state number
type SMStat struct {
    State int32
}

// StatChge is the SM_NOTIFY argument
type StatChge struct {
    MonName string  // Host that changed state
    State   int32   // New state number
}

// Status is sent in SM_NOTIFY callbacks to registered monitors
type Status struct {
    MonName string    // Host that changed state
    State   int32     // New state number
    Priv    [16]byte  // Client's private data from SM_MON
}
```

### NSM Procedure Numbers

```go
// Source: Open Group NSM specification

const (
    // ProgramNSM is the NSM RPC program number
    ProgramNSM uint32 = 100024

    // SMVersion1 is the only NSM version
    SMVersion1 uint32 = 1
)

const (
    SMProcNull     uint32 = 0  // NULL - ping
    SMProcStat     uint32 = 1  // STAT - query host status
    SMProcMon      uint32 = 2  // MON - register for monitoring
    SMProcUnmon    uint32 = 3  // UNMON - unregister single host
    SMProcUnmonAll uint32 = 4  // UNMON_ALL - unregister all hosts
    SMProcSimuCrsh uint32 = 5  // SIMU_CRASH - simulate crash (testing)
    SMProcNotify   uint32 = 6  // NOTIFY - state change notification
)
```

### XDR Encoding for SM_MON

```go
// Source: Derived from existing internal/protocol/xdr patterns

func DecodeSMMonArgs(r io.Reader) (*Mon, error) {
    mon := &Mon{}

    // Decode mon_id.mon_name (string)
    monName, err := xdr.DecodeString(r)
    if err != nil {
        return nil, fmt.Errorf("decode mon_name: %w", err)
    }
    if len(monName) > SMMaxStrLen {
        return nil, fmt.Errorf("mon_name too long: %d > %d", len(monName), SMMaxStrLen)
    }
    mon.MonID.MonName = monName

    // Decode my_id.my_name (string)
    myName, err := xdr.DecodeString(r)
    if err != nil {
        return nil, fmt.Errorf("decode my_name: %w", err)
    }
    mon.MonID.MyID.MyName = myName

    // Decode my_id.my_prog (int32)
    myProg, err := xdr.DecodeInt32(r)
    if err != nil {
        return nil, fmt.Errorf("decode my_prog: %w", err)
    }
    mon.MonID.MyID.MyProg = uint32(myProg)

    // Decode my_id.my_vers (int32)
    myVers, err := xdr.DecodeInt32(r)
    if err != nil {
        return nil, fmt.Errorf("decode my_vers: %w", err)
    }
    mon.MonID.MyID.MyVers = uint32(myVers)

    // Decode my_id.my_proc (int32)
    myProc, err := xdr.DecodeInt32(r)
    if err != nil {
        return nil, fmt.Errorf("decode my_proc: %w", err)
    }
    mon.MonID.MyID.MyProc = uint32(myProc)

    // Decode priv (opaque[16] - fixed size)
    privBuf := make([]byte, 16)
    if _, err := io.ReadFull(r, privBuf); err != nil {
        return nil, fmt.Errorf("decode priv: %w", err)
    }
    copy(mon.Priv[:], privBuf)

    return mon, nil
}

func EncodeSMStatRes(buf *bytes.Buffer, res *SMStatRes) error {
    // Encode result (uint32 enum)
    if err := xdr.WriteUint32(buf, res.Result); err != nil {
        return fmt.Errorf("encode result: %w", err)
    }

    // Encode state (int32)
    if err := xdr.WriteInt32(buf, res.State); err != nil {
        return fmt.Errorf("encode state: %w", err)
    }

    return nil
}
```

### NSM Metrics

```go
// Source: Follow NLM metrics pattern from Phase 2

type Metrics struct {
    // RequestsTotal counts NSM requests by procedure and result
    RequestsTotal *prometheus.CounterVec

    // RequestDuration tracks latency distribution
    RequestDuration *prometheus.HistogramVec

    // ClientsRegistered tracks current number of monitored clients
    ClientsRegistered prometheus.Gauge

    // NotificationsTotal counts SM_NOTIFY callbacks by result
    NotificationsTotal *prometheus.CounterVec

    // NotificationDuration tracks notification callback latency
    NotificationDuration prometheus.Histogram

    // CrashesDetected counts client crashes detected
    CrashesDetected prometheus.Counter

    // LocksCleanedOnCrash counts locks released due to crash
    LocksCleanedOnCrash prometheus.Counter
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
    m := &Metrics{
        RequestsTotal: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "nsm_requests_total",
                Help: "Total NSM requests by procedure and result",
            },
            []string{"procedure", "result"},
        ),
        // ... other metrics
    }
    // Register all metrics
    return m
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| UDP only for NSM | TCP/UDP supported | Modern practice | Better reliability |
| Manual rpc.statd/sm-notify | Integrated in NFS server | Modern NFS servers | Simplified deployment |
| Separate NSM daemon | Embedded in NFS process | Modern practice | Single process, easier state |
| 45s grace period | Configurable (90s default) | Best practices | User-tunable recovery |

**Deprecated/outdated:**
- SM_SIMU_CRASH (procedure 5): Testing-only, not needed in production
- External rpc.statd daemon: Modern servers embed NSM functionality

## Open Questions

Things that couldn't be fully resolved:

1. **Callback Port Resolution**
   - What we know: my_id.my_prog is program number (e.g., 100021), not port.
   - What's unclear: Whether to use portmapper to resolve port or assume standard port.
   - Recommendation: Use standard NLM port (from NFS adapter config) for callbacks.

2. **Multiple Registrations for Same Client**
   - What we know: SM_MON can be called multiple times.
   - What's unclear: Should priv and callback info be updated or rejected?
   - Recommendation: Per CONTEXT.md, update existing registration (idempotent).

3. **SM_STAT vs SM_MON State Behavior**
   - What we know: Both return current server state.
   - What's unclear: Does SM_STAT register the caller for monitoring?
   - Recommendation: No, SM_STAT is query-only. Only SM_MON registers.

## Sources

### Primary (HIGH confidence)
- [Open Group NSM Protocol Specification](https://pubs.opengroup.org/onlinepubs/9629799/chap11.htm) - Complete protocol definition
- [Open Group SM_MON Procedure](https://pubs.opengroup.org/onlinepubs/9629799/SM_MON.htm) - Detailed SM_MON specification
- [Open Group SM_NOTIFY Procedure](https://pubs.opengroup.org/onlinepubs/9629799/SM_NOTIFY.htm) - Notification callback spec
- DittoFS codebase: `pkg/metadata/lock/connection.go`, `pkg/metadata/lock/grace.go`, `internal/protocol/nlm/`
- [Wireshark NSM Protocol](https://wiki.wireshark.org/Network_Status_Monitoring_Protocol) - Wire format reference

### Secondary (MEDIUM confidence)
- [Linux sm-notify(8) man page](https://www.man7.org/linux/man-pages/man8/sm-notify.8.html) - Implementation details
- [Linux rpc.statd(8) man page](https://man7.org/linux/man-pages/man8/statd.8.html) - NSM daemon behavior
- [NetApp NFS Lock Recovery KB](https://kb.netapp.com/Legacy/ONTAP/7Mode/What_are_the_details_of_Network_File_System_Lock_Recovery_and_Network_Status_Monitor) - Recovery workflow
- Phase 2 Research (`02-RESEARCH.md`) - NLM callback patterns

### Tertiary (LOW confidence)
- Various web search results on NSM/rpc.statd implementation details
- Linux kernel lockd source (for FREE_ALL usage verification)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Uses existing DittoFS patterns, extends Phase 1/2 infrastructure
- Architecture: HIGH - Clear specification, mirrors NLM structure
- XDR structures: HIGH - Official Open Group specification with fixed priv[16] array
- Callback mechanism: HIGH - Same pattern as NLM_GRANTED from Phase 2
- FREE_ALL integration: MEDIUM - NLM procedure exists but less documented
- Persistence: HIGH - Reuses Phase 1 LockStore patterns

**Research date:** 2026-02-05
**Valid until:** 90 days (stable protocol, no external dependency changes)
