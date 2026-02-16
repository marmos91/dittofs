---
phase: 14-control-plane-v2-0
plan: 06
subsystem: testing
tags: [integration-tests, sqlite, httptest, settings-watcher, netgroup, dns-cache, race-detection]

# Dependency graph
requires:
  - phase: 14-control-plane-v2-0
    provides: adapter settings CRUD, netgroup CRUD, settings watcher, netgroup access checking
provides:
  - 71 integration tests covering adapter settings, netgroups, settings watcher, and netgroup access
  - Test patterns for runtime component testing with SQLite in-memory stores
affects: [14-control-plane-v2-0]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Runtime test helper: addShareDirect() bypasses metadata store setup for unit testing"
    - "DNS cache test pattern: pre-populate cache entries to avoid real DNS lookups"
    - "Settings watcher test pattern: short poll interval (50ms) for fast integration tests"

key-files:
  created:
    - pkg/controlplane/runtime/settings_watcher_test.go
    - pkg/controlplane/runtime/netgroups_test.go
  modified:
    - pkg/controlplane/store/adapter_settings_test.go

key-decisions:
  - "Test hostname matching by pre-populating DNS cache rather than mocking net.LookupAddr"
  - "Removed duplicate createTestStore helper from adapter_settings_test.go (already defined in store_test.go)"

patterns-established:
  - "Runtime integration tests: create Runtime with real SQLite store, inject shares directly for isolated testing"
  - "DNS cache testing: set cache entries with known TTLs to test expiry and negative caching without real DNS"

# Metrics
duration: 10min
completed: 2026-02-16
---

# Phase 14 Plan 06: Control Plane v2.0 Tests Summary

**71 integration tests covering store CRUD, handler validation, settings watcher concurrency, and netgroup IP/CIDR/hostname access checking with DNS cache**

## Performance

- **Duration:** 10 min
- **Started:** 2026-02-16T15:49:43Z
- **Completed:** 2026-02-16T16:00:04Z
- **Tasks:** 2
- **Files modified:** 3

## Accomplishments
- 42 store and handler tests: adapter settings CRUD with version tracking, netgroup lifecycle with in-use protection, handler validation (PATCH/PUT/force/dry_run)
- 29 runtime tests: settings watcher polling with atomic swap and DB error resilience, netgroup access for IP/CIDR/hostname members, wildcard matching, DNS cache TTL behavior
- All tests pass with race detection enabled (-race flag)

## Task Commits

Each task was committed atomically:

1. **Task 1: Store and Handler Tests** - `ac34aef` (test)
2. **Task 2: Runtime Tests (Settings Watcher + Netgroup Access)** - `1fa9e27` (test)

## Files Created/Modified
- `pkg/controlplane/store/adapter_settings_test.go` - Fixed duplicate createTestStore; 23 tests for NFS/SMB adapter settings and netgroup store CRUD
- `internal/controlplane/api/handlers/adapter_settings_test.go` - 10 handler tests for GET/PATCH/PUT/reset with validation, force, dry_run
- `internal/controlplane/api/handlers/netgroups_test.go` - 9 handler tests for netgroup CRUD and member management
- `pkg/controlplane/runtime/settings_watcher_test.go` - 8 tests: load initial, change detection, no-op poll, atomic swap concurrency, clean stop, DB error resilience, defaults, nil-before-load
- `pkg/controlplane/runtime/netgroups_test.go` - 21 tests: CheckNetgroupAccess (IP/CIDR match/no-match, empty netgroup, mixed members, not found), matchHostname (exact, case-insensitive, wildcard, DNS failure, multi-PTR), DNS cache (defaults, TTLs, cache hits, expiry, negative cache, cleanup)

## Decisions Made
- Tested hostname matching by pre-populating the DNS cache with known entries rather than mocking net.LookupAddr, since matchHostname is a method on Runtime that delegates to dnsCache.lookupAddr
- Removed duplicate createTestStore function from adapter_settings_test.go (already defined in store_test.go within the same build tag)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed duplicate createTestStore function**
- **Found during:** Task 1 (Store tests)
- **Issue:** adapter_settings_test.go declared createTestStore which was already defined in store_test.go (same package, same build tag), causing compilation failure
- **Fix:** Removed the duplicate function declaration from adapter_settings_test.go
- **Files modified:** pkg/controlplane/store/adapter_settings_test.go
- **Verification:** `go test -tags=integration ./pkg/controlplane/store/... -count=1 -race` passes
- **Committed in:** ac34aef (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (Rule 1 - bug)
**Impact on plan:** Minor fix required for compilation. No scope creep.

## Issues Encountered
- macOS linker warning about malformed LC_DYSYMTAB in Go race detector builds. This is a known macOS toolchain issue and does not affect test correctness.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All control plane v2.0 test coverage is in place
- Store, handler, and runtime layers are fully tested
- Ready for plan 07 (final plan in phase 14)

## Self-Check: PASSED

All files verified present. All commit hashes verified in git log.

---
*Phase: 14-control-plane-v2-0*
*Completed: 2026-02-16*
