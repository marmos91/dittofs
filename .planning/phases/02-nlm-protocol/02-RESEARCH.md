# Phase 2: NLM Protocol - Research

**Researched:** 2026-02-05
**Domain:** Network Lock Manager protocol (RPC 100021) implementation for NFSv3 locking
**Confidence:** HIGH

## Summary

This phase implements the NLM (Network Lock Manager) protocol to enable NFSv3 clients to acquire and release byte-range locks via standard fcntl() calls. The research examined: the NLM v4 protocol specification (from Open Group and RFC 1813 Appendix II), the existing DittoFS locking infrastructure from Phase 1, the current RPC dispatcher architecture, XDR encoding patterns in the codebase, and blocking lock callback mechanisms.

**Key findings:**
- NLM v4 is the appropriate version for NFSv3 (uses 64-bit offsets). Program number is 100021.
- The existing lock manager from Phase 1 provides the core functionality (EnhancedLock, LockOwner, conflict detection). NLM handlers translate between NLM wire format and this unified lock model.
- Blocking locks require a callback mechanism: server returns NLM4_BLOCKED immediately, then sends NLM_GRANTED callback to client when lock becomes available.
- The RPC dispatcher in dispatch.go already handles program routing (NFS=100003, Mount=100005). Extending it to handle NLM (100021) follows the same pattern.
- XDR encoding/decoding utilities in internal/protocol/nfs/xdr/ can be refactored to a shared location for reuse by NLM.

**Primary recommendation:** Extend dispatch.go to route NLM requests to new internal/protocol/nlm/ handlers. Implement the 6 core synchronous procedures (NULL, TEST, LOCK, UNLOCK, CANCEL, GRANTED). Add MetadataService methods for NLM-specific lock operations. Implement blocking lock queue with per-file channels and callback client for NLM_GRANTED notifications.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `net` | 1.22+ | TCP connections for callbacks | Standard network I/O |
| Go stdlib `sync` | 1.22+ | Per-file blocking queues | Concurrent access control |
| Go stdlib `context` | 1.22+ | Timeout control for callbacks | 5s callback timeout |
| Existing `internal/protocol/nfs/xdr` | - | XDR encoding/decoding | Refactor to shared location |
| Existing `internal/protocol/nfs/rpc` | - | RPC message handling | Reuse for NLM RPC |
| Existing `pkg/metadata/lock` | - | Lock manager from Phase 1 | Unified lock backend |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/prometheus/client_golang` | 1.19+ | NLM metrics (nlm_* prefix) | Already in project |
| Go stdlib `encoding/binary` | 1.22+ | XDR binary encoding | Big-endian wire format |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Synchronous procedures only | Full async MSG/RES variants | Async adds complexity, clients handle sync fine |
| Fresh TCP for callbacks | Connection pooling | Simpler, callbacks are infrequent |
| Per-file channels for blocking queue | Global priority queue | Per-file matches LockManager design, simpler |

**No new dependencies required** - extend existing patterns.

## Architecture Patterns

### Recommended Project Structure

```
internal/protocol/
├── xdr/                          # NEW: Shared XDR utilities (extracted from nfs/xdr)
│   ├── decode.go                 # Basic XDR decoding
│   ├── encode.go                 # Basic XDR encoding
│   └── types.go                  # Common XDR types
├── nfs/
│   ├── dispatch.go               # MODIFY: Add NLM program routing
│   ├── xdr/                      # NFS-specific XDR (imports shared)
│   │   └── *.go
│   └── ...
└── nlm/                          # NEW: NLM protocol implementation
    ├── constants.go              # NLM procedure numbers, status codes
    ├── types.go                  # nlm4_lock, nlm4_holder, nlm4_res
    ├── xdr/                      # NLM-specific XDR encoding/decoding
    │   ├── decode.go             # Decode NLM requests
    │   └── encode.go             # Encode NLM responses
    ├── handlers/
    │   ├── context.go            # NLMHandlerContext
    │   ├── handler.go            # Handler struct with MetadataService ref
    │   ├── null.go               # NLMPROC4_NULL
    │   ├── test.go               # NLMPROC4_TEST
    │   ├── lock.go               # NLMPROC4_LOCK
    │   ├── unlock.go             # NLMPROC4_UNLOCK
    │   ├── cancel.go             # NLMPROC4_CANCEL
    │   └── granted.go            # NLMPROC4_GRANTED (for callback responses)
    ├── blocking/
    │   ├── queue.go              # Per-file blocking lock queue
    │   └── waiter.go             # Waiter entry with callback info
    └── callback/
        ├── client.go             # TCP callback client
        └── granted.go            # NLM_GRANTED callback logic

