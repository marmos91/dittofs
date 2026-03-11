# Phase 5: Cross-Protocol Integration - Research

**Researched:** 2026-02-05
**Domain:** Cross-protocol lock/lease visibility, conflict detection, unified grace periods, E2E testing
**Confidence:** HIGH

## Summary

This phase integrates NFS (NLM) locking with SMB leases to enable cross-protocol conflict detection and resolution. The research examined: the existing DittoFS codebase architecture (Phase 1-4 implementations including LockManager, OplockManager, LeaseBreakScanner, GracePeriodManager), MS-SMB2 protocol specifications for lease break semantics, NLM protocol specifications, and the existing E2E test infrastructure.

**Key findings:**
- The foundational architecture is already in place. Phase 1 created the unified lock manager (`pkg/metadata/lock/`), Phase 2-3 implemented NLM protocol handlers, and Phase 4 implemented SMB leases with cross-protocol visibility hooks (`OplockChecker` interface). This phase connects these pieces with actual conflict resolution logic.
- The CONTEXT.md decisions are comprehensive and prescriptive. The key patterns are: NFS byte-range locks win over SMB opportunistic leases (deny immediately), SMB Write leases trigger break-then-grant for NFS locks, and Handle leases must break before NFS REMOVE/RENAME operations.
- A new `UnifiedLockView` struct will provide a single query API for both protocols, allowing NLM handlers to translate SMB lease conflicts into holder info for NLM4_DENIED responses (and vice versa).
- Grace period coordination is straightforward: a single 90-second shared grace period for both protocols using the existing `GracePeriodManager`, with reclaim verification against persisted lock state.
- E2E testing uses real kernel mounts (NFS via `mount -t nfs`, SMB via `mount.cifs` or `mount_smbfs`). The existing cross-protocol test infrastructure (`test/e2e/cross_protocol_test.go`) provides patterns for setting up dual-mount scenarios.

**Primary recommendation:** Create `UnifiedLockView` in `pkg/metadata/lock/` to provide the unified query API, extend existing NLM handlers to check for SMB leases via `OplockChecker` interface with blocking wait for Write lease breaks, implement SMB conflict checking against NLM locks using the existing lock manager, and create comprehensive E2E tests covering all cross-protocol permutations.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | 1.22+ | Mutex/RWMutex for unified view coordination | Thread-safe access |
| Go stdlib `time` | 1.22+ | Timeout management for lease break waits | Timer handling |
| Go stdlib `context` | 1.22+ | Cancellation for blocking operations | Timeout support |
| Existing `pkg/metadata/lock` | - | LockManager, EnhancedLock, GracePeriodManager | From Phase 1 |
| Existing `internal/protocol/smb/v2/handlers` | - | OplockManager, CheckAndBreakForWrite/Read | From Phase 4 |
| Existing `internal/protocol/nlm/handlers` | - | NLM_LOCK, NLM_UNLOCK handlers | From Phase 2-3 |
| Existing `test/e2e/framework` | - | Mount helpers, cross-protocol testing | E2E infrastructure |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/stretchr/testify` | 1.9+ | Test assertions | All E2E tests |
| `github.com/prometheus/client_golang` | 1.19+ | Metrics for cross-protocol events | Observability |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| UnifiedLockView struct | Inline queries in handlers | Struct provides cleaner API, easier testing |
| Polling for lease break wait | Condition variables | Polling simpler to implement, 100ms interval acceptable per Phase 4 decision |
| Sequential E2E tests | Parallel with coordination | Sequential cleaner for cross-protocol state verification |

**No new dependencies required** - extend existing patterns.

## Architecture Patterns

### Recommended Project Structure

Cross-protocol integration extends existing structure:

```
pkg/metadata/lock/
├── types.go           # EXISTING: EnhancedLock with Lease field
├── store.go           # EXISTING: LockStore interface
├── manager.go         # EXISTING: LockManager
├── lease_types.go     # EXISTING: LeaseInfo, state constants
├── lease_break.go     # EXISTING: LeaseBreakScanner
├── grace.go           # EXISTING: GracePeriodManager
├── unified_view.go    # NEW: UnifiedLockView for cross-protocol queries
└── cross_protocol.go  # NEW: Conflict translation helpers

pkg/metadata/
├── service.go         # EXTEND: Add UnifiedLockView methods
└── oplock_checker.go  # EXISTING: OplockChecker interface

