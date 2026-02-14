---
phase: 09-state-management
plan: 02
subsystem: protocol
tags: [nfsv4, stateid, open-owner, state-management, seqid, replay-cache, rfc7530]

# Dependency graph
requires:
  - phase: 09-01
    provides: "StateManager with SETCLIENTID five-case algorithm and client record management"
provides:
  - "Stateid generation with type-tagged other field (boot epoch + counter)"
  - "OpenOwner tracking with seqid validation and replay caching"
  - "OpenState lifecycle: OpenFile, ConfirmOpen, CloseFile, DowngradeOpen"
  - "Stateid validation for READ/WRITE handlers (special stateid bypass)"
  - "RENEW handler validates client ID and updates LastRenewal timestamp"
  - "OPEN_DOWNGRADE handler with share mode subset validation"
affects: [09-03-lock-owners, 09-04-lease-management, 10-lock-operations]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "NFS4StateError type for handler error mapping"
    - "openOwnerKey composite key (clientID:hex(ownerData))"
    - "Stateid other field layout: type(1) + epoch(3) + counter(8)"
    - "SeqID validation three-way: OK/Replay/Bad"

key-files:
  created:
    - "internal/protocol/nfs/v4/state/stateid.go"
    - "internal/protocol/nfs/v4/state/openowner.go"
    - "internal/protocol/nfs/v4/state/stateid_test.go"
    - "internal/protocol/nfs/v4/handlers/state_integration_test.go"
  modified:
    - "internal/protocol/nfs/v4/state/manager.go"
    - "internal/protocol/nfs/v4/state/client.go"
    - "internal/protocol/nfs/v4/handlers/open.go"
    - "internal/protocol/nfs/v4/handlers/close.go"
    - "internal/protocol/nfs/v4/handlers/read.go"
    - "internal/protocol/nfs/v4/handlers/write.go"
    - "internal/protocol/nfs/v4/handlers/renew.go"
    - "internal/protocol/nfs/v4/handlers/stubs.go"
    - "internal/protocol/nfs/v4/handlers/stubs_test.go"

key-decisions:
  - "Stateid other field uses 1-byte type tag + 3-byte boot epoch + 8-byte atomic counter for uniqueness across types and server restarts"
  - "Special stateids (all-zeros, all-ones) bypass validation and CloseFile for backward compatibility"
  - "OpenOwner keyed by composite string (clientID:hex(ownerData)) for map efficiency"
  - "SeqID wrap-around at 0xFFFFFFFF goes to 1 (not 0) per RFC 7530 since 0 is reserved"
  - "NFS4StateError carries the NFS4 status code for direct handler mapping"

patterns-established:
  - "mapOpenStateError dispatches NFS4StateError.Status then falls back to mapStateError for generic errors"
  - "CacheOpenResult pattern: handler caches XDR-encoded result data after encoding for replay detection"
  - "CloseFile gracefully handles special stateids by returning zeroed stateid immediately"

# Metrics
duration: 12min
completed: 2026-02-13
---

# Phase 9 Plan 2: Stateid and Open-State Tracking Summary

**Stateid generation with type-tagged other field, OpenOwner seqid validation with replay caching, and full OPEN/CLOSE/CONFIRM/DOWNGRADE state lifecycle integrated into handlers**

## Performance

- **Duration:** 12 min
- **Started:** 2026-02-13T22:29:26Z
- **Completed:** 2026-02-13T22:41:28Z
- **Tasks:** 5
- **Files modified:** 13

## Accomplishments
- Replaced all stub/random stateids with tracked state via StateManager (OPEN, CLOSE, OPEN_CONFIRM, OPEN_DOWNGRADE)
- Stateid validation in READ/WRITE rejects bad/old/stale stateids while allowing special stateids
- Full end-to-end integration test: SETCLIENTID -> CONFIRM -> OPEN -> CONFIRM -> WRITE -> READ -> RENEW -> CLOSE
- OpenOwner seqid validation with three-way dispatch (OK/Replay/Bad) and wrap-around at 0xFFFFFFFF
- Share accumulation on same-file OPEN and downgrade subset validation

## Task Commits

Each task was committed atomically:

