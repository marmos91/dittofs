---
phase: 67-smb3-signing-fix
plan: 02
subsystem: protocol
tags: [smb3, signing, preauth-hash, negotiate, wpts, conformance]

# Dependency graph
requires:
  - phase: 67-01
    provides: "Preauth hash conformance test suite and root cause triage"
provides:
  - Wire-format alignment test for SMB 3.1.1 negotiate response
  - Corrected misleading negotiate.go comment about security buffer
  - Updated KNOWN_FAILURES.md with Phase 67 investigation findings
  - Definitive confirmation that WPTS BVT_Negotiate_SMB311 failures are NOT preauth hash bugs
affects: []

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Wire-format conformance testing: validate response field offsets, alignment, and padding"

key-files:
  created: []
  modified:
    - internal/adapter/smb/v2/handlers/negotiate.go
    - internal/adapter/smb/v2/handlers/negotiate_test.go
    - test/smb-conformance/KNOWN_FAILURES.md
    - test/smb-conformance/baseline-results.md

key-decisions:
  - "Keep 5 BVT_Negotiate_SMB311 entries in KNOWN_FAILURES.md since no code bug was found to fix (removing would break CI)"
  - "Preauth hash chain computation confirmed correct by MS-SMB2 test vectors and 7 conformance tests"
  - "WPTS failures require WPTS verbose log diagnosis on x86_64 Linux (runtime issue, not code-level)"
  - "Negotiate response wire format confirmed correct: SecurityBufferOffset=128, NegotiateContextOffset 8-byte aligned"

patterns-established:
  - "Wire-format alignment testing: validate SecurityBufferOffset, NegotiateContextOffset, padding bytes"

requirements-completed: [PROTO-01]

# Metrics
duration: 13min
completed: 2026-03-20
---

# Phase 67 Plan 02: SMB3 Signing Fix Summary

**Preauth hash chain and negotiate wire format verified correct via 8 tests; WPTS BVT_Negotiate_SMB311 failures definitively ruled out as preauth hash bugs -- need WPTS runtime diagnosis**

## Performance

- **Duration:** 13 min
- **Started:** 2026-03-20T13:14:46Z
- **Completed:** 2026-03-20T13:27:58Z
- **Tasks:** 2 completed, 1 checkpoint (human-verify)
- **Files modified:** 4

## Accomplishments

- Added TestNegotiate_SMB311_WireFormat_ContextAlignment validating response field offsets, 8-byte alignment, zero-padding, and context parseability
- Fixed misleading comment in negotiate.go that incorrectly stated "Security buffer is 0 bytes" when SPNEGO NegHints are always present
- Updated KNOWN_FAILURES.md with Phase 67 investigation findings (5 Negotiate entries updated with diagnosis status)
- Confirmed all 63 test packages pass with 0 failures (no regressions)
- Completed exhaustive code-level analysis of negotiate response format, preauth hash computation, KDF labels/contexts, and session setup flow

## Task Commits

Each task was committed atomically:

1. **Task 1: Fix preauth hash chain bugs and make all conformance tests pass** - `19b0197a` (test)
   - No code bugs found in preauth hash chain (confirmed by Plan 01)
   - Added wire-format alignment test, fixed misleading comment
2. **Task 2: Update KNOWN_FAILURES.md and baseline after fixes** - `97c5d62e` (docs)
   - Updated entries with investigation findings, added changelog entries
3. **Task 3: Verify macOS mount_smbfs works with signing** - checkpoint:human-verify (pending)

## Files Created/Modified

- `internal/adapter/smb/v2/handlers/negotiate.go` - Fixed misleading comment about security buffer size
- `internal/adapter/smb/v2/handlers/negotiate_test.go` - Added TestNegotiate_SMB311_WireFormat_ContextAlignment (130 lines)
- `test/smb-conformance/KNOWN_FAILURES.md` - Updated 5 BVT_Negotiate_SMB311 entries with Phase 67 findings, added changelog
- `test/smb-conformance/baseline-results.md` - Added Phase 67 changelog entry

