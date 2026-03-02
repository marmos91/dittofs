---
phase: 36-kerberos-smb3-integration
plan: 02
subsystem: auth
tags: [kerberos, smb3, spnego, session-key, kdf, mutual-auth, identity-mapping]

# Dependency graph
requires:
  - phase: 36-01
    provides: "Shared KerberosService with Authenticate() and BuildMutualAuth()"
provides:
  - "handleKerberosAuth in kerberos_auth.go with session key normalization and KDF integration"
  - "SPNEGO MIC computation and verification for downgrade protection"
  - "ResolvePrincipal for configurable Kerberos principal-to-username mapping"
affects: [36-03, smb-adapter-wiring]

# Tech tracking
tech-stack:
  added: []
  patterns: ["session key normalization (truncate/pad to 16 bytes)", "SPNEGO MIC downgrade protection", "OID-matching for Windows SSPI"]

key-files:
  created:
    - internal/adapter/smb/v2/handlers/kerberos_auth.go
    - internal/adapter/smb/v2/handlers/kerberos_auth_test.go
    - pkg/auth/kerberos/identity.go
    - pkg/auth/kerberos/identity_test.go
  modified:
    - internal/adapter/smb/auth/spnego.go
    - internal/adapter/smb/auth/spnego_test.go
    - internal/adapter/smb/v2/handlers/session_setup.go
    - internal/adapter/smb/v2/handlers/handler.go

key-decisions:
  - "Session key normalized to 16 bytes via copy() (truncate >16, zero-pad <16) per MS-SMB2 3.3.5.5.3"
  - "MIC computation uses key usage 23 (acceptor sign); verification uses key usage 25 (initiator sign) per RFC 4121"
  - "Client Kerberos OID echoed in SPNEGO response (MS OID preferred for Windows SSPI)"
  - "Valid Kerberos ticket from unknown principal = hard failure (not guest fallback)"
  - "Server mechListMIC computed with full session key, not normalized 16-byte key"

patterns-established:
  - "KerberosService used instead of inline gokrb5 calls for AP-REQ verification"
  - "SPNEGO MIC for downgrade protection on Kerberos accept-complete responses"

requirements-completed: [AUTH-01, AUTH-02, KDF-04]

# Metrics
duration: 10min
completed: 2026-03-02
---

# Phase 36 Plan 02: SMB Kerberos Auth Handler Summary

**SMB Kerberos auth with 16-byte session key normalization, KDF integration, AP-REP mutual auth, SPNEGO MIC, and configurable principal-to-username mapping**

## Performance

- **Duration:** 10 min
- **Started:** 2026-03-02T10:24:48Z
- **Completed:** 2026-03-02T10:34:43Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Created SPNEGO MIC helpers (ComputeMechListMIC, VerifyMechListMIC) with correct RFC 4121 key usages
- Extended ParsedToken with MechListBytes and MechListMIC fields for downgrade protection
- Added BuildAcceptCompleteWithMIC for SPNEGO NegTokenResp with MIC field
- Implemented ResolvePrincipal with strip-realm default, explicit mapping table, and service principal handling
- Extracted handleKerberosAuth from session_setup.go into dedicated kerberos_auth.go
- Session key normalized to 16 bytes (AES-256 truncated, DES zero-padded, AES-128 pass-through)
- Real AP-REP token via shared KerberosService.BuildMutualAuth() in SPNEGO response
- Client Kerberos OID matched (MS OID vs standard) for Windows SSPI compatibility
- Handler struct extended with KerberosService, IdentityConfig, SMBServicePrincipal fields
- Removed 100+ lines of inline gokrb5 code from session_setup.go
- All tests pass including existing NTLM regression tests

## Task Commits

Each task was committed atomically:

1. **Task 1: SPNEGO MIC helpers and identity resolution** - `16697f2c` (test) + `3a3e332b` (feat)
2. **Task 2: Extract kerberos_auth.go** - `753a7449` (test) + `9ccf23ee` (feat)

_TDD tasks have separate test and implementation commits._

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/kerberos_auth.go` - Extracted Kerberos auth handler with session key normalization, KDF integration, and AP-REP mutual auth
- `internal/adapter/smb/v2/handlers/kerberos_auth_test.go` - Tests for normalizeSessionKey, deriveSMBPrincipal, clientKerberosOID
- `internal/adapter/smb/auth/spnego.go` - Extended with MIC helpers, MechListBytes/MechListMIC in ParsedToken, BuildAcceptCompleteWithMIC
- `internal/adapter/smb/auth/spnego_test.go` - Tests for MIC computation/verification, BuildAcceptCompleteWithMIC, MechListBytes
- `internal/adapter/smb/v2/handlers/session_setup.go` - Removed old handleKerberosAuth, updated call site to pass parsedToken
- `internal/adapter/smb/v2/handlers/handler.go` - Added KerberosService, IdentityConfig, SMBServicePrincipal fields
- `pkg/auth/kerberos/identity.go` - ResolvePrincipal and IdentityConfig for principal-to-username mapping
- `pkg/auth/kerberos/identity_test.go` - Tests for strip-realm, explicit mapping, service principals

## Decisions Made
- Session key normalized to 16 bytes via `copy()` which naturally truncates longer keys and leaves shorter keys zero-padded. This matches MS-SMB2 Section 3.3.5.5.3 requirement.
- MIC computation (server as acceptor) uses key usage 23; MIC verification (client as initiator) uses key usage 25. These are the correct RFC 4121 key usages for GSS-API MICTokens.
- Valid Kerberos ticket from an unknown DittoFS principal returns STATUS_LOGON_FAILURE (not guest fallback). This is a security decision -- domain authentication should not silently degrade.
- Server mechListMIC uses the full Kerberos session key (before normalization) per RFC 4178. The 16-byte normalization is only for SMB3 KDF input.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Kerberos auth handler ready for adapter wiring in plan 36-03
- KerberosService field needs injection during adapter startup
- IdentityConfig configurable from control plane settings
- SMBServicePrincipal overridable from control plane settings

## Self-Check: PASSED

All 8 files verified present. All 4 commits verified in git log.

---
*Phase: 36-kerberos-smb3-integration*
*Completed: 2026-03-02*
