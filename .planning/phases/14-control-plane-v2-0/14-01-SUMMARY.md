---
phase: 14-control-plane-v2-0
plan: 01
subsystem: database
tags: [gorm, sqlite, postgresql, adapter-settings, netgroups, security-policy]

# Dependency graph
requires:
  - phase: 13-nfsv4-acls
    provides: IdentityMapping GORM model and controlplane store CRUD
provides:
  - NFSAdapterSettings and SMBAdapterSettings GORM models with typed fields
  - Netgroup and NetgroupMember GORM models with IP/CIDR/hostname validation
  - Share security policy fields (AllowAuthSys, RequireKerberos, MinKerberosLevel, NetgroupID, BlockedOperations)
  - Store interface with 15 new methods for adapter settings and netgroup CRUD
  - GORM implementations with atomic version counters and data migration
affects: [14-02, 14-03, 14-04, 14-05, 14-06, 14-07]

# Tech tracking
tech-stack:
  added: []
  patterns: [version-counter-for-change-detection, post-migrate-data-fixup, typed-settings-separate-table]

key-files:
  created:
    - pkg/controlplane/models/adapter_settings.go
    - pkg/controlplane/models/netgroup.go
    - pkg/controlplane/store/adapter_settings.go
    - pkg/controlplane/store/netgroups.go
  modified:
    - pkg/controlplane/models/share.go
    - pkg/controlplane/models/models.go
    - pkg/controlplane/models/errors.go
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/store/gorm.go
    - pkg/controlplane/store/shares.go

key-decisions:
  - "Version counter (monotonic int) for settings change detection instead of timestamps"
  - "Post-migrate SQL UPDATE to fix GORM zero-value boolean trap for existing shares"
  - "EnsureAdapterSettings called in New() to auto-populate defaults for existing adapters"
  - "Netgroup deletion uses ErrNetgroupInUse check via share FK reference count"
  - "Share security fields added directly to Share struct (not separate table)"

patterns-established:
  - "Version counter pattern: atomic gorm.Expr('version + 1') for change detection polling"
  - "Post-migrate fixup: raw SQL after AutoMigrate to correct zero-value defaults on existing rows"
  - "Typed settings in separate table: 1:1 with adapter via AdapterID unique index"

# Metrics
duration: 4min
completed: 2026-02-16
---

# Phase 14 Plan 01: Data Layer Foundation Summary

**GORM models for NFS/SMB adapter settings with 20+ typed fields each, netgroup IP access control, and share security policy with auth flavor toggles and Kerberos level control**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-16T14:56:43Z
- **Completed:** 2026-02-16T15:01:06Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- NFSAdapterSettings with 15+ typed fields covering version negotiation, timeouts, connection limits, transport tuning, delegation policy, and operation blocklist
- SMBAdapterSettings with dialect negotiation, session timeout, encryption stub, and operation blocklist
- Netgroup and NetgroupMember models with IP/CIDR/hostname validation (including wildcard patterns)
- Share security policy: AllowAuthSys, RequireKerberos, MinKerberosLevel, NetgroupID, BlockedOperations
- Store interface extended with 15 new methods; GORM implementations with atomic version increment
- Data migration auto-populates defaults for existing adapters and shares

## Task Commits

Each task was committed atomically:

1. **Task 1: GORM Models for Adapter Settings, Netgroups, and Share Security** - `3bdf903` (feat)
2. **Task 2: Store Interface Extension and GORM Implementation** - `9ef0cdd` (feat)

## Files Created/Modified
- `pkg/controlplane/models/adapter_settings.go` - NFSAdapterSettings/SMBAdapterSettings GORM models with helpers and validation ranges
- `pkg/controlplane/models/netgroup.go` - Netgroup/NetgroupMember GORM models with member type/value validation
- `pkg/controlplane/models/share.go` - Extended Share with 5 security policy fields and GetBlockedOps/SetBlockedOps helpers
- `pkg/controlplane/models/models.go` - AllModels() updated with 4 new types
- `pkg/controlplane/models/errors.go` - ErrNetgroupNotFound, ErrDuplicateNetgroup, ErrNetgroupInUse sentinel errors
- `pkg/controlplane/store/interface.go` - 15 new Store interface methods for adapter settings and netgroups
- `pkg/controlplane/store/adapter_settings.go` - GORM adapter settings CRUD with atomic version increment
- `pkg/controlplane/store/netgroups.go` - GORM netgroup CRUD with member management and in-use protection
- `pkg/controlplane/store/gorm.go` - Post-migrate EnsureAdapterSettings + share default fixup SQL
- `pkg/controlplane/store/shares.go` - UpdateShare includes security policy fields

## Decisions Made
- Used monotonic version counter (int, incremented via `gorm.Expr("version + 1")`) for settings change detection, following research recommendation over timestamp comparison
- Post-migrate raw SQL to fix GORM zero-value boolean trap: existing shares get `allow_auth_sys=true` when all security fields are zero-valued (untouched by ALTER TABLE)
- EnsureAdapterSettings called in store New() to auto-create default settings for pre-existing adapters on every startup
- Netgroup deletion checks share FK reference count before allowing delete (ErrNetgroupInUse)
- Share security fields added directly to Share struct (not a separate table) as they are part of the share's identity

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Data layer complete: all GORM models, store interface, and GORM implementations ready
- Plans 02-07 can build API handlers, CLI commands, runtime integration, and hot-reload on this foundation
- No blockers

## Self-Check: PASSED

- All 10 files verified present on disk
- Both task commits verified in git log (3bdf903, 9ef0cdd)
- `go build ./pkg/controlplane/...` passes
- `go vet ./pkg/controlplane/...` passes
- `go test -tags=integration ./pkg/controlplane/store/...` passes (no regressions)

---
*Phase: 14-control-plane-v2-0*
*Completed: 2026-02-16*
