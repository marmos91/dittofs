---
phase: 16-nfsv4-1-types-and-constants
plan: 05
subsystem: protocol
tags: [nfsv4.1, compound, dispatch, minorversion, stub-handlers, rfc8881]

# Dependency graph
requires:
  - phase: 16-nfsv4-1-types-and-constants
    plan: 02
    provides: Session operation types (ExchangeIdArgs, CreateSessionArgs, SequenceArgs, etc.) with Decode methods
  - phase: 16-nfsv4-1-types-and-constants
    plan: 03
    provides: Forward-channel operation types (LayoutGetArgs, GetDeviceInfoArgs, etc.) with Decode methods
provides:
  - V41OpHandler type for NFSv4.1 operation handlers with session context parameter
  - v41DispatchTable with arg-consuming stubs for all 19 v4.1 operations (OP 40-58)
  - COMPOUND minorversion routing (0 -> v4.0, 1 -> v4.1, 2+ -> MINOR_VERS_MISMATCH)
  - v4.0 operation fallback in v4.1 compounds (PUTFH, GETATTR, READ, WRITE, etc.)
  - v41StubHandler helper for creating typed arg-consuming stubs
  - Protocol CLAUDE.md v4.0/v4.1 coexistence reference documentation
affects: [17, 18, 19, 20, 21, 22, 23, 24, 25]

# Tech tracking
tech-stack:
  added: []
  patterns: [v41StubHandler factory with typed decoder closure, minorversion switch dispatch, dual dispatch table fallback]

key-files:
  created: []
  modified:
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/handlers/compound.go
    - internal/protocol/nfs/v4/handlers/compound_test.go
    - internal/protocol/CLAUDE.md

key-decisions:
  - "Extracted dispatchV40/dispatchV41 helper methods to avoid code duplication in ProcessCompound"
  - "v4.1 stubs use typed decoder closures (not io.Discard) to validate XDR args and prevent stream desync"
  - "v4.0 ops accessible from v4.1 compounds via fallback to opDispatchTable (per RFC 8881)"

patterns-established:
  - "v41StubHandler pattern: factory function takes opCode + decoder closure, returns V41OpHandler that decodes args and returns NOTSUPP"
  - "Dual dispatch table: v4.1 COMPOUND checks v41DispatchTable first, then falls back to opDispatchTable for v4.0 ops"
  - "Replace-stub pattern: real v4.1 handlers (Phases 17-24) replace stubs by reassigning v41DispatchTable entries in NewHandler"

requirements-completed: [SESS-05]

# Metrics
duration: 6min
completed: 2026-02-20
---

# Phase 16 Plan 05: COMPOUND v4.1 Dispatch Summary

**v4.1 COMPOUND minorversion routing with 19 arg-consuming stub handlers and dual dispatch table fallback to v4.0 operations**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-20T15:50:47Z
- **Completed:** 2026-02-20T15:57:04Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- COMPOUND dispatcher correctly routes minorversion 0 (v4.0), 1 (v4.1), and 2+ (MINOR_VERS_MISMATCH)
- All 19 v4.1 operations (OP 40-58) have typed arg-consuming stub handlers preventing XDR stream desync
- v4.0 operations work in v4.1 compounds via fallback to opDispatchTable
- Zero regressions in v4.0 behavior (all existing tests pass unchanged)
- Protocol CLAUDE.md updated with comprehensive v4.0/v4.1 coexistence reference

## Task Commits

Each task was committed atomically:

1. **Task 1: Add v4.1 dispatch table and COMPOUND minorversion branch** - `7a53326d` (feat)
2. **Task 2: Update protocol CLAUDE.md with v4.0/v4.1 coexistence conventions** - `8368a533` (docs)

## Files Created/Modified
- `internal/protocol/nfs/v4/handlers/handler.go` - Added V41OpHandler type, v41DispatchTable field, v41StubHandler factory, 19 stub registrations
- `internal/protocol/nfs/v4/handlers/compound.go` - Refactored ProcessCompound with minorversion switch, extracted dispatchV40/dispatchV41 helpers
- `internal/protocol/nfs/v4/handlers/compound_test.go` - Updated minorversion=1 test, added 8 new v4.1 dispatch tests
- `internal/protocol/CLAUDE.md` - Added NFSv4.0/v4.1 Coexistence section with routing, handler types, dispatch strategy, and adding-handler guide

## Decisions Made
- Extracted dispatchV40/dispatchV41 as separate methods rather than using a single parameterized loop, because the v4.1 path needs V41RequestContext and dual-table lookup which makes the logic sufficiently different
- Used typed decoder closures in v41StubHandler (var args T; args.Decode(r)) rather than a generic skip, to validate XDR structure and catch malformed requests early
- v4.0 ops accessible from v4.1 compounds without duplication, following RFC 8881 which allows PUTFH, GETATTR, READ, etc. in v4.1 COMPOUNDs

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Codebase ready for Phase 17 (slot table) to start replacing stubs with real handlers
- v41StubHandler pattern provides clear upgrade path: replace stub in NewHandler with real V41OpHandler
- All v4.1 type Decode methods verified working through stub arg consumption

---
*Phase: 16-nfsv4-1-types-and-constants*
*Completed: 2026-02-20*
