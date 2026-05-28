---
phase: 13-nfsv4-acls
plan: 01
subsystem: metadata
tags: [acl, nfsv4, rfc7530, access-control, inheritance, permissions]

# Dependency graph
requires: []
provides:
  - "ACE/ACL types with all RFC 7530 Section 6 constants"
  - "Process-first-match ACL evaluation engine"
  - "Canonical ordering validation (strict Windows order)"
  - "Mode-ACL synchronization (DeriveMode, AdjustACLForMode)"
  - "ACL inheritance computation (ComputeInheritedACL, PropagateACL)"
affects: [13-02, 13-03, 13-04, 13-05]

# Tech tracking
tech-stack:
  added: []
  patterns: [process-first-match-evaluation, canonical-ordering-validation, mode-acl-sync]

key-files:
  created:
    - pkg/metadata/acl/types.go
    - pkg/metadata/acl/evaluate.go
    - pkg/metadata/acl/validate.go
    - pkg/metadata/acl/mode.go
    - pkg/metadata/acl/inherit.go
    - pkg/metadata/acl/CLAUDE.md
  modified: []

key-decisions:
  - "Zero requestedMask returns true (vacuously allowed) -- checked before nil/empty ACL"
  - "AUDIT/ALARM ACEs stored and returned only, skipped during evaluation"
  - "Canonical ordering uses four-bucket system: explicit deny > explicit allow > inherited deny > inherited allow"
  - "DeriveMode only considers ALLOW ACEs of special identifiers (DENY/AUDIT/ALARM ignored)"
  - "AdjustACLForMode preserves non-rwx mask bits (READ_ACL, WRITE_ACL, DELETE, SYNCHRONIZE)"
  - "PropagateACL replaces inherited ACEs while preserving explicit ACEs in canonical order"

patterns-established:
  - "Process-first-match: ACEs evaluated in order, each bit decided once (no override)"
  - "INHERIT_ONLY skipping: ACEs with this flag are skipped during evaluation but inherited by children"
  - "Dynamic special identifiers: OWNER@/GROUP@/EVERYONE@ resolved at evaluation time, not storage time"
  - "Protocol-agnostic ACL package: no imports from internal/protocol or pkg/metadata"

# Metrics
duration: 9min
completed: 2026-02-16
---

# Phase 13 Plan 01: ACL Types and Evaluation Engine Summary

**Pure ACL package with RFC 7530 process-first-match evaluation, canonical ordering validation, mode-ACL synchronization, and four-flag inheritance computation**

## Performance

- **Duration:** 9 min
- **Started:** 2026-02-16T08:15:27Z
- **Completed:** 2026-02-16T08:24:33Z
- **Tasks:** 2
- **Files created:** 11 (6 source + 5 test)

## Accomplishments
- Complete RFC 7530 Section 6 ACE/ACL type system with all 4 ACE types, 7 flag constants, 16 mask bits, and 3 special identifiers
- Process-first-match ACL evaluation engine with INHERIT_ONLY skipping, dynamic OWNER@/GROUP@/EVERYONE@ resolution, and early termination
- Canonical ordering validation enforcing strict Windows ordering with AUDIT/ALARM flexibility
- Bidirectional mode-ACL synchronization preserving explicit user/group ACEs during chmod
- Full inheritance computation supporting FILE_INHERIT, DIRECTORY_INHERIT, NO_PROPAGATE, INHERIT_ONLY flags
- PropagateACL for recursive propagation replacing inherited ACEs while preserving explicit ACEs

## Task Commits

Each task was committed atomically:

1. **Task 1: ACL Types, Evaluation Engine, and Validation** - `866b1b7` (feat)
2. **Task 2: Mode-ACL Synchronization and Inheritance** - `ec9b400` (feat)

## Files Created/Modified
- `pkg/metadata/acl/types.go` - ACE/ACL types, all constants, helper methods
- `pkg/metadata/acl/types_test.go` - JSON round-trip, helper methods, constant verification
- `pkg/metadata/acl/evaluate.go` - EvaluateContext, Evaluate, aceMatchesWho
- `pkg/metadata/acl/evaluate_test.go` - 16 test cases covering all evaluation scenarios
- `pkg/metadata/acl/validate.go` - ValidateACL, ValidateACE, canonical ordering
- `pkg/metadata/acl/validate_test.go` - 11 test cases covering validation scenarios
- `pkg/metadata/acl/mode.go` - DeriveMode, AdjustACLForMode, rwx helpers
- `pkg/metadata/acl/mode_test.go` - 12 test cases including round-trip verification
- `pkg/metadata/acl/inherit.go` - ComputeInheritedACL, PropagateACL
- `pkg/metadata/acl/inherit_test.go` - 14 test cases covering all inheritance flags
- `pkg/metadata/acl/CLAUDE.md` - Package conventions and gotchas

## Decisions Made
- Zero requestedMask returns true (vacuously allowed) before checking nil/empty ACL
- AUDIT/ALARM ACEs are store-only per project decision, skipped during evaluation
- Mode derivation only considers ALLOW ACEs from OWNER@/GROUP@/EVERYONE@
- AdjustACLForMode preserves all non-rwx mask bits (READ_ACL, WRITE_ACL, DELETE, etc.)
- PropagateACL maintains canonical ordering (explicit first, then inherited)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed zero mask evaluation order**
- **Found during:** Task 1 (Evaluation Engine)
- **Issue:** Zero requestedMask check was after nil/empty ACL check, causing empty ACL with zero mask to return false instead of true
- **Fix:** Moved zero mask check before nil/empty ACL check
- **Files modified:** pkg/metadata/acl/evaluate.go
- **Verification:** TestEvaluate_ZeroMaskAllowed passes
- **Committed in:** 866b1b7 (Task 1 commit)

**2. [Rule 1 - Bug] Fixed uint32 type mismatches in mode tests**
- **Found during:** Task 2 (Mode tests)
- **Issue:** OR-ing multiple constants produced untyped int values that couldn't compare to uint32 AccessMask
- **Fix:** Added explicit uint32() casts on constant expressions in test comparisons
- **Files modified:** pkg/metadata/acl/mode_test.go
- **Verification:** All mode tests pass
- **Committed in:** ec9b400 (Task 2 commit)

---

**Total deviations:** 2 auto-fixed (2 bugs)
**Impact on plan:** Both auto-fixes necessary for correctness. No scope creep.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- ACL package is complete and fully tested (53+ test cases, all passing with -race)
- Package has zero external dependencies (protocol-agnostic)
- Ready for Plan 13-02 (metadata store integration) and Plan 13-03 (NFS handler integration)

## Self-Check: PASSED

All 11 files verified present. Both commit hashes (866b1b7, ec9b400) verified in git log.

---
*Phase: 13-nfsv4-acls*
*Completed: 2026-02-16*