internal/protocol/nlm/handlers/
├── lock.go            # EXTEND: Check SMB leases before granting
├── unlock.go          # EXISTING
└── cross_protocol.go  # NEW: SMB conflict translation for NLM responses

internal/protocol/smb/v2/handlers/
├── oplock.go          # EXTEND: Check NLM locks before granting leases
├── lease.go           # EXISTING
└── cross_protocol.go  # NEW: NLM lock checking for SMB operations

test/e2e/
├── cross_protocol_test.go     # EXISTING: Basic interop tests
├── cross_protocol_lock_test.go # NEW: Lock/lease conflict tests
└── grace_period_test.go       # NEW: Cross-protocol grace period tests
```

### Pattern 1: UnifiedLockView Struct

**What:** Single query API that returns both NLM locks and SMB leases for a file.

**When to use:** Any handler needing to check cross-protocol conflicts.

**Example:**
```go
// Source: Based on CONTEXT.md decisions and existing lock/types.go
package lock

// UnifiedLockView provides a unified view of all locks and leases on a file.
// Each protocol handler can query this view and translate results to wire format.
type UnifiedLockView struct {
    lockStore LockStore
    mu        sync.RWMutex
}

// FileLocksInfo contains all lock/lease information for a file.
type FileLocksInfo struct {
    // ByteRangeLocks contains NLM/SMB byte-range locks
    ByteRangeLocks []*EnhancedLock

    // Leases contains SMB2/3 leases (whole-file)
    Leases []*EnhancedLock
}

// GetAllLocksOnFile returns all locks and leases for a file.
// Protocols translate this to their wire format.
func (v *UnifiedLockView) GetAllLocksOnFile(ctx context.Context, fileHandle FileHandle) (*FileLocksInfo, error) {
    v.mu.RLock()
    defer v.mu.RUnlock()

    // Query all locks for this file
    locks, err := v.lockStore.ListLocks(ctx, LockQuery{
        FileID: string(fileHandle),
    })
    if err != nil {
        return nil, err
    }

    info := &FileLocksInfo{
        ByteRangeLocks: make([]*EnhancedLock, 0),
        Leases:         make([]*EnhancedLock, 0),
    }

    for _, pl := range locks {
        el := FromPersistedLock(pl)
        if el.IsLease() {
            info.Leases = append(info.Leases, el)
        } else {
            info.ByteRangeLocks = append(info.ByteRangeLocks, el)
        }
    }

    return info, nil
}

// TranslateToNLMHolder converts an SMB lease to NLM holder info.
// Used when denying NLM lock due to SMB conflict.
// Per CONTEXT.md: owner="smb:<client>", offset=0, length=MAX
func TranslateToNLMHolder(lease *EnhancedLock) NLMHolderInfo {
    return NLMHolderInfo{
        CallerName: fmt.Sprintf("smb:%s", lease.Owner.ClientID),
        Svid:       0,
        OH:         []byte(lease.Lease.LeaseKey[:]),
        Offset:     0,
        Length:     ^uint64(0), // MAX (whole file)
        Exclusive:  lease.Lease.HasWrite(),
    }
}
```

### Pattern 2: NFS Lock vs SMB Write Lease - Trigger Break and Wait

**What:** When NFS requests a lock and SMB Write lease exists, trigger break and wait for acknowledgment.

**When to use:** NLM_LOCK handler when SMB lease exists on target file.

**Example:**
```go
// Source: Based on CONTEXT.md decision: "SMB Write lease vs NFS lock: Trigger SMB lease break"
// In internal/protocol/nlm/handlers/lock.go