pkg/metadata/
├── service.go                    # ADD: NLM-specific lock methods
└── lock/
    └── manager.go                # Already has EnhancedLock support
```

### Pattern 1: NLM Dispatcher Integration

**What:** Extend existing RPC dispatcher to route NLM program (100021) to NLM handlers.
**When to use:** All NLM requests come through the same port as NFS (12049).

**Example:**
```go
// Source: Derived from existing dispatch.go pattern
// In handleRPCCall() switch statement:

case rpc.ProgramNLM: // 100021
    if call.Version != rpc.NLMVersion4 {
        return c.handleUnsupportedVersion(call, rpc.NLMVersion4, "NLM", clientAddr)
    }
    replyData, err = c.handleNLMProcedure(ctx, call, procedureData, clientAddr)
```

### Pattern 2: NLM Owner Identity Construction

**What:** Construct owner ID from NLM request fields matching CONTEXT.md decision.
**When to use:** All lock operations (TEST, LOCK, UNLOCK, CANCEL).

**Example:**
```go
// Source: CONTEXT.md decision - Owner format: nlm:{hostname}:{pid}:{oh}
func buildOwnerID(callerName string, svid int32, oh []byte) string {
    // Oh is opaque - hex encode for string representation
    ohHex := hex.EncodeToString(oh)
    return fmt.Sprintf("nlm:%s:%d:%s", callerName, svid, ohHex)
}

// For conflict response, parse back:
func parseOwnerID(ownerID string) (callerName string, svid int32, oh []byte, err error) {
    // Parse "nlm:{hostname}:{svid}:{oh_hex}"
    parts := strings.SplitN(ownerID, ":", 4)
    if len(parts) != 4 || parts[0] != "nlm" {
        return "", 0, nil, fmt.Errorf("invalid NLM owner ID format")
    }
    // ... parse parts
}
```

### Pattern 3: Blocking Lock Queue with Channels

**What:** Per-file channel for waiting lock requests with callback notification.
**When to use:** When NLMPROC4_LOCK has block=true and lock conflicts.

**Example:**
```go
// Source: Go-idiomatic channel pattern per CONTEXT.md
type BlockingQueue struct {
    mu       sync.RWMutex
    queues   map[string]chan *Waiter  // fileHandle -> waiter channel
    maxQueue int                       // Per-file limit (e.g., 100)
}

type Waiter struct {
    Lock           *lock.EnhancedLock
    CallbackAddr   string      // Client's callback address
    CallbackProg   uint32      // Client's callback program (NLM)
    CallbackVers   uint32      // Callback version
    ResponseCh     chan error  // Signal when processed
}

func (bq *BlockingQueue) Enqueue(fileHandle string, waiter *Waiter) error {
    bq.mu.Lock()
    q, exists := bq.queues[fileHandle]
    if !exists {
        q = make(chan *Waiter, bq.maxQueue)
        bq.queues[fileHandle] = q
    }
    bq.mu.Unlock()

    select {
    case q <- waiter:
        return nil
    default:
        return ErrQueueFull  // Return NLM4_DENIED_NOLOCKS
    }
}

