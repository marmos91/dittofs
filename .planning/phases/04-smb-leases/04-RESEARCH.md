# Phase 4: SMB Leases - Research

**Researched:** 2026-02-05
**Domain:** SMB2/3 oplock and lease support integrated with unified lock manager
**Confidence:** HIGH

## Summary

This phase integrates SMB2.1+ leases (R/W/H caching) with the existing unified lock manager from Phase 1. The research examined: the MS-SMB2 protocol specification for lease semantics, the existing DittoFS codebase (current `OplockManager` in `internal/protocol/smb/v2/handlers/oplock.go` and unified lock manager in `pkg/metadata/lock/`), Samba's implementation patterns, and lease-to-lock mapping strategies.

**Key findings:**
- The existing `OplockManager` is path-based and standalone. It must be refactored to delegate to the unified lock manager using FileHandle-based keys, while preserving the existing break notification machinery (`OplockBreakNotifier` interface).
- Leases map cleanly to EnhancedLock: R = shared read lock (whole file), W = exclusive write lock (whole file), H = handle caching lock (new lock type). The `EnhancedLock` structure needs extension with lease-specific fields (LeaseKey, LeaseState R/W/H flags, Epoch for SMB3).
- Cross-protocol visibility is the primary value: NLM write lock triggers SMB W/R lease break; SMB exclusive lease blocks NLM requests. This flows naturally through the unified conflict detection already in `pkg/metadata/lock/types.go`.
- Lease break timeout: Windows default is 35 seconds (implementation-specific per MS-SMB2). The decision allows Claude's discretion; recommend 35s for Windows compatibility, configurable.

**Primary recommendation:** Extend `EnhancedLock` with lease fields (LeaseKey, LeaseState, Epoch), add new lock types for leases, refactor `OplockManager` to delegate to unified lock manager while keeping handler API stable, implement lease break notification using existing `OplockBreakNotifier` pattern.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | 1.22+ | Mutex/RWMutex for in-memory coordination | Thread-safe lease table access |
| Go stdlib `time` | 1.22+ | Lease break timers, timeout management | Timer management |
| Existing `pkg/metadata/lock` | - | Unified lock manager, EnhancedLock | Already implements conflict detection |
| Existing `internal/protocol/smb/v2/handlers` | - | OplockManager, break notification | Has working break machinery to preserve |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| Go stdlib `context` | 1.22+ | Cancellation for break acknowledgment timeouts | Timeout handling |
| `github.com/google/uuid` | 1.6+ | LeaseKey generation (server-side) | Already used in lock package |
| Existing `pkg/metadata/store` | - | LockStore interface | Lease persistence |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Embedded lease in EnhancedLock | Separate LeaseState struct | Separate adds complexity; embedded keeps unified model |
| Force break immediately | Retry with backoff | Windows expects acknowledgment wait; force on timeout is correct |
| Per-lease timers | Single background scanner | Scanner is simpler, matches MS-SMB2 spec behavior |

**No new dependencies required** - extend existing patterns.

## Architecture Patterns

### Recommended Project Structure

Lease support integrates into existing structure:

```
pkg/metadata/lock/
├── types.go           # EXTEND: Add LeaseState, LeaseFlags to EnhancedLock
├── store.go           # EXTEND: Add lease-specific queries
├── manager.go         # EXTEND: Add lease acquire/break methods
├── lease_types.go     # NEW: Lease constants, state machine helpers
└── lease_break.go     # NEW: Lease break timer management

internal/protocol/smb/v2/handlers/
├── oplock.go          # REFACTOR: Delegate to unified lock manager
├── lease.go           # NEW: SMB2_CREATE_REQUEST_LEASE handling
├── lease_context.go   # NEW: Lease create context encode/decode
└── lease_break.go     # NEW: Lease break notification handling
```

### Pattern 1: Lease-to-Lock Mapping

**What:** Map SMB2 lease states (R/W/H) to unified lock types.

**When to use:** All lease acquire/release operations.