func (h *Handler) Lock(ctx *NLMHandlerContext, req *LockRequest) (*LockResponse, error) {
    // ... existing validation ...

    // Check for SMB leases that need to break
    checker := metadata.GetOplockChecker()
    if checker != nil {
        // CheckAndBreakForWrite triggers break for Write leases
        // Returns ErrLeaseBreakPending if break in progress
        for {
            err := checker.CheckAndBreakForWrite(ctx.Context, lock.FileHandle(handle))
            if err == nil {
                break // No conflicting lease or break completed
            }
            if err != handlers.ErrLeaseBreakPending {
                // Unexpected error
                logger.Warn("NLM LOCK: failed to check SMB leases", "error", err)
                break // Proceed anyway, SMB client will see conflict
            }
            // Lease break in progress - wait with timeout
            select {
            case <-ctx.Context.Done():
                return &LockResponse{Cookie: req.Cookie, Status: types.NLM4Failed}, nil
            case <-time.After(100 * time.Millisecond):
                // Poll again - per Phase 4 decision (100ms interval)
            }
        }
    }

    // Proceed with lock acquisition via MetadataService
    result, err := h.metadataService.LockFileNLM(...)
    // ... rest of handler ...
}
```

### Pattern 3: SMB Lease vs NFS Lock - Deny Immediately

**What:** When SMB requests a lease and NFS holds a conflicting byte-range lock, deny immediately.

**When to use:** SMB CREATE handler with lease request when NLM lock exists.

**Example:**
```go
// Source: Based on CONTEXT.md decision: "NFS lock vs SMB Write lease: Deny SMB immediately"
// In internal/protocol/smb/v2/handlers/oplock.go

func (m *OplockManager) RequestLease(...) (grantedState uint32, epoch uint16, err error) {
    // ... existing logic ...

    // Check for NLM byte-range locks that would conflict
    locks, err := m.lockStore.ListLocks(ctx, lock.LockQuery{
        FileID:  string(fileHandle),
        IsLease: boolPtr(false), // Byte-range locks only
    })
    if err == nil && len(locks) > 0 {
        for _, pl := range locks {
            el := lock.FromPersistedLock(pl)
            // Any exclusive NLM lock conflicts with Write lease
            if requestedHasWrite && el.IsExclusive() {
                logger.Info("Lease: denied due to NLM lock",
                    "leaseKey", fmt.Sprintf("%x", leaseKey),
                    "nlmOwner", el.Owner.OwnerID)
                // Return None - STATUS_LOCK_NOT_GRANTED will be set by caller
                return lock.LeaseStateNone, 0, nil
            }
            // Shared NLM lock conflicts with exclusive Write lease request
            if requestedHasWrite && !el.IsExclusive() {
                // Same as above - Write lease requires exclusive access
                return lock.LeaseStateNone, 0, nil
            }
        }
    }

    // ... rest of lease acquisition ...
}
```

### Pattern 4: Handle Lease Break for NFS REMOVE/RENAME

**What:** Break SMB Handle lease before NFS REMOVE/RENAME can proceed.

**When to use:** NFS REMOVE/RENAME handlers when file has SMB Handle lease.

**Example:**
```go
// Source: Based on CONTEXT.md decision: "SMB Handle (H) lease vs NFS REMOVE/RENAME: Break H lease"
// In internal/protocol/nfs/v3/handlers/remove.go

func (h *Handler) Remove(ctx *NFSHandlerContext, req *RemoveRequest) (*RemoveResponse, error) {
    // ... validate and lookup file ...

    // Check for Handle leases that need to break
    checker := metadata.GetOplockChecker()
    if checker != nil {
        // Need a new method for Handle lease breaks specifically
        err := checker.CheckAndBreakForDelete(ctx.Context, fileHandle)
        if err == handlers.ErrLeaseBreakPending {
            // Wait for Handle lease acknowledgment (client closes handles)
            deadline := time.Now().Add(35 * time.Second) // Per Phase 4 decision
            for time.Now().Before(deadline) {
                select {
                case <-ctx.Context.Done():
                    return nil, nfs.ErrNFS3ErrJukebox // Tell client to retry
                case <-time.After(100 * time.Millisecond):
                    err = checker.CheckAndBreakForDelete(ctx.Context, fileHandle)
                    if err == nil {
                        goto proceed // Break completed
                    }
                    if err != handlers.ErrLeaseBreakPending {
                        goto proceed // Unexpected error, proceed anyway
                    }
                }
            }
            // Timeout - lease force-revoked by scanner, proceed
        }
    }

proceed:
    // ... perform actual delete ...
}
```

### Pattern 5: NLM4_DENIED with Cross-Protocol Holder Info

**What:** When denying NLM lock due to SMB conflict, return holder info with "smb:" prefix.

**When to use:** NLM_LOCK returning NLM4_DENIED.

**Example:**
```go
// Source: Based on CONTEXT.md decision: "NFS denial due to SMB: owner='smb:<client>'"
// In internal/protocol/nlm/handlers/lock.go

