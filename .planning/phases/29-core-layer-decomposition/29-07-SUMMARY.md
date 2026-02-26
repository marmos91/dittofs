---
phase: 29-core-layer-decomposition
plan: 07
subsystem: auth, api, docs
tags: [auth, kerberos, spnego, identity-mapper, api-errors, rfc7807, architecture]

# Dependency graph
requires:
  - phase: 29-core-layer-decomposition (plan 06)
    provides: Runtime sub-service decomposition, BaseAdapter with MapError
provides:
  - Centralized auth abstractions (AuthProvider, Authenticator, IdentityMapper, Identity)
  - Kerberos Provider implementing AuthProvider interface
  - IdentityMappingAdapter interface for protocol-specific identity conversion
  - Centralized API error mapping (MapStoreError/HandleStoreError)
  - Updated ARCHITECTURE.md and CLAUDE.md reflecting all Phase 29 changes
affects: [nfs-adapter, smb-adapter, api-handlers, kerberos-auth]

# Tech tracking
tech-stack:
  added: []
  patterns: [auth-provider-chain, identity-mapper, centralized-error-mapping]

key-files:
  created:
    - pkg/auth/auth.go
    - pkg/auth/identity.go
    - pkg/auth/doc.go
  modified:
    - pkg/auth/kerberos/provider.go
    - pkg/auth/kerberos/config.go
    - pkg/adapter/adapter.go
    - pkg/adapter/base.go
    - internal/controlplane/api/handlers/helpers.go
    - internal/controlplane/api/handlers/groups.go
    - docs/ARCHITECTURE.md
    - CLAUDE.md

key-decisions:
  - "IdentityMappingAdapter as separate interface (not embedded in ProtocolAdapter) to avoid breaking all existing adapters"
  - "MapIdentity default stub on BaseAdapter so NFS/SMB inherit without code changes"
  - "Kerberos Provider.Authenticate returns Authenticated:false by design (full token validation handled by protocol-specific layers)"
  - "HandleStoreError convenience function wraps MapStoreError + WriteProblem for one-line error handling"
  - "Converted all groups.go handlers to centralized error mapping as demonstration of the pattern"

patterns-established:
  - "AuthProvider chain: CanHandle(token) -> Authenticate(ctx, token) for pluggable auth mechanisms"
  - "IdentityMapper: protocol adapters implement MapIdentity to convert auth results to protocol-specific identities"
  - "Centralized error mapping: HandleStoreError(w, err) replaces per-handler switch blocks"

requirements-completed: [REF-06.2, REF-06.4]

# Metrics
duration: 13min
completed: 2026-02-26
---

# Phase 29 Plan 07: Auth Centralization, API Error Mapping, Documentation Summary

**Centralized auth abstractions (AuthProvider chain, IdentityMapper, Identity) in pkg/auth/, Kerberos AuthProvider interface, centralized API error mapping via MapStoreError/HandleStoreError, ARCHITECTURE.md + CLAUDE.md updated for all Phase 29 changes**

## Performance

- **Duration:** 13 min
- **Started:** 2026-02-26T11:01:00Z
- **Completed:** 2026-02-26T11:14:00Z
- **Tasks:** 2
- **Files modified:** 11

## Accomplishments
- Created pkg/auth/ with AuthProvider interface, Authenticator chain, Identity model, and IdentityMapper interface
- Kerberos Provider now implements auth.AuthProvider with SPNEGO/AP-REQ token detection
- Added IdentityMappingAdapter interface and BaseAdapter.MapIdentity stub for protocol adapters
- Centralized API error mapping: MapStoreError maps all sentinel errors to HTTP status codes, HandleStoreError provides one-line handler error responses
- Converted groups.go (7 error handling blocks) to use centralized HandleStoreError
- Updated ARCHITECTURE.md and CLAUDE.md with complete Phase 29 package structure (offloader, gc, io, file, auth, storetest, runtime sub-services, store sub-interfaces)

## Task Commits

Each task was committed atomically:

1. **Task 1: Centralize auth abstractions and update adapter interface** - `4632af79` (feat)
2. **Task 2: Centralize API error mapping and update documentation** - `0a8968c8` (feat)

## Files Created/Modified
- `pkg/auth/auth.go` - AuthProvider interface, Authenticator chain, AuthResult, error sentinels
- `pkg/auth/identity.go` - Identity model (Unix/Kerberos/NTLM/anonymous), IdentityMapper interface
- `pkg/auth/doc.go` - Package documentation for pkg/auth/
- `pkg/auth/kerberos/provider.go` - Renamed from kerberos.go; implements auth.AuthProvider (CanHandle, Authenticate, Name)
- `pkg/auth/kerberos/config.go` - Updated package documentation referencing AuthProvider
- `pkg/adapter/adapter.go` - Added IdentityMappingAdapter interface (extends Adapter with IdentityMapper)
- `pkg/adapter/base.go` - Added default MapIdentity stub returning "not implemented" error
- `internal/controlplane/api/handlers/helpers.go` - Added MapStoreError and HandleStoreError for centralized error-to-HTTP mapping
- `internal/controlplane/api/handlers/groups.go` - Converted all error handling to use HandleStoreError
- `docs/ARCHITECTURE.md` - Updated package structure, data flow, directory tree for all Phase 29 changes
- `CLAUDE.md` - Updated architecture section, key interfaces, directory structure for Phase 29

## Decisions Made
- IdentityMappingAdapter as separate interface (not embedded in ProtocolAdapter) to avoid breaking all existing adapter implementations; runtime checks via type assertion
- MapIdentity default stub on BaseAdapter so both NFS and SMB adapters inherit without code changes
- Kerberos Provider.Authenticate deliberately returns Authenticated:false -- full Kerberos token validation happens in protocol-specific layers (gss.Krb5Verifier for NFS, SMB auth handler) that use Provider for keytab state
- HandleStoreError wraps MapStoreError + WriteProblem for one-line handler error responses
- Converted all groups.go handlers as demonstration; other handlers can adopt incrementally

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed build failure from partial groups.go conversion**
- **Found during:** Task 2 (API error mapping centralization)
- **Issue:** After converting Create, Get, Delete handlers to HandleStoreError and removing the `errors` import, Update, AddMember, RemoveMember, and ListMembers still used `errors.Is()` directly, causing build failure
- **Fix:** Converted all remaining error handling blocks in groups.go to use HandleStoreError
- **Files modified:** internal/controlplane/api/handlers/groups.go
- **Verification:** go build ./... passed
- **Committed in:** 0a8968c8 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Auto-fix was necessary for correctness. No scope creep.

## Issues Encountered
- git add for renamed file (kerberos.go -> provider.go) failed when trying to add the old path; git mv already staged the rename correctly

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Phase 29 (Core Layer Decomposition) is now fully complete (7/7 plans)
- All auth abstractions centralized and ready for protocol-specific implementations
- API error mapping pattern established and demonstrated
- Documentation updated to reflect the complete decomposed architecture
- Ready for next milestone/phase

## Self-Check: PASSED

All 3 created files verified present. All 8 modified files verified present. Both task commits (4632af79, 0a8968c8) verified in git log. SUMMARY.md created successfully.

---
*Phase: 29-core-layer-decomposition*
*Completed: 2026-02-26*