**Example:**
```go
// Source: Derived from MS-SMB2 2.2.13.2.8 and CONTEXT.md decisions
// Lease flags (can be combined)
const (
    LeaseStateNone   uint32 = 0x00
    LeaseStateRead   uint32 = 0x01  // SMB2_LEASE_READ_CACHING
    LeaseStateWrite  uint32 = 0x02  // SMB2_LEASE_WRITE_CACHING
    LeaseStateHandle uint32 = 0x04  // SMB2_LEASE_HANDLE_CACHING
)

// LeaseInfo extends EnhancedLock for SMB leases
type LeaseInfo struct {
    // LeaseKey is the 128-bit client-generated key identifying the lease
    LeaseKey [16]byte

    // LeaseState is the current lease state (R/W/H flags)
    LeaseState uint32

    // BreakToState is the state we're breaking to (0 if not breaking)
    BreakToState uint32

    // Breaking indicates a break is in progress awaiting acknowledgment
    Breaking bool

    // Epoch is incremented on every state change (SMB3)
    Epoch uint16
}

// EnhancedLock extension (add to existing struct)
type EnhancedLock struct {
    // ... existing fields ...

    // Lease holds lease-specific state (nil for byte-range locks)
    Lease *LeaseInfo
}

// Mapping helpers
func (l *LeaseInfo) HasRead() bool   { return l.LeaseState&LeaseStateRead != 0 }
func (l *LeaseInfo) HasWrite() bool  { return l.LeaseState&LeaseStateWrite != 0 }
func (l *LeaseInfo) HasHandle() bool { return l.LeaseState&LeaseStateHandle != 0 }
```

### Pattern 2: OplockManager Delegation

**What:** Refactor `OplockManager` to delegate to unified lock manager while preserving API.

**When to use:** All oplock/lease operations from SMB handlers.

**Example:**
```go
// Source: Derived from existing oplock.go and CONTEXT.md decisions
type OplockManager struct {
    mu            sync.RWMutex
    lockManager   *lock.Manager       // Unified lock manager
    lockStore     lock.LockStore      // For persistence
    notify        OplockBreakNotifier // Existing break notification interface

    // Mapping: FileHandle -> LeaseKey for quick lookup
    handleToLease map[string][16]byte
}

// RequestLease acquires a lease through the unified lock manager
func (m *OplockManager) RequestLease(
    fileHandle lock.FileHandle,
    leaseKey [16]byte,
    sessionID uint64,
    clientID string,
    requestedState uint32,
) (grantedState uint32, err error) {
    m.mu.Lock()
    defer m.mu.Unlock()

    // Build owner ID for cross-protocol visibility
    ownerID := fmt.Sprintf("smb:lease:%x", leaseKey)

    // Check for existing lease with same key
    existing := m.findLeaseByKey(fileHandle, leaseKey)
    if existing != nil {
        // Same lease key - upgrade/maintain (no break to self)
        return m.upgradeLeaseState(existing, requestedState)
    }

    // Check for conflicting leases/locks (cross-protocol)
    if conflict := m.checkLeaseConflict(fileHandle, requestedState); conflict != nil {
        // Initiate break to conflicting lease holder
        m.initiateBreak(conflict, m.calculateBreakToState(requestedState))
        return LeaseStateNone, nil // Caller retries after break
    }

    // Grant new lease through unified lock manager
    leaseLock := lock.NewEnhancedLock(
        lock.LockOwner{
            OwnerID:   ownerID,
            ClientID:  clientID,
            ShareName: m.extractShare(fileHandle),
        },
        fileHandle,
        0,      // Whole file: offset=0
        0,      // Whole file: length=0 (unbounded)
        lock.LockTypeShared, // Base type; lease flags override
    )
    leaseLock.Lease = &lock.LeaseInfo{
        LeaseKey:   leaseKey,
        LeaseState: requestedState,
        Epoch:      1,
    }

    if err := m.lockManager.AddEnhancedLock(string(fileHandle), leaseLock); err != nil {
        return LeaseStateNone, err
    }

    m.handleToLease[string(fileHandle)] = leaseKey
    return requestedState, nil
}
```

### Pattern 3: Cross-Protocol Break Triggering