func buildDeniedResponseFromSMBLease(cookie []byte, lease *lock.EnhancedLock) *LockResponse {
    // Translate SMB lease to NLM holder format
    holder := &types.NLM4Holder{
        Exclusive:  lease.Lease.HasWrite(),
        Svid:       0, // SMB doesn't have svid
        OH:         []byte(fmt.Sprintf("smb:%s", lease.Owner.ClientID)),
        Offset:     0,
        Length:     ^uint64(0), // Whole file (leases are whole-file)
    }

    return &LockResponse{
        Cookie: cookie,
        Status: types.NLM4Denied,
        Holder: holder,
    }
}
```

### Pattern 6: Shared Grace Period for Both Protocols

**What:** Single 90-second grace period serves both NFS and SMB reclaim.

**When to use:** Server startup when persisted locks exist.

**Example:**
```go
// Source: Based on CONTEXT.md decision: "Single shared grace period for both NFS and SMB"
// In pkg/metadata/service.go or runtime initialization

func (s *MetadataService) InitializeGracePeriod(lockStore lock.LockStore) error {
    // Load all persisted locks to identify expected clients
    locks, err := lockStore.ListLocks(ctx, lock.LockQuery{})
    if err != nil {
        return err
    }

    if len(locks) == 0 {
        return nil // No locks to reclaim, no grace period needed
    }

    // Extract unique client IDs (both NLM and SMB clients)
    clientSet := make(map[string]bool)
    for _, pl := range locks {
        clientSet[pl.ClientID] = true
    }

    clients := make([]string, 0, len(clientSet))
    for clientID := range clientSet {
        clients = append(clients, clientID)
    }

    // Enter 90-second shared grace period
    s.gracePeriodManager.EnterGracePeriod(clients)

    return nil
}
```

### Anti-Patterns to Avoid

- **Separate grace periods for each protocol:** Creates window for race conditions; use single shared period
- **Ignoring Handle lease on REMOVE/RENAME:** H lease exists to prevent surprise deletion; must break first
- **Blocking NFS indefinitely on lease break:** Use configurable timeout (5s for tests, 35s default)
- **Not including protocol prefix in holder info:** Cross-protocol debugging requires clear identification
- **Testing with only one mount at a time:** Cross-protocol tests need simultaneous NFS and SMB mounts

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Lease break notification | New mechanism | Existing OplockManager.initiateBreak() | Has working async machinery from Phase 4 |
| Break timeout scanning | Custom timer per break | Existing LeaseBreakScanner | Background scanner already implemented |
| Grace period state machine | New implementation | Existing GracePeriodManager | Full state machine from Phase 1 |
| Cross-protocol mount setup | Custom mount code | framework.MountNFS(), framework.MountSMB() | E2E framework handles platform differences |
| Lock persistence | New format | Existing PersistedLock with lease fields | Already supports both lock types |

**Key insight:** The infrastructure exists. Phase 5 connects the pieces with cross-protocol conflict detection logic, not new infrastructure.

## Common Pitfalls

### Pitfall 1: Not Waiting for Write Lease Break Before NFS Write

**What goes wrong:** NFS write proceeds while SMB client has uncommitted cached writes, causing data corruption.
**Why it happens:** Forgetting to poll for break acknowledgment, or not using proper timeout.
**How to avoid:** Always wait (with timeout) for Write lease break acknowledgment before proceeding.
**Warning signs:** Data inconsistency between NFS and SMB views of same file.

### Pitfall 2: E2E Tests Without Proper Mount Options

**What goes wrong:** Tests pass spuriously because client caching masks synchronization issues.
**Why it happens:** Default mount options enable aggressive caching.
**How to avoid:** Use `actimeo=0` for NFS, fresh mounts per test scenario.
**Warning signs:** Tests pass locally but fail in CI, or vice versa.

### Pitfall 3: Grace Period Doesn't Cover Both Protocols

**What goes wrong:** One protocol reclaims during grace while other is blocked, leading to lock theft.
**Why it happens:** Separate grace period tracking for each protocol.
**How to avoid:** Single GracePeriodManager with all persisted locks (both NLM and SMB).
**Warning signs:** Lock state inconsistent after restart when both protocols were active.

### Pitfall 4: Wrong SMB Status Code for NLM Conflict

**What goes wrong:** SMB client doesn't understand denial reason.
**Why it happens:** Using STATUS_SHARING_VIOLATION instead of STATUS_LOCK_NOT_GRANTED.
**How to avoid:** Use STATUS_LOCK_NOT_GRANTED for byte-range lock conflicts (per [MS-ERREF](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/)).
**Warning signs:** Windows SMB client shows confusing error message or retries inappropriately.

### Pitfall 5: Blocking Lock Waits on SMB Forever

**What goes wrong:** NLM_BLOCKED never transitions to NLM_GRANTED because SMB lease break times out silently.
**Why it happens:** Not integrating NLM blocking queue with lease break scanner notifications.
**How to avoid:** When lease break completes (ack or timeout), check NLM blocking queue for waiters.
**Warning signs:** NLM blocking locks never granted when SMB client disconnects without ack.

### Pitfall 6: E2E Test Timeout Too Short

**What goes wrong:** Tests fail intermittently due to real kernel clients being slower than expected.
**Why it happens:** Using tight timeouts that work locally but fail under CI load.
**How to avoid:** Use configurable timeout (5s short for CI, 35s for thorough testing per decision).
**Warning signs:** Flaky tests that pass when run individually but fail in full suite.

## Code Examples

Verified patterns from official sources and project codebase:

### SMB STATUS Code Selection for NLM Conflicts

```go
// Source: Based on MS-ERREF and CONTEXT.md Claude's Discretion for SMB error code
// In internal/protocol/smb/v2/handlers/lock.go

