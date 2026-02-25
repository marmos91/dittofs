---
phase: 28-smb-adapter-restructuring
plan: 01
subsystem: adapter
tags: [smb, refactoring, naming-conventions]

# Dependency graph
requires:
  - phase: 27-nfs-adapter-restructuring
    provides: NFS adapter naming conventions pattern to replicate for SMB
provides:
  - Renamed SMB adapter files (no smb_ prefix)
  - Renamed SMB structs (Adapter, Connection, Config, etc.)
  - Consolidated auth packages under internal/adapter/smb/auth/
  - Renamed utils.go to helpers.go
affects: [28-02, 28-03, 28-04, 28-05]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "SMB adapter naming matches NFS adapter conventions (Adapter, Connection, Config)"
    - "Auth packages colocated under owning adapter (internal/adapter/smb/auth/)"

key-files:
  created: []
  modified:
    - pkg/adapter/smb/adapter.go
    - pkg/adapter/smb/connection.go
    - pkg/adapter/smb/connection_test.go
    - pkg/adapter/smb/config.go
    - internal/adapter/smb/auth/ntlm.go
    - internal/adapter/smb/auth/ntlm_test.go
    - internal/adapter/smb/auth/spnego.go
    - internal/adapter/smb/auth/spnego_test.go
    - internal/adapter/smb/helpers.go

key-decisions:
  - "Auth package flattened from ntlm/spnego to single auth package (no naming conflicts)"
  - "Test function names updated to match new type names (TestConnection_ instead of TestSMBConnection_)"

patterns-established:
  - "SMB adapter types: smb.Adapter, smb.Connection, smb.Config (package name provides context)"
  - "Auth code lives under owning adapter: internal/adapter/smb/auth/"

requirements-completed: [REF-04]

# Metrics
duration: 6min
completed: 2026-02-25
---

# Phase 28 Plan 01: SMB File and Struct Renames Summary

**Renamed SMB adapter files (drop smb_ prefix), structs (SMBAdapter->Adapter, SMBConnection->Connection, SMBConfig->Config), moved auth to internal/adapter/smb/auth/, renamed utils.go to helpers.go**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-25T20:35:50Z
- **Completed:** 2026-02-25T20:42:00Z
- **Tasks:** 2
- **Files modified:** 13

## Accomplishments
- Renamed all pkg/adapter/smb/ files to drop the smb_ prefix (adapter.go, connection.go, connection_test.go)
- Renamed all SMB struct types to use package-scoped names (Adapter, Connection, Config, TimeoutsConfig, CreditsConfig, SigningConfig)
- Moved NTLM and SPNEGO auth code from internal/auth/ to internal/adapter/smb/auth/ with flat package declaration
- Renamed utils.go to helpers.go for consistency with NFS adapter conventions

## Task Commits

Each task was committed atomically:

1. **Task 1: Rename pkg/adapter/smb/ files and structs** - `041a2e7b` (refactor)
2. **Task 2: Move auth packages and rename utils.go** - `7b29f386` (refactor)

## Files Created/Modified
- `pkg/adapter/smb/adapter.go` - Renamed from smb_adapter.go; SMBAdapter -> Adapter
- `pkg/adapter/smb/connection.go` - Renamed from smb_connection.go; SMBConnection -> Connection
- `pkg/adapter/smb/connection_test.go` - Renamed from smb_connection_test.go; updated test helpers and function names
- `pkg/adapter/smb/config.go` - SMBConfig -> Config, SMBTimeoutsConfig -> TimeoutsConfig, etc.
- `cmd/dfs/commands/start.go` - Updated smb.SMBConfig -> smb.Config
- `internal/adapter/smb/v2/handlers/handler.go` - Updated comment references
- `internal/adapter/smb/auth/ntlm.go` - Moved from internal/auth/ntlm/; package ntlm -> auth
- `internal/adapter/smb/auth/ntlm_test.go` - Moved from internal/auth/ntlm/; package ntlm -> auth
- `internal/adapter/smb/auth/spnego.go` - Moved from internal/auth/spnego/; package spnego -> auth
- `internal/adapter/smb/auth/spnego_test.go` - Moved from internal/auth/spnego/; package spnego -> auth
- `internal/adapter/smb/helpers.go` - Renamed from utils.go
- `internal/adapter/smb/v2/handlers/session_setup.go` - Updated imports: ntlm/spnego -> auth
- `internal/adapter/smb/v2/handlers/session_setup_test.go` - Updated imports and type references

## Decisions Made
- Auth packages flattened from two packages (ntlm, spnego) into a single `auth` package -- no naming conflicts exist between the NTLM and SPNEGO types
- Test function names updated to reflect renamed types (TestConnection_ instead of TestSMBConnection_)
- Comment references in handler.go updated alongside code changes for consistency

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- All SMB adapter files follow consistent naming conventions matching NFS adapter pattern
- Auth packages now live under the adapter that owns them
- No stale directories remain (internal/auth/ deleted)
- Full test suite green -- ready for Plan 02 (dispatch/handler split)

---
*Phase: 28-smb-adapter-restructuring*
*Completed: 2026-02-25*

## Self-Check: PASSED

All 9 created/modified files verified present. Both commit hashes (041a2e7b, 7b29f386) verified in git log. Old files (smb_adapter.go, smb_connection.go, utils.go) confirmed deleted. internal/auth/ directory confirmed removed.
