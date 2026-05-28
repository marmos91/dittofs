---
phase: 68-protocol-correctness-and-hot-reload
plan: 01
subsystem: auth
tags: [ntlm, smb, protocol-correctness, security-flags]

# Dependency graph
requires: []
provides:
  - "Corrected NTLM Type 2 challenge flags (no encryption capability mismatch)"
  - "Explicit absent-flag test assertions for FlagSeal, Flag128, Flag56"
affects: [smb-protocol, ntlm-auth]

# Tech tracking
tech-stack:
  added: []
  patterns: ["Intentional capability omission with explicit test assertions"]

key-files:
  created: []
  modified:
    - "internal/adapter/smb/auth/ntlm.go"
    - "internal/adapter/smb/auth/ntlm_test.go"

key-decisions:
  - "NTLM-level sealing (RC4) will never be implemented; SMB3 AES transport encryption is the only confidentiality path"
  - "Flag constants (Flag128, Flag56, FlagSeal) kept for parsing incoming client messages; only removed from server's Type 2 challenge"

patterns-established:
  - "Absent-flag testing: explicitly assert security-sensitive flags are NOT set, not just that expected flags are present"

requirements-completed: [PROTO-02]

# Metrics
duration: 3min
completed: 2026-03-20
---

# Phase 68 Plan 01: NTLM Encryption Flag Correction Summary

**Removed unimplemented Flag128/Flag56 from NTLM CHALLENGE_MESSAGE to prevent capability mismatch with strict clients**

## Performance

- **Duration:** 3 min
- **Started:** 2026-03-20T13:46:40Z
- **Completed:** 2026-03-20T13:49:56Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- BuildChallenge no longer advertises Flag128 (128-bit encryption) or Flag56 (56-bit encryption)
- New DoesNotAdvertiseEncryptionFlags subtest explicitly verifies FlagSeal, Flag128, and Flag56 are absent
- TODO comment about NTLM encryption replaced with intentional-omission documentation
- Full test suite passes with zero regressions (go build, go test, go vet all clean)

## Task Commits

Each task was committed atomically:

1. **Task 1 (RED): Add failing test for encryption flag absence** - `1a8115df` (test)
2. **Task 1 (GREEN): Remove encryption flags from BuildChallenge** - `c0cb716b` (feat)
3. **Task 2: Full test suite validation** - No code changes needed (validation only)

_Note: TDD task had RED and GREEN commits. No REFACTOR needed._

## Files Created/Modified
- `internal/adapter/smb/auth/ntlm.go` - Removed Flag128 and Flag56 from BuildChallenge flags expression; replaced TODO with intentional-omission comment
- `internal/adapter/smb/auth/ntlm_test.go` - Removed Flag128/Flag56 from HasExpectedFlags; added DoesNotAdvertiseEncryptionFlags subtest

## Decisions Made
- NTLM-level sealing (RC4) will not be implemented; SMB3 AES transport encryption is the chosen confidentiality path per PR #285
- Flag constant definitions preserved for parsing incoming Type 1 and Type 3 client messages

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Fixed pre-existing MetadataStoreName typo in share_hotreload_test.go**
- **Found during:** Task 1 GREEN commit (pre-commit hook blocked commit)
- **Issue:** `pkg/controlplane/runtime/share_hotreload_test.go:29` used `MetadataStoreName` but `ShareConfig` struct field is `MetadataStore`
- **Fix:** Pre-commit linter auto-corrected the field name
- **Files modified:** `pkg/controlplane/runtime/share_hotreload_test.go`
- **Verification:** `go vet ./...` passes clean
- **Committed in:** `c0cb716b` (part of GREEN commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Pre-existing issue in unrelated file was blocking all commits via pre-commit hook. Trivial field name fix, no scope creep.

## Issues Encountered
None beyond the pre-existing blocking issue documented above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- NTLM challenge flags are now correct; ready for Plan 02 (Kerberos keytab hot-reload)
- No blockers

---
*Phase: 68-protocol-correctness-and-hot-reload*
*Completed: 2026-03-20*
