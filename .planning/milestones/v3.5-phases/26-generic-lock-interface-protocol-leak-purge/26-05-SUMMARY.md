---
phase: 26-generic-lock-interface-protocol-leak-purge
plan: 05
subsystem: runtime-api-identity
tags: [runtime-cleanup, mount-tracking, api-routing, identity-dissolution, protocol-decoupling]

# Dependency graph
requires: [26-02, 26-04]
provides:
  - Unified MountTracker for cross-protocol mount management
  - Generic adapter provider pattern replacing NFS-specific fields
  - NFS API handlers scoped under /api/v1/adapters/nfs/ namespace
  - MountHandler for unified and per-protocol mount listing
  - pkg/identity dissolved into pkg/adapter/nfs/identity
  - Package-level DNS cache replacing Runtime struct fields
affects: []

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Unified MountTracker with protocol:client:share composite keys"
    - "Generic SetAdapterProvider/GetAdapterProvider replacing NFS-specific provider methods"
    - "Package-level DNS cache (sync.Once) instead of Runtime struct fields"
    - "Adapter-scoped API routes under /api/v1/adapters/{type}/"
    - "MountHandler with ListByProtocol factory for per-adapter mount views"

key-files:
  created:
    - pkg/controlplane/runtime/mounts.go
    - internal/controlplane/api/handlers/mounts.go
    - pkg/adapter/nfs/identity/mapper.go
    - pkg/adapter/nfs/identity/cache.go
    - pkg/adapter/nfs/identity/convention.go
    - pkg/adapter/nfs/identity/static.go
    - pkg/adapter/nfs/identity/table.go
  modified:
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/share.go
    - pkg/controlplane/runtime/netgroups.go
    - pkg/controlplane/runtime/netgroups_test.go
    - pkg/controlplane/runtime/runtime_test.go
    - pkg/controlplane/api/router.go
    - pkg/apiclient/clients.go
    - pkg/apiclient/grace.go
    - pkg/apiclient/netgroups.go
    - pkg/apiclient/identity_mappings.go
    - pkg/config/config.go
    - internal/protocol/nfs/rpc/gss/framework.go
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/attrs/encode.go

key-decisions:
  - "Used package-level DNS cache (sync.Once) instead of moving to NFS adapter to avoid breaking netgroup matching that lives in runtime package"
  - "Kept shareChangeCallbacks in Runtime (generic mechanism used by multiple adapters, not NFS-specific)"
  - "Kept NFS handler code in internal/controlplane/api/handlers/ (simpler import graph), re-routed under adapter namespace"
  - "Moved pkg/identity to pkg/adapter/nfs/identity sub-package (no import cycles, exclusively NFS code)"
  - "Created deprecated wrapper methods for backward compat (SetNFSClientProvider, NFSClientProvider, RecordMount, etc.)"

patterns-established:
  - "Unified MountTracker with Record/Remove/List/ListByProtocol API"
  - "Generic adapter provider map replacing protocol-specific fields"
  - "Adapter-scoped API routing with per-protocol mount views"

requirements-completed: [REF-02]

# Metrics
duration: 25min
completed: 2026-02-25
---

# Phase 26 Plan 05: Runtime Purge, NFS API Relocation, Identity Dissolution Summary

**Purged NFS-specific fields from Runtime (mounts, dnsCache, nfsClientProvider), created unified MountTracker, moved NFS API handlers under adapter-scoped routes, dissolved pkg/identity into pkg/adapter/nfs/identity**

## Performance

- **Duration:** 25 min
- **Started:** 2026-02-25T09:50:00Z
- **Completed:** 2026-02-25T10:14:17Z
- **Tasks:** 3
- **Files modified:** 32

## Accomplishments
- Removed NFS-specific fields from Runtime struct: mounts map, dnsCache, dnsCacheOnce, nfsClientProvider
- Created unified MountTracker (pkg/controlplane/runtime/mounts.go) with cross-protocol mount management
- Added generic adapter provider pattern (SetAdapterProvider/GetAdapterProvider) replacing NFS-specific methods
- Moved DNS cache to package-level variables (sync.Once pattern) for netgroup matching
- Relocated NFS API routes under /api/v1/adapters/nfs/ namespace (clients, grace, netgroups, identity-mappings)
- Created MountHandler for unified mount listing (/api/v1/mounts) and per-protocol views
- Dissolved pkg/identity/ into pkg/adapter/nfs/identity/ with all tests passing
- Updated all import references across 6 consumer packages
- Updated API client (pkg/apiclient/) endpoints for new adapter-scoped paths
- All Phase 26 success criteria verified: build, vet, tests, race detection all pass

## Task Commits

Each task was committed atomically:

