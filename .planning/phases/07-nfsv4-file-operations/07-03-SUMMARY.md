---
phase: 07-nfsv4-file-operations
plan: 03
subsystem: nfs
tags: [nfsv4, open, close, read, write, commit, stateid, rfc7530]

# Dependency graph
requires:
  - phase: 07-02
    provides: NFSv4 COMPOUND dispatcher, pseudo-fs, LOOKUP, GETATTR, CREATE, REMOVE handlers
provides:
  - NFSv4 Stateid4 type with encode/decode and special-stateid detection
  - OPEN handler (CLAIM_NULL with UNCHECKED4/GUARDED4/EXCLUSIVE4 create modes)
  - OPEN_CONFIRM handler (placeholder for Phase 9 state management)
  - CLOSE handler (accepts any stateid, returns zeroed stateid)
  - READ handler (PayloadService.ReadAt with EOF detection and COW support)
  - WRITE handler (two-phase PrepareWrite/WriteAt/CommitWrite pattern)
  - COMMIT handler (PayloadService.Flush with server boot verifier)
  - I/O test fixture with metadata + payload services for data tests
affects: [07-04-nfsv4-rename-setattr, 09-state-management]

# Tech tracking
tech-stack:
  added: []
  patterns: [stateid-placeholder-pattern, io-test-fixture-with-payload, server-boot-verifier]

key-files:
  created:
    - internal/protocol/nfs/v4/handlers/open.go
    - internal/protocol/nfs/v4/handlers/close.go
    - internal/protocol/nfs/v4/handlers/read.go
    - internal/protocol/nfs/v4/handlers/write.go
    - internal/protocol/nfs/v4/handlers/commit.go
    - internal/protocol/nfs/v4/handlers/io_test.go
  modified:
    - internal/protocol/nfs/v4/types/types.go
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/handlers/compound_test.go

key-decisions:
  - "Placeholder stateids for Phase 7: OPEN returns random stateid, all handlers accept any stateid"
  - "WRITE always returns UNSTABLE4 stability to leverage cache+WAL for performance"
  - "Server boot verifier uses time.Now().UnixNano() encoded as uint64 in 8 bytes"
  - "OPEN always sets OPEN4_RESULT_CONFIRM flag, OPEN_CONFIRM echoes seqid+1"

patterns-established:
  - "ioTestFixture: test fixture with metadata+payload for data I/O handler tests"
  - "Stateid placeholder pattern: Phase 7 accepts all stateids, Phase 9 adds proper validation"
  - "Two-phase write in v4: PrepareWrite -> PayloadService.WriteAt -> CommitWrite"

# Metrics
duration: 12min
completed: 2026-02-13
---

# Phase 7 Plan 3: NFSv4 File I/O Handlers Summary

**NFSv4 OPEN/CLOSE/READ/WRITE/COMMIT handlers with placeholder stateids and two-phase write pattern via PayloadService**

## Performance

- **Duration:** 12 min
- **Started:** 2026-02-13T15:07:49Z
- **Completed:** 2026-02-13T15:19:46Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Complete NFSv4 file I/O lifecycle: OPEN -> WRITE -> COMMIT -> READ -> CLOSE
- Stateid4 type with RFC 7530-compliant encode/decode and special-stateid detection
- 37+ comprehensive tests covering success paths, error cases, roundtrips, and edge cases
- WRITE uses two-phase pattern (PrepareWrite/WriteAt/CommitWrite) matching v3 architecture

## Task Commits

Each task was committed atomically:

1. **Task 1: Stateid types and OPEN/OPEN_CONFIRM/CLOSE** - `995e654` (feat)
2. **Task 2: READ/WRITE/COMMIT handlers with tests** - `ce7a2a4` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/types/types.go` - Added Stateid4 type with encode/decode
- `internal/protocol/nfs/v4/handlers/open.go` - OPEN (CLAIM_NULL) and OPEN_CONFIRM handlers
- `internal/protocol/nfs/v4/handlers/close.go` - CLOSE handler with zeroed stateid response
- `internal/protocol/nfs/v4/handlers/read.go` - READ handler with EOF detection and COW
- `internal/protocol/nfs/v4/handlers/write.go` - WRITE handler with two-phase pattern
- `internal/protocol/nfs/v4/handlers/commit.go` - COMMIT handler with PayloadService.Flush
- `internal/protocol/nfs/v4/handlers/io_test.go` - Comprehensive I/O test suite with fixture
- `internal/protocol/nfs/v4/handlers/handler.go` - Registered all 6 new op handlers
- `internal/protocol/nfs/v4/handlers/compound_test.go` - Updated unimplemented-op test

## Decisions Made
- Placeholder stateids for Phase 7: OPEN returns random stateid, all handlers accept any stateid without validation. Phase 9 will add proper state tracking.
- WRITE always returns UNSTABLE4 stability level, matching v3 behavior where cache+WAL provides crash safety.
- Server boot verifier uses time.Now().UnixNano() for high-resolution restart detection.
- EXCLUSIVE4 create mode is treated as GUARDED4 in Phase 7 (consumes 8-byte verifier from wire).
- OPEN always requests confirmation (OPEN4_RESULT_CONFIRM) since we lack state tracking in Phase 7.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Updated compound test for unimplemented-op**
- **Found during:** Task 1
- **Issue:** TestCompoundUnimplementedValidOp used OP_OPEN which was now implemented, causing test failure (expected NFS4ERR_NOTSUPP but got NFS4ERR_NOFILEHANDLE)
- **Fix:** Changed test to use OP_LOCK which is still unimplemented
- **Files modified:** internal/protocol/nfs/v4/handlers/compound_test.go
- **Verification:** All 112 handler tests pass
- **Committed in:** 995e654

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Trivial test update. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- NFSv4 file I/O is functional: clients can OPEN, WRITE, COMMIT, READ, CLOSE files
- Ready for Plan 07-04 (RENAME, SETATTR, LINK, and remaining mutation operations)
- Phase 9 will replace placeholder stateids with proper state management

## Self-Check: PASSED

All 9 created/modified files verified present on disk.
Both task commits (995e654, ce7a2a4) verified in git log.
All 112 handler tests pass with race detection enabled.

---
*Phase: 07-nfsv4-file-operations*
*Completed: 2026-02-13*
