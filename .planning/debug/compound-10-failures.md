---
status: awaiting_human_verify
trigger: "Fix 10 failing smbtorture compound tests"
created: 2026-03-25T00:00:00Z
updated: 2026-03-25T11:00:00Z
---

## Current Focus

hypothesis: CONFIRMED - 8 of 10 compound tests fixed; 2 remaining require DACL enforcement
test: Full smbtorture compound suite
expecting: 18/20 pass, 2 fail (related4, related7 need DACL enforcement)
next_action: User verifies the fixes

## Symptoms

expected: All 20 compound tests pass
actual: 10 pass, 10 fail
errors: See initial list in trigger
reproduction: ./run.sh --profile memory --filter smb2.compound
started: recent compound.go and response.go changes

## Eliminated

- hypothesis: compound-break, compound-padding, create-write-close have compound bugs
  evidence: They pass standalone, fail only after interim2/interim3 timeouts
  timestamp: 2026-03-25

- hypothesis: related4/related7 are compound processing bugs
  evidence: CREATE handler returns OK because DittoFS doesn't enforce DACLs
  timestamp: 2026-03-25

## Evidence

- timestamp: 2026-03-25
  checked: related5 test - IOCTL(invalid handle) + CLOSE(related)
  found: First command's FileID not extracted from request body for non-CREATE commands
  implication: Related commands couldn't inherit FileID, got INVALID_PARAMETER instead of FILE_CLOSED

- timestamp: 2026-03-25
  checked: related8 test - CREATE(nonexistent) + related commands
  found: When CREATE fails, lastCmdStatus not propagated to related commands
  implication: Related commands got INVALID_PARAMETER instead of OBJECT_NAME_NOT_FOUND

- timestamp: 2026-03-25
  checked: invalid2 test - compound with bogus SessionID
  found: Compound signing used per-response SessionID, not first command's session
  implication: Responses with unknown SessionID couldn't be signed, client rejected as ACCESS_DENIED

- timestamp: 2026-03-25
  checked: interim2 test - CHANGE_NOTIFY in middle of compound
  found: NOTIFY as non-last compound command blocks the compound indefinitely
  implication: Windows returns STATUS_INTERNAL_ERROR for non-last NOTIFY in compound

- timestamp: 2026-03-25
  checked: interim3 test - CREATE+CLOSE+CREATE+NOTIFY compound
  found: ProcessRequestWithInheritedFileID discarded FileID from related CREATE responses
  implication: NOTIFY inherited old (closed) handle instead of new handle from second CREATE

- timestamp: 2026-03-25
  checked: related4/related7 tests
  found: DittoFS CREATE handler doesn't enforce DACL-based access control
  implication: Cannot be fixed without implementing full DACL enforcement (major feature)

## Resolution

root_cause: |
  Five distinct bugs in compound request processing:
  1. First command's FileID not extracted from request body for non-CREATE commands
  2. Error status not propagated from failed commands to subsequent related commands (returned INVALID_PARAMETER instead)
  3. Compound response signing used per-command SessionID instead of falling back to first command's session
  4. CHANGE_NOTIFY in non-last compound position blocked indefinitely instead of returning INTERNAL_ERROR
  5. ProcessRequestWithInheritedFileID discarded FileID returned by related CREATE commands

fix: |
  1. Extract FileID from first command's body for non-CREATE first commands
  2. Track and propagate predecessor error status to related commands
  3. Fall back to first command's session for signing when sub-command session unknown
  4. Return STATUS_INTERNAL_ERROR for CHANGE_NOTIFY in non-last compound position
  5. Return FileID from ProcessRequestWithInheritedFileID and update lastFileID for related CREATE

verification: |
  18/20 compound tests pass. 2 remaining (related4, related7) require DACL enforcement.
  All unit tests pass.

files_changed:
  - internal/adapter/smb/compound.go
  - internal/adapter/smb/response.go