**What:** NFS operations check for SMB leases and trigger breaks.

**When to use:** NFS READ/WRITE operations that conflict with SMB caching.

**Example:**
```go
// Source: Design based on CONTEXT.md cross-protocol visibility decisions
// Called from NFS v3 WRITE handler before committing write
func (m *OplockManager) CheckAndBreakForNFSWrite(
    fileHandle lock.FileHandle,
    clientID string,
) error {
    m.mu.Lock()
    defer m.mu.Unlock()

    // Find any leases on this file
    leases := m.findLeases(fileHandle)

    for _, lease := range leases {
        // NFS write conflicts with Write lease (client has cached writes)
        if lease.Lease.HasWrite() {
            // Break W lease - client must flush cached data
            m.initiateBreak(lease, LeaseStateRead|LeaseStateHandle)
        }
        // NFS write also conflicts with Read lease (cached reads now stale)
        if lease.Lease.HasRead() {
            // Break R lease - no wait needed for read-only cache
            lease.Lease.LeaseState &^= LeaseStateRead
        }
    }

    return nil
}

// Called from NFS v3 READ handler (less restrictive)
func (m *OplockManager) CheckAndBreakForNFSRead(
    fileHandle lock.FileHandle,
    clientID string,
) error {
    m.mu.Lock()
    defer m.mu.Unlock()

    leases := m.findLeases(fileHandle)

    for _, lease := range leases {
        // NFS read only conflicts with Write lease
        // (reading while client may have uncommitted writes)
        if lease.Lease.HasWrite() {
            // Break W lease - client must flush
            m.initiateBreak(lease, LeaseStateRead|LeaseStateHandle)
        }
        // Read lease coexists with NFS reads - no break needed
    }

    return nil
}
```

### Pattern 4: Lease Break Timer Management

**What:** Background scanner for lease break timeouts, matching MS-SMB2 spec.

**When to use:** Server startup; runs continuously.

**Example:**
```go
// Source: Derived from MS-SMB2 3.3.6.5 Lease Break Acknowledgment Timer Event
const (
    LeaseBreakTimeout = 35 * time.Second // Windows default
    ScanInterval      = 1 * time.Second  // Check frequency
)

type LeaseBreakScanner struct {
    manager *OplockManager
    stop    chan struct{}
}

func (s *LeaseBreakScanner) Start() {
    ticker := time.NewTicker(ScanInterval)
    defer ticker.Stop()

    for {
        select {
        case <-s.stop:
            return
        case now := <-ticker.C:
            s.scanExpiredBreaks(now)
        }
    }
}

func (s *LeaseBreakScanner) scanExpiredBreaks(now time.Time) {
    s.manager.mu.Lock()
    defer s.manager.mu.Unlock()

    for _, lease := range s.manager.getAllBreakingLeases() {
        if !lease.Lease.Breaking {
            continue
        }

        breakDeadline := lease.AcquiredAt.Add(LeaseBreakTimeout)
        if now.After(breakDeadline) {
            // Timeout expired - force revoke per CONTEXT.md decision
            logger.Debug("Lease: break timeout expired, force revoking",
                "leaseKey", fmt.Sprintf("%x", lease.Lease.LeaseKey),
                "fileHandle", lease.FileHandle)

            // Set lease state to NONE and clear breaking flag
            lease.Lease.LeaseState = LeaseStateNone
            lease.Lease.Breaking = false

            // Allow conflicting operation to proceed
            s.manager.notifyBreakComplete(lease)
        }
    }
}
```

### Pattern 5: Lease Persistence Schema Extension

**What:** Extend PersistedLock for lease state.

**When to use:** All lease operations that need persistence.

**Example:**
```go
// Source: Derived from existing pkg/metadata/lock/store.go pattern
type PersistedLock struct {
    // ... existing fields ...

    // LeaseKey is non-nil for leases (128-bit key)
    LeaseKey []byte `json:"lease_key,omitempty"`

    // LeaseState is the R/W/H flags
    LeaseState uint32 `json:"lease_state,omitempty"`

    // LeaseEpoch is the SMB3 epoch counter
    LeaseEpoch uint16 `json:"lease_epoch,omitempty"`

    // BreakToState is set during active breaks
    BreakToState uint32 `json:"break_to_state,omitempty"`

    // Breaking indicates a break is in progress
    Breaking bool `json:"breaking,omitempty"`
}
```

