---
phase: 24-directory-delegations
plan: 02
subsystem: nfs
tags: [nfsv4.1, delegation, get-dir-delegation, delegreturn, settings, config-stack]

# Dependency graph
requires:
  - phase: 24-directory-delegations-01
    provides: "Directory delegation state model, GrantDirDelegation, notification batching, CB_NOTIFY encoders"
provides:
  - "GET_DIR_DELEGATION handler with GDD4_OK/GDD4_UNAVAIL response encoding"
  - "DELEGRETURN flush of pending directory notifications before acknowledgment"
  - "MaxDelegations and DirDelegBatchWindowMs wired through full config stack (store, API, client, CLI, watcher)"
affects: [24-directory-delegations-03]

# Tech tracking
tech-stack:
  added: []
  patterns: ["V41OpHandler with Bitmap4 notification mask decoding", "two-phase lock release for flush-before-return"]

key-files:
  created:
    - internal/protocol/nfs/v4/handlers/get_dir_delegation_handler.go
    - internal/protocol/nfs/v4/handlers/get_dir_delegation_handler_test.go
  modified:
    - pkg/controlplane/store/adapter_settings.go
    - internal/controlplane/api/handlers/adapter_settings.go
    - pkg/apiclient/adapter_settings.go
    - cmd/dfsctl/commands/adapter/settings.go
    - pkg/adapter/nfs/nfs_adapter_settings.go

key-decisions:
  - "GDD4_UNAVAIL is non-fatal: returned when limit reached or delegations disabled, does not fail COMPOUND"
  - "Bitmap4 notification mask extracted from first word of []uint32 slice, matching wire format"
  - "Validation ranges: max_delegations 0-1000000 (0=unlimited), dir_deleg_batch_window_ms 0-5000"

patterns-established:
  - "Two-phase DELEGRETURN for directory delegations: flush notifications with lock released, then re-acquire for removal"

requirements-completed: [DDELEG-01, DDELEG-03]

# Metrics
duration: 3min
completed: 2026-02-22
---

# Phase 24 Plan 02: GET_DIR_DELEGATION Handler and Config Stack Summary

**GET_DIR_DELEGATION handler with notification bitmask decode, DELEGRETURN flush, and full config stack for MaxDelegations/DirDelegBatchWindowMs**

## Performance

- **Duration:** 3 min
- **Started:** 2026-02-22T21:33:59Z
- **Completed:** 2026-02-22T21:37:08Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Implemented GET_DIR_DELEGATION V41OpHandler that grants directory delegations with notification bitmask, returning GDD4_OK with stateid/cookie/types or GDD4_UNAVAIL when limit reached
- DELEGRETURN for directory delegations now flushes pending notifications before acknowledging, using two-phase lock release to avoid deadlock with backchannel sender
- Wired MaxDelegations and DirDelegBatchWindowMs through the complete config stack: GORM store, REST API handlers, API client types, CLI flags/display, and settings watcher propagation to StateManager

## Task Commits

Each task was committed atomically:

1. **Task 1: GET_DIR_DELEGATION handler, DELEGRETURN flush, dispatch** - `5793406d` (feat)
2. **Task 2: Config full stack for MaxDelegations and DirDelegBatchWindowMs** - `9023fa48` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/handlers/get_dir_delegation_handler.go` - V41OpHandler: decode args, extract notifMask from Bitmap4, GrantDirDelegation, encode GDD4_OK/GDD4_UNAVAIL
- `internal/protocol/nfs/v4/handlers/get_dir_delegation_handler_test.go` - 7 tests: success, limit-reached, disabled, no-fh, bad-session, bad-xdr, delegreturn-flushes
- `pkg/controlplane/store/adapter_settings.go` - Added max_delegations and dir_deleg_batch_window_ms to Update and Reset maps
- `internal/controlplane/api/handlers/adapter_settings.go` - Added fields to Patch/Put/Response types, validation (0-1M, 0-5000ms), defaults, reset cases, response converter
- `pkg/apiclient/adapter_settings.go` - Added MaxDelegations and DirDelegBatchWindowMs to response and patch request types
- `cmd/dfsctl/commands/adapter/settings.go` - Added --max-delegations and --dir-deleg-batch-window-ms flags and Delegation display group
- `pkg/adapter/nfs/nfs_adapter_settings.go` - Added SetMaxDelegations and SetDirDelegBatchWindow calls in applyNFSSettings

## Decisions Made
- GDD4_UNAVAIL is non-fatal per RFC 8881: it does not fail the COMPOUND, just tells the client no delegation is available
- Notification mask is extracted from the first uint32 word of the Bitmap4 type, matching the wire format where notification types are bit flags in bitmap position 0
- Validation ranges chosen: max_delegations 0-1000000 (0 means unlimited), dir_deleg_batch_window_ms 0-5000 (0 disables batching, 5s max)

## Deviations from Plan

None - plan executed exactly as written. The dispatch table change and DELEGRETURN flush logic were already committed as part of plan 24-01/24-03, so Task 1 only needed the new handler and test files.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- GET_DIR_DELEGATION handler operational and tested
- Config stack complete: operators can tune MaxDelegations and batch window via API/CLI
- Ready for plan 24-03 (notification hooks in mutation handlers) which is already committed

## Self-Check: PASSED

All files exist. All commits verified: `5793406d` (Task 1), `9023fa48` (Task 2).

---
*Phase: 24-directory-delegations*
*Completed: 2026-02-22*
