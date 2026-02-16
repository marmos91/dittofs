---
phase: 14-control-plane-v2-0
plan: 03
subsystem: runtime
tags: [settings-watcher, netgroups, dns-cache, hot-reload, access-control]

# Dependency graph
requires:
  - phase: 14-control-plane-v2-0
    provides: GORM models for adapter settings and netgroups (Plan 14-01), Store interface with settings/netgroup CRUD (Plan 14-01)
provides:
  - SettingsWatcher with 10s DB polling for NFS/SMB adapter settings hot-reload
  - CheckNetgroupAccess with IP/CIDR/hostname matching and DNS cache
  - Runtime Share extended with security policy fields (AllowAuthSys, RequireKerberos, etc.)
  - Adapter integration hooks for settings consumption
affects: [14-04, 14-05, 14-06, 14-07]

# Tech tracking
tech-stack:
  added: []
  patterns: [settings-watcher-polling, atomic-pointer-swap, dns-cache-with-negative-ttl, piggybacked-cleanup]

key-files:
  created:
    - pkg/controlplane/runtime/settings_watcher.go
    - pkg/controlplane/runtime/netgroups.go
  modified:
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/share.go
    - pkg/controlplane/runtime/init.go
    - pkg/adapter/nfs/nfs_adapter.go
    - pkg/adapter/smb/smb_adapter.go

key-decisions:
  - "SettingsWatcher non-fatal on LoadInitial: adapters may not exist at startup"
  - "DNS cache piggybacked cleanup on lookups (no separate goroutine)"
  - "NetgroupID stored as name in runtime Share (resolved from DB ID during share loading)"
  - "Empty netgroup allowlist = allow all; netgroup with no members = deny all"
  - "Settings watcher stops before adapters in shutdown sequence"

patterns-established:
  - "Atomic pointer swap for settings: create new struct, swap under write lock, readers get immutable snapshot under RLock"
  - "DNS cache with positive/negative TTL: 5min for success, 1min for errors, piggybacked cleanup"
  - "Adapter integration hooks: document settings consumption in SetRuntime, actual enforcement deferred to Plan 14-04"

# Metrics
duration: 3min
completed: 2026-02-16
---

# Phase 14 Plan 03: Settings Hot-Reload and Netgroup Access Summary

**SettingsWatcher with 10s DB polling and version-based change detection, netgroup IP/CIDR/hostname access checking with 5-min DNS cache, runtime Share extended with security policy fields**

## Performance

- **Duration:** 3 min
- **Started:** 2026-02-16T15:14:37Z
- **Completed:** 2026-02-16T15:18:34Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- SettingsWatcher polls DB every 10s for NFS/SMB settings changes with atomic pointer swap (version-based change detection)
- Start/Stop lifecycle integrated into Runtime serve/shutdown with LoadInitial at startup
- CheckNetgroupAccess supporting IP exact match, CIDR range, and hostname (with wildcard) via reverse DNS
- DNS cache with 5-min positive TTL and 1-min negative TTL, piggybacked cleanup (no extra goroutine)
- Runtime Share/ShareConfig extended with AllowAuthSys, RequireKerberos, MinKerberosLevel, NetgroupID, BlockedOperations
- LoadSharesFromStore populates security policy fields from DB, resolving netgroup ID to name
- NFS/SMB adapter SetRuntime documented for settings hot-reload integration

## Task Commits

Each task was committed atomically:

1. **Task 1: Settings Watcher with DB Polling** - `8353196` (feat)
2. **Task 2: Netgroup Access Checking with DNS Cache** - `7f7e075` (feat)

## Files Created/Modified
- `pkg/controlplane/runtime/settings_watcher.go` - SettingsWatcher with DB polling, version detection, atomic swap, Start/Stop lifecycle
- `pkg/controlplane/runtime/netgroups.go` - CheckNetgroupAccess, dnsCache with TTL, matchHostname with wildcard support
- `pkg/controlplane/runtime/runtime.go` - Settings watcher field, DNS cache fields, GetNFSSettings/GetSMBSettings/GetSettingsWatcher accessors, lifecycle integration
- `pkg/controlplane/runtime/share.go` - Share and ShareConfig extended with security policy fields
- `pkg/controlplane/runtime/init.go` - LoadSharesFromStore populates security policy, resolves netgroup ID to name
- `pkg/adapter/nfs/nfs_adapter.go` - SetRuntime documented for settings hot-reload integration
- `pkg/adapter/smb/smb_adapter.go` - SetRuntime documented for settings hot-reload integration

## Decisions Made
- SettingsWatcher non-fatal on LoadInitial: NFS/SMB adapters may not exist at startup, logged as WARN and continued
- DNS cache uses piggybacked cleanup (removes expired entries on each new lookup, no separate goroutine)
- NetgroupID stored as netgroup name in runtime Share (resolved from DB UUID during share loading via GetNetgroupByID)
- Empty netgroup allowlist (no NetgroupID on share) = allow all; netgroup exists but has no members = deny all
- Settings watcher stops before adapters in shutdown sequence to prevent stale reads during drain
- Adapter integration is comment-only in this plan; actual enforcement deferred to Plan 14-04

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Settings hot-reload and netgroup access checking ready for adapter enforcement in Plan 14-04
- Runtime Share has all security policy fields for Plan 14-06 enforcement
- No blockers

---
*Phase: 14-control-plane-v2-0*
*Completed: 2026-02-16*
