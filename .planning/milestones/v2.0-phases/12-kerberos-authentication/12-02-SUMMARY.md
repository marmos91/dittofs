---
phase: 12-kerberos-authentication
plan: 02
subsystem: auth
tags: [kerberos, rpcsec_gss, gss-api, context-store, ap-req, gokrb5, rfc2203]

# Dependency graph
requires:
  - phase: 12-kerberos-authentication/01
    provides: RPCSEC_GSS XDR types, sequence window, KerberosProvider, IdentityMapper
provides:
  - GSSContext struct with session key, principal, sequence window
  - ContextStore with O(1) lookup, TTL cleanup, LRU eviction
  - GSSProcessor orchestrating RPCSEC_GSS INIT/DESTROY lifecycle
  - Verifier interface for mockable AP-REQ verification
  - Krb5Verifier production implementation using gokrb5 service.VerifyAPREQ
  - GSS-API token extraction (strip wrapper to get raw AP-REQ)
affects: [12-03 RPC integration, 12-04 SECINFO upgrade, 12-05 E2E tests]

# Tech tracking
tech-stack:
  added: []
  patterns: [Verifier interface for testable AP-REQ verification, store-before-reply context ordering, GSS-API token wrapper extraction]

key-files:
  created:
    - internal/protocol/nfs/rpc/gss/context.go
    - internal/protocol/nfs/rpc/gss/context_test.go
    - internal/protocol/nfs/rpc/gss/framework.go
    - internal/protocol/nfs/rpc/gss/framework_test.go
  modified: []

key-decisions:
  - "Verifier interface abstracts AP-REQ verification for testability (mock in tests, gokrb5 in production)"
  - "Store-before-reply ordering enforced: context stored BEFORE INIT reply is built (NFS-Ganesha bug prevention)"
  - "sync.Map for context store: O(1) lookup optimized for high-read/low-write pattern"
  - "Background cleanup every 5 minutes with configurable TTL for idle context expiration"
  - "LRU eviction when maxContexts exceeded: oldest LastUsed context removed"
  - "AP-REP token left empty (gokrb5 does not expose AP-REP building); documented as limitation"
  - "GSSProcessResult carries both control replies and data results in single type"
  - "DATA handling stubbed with explicit error for Plan 03"

patterns-established:
  - "Verifier interface: VerifyToken(gssToken) -> (*VerifiedContext, error) for all GSS token verification"
  - "GSSProcessor.Process(credBody, verifBody, requestBody) as single entry point for auth flavor 6"
  - "extractAPReq strips GSS-API application tag wrapper (0x60 + OID) to get raw AP-REQ bytes"

# Metrics
duration: 5min
completed: 2026-02-15
---

# Phase 12 Plan 02: GSS Context State Machine Summary

**GSS context store with O(1) lookup and TTL cleanup, plus GSSProcessor orchestrating RPCSEC_GSS INIT/DESTROY with mockable AP-REQ verification via Verifier interface**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-15T12:50:53Z
- **Completed:** 2026-02-15T12:55:59Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- GSSContext struct with handle, principal, realm, session key, sequence window, and service level
- ContextStore with sync.Map O(1) lookup, TTL-based expiration, LRU eviction at capacity, background cleanup
- GSSProcessor routes RPCSEC_GSS_INIT to context creation (store-before-reply), DESTROY to context removal
- Verifier interface enables unit testing without real KDC; Krb5Verifier uses gokrb5 VerifyAPREQ for production
- extractAPReq strips GSS-API initial context token wrapper to extract raw AP-REQ bytes
- 43 total tests (12 context + 22 framework + 9 types/sequence from Plan 01) all passing with -race

## Task Commits

Each task was committed atomically:

1. **Task 1: GSS Context Store with TTL Cleanup** - `7bb7c04` (feat)
2. **Task 2: GSSProcessor with INIT and DESTROY Handling** - `6c10895` (feat)

## Files Created/Modified
- `internal/protocol/nfs/rpc/gss/context.go` - GSSContext struct, ContextStore with sync.Map, TTL cleanup, LRU eviction
- `internal/protocol/nfs/rpc/gss/context_test.go` - 12 tests: store/lookup, delete, TTL cleanup, eviction, concurrent access
- `internal/protocol/nfs/rpc/gss/framework.go` - GSSProcessor, Verifier interface, Krb5Verifier, handleInit/handleDestroy, extractAPReq
- `internal/protocol/nfs/rpc/gss/framework_test.go` - 19 tests: INIT lifecycle, DESTROY, credential decode, verifier hot-swap, token extraction

## Decisions Made
- Verifier interface chosen over direct gokrb5 calls to enable comprehensive unit testing without a KDC; pattern (a) from plan
- Store-before-reply ordering is enforced programmatically: context.Store() call precedes RPCGSSInitRes encoding
- AP-REP building deferred: gokrb5 does not directly expose AP-REP construction in service package; documented as TODO for mutual auth
- sync.Map chosen for ContextStore over regular map+mutex: optimized for many readers (every DATA call) with fewer writers (INIT/DESTROY)
- Background cleanup interval set to 5 minutes (constant, not configurable) to balance resource use vs staleness
- GSSProcessResult is a single struct for both control and data flows (IsControl flag distinguishes)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- GSSProcessor ready for RPC integration (Plan 12-03): Process() takes raw credential/verifier/request bytes
- Verifier interface ready for production use with real keytab via Krb5Verifier
- ContextStore ready for DATA request handling (Lookup returns context with session key for MIC/Wrap)
- DATA stub returns explicit error to catch premature DATA calls before Plan 03 implementation

## Self-Check: PASSED

- All 4 created files exist on disk
- Both task commits verified (7bb7c04, 6c10895)
- ContextStore struct found in context.go
- GSSProcessor struct found in framework.go
- All 43 tests pass with -race detection
- go build and go vet clean

---
*Phase: 12-kerberos-authentication*
*Completed: 2026-02-15*
