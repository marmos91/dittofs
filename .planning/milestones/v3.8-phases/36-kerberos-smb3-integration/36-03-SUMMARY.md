---
phase: 36-kerberos-smb3-integration
plan: 03
subsystem: auth
tags: [smb, spnego, ntlm, kerberos, gssapi, guest-session, negotiate]

# Dependency graph
requires:
  - phase: 36-02
    provides: Kerberos AP-REQ verification, session key normalization, SPNEGO accept-complete with MIC
provides:
  - NTLM fallback on Kerberos failure via SPNEGO reject
  - Guest session policy enforcement (GuestEnabled, signing-required checks)
  - NEGOTIATE SecurityBuffer with SPNEGO NegHints (available mechanisms)
  - NtlmEnabled/GuestEnabled/SMBServicePrincipal control plane settings
  - Adapter wiring of KerberosService and IdentityConfig from Provider
affects: [smb-testing, cross-protocol-auth]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Policy enforcement in guest session creation (check enabled + signing policy)"
    - "SPNEGO NegHints in NEGOTIATE response for mechanism advertisement"
    - "Kerberos failure -> SPNEGO reject -> NTLM fallback pattern"

key-files:
  created: []
  modified:
    - internal/adapter/smb/auth/spnego.go
    - internal/adapter/smb/auth/spnego_test.go
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/session_setup.go
    - internal/adapter/smb/v2/handlers/session_setup_test.go
    - internal/adapter/smb/v2/handlers/negotiate.go
    - internal/adapter/smb/v2/handlers/negotiate_test.go
    - pkg/controlplane/models/adapter_settings.go
    - pkg/adapter/smb/adapter.go

key-decisions:
  - "Kerberos failure returns SPNEGO reject (not STATUS_MORE_PROCESSING_REQUIRED) so client retries with fresh SessionId=0 for NTLM"
  - "Guest sessions gated by both GuestEnabled policy AND signing.required check (no key material for signing)"
  - "NEGOTIATE SecurityBuffer populated with SPNEGO NegTokenInit listing available mechanisms"
  - "SetKerberosProvider creates KerberosService and IdentityConfig (strip-realm default) automatically"
  - "NTLM disable check happens before NTLM message type dispatch (early rejection)"

patterns-established:
  - "Auth policy enforcement: check handler.NtlmEnabled / handler.GuestEnabled before processing auth tokens"
  - "SPNEGO NegHints: BuildNegHints(kerberosEnabled, ntlmEnabled) -> NEGOTIATE SecurityBuffer"

requirements-completed: [AUTH-03, AUTH-04]

# Metrics
duration: 8min
completed: 2026-03-02
---

# Phase 36 Plan 03: NTLM Fallback, Guest Policy, and SPNEGO NegHints Summary

**NTLM fallback via SPNEGO reject on Kerberos failure, guest session policy enforcement with signing checks, and NEGOTIATE SecurityBuffer advertising available auth mechanisms**

## Performance

- **Duration:** 8 min
- **Started:** 2026-03-02T10:37:31Z
- **Completed:** 2026-03-02T10:46:21Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Kerberos failure now returns clean SPNEGO reject, enabling client to retry with NTLM on fresh SessionId=0
- Guest sessions properly gated by GuestEnabled policy and signing.required check (no session key = no signing)
- NEGOTIATE response SecurityBuffer contains SPNEGO NegTokenInit with available mechanisms (Kerberos + NTLM when configured)
- NtlmEnabled, GuestEnabled, SMBServicePrincipal added to SMBAdapterSettings for runtime configurability
- SetKerberosProvider now automatically creates KerberosService and IdentityConfig with strip-realm default

## Task Commits

Each task was committed atomically:

1. **Task 1: NTLM fallback, guest session policy, and SPNEGO NegHints** (TDD)
   - `5d113f0` (test) - Failing tests for BuildNegHints, NTLM disable, guest policy, Kerberos reject
   - `50a92aa` (feat) - Implementation of BuildNegHints, NtlmEnabled/GuestEnabled, SPNEGO reject, NEGOTIATE SecurityBuffer
2. **Task 2: Control plane settings and adapter wiring** - `5ad7db1` (feat)

## Files Created/Modified
- `internal/adapter/smb/auth/spnego.go` - Added BuildNegHints for NEGOTIATE SecurityBuffer construction
- `internal/adapter/smb/auth/spnego_test.go` - Tests for BuildNegHints (Kerberos+NTLM, NTLM-only, Kerberos-only)
- `internal/adapter/smb/v2/handlers/handler.go` - Added NtlmEnabled and GuestEnabled fields with defaults
- `internal/adapter/smb/v2/handlers/session_setup.go` - NTLM disable check, guest policy enforcement, Kerberos failure SPNEGO reject
- `internal/adapter/smb/v2/handlers/session_setup_test.go` - Tests for NTLM disable, guest policy, signing required reject, Kerberos SPNEGO reject
- `internal/adapter/smb/v2/handlers/negotiate.go` - SecurityBuffer populated with SPNEGO NegHints
- `internal/adapter/smb/v2/handlers/negotiate_test.go` - Updated response format tests for non-empty SecurityBuffer
- `pkg/controlplane/models/adapter_settings.go` - NtlmEnabled, GuestEnabled, SMBServicePrincipal fields on SMBAdapterSettings
- `pkg/adapter/smb/adapter.go` - Wire settings to handler, create KerberosService/IdentityConfig in SetKerberosProvider

## Decisions Made
- Kerberos failure returns SPNEGO reject (NegState=reject) with STATUS_LOGON_FAILURE so client retries with fresh SessionId=0 for NTLM -- clean state, no stale Kerberos context
- Guest sessions gated by both GuestEnabled policy AND signing.required check (guest has no session key, so signing is impossible)
- NTLM disable check happens early in SessionSetup before message type dispatch
- SetKerberosProvider creates KerberosService and IdentityConfig automatically (no separate setup step needed)
- Windows 11 24H2 insecure guest logon hint logged at INFO level when guest session created

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Updated negotiate response format tests for SPNEGO NegHints**
- **Found during:** Task 1
- **Issue:** Existing TestNegotiate_ResponseFormat tests expected 65-byte response with empty SecurityBuffer, but NEGOTIATE now includes SPNEGO NegHints
- **Fix:** Updated test expectations to check for non-empty SecurityBuffer and correct overall length
- **Files modified:** internal/adapter/smb/v2/handlers/negotiate_test.go
- **Verification:** All negotiate tests pass
- **Committed in:** 50a92aa (Task 1 GREEN commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Test update was necessary due to planned behavioral change. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 36 (Kerberos SMB3 Integration) complete with all 3 plans done
- AUTH-01 through AUTH-04 requirements satisfied
- Ready for cross-protocol testing and integration verification

---
*Phase: 36-kerberos-smb3-integration*
*Completed: 2026-03-02*
