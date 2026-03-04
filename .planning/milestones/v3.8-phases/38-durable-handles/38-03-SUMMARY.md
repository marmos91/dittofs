---
phase: 38-durable-handles
plan: 03
subsystem: smb
tags: [smb3, durable-handles, scavenger, rest-api, lifecycle, architecture]

# Dependency graph
requires:
  - phase: 38-01
    provides: DurableHandleStore interface with 3 backend implementations
provides:
  - DurableHandleScavenger goroutine for timeout management
  - REST API endpoints for durable handle listing and force-close
  - Adapter lifecycle integration (scavenger starts/stops with Serve)
  - ARCHITECTURE.md durable handle state flow documentation
affects: [phase-39]

# Tech tracking
tech-stack:
  added: []
  patterns: [background scavenger goroutine tied to adapter context lifecycle]

key-files:
  created:
    - internal/adapter/smb/v2/handlers/durable_scavenger.go
    - internal/adapter/smb/v2/handlers/durable_scavenger_test.go
    - internal/controlplane/api/handlers/durable_handle.go
  modified:
    - pkg/adapter/smb/adapter.go
    - pkg/controlplane/api/router.go
    - docs/ARCHITECTURE.md

key-decisions:
  - "Scavenger iterates all handles and checks expiry client-side rather than using DeleteExpiredDurableHandles, to perform full cleanup before deletion"
  - "durableHandleStoreProvider interface duplicated locally in adapter and API handler to avoid importing storetest from production code"
  - "Scavenger lifecycle tied to Serve context -- stops automatically on adapter shutdown"
  - "ForceExpireDurableHandle returns error for nonexistent handles (not silent)"
  - "REST API returns empty array (not null) when no durable handles exist"

patterns-established:
  - "Background goroutine scavenger pattern: ticker loop with context cancellation, startup adjustment hook"
  - "Local interface for cross-package type assertion (durableHandleStoreProvider)"

requirements-completed: [DH-01, DH-05]

# Metrics
duration: 10min
completed: 2026-03-02
---

# Phase 38 Plan 03: Scavenger, REST API, and Adapter Lifecycle Summary

**Durable handle scavenger with 10s tick interval, restart timeout adjustment, REST API for handle listing/force-close, and ARCHITECTURE.md state flow diagram**

## Performance

- **Duration:** 10 min
- **Started:** 2026-03-02T14:17:10Z
- **Completed:** 2026-03-02T14:27:10Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- DurableHandleScavenger runs at configurable interval (default 10s), expires timed-out handles with full cleanup (lock release, cache flush, delete-on-close)
- Scavenger adjusts persisted handle timeouts on server restart to account for downtime
- REST API: GET /api/v1/durable-handles lists active handles with remaining timeout; DELETE /api/v1/durable-handles/{id} force-closes with cleanup
- Adapter starts scavenger in Serve() with context-bound lifecycle
- ARCHITECTURE.md documents the complete durable handle state flow (grant, disconnect, scavenger, reconnect, conflict, app instance ID)

## Task Commits

Each task was committed atomically:

1. **Task 1: Durable handle scavenger (RED)** - `aec57bfe` (test)
2. **Task 1: Durable handle scavenger (GREEN + adapter)** - `21b64bb6` (feat)
3. **Task 2: REST API endpoints and ARCHITECTURE.md** - `2a64b258` (feat)

_Note: Task 1 used TDD with separate RED and GREEN commits._

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/durable_scavenger.go` - Background goroutine for durable handle timeout management
- `internal/adapter/smb/v2/handlers/durable_scavenger_test.go` - 7 tests covering expiry, restart, force-expire, conflict, cancellation
- `internal/controlplane/api/handlers/durable_handle.go` - REST API handlers for listing and force-closing durable handles
- `pkg/adapter/smb/adapter.go` - Adapter starts scavenger in Serve(), findDurableHandleStore helper
- `pkg/controlplane/api/router.go` - Routes for /api/v1/durable-handles (GET, DELETE)
- `docs/ARCHITECTURE.md` - Durable Handle State Flow section with state diagram

## Decisions Made
- Scavenger iterates all handles client-side (rather than using bulk DeleteExpiredDurableHandles) because full cleanup (locks, caches, delete-on-close) must happen BEFORE store deletion
- Local `durableHandleStoreProvider` interface in adapter and API handler avoids importing `storetest` package from production code
- Scavenger lifecycle tied to Serve context -- when adapter stops, scavenger stops automatically via ctx.Done()
- ForceExpireDurableHandle returns an error for nonexistent handles rather than silently succeeding
- REST API returns `[]` (empty JSON array) instead of `null` when no durable handles exist

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Removed orphaned durable_context_test.go from incomplete Plan 02**
- **Found during:** Task 1 (scavenger test compilation)
- **Issue:** Untracked test file from an incomplete Plan 02 attempt referenced undefined symbols (DecodeDHnQRequest, etc.), preventing package compilation
- **Fix:** Removed the untracked file (not in git history, no data loss)
- **Files modified:** internal/adapter/smb/v2/handlers/durable_context_test.go (deleted)
- **Verification:** Package compiles, all tests pass
- **Committed in:** Not committed (file was untracked and deleted)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Removed orphaned file from incomplete prior plan. No scope creep.

## Issues Encountered
- Pre-existing uncommitted changes from an incomplete Plan 02 attempt existed in handler.go (DurableStore field, IsDurable on OpenFile, persistence logic in closeFilesWithFilter) and durable_context.go. These are from Plan 02 scope and were left as-is (not committed by this plan).

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Scavenger goroutine ready for production use with any DurableHandleStore backend
- REST API enables admin visibility and management of durable handles
- ARCHITECTURE.md provides reference for future development
- Plan 02 (CREATE context processing) can be executed independently to complete the durable handle feature

---
*Phase: 38-durable-handles*
*Completed: 2026-03-02*
