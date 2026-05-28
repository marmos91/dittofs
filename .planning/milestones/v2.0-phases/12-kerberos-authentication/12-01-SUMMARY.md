---
phase: 12-kerberos-authentication
plan: 01
subsystem: auth
tags: [kerberos, rpcsec_gss, xdr, gokrb5, rfc2203, rfc4121]

# Dependency graph
requires:
  - phase: 06-nfsv4-protocol-foundation
    provides: NFSv4 COMPOUND dispatcher and auth context threading
provides:
  - RPCSEC_GSS XDR types (RPCGSSCredV1, RPCGSSInitRes)
  - Sequence window replay detection
  - KerberosProvider with keytab/krb5.conf loading
  - StaticMapper for principal-to-Identity conversion
  - KerberosConfig in pkg/config with defaults and env overrides
  - AuthRPCSECGSS constant (value 6) in rpc/auth.go
affects: [12-02 GSS context state machine, 12-03 RPC integration, 12-04 SECINFO upgrade, 12-05 E2E tests]

# Tech tracking
tech-stack:
  added: []
  patterns: [RPCSEC_GSS credential XDR encode/decode, sliding window bitmap for replay detection, env var override pattern for keytab/SPN]

key-files:
  created:
    - internal/protocol/nfs/rpc/gss/types.go
    - internal/protocol/nfs/rpc/gss/sequence.go
    - internal/protocol/nfs/rpc/gss/types_test.go
    - internal/protocol/nfs/rpc/gss/sequence_test.go
    - pkg/auth/kerberos/config.go
    - pkg/auth/kerberos/kerberos.go
    - pkg/auth/kerberos/identity.go
  modified:
    - internal/protocol/nfs/rpc/auth.go
    - pkg/config/config.go
    - pkg/config/defaults.go

key-decisions:
  - "KerberosConfig in pkg/config (not pkg/auth/kerberos) to avoid circular imports"
  - "StaticMapper as initial identity mapping strategy with DefaultUID/GID=65534"
  - "Env var overrides: DITTOFS_KERBEROS_KEYTAB_PATH, SERVICE_PRINCIPAL, KRB5CONF"
  - "SeqWindow uses bitmap ([]uint64) for O(1) duplicate detection"
  - "Sequence number 0 rejected (not valid in RPCSEC_GSS)"

patterns-established:
  - "RPCSEC_GSS credential: version(4) + gss_proc(4) + seq_num(4) + service(4) + handle(opaque)"
  - "Sliding window: bitmap per uint64 word, slide clears overwritten positions"
  - "Provider pattern: load-at-startup, RWMutex-protected hot-reload for keytab rotation"

# Metrics
duration: 7min
completed: 2026-02-15
---

# Phase 12 Plan 01: Foundation Types and Configuration Summary

**RPCSEC_GSS XDR types with sequence window replay detection, Kerberos provider with keytab hot-reload, and static identity mapper for principal-to-UID/GID conversion**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-15T12:39:59Z
- **Completed:** 2026-02-15T12:46:59Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- RPCSEC_GSS credential decode/encode with all gss_proc values (DATA, INIT, CONTINUE_INIT, DESTROY)
- Sequence window with bitmap-based replay detection, thread-safe concurrent access, window sliding
- KerberosProvider loads keytab/krb5.conf with environment variable overrides and hot-reload
- StaticMapper maps "principal@REALM" to metadata.Identity with UID/GID for NFS permission checks
- KerberosConfig added to main Config struct with proper defaults (disabled by default)
- 20 tests passing with -race detection (11 sequence window + 9 types)

## Task Commits

Each task was committed atomically:

1. **Task 1: RPCSEC_GSS XDR Types and Sequence Window** - `63e6fc1` (feat)
2. **Task 2: Shared Kerberos Provider and Configuration** - `c6419c4` (feat)

## Files Created/Modified
- `internal/protocol/nfs/rpc/gss/types.go` - RPCSEC_GSS constants, RPCGSSCredV1 encode/decode, RPCGSSInitRes encode
- `internal/protocol/nfs/rpc/gss/sequence.go` - SeqWindow with bitmap-based sliding window
- `internal/protocol/nfs/rpc/gss/types_test.go` - 9 type tests (INIT/DATA cred, version rejection, roundtrip, init res)
- `internal/protocol/nfs/rpc/gss/sequence_test.go` - 11 sequence tests (accept, reject, slide, MAXSEQ, concurrent)
- `pkg/auth/kerberos/config.go` - Package doc, references config structs in pkg/config
- `pkg/auth/kerberos/kerberos.go` - Provider with keytab/krb5.conf loading, hot-reload, env overrides
- `pkg/auth/kerberos/identity.go` - IdentityMapper interface, StaticMapper implementation
- `internal/protocol/nfs/rpc/auth.go` - Added AuthRPCSECGSS=6 constant
- `pkg/config/config.go` - KerberosConfig, IdentityMappingConfig, StaticIdentity structs
- `pkg/config/defaults.go` - applyKerberosDefaults with all default values

## Decisions Made
- KerberosConfig structs placed in pkg/config to follow existing pattern (LockConfig, CacheConfig, etc.) and avoid circular imports with pkg/auth/kerberos
- StaticMapper as initial strategy - sufficient for small deployments; LDAP/nsswitch can be added later via the IdentityMapper interface
- Environment variable overrides (DITTOFS_KERBEROS_KEYTAB_PATH, DITTOFS_KERBEROS_SERVICE_PRINCIPAL, DITTOFS_KERBEROS_KRB5CONF) for container/orchestrator deployments
- Sequence number 0 is rejected as invalid - RPCSEC_GSS contexts start at seq_num 1
- No new dependencies added - uses existing gokrb5 v8.4.4 already in go.mod

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- GSS types and sequence window ready for context state machine (Plan 12-02)
- KerberosProvider ready to be consumed by GSS context manager
- IdentityMapper interface ready for integration with auth context threading
- AuthRPCSECGSS constant available for RPC dispatch routing (Plan 12-03)

---
*Phase: 12-kerberos-authentication*
*Completed: 2026-02-15*
