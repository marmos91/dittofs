---
phase: 69-smb-protocol-foundation
plan: 03
subsystem: protocol
tags: [smb2, credit-management, sequence-window, compound-requests, flow-control]

# Dependency graph
requires:
  - phase: 69-02
    provides: "CommandSequenceWindow, IsCreditExempt, ValidateCreditCharge, EffectiveCreditCharge"
provides:
  - "Fully enforced credit flow control for all SMB clients"
  - "Compound-level credit accounting per MS-SMB2 3.2.4.1.4"
  - "Sequence window expansion on every response path"
  - "SupportsMultiCredit auto-detection via NEGOTIATE after-hook"
affects: [smb-conformance, smb-performance, smb-testing]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Credit validation pipeline: IsCreditExempt -> ValidateCreditCharge -> SequenceWindow.Consume before dispatch"
    - "Compound credit zeroing: middle responses grant 0, last response grants credits"
    - "Sequence window expansion via Grant() on every response path (success, error, encrypted, compound)"
    - "NEGOTIATE after-hook for dialect-dependent feature flags (SupportsMultiCredit)"

key-files:
  created:
    - "internal/adapter/smb/credit_wiring_test.go"
  modified:
    - "internal/adapter/smb/conn_types.go"
    - "internal/adapter/smb/response.go"
    - "internal/adapter/smb/compound.go"
    - "internal/adapter/smb/hooks.go"
    - "internal/adapter/smb/session/manager.go"
    - "pkg/adapter/smb/connection.go"

key-decisions:
  - "SequenceWindow initialized via factory function (NewSequenceWindowForConnection) to keep pkg/ clean"
  - "SupportsMultiCredit set via NEGOTIATE after-hook based on dialect >= 0x0210 (SMB 2.1)"
  - "Compound credit validation only for first command; sub-commands skip validation per MS-SMB2 3.2.4.1.4"
  - "Sequence window Grant deferred until after successful wire write (client gets credits when response is actually sent)"
  - "applyCompoundCreditZeroing as a separate function for testability"

patterns-established:
  - "Credit validation before dispatch: check exempt, validate charge, consume window"
  - "Compound-level credit accounting: first command charges, middle grant 0, last grants"
  - "After-hook pattern for connection state updates (multiCreditAfterHook)"

requirements-completed: [SMB-02, SMB-04, SMB-05]

# Metrics
duration: 11min
completed: 2026-03-20
---

# Phase 69 Plan 03: Credit Validation Wiring Summary

**Wired CommandSequenceWindow and credit charge validation into ProcessSingleRequest/ProcessCompoundRequest with compound-level credit accounting and sequence window expansion on every response path**

## Performance

- **Duration:** 11 min
- **Started:** 2026-03-20T16:22:40Z
- **Completed:** 2026-03-20T16:34:03Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Credit validation pipeline enforced for all non-exempt SMB requests before handler dispatch
- Compound requests validate credits at compound level (first command only), with middle responses granting 0 credits
- Sequence window expanded on every response path: success, error, encrypted, and compound
- SupportsMultiCredit auto-detected based on negotiated dialect via NEGOTIATE after-hook
- 17 new test cases covering exempt commands, consumption, replay protection, compound zeroing, and full pipeline flow

## Task Commits

Each task was committed atomically:

1. **Task 1: Add SequenceWindow to ConnInfo and wire credit validation into ProcessSingleRequest** - `d56cc8a7` (feat)
2. **Task 2: Wire compound-level credit accounting into ProcessCompoundRequest** - `3e08ac00` (feat)

## Files Created/Modified
- `internal/adapter/smb/credit_wiring_test.go` - 17 integration tests for credit validation wiring (single + compound)
- `internal/adapter/smb/conn_types.go` - Added SequenceWindow and SupportsMultiCredit fields to ConnInfo, plus factory function
- `internal/adapter/smb/response.go` - Credit validation before dispatch, sequence window expansion in SendMessage
- `internal/adapter/smb/compound.go` - Compound-level credit validation, middle credit zeroing, window expansion
- `internal/adapter/smb/hooks.go` - multiCreditAfterHook for SupportsMultiCredit based on dialect
- `internal/adapter/smb/session/manager.go` - Added Config() accessor method
- `pkg/adapter/smb/connection.go` - SequenceWindow initialization on connection creation

## Decisions Made
- Used `NewSequenceWindowForConnection` factory to avoid `pkg/adapter/smb` directly importing `session` package (maintains existing layering)
- SupportsMultiCredit set via NEGOTIATE after-hook (dialect >= 0x0210) rather than in handler to keep dialect awareness out of conn_types
- Sequence window Grant deferred until after successful WriteNetBIOSFrame to ensure client only gets credits when response is actually sent
- `applyCompoundCreditZeroing` extracted as separate function for testability (tested independently of full compound processing)
- Added `Manager.Config()` accessor (Rule 3 - Blocking: needed by factory function)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Added Manager.Config() accessor method**
- **Found during:** Task 1 (NewSequenceWindowForConnection factory)
- **Issue:** `session.Manager.config` field is unexported; factory function needs MaxSessionCredits
- **Fix:** Added `Config() CreditConfig` method to Manager
- **Files modified:** internal/adapter/smb/session/manager.go
- **Verification:** Build passes, factory function correctly reads MaxSessionCredits
- **Committed in:** d56cc8a7 (Task 1 commit)

**2. [Rule 1 - Bug] Handled encrypted response early-return in SendMessage**
- **Found during:** Task 1 (wiring sequence window Grant)
- **Issue:** SendMessage's encrypted path had early return via `return WriteNetBIOSFrame(...)` that bypassed the Grant logic
- **Fix:** Changed encrypted path to store write error, call Grant, then return error
- **Files modified:** internal/adapter/smb/response.go
- **Verification:** Both encrypted and unencrypted response paths expand the sequence window
- **Committed in:** d56cc8a7 (Task 1 commit)

---

**Total deviations:** 2 auto-fixed (1 blocking, 1 bug)
**Impact on plan:** Both auto-fixes necessary for correctness. No scope creep.

## Issues Encountered
- Pre-commit hooks prevent committing non-compilable test code (RED phase of TDD), so RED+GREEN were committed together

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Credit flow control fully enforced for all SMB2 clients
- Phase 69 (SMB Protocol Foundation) is now complete with all 3 plans executed
- Ready for conformance testing and performance validation

---
*Phase: 69-smb-protocol-foundation*
*Completed: 2026-03-20*
