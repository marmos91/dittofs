---
phase: 14-control-plane-v2-0
plan: 02
subsystem: api
tags: [rest-api, chi-router, adapter-settings, netgroups, security-policy, patch-semantics, rfc7807]

# Dependency graph
requires:
  - phase: 14-control-plane-v2-0
    plan: 01
    provides: GORM models for adapter settings, netgroups, share security policy; store interface with 15 methods
provides:
  - REST API handlers for adapter settings (GET/PUT/PATCH with validation, force, dry_run, reset, defaults)
  - REST API handlers for netgroups (full CRUD with member management)
  - Share API handlers extended with security policy fields (auth flavor, Kerberos, netgroup, blocked operations)
  - Router registration for all new endpoints
  - API client with patch() method and typed adapter settings + netgroup methods
  - SettingsOption functional pattern for force/dry_run query params
affects: [14-03, 14-04, 14-05, 14-06, 14-07]

# Tech tracking
tech-stack:
  added: []
  patterns: [patch-with-pointer-fields, validation-error-response-rfc7807, settings-option-functional-pattern, per-field-reset]

key-files:
  created:
    - internal/controlplane/api/handlers/adapter_settings.go
    - internal/controlplane/api/handlers/netgroups.go
    - pkg/apiclient/adapter_settings.go
    - pkg/apiclient/netgroups.go
  modified:
    - internal/controlplane/api/handlers/shares.go
    - pkg/controlplane/api/router.go
    - pkg/apiclient/client.go
    - pkg/apiclient/shares.go

key-decisions:
  - "ValidationErrorResponse with per-field errors map follows RFC 7807 Pattern 5 from research"
  - "force=true bypasses range validation with logger.Warn audit trail"
  - "dry_run=true validates and returns would-be result without persisting"
  - "Per-field reset via ?setting= query param on POST .../reset endpoint"
  - "Blocked operations validated against known NFS/SMB operation name lists"
  - "Share security policy uses isValidBlockedOperation checking both NFS and SMB ops"

patterns-established:
  - "PATCH with pointer fields: nil means keep current, non-nil means update"
  - "ValidationErrorResponse: RFC 7807 with errors map for per-field validation failures"
  - "SettingsOption functional options: WithForce(), WithDryRun() for API client query params"
  - "Per-field reset: single field reset via query param, full reset via POST without param"

# Metrics
duration: 5min
completed: 2026-02-16
---

# Phase 14 Plan 02: REST API and API Client Summary

**REST API for NFS/SMB adapter settings with PATCH/PUT/validation/force/dry_run/defaults/reset, netgroup CRUD with member management, share security policy extensions, and API client with patch method and SettingsOption pattern**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-16T15:05:08Z
- **Completed:** 2026-02-16T15:10:49Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Adapter settings API with GET/PUT/PATCH supporting per-field validation, force bypass, dry_run preview, full reset, per-field reset, and defaults endpoint with valid ranges
- Netgroup CRUD API following groups.go handler pattern with member add/remove and 409 Conflict on in-use deletion
- Share create/update extended with security policy fields (AllowAuthSys, RequireKerberos, MinKerberosLevel, NetgroupID, BlockedOperations) with validation
- API client with patch() method, typed NFS/SMB settings methods, SettingsOption pattern, and netgroup methods

## Task Commits

Each task was committed atomically:

1. **Task 1: API Handlers for Adapter Settings and Netgroups** - `ac0dd48` (feat)
2. **Task 2: API Client Extension** - `dcfc4b3` (feat)

## Files Created/Modified
- `internal/controlplane/api/handlers/adapter_settings.go` - Adapter settings API handlers (GET/PUT/PATCH/reset/defaults) with NFS and SMB support
- `internal/controlplane/api/handlers/netgroups.go` - Netgroup CRUD handlers with member management
- `internal/controlplane/api/handlers/shares.go` - Extended with security policy fields on create/update and response
- `pkg/controlplane/api/router.go` - Routes for adapter settings and netgroups registered
- `pkg/apiclient/client.go` - Added patch() method for PATCH HTTP support
- `pkg/apiclient/adapter_settings.go` - Typed NFS/SMB settings client methods with SettingsOption pattern
- `pkg/apiclient/netgroups.go` - Netgroup CRUD client methods
- `pkg/apiclient/shares.go` - Share types extended with security policy fields

## Decisions Made
- Used RFC 7807 ValidationErrorResponse with `errors` map for per-field validation failures (status 422)
- `force=true` query param bypasses range validation but logs warning via logger.Warn for audit trail
- `dry_run=true` query param runs full validation and returns would-be result without persisting changes
- Per-field reset uses `?setting=field_name` on POST .../reset; without param resets all settings
- Blocked operations validated against comprehensive known NFS and SMB operation name lists
- Share create applies security policy defaults for nil fields (AllowAuthSys=true, RequireKerberos=false, MinKerberosLevel=krb5)
- SettingsOption functional options pattern for clean API client query param support

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- API layer complete: all handlers and router routes registered for adapter settings and netgroups
- API client ready for CLI consumption in Plan 14-03/14-04
- No blockers

## Self-Check: PASSED

- All 8 files verified present on disk
- Both task commits verified in git log (ac0dd48, dcfc4b3)
- `go build ./...` passes
- `go vet ./internal/controlplane/... ./pkg/controlplane/... ./pkg/apiclient/...` passes

---
*Phase: 14-control-plane-v2-0*
*Completed: 2026-02-16*
