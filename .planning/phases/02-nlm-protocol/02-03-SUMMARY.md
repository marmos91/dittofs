---
phase: 02-nlm-protocol
plan: 03
subsystem: locking
tags: [nlm, blocking-locks, callbacks, prometheus, rpc]

# Dependency graph
requires:
  - phase: 02-nlm-protocol
    provides: NLM dispatcher, synchronous lock handlers, MetadataService NLM methods
provides:
  - Per-file blocking lock queue with configurable limit
  - NLM_GRANTED callback client with 5s total timeout
  - Queue integration with lock/unlock handlers
  - NLM Prometheus metrics (nlm_* prefix)
affects: [04-e2e-testing, future-nlm-enhancements]

# Tech tracking
tech-stack:
  added: [prometheus]
  patterns: [callback-on-unlock, per-file-waiter-queue, nil-metrics-receiver]

key-files:
  created:
    - internal/protocol/nlm/blocking/queue.go
    - internal/protocol/nlm/blocking/waiter.go
    - internal/protocol/nlm/callback/client.go
    - internal/protocol/nlm/callback/granted.go
    - internal/protocol/nlm/handlers/granted.go
    - internal/protocol/nlm/metrics.go
  modified:
    - internal/protocol/nlm/handlers/handler.go
    - internal/protocol/nlm/handlers/lock.go
    - internal/protocol/nlm/handlers/cancel.go
    - pkg/metadata/service.go
    - pkg/adapter/nfs/nfs_adapter.go

key-decisions:
  - "5 second TOTAL timeout for NLM_GRANTED callbacks (per CONTEXT.md locked decision)"
  - "Fresh TCP connection for each callback (no connection caching)"
  - "Release lock immediately on callback failure (no hold period)"
  - "Per-file queue limit of 100 (DefaultBlockingQueueSize)"
  - "Callback address uses client IP with standard NLM port (12049)"
  - "Nil metrics receiver pattern for zero-overhead when disabled"

patterns-established:
  - "Unlock callback pattern: MetadataService.SetNLMUnlockCallback for async waiter notification"
  - "Per-file queue pattern: map[string][]*Waiter with RWMutex protection"
  - "Waiter cancellation: thread-safe via internal mutex on cancelled field"

# Metrics
duration: 8min
completed: 2026-02-05
---

# Phase 02 Plan 03: Blocking Lock Queue and NLM_GRANTED Callback Summary

**Per-file blocking lock queue with FIFO waiter processing and NLM_GRANTED callback client using 5-second total timeout**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-05T10:16:29Z
- **Completed:** 2026-02-05T10:24:32Z
- **Tasks:** 4
- **Files created:** 6
- **Files modified:** 5

## Accomplishments

- Per-file blocking lock queue with configurable limit (100 per file)
- NLM_GRANTED callback client with 5s TOTAL timeout (per CONTEXT.md locked decision)
- Integration with lock handler to queue waiters on conflict (block=true)
- Integration with unlock path via SetNLMUnlockCallback to process waiters
- NLM Prometheus metrics for observability (nlm_* prefix)

## Task Commits

Each task was committed atomically:

1. **Task 1: Create blocking lock queue infrastructure** - `250cc31` (feat)
2. **Task 2: Implement NLM_GRANTED callback client** - `6237ce0` (feat)
3. **Task 3: Integrate blocking queue with handlers** - `22a65ea` (feat)
4. **Task 4: Add NLM Prometheus metrics** - `011f5db` (feat)

## Files Created/Modified

**Created:**
- `internal/protocol/nlm/blocking/waiter.go` - Waiter struct with callback info and thread-safe cancellation
- `internal/protocol/nlm/blocking/queue.go` - BlockingQueue with per-file FIFO queues and limit enforcement
- `internal/protocol/nlm/callback/client.go` - TCP callback client with 5s total timeout
- `internal/protocol/nlm/callback/granted.go` - ProcessGrantedCallback with lock release on failure
- `internal/protocol/nlm/handlers/granted.go` - NLM_GRANTED procedure handler
- `internal/protocol/nlm/metrics.go` - NLM Prometheus metrics (requests, callbacks, queue size)

**Modified:**
- `internal/protocol/nlm/handlers/handler.go` - Added blockingQueue field and GetBlockingQueue() method
- `internal/protocol/nlm/handlers/lock.go` - Queue waiters on conflict, return NLM4_DENIED_NOLOCKS when full
- `internal/protocol/nlm/handlers/cancel.go` - Cancel from blocking queue instead of metadata service
- `pkg/metadata/service.go` - Added SetNLMUnlockCallback and GetLockManagerForShare methods
- `pkg/adapter/nfs/nfs_adapter.go` - Create blocking queue, set unlock callback, process waiters async

## Decisions Made

1. **5 second TOTAL timeout for callbacks**: Per CONTEXT.md locked decision, the callback timeout applies to dial+I/O combined, not per-operation. This is simpler and prevents long-running callbacks from blocking waiter processing.

2. **Fresh TCP connection per callback**: No connection caching or pooling. Each NLM_GRANTED callback opens a new TCP connection. This is simpler and avoids connection state management for rarely-used callbacks.

3. **Release lock immediately on callback failure**: Per CONTEXT.md locked decision, if the callback fails, the lock is released immediately. No hold period or retry. This prevents orphaned grants when clients become unreachable.

4. **Callback address uses standard NLM port**: Extracting callback address from client's source IP with port 12049. Real NLM clients may provide a different callback port, but this works for standard configurations.

5. **Nil metrics receiver pattern**: All Metrics methods handle nil receiver gracefully, enabling zero-overhead when metrics are disabled.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None - implementation proceeded smoothly following CONTEXT.md locked decisions.

## Next Phase Readiness

- Blocking lock infrastructure complete
- Ready for Plan 02-04: Grace period and client crash recovery
- NSM integration will build on the waiter queue for client crash cleanup

---
*Phase: 02-nlm-protocol*
*Completed: 2026-02-05*
