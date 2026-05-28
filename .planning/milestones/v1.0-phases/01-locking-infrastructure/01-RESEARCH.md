# Phase 1: Locking Infrastructure - Research

**Researched:** 2026-02-04
**Domain:** Protocol-agnostic file lock manager with persistence, grace periods, and connection tracking
**Confidence:** HIGH

## Summary

This phase builds the unified lock manager that provides byte-range locking for both NFS (NLM) and SMB protocols. The research examined: the existing DittoFS codebase architecture (especially the current ephemeral `LockManager` in `pkg/metadata/locking.go`), POSIX lock semantics, NLM protocol specifications, distributed lock manager patterns, and persistence strategies.

**Key findings:**
- DittoFS already has a basic in-memory `LockManager` with correct POSIX conflict detection. This needs to be enhanced with persistence, grace periods, and connection tracking.
- The lock manager should remain embedded in `MetadataService` (per project decision) and use the existing `MetadataStore` transaction abstraction for atomic persistence.
- Grace period recovery follows NLM/NFSv4 patterns: restore locks on startup, deny new locks during grace, allow reclaims, release unclaimed locks when grace ends.
- Deadlock detection uses the Wait-For Graph (WFG) algorithm with cycle detection. For single-node deployment, a simple DFS-based cycle detector is sufficient.

**Primary recommendation:** Extend the existing `LockManager` to support persistence via the metadata store's transaction abstraction, add grace period state machine, implement connection tracking with adapter-controlled TTLs, and add deadlock detection using WFG cycle detection.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | 1.22+ | Mutex/RWMutex for in-memory coordination | Thread-safe lock table access |
| Go stdlib `context` | 1.22+ | Cancellation, timeouts for blocking locks | Proper timeout handling |
| Go stdlib `time` | 1.22+ | Grace period timers, lock timestamps | Timer management |
| `github.com/prometheus/client_golang` | 1.19+ | Metrics exposition | Already used in DittoFS |
| Existing `pkg/metadata` store abstractions | - | BadgerDB/Postgres transactions | Atomic lock persistence |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/stretchr/testify` | 1.9+ | Test assertions | Already used in DittoFS |
| Go stdlib `container/heap` | 1.22+ | Priority queue for blocking lock timeouts | Efficient timeout management |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| In-process WFG | Distributed deadlock detection | Single-node only in Phase 1, distributed adds complexity |
| Linear scan for conflicts | Interval tree | Linear is O(n) but n is small per file; interval tree adds complexity |
| Synchronous persistence | Write-ahead log | Metadata store transactions already provide durability |

**No new dependencies required** - extend existing patterns.

## Architecture Patterns

### Recommended Project Structure

The lock manager should be enhanced within the existing `pkg/metadata` package structure:

```
pkg/metadata/
├── locking.go           # Existing: FileLock, RangesOverlap, IsLockConflicting (enhance)
├── locking_test.go      # Existing: Tests (extend)
├── lock_manager.go      # NEW: Enhanced LockManager with persistence hooks
├── lock_persistence.go  # NEW: Lock persistence interface and helpers
├── lock_grace.go        # NEW: Grace period state machine
├── lock_deadlock.go     # NEW: Wait-For Graph deadlock detection
├── lock_connection.go   # NEW: Connection tracking
├── lock_metrics.go      # NEW: Prometheus metrics
├── service.go           # Existing: MetadataService owns LockManager
└── store/
    ├── memory/
    │   └── locks.go     # NEW: In-memory lock persistence (for testing)
    ├── badger/
    │   └── locks.go     # NEW: BadgerDB lock persistence
    └── postgres/
        └── locks.go     # NEW: PostgreSQL lock persistence
```

### Pattern 1: Lock Persistence via Metadata Store Transactions

**What:** Lock operations use the same transaction abstraction as file operations, ensuring atomicity.

**When to use:** All lock acquire/release operations that must survive restart.

**Example:**
```go
// Source: Derived from existing pkg/metadata/store/badger/transaction.go pattern
func (lm *LockManager) AcquireLock(ctx context.Context, handle FileHandle, lock FileLock) error {
    store, err := lm.storeForHandle(handle)
    if err != nil {
        return err
    }

    return store.WithTransaction(ctx, func(tx Transaction) error {
        // 1. Check for conflicts (in-memory + persisted)
        if conflict := lm.checkConflict(tx, handle, lock); conflict != nil {
            return NewLockedError("", conflict)
        }

        // 2. Check deadlock (if blocking)
        if lock.Blocking && lm.wouldDeadlock(lock.Owner, handle) {
            return NewDeadlockError("")
        }

        // 3. Persist lock
        if err := tx.PutLock(ctx, handle, lock); err != nil {
            return err
        }

        // 4. Update in-memory state
        lm.addLockInMemory(handle, lock)
        return nil
    })
}
```

### Pattern 2: Grace Period State Machine

**What:** After server restart, enter grace period where only reclaims are allowed.

**When to use:** Server startup when locks were previously persisted.

**Example:**
```go
// Source: Derived from NLM RFC and Martin Kleppmann's distributed locking patterns
type GraceState int