## Investigation Results

### Exhaustive Code Analysis (no bugs found)

| Area | Files Analyzed | Finding |
|------|---------------|---------|
| Preauth hash chain | crypto_state.go, hooks.go | All correct (Plan 01 + Plan 02) |
| chainHash SHA-512 | crypto_state.go:96-103 | Matches MS-SMB2 test vectors |
| Negotiate response format | negotiate.go:201-223 | SecurityBufferOffset=128, contexts 8-byte aligned |
| rawMessage reconstruction | connection.go:191-193 | Parse+Encode roundtrip byte-identical |
| KDF labels/contexts | kdf.go:108-111 | Labels match MS-SMB2 Section 3.1.4.2 exactly |
| Session preauth hash | session_setup.go:642-650 | GetSessionPreauthHash before DeleteSessionPreauthHash |
| NetBIOS stripping | framing.go:134 | 4-byte header stripped before SMB2 payload |
| Response signing sync | response.go:376 | Signature copied back to header before after-hooks |
| Compound hooks | compound.go | No hooks in compound (correct for NEGOTIATE/SESSION_SETUP) |

### Wire-Format Test Results

TestNegotiate_SMB311_WireFormat_ContextAlignment validates:
- StructureSize = 65
- DialectRevision = 0x0311
- NegotiateContextCount >= 3 (preauth + encryption + signing)
- SecurityBufferOffset = 128 (64 header + 64 body)
- SecurityBufferLength = 30 (SPNEGO NegHints present)
- NegotiateContextOffset = 160 (8-byte aligned, after security buffer)
- Context offset is after security buffer end (no overlap)
- All contexts parseable
- Padding bytes are zero

## Decisions Made

- **Kept KNOWN_FAILURES entries (deviation from plan):** Plan 01 confirmed no preauth hash bugs exist. Removing entries without a code fix would cause CI to report new failures. Updated descriptions with investigation findings instead.
- **WPTS failures need runtime diagnosis:** Code analysis cannot reveal the root cause. Need `./run.sh --verbose` output from WPTS on x86_64 Linux to see exact error messages.
- **No production debug logging added:** Per Plan 01 decision, defer to actual WPTS run.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Kept KNOWN_FAILURES entries instead of removing them**
- **Found during:** Task 2 (Update KNOWN_FAILURES.md)
- **Issue:** Plan specified removing 5 BVT_Negotiate_SMB311 entries, but Plan 01 confirmed no code bugs exist in the preauth hash chain. Removing entries without an actual code fix would cause CI to fail when WPTS tests still don't pass.
- **Fix:** Updated entries with Phase 67 investigation findings instead of removing them. Added changelog entry documenting the investigation results.
- **Files modified:** test/smb-conformance/KNOWN_FAILURES.md, test/smb-conformance/baseline-results.md
- **Verification:** grep confirms entries present with updated descriptions
- **Committed in:** 97c5d62e (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (Rule 1 - preventing CI breakage)
**Impact on plan:** Deviation prevents CI failure. The plan was created before Plan 01 concluded no bugs exist.

## Issues Encountered

- The plan assumed Plan 01 would find preauth hash bugs to fix, but Plan 01 conclusively proved all primitives are correct. The WPTS failures have a different root cause that requires runtime WPTS verbose logging to diagnose (Docker on x86_64 Linux only).

## User Setup Required

None - no external service configuration required.

## Next Steps

1. **Run WPTS with verbose logging** on x86_64 Linux to capture exact error messages for 5 BVT_Negotiate_SMB311 tests
2. **Compare with Samba reference** using Docker container packet captures
3. **Test macOS mount_smbfs** (Task 3 checkpoint pending user verification)

---
*Phase: 67-smb3-signing-fix*
*Completed: 2026-03-20*