### Anti-Patterns to Avoid

- **Path-based lease keys:** Current `OplockManager` uses paths; must convert to FileHandle-based for unified model
- **Ignoring lease during NFS operations:** NFS handlers must check for SMB leases and trigger breaks
- **Blocking on break acknowledgment indefinitely:** Must use timeout and force-revoke per decision
- **Separate lease tracking from lock manager:** Defeats cross-protocol visibility; integrate into unified model
- **Breaking read leases on NFS read:** Read leases can coexist with NFS reads (only Write needs break)

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Lease conflict detection | Custom logic | Extend `IsEnhancedLockConflicting` | Already handles cross-protocol |
| Break notification | New mechanism | Existing `OplockBreakNotifier` | Has working async machinery |
| Persistence | Custom format | Extend `PersistedLock` | Consistent with byte-range locks |
| Timer management | Per-lease goroutines | Single background scanner | Simpler, matches MS-SMB2 spec |
| FileHandle generation | New scheme | Existing store handles | Maintains routing consistency |

**Key insight:** The unified lock manager already has the conflict detection and persistence infrastructure. Leases are a new lock type, not a parallel system. The challenge is mapping lease semantics correctly, not building new infrastructure.

## Common Pitfalls

### Pitfall 1: Oplock/Lease Level Confusion

**What goes wrong:** Mixing up oplock levels (None/II/Exclusive/Batch) with lease states (R/W/H).
**Why it happens:** MS-SMB2 supports both; they map differently.
**How to avoid:** Use explicit mapping tables. Oplock levels are protocol wire format; lease states are caching permissions.
**Warning signs:** Granting wrong caching level, unexpected breaks.

| Oplock Level | Lease Equivalent | Caching |
|--------------|------------------|---------|
| None | 0 | No caching |
| Level II | R | Read only |
| Exclusive | RW | Read + Write |
| Batch | RWH | Read + Write + Handle |

### Pitfall 2: Ignoring Same-Lease-Key Semantics

**What goes wrong:** Breaking lease when same client opens file again with same LeaseKey.
**Why it happens:** Treating all opens as new lease requests.
**How to avoid:** Group opens by LeaseKey; same key = no conflict (this is the primary lease advantage over oplocks).
**Warning signs:** Performance regression vs traditional oplocks, excessive breaks for single client.

### Pitfall 3: Break Notification Race with Close

**What goes wrong:** Sending break notification to a closed session.
**Why it happens:** File closed between initiating break and sending notification.
**How to avoid:** Check session validity before sending; clean up leases on session close.
**Warning signs:** "Failed to send break notification" errors, leaked lease state.

### Pitfall 4: Cross-Protocol Break Ordering

**What goes wrong:** Allowing NFS operation before SMB client acknowledges break.
**Why it happens:** Not blocking NFS operation during break wait.
**How to avoid:** For Write/Handle breaks, NFS operation must wait (up to timeout) for acknowledgment.
**Warning signs:** Data corruption, stale reads.

### Pitfall 5: Handle Lease vs File Close Timing

**What goes wrong:** Releasing Handle lease immediately on CLOSE.
**Why it happens:** Not understanding Handle caching purpose.
**How to avoid:** Handle lease allows *delaying* close; don't revoke H until conflicting open or parent rename.
**Warning signs:** Client can't reopen file quickly, excessive round trips.

### Pitfall 6: Epoch Overflow Without Wrap Handling

**What goes wrong:** Epoch wraps to 0, client sees "old" state change.
**Why it happens:** 16-bit counter after 65535 changes.
**How to avoid:** Epoch comparison must handle wrap (check MS-SMB2 for comparison algorithm).
**Warning signs:** Client caches stale state after long-lived lease.

## Code Examples

Verified patterns from official sources:

