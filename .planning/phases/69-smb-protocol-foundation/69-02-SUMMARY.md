---
phase: 69-smb-protocol-foundation
plan: 02
subsystem: protocol
tags: [smb2, credits, sequence-window, bitmap, flow-control, ms-smb2]

# Dependency graph
requires:
  - phase: 69-smb-protocol-foundation
    provides: "Session package with CreditConfig, CalculateCreditCharge, Manager"
provides:
  - "CommandSequenceWindow: bitmap-based MessageId tracker with Consume/Grant/Size"
  - "IsCreditExempt: identifies credit-exempt commands (NEGOTIATE, first SESSION_SETUP, CANCEL)"
  - "ValidateCreditCharge: payload-based CreditCharge validation for READ/WRITE/IOCTL/QUERY_DIRECTORY"
  - "EffectiveCreditCharge: CreditCharge=0 to 1 normalization"
  - "GrantCredits minimum floor: never returns 0 credits"
affects: [69-03-PLAN, smb-request-pipeline, smb-credit-processing]

# Tech tracking
tech-stack:
  added: []
  patterns: ["bitmap sliding window for sequence tracking", "table-driven test pattern for validation"]

key-files:
  created:
    - internal/adapter/smb/session/sequence_window.go
    - internal/adapter/smb/session/sequence_window_test.go
    - internal/adapter/smb/session/credit_validation.go
    - internal/adapter/smb/session/credit_validation_test.go
  modified:
    - internal/adapter/smb/session/manager.go
    - internal/adapter/smb/session/manager_test.go

key-decisions:
  - "Bitmap uses absolute low/high watermarks rather than relative size for correct compaction"
  - "Grant capped at maxSize to prevent unbounded bitmap growth"
  - "NEGOTIATE exempt only when SessionID=0 (not for late re-negotiations)"

patterns-established:
  - "CommandSequenceWindow: bitmap sliding window with advanceLow compaction"
  - "Credit validation uses body offset parsing per MS-SMB2 section references"

requirements-completed: [SMB-02, SMB-03, SMB-04]

# Metrics
duration: 7min
completed: 2026-03-20
---

# Phase 69 Plan 02: Sequence Window & Credit Validation Summary

**Bitmap-based CommandSequenceWindow for MessageId tracking and credit charge validation helpers per MS-SMB2 3.3.5.2.3/3.3.5.2.5**

## Performance

- **Duration:** 7 min
- **Started:** 2026-03-20T16:08:18Z
- **Completed:** 2026-03-20T16:15:19Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- Implemented CommandSequenceWindow with bitmap-based sliding window that validates, consumes, and rejects duplicate/out-of-range MessageIds
- Added credit validation helpers (IsCreditExempt, ValidateCreditCharge, EffectiveCreditCharge) with per-command payload size extraction
- Enforced MinimumCreditGrant floor in Manager.GrantCredits to prevent client deadlock
- All 42 tests pass with race detector enabled, including concurrent stress test with 50 goroutines

## Task Commits

Each task was committed atomically:

1. **Task 1: CommandSequenceWindow** - `3f93d2dd` (feat)
2. **Task 2: Credit validation and minimum grant** - `27b2886a` (feat)

_TDD workflow: tests written first (RED), implementation to pass (GREEN)._

## Files Created/Modified
- `internal/adapter/smb/session/sequence_window.go` - Bitmap-based sliding window for MessageId tracking per MS-SMB2 3.3.1.1
- `internal/adapter/smb/session/sequence_window_test.go` - 13 tests including concurrent stress test
- `internal/adapter/smb/session/credit_validation.go` - IsCreditExempt, ValidateCreditCharge, EffectiveCreditCharge per MS-SMB2 3.3.5.1/3.3.5.2.5
- `internal/adapter/smb/session/credit_validation_test.go` - Table-driven tests for all validation paths
- `internal/adapter/smb/session/manager.go` - Added MinimumCreditGrant floor per MS-SMB2 3.3.1.2
- `internal/adapter/smb/session/manager_test.go` - Added TestManager_MinimumCreditGrant with extreme condition subtests

## Decisions Made
- Used absolute low/high watermark tracking rather than relative size offsets to avoid bitmap corruption during advanceLow compaction
- Window maxSize cap prevents unbounded bitmap growth (set to 2x MaxSessionCredits per MS-SMB2)
- NEGOTIATE is only exempt when SessionID=0, matching pre-auth semantics
- Body offset parsing extracts payload sizes directly from raw request bytes without full struct deserialization

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed bitmap position tracking in sequence window**
- **Found during:** Task 1 (CommandSequenceWindow implementation)
- **Issue:** Initial design using relative `size` field caused bitmap corruption when advanceLow removed consumed words. After consuming sequence 0, low watermark advanced by 64 but size was only 1, so subsequent grants targeted wrong positions.
- **Fix:** Replaced `size` field with explicit `high` watermark tracking absolute sequence numbers. Bitmap positions are now always relative to `low`, and `high` tracks the next grantable sequence.
- **Files modified:** internal/adapter/smb/session/sequence_window.go
- **Verification:** All 13 sequence window tests pass including concurrent stress test
- **Committed in:** 3f93d2dd (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Bug fix was necessary for correctness. No scope creep.

## Issues Encountered
None beyond the bitmap tracking bug fixed above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- CommandSequenceWindow and credit validation are standalone, tested building blocks
- Ready for Plan 03 to wire these into the SMB request processing pipeline
- Manager.GrantCredits now has the minimum floor guarantee needed by the dispatch layer

---
*Phase: 69-smb-protocol-foundation*
*Completed: 2026-03-20*
