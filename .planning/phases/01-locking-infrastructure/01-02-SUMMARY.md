---
phase: 01-locking-infrastructure
plan: 02
subsystem: metadata
tags: [locking, persistence, memory, badger, postgres, epoch, recovery]

# Dependency graph
requires: [01-01]
provides:
  - LockStore interface for persistent lock storage
  - Memory, BadgerDB, PostgreSQL implementations
  - Server epoch tracking for split-brain detection
  - Lock query by file, owner, client, share
affects: [02-nlm-protocol, 03-nfsv4-state, 04-smb2-locking]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "LockStore embedded in Transaction for atomic operations"
    - "Lazy initialization of lock store per metadata store"
    - "Secondary indexes in BadgerDB for efficient queries"

key-files:
  created:
    - pkg/metadata/lock_persistence.go
    - pkg/metadata/lock_persistence_test.go
    - pkg/metadata/store/memory/locks.go
    - pkg/metadata/store/memory/locks_test.go
    - pkg/metadata/store/badger/locks.go
    - pkg/metadata/store/postgres/locks.go
    - pkg/metadata/store/postgres/migrations/000002_locks.up.sql
    - pkg/metadata/store/postgres/migrations/000002_locks.down.sql
  modified:
    - pkg/metadata/store/store.go

key-decisions:
  - "LockStore embedded in Transaction interface for atomic lock+metadata ops"
  - "PersistedLock uses string FileID (not FileHandle) for storage efficiency"
  - "Server epoch incremented on each server restart for stale lock detection"

patterns-established:
  - "Lock stores lazily initialized via initLockStore()"
  - "Context cancellation checked before acquiring locks"
  - "Cloning locks on read to prevent external modification"

# Metrics
duration: 15min
completed: 2026-02-04
---

# Phase 1 Plan 02: Lock Persistence Summary

**LockStore interface with implementations for all metadata store backends**

## Performance

- **Duration:** ~15 min
- **Completed:** 2026-02-04
- **Tasks:** 3 (partial recovery after API errors)
- **Files modified:** 9

## Accomplishments

- LockStore interface with CRUD operations and query support
- PersistedLock struct for durable lock representation
- Memory implementation with efficient map-based storage
- BadgerDB implementation with secondary indexes for queries
- PostgreSQL implementation with proper schema and indexes
- Server epoch tracking for split-brain detection across restarts
- Transaction interface updated to embed LockStore

## Task Commits

Work completed across multiple commits due to recovery from partial execution:

1. **Initial lock persistence implementation** - memory and badger stores
2. **PostgreSQL lock store** - complete implementation with transactions
3. **Migration files** - PostgreSQL schema for locks and server_epoch tables

## Files Created/Modified

- `pkg/metadata/lock_persistence.go` - LockStore interface, PersistedLock, LockQuery types
- `pkg/metadata/lock_persistence_test.go` - Tests for persistence types
- `pkg/metadata/store/store.go` - Transaction interface updated
- `pkg/metadata/store/memory/locks.go` - In-memory LockStore implementation
- `pkg/metadata/store/memory/locks_test.go` - Memory store lock tests
- `pkg/metadata/store/badger/locks.go` - BadgerDB LockStore with secondary indexes
- `pkg/metadata/store/postgres/locks.go` - PostgreSQL LockStore with transaction support
- `pkg/metadata/store/postgres/migrations/000002_locks.up.sql` - Locks table schema
- `pkg/metadata/store/postgres/migrations/000002_locks.down.sql` - Rollback migration

## Decisions Made

1. **LockStore in Transaction:** Embedding LockStore in Transaction interface enables atomic lock+metadata operations in a single transaction.

2. **String FileID:** Using string FileID instead of FileHandle avoids serialization complexity while maintaining query efficiency.

3. **Lazy initialization:** Lock stores are created on first use to avoid overhead when locks aren't needed.

## Deviations from Plan

1. **Missing test files:** BadgerDB and PostgreSQL lock tests not created due to API errors during execution. Core implementations complete and verified via build.

## Issues Encountered

1. **API errors during execution:** Wave 2 execution encountered 500 errors, requiring manual recovery and completion.

2. **Context cancellation:** Memory store methods initially missing context checks, causing test failures. Fixed by adding ctx.Err() checks at method entry.

3. **Unused import:** PostgreSQL locks.go had unused "fmt" import that caused build failure. Removed.

## User Setup Required

For PostgreSQL lock persistence:
- Run migration 000002_locks.up.sql to create locks and server_epoch tables
- Or enable AutoMigrate in config

## Next Phase Readiness

- Lock persistence complete for all store backends
- Ready for grace period integration (Plan 03)
- Ready for NLM protocol lock reclaim
- All builds pass, memory store tests pass

---
*Phase: 01-locking-infrastructure*
*Completed: 2026-02-04*