func statusForNLMConflict(nlmLock *lock.EnhancedLock) types.Status {
    // STATUS_LOCK_NOT_GRANTED (0xC0000054) is appropriate for byte-range lock conflicts
    // STATUS_SHARING_VIOLATION (0xC0000043) is for share mode conflicts at file open time
    //
    // Since NLM locks are byte-range locks, STATUS_LOCK_NOT_GRANTED is correct.
    // Reference: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/
    return types.StatusLockNotGranted
}
```

### E2E Test Setup for Cross-Protocol Locking

```go
// Source: Based on existing test/e2e/cross_protocol_test.go patterns
//go:build e2e

func TestCrossProtocolLocking(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping cross-protocol lock tests in short mode")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    // Create shared stores
    metaStoreName := helpers.UniqueTestName("lockmeta")
    payloadStoreName := helpers.UniqueTestName("lockpayload")
    shareName := "/export"

    _, _ = cli.CreateMetadataStore(metaStoreName, "memory")
    _, _ = cli.CreatePayloadStore(payloadStoreName, "memory")
    _, _ = cli.CreateShare(shareName, metaStoreName, payloadStoreName,
        helpers.WithShareDefaultPermission("read-write"))

    // Setup SMB user
    smbUser := helpers.UniqueTestName("lockuser")
    smbPass := "testpass123"
    _, _ = cli.CreateUser(smbUser, smbPass)
    _ = cli.GrantUserPermission(shareName, smbUser, "read-write")

    // Enable adapters
    nfsPort := helpers.FindFreePort(t)
    _, _ = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))

    smbPort := helpers.FindFreePort(t)
    _, _ = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))

    framework.WaitForServer(t, nfsPort, 10*time.Second)
    framework.WaitForServer(t, smbPort, 10*time.Second)

    // Mount both protocols
    nfsMount := framework.MountNFS(t, nfsPort)
    t.Cleanup(nfsMount.Cleanup)

    smbMount := framework.MountSMB(t, smbPort, framework.SMBCredentials{
        Username: smbUser,
        Password: smbPass,
    })
    t.Cleanup(smbMount.Cleanup)

    // Run cross-protocol lock subtests
    t.Run("XPRO-01 NFS lock blocks SMB write lease", func(t *testing.T) {
        testNFSLockBlocksSMBLease(t, nfsMount, smbMount)
    })

    t.Run("XPRO-02 SMB write lease breaks for NFS lock", func(t *testing.T) {
        testSMBLeaseBreaksForNFSLock(t, nfsMount, smbMount)
    })

    t.Run("XPRO-03 Cross-protocol file integrity", func(t *testing.T) {
        testCrossProtocolFileIntegrity(t, nfsMount, smbMount)
    })
}
```

### Prometheus Metrics for Cross-Protocol Events

```go
// Source: Based on Prometheus naming conventions and CONTEXT.md Claude's Discretion
// In pkg/metadata/lock/metrics.go (extend existing)