// Called when lock is released
func (bq *BlockingQueue) ProcessRelease(fileHandle string, lm *lock.Manager) {
    bq.mu.RLock()
    q, exists := bq.queues[fileHandle]
    bq.mu.RUnlock()
    if !exists {
        return
    }

    // Try to grant locks to waiters
    for {
        select {
        case waiter := <-q:
            // Try to acquire lock
            err := lm.AddEnhancedLock(fileHandle, waiter.Lock)
            if err == nil {
                // Send NLM_GRANTED callback
                go sendGrantedCallback(waiter)
            } else {
                // Re-queue if still conflicts
                select {
                case q <- waiter:
                default:
                    // Queue full, notify failure
                    waiter.ResponseCh <- ErrQueueFull
                }
            }
        default:
            return
        }
    }
}
```

### Pattern 4: NLM_GRANTED Callback Client

**What:** Fresh TCP connection to client for lock grant notification.
**When to use:** When blocked lock becomes available.

**Example:**
```go
// Source: CONTEXT.md - Fresh TCP, 5s timeout
func sendGrantedCallback(waiter *Waiter) error {
    // Create TCP connection with 5s timeout
    conn, err := net.DialTimeout("tcp", waiter.CallbackAddr, 5*time.Second)
    if err != nil {
        // Callback failed - release lock immediately per CONTEXT.md
        releaseLock(waiter.Lock)
        return err
    }
    defer conn.Close()

    // Set read/write deadline
    conn.SetDeadline(time.Now().Add(5 * time.Second))

    // Build NLM_GRANTED RPC call
    grantedReq := &NLM4GrantedArgs{
        Cookie:    waiter.Cookie,
        Exclusive: waiter.Lock.Type == lock.LockTypeExclusive,
        Lock: NLM4Lock{
            CallerName: extractCallerName(waiter.Lock.Owner.OwnerID),
            FH:         waiter.Lock.FileHandle,
            OH:         extractOH(waiter.Lock.Owner.OwnerID),
            Svid:       extractSvid(waiter.Lock.Owner.OwnerID),
            Offset:     waiter.Lock.Offset,
            Length:     waiter.Lock.Length,
        },
    }

    // Send RPC call and read response
    reply, err := sendRPCCall(conn, waiter.CallbackProg, waiter.CallbackVers,
                              NLMPROC4_GRANTED, grantedReq)
    if err != nil {
        releaseLock(waiter.Lock)
        return err
    }

    // Per CONTEXT.md: Ignore response status (client idempotency issues)
    // Lock is already granted
    return nil
}
```

### Pattern 5: NLM Status Code Mapping

**What:** Map lock manager errors to NLM status codes.
**When to use:** All NLM response encoding.

**Example:**
```go
// Source: Open Group NLM v4 specification
const (
    NLM4_GRANTED          = 0  // Success
    NLM4_DENIED           = 1  // Lock denied (conflict)
    NLM4_DENIED_NOLOCKS   = 2  // No resources (queue full)
    NLM4_BLOCKED          = 3  // Blocking - will callback
    NLM4_DENIED_GRACE     = 4  // Grace period active
    NLM4_DEADLCK          = 5  // Would cause deadlock
    NLM4_ROFS             = 6  // Read-only filesystem
    NLM4_STALE_FH         = 7  // Stale file handle
    NLM4_FBIG             = 8  // Offset too big
    NLM4_FAILED           = 9  // General failure
)

