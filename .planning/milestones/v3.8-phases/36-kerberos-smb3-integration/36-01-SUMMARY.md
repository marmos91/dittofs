---
phase: 36-kerberos-smb3-integration
plan: 01
subsystem: auth
tags: [kerberos, gss-api, ap-req, ap-rep, replay-cache, rpcsec-gss, smb3]

# Dependency graph
requires:
  - phase: 12-kerberos-gss
    provides: "RPCSEC_GSS framework with AP-REQ verification and AP-REP construction"
provides:
  - "Shared KerberosService in internal/auth/kerberos/ with Authenticate() and BuildMutualAuth()"
  - "Cross-protocol ReplayCache for Kerberos authenticator dedup"
  - "NFS GSS framework refactored to delegate to shared KerberosService"
affects: [36-02, 36-03, smb-kerberos-auth]

# Tech tracking
tech-stack:
  added: []
  patterns: ["shared auth service extraction", "protocol-agnostic Kerberos verification"]

key-files:
  created:
    - internal/auth/kerberos/service.go
    - internal/auth/kerberos/service_test.go
    - internal/auth/kerberos/replay.go
    - internal/auth/kerberos/replay_test.go
    - internal/auth/kerberos/doc.go
  modified:
    - internal/adapter/nfs/rpc/gss/framework.go
    - pkg/adapter/nfs/nlm.go
    - test/integration/kerberos/kerberos_integration_test.go

key-decisions:
  - "BuildMutualAuth returns raw AP-REP (APPLICATION 15), not GSS-wrapped; NFS adds GSS wrapper, SMB passes to SPNEGO"
  - "ReplayCache keyed by 4-tuple (principal, ctime, cusec, servicePrincipal) for cross-protocol dedup"
  - "sync.Map for ReplayCache concurrent access (optimized for high-churn read-heavy workload)"
  - "HasSubkey exported as package-level function for use by both NFS GSS and future SMB auth"

patterns-established:
  - "Shared auth service extraction: protocol-agnostic core in internal/auth/, protocol-specific framing in adapter packages"
  - "Raw token + protocol wrapper pattern: shared service produces raw tokens, adapters add protocol-specific framing"

requirements-completed: [ARCH-03]

# Metrics
duration: 7min
completed: 2026-03-02
---

# Phase 36 Plan 01: Shared KerberosService Summary

**Shared KerberosService with AP-REQ verification, AP-REP mutual auth, and cross-protocol replay cache; NFS GSS framework refactored to delegate**

## Performance

- **Duration:** 7 min
- **Started:** 2026-03-02T10:13:12Z
- **Completed:** 2026-03-02T10:20:35Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Created shared KerberosService in internal/auth/kerberos/ with Authenticate() and BuildMutualAuth()
- Implemented cross-protocol ReplayCache with sync.Map, TTL expiry, and lazy cleanup
- Refactored NFS GSS framework to delegate AP-REQ verification and AP-REP construction to shared service
- Removed 160+ lines of duplicated Kerberos logic from gss/framework.go
- All 95 tests pass (15 new + 80 existing GSS), including race detector

## Task Commits

Each task was committed atomically:

1. **Task 1: KerberosService and ReplayCache** - `3fc89862` (test) + `a2c64aa9` (feat)
2. **Task 2: Refactor NFS GSS framework** - `9996f309` (feat)

_TDD tasks have separate test and implementation commits._

## Files Created/Modified
- `internal/auth/kerberos/doc.go` - Package documentation for shared Kerberos service layer
- `internal/auth/kerberos/service.go` - KerberosService with Authenticate() and BuildMutualAuth()
- `internal/auth/kerberos/service_test.go` - Tests for BuildMutualAuth, HasSubkey, AuthResult
- `internal/auth/kerberos/replay.go` - ReplayCache with sync.Map and TTL-based expiry
- `internal/auth/kerberos/replay_test.go` - Tests for replay detection, expiry, concurrency
- `internal/adapter/nfs/rpc/gss/framework.go` - Refactored Krb5Verifier to delegate to KerberosService
- `pkg/adapter/nfs/nlm.go` - Updated to create KerberosService and pass to NewKrb5Verifier
- `test/integration/kerberos/kerberos_integration_test.go` - Updated constructor calls

## Decisions Made
- BuildMutualAuth returns raw AP-REP (APPLICATION 15 tag), not GSS-API wrapped. NFS adds GSS wrapper (0x60 + OID + 0x0200), SMB will pass raw to SPNEGO. This avoids coupling the shared service to any protocol's framing.
- ReplayCache uses 4-tuple key (principal, ctime, cusec, servicePrincipal) to detect replays across NFS and SMB simultaneously.
- HasSubkey exported as package-level function (not method) for reuse by both protocols.
- AP-REP encryption uses ticket session key (not context key/subkey) per RFC 4120 Section 5.5.2.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- KerberosService ready for SMB SESSION_SETUP integration (plan 36-02)
- ReplayCache shared across protocols for authenticator dedup
- NFS GSS framework fully functional with shared service delegation

## Self-Check: PASSED

All 6 files verified present. All 3 commits verified in git log.

---
*Phase: 36-kerberos-smb3-integration*
*Completed: 2026-03-02*
