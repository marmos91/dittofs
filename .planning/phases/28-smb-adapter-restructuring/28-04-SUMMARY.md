---
phase: 28-smb-adapter-restructuring
plan: 04
subsystem: auth
tags: [authenticator, ntlm, spnego, auth-unix, interface]

# Dependency graph
requires:
  - phase: 28-01
    provides: "SMB auth packages (ntlm, spnego) in internal/adapter/smb/auth"
provides:
  - "adapter.Authenticator interface in pkg/adapter/auth.go"
  - "adapter.AuthResult struct with User, SessionKey, IsGuest"
  - "ErrMoreProcessingRequired sentinel for multi-round auth"
  - "SMBAuthenticator wrapping NTLM+SPNEGO in internal/adapter/smb/auth"
  - "UnixAuthenticator for NFS AUTH_UNIX in internal/adapter/nfs/auth"
affects: [39-smb3-protocol, 29-nfs-auth-refactoring]

# Tech tracking
tech-stack:
  added: []
  patterns: [unified-authenticator-interface, multi-round-auth-sentinel-error]

key-files:
  created:
    - pkg/adapter/auth.go
    - internal/adapter/smb/auth/authenticator.go
    - internal/adapter/smb/auth/authenticator_test.go
    - internal/adapter/nfs/auth/unix.go
    - internal/adapter/nfs/auth/unix_test.go
  modified: []

key-decisions:
  - "Standalone AUTH_UNIX parser in nfs/auth to avoid RPC package import dependency"
  - "SMBAuthenticator uses sync.Map for pending auth state tracking across concurrent sessions"
  - "Unknown UIDs produce synthetic users (unix:UID) rather than errors for NFS backward compat"

patterns-established:
  - "Authenticator interface: three return patterns (success, more-processing, failure)"
  - "Protocol authenticators maintain internal state for multi-round auth"

requirements-completed: [REF-04]

# Metrics
duration: 6min
completed: 2026-02-25
---

# Phase 28 Plan 04: Authenticator Interface Summary

**Unified Authenticator interface with SMB NTLM/SPNEGO and NFS AUTH_UNIX implementations bridging protocol auth to DittoFS identity model**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-25T20:56:35Z
- **Completed:** 2026-02-25T21:02:59Z
- **Tasks:** 2
- **Files modified:** 5

## Accomplishments
- Defined clean adapter.Authenticator interface with three return patterns (success, challenge, failure)
- Created SMBAuthenticator wrapping NTLM+SPNEGO with multi-round challenge-response support
- Created NFS UnixAuthenticator parsing AUTH_UNIX credentials with UID-to-user resolution
- Both implementations have comprehensive unit tests (10+ tests each)
- No behavioral changes to existing code paths

## Task Commits

Each task was committed atomically:

1. **Task 1: Define Authenticator interface and SMB implementation** - `94f70978` (feat)
2. **Task 2: Create NFS AUTH_UNIX Authenticator** - `81b902ef` (feat)

## Files Created/Modified
- `pkg/adapter/auth.go` - Authenticator interface, AuthResult struct, ErrMoreProcessingRequired
- `internal/adapter/smb/auth/authenticator.go` - SMBAuthenticator wrapping NTLM+SPNEGO for Authenticator interface
- `internal/adapter/smb/auth/authenticator_test.go` - Tests for SMB authenticator (10 tests)
- `internal/adapter/nfs/auth/unix.go` - UnixAuthenticator for NFS AUTH_UNIX credential extraction
- `internal/adapter/nfs/auth/unix_test.go` - Tests for NFS authenticator (11 tests)

## Decisions Made
- Standalone AUTH_UNIX parser in nfs/auth package rather than importing rpc.ParseUnixAuth. Avoids creating a dependency from the auth package to the RPC package, keeping the auth abstraction layer clean.
- SMBAuthenticator uses sync.Map + atomic.Uint64 for session tracking, matching the pattern already used in the Handler.pendingAuth field. This supports concurrent NTLM handshakes from different connections.
- Unknown UIDs in NFS AUTH_UNIX produce synthetic users (username "unix:UID") rather than errors. This preserves NFS's traditional behavior where any Unix client can access the server.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

Pre-existing untracked files from plan 28-03 (compound.go, conn_types.go, framing.go) caused build errors. Moved them aside during development and restored afterward. These are out of scope for this plan.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Authenticator interface ready for SMB3 integration in Phase 39
- NFS UnixAuthenticator ready for full NFS auth refactoring
- Existing session_setup.go continues working unchanged

## Self-Check: PASSED

All 5 created files verified on disk. Both task commits (94f70978, 81b902ef) verified in git log.

---
*Phase: 28-smb-adapter-restructuring*
*Completed: 2026-02-25*