func mapLockErrorToNLMStatus(err error) uint32 {
    switch {
    case err == nil:
        return NLM4_GRANTED
    case errors.IsLockConflict(err):
        return NLM4_DENIED
    case errors.IsDeadlock(err):
        return NLM4_DEADLCK
    case errors.IsNotFound(err):
        return NLM4_STALE_FH
    case errors.IsGracePeriod(err):
        return NLM4_DENIED_GRACE
    default:
        return NLM4_FAILED
    }
}
```

### Anti-Patterns to Avoid

- **Parsing OwnerID in lock manager:** Lock manager treats OwnerID as opaque. NLM handlers do the parsing.
- **Global blocking queue:** Use per-file queues matching LockManager's per-file lock storage.
- **Caching callback connections:** Fresh TCP per CONTEXT.md - callbacks are infrequent.
- **Waiting for callback response:** Callback status is best-effort; lock is already granted.
- **Supporting NLM v3:** Only v4 needed for NFSv3 (32-bit offsets obsolete).

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Lock conflict detection | Custom NLM conflict logic | `lock.IsEnhancedLockConflicting()` | Phase 1 already implements POSIX rules |
| Owner identity storage | NLM-specific owner format | `lock.LockOwner.OwnerID` string | Protocol-agnostic, tested |
| Lock persistence | NLM-specific storage | Phase 1 `LockStore` interface | Unified across protocols |
| RPC message framing | Custom framing | `internal/protocol/nfs/rpc` | Already handles fragment headers |
| XDR encoding | Custom NLM XDR | Extend `internal/protocol/nfs/xdr` | Proven patterns exist |
| Deadlock detection | NLM-specific detection | `lock.WaitForGraph` | Phase 1 implementation |

**Key insight:** Phase 1 built the lock management foundation. Phase 2 builds the NLM protocol adapter on top of it, translating wire format to unified lock model.

## Common Pitfalls

### Pitfall 1: Callback Address Extraction

**What goes wrong:** Client callback address isn't the same as request source address.
**Why it happens:** NLM client provides callback program info in lock request, not implied from connection.
**How to avoid:** Extract callback address from NLM lock request arguments (client provides it).
**Warning signs:** Callbacks fail with connection refused; logs show wrong target address.

### Pitfall 2: Stale Blocking Waiters After CANCEL

**What goes wrong:** Cancelled lock request still receives NLM_GRANTED callback.
**Why it happens:** Waiter removed from queue but callback already in progress.
**How to avoid:** Track cancelled waiters; callback client checks before sending.
**Warning signs:** Client receives grant for lock it cancelled; client logs errors.

### Pitfall 3: Queue Full During Lock Release Storm

**What goes wrong:** When many blocked requests exist and lock releases, re-queueing conflicts fails.
**Why it happens:** ProcessRelease tries to re-queue failed attempts into full queue.
**How to avoid:** Failed re-queue should notify waiter of failure, not block.
**Warning signs:** Goroutines pile up; clients timeout waiting for blocked locks.

### Pitfall 4: Owner Handle (OH) Comparison Issues

**What goes wrong:** Same client's locks don't merge correctly due to OH encoding.
**Why it happens:** OH is opaque bytes; different encodings for same logical owner.
**How to avoid:** Consistent hex encoding in ownerID; compare ownerIDs as strings.
**Warning signs:** Client gets conflict on its own lock; can't unlock own lock.

### Pitfall 5: Unlock of Non-Existent Lock Fails

**What goes wrong:** Client retries unlock; second attempt returns error.
**Why it happens:** First unlock succeeded; second finds no lock.
**How to avoid:** Per CONTEXT.md, unlock of non-existent lock silently succeeds (NLM4_GRANTED).
**Warning signs:** Client reports unlock errors; client hangs waiting for unlock confirmation.

### Pitfall 6: Grace Period Blocks NLM_TEST

**What goes wrong:** NLM_TEST fails during grace period.
**Why it happens:** Grace period blocks all operations.
**How to avoid:** Per Phase 1, NLM_TEST should be allowed during grace (read-only operation).
**Warning signs:** Clients can't even test locks during recovery; prolonged unavailability.

## Code Examples

Verified patterns from official sources:

### NLM v4 XDR Structures

```go
// Source: Open Group NLM v4 specification
// https://pubs.opengroup.org/onlinepubs/9629799/chap14.htm

// nlm4_holder describes lock ownership in conflict responses
type NLM4Holder struct {
    Exclusive bool    // Lock type
    Svid      int32   // Process identifier
    OH        []byte  // Opaque host identifier (netobj)
    Offset    uint64  // Starting position (64-bit in v4)
    Length    uint64  // Region length (64-bit in v4)
}

// nlm4_lock describes a lock request
type NLM4Lock struct {
    CallerName string   // Client hostname (max 1024 chars)
    FH         []byte   // NFS v3 file handle (netobj)
    OH         []byte   // Opaque host identifier (netobj)
    Svid       int32    // Process identifier
    Offset     uint64   // Starting position
    Length     uint64   // Zero = to end of file
}

// nlm4_lockargs - LOCK procedure arguments
type NLM4LockArgs struct {
    Cookie     []byte    // Opaque cookie (netobj)
    Block      bool      // Block if can't grant immediately
    Exclusive  bool      // Exclusive (write) vs shared (read)
    Lock       NLM4Lock  // Lock specification
    Reclaim    bool      // Reclaiming lock after server reboot
    State      int32     // NSM state (for crash recovery)
}

// nlm4_res - common result structure
type NLM4Res struct {
    Cookie []byte      // Echoed from request
    Status uint32      // NLM4_* status code
}

// nlm4_testres - TEST procedure result
type NLM4TestRes struct {
    Cookie []byte       // Echoed from request
    Status uint32       // NLM4_* status code
    Holder *NLM4Holder  // If denied, the conflicting lock holder
}
```

### NLM Procedure Dispatch Table

```go
// Source: Open Group NLM v4 specification

const (
    NLMProcNull    = 0   // NULL - ping
    NLMProcTest    = 1   // TEST - check lock availability
    NLMProcLock    = 2   // LOCK - acquire lock
    NLMProcCancel  = 3   // CANCEL - cancel pending lock
    NLMProcUnlock  = 4   // UNLOCK - release lock
    NLMProcGranted = 5   // GRANTED - callback notification

    // Async message variants (6-15) - not implemented per CONTEXT.md

    // DOS sharing (20-22) - not implemented
)

