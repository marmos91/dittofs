# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-04)

**Core value:** Enterprise-grade multi-protocol file access with unified locking and Kerberos authentication
**Current focus:** Phase 4 COMPLETE (SMB Leases)

## Current Position

Phase: 4 of 28 (SMB Leases)
Plan: 3 of 3 complete
Status: PHASE COMPLETE
Last activity: 2026-02-05 - Completed 04-03-PLAN.md

Progress: [#############-----------] 46% (13/28 plans complete)

## Performance Metrics

**Velocity:**
- Total plans completed: 13
- Average duration: 12 min
- Total execution time: 2.5 hours

**By Phase:**

| Phase | Plans | Total | Avg/Plan | Status |
|-------|-------|-------|----------|--------|
| 01-locking-infrastructure | 4 | 75 min | 18.75 min | COMPLETE |
| 02-nlm-protocol | 3 | 25 min | 8.3 min | COMPLETE |
| 03-nsm-protocol | 3 | 19 min | 6.3 min | COMPLETE |
| 04-smb-leases | 3 | 29 min | 9.7 min | COMPLETE |

**Recent Trend:**
- Last 5 plans: 03-03 (6 min), 04-01 (7 min), 04-02 (7 min), 04-03 (15 min)
- Trend: Cross-protocol integration requires more time but patterns established

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

### Plan 03-02: NSM Handlers and Dispatch - COMPLETE
- NSM handler struct with ConnectionTracker, ClientRegistrationStore, server state
- NSM dispatch table mapping procedures to handlers
- SM_NULL, SM_STAT, SM_MON, SM_UNMON, SM_UNMON_ALL, SM_NOTIFY handlers
- Client registration storage in Memory, BadgerDB, PostgreSQL
- PostgreSQL migration 000003_clients for nsm_client_registrations table
- NSM program (100024) routing in NFS adapter

### Plan 03-03: NSM Crash Recovery - COMPLETE
- SM_NOTIFY callback client with 5s total timeout
- Notifier for parallel SM_NOTIFY on server restart
- NLM FREE_ALL handler (procedure 23) for bulk lock release
- NSM Prometheus metrics (nsm_* prefix)
- NFS adapter integration for startup notification

## Phase 04 Accomplishments

### Plan 04-01: SMB Lease Types - COMPLETE
- LeaseInfo struct with R/W/H state flags matching MS-SMB2 spec
- Lease state constants (0x01=R, 0x02=W, 0x04=H)
- EnhancedLock.Lease field for unified lock manager integration
- PersistedLock lease fields (LeaseKey, LeaseState, LeaseEpoch, BreakToState, Breaking)
- Lease conflict detection in IsEnhancedLockConflicting
- LockQuery.IsLease filter for listing leases vs byte-range locks
- Full round-trip conversion (ToPersistedLock/FromPersistedLock)

### Plan 04-02: OplockManager Refactoring - COMPLETE
- OplockManager refactored with LockStore dependency for lease persistence
- RequestLease/AcknowledgeLeaseBreak/ReleaseLease methods
- LeaseBreakScanner with 35s default timeout
- Cross-protocol break triggers (CheckAndBreakForWrite/Read)
- Backward compatible with existing oplock API

### Plan 04-03: Cross-Protocol Breaks and CREATE Context - COMPLETE
- ErrLeaseBreakPending error for signaling pending breaks
- CheckAndBreakForWrite/Read return ErrLeaseBreakPending for W leases
- OplockChecker interface in MetadataService for cross-protocol visibility
- waitForLeaseBreak helper in NFS handlers with 35s timeout
- NFS WRITE/READ handlers call CheckAndBreakLeasesFor{Write,Read}
- Lease create context (RqLs/RsLs) parsing and encoding per MS-SMB2
- SMB CREATE processes lease contexts when OplockLevel=0xFF
- CREATE response includes granted lease state in RsLs context

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
- [03-02]: HandlerResult in handlers package (close to handlers)
- [03-02]: Client ID format: nsm:{client_addr}:{callback_host}
- [03-02]: NSM v1 only (standard version)
- [03-03]: Parallel SM_NOTIFY using goroutines for fastest recovery
- [03-03]: Failed notification = client crashed, cleanup locks immediately
- [03-03]: FREE_ALL returns void per NLM spec
- [03-03]: Background notification goroutine (non-blocking)
- [04-01]: Lease state constants match MS-SMB2 2.2.13.2.8 spec values
- [04-01]: LeaseInfo embedded in EnhancedLock via pointer (nil for byte-range locks)
- [04-01]: Centralized MatchesLock method in LockQuery for consistent filtering
- [04-01]: BreakStarted is runtime-only, not persisted
- [04-02]: LockStore dependency injected via NewOplockManagerWithStore
- [04-02]: Break timeout 35 seconds (Windows default per MS-SMB2)
- [04-02]: Scan interval 1 second for balance of responsiveness and efficiency
- [04-02]: Session tracking map for break notification routing
- [04-03]: 35-second lease break timeout matches Windows MS-SMB2 default
- [04-03]: Polling-based lease break wait with 100ms interval
- [04-03]: OplockChecker interface in MetadataService for clean cross-protocol visibility

### Pending Todos

None.

### Blockers/Concerns

None.

## Next Steps

**Phase 4 COMPLETE - Ready for Phase 5:**
- Phase 5: Cross-Protocol Integration

## Session Continuity

Last session: 2026-02-05
Stopped at: Completed 04-03-PLAN.md - Phase 04 COMPLETE
Resume file: None