var (
    crossProtocolConflictTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "dittofs",
            Subsystem: "locks",
            Name:      "cross_protocol_conflict_total",
            Help:      "Total cross-protocol lock conflicts",
        },
        []string{"initiator", "conflicting", "resolution"},
        // initiator: "nfs" or "smb"
        // conflicting: "nfs_lock" or "smb_lease" or "smb_lock"
        // resolution: "denied" or "break_initiated" or "break_completed"
    )

    crossProtocolBreakDuration = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Namespace: "dittofs",
            Subsystem: "locks",
            Name:      "cross_protocol_break_duration_seconds",
            Help:      "Time to complete cross-protocol lease/lock breaks",
            Buckets:   prometheus.ExponentialBuckets(0.1, 2, 10), // 0.1s to ~100s
        },
        []string{"trigger", "target"},
        // trigger: "nfs_write", "nfs_lock", "nfs_remove"
        // target: "smb_write_lease", "smb_handle_lease"
    )
)
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Separate lock namespaces | Unified lock manager | DittoFS Phase 1 | Cross-protocol visibility |
| No lease awareness in NFS | OplockChecker interface | DittoFS Phase 4 | NFS triggers SMB breaks |
| Per-protocol grace periods | Shared grace period | DittoFS Phase 5 (this) | Consistent recovery |
| Status only conflicts | Holder info in denials | DittoFS Phase 5 (this) | Better debugging |

**Deprecated/outdated:**
- Separate OplockManager state (Phase 4 migrated to unified lock store)
- Protocol-specific lock tables (replaced by unified EnhancedLock)

## Open Questions

Things that couldn't be fully resolved:

1. **Exact behavior when both Write lease break and NLM blocking queue have waiters**
   - What we know: Lease break completes, waiters should be checked
   - What's unclear: Order of notification when multiple waiters exist
   - Recommendation: FIFO order based on arrival time, lease break completion notifies blocking queue

2. **Handle lease break during rename (source vs destination)**
   - What we know: H lease on source must break before rename
   - What's unclear: What if destination also has H lease?
   - Recommendation: Break both source and destination H leases, wait for both

3. **Exact test timing for flaky prevention**
   - What we know: 200ms sleep after writes works for existing tests
   - What's unclear: Whether this is sufficient for lock operations
   - Recommendation: Use longer delays (500ms-1s) for lock-related tests, add retry logic

## Sources

### Primary (HIGH confidence)
- DittoFS codebase: `pkg/metadata/lock/` (LockManager, EnhancedLock, GracePeriodManager, LeaseBreakScanner)
- DittoFS codebase: `internal/protocol/smb/v2/handlers/oplock.go` (OplockManager, CheckAndBreakForWrite/Read)
- DittoFS codebase: `internal/protocol/nlm/handlers/lock.go` (NLM_LOCK handler)
- DittoFS codebase: `test/e2e/cross_protocol_test.go` (existing cross-protocol test patterns)
- [MS-SMB2 Protocol Specification](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) - Lease semantics
- [MS-ERREF](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/) - STATUS codes

### Secondary (MEDIUM confidence)
- [SMB Troubleshooting STATUS_LOCK_NOT_GRANTED](https://osqa-ask.wireshark.org/questions/37818/smb-troubleshooting-status_lock_not_granted/) - Status code usage
- [libsmb2 errors.c](https://github.com/sahlberg/libsmb2/blob/master/lib/errors.c) - Status to errno mapping
- [NetApp KB on STATUS_SHARING_VIOLATION](https://kb.netapp.com/on-prem/ontap/da/NAS/NAS-KBs/What_does_STATUS_SHARING_VIOLATION_MEAN) - Status distinction

### Tertiary (LOW confidence)
- Web search results for cross-protocol lock semantics
- Samba mailing list discussions on lease breaks

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Uses existing DittoFS patterns, no new dependencies
- Architecture: HIGH - Clear integration with existing Phase 1-4 code
- Cross-protocol conflict rules: HIGH - Based on CONTEXT.md locked decisions
- SMB status codes: HIGH - MS-ERREF specification is authoritative
- E2E test patterns: MEDIUM - Existing patterns work, timing may need tuning
- Grace period coordination: HIGH - Extends existing GracePeriodManager

**Research date:** 2026-02-05
**Valid until:** 60 days (stable domain, builds on completed Phase 1-4)