var NLMDispatchTable = map[uint32]*nlmProcedure{
    NLMProcNull: {
        Name:    "NULL",
        Handler: handleNLMNull,
    },
    NLMProcTest: {
        Name:    "TEST",
        Handler: handleNLMTest,
    },
    NLMProcLock: {
        Name:    "LOCK",
        Handler: handleNLMLock,
    },
    NLMProcCancel: {
        Name:    "CANCEL",
        Handler: handleNLMCancel,
    },
    NLMProcUnlock: {
        Name:    "UNLOCK",
        Handler: handleNLMUnlock,
    },
    NLMProcGranted: {
        Name:    "GRANTED",
        Handler: handleNLMGranted,  // For responses to our callbacks
    },
}
```

### XDR Decoding for NLM Lock

```go
// Source: Derived from existing internal/protocol/nfs/xdr patterns

func DecodeNLM4LockArgs(r io.Reader) (*NLM4LockArgs, error) {
    args := &NLM4LockArgs{}

    // Cookie (netobj - variable length opaque)
    cookie, err := DecodeOpaque(r)
    if err != nil {
        return nil, fmt.Errorf("decode cookie: %w", err)
    }
    args.Cookie = cookie

    // Block (bool as uint32)
    var block uint32
    if err := binary.Read(r, binary.BigEndian, &block); err != nil {
        return nil, fmt.Errorf("decode block: %w", err)
    }
    args.Block = block != 0

    // Exclusive (bool as uint32)
    var exclusive uint32
    if err := binary.Read(r, binary.BigEndian, &exclusive); err != nil {
        return nil, fmt.Errorf("decode exclusive: %w", err)
    }
    args.Exclusive = exclusive != 0

    // Lock structure
    lock, err := DecodeNLM4Lock(r)
    if err != nil {
        return nil, fmt.Errorf("decode lock: %w", err)
    }
    args.Lock = *lock

    // Reclaim (bool as uint32)
    var reclaim uint32
    if err := binary.Read(r, binary.BigEndian, &reclaim); err != nil {
        return nil, fmt.Errorf("decode reclaim: %w", err)
    }
    args.Reclaim = reclaim != 0

    // State (int32)
    if err := binary.Read(r, binary.BigEndian, &args.State); err != nil {
        return nil, fmt.Errorf("decode state: %w", err)
    }

    return args, nil
}

func DecodeNLM4Lock(r io.Reader) (*NLM4Lock, error) {
    lock := &NLM4Lock{}

    // CallerName (XDR string)
    callerName, err := DecodeString(r)
    if err != nil {
        return nil, fmt.Errorf("decode caller_name: %w", err)
    }
    lock.CallerName = callerName

    // FH (netobj - file handle)
    fh, err := DecodeOpaque(r)
    if err != nil {
        return nil, fmt.Errorf("decode fh: %w", err)
    }
    lock.FH = fh

    // OH (netobj - owner handle)
    oh, err := DecodeOpaque(r)
    if err != nil {
        return nil, fmt.Errorf("decode oh: %w", err)
    }
    lock.OH = oh

    // Svid (int32)
    if err := binary.Read(r, binary.BigEndian, &lock.Svid); err != nil {
        return nil, fmt.Errorf("decode svid: %w", err)
    }

    // Offset (uint64 in NLM v4)
    if err := binary.Read(r, binary.BigEndian, &lock.Offset); err != nil {
        return nil, fmt.Errorf("decode offset: %w", err)
    }

    // Length (uint64 in NLM v4)
    if err := binary.Read(r, binary.BigEndian, &lock.Length); err != nil {
        return nil, fmt.Errorf("decode length: %w", err)
    }

    return lock, nil
}
```

### MetadataService NLM Methods

```go
// Source: Extend existing pkg/metadata/service.go pattern

// LockFileNLM acquires a lock for NLM protocol.
// Unlike LockFile, this:
// - Takes ownerID string directly (NLM handler constructs it)
// - Returns detailed conflict info for NLM_DENIED responses
// - Supports blocking semantics (returns should-wait indication)
func (s *MetadataService) LockFileNLM(
    ctx context.Context,
    handle FileHandle,
    owner lock.LockOwner,
    offset, length uint64,
    exclusive bool,
    blocking bool,
    reclaim bool,
) (*lock.LockResult, error) {
    // ... implementation using lock.Manager
}