const (
    GraceStateNormal GraceState = iota  // Normal operation
    GraceStateActive                     // Grace period active, reclaims only
)

type GracePeriodManager struct {
    mu              sync.RWMutex
    state           GraceState
    graceEnd        time.Time
    expectedClients map[string]bool  // Clients expected to reclaim
    reclaimedBy     map[string]bool  // Clients that have reclaimed
}

func (g *GracePeriodManager) IsAllowed(op LockOperation) bool {
    g.mu.RLock()
    defer g.mu.RUnlock()

    if g.state == GraceStateNormal {
        return true
    }

    // During grace: only reclaims and tests allowed
    return op.IsReclaim || op.IsTest
}

func (g *GracePeriodManager) CheckEarlyExit() {
    g.mu.Lock()
    defer g.mu.Unlock()

    // Exit early if all expected clients have reclaimed
    if len(g.expectedClients) > 0 &&
       len(g.reclaimedBy) >= len(g.expectedClients) {
        g.state = GraceStateNormal
    }
}
```

### Pattern 3: Wait-For Graph Deadlock Detection

**What:** Maintain a directed graph where edges represent "A waits for B". Detect cycles before adding edges.

**When to use:** When a lock request would block.

**Example:**
```go
// Source: Based on academic literature on WFG deadlock detection
type WaitForGraph struct {
    mu    sync.RWMutex
    edges map[string]map[string]bool  // waiter -> set of owners being waited on
}

func (wfg *WaitForGraph) WouldCauseCycle(waiter string, owners []string) bool {
    wfg.mu.RLock()
    defer wfg.mu.RUnlock()

    // DFS from each owner to see if we can reach waiter
    visited := make(map[string]bool)
    for _, owner := range owners {
        if wfg.canReach(owner, waiter, visited) {
            return true
        }
    }
    return false
}

func (wfg *WaitForGraph) canReach(from, to string, visited map[string]bool) bool {
    if from == to {
        return true
    }
    if visited[from] {
        return false
    }
    visited[from] = true

    for next := range wfg.edges[from] {
        if wfg.canReach(next, to, visited) {
            return true
        }
    }
    return false
}
```

### Pattern 4: Connection Tracking with Adapter-Controlled TTL

**What:** Adapters register clients with optional TTL. Lock manager releases locks based on TTL policy.

**When to use:** Client disconnect handling differs by protocol.

**Example:**
```go
// Source: Design based on CONTEXT.md decisions
type ClientRegistration struct {
    ClientID     string
    AdapterType  string
    TTL          time.Duration  // 0 = immediate release on disconnect
    RegisteredAt time.Time
    LastSeen     time.Time
}

type ConnectionTracker struct {
    mu      sync.RWMutex
    clients map[string]*ClientRegistration
    limits  map[string]int  // AdapterType -> max connections
}

func (ct *ConnectionTracker) RegisterClient(clientID, adapterType string, ttl time.Duration) error {
    ct.mu.Lock()
    defer ct.mu.Unlock()

    // Check connection limit for adapter
    count := ct.countClientsForAdapter(adapterType)
    if limit, ok := ct.limits[adapterType]; ok && count >= limit {
        return ErrConnectionLimitReached
    }

    ct.clients[clientID] = &ClientRegistration{
        ClientID:     clientID,
        AdapterType:  adapterType,
        TTL:          ttl,
        RegisteredAt: time.Now(),
        LastSeen:     time.Now(),
    }
    return nil
}

