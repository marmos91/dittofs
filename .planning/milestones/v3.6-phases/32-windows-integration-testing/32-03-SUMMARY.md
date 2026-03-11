---
phase: 32-windows-integration-testing
plan: 03
subsystem: testing
tags: [windows, validation, checklist, wpts, conformance, documentation]

# Dependency graph
requires:
  - phase: 32-01
    provides: Protocol compatibility fixes (MxAc, QFid, FileInfoClass, capability flags)
  - phase: 32-02
    provides: smbtorture Docker infrastructure and KNOWN_FAILURES baseline
provides:
  - Windows 11 VM setup guide (UTM, VirtualBox, Hyper-V)
  - Formal versioned validation checklist (Explorer, cmd.exe, PowerShell, Office, VS Code, NFS)
  - Updated WPTS KNOWN_FAILURES.md with Phase 30-32 fix annotations
affects: [32.5-manual-verification, 44-smb3-conformance-testing]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Versioned validation checklist with pass/fail/skip columns for manual testing"
    - "Known-failure status tracking (Expected, Permanent, Potentially fixed)"

key-files:
  created:
    - docs/WINDOWS_TESTING.md
  modified:
    - test/smb-conformance/KNOWN_FAILURES.md

key-decisions:
  - "Checklist covers both mapped drives and UNC paths per user decision"
  - "Guest auth GPO documented for Windows 11 24H2 (registry and gpedit.msc)"
  - "KNOWN_FAILURES.md adds Status column without fabricating test names or pass counts"

patterns-established:
  - "Validation checklist versioned in docs/WINDOWS_TESTING.md, extensible for future milestones"
  - "KNOWN_FAILURES.md changelog tracks baseline evolution across phases"

requirements-completed: [WIN-09]

# Metrics
duration: 5min
completed: 2026-02-28
---

# Phase 32 Plan 03: Windows Validation Checklist and KNOWN_FAILURES Update Summary

**Windows 11 VM setup guide, formal SMB/NFS validation checklist (70+ test items), and WPTS KNOWN_FAILURES.md reconciled with Phase 30-32 fixes**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-28T07:09:53Z
- **Completed:** 2026-02-28T07:15:44Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- Created comprehensive Windows 11 VM setup guide covering UTM (ARM), VirtualBox, Hyper-V with networking and guest auth GPO configuration
- Built formal validation checklist with 70+ test items across 9 categories: connection, Explorer, cmd.exe, PowerShell, Office, VS Code, NFS, file sizes
- Updated WPTS KNOWN_FAILURES.md with Phase 30-32 improvement annotations, status classification, and permanently out-of-scope categories
- Documented all known limitations (no ADS, no Change Notify, no SMB3) with future phase references

## Task Commits

Each task was committed atomically:

1. **Task 1: Windows VM setup guide and formal validation checklist** - `77f43ba3` (docs)
2. **Task 2: Update WPTS KNOWN_FAILURES.md with current baseline** - `127f452f` (docs)

## Files Created/Modified
- `docs/WINDOWS_TESTING.md` - Windows 11 VM setup guide (UTM/VirtualBox/Hyper-V), SMB validation checklist (Explorer, cmd.exe, PowerShell, Office, VS Code), NFS client testing, known limitations, troubleshooting (382 lines)
- `test/smb-conformance/KNOWN_FAILURES.md` - Updated with Phase 30-32 fix annotations, Status column, permanently out-of-scope categories, changelog section

## Decisions Made
- Checklist covers both mapped drives (`net use Z:`) and UNC paths (`\\host\smbbasic`) per user decision
- Guest auth GPO configuration documented with both gpedit.msc and registry approaches for Windows 11 24H2
- KNOWN_FAILURES.md uses Status column (Expected/Permanent/Potentially fixed) without fabricating specific test names or pass/fail counts
- BVT_OpLockBreak marked as "Potentially fixed" after Phase 30 oplock break wiring fix

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 32 complete -- all 3 plans delivered
- Windows 11 validation checklist ready for Phase 32.5 manual testing
- KNOWN_FAILURES.md should be re-measured by running full WPTS BVT suite after Phase 30-32 fixes
- smbtorture baseline should be established after first run

## Self-Check: PASSED

All files verified present. Both task commits (77f43ba3, 127f452f) verified in git log.

---
*Phase: 32-windows-integration-testing*
*Completed: 2026-02-28*
