---
phase: 63-cache-retention-model-and-eviction-policy
plan: 01
subsystem: blockstore
tags: [retention, cache, eviction, lru, ttl, pin, gorm]

# Dependency graph
requires: []
provides:
  - RetentionPolicy type (pin/ttl/lru) in pkg/blockstore/retention.go
  - Share GORM model with RetentionPolicy and RetentionTTL fields
  - ShareConfig and runtime Share structs carrying retention end-to-end
  - LoadSharesFromStore populating retention from DB
  - UpdateShare supporting runtime retention changes
affects: [63-02, 63-03]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "String-typed enum for GORM compatibility with ParseX/ValidateX pattern"
    - "Empty-string DB default with application-level fallback to LRU"

key-files:
  created:
    - pkg/blockstore/retention.go
    - pkg/blockstore/retention_test.go
  modified:
    - pkg/controlplane/models/share.go
    - pkg/controlplane/runtime/shares/service.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/init.go
    - pkg/controlplane/runtime/runtime_test.go
    - internal/controlplane/api/handlers/shares.go

key-decisions:
  - "RetentionPolicy as string type (not iota) for GORM/JSON compatibility"
  - "Empty string in DB defaults to LRU at application level for CACHE-06 backward compat"
  - "RetentionTTL stored as int64 seconds in DB, converted to time.Duration at application level"

patterns-established:
  - "Retention enum: ParseRetentionPolicy + ValidateRetentionPolicy pattern for safe parsing"
  - "DB model helper methods (GetRetentionPolicy/GetRetentionTTL) for type conversion"

requirements-completed: [CACHE-01, CACHE-06]

# Metrics
duration: 6min
completed: 2026-03-13
---

# Phase 63 Plan 01: Retention Policy Data Model Summary

**RetentionPolicy enum type (pin/ttl/lru) with Share GORM model fields and runtime config threading from DB to per-share BlockStore**

## Performance

- **Duration:** 6 min
- **Started:** 2026-03-13T12:56:15Z
- **Completed:** 2026-03-13T13:02:15Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Defined RetentionPolicy type with Pin/TTL/LRU constants, case-insensitive parser, and validator
- Added RetentionPolicy and RetentionTTL fields to Share GORM model with backward-compatible defaults
- Threaded retention config through runtime (ShareConfig -> Share -> BlockStore init log)
- Extended UpdateShare to support runtime retention policy changes without restart

## Task Commits

Each task was committed atomically:

1. **Task 1: Create RetentionPolicy type and Share model fields** - `e0db6297` (feat, TDD)
2. **Task 2: Thread retention config through runtime** - `b6397414` (feat)

_Note: Task 1 was TDD -- RED phase tests were folded into GREEN commit due to pre-commit hooks requiring compilable code._

## Files Created/Modified
- `pkg/blockstore/retention.go` - RetentionPolicy type, constants, ParseRetentionPolicy, ValidateRetentionPolicy
- `pkg/blockstore/retention_test.go` - Comprehensive test coverage for retention type (11 parse, 3 string, 5 valid, 8 validate tests)
- `pkg/controlplane/models/share.go` - Added RetentionPolicy/RetentionTTL GORM fields and helper methods
- `pkg/controlplane/runtime/shares/service.go` - Added retention fields to ShareConfig/Share, updated prepareShare and UpdateShare
- `pkg/controlplane/runtime/runtime.go` - Updated UpdateShare signature with retention params, added blockstore import
- `pkg/controlplane/runtime/init.go` - Populated retention from DB model in LoadSharesFromStore
- `pkg/controlplane/runtime/runtime_test.go` - Updated UpdateShare test callsites for new signature
- `internal/controlplane/api/handlers/shares.go` - Updated handler callsite with nil retention params

## Decisions Made
- Used `string` type for RetentionPolicy (not `iota`) for GORM column compatibility and JSON serialization
- Empty/NULL RetentionPolicy in DB defaults to LRU at application level (CACHE-06 backward compatibility)
- RetentionTTL stored as `int64` seconds in GORM (avoids GORM time.Duration serialization issues), converted to `time.Duration` via helper
- UpdateShare extended with optional `*blockstore.RetentionPolicy` and `*time.Duration` params (nil = no change)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- RetentionPolicy type and Share model fields ready for API/CLI exposure in plan 02
- Runtime config threading ready for eviction engine integration in plan 03
- GORM AutoMigrate will add new columns on next server start

---
*Phase: 63-cache-retention-model-and-eviction-policy*
*Completed: 2026-03-13*
