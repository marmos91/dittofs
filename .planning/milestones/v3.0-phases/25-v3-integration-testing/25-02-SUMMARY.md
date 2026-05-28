---
phase: 25-v3-integration-testing
plan: 02
subsystem: auth
tags: [kerberos, spnego, smb, gokrb5, session-setup, cross-protocol, identity-mapping]

# Dependency graph
requires:
  - phase: 12-kerberos-authentication
    provides: "Shared Kerberos provider, keytab management, RPCSEC_GSS framework"
  - phase: 05-cross-protocol-integration
    provides: "Cross-protocol test patterns, shared store infrastructure"
provides:
  - "SMB SESSION_SETUP Kerberos auth path via SPNEGO/gokrb5"
  - "Principal-to-control-plane-user identity mapping for SMB"
  - "SMB Kerberos E2E tests (auth, identity, NTLM coexistence)"
  - "Cross-protocol Kerberos identity consistency tests (NFS+SMB)"
affects: [25-v3-integration-testing, smb-adapter, kerberos-authentication]

# Tech tracking
tech-stack:
  added: [gokrb5/v8/messages, gokrb5/v8/service]
  patterns: [SPNEGO-Kerberos-detection-before-NTLM, principal-realm-stripping, shared-keytab-access-via-KerberosProvider]

key-files:
  created:
    - test/e2e/smb_kerberos_test.go
    - test/e2e/cross_protocol_kerberos_test.go
  modified:
    - internal/protocol/smb/v2/handlers/session_setup.go
    - internal/protocol/smb/v2/handlers/session_setup_test.go
    - internal/protocol/smb/v2/handlers/handler.go

key-decisions:
  - "Kerberos detection placed BEFORE NTLM extraction in SESSION_SETUP to route SPNEGO tokens correctly"
  - "KerberosProvider field added to Handler struct (same pattern as NFS adapter's keytab access)"
  - "Principal-to-username mapping strips realm part (alice@REALM -> alice) then looks up control plane user"
  - "E2E tests are platform-aware: Linux primary with mount.cifs sec=krb5, macOS best-effort skip"

patterns-established:
  - "SPNEGO-first detection: check HasKerberos() on parsed SPNEGO before falling through to NTLM"
  - "Shared KerberosProvider: same keytab/config used by both NFS and SMB adapters"
  - "Cross-protocol Kerberos identity test pattern: NFS create -> SMB read and SMB create -> NFS read"

requirements-completed: [SMBKRB-01, SMBKRB-02]

# Metrics
duration: 12min
completed: 2026-02-23
---

# Phase 25 Plan 02: SMB Kerberos Authentication Summary

**SPNEGO/Kerberos auth path in SMB SESSION_SETUP handler with gokrb5 validation, principal-to-user mapping, and cross-protocol identity E2E tests**

## Performance

- **Duration:** 12 min
- **Started:** 2026-02-23T09:50:00Z
- **Completed:** 2026-02-23T10:02:00Z
- **Tasks:** 2
- **Files modified:** 5

## Accomplishments
- SMB SESSION_SETUP handler now detects Kerberos tokens in SPNEGO and validates them via gokrb5 service keytab
- Kerberos principal maps to control plane user identity (alice@REALM -> alice) with full session creation
- Existing NTLM authentication continues to work unchanged (regression guard tests)
- Comprehensive E2E tests: SMB Kerberos auth, identity mapping, NTLM+Kerberos coexistence, cross-protocol identity consistency

## Task Commits

Each task was committed atomically:

1. **Task 1: Implement Kerberos auth path in SMB SESSION_SETUP handler** - `cb4eef16` (feat)
2. **Task 2: SMB Kerberos E2E tests and cross-protocol identity verification** - `5cf2e652` (test)

## Files Created/Modified
- `internal/protocol/smb/v2/handlers/session_setup.go` - Added Kerberos detection and handleKerberosAuth() method
- `internal/protocol/smb/v2/handlers/handler.go` - Added KerberosProvider field to Handler struct
- `internal/protocol/smb/v2/handlers/session_setup_test.go` - Added Kerberos detection, auth failure, and NTLM regression tests
- `test/e2e/smb_kerberos_test.go` - SMB Kerberos auth, identity mapping, and NTLM coexistence E2E tests
- `test/e2e/cross_protocol_kerberos_test.go` - Cross-protocol NFS/SMB Kerberos identity consistency tests

## Decisions Made
- Kerberos detection placed BEFORE NTLM extraction in SessionSetup() to route SPNEGO tokens with Kerberos OID to the new path
- KerberosProvider field added to Handler struct rather than passing through context (matches NFS adapter pattern)
- Principal-to-username mapping strips realm part (alice@REALM -> alice), then looks up user in UserStore
- SPNEGO accept-complete response built on successful Kerberos auth (wraps result in negTokenResp)
- E2E platform strategy: Linux primary (mount.cifs sec=krb5), macOS best-effort (skip on failure)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed unused variable in handleKerberosAuth**
- **Found during:** Task 1
- **Issue:** Session variable `sess` declared but not used in logging statement (compiler error)
- **Fix:** Changed logging to use sess.SessionID, sess.Username, sess.Domain, sess.IsGuest fields
- **Files modified:** internal/protocol/smb/v2/handlers/session_setup.go
- **Verification:** `go test ./internal/protocol/smb/...` passed
- **Committed in:** cb4eef16 (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Minor compilation fix. No scope creep.

## Issues Encountered
None beyond the auto-fixed unused variable.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- SMB Kerberos auth is ready for real-world testing in Linux environments with mount.cifs
- Cross-protocol identity consistency verified: same Kerberos user sees same files from both NFS and SMB
- Ready for plan 25-03 (EOS replay, backchannel, directory delegation E2E tests)

## Self-Check: PASSED

All files exist, all commits verified.

---
*Phase: 25-v3-integration-testing*
*Completed: 2026-02-23*