func (ct *ConnectionTracker) UnregisterClient(clientID string) {
    ct.mu.Lock()
    reg, ok := ct.clients[clientID]
    ct.mu.Unlock()

    if !ok {
        return
    }

    if reg.TTL == 0 {
        // Immediate release (NFS behavior)
        ct.releaseLocks(clientID)
        ct.removeClient(clientID)
    } else {
        // Deferred release (SMB durable handles)
        time.AfterFunc(reg.TTL, func() {
            ct.releaseLocks(clientID)
            ct.removeClient(clientID)
        })
    }
}
```

### Pattern 5: Lock Splitting (POSIX Semantics)

**What:** Unlocking the middle of a range splits into two locks.

**When to use:** When unlock range is subset of existing lock.

**Example:**
```go
// Source: POSIX fcntl(2) specification
func (lm *LockManager) splitLock(existing FileLock, unlockOffset, unlockLength uint64) []FileLock {
    unlockEnd := unlockOffset + unlockLength
    existingEnd := existing.Offset + existing.Length

    var result []FileLock

    // Left portion: [existing.Offset, unlockOffset)
    if unlockOffset > existing.Offset {
        result = append(result, FileLock{
            SessionID: existing.SessionID,
            Offset:    existing.Offset,
            Length:    unlockOffset - existing.Offset,
            Exclusive: existing.Exclusive,
        })
    }

    // Right portion: [unlockEnd, existingEnd)
    if unlockEnd < existingEnd {
        result = append(result, FileLock{
            SessionID: existing.SessionID,
            Offset:    unlockEnd,
            Length:    existingEnd - unlockEnd,
            Exclusive: existing.Exclusive,
        })
    }

    return result
}
```

### Anti-Patterns to Avoid

- **Global lock table lock during persistence:** Use per-file locking or lock-free structures for scalability
- **Blocking indefinitely on lock acquire:** Always use context with timeout
- **Checking conflicts only in memory:** Must check persisted state too after restart
- **Ignoring protocol semantics in lock manager:** Keep protocol-agnostic; adapters translate
- **Persisting transient state (blocking waiters):** Only persist granted locks, not wait queues

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Transaction atomicity | Custom WAL | MetadataStore.WithTransaction | BadgerDB/Postgres handle durability |
| Range overlap detection | Ad-hoc comparisons | Existing `RangesOverlap()` | Already correct and tested |
| Lock conflict detection | Custom logic | Existing `IsLockConflicting()` | POSIX rules already implemented |
| Metrics collection | Custom counters | `prometheus/client_golang` | Standard, already integrated |
| Timer management | Manual goroutines | `time.AfterFunc`, `context.WithTimeout` | Proper cancellation handling |

**Key insight:** DittoFS already has the building blocks. The challenge is composing them correctly with persistence and state machines, not building from scratch.

## Common Pitfalls

### Pitfall 1: Stale In-Memory State After Crash Recovery

**What goes wrong:** Lock manager loads persisted locks but doesn't validate clients still exist.
**Why it happens:** Persisted locks reference clients that may have disconnected during downtime.
**How to avoid:** On recovery, mark all locks as "pending reclaim" and require clients to reclaim during grace period. Unclaimed locks are released when grace ends.
**Warning signs:** Locks persist forever blocking legitimate clients after restart.

### Pitfall 2: Race Between Conflict Check and Lock Insert

**What goes wrong:** Two concurrent requests both check for conflicts, both see none, both insert conflicting locks.
**Why it happens:** Non-atomic read-then-write pattern.
**How to avoid:** Use database transaction with appropriate isolation level, or hold lock table mutex across check-and-insert.
**Warning signs:** Spurious "lock already held" errors or data corruption.

### Pitfall 3: Deadlock Detection False Positives

**What goes wrong:** Detect cycles that don't represent real deadlocks.
**Why it happens:** WFG contains stale edges from released locks or disconnected clients.
**How to avoid:** Remove WFG edges immediately when locks are released or clients disconnect.
**Warning signs:** Legitimate lock requests denied with DEADLCK error.

### Pitfall 4: Grace Period Never Ends

**What goes wrong:** Server stays in grace period indefinitely.
**Why it happens:** Missing grace period timer, or early-exit check never triggers.
**How to avoid:** Always start a timer for grace period end; early exit is optimization, not requirement.
**Warning signs:** All new lock requests fail with "grace period" error forever.

### Pitfall 5: Lock Splitting Produces Overlapping Locks

**What goes wrong:** After split, the two resulting locks overlap or have gaps.
**Why it happens:** Off-by-one errors in offset/length calculations.
**How to avoid:** Comprehensive unit tests with edge cases (unlock at start, end, middle, exact match).
**Warning signs:** Subsequent lock requests succeed when they should fail, or vice versa.

### Pitfall 6: Memory Leak in Long-Running Server

**What goes wrong:** Lock entries accumulate for deleted files.
**Why it happens:** File deletion doesn't clean up associated locks.
**How to avoid:** `RemoveFileLocks()` already exists; ensure it's called on file deletion. Also run periodic garbage collection.
**Warning signs:** Memory usage grows over time; lock table contains stale entries.

## Code Examples

Verified patterns from official sources and project codebase:

### Lock Persistence Schema (PostgreSQL)

```sql
-- Source: Derived from existing pkg/metadata/store/postgres/migrations pattern
CREATE TABLE locks (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    share_name      TEXT NOT NULL,
    file_id         UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    owner_id        TEXT NOT NULL,          -- Protocol-provided owner identifier
    client_id       TEXT NOT NULL,          -- Connection tracker client ID
    lock_type       SMALLINT NOT NULL,      -- 0=shared, 1=exclusive
    offset          BIGINT NOT NULL,
    length          BIGINT NOT NULL,        -- 0 = to EOF
    acquired_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    server_epoch    BIGINT NOT NULL,        -- For split-brain detection

    CONSTRAINT valid_lock_type CHECK (lock_type IN (0, 1)),
    CONSTRAINT valid_offset CHECK (offset >= 0),
    CONSTRAINT valid_length CHECK (length >= 0)
);

