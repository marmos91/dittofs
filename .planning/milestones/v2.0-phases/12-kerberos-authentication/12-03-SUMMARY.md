---
phase: 12-kerberos-authentication
plan: 03
subsystem: auth
tags: [rpcsec-gss, kerberos, krb5, gss-api, nfs, rpc, mic, verifier]

# Dependency graph
requires:
  - phase: 12-02
    provides: GSSProcessor with INIT/DESTROY lifecycle, GSSContext store, Verifier interface
provides:
  - RPCSEC_GSS DATA path handling with sequence validation and identity mapping
  - Reply verifier computation (MIC of seq_num per RFC 2203)
  - GSSProcessor wired into NFS connection handler for auth flavor 6 interception
  - GSS identity propagation via context.Value for both NFSv3 and NFSv4
  - MakeGSSSuccessReply and MakeAuthErrorReply RPC reply builders
affects: [12-04, 12-05]

# Tech tracking
tech-stack:
  added: [gokrb5/v8 MICToken, gokrb5/v8 EncryptionKey]
  patterns: [context.Value for GSS identity threading, GSSSessionInfo for reply verifier construction]

key-files:
  created:
    - internal/protocol/nfs/rpc/gss/verifier.go
    - internal/protocol/nfs/rpc/gss/verifier_test.go
  modified:
    - internal/protocol/nfs/rpc/gss/framework.go
    - internal/protocol/nfs/rpc/gss/framework_test.go
    - internal/protocol/nfs/rpc/gss/context.go
    - internal/protocol/nfs/rpc/parser.go
    - internal/protocol/nfs/rpc/message.go
    - internal/protocol/nfs/dispatch.go
    - internal/protocol/nfs/v4/handlers/context.go
    - pkg/adapter/nfs/nfs_adapter.go
    - pkg/adapter/nfs/nfs_connection.go

key-decisions:
  - "context.Value pattern for GSS identity and session info threading (no handler signature changes)"
  - "GSSSessionInfo carries session key + seq_num for reply verifier without modifying sendReply"
  - "SetKerberosConfig as pre-SetRuntime method to avoid changing NFSAdapter constructor"
  - "AUTH_NULL verifier for INIT/DESTROY control replies; GSS MIC verifier for DATA replies"
  - "Silent discard returns nil from handleRPCCall (no reply written to connection)"

patterns-established:
  - "GSS identity injection: gss.ContextWithIdentity(ctx, id) -> gss.IdentityFromContext(ctx) in dispatch"
  - "GSS session info injection: gss.ContextWithSessionInfo(ctx, info) -> gss.SessionInfoFromContext(ctx) for reply"
  - "AUTH_ERROR replies: MakeAuthErrorReply with CREDPROBLEM/CTXPROBLEM for GSS failures"
  - "GSS interception before program dispatch: flavor 6 check at top of handleRPCCall"

# Metrics
duration: 14min
completed: 2026-02-15
---

# Phase 12 Plan 03: RPC Integration Summary

**RPCSEC_GSS DATA path with handleData sequence validation, identity mapping via IdentityMapper, reply verifier (MIC of seq_num), and full NFS connection handler integration for krb5 auth-only**

## Performance

- **Duration:** 14 min
- **Started:** 2026-02-15T13:09:28Z
- **Completed:** 2026-02-15T13:23:41Z
- **Tasks:** 2
- **Files modified:** 11

## Accomplishments
- handleData validates sequence numbers, checks MAXSEQ, maps principal to Identity via IdentityMapper for svc_none
- Reply verifier computes MIC of XDR-encoded seq_num using gokrb5 MICToken with KeyUsageAcceptorSign (25)
- GSSProcessor wired into handleRPCCall with full lifecycle: INIT/DESTROY control path, DATA dispatch path, silent discard, AUTH_ERROR replies
- GSS identity flows through context.Value to both NFSv3 ExtractHandlerContext and NFSv4 ExtractV4HandlerContext
- Reply path detects GSS session info and uses MakeGSSSuccessReply with computed MIC verifier