// TestLockNLM tests lock without acquiring.
// Returns detailed holder info for conflict responses.
func (s *MetadataService) TestLockNLM(
    ctx context.Context,
    handle FileHandle,
    owner lock.LockOwner,
    offset, length uint64,
    exclusive bool,
) (bool, *lock.EnhancedLockConflict, error) {
    // ... implementation
}

// UnlockFileNLM releases a lock for NLM protocol.
// Silently succeeds if lock doesn't exist (idempotency).
func (s *MetadataService) UnlockFileNLM(
    ctx context.Context,
    handle FileHandle,
    ownerID string,
    offset, length uint64,
) error {
    // ... implementation
}

// CancelBlockingLock cancels a pending blocking lock request.
func (s *MetadataService) CancelBlockingLock(
    ctx context.Context,
    handle FileHandle,
    ownerID string,
    offset, length uint64,
) error {
    // ... implementation
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| NLM v1-3 (32-bit offsets) | NLM v4 (64-bit offsets) | NFSv3 (1995) | Large file support |
| Separate NLM port | Same port as NFS | Common practice | Simpler firewall config |
| Always async MSG/RES | Sync procedures sufficient | Modern practice | Simpler implementation |
| IP-based owner identity | Session-based (oh+svid) | NFSv4 era | Better multi-homed support |

**Deprecated/outdated:**
- NLM v1-3: Only for NFSv2, 32-bit offset limitation
- Async MSG/RES procedures: Complexity without benefit for modern clients
- DOS sharing procedures (SHARE/UNSHARE): SMB-specific, not used by NFS clients

## Open Questions

Things that couldn't be fully resolved:

1. **Client Callback Program Discovery**
   - What we know: Client provides callback program info in lock request.
   - What's unclear: Exact field names vary across implementations (some use client_addr, some derive from connection).
   - Recommendation: Parse from NLM request; fall back to connection source if not present.

2. **Blocking Lock Timeout Behavior**
   - What we know: Clients wait for callback. CONTEXT.md says wait indefinitely.
   - What's unclear: Should server have internal timeout to clean up abandoned waiters?
   - Recommendation: No server-side timeout per CONTEXT.md. Client-side CANCEL handles abandonment.

3. **Multiple Blocked Waiters for Same Range**
   - What we know: Queue is FIFO per CONTEXT.md.
   - What's unclear: If first waiter's callback fails, should we try the next?
   - Recommendation: Yes, try next waiter. Failed callback = lock released, next waiter gets chance.

## Sources

### Primary (HIGH confidence)
- [Open Group NLM v4 Specification](https://pubs.opengroup.org/onlinepubs/9629799/chap14.htm) - Complete procedure and XDR definitions
- [RFC 1813 Appendix II](https://www.freesoft.org/CIE/RFC/1813/70.htm) - NLM relationship to NFS
- DittoFS codebase: `internal/protocol/nfs/dispatch.go`, `pkg/metadata/lock/`, `internal/protocol/nfs/xdr/`
- [Wireshark NLM Protocol](https://wiki.wireshark.org/Network_Lock_Manager) - Wire format reference

### Secondary (MEDIUM confidence)
- [Open Group File Locking over XNFS](https://pubs.opengroup.org/onlinepubs/9629799/chap9.htm) - Lock semantics
- [Linux NFS Wiki - Byte Range Locking](http://wiki.linux-nfs.org/wiki/index.php/Cluster_Coherent_NFS_and_Byte_Range_Locking) - Implementation patterns
- Phase 1 Research (`01-RESEARCH.md`) - Lock manager architecture

### Tertiary (LOW confidence)
- Various web search results on NLM callback implementation
- Linux kernel NFS client source (for callback behavior verification)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Uses existing DittoFS patterns, extends proven code
- Architecture: HIGH - Clear specification, matches existing dispatch pattern
- XDR structures: HIGH - Official Open Group specification
- Callback mechanism: MEDIUM - Documented behavior but implementation-specific details vary
- Error mapping: HIGH - Status codes from specification

**Research date:** 2026-02-05
**Valid until:** 90 days (stable protocol, no external dependency changes)