CREATE INDEX idx_locks_file_id ON locks(file_id);
CREATE INDEX idx_locks_owner_id ON locks(owner_id);
CREATE INDEX idx_locks_client_id ON locks(client_id);
CREATE INDEX idx_locks_share_name ON locks(share_name);
```

### Lock Persistence Schema (BadgerDB)

```go
// Source: Derived from existing pkg/metadata/store/badger/encoding.go pattern
const (
    prefixLock       = "lock:"       // lock:<shareName>:<fileID>:<lockID>
    prefixLockByOwner = "lockowner:" // lockowner:<ownerID>:<lockID>
    prefixLockByFile  = "lockfile:"  // lockfile:<fileID>:<lockID>
)

func keyLock(shareName, fileID, lockID string) []byte {
    return []byte(fmt.Sprintf("%s%s:%s:%s", prefixLock, shareName, fileID, lockID))
}

type persistedLock struct {
    ID         string    `json:"id"`
    ShareName  string    `json:"share_name"`
    FileID     string    `json:"file_id"`
    OwnerID    string    `json:"owner_id"`
    ClientID   string    `json:"client_id"`
    LockType   int       `json:"lock_type"`  // 0=shared, 1=exclusive
    Offset     uint64    `json:"offset"`
    Length     uint64    `json:"length"`
    AcquiredAt time.Time `json:"acquired_at"`
    Epoch      uint64    `json:"epoch"`
}
```

### Prometheus Metrics

```go
// Source: Based on Prometheus naming conventions from official docs
var (
    lockAcquireTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "dittofs",
            Subsystem: "locks",
            Name:      "acquire_total",
            Help:      "Total number of lock acquire attempts",
        },
        []string{"share", "type", "status"},  // type: shared/exclusive, status: granted/denied/deadlock
    )

    lockReleaseTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "dittofs",
            Subsystem: "locks",
            Name:      "release_total",
            Help:      "Total number of lock releases",
        },
        []string{"share", "reason"},  // reason: explicit/timeout/disconnect/grace_expired
    )

    lockBlockingDuration = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Namespace: "dittofs",
            Subsystem: "locks",
            Name:      "blocking_duration_seconds",
            Help:      "Time spent waiting for blocked lock requests",
            Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15),  // 1ms to ~32s
        },
        []string{"share"},
    )

    lockActiveGauge = promauto.NewGaugeVec(
        prometheus.GaugeOpts{
            Namespace: "dittofs",
            Subsystem: "locks",
            Name:      "active_count",
            Help:      "Number of currently held locks",
        },
        []string{"share", "type"},
    )

    connectionActiveGauge = promauto.NewGaugeVec(
        prometheus.GaugeOpts{
            Namespace: "dittofs",
            Subsystem: "connections",
            Name:      "active_count",
            Help:      "Number of active client connections",
        },
        []string{"adapter"},
    )

    gracePeriodActive = promauto.NewGauge(
        prometheus.GaugeOpts{
            Namespace: "dittofs",
            Subsystem: "locks",
            Name:      "grace_period_active",
            Help:      "1 if grace period is active, 0 otherwise",
        },
    )
)
```

### Lock Store Interface Extension

```go
// Source: Derived from existing pkg/metadata/store.go pattern
// Add to Transaction interface for lock operations