1. **Task 1: Stateid generation, OpenState, OpenOwner** - `3ada070` (feat)
2. **Task 2: Upgrade OPEN/CLOSE/OPEN_CONFIRM/OPEN_DOWNGRADE handlers** - `56a284a` (feat)
3. **Task 3: Stateid validation in READ/WRITE** - `a56d7c0` (feat)
4. **Task 4: RENEW handler with client validation** - `fde1e94` (feat)
5. **Task 5: End-to-end integration tests** - `233f71c` (test)

## Files Created/Modified

**Created:**
- `internal/protocol/nfs/v4/state/stateid.go` - Stateid generation (type tag + epoch + counter), validation (seqid, epoch, FH match), NFS4StateError type
- `internal/protocol/nfs/v4/state/openowner.go` - OpenOwner with seqid validation, OpenState tracking, CachedResult for replay, share accumulation
- `internal/protocol/nfs/v4/state/stateid_test.go` - 30+ tests: generation uniqueness, epoch validation, seqid checks, full lifecycle, RENEW
- `internal/protocol/nfs/v4/handlers/state_integration_test.go` - 3 integration tests: full lifecycle, RENEW unknown, confirmed owner skip

**Modified:**
- `internal/protocol/nfs/v4/state/manager.go` - Added openStateByOther/openOwners maps, OpenFile/ConfirmOpen/CloseFile/DowngradeOpen/RenewLease methods
- `internal/protocol/nfs/v4/state/client.go` - Added LastRenewal field, removed old OpenOwner placeholder
- `internal/protocol/nfs/v4/handlers/open.go` - Replaced random stateids with StateManager.OpenFile, added replay handling
- `internal/protocol/nfs/v4/handlers/close.go` - Replaced zeroed-stateid stub with StateManager.CloseFile
- `internal/protocol/nfs/v4/handlers/read.go` - Added stateid validation via ValidateStateid
- `internal/protocol/nfs/v4/handlers/write.go` - Added stateid validation via ValidateStateid
- `internal/protocol/nfs/v4/handlers/renew.go` - Replaced stub with StateManager.RenewLease validation
- `internal/protocol/nfs/v4/handlers/stubs.go` - Implemented OPEN_DOWNGRADE via StateManager.DowngradeOpen
- `internal/protocol/nfs/v4/handlers/stubs_test.go` - Updated OPEN_DOWNGRADE test for BAD_STATEID behavior, added NoCurrentFH test

## Decisions Made

- **Stateid other layout**: type(1) + epoch_low24(3) + counter(8) = 12 bytes. Enables fast stale detection without map lookup, and type identification for future lock/delegation stateids.
- **Special stateid handling in CloseFile**: Returns zeroed stateid immediately without error, maintaining backward compatibility with existing tests and clients that pass anonymous stateids.
- **NFS4StateError type**: Carries the NFS4 status code directly, so mapOpenStateError can extract it without a switch on error messages. Falls back to mapStateError for generic errors.
- **SeqID wrap-around to 1 (not 0)**: RFC 7530 reserves seqid 0 for special stateids, so the counter wraps from 0xFFFFFFFF to 1.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Updated OPEN_DOWNGRADE test expectation**
- **Found during:** Task 2 (Handler upgrades)
- **Issue:** Old test expected NFS4ERR_NOTSUPP; new implementation returns NFS4ERR_BAD_STATEID for unknown stateids
- **Fix:** Changed test to expect BAD_STATEID; added NoCurrentFH test
- **Files modified:** internal/protocol/nfs/v4/handlers/stubs_test.go
- **Verification:** All handler tests pass
- **Committed in:** 56a284a (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Test expectation update was necessary since OPEN_DOWNGRADE now performs real state validation instead of returning NOTSUPP.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Stateid and open-state infrastructure complete; ready for lock-owner tracking (09-03)
- Lock stateids will use StateTypeLock=0x02 in the type tag byte
- Lease management (09-04) can add expiry checking to ValidateStateid and RenewLease
- Grace period (09-05) can use the confirmed/unconfirmed state for CLAIM_PREVIOUS

## Self-Check: PASSED

All 13 claimed files verified present. All 5 task commits verified in git log.

---
*Phase: 09-state-management*
*Completed: 2026-02-13*