### Lease State Mapping (from MS-SMB2)

```go
// Source: MS-SMB2 2.2.13.2.8 SMB2_CREATE_REQUEST_LEASE_V2
// Valid lease state combinations (files)
var validLeaseStates = []uint32{
    LeaseStateNone,                                    // No caching
    LeaseStateRead,                                    // R - read only
    LeaseStateRead | LeaseStateWrite,                  // RW - read + write
    LeaseStateRead | LeaseStateHandle,                 // RH - read + handle
    LeaseStateRead | LeaseStateWrite | LeaseStateHandle, // RWH - full
}

// Directories can only have R or RH
var validDirectoryLeaseStates = []uint32{
    LeaseStateNone,
    LeaseStateRead,
    LeaseStateRead | LeaseStateHandle,
}
```

### Lease Create Context Parsing

```go
// Source: MS-SMB2 2.2.13.2.8 SMB2_CREATE_REQUEST_LEASE_V2
type SMB2CreateRequestLeaseV2 struct {
    LeaseKey       [16]byte // Client-generated key
    LeaseState     uint32   // Requested state (R/W/H flags)
    Flags          uint32   // Reserved (set to 0)
    LeaseDuration  uint64   // Reserved (set to 0)
    ParentLeaseKey [16]byte // Parent directory lease key (SMB3)
    Epoch          uint16   // Incremented on state change
    Reserved       uint16
}

func ParseLeaseCreateContext(data []byte) (*SMB2CreateRequestLeaseV2, error) {
    if len(data) < 52 {
        return nil, fmt.Errorf("lease context too short: %d < 52", len(data))
    }

    ctx := &SMB2CreateRequestLeaseV2{
        LeaseState:    binary.LittleEndian.Uint32(data[16:20]),
        Flags:         binary.LittleEndian.Uint32(data[20:24]),
        LeaseDuration: binary.LittleEndian.Uint64(data[24:32]),
        Epoch:         binary.LittleEndian.Uint16(data[48:50]),
    }
    copy(ctx.LeaseKey[:], data[0:16])
    copy(ctx.ParentLeaseKey[:], data[32:48])

    return ctx, nil
}
```

### Lease Break Notification

```go
// Source: MS-SMB2 2.2.23.2 Lease Break Notification
type SMB2LeaseBreakNotification struct {
    StructureSize     uint16   // 44
    NewEpoch          uint16
    Flags             uint32   // SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED
    LeaseKey          [16]byte
    CurrentLeaseState uint32   // What client currently has
    NewLeaseState     uint32   // What client should break to
}

func (n *SMB2LeaseBreakNotification) Encode() []byte {
    buf := make([]byte, 44)
    binary.LittleEndian.PutUint16(buf[0:2], 44)
    binary.LittleEndian.PutUint16(buf[2:4], n.NewEpoch)
    binary.LittleEndian.PutUint32(buf[4:8], n.Flags)
    copy(buf[8:24], n.LeaseKey[:])
    binary.LittleEndian.PutUint32(buf[24:28], n.CurrentLeaseState)
    binary.LittleEndian.PutUint32(buf[28:32], n.NewLeaseState)
    // Bytes 32-44 are reserved (0)
    return buf
}
```

### Lease Break Acknowledgment

