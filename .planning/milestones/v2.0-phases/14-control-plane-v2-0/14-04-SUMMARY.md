---
phase: 14-control-plane-v2-0
plan: 04
subsystem: protocol
tags: [nfs, smb, settings-watcher, security-policy, operation-blocklist, delegation-policy, netgroup]

# Dependency graph
requires:
  - phase: 14-02
    provides: "SettingsWatcher with GetNFSSettings/GetSMBSettings for live settings polling"
  - phase: 14-03
    provides: "Share security policy fields (AllowAuthSys, RequireKerberos, NetgroupID, BlockedOperations)"
provides:
  - "NFS adapter consuming live settings from SettingsWatcher"
  - "NFSv4 operation blocklist enforcement in COMPOUND dispatcher"
  - "Mount-time security policy enforcement (auth flavor, Kerberos requirement)"
  - "Netgroup IP access control on mount"
  - "Delegation policy controlled by settings"
  - "SMB adapter consuming live settings from SettingsWatcher"
  - "Dynamic max_connections enforcement for both adapters"
  - "OpNameToNum reverse mapping for string-based operation blocklists"
affects: ["14-06", "14-07"]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Live settings consumption at connection accept time (grandfathering)"
    - "Adapter-level operation blocklist via map[uint32]bool"
    - "Security policy enforcement at protocol boundary (mount/PUTFH)"

key-files:
  created: []
  modified:
    - "pkg/adapter/nfs/nfs_adapter.go"
    - "pkg/adapter/smb/smb_adapter.go"
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/handlers/compound.go"
    - "internal/protocol/nfs/v4/state/manager.go"
    - "internal/protocol/nfs/v4/state/delegation.go"
    - "internal/protocol/nfs/v4/types/constants.go"
    - "internal/protocol/nfs/mount/handlers/mount.go"

key-decisions:
  - "Operation blocklist returns NFS4ERR_NOTSUPP (not NFS4ERR_PERM) per locked decision"
  - "Security policy checked at mount handler for NFSv3, checked at COMPOUND for NFSv4 after PUTFH"
  - "Netgroup check is fail-closed: on error, deny access"
  - "SMB operation blocklist is advisory-only with logging (SMB lacks per-op COMPOUND granularity)"
  - "SMB encryption is a stub that logs warning per locked decision"

patterns-established:
  - "applyNFSSettings/applySMBSettings pattern: read settings from runtime, apply to handlers/state"
  - "Live max_connections check in accept loop supplements static connSemaphore"

# Metrics
duration: 12min
completed: 2026-02-16
---

# Phase 14 Plan 04: Adapter Settings Enforcement Summary

**NFS/SMB adapters wired to consume live settings from SettingsWatcher with operation blocklist, security policy, delegation policy, and netgroup enforcement**

## Performance

- **Duration:** ~12 min
- **Started:** 2026-02-16T15:30:53Z
- **Completed:** 2026-02-16T15:39:29Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments

- NFS adapter reads live settings at startup and enforces them: lease time, grace period, delegation policy, operation blocklist, and max connections
- NFSv4 COMPOUND dispatcher checks adapter-level blocklist before dispatching each operation, returning NFS4ERR_NOTSUPP for blocked ops
- Mount handler enforces per-share security policy (AllowAuthSys, RequireKerberos) and netgroup IP access control
- SMB adapter reads live settings and enforces dynamic max_connections with encryption stub logging
- Added OpNameToNum reverse mapping to types package for translating string-based blocklists from the DB into numeric lookup tables

## Task Commits

Each task was committed atomically:

1. **Task 1: NFS Adapter Settings Enforcement** - `39033d4` (feat)
2. **Task 2: SMB Adapter Settings Enforcement** - `ccfb863` (feat)

## Files Created/Modified

- `pkg/adapter/nfs/nfs_adapter.go` - Added applyNFSSettings(), live max_connections check in accept loop, v4attrs import
- `pkg/adapter/smb/smb_adapter.go` - Added applySMBSettings(), live max_connections check in accept loop, encryption stub log
- `internal/protocol/nfs/v4/handlers/handler.go` - Added blockedOps field, SetBlockedOps(), IsOperationBlocked() methods
- `internal/protocol/nfs/v4/handlers/compound.go` - Added blocklist check before operation dispatch in COMPOUND loop
- `internal/protocol/nfs/v4/state/manager.go` - Added delegationsEnabled field, SetDelegationsEnabled(), SetLeaseTime(), SetGracePeriodDuration() methods
- `internal/protocol/nfs/v4/state/delegation.go` - Added Check 0 for delegationsEnabled in ShouldGrantDelegation
- `internal/protocol/nfs/v4/types/constants.go` - Added OpNameToNum() reverse mapping and opNameToNum init map
- `internal/protocol/nfs/mount/handlers/mount.go` - Added security policy checks (AllowAuthSys, RequireKerberos), netgroup IP access check

## Decisions Made

- Operation blocklist returns NFS4ERR_NOTSUPP (consistent with unimplemented operations, per locked decision)
- Security policy enforcement is fail-closed: errors in netgroup check deny access rather than silently allowing
- SMB blocklist is advisory-only with INFO logging because SMB lacks the per-operation dispatch model of NFS COMPOUND
- SMB encryption is a stub per locked decision (config knob present, logs warning, ready for future SMB3 encryption)
- Delegation policy check is the very first check in ShouldGrantDelegation (Check 0) to short-circuit early

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- All adapter settings are now consumed and enforced at runtime
- Plans 14-06 and 14-07 can build on this enforcement layer for integration testing and API finalization
- The operation blocklist framework supports both adapter-level and per-share blocklists

---
*Phase: 14-control-plane-v2-0*
*Plan: 04*
*Completed: 2026-02-16*
