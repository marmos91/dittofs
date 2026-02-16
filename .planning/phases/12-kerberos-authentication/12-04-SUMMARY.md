---
phase: 12-kerberos-authentication
plan: 04
subsystem: auth
tags: [kerberos, rpcsec-gss, krb5i, krb5p, integrity, privacy, secinfo, mic, wrap-token, gokrb5]

# Dependency graph
requires:
  - phase: 12-03
    provides: "RPCSEC_GSS DATA path, reply verifier, NFS connection handler GSS integration"
provides:
  - "krb5i integrity wrap/unwrap for rpc_gss_integ_data (RFC 2203 Section 5.3.3.4.2)"
  - "krb5p privacy wrap/unwrap for rpc_gss_priv_data (RFC 2203 Section 5.3.3.4.3)"
  - "SECINFO returns RPCSEC_GSS krb5p/krb5i/krb5 entries when Kerberos enabled"
  - "Reply body wrapping for krb5i/krb5p in NFS connection handler"
affects: [12-kerberos-authentication, nfs-security]

# Tech tracking
tech-stack:
  added: []
  patterns: ["initiator/acceptor MIC/WrapToken key usage separation", "XDR opaque encoding for GSS tokens", "KRB5 OID DER encoding for SECINFO"]

key-files:
  created:
    - internal/protocol/nfs/rpc/gss/integrity.go
    - internal/protocol/nfs/rpc/gss/privacy.go
    - internal/protocol/nfs/rpc/gss/integrity_test.go
    - internal/protocol/nfs/rpc/gss/privacy_test.go
    - internal/protocol/nfs/v4/handlers/secinfo_test.go
  modified:
    - internal/protocol/nfs/rpc/gss/framework.go
    - internal/protocol/nfs/rpc/gss/framework_test.go
    - internal/protocol/nfs/rpc/gss/context.go
    - pkg/adapter/nfs/nfs_connection.go
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/handlers/secinfo.go
    - internal/protocol/nfs/v4/handlers/stubs_test.go
    - pkg/adapter/nfs/nfs_adapter.go

key-decisions:
  - "gokrb5 WrapToken provides integrity (HMAC) but not actual encryption; documented as limitation"
  - "Separate key usage constants for initiator vs acceptor direction (RFC 4121)"
  - "KerberosEnabled bool on v4 Handler struct (simplest approach, avoids config dependency)"
  - "KRB5 OID encoded as full DER (tag+length+value) in sec_oid4 per RFC 7530"
  - "SECINFO entry order: krb5p > krb5i > krb5 > AUTH_SYS > AUTH_NONE (most secure first)"

patterns-established:
  - "Initiator/Acceptor direction pattern: client uses KeyUsageInitiatorSign/Seal, server uses AcceptorSign/Seal"
  - "Dual sequence number validation: credential seq_num must match body seq_num for integrity and privacy"
  - "encodeSecInfoGSSEntry helper for encoding RPCSEC_GSS secinfo4 entries with OID"

# Metrics
duration: 14min
completed: 2026-02-15
---

# Phase 12 Plan 04: SECINFO Upgrade Summary

**krb5i/krb5p security services with MIC/WrapToken wrapping and SECINFO RPCSEC_GSS pseudo-flavor advertisement**

## Performance

- **Duration:** 14 min
- **Started:** 2026-02-15T13:27:00Z
- **Completed:** 2026-02-15T13:41:00Z
- **Tasks:** 2
- **Files modified:** 13 (5 created, 8 modified)

## Accomplishments
- Full krb5i integrity protection: MIC verification on incoming requests, MIC on outgoing replies
- Full krb5p privacy protection: WrapToken verification on incoming requests, WrapToken on outgoing replies
- Dual sequence number validation (credential + body) enforced for both modes
- SECINFO returns 5 entries (krb5p, krb5i, krb5, AUTH_SYS, AUTH_NONE) when Kerberos enabled
- Reply body wrapping in NFS connection handler for krb5i/krb5p service levels
- 22 new tests across integrity, privacy, and SECINFO

## Task Commits

Each task was committed atomically:

1. **Task 1: krb5i Integrity and krb5p Privacy** - `5742f98` (feat)
2. **Task 2: SECINFO Upgrade with RPCSEC_GSS Pseudo-Flavors** - `a1dee5c` (feat)

## Files Created/Modified
- `internal/protocol/nfs/rpc/gss/integrity.go` - UnwrapIntegrity/WrapIntegrity for rpc_gss_integ_data
- `internal/protocol/nfs/rpc/gss/privacy.go` - UnwrapPrivacy/WrapPrivacy for rpc_gss_priv_data
- `internal/protocol/nfs/rpc/gss/framework.go` - Replaced stubs with real integrity/privacy dispatch
- `internal/protocol/nfs/rpc/gss/context.go` - Added Service field to GSSSessionInfo
- `pkg/adapter/nfs/nfs_connection.go` - Reply body wrapping for krb5i/krb5p
- `internal/protocol/nfs/v4/handlers/handler.go` - Added KerberosEnabled field to Handler
- `internal/protocol/nfs/v4/handlers/secinfo.go` - RPCSEC_GSS entries with KRB5 OID encoding
- `internal/protocol/nfs/v4/handlers/secinfo_test.go` - 11 SECINFO tests (no Kerberos + Kerberos)
- `internal/protocol/nfs/rpc/gss/integrity_test.go` - 9 integrity tests
- `internal/protocol/nfs/rpc/gss/privacy_test.go` - 9 privacy tests

## Decisions Made
- gokrb5 WrapToken does not provide actual encryption (only HMAC-based integrity with a different wire format). Documented as limitation since gokrb5 library does not expose full GSS-API Wrap encryption.
- Used separate key usage constants for initiator (client) and acceptor (server) per RFC 4121: InitiatorSign(23)/InitiatorSeal(24) for unwrapping client data, AcceptorSign(25)/AcceptorSeal(26) for wrapping server replies.
- Added KerberosEnabled bool to v4 Handler struct (set from nfs_adapter.go based on kerberosConfig presence). Simplest approach that avoids the handler needing to know about Kerberos config directly.
- KRB5 OID encoded as full ASN.1 DER (0x06 0x09 + 9 value bytes) in the sec_oid4 field per RFC 7530 Section 3.2.1.
- SECINFO entries ordered most secure first per RFC 7530 convention: krb5p (privacy=3) > krb5i (integrity=2) > krb5 (none=1) > AUTH_SYS > AUTH_NONE.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- MIC token direction mismatch in initial roundtrip tests: WrapIntegrity creates acceptor (server) MIC tokens while UnwrapIntegrity expects initiator (client) MIC tokens. Resolved by restructuring tests to properly simulate client-side wrapping using buildInitiatorIntegData/buildInitiatorPrivData helpers.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Full RPCSEC_GSS krb5/krb5i/krb5p support complete
- SECINFO advertises all security mechanisms correctly
- Ready for Plan 12-05 (E2E Tests) to validate end-to-end Kerberos flow

## Self-Check: PASSED

- All 11 key files verified on disk
- Both task commits (5742f98, a1dee5c) verified in git log
- All tests pass with race detection
- Full build and vet pass cleanly

---
*Phase: 12-kerberos-authentication*
*Completed: 2026-02-15*