```go
// Source: MS-SMB2 2.2.24.2 Lease Break Acknowledgment
type SMB2LeaseBreakAcknowledgment struct {
    StructureSize uint16   // 36
    Reserved      uint16
    Flags         uint32   // Reserved (0)
    LeaseKey      [16]byte
    LeaseState    uint32   // State client is acknowledging
}

func ParseLeaseBreakAck(data []byte) (*SMB2LeaseBreakAcknowledgment, error) {
    if len(data) < 36 {
        return nil, fmt.Errorf("lease break ack too short: %d < 36", len(data))
    }

    structSize := binary.LittleEndian.Uint16(data[0:2])
    if structSize != 36 {
        return nil, fmt.Errorf("invalid structure size: %d", structSize)
    }

    ack := &SMB2LeaseBreakAcknowledgment{
        StructureSize: structSize,
        Reserved:      binary.LittleEndian.Uint16(data[2:4]),
        Flags:         binary.LittleEndian.Uint32(data[4:8]),
        LeaseState:    binary.LittleEndian.Uint32(data[24:28]),
    }
    copy(ack.LeaseKey[:], data[8:24])

    return ack, nil
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Oplock (FID-based) | Lease (key-based) | SMB 2.1 (2009) | Multiple handles share caching |
| Level II + Batch | R/W/H combinations | SMB 2.1 (2009) | Finer-grained caching control |
| Break loses all | Break to reduced state | SMB 2.1 (2009) | Preserve partial caching |
| No directory caching | Directory R/RH leases | SMB 3.0 (2012) | Directory enumeration caching |
| V1 lease context | V2 lease context | SMB 3.0 (2012) | Parent lease key, epoch tracking |

**Deprecated/outdated:**
- OplockLevelLease (0xFF): Marker to use lease context, not an actual level
- V1 lease context: Use V2 for SMB3+ connections

## Open Questions

Things that couldn't be fully resolved:

1. **Exact Windows lease break timeout value**
   - What we know: MS-SMB2 says "implementation-specific default value in milliseconds"
   - What's unclear: The product behavior notes reference (section 246) not publicly accessible
   - Recommendation: Use 35 seconds (widely cited default); make configurable

2. **Epoch comparison with wrap**
   - What we know: 16-bit counter that wraps
   - What's unclear: Exact comparison algorithm for wrap-around
   - Recommendation: Implement modular comparison: `(new - old) < 32768` means new is newer

3. **Grace period interaction with leases**
   - What we know: Byte-range locks have grace period for reclaim after restart
   - What's unclear: Whether leases should also have reclaim period
   - Recommendation: Yes, persist leases and allow reclaim during grace period (consistent with byte-range locks)

4. **Directory lease break on child rename**
   - What we know: Parent directory's RH lease breaks on child rename
   - What's unclear: Exact trigger (any rename? only cross-directory?)
   - Recommendation: Break on any child rename (conservative; matches MS-SMB2 wording)

## Sources

### Primary (HIGH confidence)
- [MS-SMB2 Protocol Specification](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5606ad47-5ee0-437a-817e-70c366052962) - Sections 2.2.13.2.8, 2.2.23.2, 2.2.24.2, 3.3.5.9, 3.3.6.5
- [MS-SMB2 Per Lease](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/212eb853-7e50-4608-877e-22d42e0664f3) - Lease data structure
- [MS-SMB2 Algorithm for Leasing](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/d8df943d-6ad7-4b30-9f58-96ae90fc6204) - Object store algorithm
- DittoFS codebase: `internal/protocol/smb/v2/handlers/oplock.go` (existing implementation)
- DittoFS codebase: `pkg/metadata/lock/` (unified lock manager from Phase 1)

### Secondary (MEDIUM confidence)
- [Oplock vs Lease](https://learn.microsoft.com/en-us/archive/blogs/openspecification/client-caching-features-oplock-vs-lease) - Microsoft OpenSpec blog
- [Samba SMB2 Wiki](https://wiki.samba.org/index.php/SMB3) - Lease state mapping
- [MS-SMB2 Lease Break Timer](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/a009e3f0-4d5b-48a3-b16e-0f75c8de0812) - Timer event processing

### Tertiary (LOW confidence)
- [Samba smb2_create.c](https://github.com/SpectraLogic/samba/blob/master/source3/smbd/smb2_create.c) - Reference implementation
- Various web searches for lease timeout defaults

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Uses existing DittoFS patterns, no new dependencies
- Architecture: HIGH - Clear integration points with existing code
- Lease semantics: HIGH - MS-SMB2 specification is authoritative
- Cross-protocol interaction: MEDIUM - Based on CONTEXT.md decisions, needs validation
- Timeout values: MEDIUM - Windows default not officially documented, 35s is best estimate

**Research date:** 2026-02-05
**Valid until:** 60 days (stable domain, MS-SMB2 spec does not change frequently)