## Task Commits

Each task was committed atomically:

1. **Task 1: handleData Implementation and Reply Verifier** - `fe5ec77` (feat)
2. **Task 2: NFS Connection Handler GSS Integration** - `415447b` (feat)

## Files Created/Modified
- `internal/protocol/nfs/rpc/gss/verifier.go` - ComputeReplyVerifier (MIC of seq_num) and WrapReplyVerifier
- `internal/protocol/nfs/rpc/gss/verifier_test.go` - Tests for verifier computation and MakeGSSSuccessReply
- `internal/protocol/nfs/rpc/gss/framework.go` - handleData implementation with sequence validation, MAXSEQ check, identity mapping
- `internal/protocol/nfs/rpc/gss/framework_test.go` - DATA path tests (sequence validation, identity mapping, svc_none)
- `internal/protocol/nfs/rpc/gss/context.go` - ContextWithIdentity/IdentityFromContext and GSSSessionInfo context helpers
- `internal/protocol/nfs/rpc/parser.go` - MakeGSSSuccessReply and MakeAuthErrorReply with CREDPROBLEM/CTXPROBLEM constants
- `internal/protocol/nfs/rpc/message.go` - GetVerifierBody method on RPCCallMessage
- `internal/protocol/nfs/dispatch.go` - GSS identity extraction for NFSv3 handlers when auth flavor is RPCSEC_GSS
- `internal/protocol/nfs/v4/handlers/context.go` - GSS identity extraction for NFSv4 COMPOUND context
- `pkg/adapter/nfs/nfs_adapter.go` - GSSProcessor field, SetKerberosConfig, initGSSProcessor, Stop cleanup
- `pkg/adapter/nfs/nfs_connection.go` - GSS interception in handleRPCCall, sendGSSReply with MIC verifier

## Decisions Made
- Used context.Value pattern (gss.ContextWithIdentity, gss.ContextWithSessionInfo) to thread GSS identity and session info through the request pipeline without modifying existing handler or sendReply signatures
- SetKerberosConfig as a separate method called before SetRuntime to avoid changing the New() constructor signature that all existing callers depend on
- AUTH_NULL verifier for INIT/DESTROY control replies (no session key available for INIT, DESTROY cleanup already done); GSS MIC verifier only for DATA replies
- Silent discard returns nil from handleRPCCall with no reply written, per RFC 2203 Section 5.3.3.1
- IdentityMapping config field is a struct (not pointer); took address of local variable for NewStaticMapper

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed IdentityMapping struct vs pointer mismatch**
- **Found during:** Task 2 (NFS Connection Handler GSS Integration)
- **Issue:** KerberosConfig.IdentityMapping is a struct (IdentityMappingConfig), not a pointer. Code compared it to nil and passed it directly to NewStaticMapper which expects *IdentityMappingConfig.
- **Fix:** Created local variable and took its address: `idMapping := s.kerberosConfig.IdentityMapping; mapper = kerberos.NewStaticMapper(&idMapping)`
- **Files modified:** pkg/adapter/nfs/nfs_adapter.go
- **Verification:** `go build ./pkg/adapter/nfs/...` succeeds
- **Committed in:** 415447b (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Minor type mismatch fix, no scope creep.

## Issues Encountered
None beyond the auto-fixed deviation above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- RPCSEC_GSS auth flavor 6 is fully integrated into the NFS connection handler
- Ready for Plan 12-04 (SECINFO Upgrade) to advertise Kerberos security flavors
- Ready for Plan 12-05 (E2E Tests) to validate end-to-end Kerberos authentication

## Self-Check: PASSED

All 11 files verified present. Both task commits (fe5ec77, 415447b) verified in git log.

---
*Phase: 12-kerberos-authentication*
*Completed: 2026-02-15*