1. **Task 1: Move NFS-specific runtime fields and implement unified mount tracking** - `55c0bb94` (refactor)
2. **Task 2: Relocate NFS API handlers and dissolve pkg/identity/** - `e5ea3a3a` (refactor)
3. **Task 3: Final validation** - No changes needed (verification only)

## Files Created/Modified

### Created
- `pkg/controlplane/runtime/mounts.go` - Unified MountTracker (155 lines) with Record, Remove, List, ListByProtocol
- `internal/controlplane/api/handlers/mounts.go` - MountHandler with List and ListByProtocol factory
- `pkg/adapter/nfs/identity/*.go` - Full identity package (mapper, cache, convention, static, table + tests)

### Modified (key files)
- `pkg/controlplane/runtime/runtime.go` - Removed NFS fields, added mountTracker, adapterProviders map
- `pkg/controlplane/runtime/share.go` - Renamed MountInfo to LegacyMountInfo
- `pkg/controlplane/runtime/netgroups.go` - Package-level DNS cache, standalone functions
- `pkg/controlplane/api/router.go` - NFS routes under /api/v1/adapters/nfs/, unified /api/v1/mounts
- `pkg/apiclient/{clients,grace,netgroups,identity_mappings}.go` - Updated API paths
- `pkg/config/config.go` - Import path update
- `internal/protocol/nfs/rpc/gss/framework.go` - Import path update
- `internal/protocol/nfs/v4/handlers/handler.go` - Import path update
- `internal/protocol/nfs/v4/attrs/encode.go` - Import path update
- `pkg/identity/*.go` - Gutted to empty stubs (code moved to pkg/adapter/nfs/identity)

## Decisions Made
- Kept shareChangeCallbacks in Runtime (used by both NFS and SMB adapters for share change notifications)
- Used package-level DNS cache because netgroups.go remains in runtime package for store access
- Kept handler source files in internal/controlplane/api/handlers/ (avoid new package, simpler imports)
- Created backward-compatible wrapper methods with Deprecated comments for gradual migration

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] DNS cache field reference after Runtime purge**
- **Found during:** Task 1 build verification
- **Issue:** netgroups.go referenced r.dnsCache/r.dnsCacheOnce which were removed from Runtime struct
- **Fix:** Moved to package-level variables (pkgDNSCache, pkgDNSCacheOnce) with ensureDNSCache() init
- **Files modified:** netgroups.go, netgroups_test.go

**2. [Rule 3 - Blocking] Test compilation after mounts field removal**
- **Found during:** Task 1 build verification
- **Issue:** runtime_test.go referenced rt.mounts which was removed
- **Fix:** Changed to rt.mountTracker check
- **Files modified:** runtime_test.go

---

**Total deviations:** 2 auto-fixed (1 bug, 1 blocking)
**Impact on plan:** None - deviations were straightforward fixes within task scope.

## Phase 26 Success Criteria Verification

| # | Criterion | Status |
|---|-----------|--------|
| a | EnhancedLock renamed to UnifiedLock | PASS (no EnhancedLock in pkg/) |
| b | SMB lock types removed from pkg/metadata/lock/ | PASS (no ShareReservation, LeaseInfo) |
| c | SMB lease methods removed from MetadataService | PASS (comment only remains) |
| d | NLM methods moved from MetadataService | PASS (no NLM methods in service.go) |
| e | GracePeriodManager stays generic | PASS (unchanged in lock/grace.go) |
| f | Share model cleaned | PASS (no Squash/AnonymousUID/GuestEnabled) |
| g | SquashMode/Netgroup removed from generic interfaces | PASS (NetgroupStore is separate optional interface) |
| h | NFS API handlers under adapter scope | PASS (routes under /api/v1/adapters/nfs/) |
| i | pkg/identity dissolved | PASS (code in pkg/adapter/nfs/identity, old files gutted) |
| j | Centralized conflict detection | PASS (UnifiedLock.ConflictsWith exists) |

## Issues Encountered
None beyond the auto-fixed deviations documented above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 26 (generic lock interface / protocol leak purge) is complete
- All protocol-specific code removed from generic layers
- Runtime, MetadataService, Store interface, Share model all clean of protocol-specific code
- Lock types unified with centralized conflict detection

## Self-Check: PASSED

- pkg/controlplane/runtime/mounts.go exists with MountTracker
- internal/controlplane/api/handlers/mounts.go exists with MountHandler
- pkg/adapter/nfs/identity/mapper.go exists with full identity package
- pkg/identity/ files gutted (no exported symbols)
- Runtime struct has no NFS-specific fields (verified via grep)
- Both task commits verified (55c0bb94, e5ea3a3a)
- `go build ./...` compiles clean
- `go vet ./...` passes
- `go test ./...` passes
- `go test -race` passes (no data races)

---
*Phase: 26-generic-lock-interface-protocol-leak-purge*
*Completed: 2026-02-25*
