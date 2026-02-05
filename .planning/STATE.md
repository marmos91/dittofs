# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-04)

**Core value:** Enterprise-grade multi-protocol file access with unified locking and Kerberos authentication
**Current focus:** Phase 3 in progress (NSM Protocol)

## Current Position

Phase: 3 of 28 (NSM Protocol)
Plan: 1 of 3 complete
Status: In progress
Last activity: 2026-02-05 - Completed 03-01-PLAN.md

Progress: [########----------------] 29% (8/28 plans complete)

## Performance Metrics

**Velocity:**
- Total plans completed: 8
- Average duration: 13 min
- Total execution time: 1.73 hours

**By Phase:**

| Phase | Plans | Total | Avg/Plan | Status |
|-------|-------|-------|----------|--------|
| 01-locking-infrastructure | 4 | 75 min | 18.75 min | COMPLETE |
| 02-nlm-protocol | 3 | 25 min | 8.3 min | COMPLETE |
| 03-nsm-protocol | 1 | 3 min | 3 min | IN PROGRESS |

**Recent Trend:**
- Last 5 plans: 02-01 (6 min), 02-02 (11 min), 02-03 (8 min), 03-01 (3 min)
- Trend: Foundation work pays off - type/XDR packages are fast

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

## Phase 03 Accomplishments

### Plan 03-01: NSM Types and Foundation - COMPLETE
- NSM types package at internal/protocol/nsm/types/
- NSM XDR encode/decode at internal/protocol/nsm/xdr/
- Extended ClientRegistration with NSM fields (MonName, Priv, SMState, CallbackInfo)
- NSMCallback struct for RPC callback details
- ClientRegistrationStore interface for persistence
- Conversion functions for persistence (To/From PersistedClientRegistration)

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
- [03-01]: NSM types package mirrors NLM structure
- [03-01]: priv field as [16]byte fixed array (XDR opaque[16])
- [03-01]: ClientRegistrationStore interface for persistence
- [03-01]: Extend existing ClientRegistration vs new type

### Pending Todos

None.

### Blockers/Concerns

None.

## Next Steps

**Phase 3 Continuation:**
- Plan 03-02: NSM Dispatcher and Handlers
- Plan 03-03: NSM Service and Store Implementations

## Session Continuity

Last session: 2026-02-05
Stopped at: Completed 03-01-PLAN.md
Resume file: None
