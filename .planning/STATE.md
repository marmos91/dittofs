# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-04)

**Core value:** Enterprise-grade multi-protocol file access with unified locking and Kerberos authentication
**Current focus:** Phase 2 - NLM Protocol Implementation (Plan 03 complete)

## Current Position

Phase: 2 of 28 (NLM Protocol)
Plan: 3 of 4 in current phase
Status: In progress
Last activity: 2026-02-05 - Completed 02-03-PLAN.md (Blocking lock queue and GRANTED callback)

Progress: [#######-----------------] 25% (7/28 plans complete)

## Performance Metrics

**Velocity:**
- Total plans completed: 7
- Average duration: 14 min
- Total execution time: 1.68 hours

**By Phase:**

| Phase | Plans | Total | Avg/Plan | Status |
|-------|-------|-------|----------|--------|
| 01-locking-infrastructure | 4 | 75 min | 18.75 min | COMPLETE |
| 02-nlm-protocol | 3 | 25 min | 8.3 min | IN PROGRESS |

**Recent Trend:**
- Last 5 plans: 01-04 (45 min), 02-01 (6 min), 02-02 (11 min), 02-03 (8 min)
- Trend: Phase 2 plans faster due to building on Phase 1 infrastructure

*Updated after each plan completion*

## Phase 01 Accomplishments

### Plan 01-01: Lock Manager Enhancements
- EnhancedLock type with protocol-agnostic ownership model
- POSIX lock splitting (SplitLock, MergeLocks)
- Atomic lock upgrade (shared to exclusive)
- Wait-For Graph deadlock detection
- Lock configuration and limits tracking

### Plan 01-02: Lock Persistence
- LockStore interface for all metadata store backends
- Memory, BadgerDB, PostgreSQL implementations
- Server epoch tracking for split-brain detection
- Transaction integration for atomic operations

### Plan 01-03: Grace Period and Metrics
- Grace period state machine for lock reclaim
- Connection tracker with adapter-controlled TTL
- Full Prometheus metrics suite
- Early grace period exit optimization

### Plan 01-04: Package Reorganization - COMPLETE
- Created `pkg/metadata/errors/` package (leaf, no deps)
- Created `pkg/metadata/lock/` package for all lock code
- Import graph: errors <- lock <- metadata <- stores
- No circular dependencies
- Backward compatibility via type aliases

## Phase 02 Accomplishments

### Plan 02-01: XDR Utilities and NLM Types - COMPLETE
- Shared XDR package at internal/protocol/xdr/ (no DittoFS dependencies)
- NFS XDR refactored to delegate to shared utilities
- NLM v4 constants (program 100021, procedures, status codes)
- NLM v4 types (NLM4Lock, NLM4Holder, request/response structures)
- NLM XDR encode/decode functions for all message types

### Plan 02-02: NLM Dispatcher and Synchronous Operations - COMPLETE
- NLM procedure handlers (NULL, TEST, LOCK, UNLOCK, CANCEL)
- NLM dispatch table mapping procedures to handlers
- MetadataService NLM methods (LockFileNLM, TestLockNLM, UnlockFileNLM, CancelBlockingLock)
- NLM program routing in NFS adapter (same port 12049)
- Package restructure to avoid import cycles (nlm/types subpackage)

### Plan 02-03: Blocking Lock Queue and GRANTED Callback - COMPLETE
- Per-file blocking lock queue with configurable limit (100 per file)
- NLM_GRANTED callback client with 5s TOTAL timeout
- Queue integration with lock/unlock handlers
- SetNLMUnlockCallback for async waiter notification
- NLM Prometheus metrics (nlm_* prefix)

## Accumulated Context

### Decisions

Decisions are logged in PROJECT.md Key Decisions table.
Recent decisions affecting current work:

- [Init]: NLM before NFSv4 - Build locking foundation first
- [Init]: Unified Lock Manager - Single lock model for NFS+SMB
- [Init]: Lock state in metadata store - Atomic with file operations
- [01-01]: OwnerID as opaque string - Lock manager does not parse protocol prefix
- [01-01]: Enhanced locks stored per-LockManager instance (not global)
- [01-01]: Atomic upgrade returns ErrLockConflict when other readers exist
- [01-02]: LockStore embedded in Transaction for atomic operations
- [01-02]: PersistedLock uses string FileID for storage efficiency
- [01-03]: Grace period blocks new locks, allows reclaims and tests
- [01-03]: Connection TTL controlled by adapter (NFS=0, SMB may have grace)
- [01-04]: Import graph: errors <- lock <- metadata <- stores
- [01-04]: Backward compatibility via type aliases in pkg/metadata
- [02-01]: Shared XDR package at internal/protocol/xdr/ for NFS+NLM reuse
- [02-01]: NLM v4 only (64-bit offsets/lengths), not v1-3
- [02-02]: NLM types moved to nlm/types subpackage to avoid import cycle
- [02-02]: NLM handler initialized with MetadataService from runtime
- [02-02]: Owner ID format: nlm:{caller_name}:{svid}:{oh_hex}
- [02-03]: 5 second TOTAL timeout for NLM_GRANTED callbacks
- [02-03]: Fresh TCP connection per callback (no caching)
- [02-03]: Release lock immediately on callback failure
- [02-03]: Unlock callback pattern for async waiter notification

### Pending Todos

None.

### Blockers/Concerns

None.

## Next Steps

**Plan 02-04: NSM Integration and Lock Reclaim**
- Network Status Monitor integration
- Lock reclaim during grace period
- Client state tracking
- FREE_ALL procedure for client crash cleanup

## Session Continuity

Last session: 2026-02-05 10:24:32 UTC
Stopped at: Completed 02-03-PLAN.md
Resume file: None
