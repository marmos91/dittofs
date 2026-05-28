---
phase: 31-windows-acl-support
plan: 01
subsystem: auth
tags: [sid, windows, acl, identity-mapping, smb, security-descriptor]

# Dependency graph
requires:
  - phase: 30-smb-bug-fixes
    provides: Working SMB security descriptor encode/decode in handlers package
provides:
  - Shared SID package (pkg/auth/sid/) importable by both SMB and NFS adapters
  - SIDMapper with Samba-style RID allocation preventing user/group SID collisions
  - Machine SID persistence via SettingsStore for stable identity across restarts
  - Well-known SID constants (Everyone, Administrators, Anonymous, System)
  - SID.Equal method for SID comparison
affects: [31-02-PLAN, 31-03-PLAN, smb-handlers, nfs-handlers]

# Tech tracking
tech-stack:
  added: [crypto/rand for machine SID generation]
  patterns: [Samba-style RID allocation uid*2+1000/gid*2+1001, package-level mapper with SetSIDMapper/GetSIDMapper]

key-files:
  created:
    - pkg/auth/sid/sid.go
    - pkg/auth/sid/mapper.go
    - pkg/auth/sid/wellknown.go
    - pkg/auth/sid/sid_test.go
  modified:
    - internal/adapter/smb/v2/handlers/security.go
    - internal/adapter/smb/v2/handlers/security_test.go
    - pkg/adapter/smb/adapter.go
    - pkg/controlplane/runtime/lifecycle/service.go
    - pkg/controlplane/runtime/runtime.go

key-decisions:
  - "Samba-style RID allocation (uid*2+1000 for users, gid*2+1001 for groups) prevents collision"
  - "Package-level defaultSIDMapper with SetSIDMapper setter (simplest wiring, no interface changes)"
  - "Fallback mapper with zeroed sub-authorities (0,0,0) for backward compat in tests"
  - "sidToGID tries group SID first, falls back to user SID for backward compat with old SID format"
  - "Machine SID initialized in lifecycle.serve() before adapter loading to prevent race conditions"
  - "MachineSIDStore as narrow interface on lifecycle service (accepts SettingsStore-compatible types)"

patterns-established:
  - "pkg/auth/sid/ as the single source of truth for SID types and identity mapping"
  - "SIDMapper injected via SetSIDMapper before connections are accepted"
  - "Machine SID persisted in SettingsStore under 'machine_sid' key"

requirements-completed: [SD-07]

# Metrics
duration: 8min
completed: 2026-02-27
---

# Phase 31 Plan 01: SID Package + Machine SID Persistence Summary

**Shared SID package with Samba-style RID mapping (uid*2+1000/gid*2+1001) preventing user/group collisions, machine SID generation and persistence in SettingsStore**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-27T17:03:20Z
- **Completed:** 2026-02-27T17:11:20Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Created pkg/auth/sid/ package with SID struct, encode/decode, format/parse, and Equal method
- Added SIDMapper with collision-free RID allocation (user RID = uid*2+1000, group RID = gid*2+1001)
- Added machine SID generation (crypto/rand) with persistence in SettingsStore for stable identity
- Refactored security.go to remove all SID types/functions (now delegates to pkg/auth/sid/)
- Wired machine SID initialization in lifecycle.Service before adapter startup
- 22 tests in sid package + 7 security descriptor tests all passing

## Task Commits

Each task was committed atomically:

1. **Task 1: Create pkg/auth/sid/ package** - `8bd52d1e` (feat)
2. **Task 2: Refactor security.go + machine SID persistence** - `f6389d1a` (refactor)

## Files Created/Modified
- `pkg/auth/sid/sid.go` - SID struct, SIDSize, EncodeSID, DecodeSID, FormatSID, ParseSIDString, Equal
- `pkg/auth/sid/mapper.go` - SIDMapper with UserSID, GroupSID, UIDFromSID, GIDFromSID, PrincipalToSID, SIDToPrincipal
- `pkg/auth/sid/wellknown.go` - Well-known SID constants and WellKnownName lookup
- `pkg/auth/sid/sid_test.go` - 22 tests covering round-trip, collision prevention, persistence, mapping
- `internal/adapter/smb/v2/handlers/security.go` - Removed SID types, now imports pkg/auth/sid/
- `internal/adapter/smb/v2/handlers/security_test.go` - Updated to use deterministic mapper via TestMain
- `pkg/adapter/smb/adapter.go` - Wires SIDMapper from Runtime in SetRuntime
- `pkg/controlplane/runtime/lifecycle/service.go` - Machine SID init and MachineSIDStore interface
- `pkg/controlplane/runtime/runtime.go` - SIDMapper() accessor, passes store to Serve()

## Decisions Made
- Used Samba-style RID allocation (uid*2+1000, gid*2+1001) to guarantee UserSID(n) != GroupSID(n)
- UID 0 (root) maps to BUILTIN\Administrators (S-1-5-32-544), consistent with Samba behavior
- Package-level defaultSIDMapper with setter function (avoids modifying Handler struct or SD function signatures)
- Default fallback mapper uses (0,0,0) sub-authorities for backward compatibility in unit tests
- Machine SID persisted under "machine_sid" key in SettingsStore
- lifecycle.Service receives MachineSIDStore as narrow interface (not full Store)
- Added sidToGID helper that checks group SIDs first, falls back to user SIDs for backward compat

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- pkg/auth/sid/ is ready for import by Plan 02 (DACL synthesis from mode bits) and Plan 03 (NT SD synthesis)
- Machine SID is automatically generated on first boot, no manual setup required
- All existing security descriptor tests pass with the new mapper

## Self-Check: PASSED

- All 4 created files verified present
- Both task commits (8bd52d1e, f6389d1a) verified in git log

---
*Phase: 31-windows-acl-support*
*Completed: 2026-02-27*
