---
phase: 09-state-management
plan: 03
subsystem: protocol
tags: [nfsv4, lease, timer, renew, state-management, rfc7530]

# Dependency graph
requires:
  - phase: 09-02
    provides: "Stateid generation, OpenOwner seqid tracking, OpenState lifecycle, ValidateStateid"
provides:
  - "LeaseState with timer-based expiration and renewal"
  - "ConfirmClientID creates lease timer for confirmed clients"
  - "onLeaseExpired cleans up all client state (open states, owners, records)"
  - "ValidateStateid checks lease expiry and implicitly renews on success"
  - "RENEW handler validates client ID and renews lease timer"
  - "WRITE handler checks share access mode (NFS4ERR_OPENMODE for read-only)"
  - "GETATTR returns configured lease_time from StateManager"
  - "StateManager.Shutdown stops all lease timers for graceful shutdown"
affects: [09-04-grace-period, 10-lock-operations]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "LeaseState with separate mu from StateManager to avoid timer callback deadlock"
    - "Implicit lease renewal inside ValidateStateid for READ-only clients (Pitfall 3)"
    - "attrs.SetLeaseTime for dynamic FATTR4_LEASE_TIME configuration"
    - "NFS4ERR_OPENMODE for write to read-only open state"

key-files:
  created:
    - "internal/protocol/nfs/v4/state/lease.go"
    - "internal/protocol/nfs/v4/state/lease_test.go"
  modified:
    - "internal/protocol/nfs/v4/state/manager.go"
    - "internal/protocol/nfs/v4/state/stateid.go"
    - "internal/protocol/nfs/v4/state/client.go"
    - "internal/protocol/nfs/v4/handlers/renew.go"
    - "internal/protocol/nfs/v4/handlers/write.go"
    - "internal/protocol/nfs/v4/handlers/getattr.go"
    - "internal/protocol/nfs/v4/attrs/encode.go"

key-decisions:
  - "LeaseState.mu is separate from StateManager.mu to prevent deadlock in timer callback"
  - "Timer callback calls sm.onLeaseExpired directly (no intermediate lock)"
  - "Implicit lease renewal in ValidateStateid prevents READ-only client expiry (Pitfall 3)"
  - "attrs.SetLeaseTime package-level setter for dynamic FATTR4_LEASE_TIME encoding"
  - "NFS4ERR_OPENMODE returned for WRITE on read-only open state"

patterns-established:
  - "LeaseState timer pattern: separate mutex + Stop() for clean shutdown"
  - "onLeaseExpired cleans up cascading: openStates -> openOwners -> client record"
  - "mapOpenStateError used for RENEW (dispatches NFS4StateError.Status for EXPIRED)"

# Metrics
duration: 6min
completed: 2026-02-13
---

# Phase 9 Plan 3: Lease Management Summary

**LeaseState with timer-based expiration, RENEW/implicit renewal in READ/WRITE, NFS4ERR_OPENMODE enforcement, and automatic state cleanup on client lease expiry**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-13T22:47:57Z
- **Completed:** 2026-02-13T22:54:44Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- LeaseState type with timer-based expiration, renewal, and thread-safe concurrent access
- Automatic cleanup of all client state (open states, open owners, client record) on lease expiry
- ValidateStateid implicitly renews leases for stateid-carrying operations (READ, WRITE)
- WRITE handler enforces NFS4ERR_OPENMODE for read-only opens
- GETATTR returns configured lease_time from StateManager instead of hardcoded constant
- 14 lease tests covering expiration, renewal, cleanup, concurrent access, shutdown

## Task Commits

Each task was committed atomically:

1. **Task 1: Implement LeaseState and integrate into StateManager** - `48feac5` (feat)
2. **Task 2: Upgrade RENEW, WRITE, GETATTR handlers for lease management** - `ca99747` (feat)

## Files Created/Modified

**Created:**
- `internal/protocol/nfs/v4/state/lease.go` - LeaseState with timer, Renew, Stop, IsExpired, RemainingTime
- `internal/protocol/nfs/v4/state/lease_test.go` - 14 tests: expiration, renewal, cleanup, concurrent, shutdown

**Modified:**
- `internal/protocol/nfs/v4/state/manager.go` - Added onLeaseExpired, GetLeaseDuration, Shutdown; updated ConfirmClientID and RenewLease
- `internal/protocol/nfs/v4/state/stateid.go` - ValidateStateid checks lease expiry and renews implicitly
- `internal/protocol/nfs/v4/state/client.go` - Added Lease field to ClientRecord
- `internal/protocol/nfs/v4/handlers/renew.go` - Uses mapOpenStateError for NFS4ERR_EXPIRED, logs failures at INFO
- `internal/protocol/nfs/v4/handlers/write.go` - Checks ShareAccess for write permission (NFS4ERR_OPENMODE)
- `internal/protocol/nfs/v4/handlers/getattr.go` - Reads lease_time from StateManager.GetLeaseDuration()
- `internal/protocol/nfs/v4/attrs/encode.go` - SetLeaseTime/GetLeaseTime for dynamic FATTR4_LEASE_TIME

## Decisions Made

- **Separate mutex for LeaseState**: LeaseState.mu is independent from StateManager.mu. The timer callback must NOT hold lease.mu when invoking sm.onLeaseExpired (which acquires sm.mu). This avoids lock ordering deadlocks.
- **Implicit renewal in ValidateStateid**: Per RFC 7530 Section 9.6, any operation using a stateid implicitly renews the client lease. This is implemented inside ValidateStateid rather than in each handler, centralizing the logic.
- **Package-level SetLeaseTime in attrs**: Rather than passing lease duration through every encode function signature, used a package-level variable set by the handler layer. This minimizes API churn on the encode functions.
- **NFS4ERR_OPENMODE for write on read-only open**: When a WRITE operation references a stateid that was opened with READ-only access, return NFS4ERR_OPENMODE per RFC 7530.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Lease management complete; ready for grace period (09-04 if planned) or lock operations (Phase 10)
- LeaseState timers automatically clean up disconnected clients after configured timeout
- StateManager.Shutdown() available for graceful server shutdown integration

## Self-Check: PASSED

All 9 claimed files verified present. All 2 task commits verified in git log.

---
*Phase: 09-state-management*
*Completed: 2026-02-13*
