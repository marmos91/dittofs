---
phase: 70-storage-observability-quotas
plan: 02
subsystem: metadata, nfs, smb
tags: [quota, enforcement, nfs4-attrs, space-reporting, filesystem-statistics]

# Dependency graph
requires:
  - phase: 70-01
    provides: Atomic usage counters (GetUsedBytes) on all metadata stores, QuotaBytes field on Share model
provides:
  - Per-share quota enforcement in PrepareWrite (ErrNoSpace on exceeded)
  - Quota-aware GetFilesystemStatistics (TotalBytes/AvailableBytes reflect quota)
  - SetQuotaForShare/GetQuotaForShare API on MetadataService
  - NFSv4 FATTR4_SPACE_TOTAL/SPACE_FREE/SPACE_AVAIL attributes
  - SMB hardcoded fallback removal (uses real store data)
  - 1 PiB unlimited sentinel standardized across all stores
affects: [70-03, 73-trash-soft-delete]

# Tech tracking
tech-stack:
  added: []
  patterns: [quota-overlay-in-service-layer, variadic-fsStats-parameter, bitmap-needs-check-helper]

key-files:
  created:
    - pkg/metadata/service_quota_test.go
  modified:
    - pkg/metadata/service.go
    - pkg/metadata/io.go
    - pkg/metadata/store.go
    - pkg/metadata/interface.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/init.go
    - pkg/controlplane/runtime/shares/service.go
    - internal/adapter/nfs/v4/attrs/encode.go
    - internal/adapter/nfs/v4/handlers/getattr.go
    - internal/adapter/smb/v2/handlers/query_info.go
    - pkg/metadata/store/memory/server.go
    - pkg/metadata/store/badger/server.go

key-decisions:
  - "Quota enforcement at PrepareWrite layer (after file type check, before permission check) for early rejection"
  - "Truncate/shrink always allowed even at quota (only size increases checked)"
  - "Zero-byte file creation allowed at quota (only non-zero newSize checked)"
  - "Quota overlay in MetadataService.GetFilesystemStatistics rather than in each store"
  - "Variadic fsStats parameter on EncodeRealFileAttrs to avoid breaking existing callers"
  - "NeedsFilesystemStats helper avoids fetching stats on every GETATTR (only bits 59-61)"
  - "1 PiB (1<<50) as unlimited sentinel across all stores (was 1TB in memory/badger)"

patterns-established:
  - "Quota overlay pattern: MetadataService intercepts GetFilesystemStatistics and replaces TotalBytes/AvailableBytes when quota is set"
  - "Delta-based quota check: compare (currentUsed + delta) against quota rather than absolute newSize"
  - "GetUsedBytes() on MetadataStore interface for atomic counter access"

requirements-completed: [QUOTA-02, QUOTA-03, QUOTA-04]

# Metrics
duration: 13min
completed: 2026-03-21
---

# Phase 70 Plan 02: Quota Enforcement and Protocol Reporting Summary

**Per-share quota enforcement in PrepareWrite with ErrNoSpace rejection, quota-aware FSSTAT/GETATTR, NFSv4 space attributes (bits 59-61), and SMB hardcoded fallback removal**

## Performance

- **Duration:** 13 min
- **Started:** 2026-03-21T09:46:37Z
- **Completed:** 2026-03-21T09:59:37Z
- **Tasks:** 2
- **Files modified:** 12

## Accomplishments
- Per-share quota enforcement in PrepareWrite rejects writes exceeding quota with ErrNoSpace; truncate and zero-byte creation always allowed
- GetFilesystemStatistics applies quota overlay (TotalBytes = quota, AvailableBytes = quota - used) when quota is configured
- NFSv4 GETATTR now encodes FATTR4_SPACE_TOTAL/SPACE_FREE/SPACE_AVAIL from real filesystem statistics
- SMB FileFsSizeInformation and FileFsFullSizeInformation no longer use hardcoded fallback values
- Unlimited sentinel standardized to 1 PiB (1<<50) across memory and badger stores (was 1TB)