type LockStore interface {
    // PutLock persists a lock. Overwrites if lock with same ID exists.
    PutLock(ctx context.Context, lock *PersistedLock) error

    // GetLock retrieves a lock by ID.
    GetLock(ctx context.Context, lockID string) (*PersistedLock, error)

    // DeleteLock removes a lock by ID.
    DeleteLock(ctx context.Context, lockID string) error

    // ListLocksByFile returns all locks for a file.
    ListLocksByFile(ctx context.Context, fileHandle FileHandle) ([]*PersistedLock, error)

    // ListLocksByOwner returns all locks held by an owner.
    ListLocksByOwner(ctx context.Context, ownerID string) ([]*PersistedLock, error)

    // ListLocksByClient returns all locks held by a client.
    ListLocksByClient(ctx context.Context, clientID string) ([]*PersistedLock, error)

    // DeleteLocksByClient removes all locks for a client.
    DeleteLocksByClient(ctx context.Context, clientID string) (int, error)

    // ListAllLocks returns all locks (for recovery).
    ListAllLocks(ctx context.Context) ([]*PersistedLock, error)

    // GetServerEpoch returns current server epoch.
    GetServerEpoch(ctx context.Context) (uint64, error)

    // IncrementServerEpoch increments and returns new epoch.
    IncrementServerEpoch(ctx context.Context) (uint64, error)
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Ephemeral locks only | Persistent locks for SMB/NFSv4 | NFSv4 (2003), SMB2 (2006) | Locks survive server restart |
| IP-based client identity | Session-based identity | NFSv4 | Migration, multi-homed clients |
| Synchronous blocking | Async callbacks (NLM) | Original NLM | Better protocol efficiency |
| Single lock manager | Protocol-specific translation | Modern filesystems | Unified backend, protocol flexibility |

**Deprecated/outdated:**
- NLM versions 1-3: Superseded by NLMv4 with 64-bit ranges
- Simple spin-lock protection: Replaced by fine-grained locking or lock-free structures at scale

## Open Questions

Things that couldn't be fully resolved:

1. **Interval Tree vs Linear Scan for Conflict Detection**
   - What we know: Linear scan is O(n) per lock request where n = locks on file. Interval tree is O(log n + k).
   - What's unclear: What's the typical lock count per file in production workloads?
   - Recommendation: Start with linear scan (already implemented). Profile in integration tests. Add interval tree if bottleneck appears. Most files have few locks.

2. **Epoch Tracking Granularity**
   - What we know: Server epoch helps detect stale locks from pre-restart.
   - What's unclear: Single global epoch vs per-share epoch vs per-client epoch.
   - Recommendation: Start with single global epoch (simplest). Per-share if needed for future HA.

3. **Connection Flapping Detection**
   - What we know: Rapid connect/disconnect could cause lock thrashing.
   - What's unclear: How common is this in practice? What's the right detection threshold?
   - Recommendation: Log events, add metrics. Implement throttling only if observed as a problem.

## Sources

### Primary (HIGH confidence)
- DittoFS codebase: `pkg/metadata/locking.go`, `pkg/metadata/service.go`, `pkg/metadata/store/*`
- [POSIX fcntl documentation](https://pubs.opengroup.org/onlinepubs/9699969599/functions/fcntl.html)
- [NLM Protocol Version 4 specification](https://pubs.opengroup.org/onlinepubs/9629799/chap14.htm)

### Secondary (MEDIUM confidence)
- [Martin Kleppmann: How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html) - Fencing token patterns
- [Prometheus naming conventions](https://prometheus.io/docs/practices/naming/)
- [Wait-For Graph deadlock detection](https://www.geeksforgeeks.org/computer-networks/wait-for-graph-deadlock-detection-in-distributed-system/)
- [Redis Distributed Locks](https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/) - Grace period patterns
- [Samba File Locking Architecture](https://www.samba.org/samba/docs/old/Samba3-HOWTO/locking.html)

### Tertiary (LOW confidence)
- Various web search results on interval trees and lock manager implementations
- Academic papers on distributed deadlock detection (for future HA work)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Uses existing DittoFS patterns, no new dependencies
- Architecture: HIGH - Follows established POSIX/NLM semantics, integrates with existing store abstraction
- Pitfalls: HIGH - Based on documented issues and codebase analysis
- Persistence schema: MEDIUM - Derived from existing patterns, needs validation in implementation
- Performance characteristics: LOW - Needs profiling in real workloads

**Research date:** 2026-02-04
**Valid until:** 60 days (stable domain, no rapidly changing external dependencies)