## Task Commits

Each task was committed atomically:

1. **Task 1: Quota enforcement and quota-aware statistics (TDD)** - `2c80d99b` (feat)
2. **Task 2: NFSv4 space attributes and SMB fallback removal** - `a4f22a41` (feat)

## Files Created/Modified
- `pkg/metadata/service_quota_test.go` - 9 TDD tests for quota enforcement and statistics
- `pkg/metadata/service.go` - Quota map, SetQuotaForShare/GetQuotaForShare, quota overlay in GetFilesystemStatistics
- `pkg/metadata/io.go` - Quota enforcement in PrepareWrite (delta check against quota)
- `pkg/metadata/store.go` - GetUsedBytes() added to MetadataStore interface
- `pkg/metadata/interface.go` - SetQuotaForShare/GetQuotaForShare on MetadataServiceInterface
- `pkg/controlplane/runtime/runtime.go` - UpdateShareQuota/GetShareUsage methods, quota wiring in AddShare
- `pkg/controlplane/runtime/init.go` - QuotaBytes wiring during startup share loading
- `pkg/controlplane/runtime/shares/service.go` - QuotaBytes field on ShareConfig
- `internal/adapter/nfs/v4/attrs/encode.go` - FATTR4_SPACE_TOTAL/FREE/AVAIL constants and encoding, NeedsFilesystemStats helper
- `internal/adapter/nfs/v4/handlers/getattr.go` - Fetch fsStats when space attrs requested
- `internal/adapter/smb/v2/handlers/query_info.go` - Removed hardcoded fallback values for cases 3 and 7
- `pkg/metadata/store/memory/server.go` - Changed 1TB default to 1 PiB sentinel
- `pkg/metadata/store/badger/server.go` - Changed 1TB default to 1 PiB sentinel

## Decisions Made
- Quota enforcement at PrepareWrite layer (after file type check, before permission check) for early rejection
- Truncate/shrink always allowed even at quota (only size increases checked against quota)
- Zero-byte file/dir creation allowed at quota (prevents operational deadlocks)
- Quota overlay applied in MetadataService.GetFilesystemStatistics rather than per-store (single enforcement point)
- Variadic fsStats parameter on EncodeRealFileAttrs avoids breaking existing callers (readdir, verify)
- NeedsFilesystemStats helper avoids stat fetch on every GETATTR (only when bits 59-61 requested)
- Standardized unlimited sentinel to 1 PiB across all stores

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Added QuotaBytes/UpdateShareQuota/GetShareUsage to runtime**
- **Found during:** Task 1 (quota enforcement)
- **Issue:** Pre-existing uncommitted changes in shares.go API handler referenced `QuotaBytes` in `ShareConfig`, `UpdateShareQuota`, and `GetShareUsage` on runtime -- all undefined, causing `go build ./...` failure
- **Fix:** Added QuotaBytes to ShareConfig struct, added UpdateShareQuota and GetShareUsage methods to Runtime, wired QuotaBytes in init.go during startup share loading
- **Files modified:** pkg/controlplane/runtime/shares/service.go, pkg/controlplane/runtime/runtime.go, pkg/controlplane/runtime/init.go
- **Verification:** `go build ./...` succeeds
- **Committed in:** 2c80d99b (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Auto-fix was necessary because pre-existing code from Plan 03 (CLI/API wiring) referenced runtime methods that didn't exist yet. No scope creep.

## Issues Encountered
- Linter auto-modified runtime.go to use interface assertions for UpdateShareQuota and GetShareUsage instead of direct method calls; accepted as correct pattern.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Quota enforcement infrastructure complete for Plan 03 CLI/API integration
- NFSv4 and SMB protocol adapters now report quota-aware filesystem statistics
- `df` on NFS mount will show quota as total and (quota - used) as available when quota is set

## Self-Check: PASSED

All created files exist, all commits verified, SUMMARY.md created.

---
*Phase: 70-storage-observability-quotas*
*Completed: 2026-03-21*
